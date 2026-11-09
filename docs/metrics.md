# Metrics

Starlight serves Prometheus metrics on the metrics listener (`listen.metrics`, `127.0.0.1:9090` by default, `:9090` in the
container image) at `GET /metrics`. Set `metrics.bearer_token_file` to require `Authorization: Bearer <token>`. Labels
come from the catalogue and fixed vocabularies, never from people, keys or prompts, so cardinality stays bounded by the
number of models and deployments. Optional replica observations have a separate fixed scrape cap below.

## Request series (per instance)

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `starlight_requests_total` | counter | `model`, `protocol`, `route`, `status`, `outcome` | Finished inference attempts |
| `starlight_request_duration_seconds` | histogram | `model`, `route` | Arrival to last byte |
| `starlight_first_byte_seconds` | histogram | `model`, `route` | Legacy boundary: response-header stage for SSE, complete buffered body for JSON |
| `starlight_upstream_first_data_seconds` | histogram | `model`, `route`, `outcome` | Arrival to the first positive upstream SSE body read, including comments or metadata |
| `starlight_first_model_output_seconds` | histogram | `model`, `route`, `outcome`, `payload` | Arrival to the first recognized generated payload in a complete SSE event |
| `starlight_first_model_output_flush_seconds` | histogram | `model`, `route`, `outcome`, `result` | Arrival to the first generated payload's flush attempt |
| `starlight_queue_wait_seconds` | histogram | `model`, `priority` | Time queued for capacity, for requests that queued |
| `starlight_tokens_total` | counter | `model`, `route`, `direction` | Tokens; `direction` is `prompt`, `completion` or `cached` |
| `starlight_admission_rejections_total` | counter | `reason` | Requests refused before dispatch |
| `starlight_inflight_requests` | gauge | `route` | Requests this instance is streaming now |

Label values:

- `route`: `local` or `cloud`.
- `protocol`: `openai.chat`, `openai.completions`, `openai.embeddings`, `openai.responses`, `anthropic.messages`,
  `anthropic.count_tokens`.
- `status`: `2xx`, `4xx`, `5xx`, or `other` for attempts that never received a status.
- `outcome`: `ok`, `ok_unmetered`, `timeout`, `client_error`, `upstream_error`, `upstream_unreachable`, `interrupted`, `cancelled`, `revoked`, or
  `not_dispatched` for an attempt refused by a node or provider before it started and then retried elsewhere.
- `priority`: `interactive`, `standard`, `batch`.
- `reason`: the error code for policy refusals (for example `invalid_json`, `model_not_found`, `credentials_detected`,
  `context_length_exceeded`, `permission_denied`), `unauthenticated`, `project_required`, `concurrency_limit`,
  `rate_limit`, `budget_limit`, `capacity`, `no_route`, `client_left`, `internal`.

Histogram buckets are 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 15, 30, 60, 120, 300 and 600 seconds.

### Stream observation boundaries

Set `metrics.stream_timings: true` to enable the three new stream series. The default is false, retaining the existing
relay path without the new payload observer. Enable it for a qualification run, measure collector overhead on your
traffic, then keep it enabled where the observation budget permits. Turning it off does not change legacy metrics.

The three new stream series use the gateway's local monotonic clock from request arrival. `payload` is `text`,
`reasoning` or `tool`; a tool name or nonempty argument fragment counts, but a tool ID, role, usage-only event,
heartbeat, signature or lifecycle event does not. The observer recognizes OpenAI Chat/Completions text, reasoning,
refusal and function/tool deltas; Responses text, refusal, reasoning, function/custom-tool/MCP argument deltas and
named function/custom-tool items; and Anthropic text, thinking and tool-use blocks/deltas. Other payload types remain
unobserved. A delta containing both content and reasoning is labeled `text`. Multi-line SSE data fields are joined for
observation without changing the bytes forwarded to the client. Inspection stops at `[DONE]`, after 64 complete data events,
or before examining more than 256 KiB of complete event bytes. Output that does not match within this budget remains
unknown; forwarding and legacy usage metering continue normally. Comment-only heartbeats and Messages `ping` data events
count toward the byte budget, but do not consume the data-event budget.

An incomplete event at EOF cannot establish first model output. A stream with no recognized output has no model-output
or model-flush sample; it is not recorded as zero latency. JSON responses have no new stream samples. A stream that
produces output and then fails still has its observed samples, labeled with its final `outcome`. Those samples do not
prove completion, metered usage, token boundaries or a token generation rate.

The flush series records the start of the flush attempt after the event is written. Its `result` is `ok` or `error`.
A write failure has no flush sample. A successful flush reports what the gateway did; network buffering and the client's
receive time require a separate client receipt. Headers alone cannot satisfy model-output timing.

