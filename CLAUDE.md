# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

`mcp-inspect` is a CLI to inspect and call MCP servers (stdio, streamable HTTP,
legacy SSE) with explicit control over auth and detailed debug output. User
documentation is in README.md.

## Commands

```sh
go build -o mcp-inspect .
go vet ./...
go test ./...
MCP_INSPECT_UPDATE_GOLDEN=1 go test -run TestToolRendering .   # rewrite testdata/*.golden
./mcp-inspect -vvv info <target>     # debug a connection
```

## Tests

- `unit_test.go`: schema parsing and type summaries, wrapping, raw result
  matching, redaction, auth cache paths.
- `cli_test.go`: the real CLI (`rootCmd`) end to end over stdio, streamable HTTP
  and SSE: tools, `tool` rendering (golden files), `call` and exit codes, `--args`,
  saved servers, target errors.
- `oauth_test.go`: auth flows against the fake authorization server, asserting
  the server-side event sequence: fresh flow, cached token, refresh, rejected
  refresh or access token (re-authorize without re-registering), trailing-slash
  resource, no retry after a failed authorization, saved servers, bearer-cmd
  rotation, rejected static bearer.

Test infrastructure:

- `internal/testservers`: `NewSchemaServer` (tools with feature-rich schemas,
  echo, a failing tool) and `AuthServer` (fake OAuth server in front of it,
  with an event log). Its issuer lives under `/as`, so legacy root discovery fails.
- `testdata/fakeauth`, `testdata/schemaserver`: the same servers runnable by hand.
- Stdio tests re-execute the test binary as the schema server (`TestMain`,
  `MCP_INSPECT_TEST_STDIO_SERVER`).
- `isolateConfig` points the config dir at a temp dir; `openBrowser` is replaced
  by an HTTP client that follows the authorization redirects.
- Commands write to `cmd.OutOrStdout()` so tests can capture output.

## Layout

Single package `main`, one file per concern:

- `main.go` — cobra root, `tools`, `info`, `withSession`, output helpers
- `servers.go` — `target` (saved as `servers/<name>.json`), config paths, target resolution, `add`/`list`/`remove`
- `connect.go` — transport setup, SDK client options, JSON-RPC logging, `rawResults` recorder
- `httpauth.go` — `authRoundTripper`, static and command bearer handlers
- `oauth.go` — OAuth handler setup, token cache, browser callback, trailing-slash resource fix
- `httplog.go` — `-vvv` HTTP logging with secret redaction
- `tool.go` — `tool` command, order-preserving JSON schema parsing and rendering
- `call.go` — `call` command, `--args` parsing, exit codes
- `log.go` — verbosity levels, `logf`, `redact`

## Design decisions

- **Auth lives in an `http.RoundTripper`**, not in the SDK's
  `StreamableClientTransport.OAuthHandler`, so streamable HTTP and SSE behave the
  same. It follows the `auth.OAuthHandler` contract: token on every request, one
  `Authorize` and retry on 401/403.
- **A failed authorization is never retried** in the same process. The SDK keeps
  sending after failures (fallback `initialize`, `notifications/cancelled` on a
  context Ctrl+C doesn't cancel), and each would otherwise start a new OAuth flow.
- **Trailing-slash tolerance**: the SDK compares the metadata `resource` with the
  endpoint as strings and silently ignores mismatches. `resourceMatchingHandler`
  reads the metadata URL from `WWW-Authenticate` and adjusts only for a trailing slash.
- **Redirect upgrade** (`--upgrade-redirects`, opt-in): the SDK's discovery
  client refuses https-to-http redirects. `redirectUpgradingRoundTripper` rewrites
  a same-host `Location` to https in the OAuth client's transport, so the SDK's
  own redirect checks (limit, loopback and private-address guards) still apply.
  Because of the loopback guard, tests can only show the upgraded redirect
  reaching that check, not a full flow.
- **Raw results**: the SDK decodes results into Go maps, losing key order.
  `loggingConn` records raw JSON-RPC results per method; `tool` parses them with
  an order-preserving decoder, `call` prints them verbatim.
- **Saved servers vs ad-hoc targets**: connection flags are rejected for saved
  names (edit the JSON instead). Auth caches are keyed by name, or by URL for
  ad-hoc targets. A cache whose `server_url` differs from the target is ignored.
- **Client capabilities are empty**: no roots, sampling, or elicitation.
- Streamable HTTP runs with `DisableStandaloneSSE` and no reconnect retries: one-shot
  inspection doesn't wait for server-initiated messages.
- Files that may hold secrets are written 0600 in 0700 directories.

## Conventions

- Prefer explicit failures over guessing (e.g. `--` is required for stdio, no
  auto-fallback from streamable HTTP to SSE, no type inference for arguments).
- Every auth or transport decision gets a `logf(vLifecycle, ...)` line; secrets
  go through `redact`.
