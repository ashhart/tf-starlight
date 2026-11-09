# Controlled routing replay

This tool compares the current affinity/headroom picker with the advisory completion-effort score using identical
synthetic arrivals, assumed phase rates and explicit cache behavior. It makes no engine requests. Its output cannot
establish real-device latency, capacity, cache hit rates or a live routing improvement.

Run from the repository root:

```sh
go run ./tools/routing-replay -input tools/routing-replay/testdata/mixed.json
```

The four supplied traces cover mixed prompt/output lengths, cold unequal devices, a warm-prefix regression and stale
observations. Each arm starts with an empty cache, uses the same request sequence and its own copy of the same random
seed. There is one single-worker FIFO queue, and every target is eligible. Priority, cancellations, admission timeouts,
model loading and network behavior are outside this simulation.

The baseline calls the same picker as live admission. The effort arm replaces a ready advisory comparison with its
proposed target. When measurements are unavailable, it keeps the baseline choice. A target has one cache entry, the key
of its most recently completed request. An equal nonzero `cache_key` discounts prompt service by the target's declared
`warm_prompt_ratio`; zero means no cache simulation. The evaluator receives no cache inventory or cache credit. A warm
prefix can therefore regress under the effort arm. This is a required negative control before any policy promotion.

Input must be one strict JSON document, at most 4 MiB. Unknown fields, including request bodies, are rejected. Use generated
opaque IDs and cache/affinity keys, not real user identifiers or prompt hashes. Request work is an explicit assumption,
not observed token usage. Limits are 2 to 128 unique targets, 1 to 10,000 requests in nondecreasing arrival order and a
one-year maximum simulated time. Service rates must be finite and positive; token totals must fit 2^53 - 1.

| Field | Meaning |
| --- | --- |
| `version`, `seed` | Schema version 1 and deterministic tie-break seed |
| `slo_seconds` | Arrival-to-completion threshold for SLO goodput |
| `starvation_seconds` | Finite queue-wait threshold; exceeding it counts as starved |
| `targets[].id` | Opaque target identifier |
| `targets[].prompt_rate`, `decode_rate` | Assumed prompt and decode tokens per second |
| `targets[].warm_prompt_ratio` | Prompt service multiplier on a simulated cache hit, greater than zero and at most one |
| `targets[].observation_age_seconds` | Age presented to the evaluator; 15 seconds or more is unavailable |
| `requests[].arrival_seconds` | Arrival offset in the synthetic trace |
| `requests[].prompt_tokens`, `completion_tokens` | Declared work, both nonnegative with a positive total |
| `requests[].affinity`, `cache_key` | Opaque unsigned integer controls for the picker and simulated cache |

Each arm reports completed requests, SLO completions/goodput, throughput, p95 arrival-to-completion time, p95/max queue
wait, starvation-threshold violations, ready comparisons, disagreements, fallback count, simulated cache hits and actual
simulated service divided by estimated effort. The ratio is null when no valid estimate exists, with a separate sample
count. Rates divide by elapsed time from first arrival to final completion. Percentiles use nearest rank. Starvation here
means a declared finite threshold violation, not a proof about infinite scheduling fairness.

Each CLI report includes the SHA-256 digest of the exact input bytes. Keep its source commit and output with any comparison. Read positive and negative controls together.
The stale trace must preserve baseline behavior and have no estimation-error sample. The hot-prefix trace deliberately
shows the cost of missing cache information. The mixed trace includes burst/queue effects and alternating hot/cold keys.
A future live policy needs separate device measurements, source review and a rollback to the existing picker.
