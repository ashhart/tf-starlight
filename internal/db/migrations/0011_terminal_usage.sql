SET LOCAL lock_timeout = '2s';

CREATE TABLE terminal_leases (
    lease_id uuid PRIMARY KEY,
    project_id uuid,
    kind text NOT NULL CHECK (kind IN ('settled', 'released', 'reaped', 'legacy_deleted')),
    charged_tokens bigint CHECK (charged_tokens >= 0),
    charged_micros bigint CHECK (charged_micros >= 0),
    observed boolean,
    ended_at timestamptz NOT NULL,
    late_until timestamptz NOT NULL,
    receipt text NOT NULL,
    receipt_hash bytea NOT NULL CHECK (receipt_hash = sha256(convert_to(receipt, 'UTF8'))),
    CHECK ((charged_tokens IS NULL) = (charged_micros IS NULL)),
    CHECK ((kind = 'legacy_deleted') = (charged_tokens IS NULL)),
    CHECK (kind <> 'legacy_deleted' OR observed IS NULL),
    CHECK (kind NOT IN ('released','reaped') OR observed IS FALSE)
);
CREATE INDEX terminal_leases_expiry ON terminal_leases (late_until, lease_id);
CREATE INDEX terminal_leases_project ON terminal_leases (project_id, ended_at DESC, lease_id);
CREATE INDEX terminal_leases_time ON terminal_leases (ended_at DESC, lease_id);

