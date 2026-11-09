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
freshness and per-field validity. No routing policy changes in this feature. Future routing consumers must apply the
same age rule at decision time rather than treating the last Prometheus value as current indefinitely.

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
qualified observations. This negotiated reporting, storage and dashboard behavior applies even when the exporter flag
is false; the flag selects only the optional scrape-time collector.
The existing `replicas.live` JSON stores the latest observation; no schema migration or extra retention table is needed.

## Not exported as series

Per-project and per-person usage and spend remain in Postgres and the console. Node leaf certificate lifetimes remain
available through `starlight doctor`; public TLS and workload-CA expiry are exported in the fleet series above.

## Alerts and dashboard

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