The existing first-byte series and `usage_events.ttft_ms` retain their earlier boundary for compatibility. Historical
records must not be reinterpreted as first generated token measurements. `ended` in
[relay.go](../internal/gateway/relay.go) returns `ok_unmetered` when EOF arrives without observed usage; that existing
outcome does not establish successful token accounting. Use the model-output series for a declared
stream-payload SLO and keep its payload type and final outcome in the receipt.

The new histograms add outcome and payload/result label combinations to the existing model/route cardinality.
For each model/route there is one data histogram per observed outcome, up to three output histograms per outcome,
and up to two flush histograms per outcome. Each histogram has the same buckets as the legacy series. Catalogue size,
outcome counts and these fixed vocabularies determine the additional series; payload content is never a label.

The recognized event shapes are documented in the official
[OpenAI streaming reference](https://developers.openai.com/api/reference/resources/responses/streaming-events),
[Chat streaming reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events)
and [Anthropic streaming reference](https://platform.claude.com/docs/en/build-with-claude/streaming).

### Qualification against a real engine

The opt-in `TestRealEngineStreamTimings` gate uses an already-running loopback engine without changing it. Set
`STARLIGHT_STREAM_GATE_URL`, `STARLIGHT_STREAM_GATE_MODEL` and `STARLIGHT_STREAM_ENGINE_SHA` to the server URL, served
model name and full engine revision, then run:

```sh
go test -run '^TestRealEngineStreamTimings$' -count=1 -v ./internal/gateway
```

If authentication is required, supply `STARLIGHT_STREAM_GATE_KEY` through the process environment. The gate does not
print it. It sends one short Chat request and forced `get_weather` tool requests through Chat, Responses and Messages.
Each request crosses the real relay and a loopback client with an independent map-based payload decoder and SSE parser.
It checks the same first output event and kind on both sides, identical forwarded body bytes, a terminal event,
recognized output, a successful flush and causal clock order. Tool cases must contain a tool name or argument payload;
a model may emit reasoning first, which remains the first output boundary.

Each `stream_gate_receipt` JSON line contains the fixture name, requested engine revision, body digest/byte count,
observed payload kinds and event indices, metering outcome and separate gateway/client timings. It contains no prompt, response body,
endpoint or credential. The declared revision is an input; the run owner must separately verify the actual server
binary's source revision and hash, model/checkpoint identity and hardware. The client reads SSE events, so its first
body timestamp bounds first-byte arrival from above. HTTP chunk framing is outside the body-byte comparison. These
receipts qualify boundary observation, not model quality, token speed or a before/after performance claim.

## Fleet series (installation-wide)

Every instance samples these from Postgres every 15 seconds, so all instances report the same values. Aggregate them with
`max`, not `sum`.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `starlight_nodes` | gauge | `status` | Nodes by status: `active`, `pending`, `draining`, `disabled`, `revoked`, and `stale` for active nodes not seen for 90 seconds |
| `starlight_replicas` | gauge | `deployment`, `state` | Replicas by deployment and state |
| `starlight_queue_depth` | gauge | `model` | Requests waiting for capacity across all gateways |
| `starlight_controller_epoch` | gauge | none | The fencing epoch on the current leader, 0 on every other instance |
| `starlight_certificate_expiry_timestamp_seconds` | gauge | `certificate` | When the `public` certificate and the `workload_ca` expire, as Unix time; the public pair is re-read from disk within a minute of changing |

The Go runtime and process collectors (`go_*`, `process_*`) are also exported.

## Replica work observations

Set `metrics.replica_observations: true` to register a scrape-time collector. It is off by default. Each scrape queries
at most 1,025 eligible replicas and exports at most 1,024. Only ready replicas desired to run, in active deployments on
active nodes seen within 90 seconds, are eligible. The query has a two-second timeout. The collector has no retained
replica-label cache; stopped, removed and ineligible replicas disappear on the next successful scrape.

| Series | Labels | Meaning |
| --- | --- | --- |
| `starlight_replica_decode_tokens_per_second` | `model`, `deployment`, `replica` | Growth in completed plus still-streaming generation tokens per second |
| `starlight_replica_prompt_tokens_per_second` | same | Growth in prompt tokens per second |
| `starlight_replica_running_requests` | same | Measured running request count |
| `starlight_replica_waiting_requests` | same | Measured waiting request count |
| `starlight_replica_kv_usage_ratio` | same | Maximum measured KV use ratio across engine metric label sets |
| `starlight_replica_observation_age_seconds` | same | Age of the node's measurement, absent without a timestamp |
| `starlight_replica_observation_fresh` | same | One for a current timestamped observation, otherwise zero |
| `starlight_replica_observation_valid` | same plus `measurement` | One when that field is known, valid and current, otherwise zero |
| `starlight_replica_observation_scrape_success` | none | One after a complete query, zero on query/scan failure |
| `starlight_replica_observation_truncated` | none | One when eligible replicas exceed the cap |

All these series are gauges. `measurement` is `decode`, `prompt`, `running`, `waiting` or `kv`. The maximum is 12,290
samples per scrape, excluding other Starlight collectors. Replica IDs are installation-generated identifiers. The cap
bounds current collector memory and series, while your Prometheus retention and relabel policy govern historic UUID
churn. Installation-wide observations repeat on each gateway; aggregate replicas with `max` across gateway instances.
The cap selects replicas in ID order, so a truncated scrape is incomplete and cannot describe total fleet capacity.

Nodes retain the engine measurement time. First samples, counter resets, engine restarts, replaced metric label sets,
missing counters and nonpositive or greater-than-15-second rate windows produce unknown rates. Invalid numbers and KV
ratios outside zero to one stay unknown. Request counts must fit a signed 32-bit database count. Counter values above
2^53 - 1 are unknown because float64 cannot preserve unit increments there. Reported rates cannot exceed that counter
limit divided by one nanosecond. These representation bounds do not describe serving capacity. Measured zero remains zero. Data expires at 15 seconds, including while a
node continues syncing. Duplicate, reordered and expired reports cannot refresh a prior sample's age. A report must match the current assignment
generation and cannot predate the replica's current ready period. A failed scrape
exports its failure gauge without replaying old replica values.

Missing, expired and future-dated measurements have no work-value sample. Future timestamps up to five seconds ahead
may be stored but are not current until their time arrives; larger clock skew is rejected. Consumers must check
freshness and per-field validity. The shadow evaluator applies the same age rule at comparison time. Live routing policy remains unchanged; a last
Prometheus value cannot establish current work indefinitely.

The console uses the same measurement time and expiry rule, shows unknown fields as a dash, and appends a live chart
point only when a rate is known. Fleet, model and node sums are known only when every fresh report supplies the field.
`decode_known` and
`prompt_known` distinguish incomplete coverage from measured idle work. Expired reports are excluded, so these sums
are not a complete capacity measurement of every deployed replica. Per-replica validity series expose individual gaps.

New nodes send observation metadata only after the server advertises support in sync. Older servers continue receiving
the previous report shape. If a mixed-version listener rejects the advertised observation shape, the node retries once
with legacy fields and keeps legacy mode until the next successful capability sync.
An upgraded server preserves reports without observation metadata as legacy values, with receipt-time freshness.
The console marks these reports as legacy, and its JSON carries `legacy: true`; their zeros cannot distinguish idle
work from first samples or resets. They are not qualified work samples in the exporter. Upgrade nodes to restore
qualified observations. JSON consumers must check `legacy` before interpreting a complete aggregate as measured work:
legacy zeros count as complete field coverage. A mixed-version downgrade emits one content-free warning per control client.
This negotiated reporting, storage and dashboard behavior applies even when the exporter flag
is false; the flag selects only the optional scrape-time collector.
The existing `replicas.live` JSON stores the latest observation; no schema migration or extra retention table is needed.

## Shadow routing observations

`gateway.routing_shadow` defaults to false. Enabling it records advisory comparisons for actual committed leases while
serving continues through the existing picker. It does not require `metrics.replica_observations` to be enabled.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `starlight_routing_shadow_decisions_total` | counter | `model`, `reason`, `agreement` | One result per finished actual lease attempt |
| `starlight_routing_shadow_effort_seconds` | histogram | `model`, `choice` | Heuristic effort for `actual` or `proposed` when the comparison is ready |
| `starlight_routing_shadow_observed_estimated_ratio` | histogram | `model` | Successful observed actual-route elapsed seconds divided by its estimated effort |
| `starlight_routing_shadow_results_total` | counter | `model`, `result` | Actual-route qualification as `observed`, `unobserved`, `failed` or `invalid` |

`agreement` is `same`, `different` or `unavailable`. Reasons are `ready`, `no_alternative`, `missing_actual`,
`too_many_candidates`, `invalid_work`, `invalid_capacity`, `unknown_observation`, `unknown_backlog`, `untracked_work`,
`unknown_rate`, `invalid_score`, `cloud_target` or the defensive `invalid`. Availability counters include unavailable
comparisons; effort/error samples require `ready`. A single candidate has no comparison sample. Models come from the
catalogue; target, caller, request and cache identities never become labels.

The actual-route timer runs from successful admission to finished relay, before settlement SQL. Failed or unmetered
attempts cannot become prediction-error samples. The ratio includes gateway/node overhead, the existing prompt estimate,
the output limit rather than predicted output length, outstanding reservations and uncredited cache effects. A score is
not a latency upper bound. No proposed-route completion or goodput is inferred from a disagreement or a smaller score.
The ratio uses buckets 0.01, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 64 and 256; effort uses the request histogram buckets.

## Not exported as series

Per-project and per-person usage and spend remain in Postgres and the console. Node leaf certificate lifetimes remain
available through `starlight doctor`; public TLS and workload-CA expiry are exported in the fleet series above.

## Phase telemetry health

An explicitly configured `telemetry.endpoint` enables fixed exporter-health series on gateway instances. The collector
is independent of request metrics and [replica work observations](#replica-work-observations). With no endpoint there is
no phase producer, worker or collector traffic, and these optional series are not registered.

`starlight_phase_telemetry_{enqueued,sent,queue_full,failed,rejected,shutdown,invalid,requests,retries,partial,policy}_total`
are counters without labels. `sent` counts accepted spans, `rejected` counts collector partial rejections, `failed` counts
spans lost after failed export, and `queue_full`/`shutdown` count explicit local losses. `policy` counts spans withheld by offline mode. `requests`/`retries` count HTTP
attempts, while `partial` counts partial-success replies even when they reject zero spans. None contains remote error
text, collector URLs, tokens, trace IDs, callers or request content.

`starlight_phase_telemetry_queue` is the current queued record count. The success-timestamp gauge
`starlight_phase_telemetry_success_timestamp_seconds` refreshes only when a reply accepts at least one span and remains
zero before first acceptance. Alert on drops, rejections, failures and a stale acceptance timestamp when sampled work is
expected. Unsampled or idle traffic supplies no acceptance-freshness guarantee. Gateway and node exporter warnings use
fixed classes at most once per 15 seconds; the node keeps the same bounded counters internally.

Phase records are best-effort performance data. A trace can be partial because a component is old, disabled, downsampled,
full or offline. Their transport is separate from durable identity and accounting audit. See [phase telemetry](telemetry.md)
for fields, sampling, local-clock boundaries and export limits.

## Alerts and dashboard

With `gateway.routing_shadow` enabled, `starlight_routing_shadow_token_error_tokens` records the absolute difference
between a prompt estimate or output reservation and the actual reported count. Labels contain only catalogue `model`,
`phase` and `direction`, where `under` means the estimate was below the reported count. Explicit equal counts record zero.
The gateway records only successful local generation with successful settlement and one complete explicit usage pair.
Chat/completions require `prompt_tokens` and `completion_tokens`; Responses require completed input/output counts.
Missing, null, ambiguous, invalid, failed or unsupported observations produce no sample. Cloud, Anthropic, embeddings
and token-count endpoints are outside this initial envelope. Disabled shadow mode does not parse calibration fields.
Duplicate or case-fold-colliding keys, contradictory usage locations, malformed frames and explicit stream errors disqualify the request for
token calibration even if a later frame supplies usage. Diagnostic parsing stops above 1 MiB, 4,096 values/keys or
64 nested levels; exceeding a limit leaves the observation unknown. These checks do not alter ordinary metering.

These are aggregate token-count diagnostics across served revisions, not billing records, a fitted predictor, a memory
demand model or a calibration guarantee. Prompt counts include any reported cached tokens. The output estimate is a
reservation cap, so a short actual reply can produce large overestimation. Storage, pinned/device bytes, actual prefill
work and future demand cannot be inferred from this histogram. Admission limits and live routing remain unchanged.

`deploy/prometheus/rules/starlight.yaml` alerts on an instance down, no controller leader, stale or pending nodes, failed or
lost replicas, a deep queue, a server-error ratio above 5%, a surge in admission refusals, capacity exhaustion,
`no_route` refusals, high queue waits, a surge in invalid credentials, and a certificate within 14 days of expiry. `deploy/grafana/dashboards/starlight.json` charts
the existing fleet and request series. The optional replica series can be queried separately. Thresholds are starting points; tune them to your traffic.

Example queries:

```promql
sum by (model) (rate(starlight_tokens_total{direction="completion"}[5m]))
histogram_quantile(0.95, sum by (le, model) (rate(starlight_first_byte_seconds_bucket[5m])))
histogram_quantile(0.95, sum by (le, model, payload) (rate(starlight_first_model_output_seconds_bucket{outcome="ok"}[5m])))
sum(rate(starlight_requests_total{route="cloud"}[1h])) / sum(rate(starlight_requests_total[1h]))
max by (deployment) (starlight_replicas{state="ready"})
```

The captured-engine regression fixture covers the six selected families from engine
`22c46c10aa3b222e46e77f94189b81e4c20d0dfb`. Its 20.7-second interval exceeds the 15-second rate window: the
positive token totals parse correctly and idle gauges are known zero, while rates remain unknown. It does not
qualify a positive rate over an in-window real-engine capture.