CREATE TABLE late_usage (
    lease_id uuid PRIMARY KEY REFERENCES terminal_leases (lease_id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    report text NOT NULL,
    report_hash bytea NOT NULL CHECK (report_hash = sha256(convert_to(report, 'UTF8')))
);
CREATE INDEX late_usage_created ON late_usage (created_at, lease_id);

CREATE TABLE accounting_audit (
    lease_id uuid NOT NULL REFERENCES terminal_leases (lease_id),
    kind text NOT NULL CHECK (kind IN ('terminal', 'late')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    audit_seq bigint,
    audit_hash bytea,
    attempts int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_error_class text NOT NULL DEFAULT '' CHECK (last_error_class IN
        ('','deadline','cancelled','integrity','lock_timeout','transaction','query_cancelled','postgres','other')),
    PRIMARY KEY (lease_id, kind),
    CHECK ((audit_seq IS NULL) = (audit_hash IS NULL)),
    CHECK (audit_hash IS NULL OR octet_length(audit_hash) = 32)
);
CREATE INDEX accounting_audit_pending ON accounting_audit (created_at, lease_id) WHERE audit_seq IS NULL;
CREATE INDEX accounting_audit_due ON accounting_audit (next_attempt_at, created_at, lease_id) WHERE audit_seq IS NULL;

CREATE FUNCTION starlight_accounting_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'accounting evidence is immutable' USING ERRCODE = '23514';
END $$;
CREATE TRIGGER terminal_leases_immutable BEFORE UPDATE ON terminal_leases
    FOR EACH ROW EXECUTE FUNCTION starlight_accounting_immutable();
CREATE TRIGGER late_usage_immutable BEFORE UPDATE ON late_usage
    FOR EACH ROW EXECUTE FUNCTION starlight_accounting_immutable();

CREATE FUNCTION starlight_accounting_link_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.audit_seq IS NOT NULL
        OR (to_jsonb(NEW) - ARRAY['audit_seq','audit_hash','attempts','next_attempt_at','last_error_class'])
        <> (to_jsonb(OLD) - ARRAY['audit_seq','audit_hash','attempts','next_attempt_at','last_error_class']) THEN
        RAISE EXCEPTION 'accounting audit identity and link are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.audit_seq IS NULL THEN
        IF NEW.audit_hash IS NOT NULL OR NEW.attempts < OLD.attempts THEN
            RAISE EXCEPTION 'invalid accounting audit retry transition' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.audit_hash IS NULL OR NEW.attempts <> OLD.attempts OR NEW.next_attempt_at <> OLD.next_attempt_at
        OR NEW.last_error_class <> OLD.last_error_class THEN
        RAISE EXCEPTION 'invalid accounting audit link transition' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER accounting_audit_link_once BEFORE UPDATE ON accounting_audit
    FOR EACH ROW EXECUTE FUNCTION starlight_accounting_link_once();

CREATE FUNCTION starlight_accounting_keep_pending() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.audit_seq IS NULL THEN
        RAISE EXCEPTION 'pending accounting audit cannot be deleted' USING ERRCODE = '23514';
    END IF;
    RETURN OLD;
END $$;
CREATE TRIGGER accounting_audit_keep_pending BEFORE DELETE ON accounting_audit
    FOR EACH ROW EXECUTE FUNCTION starlight_accounting_keep_pending();

CREATE FUNCTION starlight_terminal(l leases, terminal_kind text, tokens bigint, micros bigint, seen boolean)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    ended timestamptz := clock_timestamp();
    body text;
BEGIN
    body := jsonb_build_object('version', 1, 'lease', l.id, 'kind', terminal_kind,
        'request_hash', encode(sha256(convert_to(l.request_id, 'UTF8')), 'hex'),
        'gateway', l.gateway_id, 'model', l.model_id, 'replica', l.replica_id, 'route', l.route_id,
        'principal_type', l.principal_type, 'principal_id', l.principal_id, 'project_id', l.project_id,
        'reserved_tokens', l.reserved_tokens, 'reserved_micros', l.reserved_micros,
        'charged_tokens', tokens, 'charged_micros', micros, 'observed', seen,
        'started_at', l.created_at, 'ended_at', ended, 'late_until', ended + interval '7 days')::text;
    INSERT INTO terminal_leases (lease_id, project_id, kind, charged_tokens, charged_micros, observed,
        ended_at, late_until, receipt, receipt_hash)
    VALUES (l.id, l.project_id, terminal_kind, tokens, micros, seen, ended, ended + interval '7 days',
        body, sha256(convert_to(body, 'UTF8')));
    IF terminal_kind IN ('reaped', 'legacy_deleted') THEN
        INSERT INTO accounting_audit (lease_id, kind) VALUES (l.id, 'terminal');
    END IF;
END $$;

-- An older reaper's raw DELETE cannot prove what it charged; preserve that uncertainty explicitly.
CREATE FUNCTION starlight_legacy_terminal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM terminal_leases WHERE lease_id = OLD.id) THEN
        PERFORM starlight_terminal(OLD, 'legacy_deleted', NULL, NULL, NULL);
    END IF;
    RETURN OLD;
END $$;
CREATE TRIGGER leases_terminal AFTER DELETE ON leases FOR EACH ROW EXECUTE FUNCTION starlight_legacy_terminal();

CREATE FUNCTION starlight_end_lease(lease uuid, tokens bigint, micros bigint, terminal_kind text, seen boolean)
RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE
    l leases;
BEGIN
    SELECT * INTO l FROM leases WHERE id = lease FOR UPDATE;
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    PERFORM starlight_terminal(l, terminal_kind, tokens, micros, seen);
    DELETE FROM leases WHERE id = lease;
    PERFORM starlight_settle_counters(l.budget_keys,
        starlight_amounts(l.budget_keys, l.reserved_tokens, l.reserved_micros),
        starlight_amounts(l.budget_keys, tokens, micros));
    PERFORM pg_notify('starlight_capacity', l.model_id::text);
    RETURN true;
END $$;

CREATE OR REPLACE FUNCTION starlight_release(lease uuid, tokens bigint, micros bigint)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    PERFORM starlight_end_lease(lease, tokens, micros, 'released', false);
END $$;

CREATE FUNCTION starlight_reap_stale(age interval)
RETURNS TABLE (model_id uuid, budget_keys text[], amounts bigint[]) LANGUAGE plpgsql AS $$
DECLARE
    l leases;
BEGIN
    FOR l IN SELECT * FROM leases WHERE heartbeat_at < now() - age ORDER BY id FOR UPDATE LOOP
        PERFORM starlight_terminal(l, 'reaped', l.reserved_tokens, l.reserved_micros, false);
        DELETE FROM leases WHERE id = l.id;
        model_id := l.model_id;
        budget_keys := l.budget_keys;
        amounts := starlight_amounts(l.budget_keys, l.reserved_tokens, l.reserved_micros);
        RETURN NEXT;
    END LOOP;
END $$;

CREATE FUNCTION starlight_late_usage(r jsonb) RETURNS text LANGUAGE plpgsql AS $$
DECLARE
    t terminal_leases;
    authority jsonb;
    body text;
    previous text;
BEGIN
    SELECT * INTO t FROM terminal_leases WHERE lease_id = (r->>'lease')::uuid FOR UPDATE;
    IF NOT FOUND THEN RETURN 'unknown'; END IF;
    IF t.kind = 'released' THEN RETURN 'released'; END IF;
    IF t.observed THEN RETURN 'complete'; END IF;
    IF NOT coalesce((r->>'observed')::boolean, false) THEN RETURN 'unobserved'; END IF;
    authority := t.receipt::jsonb;
    IF NOT r ?& ARRAY['lease_request', 'lease_gateway', 'lease_principal_type', 'lease_principal_id', 'lease_project_id'] THEN
        RETURN 'unverified';
    END IF;
    IF authority->>'request_hash' IS DISTINCT FROM encode(sha256(convert_to(r->>'lease_request', 'UTF8')), 'hex')
        OR authority->>'gateway' IS DISTINCT FROM r->>'lease_gateway'
        OR authority->>'model' IS DISTINCT FROM r->>'model'
        OR authority->>'principal_type' IS DISTINCT FROM r->>'lease_principal_type'
        OR authority->>'principal_id' IS DISTINCT FROM nullif(r->>'lease_principal_id', '')
        OR authority->>'project_id' IS DISTINCT FROM nullif(r->>'lease_project_id', '')
        OR coalesce(authority->>'replica', authority->>'route') IS DISTINCT FROM r->>'route_id' THEN
        RAISE EXCEPTION 'late usage authority does not match its terminal lease' USING ERRCODE = '23514';
    END IF;
    IF NOT r ?& ARRAY['prompt','completion','cached','cost','protocol','status','outcome','queue_ms','ttft_ms','duration_ms']
        OR r->>'prompt' IS NULL OR r->>'completion' IS NULL OR r->>'cached' IS NULL OR r->>'cost' IS NULL
        OR r->>'protocol' IS NULL OR r->>'status' IS NULL OR r->>'outcome' IS NULL
        OR r->>'queue_ms' IS NULL OR r->>'ttft_ms' IS NULL OR r->>'duration_ms' IS NULL
        OR (r->>'prompt')::bigint < 0 OR (r->>'completion')::bigint < 0 OR (r->>'cached')::bigint < 0
        OR (r->>'cost')::bigint < 0 OR (r->>'prompt')::numeric + (r->>'completion')::numeric > 9223372036854775807
        OR (r->>'queue_ms')::bigint < 0 OR (r->>'ttft_ms')::bigint < 0 OR (r->>'duration_ms')::bigint < 0
        OR length(r->>'protocol') > 64 OR length(r->>'outcome') > 64 THEN
        RAISE EXCEPTION 'invalid late usage observation' USING ERRCODE = '23514';
    END IF;
    body := jsonb_build_object('version', 1, 'late', true, 'lease', t.lease_id,
        'terminal_hash', encode(t.receipt_hash, 'hex'), 'additional_budget_tokens', 0, 'additional_budget_micros', 0,
        'protocol', r->>'protocol', 'status', (r->>'status')::int, 'outcome', r->>'outcome',
        'prompt', (r->>'prompt')::bigint, 'completion', (r->>'completion')::bigint,
        'cached', (r->>'cached')::bigint, 'cost', (r->>'cost')::bigint,
        'queue_ms', (r->>'queue_ms')::bigint, 'ttft_ms', (r->>'ttft_ms')::bigint,
        'duration_ms', (r->>'duration_ms')::bigint)::text;
    SELECT report INTO previous FROM late_usage WHERE lease_id = t.lease_id;
    IF FOUND THEN
        IF previous = body THEN RETURN 'duplicate'; END IF;
        RAISE EXCEPTION 'conflicting late usage observation' USING ERRCODE = '23514';
    END IF;
    IF clock_timestamp() >= t.late_until THEN RETURN 'expired'; END IF;
    INSERT INTO late_usage (lease_id, report, report_hash) VALUES (t.lease_id, body, sha256(convert_to(body, 'UTF8')));
    INSERT INTO accounting_audit (lease_id, kind) VALUES (t.lease_id, 'terminal') ON CONFLICT DO NOTHING;
    INSERT INTO accounting_audit (lease_id, kind) VALUES (t.lease_id, 'late');
    RETURN 'late';
END $$;

CREATE FUNCTION starlight_settle_result(r jsonb) RETURNS text LANGUAGE plpgsql AS $$
BEGIN
    IF NOT starlight_end_lease((r->>'lease')::uuid, (r->>'tokens')::bigint, (r->>'micros')::bigint,
        'settled', (r->>'observed')::boolean) THEN
        RETURN starlight_late_usage(r);
    END IF;
    INSERT INTO usage_events (id, created_at, request_id, principal_type, principal_id, user_id, project_id, api_key_id, task_id, model_id,
        model_name, route_kind, route_id, node_id, protocol, status, outcome, prompt_tokens, completion_tokens, cached_tokens, cost_micros,
        queue_ms, ttft_ms, duration_ms, classification, priority, client)
    VALUES ((r->>'usage')::uuid, now(), r->>'request', r->>'principal_type', (r->>'principal_id')::uuid, nullif(r->>'user_id', '')::uuid,
        (r->>'project_id')::uuid, nullif(r->>'api_key_id', '')::uuid, nullif(r->>'task_id', '')::uuid, (r->>'model')::uuid, r->>'model_name',
        r->>'route_kind', nullif(r->>'route_id', '')::uuid, nullif(r->>'node_id', '')::uuid, r->>'protocol', (r->>'status')::int, r->>'outcome',
        (r->>'prompt')::bigint, (r->>'completion')::bigint, (r->>'cached')::bigint, (r->>'cost')::bigint, (r->>'queue_ms')::int,
        (r->>'ttft_ms')::int, (r->>'duration_ms')::int, r->>'classification', r->>'priority', r->>'client');
    IF r ? 'replica' THEN
        UPDATE replicas SET last_request_at = now() WHERE id = (r->>'replica')::uuid
            AND (last_request_at IS NULL OR last_request_at < now() - interval '10 seconds');
    END IF;
    RETURN 'settled';
END $$;

CREATE OR REPLACE FUNCTION starlight_settle(r jsonb) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    PERFORM starlight_settle_result(r);
END $$;
