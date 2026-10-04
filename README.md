# mcp-defer

Start MCP servers on their first tool call, not at session start.

[![ci](https://github.com/stefanahman/mcp-defer/actions/workflows/ci.yml/badge.svg)](https://github.com/stefanahman/mcp-defer/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/stefanahman/mcp-defer)](https://github.com/stefanahman/mcp-defer/releases)
[![license](https://img.shields.io/github/license/stefanahman/mcp-defer)](LICENSE)

A slow server, or one that asks for a credential, costs nothing in the
sessions that never use it. Until the first call, mcp-defer answers
`initialize` and the tool, prompt and resource lists from what the
server said the last time it ran.

## Install

```sh
brew install --cask stefanahman/tap/mcp-defer
go install github.com/stefanahman/mcp-defer@latest
```

## Quick start

Put mcp-defer in front of the server's command in your MCP config:

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

If the wrapper asks for Touch ID, the prompt now appears when the model
first uses a database tool, not when the session starts.

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

## Docs

- [How it works](docs/how-it-works.md): a session step by step, the
  cache, what the model sees, limits

See also: [owl](https://github.com/stefanahman/owl) ·
[spaces](https://github.com/stefanahman/spaces) ·
[mux](https://github.com/stefanahman/mux) ·
[claude-status](https://github.com/stefanahman/claude-status) ·
[mindoro](https://github.com/stefanahman/mindoro) ·
[eden](https://github.com/stefanahman/eden)

## License

MIT
