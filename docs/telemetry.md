# Correlated request-phase telemetry

Phase telemetry is off by default. An empty `telemetry.endpoint` creates no producer, worker or collector connection.
An installation operator can enable a gateway collector explicitly:

```yaml
telemetry:
  endpoint: http://127.0.0.1:4318/v1/traces
  sample_per_million: 10000
  queue: 256
  batch: 32
  timeout: 1s
```

This produces OTLP/HTTP JSON, with the full trace URL configured by the operator. Configure the node's
`--telemetry-endpoint` as well for connected node records. The node uses matching sampling/queue/batch/timeout flags,
documented in [nodes](nodes.md#optional-phase-telemetry). Changes take effect at process startup. A bearer token can be
read from `telemetry.bearer_token_file` or `--telemetry-bearer-token-file`; HTTPS is required for bearer authentication
outside loopback. The exporter uses system TLS roots and does not follow redirects or consult HTTP proxy settings.

Remote export follows the existing cached gateway offline policy and the node's last authoritative control policy.
A node with no received policy denies remote export. Policy failures deny export. Literal loopback addresses and
`localhost` remain usable for a local collector in an offline installation. Other DNS names do not become loopback
exceptions. Explicit `policy` losses are visible in gateway exporter health and fixed-class warnings. A local collector's
own forwarding/storage policy is outside Starlight's exporter.

## Identity and privacy

The public listener already gives each request a fresh random `X-Request-Id`, ignoring client-sent IDs. The trace ID is
that ID's 128-bit hex suffix after `req_`, so existing request logs and the API response can identify the same trace.
Public trace context is restarted at this trust boundary. Each recorded operation gets a new random span ID. The
instrumented mTLS node call and the engine HTTP call carry the same trace ID and generated request ID, with child-span
parent links. Node context is accepted only behind the authenticated gateway boundary and locally downsampled.

Records contain trace/span/parent IDs, a fixed phase name, numeric kind/status, flags and local timestamps. The only
resource attribute is the fixed service name `starlight.gateway` or `starlight.node`. They contain no request body,
prompt, tool name/argument, credential, caller/project/model/target identifier, URL, hostname or error description.
Trace IDs are intrinsic correlation fields and never metric labels. Baggage and vendor state are excluded. Internal
context is not injected into provider requests. Durable identity/accounting audit remains separate.

The wire uses version-00 [W3C trace context](https://www.w3.org/TR/trace-context/) and the JSON encoding of
[OTLP/HTTP](https://opentelemetry.io/docs/specs/otlp/). IDs are hex, timestamps are decimal-string nanoseconds and enum
fields are numeric. An unsupported or malformed node parent produces a new context instead of an inference refusal.

## Measured boundaries

| Phase | Local measured interval |
| --- | --- |
| `gateway.request` | Start of the inference handler through return, including preparation and settlement |
| `gateway.prepare` | Authentication, body preparation, catalogue and policy work before admission |
| `gateway.admission` | One admission attempt, including any capacity queue wait |
| `gateway.dispatch` | Body/permit preparation and dispatch through response headers |
| `gateway.relay` | Relaying the upstream response body |
| `gateway.settlement` | The existing settlement database call |
| `node.request` | Authenticated gateway handler through return |
| `node.permit` | Permit/body/replay checks, target lookup and slot acquisition |
| `node.engine_request` | Engine HTTP setup, headers and body relay |
| `node.engine_headers` | Engine round trip through response headers |
| `node.engine_relay` | Copying and flushing the engine response body |

Gateway first-data/output/flush records are cumulative boundaries from the original inference-handler start. They are
present only when `metrics.stream_timings` already observed them under its own bounded parser contract. A missing boundary
stays unknown. These records share their gateway clock; they are not independent processing phases to add together.

Each elapsed duration uses that host's local monotonic clock. Its OTLP end timestamp is the local wall start plus that
measured duration, so an end-wall-clock step cannot become elapsed work. Different hosts' absolute timestamps can still
be skewed. Do not subtract them to infer network, prefill or decode time. The engine intervals measure its HTTP boundary;
internal engine scheduling, prefill, decode, GPU and rank phases remain unknown without engine-produced observations.
Unknown completion/finality stays an unset span status. A phase or first-output boundary does not prove metered success.

## Sampling and bounded delivery

The default is 10,000 per million, one percent. Zero records no spans. Sampling is deterministic over the random root ID,
so gateway and node thresholds agree for a shared trace. A received sampled flag cannot raise the node's configured
threshold. Full export is one million per million. Sampling is a probability threshold, not a request-rate quota.

Each producer has one worker and a fixed queue. Defaults are 256 records, batches of 32, a 200 ms flush tick and a
one-second collector attempt deadline. Maximum settings are 4,096 queued records, batches of 128 no larger than the
queue, and a five-second attempt timeout. The minimum timeout is 10 ms. Record strings have fixed phase/identifier
bounds. Collector response headers and body each cap at 64 KiB. Token files cap at 8 KiB and collector URLs at 2 KiB.

Inference attempts a nonblocking enqueue. A full queue drops that record immediately. The worker retries a retryable
failure once, respecting `Retry-After` only within its retry budget; a larger delay ends that batch's export. Partial
success is never retried. Rejected spans, failed exports, full queues, policy withholding and shutdown losses are counted.
Remote error payloads are not retained or logged. Warnings use fixed classes at most once per 15 seconds.

Shutdown cancels export, counts queued loss and bounds the wait. This pipeline is best effort. A trace can be partial
because of sampling, downsampling, disabled/old components, queue saturation, policy, failure or shutdown. Metrics in
[phase telemetry health](metrics.md#phase-telemetry-health) expose those cases. Do not treat exporter acceptance as durable
storage, billing history, complete request coverage or an inference SLO.

## Qualification

The independent test collector accepts only this fixed wire schema. Fixtures inject local gateway database waits and
node acquire/header/relay delays, check connected IDs and byte/accounting parity, and poison request/header inputs to
check privacy. Separate tests cover wall-clock skew, slow/offline collectors, queue limits, retry/partial replies,
response bounds, redirect/token handling and disabled/zero-sampled behavior.

The opt-in disabled/sampled/full profile is described in [qualification](qualification.md#correlated-phase-telemetry).
It measures a sequential fake-engine path on one host. The additional opt-in concurrent fixture runs with
`STARLIGHT_PHASE_TELEMETRY_CONCURRENT=1 go test -count=1 -run '^TestConcurrentPhaseTelemetry' -v ./internal/e2e`.
It uses 16 clients, 256 measured requests per arm, 16 warmups, a 32-slot fake engine and the default queue/batch settings.
Disabled/one-percent/full arms run in A-B-C-C-B-A order, reporting latency, request rate, actual complete/partial trace
coverage and gateway loss counters. A separate four-record queue case holds collector responses while the same traffic
completes, checks explicit drops, then releases the collector and verifies recovery. Both check exact choices/usage,
settlement counts, zero remaining leases and private-data canaries. This is controlled concurrent HTTP qualification,
not real-engine capacity, full production traffic coverage or a deployment SLO. Review latency and trace coverage together, then
measure the deployment's traffic and collector budget before choosing a sampling rate. It does not qualify a real engine,
concurrent fleet load, internal engine phase coverage, per-team destinations or durable performance records.
