# Governed MCP transport

Starlight can expose a default-off `POST /mcp` route for one configured tool server
and one project. It implements a bounded intermediary profile of
[MCP Streamable HTTP 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http).
Model responses that contain tool calls do not execute through this route automatically.

The profile supports `server/discover`, `tools/list` and `tools/call`. Discovery
describes this gateway's tools capability. Lists retain only configured tool names
and use private caching with zero lifetime. Invalid `x-mcp-header` definitions are
excluded individually. Custom `Mcp-Param-*` headers remain opaque and pass to the
upstream, which owns their schema and header/body validation. The gateway validates
the version, method and tool-name header/body pairs it recognizes.

Each request must contain the modern version, client identity and client capabilities
metadata, matching MCP headers and an Accept header for both JSON and SSE. Numeric
IDs must be exact integers within the IEEE754 safe range, with at most 128 characters
and exponent magnitude 64. String IDs may have up to 128 decoded bytes, including an
empty string. Duplicate JSON keys, batches and case aliases of structural fields
are rejected. The response must match the request ID and contain one final result
or RPC error. Tool results require a content array and `resultType: complete`.

SSE can contain bounded comments and requested `notifications/progress` messages
before the final response. LF, CRLF and CR line endings and an initial UTF-8 BOM are accepted. Progress tokens must match the originating request and progress must increase.
Independent server RPC, input-required results, logging, resources, prompts,
subscriptions and task extensions are outside this profile. Legacy initialize,
sessions, GET/DELETE streams and Last-Event-ID resumption are rejected. Modern
`ping` was [removed from this protocol revision](https://modelcontextprotocol.io/specification/2026-07-28/changelog).
No older-revision fallback is attempted.

## Configuration

Enable only after approving the tool server and its actions for the project.
Key IDs and project IDs below are placeholders, not credentials. Configuration
changes require an instance restart.

```yaml
mcp:
  enabled: true
  endpoint: https://tools.example.com/mcp
  internal: false
  project_id: 00000000-0000-4000-8000-000000000001
  key_ids: [00000000-0000-4000-8000-000000000002]
  tools: [get_weather]
  max_class: internal
  bearer_env: STARLIGHT_MCP_UPSTREAM_TOKEN
  max_concurrent: 2
  timeout: 30s
```

`bearer_file` is an alternative to `bearer_env`. The separately configured upstream
credential is read at startup, limited to 8 KiB and never copied from the caller.
An explicitly configured empty or malformed credential refuses startup. Omit both
fields only when the approved upstream needs no credential. No inline bearer value
is accepted in YAML. Protect the configuration, secret source and process environment
with the same deployment controls as other Starlight credentials.

The calling client uses its Starlight bearer API key. Only the configured key IDs
in the configured project can use the route, and their owner must remain active
with project access. An inference key alone grants no tool authority. Browser
sessions, Connect/access tokens, delegated task tokens and IdP JWTs are rejected.
This is a preconfigured API-key profile. It does not implement the MCP OAuth
discovery/delegation recommendation or claim compatibility with generic OAuth MCP
clients. See the [authorization specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization).

Bearer authority is checked before taking a tool slot or reading its body. Authority,
project classification and offline state are checked again after parsing for each request.
The strictest of project classification, key floor and requested classification
must fit `max_class`. Project secret screening applies to JSON and decoded MCP
parameter headers. Both `local` and `block` modes refuse detected credentials before
tool dispatch, including internal tool servers. Only an explicit project guard mode
of `off` disables this screening. Isolated projects cannot use external servers. `internal: true` is an explicit operator grant for
the fixed host to resolve to internal addresses and, if needed, use HTTP; it is not
inferred from the URL. MCP uses the direct variant of the existing guarded egress
client, ignoring HTTP_PROXY, HTTPS_PROXY, ALL_PROXY and NO_PROXY in either case.
It refuses redirects and unapproved/private addresses. Existing frontier-provider
clients keep their configured environment-proxy behavior. The shared guards are in
[providers/egress.go](../internal/providers/egress.go): NewDirectEgress disables proxy
lookup, CheckRedirect stops redirects and Egress.control checks resolved addresses.
Offline mode refuses all tool-server dispatch,
including internal servers. Present Origin headers must match the configured
public origin. The gateway never forwards caller Authorization, cookies or
idempotency keys. A key revocation prevents subsequent requests; an already
dispatched tool operation may continue independently of its connection.

## Bounds and execution outcomes

Requests are limited to 64 KiB and replies to 8 MiB. Both directions use JSON depth 32
and 8192-value limits. Individual incoming and encoded outgoing SSE events are
limited to 1 MiB, with at most 4096 frames including keepalives. Incoming wire bytes
and encoded outgoing bytes each have a separate 8 MiB total bound, including
comments and frame overhead. Multi-line JSON data is compacted into one data line
without changing JSON values. Timeout must be 1s through 1m and covers authorization,
request-body reading, policy checks, upstream exchange and client writes. Each
instance permits 1 through 64 concurrent authorized requests and queues none.
Excess authorized requests receive 429. These are per-instance limits, not a shared
fleet tool budget. Inference-token limits do not price or meter tool work.

The gateway makes one application-level upstream request and never retries,
redirects, resumes or replays it. Disconnect cancels/closes the upstream exchange.
Cancellation cannot undo a side effect that the tool already performed. A timeout,
connection failure, truncated stream or lost response can leave execution unknown.
If a stream has started, failure closes it without inventing a final result. A
client reconnecting with another POST starts a new attempt, even with the same RPC
ID. A client retry after a 502 can run the tool twice. The route offers no
exactly-once tool execution or idempotency service.

## Accounting and qualification

Every request receives a generated attempt id, independent of middleware and the
client RPC id, echoed in X-Starlight-MCP-Attempt-ID. A durable `mcp.begin` event
with that id is required before forwarding. audit.Log completes its database
transaction before returning; a commit error prevents dispatch. A separate
`mcp.end` event records the transport outcome, elapsed milliseconds and whether
dispatch was possible. The latter flag is conservative: obtaining a connection
can establish possibility without proving that the tool executed. `response_complete`
means a valid final RPC response was relayed, including a tool or RPC error; it does
not mean the tool succeeded. Audit fields include principal/key/project IDs and
configured tool name and attempt id, never tool arguments, results, bearer values
or endpoint URLs. Bad credentials, ungranted tools and policy refusals create
content-free `mcp.deny` records with a status and attempt id. Anonymous refusals
carry no claimed user identity. Denial recording has a 1s bound; an unavailable
audit database still refuses the request and logs a fixed accounting error.
Inference `usage_events`, token settlements and budgets receive no synthetic rows.

JSON replies and SSE messages are also refused if a decoded JSON string contains
the configured upstream bearer, including JSON-escaped spellings. This prevents
literal credential reflection; it does not identify every possible encoding.

Completion accounting has a separate 2s cleanup bound and can fail after a response
or process crash. Such a failure emits a fixed server error message. A begin event
without a matching end is an unresolved attempt, not proof of no execution or zero
cost. Reconcile those attempts with the approved upstream's records before charging
tool costs. No tool price, production recovery objective or outside-server
conformance is established by the controlled fixtures.

Run the focused fixtures with an isolated Postgres test database:

```sh
go test -race ./internal/config ./internal/mcpgateway
go test -race ./internal/e2e -run 'TestGovernedMCP|TestMCPRoute'
```

The fixtures use a loopback tool server and generated test credentials. They cover
policy denials, key revocation, membership changes, credential boundaries, separate
audit accounting, bounded streams/deadlines, cancellation and ambiguous execution
without an automatic duplicate request. They do not call a real outside tool.
