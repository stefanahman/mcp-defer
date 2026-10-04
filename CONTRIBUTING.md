# Contributing

```sh
make test   # a fake MCP server, built once per run, plays the backend
make lint   # gofmt and vet; CI adds staticcheck, govulncheck and a Windows vet
```

To install a build from a checkout: `make install BIN=~/.local/bin`.
