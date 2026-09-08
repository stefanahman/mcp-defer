// mcp-defer runs an MCP stdio server the first time a client calls one
// of its tools. Until then it answers initialize and the list requests
// from what it cached the last time the server ran, so a server whose
// start is expensive, or asks for a credential, costs nothing in the
// sessions that never use it.
//
//	mcp-defer [-name NAME] [-cache DIR] [--] COMMAND [ARG...]
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

var version = "dev"

const usageText = `usage: mcp-defer [-name NAME] [-cache DIR] [--] COMMAND [ARG...]

Run COMMAND as an MCP stdio server, but only once a tool is called.
Before that, initialize and the tools, prompts and resources lists are
answered from the cache recorded the last time COMMAND ran.

  -name NAME   cache entry to use (default: the base name of COMMAND)
  -cache DIR   cache directory (default: $XDG_CACHE_HOME/mcp-defer)
  -version     print the version and exit
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the command line: it parses args and serves stdin/stdout.
// stdout and stderr are parameters for the version and usage text.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-defer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }
	name := fs.String("name", "", "")
	dir := fs.String("cache", "", "")
	showVersion := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *showVersion {
		fmt.Fprintln(stdout, versionString())
		return 0
	}
	command := fs.Args()
	if len(command) == 0 {
		fs.Usage()
		return 64
	}
	if *name == "" {
		*name = filepath.Base(command[0])
	}
	if *name == "." || *name == ".." || strings.ContainsAny(*name, `/\`) {
		fmt.Fprintf(stderr, "mcp-defer: -name %q: a name, not a path\n", *name)
		return 64
	}
	if *dir == "" {
		d, err := cacheDir()
		if err != nil {
			fmt.Fprintf(stderr, "mcp-defer: %v\n", err)
			return 1
		}
		*dir = d
	}
	p := newProxy(command, filepath.Join(*dir, *name+".json"), os.Stderr)
	go func() {
		<-shutdownSignals()
		p.stop()
		os.Exit(0)
	}()
	p.run(os.Stdin, os.Stdout)
	return 0
}

func versionString() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
