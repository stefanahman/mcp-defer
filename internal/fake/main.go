// Command fake is the backend the tests drive: an MCP stdio server whose
// behaviour is set through the environment.
//
//	FAKE_TOOLS        comma-separated tool names (default "echo")
//	FAKE_MARKER       file that gets one line appended per start
//	FAKE_FAIL_ON_START  exit 1 before serving when this is the Nth start (per FAKE_MARKER)
//	FAKE_PAGE         "1": serve tools/list one tool per page
//	FAKE_DIE_ON_TOOL  exit 2 instead of answering this tool
//	FAKE_ADD_ON_CALL  after the first tools/call, add this tool and announce the change
//	FAKE_BANNER       "1": print a line to stdout before speaking the protocol
//	FAKE_RESOURCES    "1": declare resources, with one resource and one template
//	FAKE_ASK_ROOTS    "1": ask the client for roots/list before answering a tools/call
//	FAKE_INIT_DELAY   milliseconds to wait before answering initialize
//	FAKE_COUNT        "1": number the tools/call answers, echo#N:args
//	FAKE_INIT_ERROR   "1": answer initialize with a JSON-RPC error
//	FAKE_BIG_STDERR   "1": write a 2 MB line to stderr before serving
//	FAKE_DETACH       "1": leave a grandchild in its own session holding stderr
//	FAKE_ROOTS_ON_INIT "1": ask the client for roots/list as soon as initialized arrives
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

