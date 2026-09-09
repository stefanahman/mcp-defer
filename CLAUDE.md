# mcp-defer — agent notes

mcp-defer runs an MCP stdio server the first time a tool is called
(README.md). It ships as a Homebrew cask, `stefanahman/tap/mcp-defer`.

A change is done when it is committed in coherent pieces, pushed with
CI green, installed, documented, and — at a milestone — tagged so the
cask other machines install catches up. Do all of it and say which
steps you did.

## Build and test

```sh
make test      # go test -race -count=1 ./...
make lint      # gofmt, go vet
make build     # the binary at ./mcp-defer, gitignored (as is dist/, goreleaser's)
```

Check exit codes, not output.

`proxy_test.go` runs the real proxy against `internal/fake`, a small
MCP stdio server whose behaviour the tests steer: which methods it
answers, how slowly it starts, whether it dies mid-call. A new proxy
behaviour usually needs a knob there rather than a new mock; a change
to the fake changes what every test in the file means, so read its
callers first. `internal/fake/detach_unix.go` and `detach_other.go`
are a build-tagged pair, like the signal files below.

**`make test && make lint` green does not predict CI.** Two jobs run
there and both do something this machine's `make` does not:

| job | what it adds |
|---|---|
| `go` (matrix: ubuntu **and** macos) | the same lint and test on both platforms, then `goreleaser check` on ubuntu — a broken `.goreleaser.yaml` fails CI here, not at tag time |
| `analysis` (ubuntu, Go stable) | `staticcheck@2026.2.1`, `govulncheck@v1.7.0`, and `GOOS=windows go vet ./...` |

That last step is the one that bites. `signal_unix.go` and
`signal_other.go` are a build-tagged pair, and everything this machine
compiles is the unix half: touch signal handling and the `!unix`
fallback can stop compiling without a single local test failing. Run
`GOOS=windows go vet ./...` yourself whenever those files move.

`gh run list --workflow ci --branch main --limit 1` after a push.

## Commit

Conventional commits, lower-case subject, a body that says why.
Smallest coherent commits. No Co-Authored-By trailers.

## Push, and install the checkout build

```sh
git push origin main
make install BIN=~/.eden/bin     # stamped with git describe
```

`--version` prints `mcp-defer <version>`, and answers to `-version`,
`--version` and a bare `version`, as owl and spaces do. An unstamped
build says `dev`.

Nothing on this machine currently wires mcp-defer into an MCP config:
`common/Brewfile` installs the cask, and no `.mcp.json` or
`~/.claude.json` entry runs it. So "installed" here means the binary
is current, nothing more — if a server does get wired to it later,
name that file in this section.

## Ship at a milestone

Not after every change. Before tagging: tree clean, no rebase in
progress, HEAD pushed, CI green at HEAD.

```sh
git tag -a v0.1.1 -m "mcp-defer 0.1.1: <the batch, one line>"
git push origin v0.1.1
```

The tag runs `release.yml`: goreleaser builds the binaries, publishes
the GitHub release and rewrites the cask in stefanahman/homebrew-tap.
That last step needs the `HOMEBREW_TAP_GITHUB_TOKEN` secret, a
fine-grained PAT with contents:write on that tap — when a release
fails at the cask step, an expired token is the first thing to check.
Then, in the background:

```sh
gh run list --workflow release --branch v0.1.1 --json status,conclusion   # until completed success
brew update && brew upgrade --cask mcp-defer
/opt/homebrew/bin/mcp-defer --version                      # the cask is what other machines get
make install BIN=~/.eden/bin                               # restamp: the checkout build predates the tag
```

Patch for fixes, minor for a new flag or a change in what the cache
holds.
