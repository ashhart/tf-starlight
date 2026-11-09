-- Serialize settlement on the lease so only its owner records usage, in the same transaction as release.
CREATE OR REPLACE FUNCTION starlight_settle(r jsonb) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM leases WHERE id = (r->>'lease')::uuid FOR UPDATE;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    PERFORM starlight_release((r->>'lease')::uuid, (r->>'tokens')::bigint, (r->>'micros')::bigint);
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
END $$;
