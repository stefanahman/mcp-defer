# mcp-defer — agent notes

mcp-defer starts an MCP stdio server the first time a tool is called
(README.md). It ships as a Homebrew cask, `stefanahman/tap/mcp-defer`.

A change is done when it is committed in coherent pieces, pushed with
CI green, installed where the MCP config finds it, documented, and — at
a milestone — tagged so the cask other machines install catches up. Do
all of it and say which steps you did.

## Build and test

```sh
make test      # go test -race -count=1 ./...
make lint      # gofmt, go vet
```

Check exit codes, not output.

## Commit

Conventional commits, lower-case subject, a body that says why.
Smallest coherent commits. No Co-Authored-By trailers.

## Push, and install the checkout build

```sh
git push origin main
make install BIN=~/.eden/bin
```

CI is `ci.yml`; `gh run list --workflow ci --branch main --limit 1`.

## Ship at a milestone

Not after every change. Before tagging: tree clean, no rebase in
progress, HEAD pushed, CI green at HEAD.

```sh
git tag -a v0.1.1 -m "mcp-defer 0.1.1: <the batch, one line>"
git push origin v0.1.1
```

The tag runs `release.yml` (goreleaser: binaries, the GitHub release,
the cask in stefanahman/homebrew-tap). Then, in the background:

```sh
gh run list --workflow release --branch v0.1.1 --json status,conclusion   # until completed success
brew update && brew upgrade --cask mcp-defer
/opt/homebrew/bin/mcp-defer --version
```
