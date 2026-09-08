# mcp-defer

Start an MCP stdio server the first time a tool is called, not when the
session starts. Until then, `initialize` and the tools, prompts and
resources lists are answered from what the server said the last time it
ran. A server whose start is slow, or asks you for a credential, costs
nothing in the sessions that never use it.

```json
{
  "mcpServers": {
    "prod-db": {
      "command": "mcp-defer",
      "args": ["--", "/path/to/prod-db-wrapper"]
    }
  }
}
```

The wrapper here fetches a connection string from a password manager,
which prompts for Touch ID, and then `exec`s the database server. With
`mcp-defer` in front, the prompt appears when the model first calls one
of the database tools, and the prompt is the ask: approve it and the
call goes through, deny it and the call fails with the server's last
words, ready to be asked again.

## Install

```sh
brew install --cask stefanahman/tap/mcp-defer
go install github.com/stefanahman/mcp-defer@latest   # with Go 1.25
```

or from a checkout, `make install BIN=~/.local/bin`.

## Usage

```
mcp-defer [-name NAME] [-cache DIR] [--] COMMAND [ARG...]
```

| | |
|---|---|
| `-name NAME` | the cache entry to use; default: the base name of `COMMAND`, so two wrappers with the same file name need distinct names |
| `-cache DIR` | where entries live; default: `$XDG_CACHE_HOME/mcp-defer`, else the OS cache directory |
| `--` | needed when `COMMAND` itself starts with a dash |

The command runs with mcp-defer's environment and working directory,
so an `env` block in the client's configuration reaches it unchanged.

## What happens in a session

1. The client sends `initialize`; mcp-defer answers from the cache.
   Tool, prompt and resource lists are served the same way. Nothing has
   been started.
2. The first request that needs the server, a tool call, a resource
   read, a prompt, starts `COMMAND`, runs the MCP handshake with it, and
   forwards the request. Requests that arrive while it starts wait, in
   order.
3. Once the server is up, mcp-defer fetches its real lists. If one
   differs from the cache, the cache is rewritten and the client gets
   the matching `list_changed` notification, so it refreshes without a
   reconnect. From here on every message is forwarded both ways,
   including the server's own notifications and requests.
4. When the client closes stdin, or ends mcp-defer with `SIGINT`,
   `SIGTERM` or `SIGHUP`, a handshake or refresh under way gets up to
   two seconds to finish, then the server's process group gets
   `SIGINT`, and `SIGKILL` two seconds after that. The group matters: a
   wrapper script waiting on a credential prompt would otherwise keep
   the prompt, and the real server, alive after the session is gone.

**First run.** With no cache entry yet, the server starts at
`initialize`, so the very first session pays the start once and writes
the entry. A cache entry is also only used for the protocol version the
client asked for last time; a different version starts the server
eagerly and re-records.

**If the server dies**, from a denied credential prompt or a crash, the
requests it owed get a JSON-RPC error carrying its last lines of stderr,
and the next request starts it again. mcp-defer itself stays up. A
server that died before it was ready gets a five-second cool-down, and
the error tells the model the user may have declined a prompt and to
ask before retrying, so a retry loop can't turn into a prompt storm.

## The cache

One JSON file per name: the `initialize` result, and each declared list
in full, pagination folded. Tool schemas and server info; never a
result, never a credential. Delete the file to forget a server; the next
session re-records it.

## What the model gets to see

Everything the server sends it, as before, plus the last three lines of
the server's stderr when a start fails. That is what lets the model tell
"declined" from "broken". A wrapper must therefore not print secrets to
stderr; `set -x` in a shell wrapper would.

mcp-defer itself writes to stderr only: which request started the
server, when it answered from the cache and how old that was, when it
rewrote the cache, and lines from the server that were not messages.
Claude Code shows that log under `/mcp`.

## Limits

Linux and macOS. One server per mcp-defer, stdio only. JSON-RPC batches,
which the 2025-06-18 revision removed, are not forwarded. The server is not stopped when
idle; a client keeps it for the session. A client's health check, such as
`claude mcp get`, reaches the cache and reports the proxy as connected
whether or not the server behind it can still start; the first call
tells. A server that must be running
to be useful, one that pushes events to the client on its own, gains
nothing from being deferred.

## Hacking

```sh
make test   # a fake MCP server, built once per run, plays the backend
make lint   # gofmt and vet; CI adds staticcheck, govulncheck and a Windows vet
```

## License

MIT