func main() {
	starts := 0
	if marker := os.Getenv("FAKE_MARKER"); marker != "" {
		f, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake:", err)
			os.Exit(1)
		}
		fmt.Fprintln(f, "start")
		f.Close()
		b, _ := os.ReadFile(marker)
		starts = strings.Count(string(b), "start")
	}
	if n, _ := strconv.Atoi(os.Getenv("FAKE_FAIL_ON_START")); n != 0 && starts == n {
		fmt.Fprintln(os.Stderr, "fake: denied")
		os.Exit(1)
	}
	tools := strings.Split(os.Getenv("FAKE_TOOLS"), ",")
	if os.Getenv("FAKE_TOOLS") == "" {
		tools = []string{"echo"}
	}
	calls := 0
	rootsSeen := -1
	out := bufio.NewWriter(os.Stdout)
	if os.Getenv("FAKE_BIG_STDERR") == "1" {
		fmt.Fprintln(os.Stderr, strings.Repeat("e", 2<<20))
	}
	if os.Getenv("FAKE_DETACH") == "1" {
		// A grandchild outside the process group, inheriting stderr.
		child := exec.Command("sleep", "31.3")
		child.Stderr = os.Stderr
		detach(child)
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "fake:", err)
			os.Exit(1)
		}
	}
	if os.Getenv("FAKE_BANNER") == "1" {
		fmt.Fprintln(out, "fake: starting up")
		out.Flush()
	}
	send := func(m message) {
		b, _ := json.Marshal(m)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var pending []message // read while waiting for a reply, served afterwards
	// readUntil pulls messages until one satisfies want; the others are
	// kept for the main loop. Used to wait for the client's reply.
	readUntil := func(want func(message) bool) (message, bool) {
		for sc.Scan() {
			var m message
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if want(m) {
				return m, true
			}
			pending = append(pending, m)
		}
		return message{}, false
	}
	next := func() (message, bool) {
		if len(pending) > 0 {
			m := pending[0]
			pending = pending[1:]
			return m, true
		}
		for sc.Scan() {
			var m message
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				return m, true
			}
		}
		return message{}, false
	}
	for {
		m, ok := next()
		if !ok {
			break
		}
		if m.ID == nil {
			if m.Method == "notifications/initialized" && os.Getenv("FAKE_ROOTS_ON_INIT") == "1" {
				send(message{JSONRPC: "2.0", ID: json.RawMessage(`"fake-roots"`), Method: "roots/list"})
				reply, ok := readUntil(func(r message) bool { return r.Method == "" && string(r.ID) == `"fake-roots"` })
				if !ok {
					os.Exit(3)
				}
				var roots struct {
					Roots []any `json:"roots"`
				}
				raw, _ := json.Marshal(reply.Result)
				_ = json.Unmarshal(raw, &roots)
				rootsSeen = len(roots.Roots)
			}
			continue // notifications
		}
		switch m.Method {
		case "initialize":
			if ms, _ := strconv.Atoi(os.Getenv("FAKE_INIT_DELAY")); ms > 0 {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
			if os.Getenv("FAKE_INIT_ERROR") == "1" {
				send(message{JSONRPC: "2.0", ID: m.ID, Error: map[string]any{"code": -32602, "message": "Unsupported protocol version"}})
				continue
			}
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			caps := map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}}
			if os.Getenv("FAKE_RESOURCES") == "1" {
				caps["resources"] = map[string]any{}
			}
			send(message{JSONRPC: "2.0", ID: m.ID, Result: map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    caps,
				"serverInfo":      map[string]any{"name": "fake", "version": "0"},
			}})
		case "ping":
			send(message{JSONRPC: "2.0", ID: m.ID, Result: map[string]any{}})
		case "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(m.Params, &p)
			from, to := 0, len(tools)
			result := map[string]any{}
			if os.Getenv("FAKE_PAGE") == "1" {
				from, _ = strconv.Atoi(p.Cursor)
				to = from + 1
				if to < len(tools) {
					result["nextCursor"] = strconv.Itoa(to)
				}
			}
			var list []map[string]any
			for _, name := range tools[from:to] {
				list = append(list, map[string]any{"name": name, "description": "fake " + name, "inputSchema": map[string]any{"type": "object"}})
			}
			result["tools"] = list
			send(message{JSONRPC: "2.0", ID: m.ID, Result: result})
		case "prompts/list":
			send(message{JSONRPC: "2.0", ID: m.ID, Result: map[string]any{"prompts": []any{}}})
		case "resources/list":
			send(message{JSONRPC: "2.0", ID: m.ID, Result: map[string]any{"resources": []any{map[string]any{"uri": "fake://one", "name": "one"}}}})
		case "resources/templates/list":
			send(message{JSONRPC: "2.0", ID: m.ID, Result: map[string]any{"resourceTemplates": []any{map[string]any{"uriTemplate": "fake://{id}", "name": "any"}}}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.Name == os.Getenv("FAKE_DIE_ON_TOOL") {
				fmt.Fprintln(os.Stderr, "fake: dying on", p.Name)
				os.Exit(2)
			}
			calls++
			text := p.Name + ":" + string(p.Arguments)
			if os.Getenv("FAKE_COUNT") == "1" {
				text = fmt.Sprintf("%s#%d:%s", p.Name, calls, p.Arguments)
			}
			if rootsSeen >= 0 {
				text = fmt.Sprintf("%s roots=%d", text, rootsSeen)
			}
			if os.Getenv("FAKE_ASK_ROOTS") == "1" {
				send(message{JSONRPC: "2.0", ID: json.RawMessage(`"fake-roots"`), Method: "roots/list"})
				reply, ok := readUntil(func(r message) bool { return r.Method == "" && string(r.ID) == `"fake-roots"` })
				if !ok {
					os.Exit(3)
				}
				var roots struct {
					Roots []any `json:"roots"`
				}
				raw, _ := json.Marshal(reply.Result)
				_ = json.Unmarshal(raw, &roots)
				text = fmt.Sprintf("%s roots=%d", text, len(roots.Roots))
			}
			send(message{JSONRPC: "2.0", ID: m.ID, Result: map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}},
			}})
			if add := os.Getenv("FAKE_ADD_ON_CALL"); add != "" && calls == 1 {
				tools = append(tools, add)
				send(message{JSONRPC: "2.0", Method: "notifications/tools/list_changed"})
			}
		default:
			send(message{JSONRPC: "2.0", ID: m.ID, Error: map[string]any{"code": -32601, "message": "Method not found"}})
		}
	}
}
