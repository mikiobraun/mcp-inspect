# mcp-inspect

A command-line tool to inspect and call MCP servers: see what a server offers,
read tool schemas, call tools with JSON, and debug connection and auth problems.

- Transports: stdio, streamable HTTP, legacy SSE
- Auth: OAuth (discovery, dynamic client registration, PKCE, refresh), static
  bearer tokens, bearer tokens from a command, arbitrary headers
- Saved servers with cached tokens, so you type `kb` instead of a URL
- Verbose modes that show every auth decision, JSON-RPC message, and HTTP request

Built on the official [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk).

## Install

From a checkout (Go 1.26+):

```sh
go install .             # into $GOBIN
go build -o mcp-inspect  # or locally
```

## Quick start

```sh
mcp-inspect add kb https://kb.example.com     # save; runs OAuth in the browser if needed
mcp-inspect info kb                           # server info, capabilities, instructions
mcp-inspect tools kb                          # tools with descriptions
mcp-inspect tool search kb                    # one tool: parameters, result schema
mcp-inspect call search kb --args '{"substring":"mcp"}'
```

## Targets

Every command that talks to a server takes a target:

| Target | Example |
|---|---|
| saved server | `mcp-inspect tools kb` |
| streamable HTTP | `mcp-inspect tools https://example.com/mcp` |
| legacy SSE | `mcp-inspect tools --sse https://example.com/sse` |
| stdio | `mcp-inspect tools -- npx -y some-mcp-server arg` |

Anything that is not a URL and not after `--` is looked up as a saved server name.

## Commands

| Command | |
|---|---|
| `info <target>` | server name, version, protocol, capabilities, instructions |
| `tools <target>` | all tools with descriptions (`--schema` adds input schemas, `--json` the list) |
| `tool <tool> <target>` | parameters in the server's order, types, defaults, constraints, nested objects, result schema (`--json` prints the definition as sent) |
| `call <tool> <target>` | call a tool, print the raw result |
| `add <name> <url>` / `add <name> -- <command>` | save a server, connecting once |
| `list` | saved servers and their auth status |
| `remove <name>` | delete a saved server and its cached auth |

### Calling tools

`--args` takes a JSON object inline, from a file (`@args.json`), or from stdin
(`-`). Without it, `{}` is sent. The result is printed as the server sent it.

Exit codes: `0` success, `1` protocol, connection, or auth error, `2` the tool
reported an error (`isError: true`; the result is still printed).

Together with [`jo`](https://github.com/jpmens/jo) and `jq` this makes a shell
wrapper for any MCP tool:

```sh
set -o pipefail
jo substring=volume-auth max_results=5 \
  | mcp-inspect call search kb --args - \
  | jq -r '.structuredContent.matches[].path'
```

Each call opens a new connection; cached OAuth tokens are reused.

## Auth

Connection flags apply to URL targets and to `add`, which saves them with the
server:

| Flag | |
|---|---|
| `-H 'Name: value'` | extra header (repeatable) |
| `--bearer TOKEN` | static bearer token |
| `--bearer-cmd 'CMD'` | run a command for the token; re-run once when the server rejects it |
| `--client-id`, `--client-secret` | pre-registered OAuth client instead of dynamic registration |
| `--scope` | OAuth scopes to request instead of the discovered ones |
| `--redirect-port` | local port for the OAuth callback (default 33418) |
| `--upgrade-redirects` | during OAuth, follow a same-host redirect from `https://` to `http://` as `https://` |
| `--sse` | legacy SSE transport |

Without a bearer token or an `Authorization` header, a 401 from the server
starts the OAuth flow: protected resource metadata, authorization server
metadata, dynamic client registration, then the authorization code flow with
PKCE. The authorization URL is printed and opened in a browser; the callback
goes to `http://127.0.0.1:33418/callback`.

On a headless machine, forward the callback port from the machine with the
browser:

```sh
ssh -L 33418:127.0.0.1:33418 headless-host
```

A trailing-slash difference between the server URL and the `resource` in its
metadata is tolerated.

Discovery refuses redirects from `https://` to `http://`. Servers behind a
TLS-terminating proxy sometimes send those by mistake, e.g. a framework that
strips a trailing slash from `/.well-known/oauth-authorization-server/` and
builds the redirect with the scheme it sees. `--upgrade-redirects` follows such
a redirect over `https://` if it stays on the same host; fixing the server
(trusting the proxy's `X-Forwarded-Proto`) is better.

## Files

Under the user config directory (`~/.config/mcp-inspect/` on Linux), all files
mode 0600:

| Path | |
|---|---|
| `servers/<name>.json` | saved server definition, including connection flags (may contain secrets) |
| `auth/<name>.json` | OAuth client and token for a saved server |
| `auth/url/<host_path>.json` | OAuth client and token for an ad-hoc URL |

Server definitions are plain JSON and can be edited by hand.

## Debugging

| Flag | Shows |
|---|---|
| `-v` | transport, auth decisions (cached token, refresh, registration, scopes), stdio server stderr, server log and progress notifications |
| `-vv` | + every JSON-RPC message |
| `-vvv` | + raw HTTP requests and responses, including OAuth discovery and token requests |

Secrets in logs are replaced by `<redacted sha256:…, N chars>`, so you can tell
whether a token changed without seeing it.

## License

MIT, see [LICENSE](LICENSE).
