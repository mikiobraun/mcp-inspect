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
./mcp-inspect -vvv info <target>     # debug a connection
```

There are no automated tests yet. Changes have been verified manually against
the go-sdk example servers (`go build github.com/modelcontextprotocol/go-sdk/examples/server/everything`,
`.../sse`, `.../toolschemas`) and small throwaway servers (a fake OAuth
authorization server with a protected MCP endpoint, a stdio server with
hand-written schemas). When touching auth, test fresh auth, cached token,
expired token refresh, rejected refresh token, and rejected access token.

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
