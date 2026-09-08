package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var fakeBin, proxyBin string

// TestMain builds the fake backend once, and the proxy for the test
// that needs it as a process.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mcp-defer-fake")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fake")
	if out, err := exec.Command("go", "build", "-o", fakeBin, "./internal/fake").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build fake: %v\n%s", err, out))
	}
	proxyBin = filepath.Join(dir, "mcp-defer")
	if out, err := exec.Command("go", "build", "-o", proxyBin, ".").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build mcp-defer: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// session is one client talking to one proxy over pipes.
type session struct {
	t      testing.TB
	in     io.WriteCloser
	msgs   chan *message
	done   chan struct{}
	stderr syncBuffer // the proxy's log and the backend's stderr, from several goroutines
	seq    int
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newSession(t testing.TB, cachePath string) *session {
	return newSessionFor(t, cachePath, []string{fakeBin})
}

func newSessionFor(t testing.TB, cachePath string, command []string) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, in: inW, msgs: make(chan *message, 64), done: make(chan struct{})}
	p := newProxy(command, cachePath, &s.stderr)
	go func() {
		p.run(inR, outW)
		outW.Close()
		close(s.done)
	}()
	go func() {
		rd := newReader(outR)
		for {
			m, junk, err := rd.next()
			if junk != nil {
				t.Errorf("proxy wrote a non-message line: %s", junk)
				continue
			}
			if err != nil {
				close(s.msgs)
				return
			}
			s.msgs <- m
		}
	}()
	return s
}

func (s *session) send(m *message) {
	s.t.Helper()
	b, _ := json.Marshal(m)
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("write: %v", err)
	}
}

// request sends a request and returns its id.
func (s *session) request(method string, params any) json.RawMessage {
	s.seq++
	id := json.RawMessage(fmt.Sprint(s.seq))
	s.send(request(id, method, params))
	return id
}

// next returns the next message from the proxy within the deadline.
func (s *session) next() *message {
	s.t.Helper()
	select {
	case m, ok := <-s.msgs:
		if !ok {
			s.t.Fatalf("proxy closed its output; stderr:\n%s", s.stderr.String())
		}
		return m
	case <-time.After(10 * time.Second):
		s.t.Fatalf("no message within 10s; stderr:\n%s", s.stderr.String())
	}
	return nil
}

// reply waits for the response to id, failing on anything else.
func (s *session) reply(id json.RawMessage) *message {
	s.t.Helper()
	m := s.next()
	if !m.isResponse() || m.idKey() != string(id) {
		s.t.Fatalf("expected response to %s, got %s", id, describe(m))
	}
	return m
}

func (s *session) initialize(protocolVersion string) *message {
	s.t.Helper()
	id := s.request("initialize", map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	m := s.reply(id)
	s.send(notification("notifications/initialized"))
	return m
}

func (s *session) close() {
	s.in.Close()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		s.t.Fatal("proxy did not exit after stdin closed")
	}
}

func describe(m *message) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func toolNames(t testing.TB, m *message) []string {
	t.Helper()
	if len(m.Error) > 0 {
		t.Fatalf("tools/list failed: %s", m.Error)
	}
	var r struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatalf("tools/list result: %v", err)
	}
	var names []string
	for _, tool := range r.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func text(t testing.TB, m *message) string {
	t.Helper()
	if len(m.Error) > 0 {
		t.Fatalf("call failed: %s", m.Error)
	}
	var r struct {
		Content []struct{ Text string } `json:"content"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil || len(r.Content) == 0 {
		t.Fatalf("call result: %s", m.Result)
	}
	return r.Content[0].Text
}

func starts(t testing.TB, marker string) int {
	t.Helper()
	b, _ := os.ReadFile(marker)
	return strings.Count(string(b), "start")
}

// setup gives a test its own cache file and start marker.
func setup(t testing.TB) (cachePath, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "starts")
	t.Setenv("FAKE_MARKER", marker)
	for _, k := range fakeVars {
		t.Setenv(k, "")
	}
	return filepath.Join(dir, "cache", "fake.json"), marker
}

var fakeVars = []string{"FAKE_TOOLS", "FAKE_FAIL_ON_START", "FAKE_PAGE", "FAKE_DIE_ON_TOOL", "FAKE_ADD_ON_CALL", "FAKE_BANNER", "FAKE_RESOURCES", "FAKE_ASK_ROOTS", "FAKE_INIT_DELAY", "FAKE_COUNT", "FAKE_INIT_ERROR", "FAKE_BIG_STDERR", "FAKE_DETACH", "FAKE_ROOTS_ON_INIT"}

// running reports the pids matching pattern; a missing pgrep fails the test
// rather than passing it.
func running(t testing.TB, pattern string) []string {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) { // 1: no match
		t.Fatalf("pgrep: %v", err)
	}
	return strings.Fields(string(out))
}

// prime runs one session so the cache exists.
func prime(t testing.TB, cachePath string) {
	t.Helper()
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	s.close()
	if loadCache(cachePath) == nil {
		t.Fatalf("no cache after the first session; stderr:\n%s", s.stderr.String())
	}
}

func TestFirstSessionStartsTheBackendAndWritesTheCache(t *testing.T) {
	cachePath, marker := setup(t)
	t.Setenv("FAKE_TOOLS", "a,b")
	s := newSession(t, cachePath)
	init := s.initialize("2025-06-18")
	if !strings.Contains(string(init.Result), `"listChanged":true`) {
		t.Errorf("initialize result should promise list_changed: %s", init.Result)
	}
	if got := toolNames(t, s.reply(s.request("tools/list", map[string]any{}))); strings.Join(got, ",") != "a,b" {
		t.Errorf("tools = %v", got)
	}
	s.close()
	if n := starts(t, marker); n != 1 {
		t.Errorf("backend started %d times, want 1", n)
	}
	c := loadCache(cachePath)
	if c == nil {
		t.Fatal("no cache written")
	}
	if c.ClientProtocolVersion != "2025-06-18" || !strings.Contains(string(c.Lists["tools/list"]), `"b"`) {
		t.Errorf("cache = %+v", c)
	}
}

func TestCachedSessionStartsOnFirstCallOnly(t *testing.T) {
	cachePath, marker := setup(t)
	t.Setenv("FAKE_TOOLS", "echo")
	prime(t, cachePath)

	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	if got := toolNames(t, s.reply(s.request("tools/list", map[string]any{}))); strings.Join(got, ",") != "echo" {
		t.Errorf("tools = %v", got)
	}
	if m := s.reply(s.request("ping", map[string]any{})); string(m.Result) != "{}" {
		t.Errorf("ping = %s", describe(m))
	}
	if m := s.reply(s.request("prompts/list", map[string]any{})); !strings.Contains(string(m.Result), `"prompts"`) {
		t.Errorf("prompts/list = %s", describe(m))
	}
	if m := s.reply(s.request("resources/list", map[string]any{})); !strings.Contains(string(m.Error), "-32601") {
		t.Errorf("resources/list without the capability = %s", describe(m))
	}
	if n := starts(t, marker); n != 1 {
		t.Fatalf("discovery started the backend: %d starts", n)
	}

	call := func(arg string) string {
		return text(t, s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"v": arg}})))
	}
	if got := call("one"); got != `echo:{"v":"one"}` {
		t.Errorf("call = %q", got)
	}
	if got := call("two"); got != `echo:{"v":"two"}` {
		t.Errorf("call = %q", got)
	}
	s.close()
	if n := starts(t, marker); n != 2 {
		t.Errorf("backend started %d times over two sessions, want 2", n)
	}
}

func TestQueuedCallsKeepTheirOrder(t *testing.T) {
	cachePath, marker := setup(t)
	prime(t, cachePath)
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	first := s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"n": 1}})
	second := s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"n": 2}})
	if got := text(t, s.reply(first)); got != `echo:{"n":1}` {
		t.Errorf("first = %q", got)
	}
	if got := text(t, s.reply(second)); got != `echo:{"n":2}` {
		t.Errorf("second = %q", got)
	}
	s.close()
	if n := starts(t, marker); n != 2 {
		t.Errorf("starts = %d, want 2", n)
	}
}

func TestDriftIsAnnouncedAndCached(t *testing.T) {
	cachePath, _ := setup(t)
	t.Setenv("FAKE_TOOLS", "a")
	prime(t, cachePath)

	t.Setenv("FAKE_TOOLS", "a,b")
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	if got := toolNames(t, s.reply(s.request("tools/list", map[string]any{}))); strings.Join(got, ",") != "a" {
		t.Fatalf("cached tools = %v", got)
	}
	id := s.request("tools/call", map[string]any{"name": "a", "arguments": map[string]any{}})
	var sawChange, sawReply bool
	for !(sawChange && sawReply) {
		m := s.next()
		switch {
		case m.isNotification() && m.Method == "notifications/tools/list_changed":
			sawChange = true
		case m.isResponse() && m.idKey() == string(id):
			sawReply = true
		default:
			t.Fatalf("unexpected %s", describe(m))
		}
	}
	if got := toolNames(t, s.reply(s.request("tools/list", map[string]any{}))); strings.Join(got, ",") != "a,b" {
		t.Errorf("live tools = %v", got)
	}
	s.close()
	if c := loadCache(cachePath); !strings.Contains(string(c.Lists["tools/list"]), `"b"`) {
		t.Errorf("cache not updated: %s", c.Lists["tools/list"])
	}
}

func TestBackendNotificationsAreForwarded(t *testing.T) {
	cachePath, _ := setup(t)
	prime(t, cachePath)
	t.Setenv("FAKE_ADD_ON_CALL", "late")
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	id := s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})
	var sawChange, sawReply bool
	for !(sawChange && sawReply) {
		m := s.next()
		switch {
		case m.isNotification() && m.Method == "notifications/tools/list_changed":
			sawChange = true
		case m.isResponse() && m.idKey() == string(id):
			sawReply = true
		default:
			t.Fatalf("unexpected %s", describe(m))
		}
	}
	s.close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c := loadCache(cachePath); c != nil && strings.Contains(string(c.Lists["tools/list"]), `"late"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache never learned the late tool: %s", loadCache(cachePath).Lists["tools/list"])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStartFailureIsAnErrorTheSessionSurvives(t *testing.T) {
	cachePath, marker := setup(t)
	prime(t, cachePath)
	t.Setenv("FAKE_FAIL_ON_START", "2") // the session's first start; prime was the first ever
	startCooldown = 300 * time.Millisecond
	defer func() { startCooldown = 5 * time.Second }()
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	m := s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}))
	for _, want := range []string{"denied", "before it was ready", "ask before retrying"} {
		if !strings.Contains(string(m.Error), want) {
			t.Errorf("error should say %q, got %s", want, describe(m))
		}
	}
	if m := s.reply(s.request("ping", map[string]any{})); string(m.Result) != "{}" {
		t.Errorf("ping after failure = %s", describe(m))
	}
	// Within the cool-down a retry is refused without a new start.
	m = s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}))
	if !strings.Contains(string(m.Error), "not trying again") {
		t.Errorf("retry inside the cool-down = %s", describe(m))
	}
	if n := starts(t, marker); n != 2 {
		t.Errorf("starts = %d during the cool-down, want 2", n)
	}
	time.Sleep(startCooldown)
	if got := text(t, s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}))); got != "echo:{}" {
		t.Errorf("retry after the cool-down = %q", got)
	}
	s.close()
	if n := starts(t, marker); n != 3 {
		t.Errorf("starts = %d, want 3 (prime, denied, retry)", n)
	}
}

func TestBackendDeathMidSessionFailsTheCallAndRestartsLater(t *testing.T) {
	cachePath, marker := setup(t)
	t.Setenv("FAKE_TOOLS", "echo,die")
	prime(t, cachePath)
	t.Setenv("FAKE_DIE_ON_TOOL", "die")
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	if got := text(t, s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}))); got != "echo:{}" {
		t.Fatalf("call = %q", got)
	}
	m := s.reply(s.request("tools/call", map[string]any{"name": "die", "arguments": map[string]any{}}))
	if !strings.Contains(string(m.Error), "exit status 2") {
		t.Fatalf("expected an exit error, got %s", describe(m))
	}
	if got := text(t, s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}))); got != "echo:{}" {
		t.Errorf("call after restart = %q", got)
	}
	s.close()
	if n := starts(t, marker); n != 3 {
		t.Errorf("starts = %d, want 3 (prime, session, restart)", n)
	}
}

func TestPaginatedListsAreCachedWhole(t *testing.T) {
	cachePath, _ := setup(t)
	t.Setenv("FAKE_TOOLS", "a,b,c")
	t.Setenv("FAKE_PAGE", "1")
	prime(t, cachePath)
	c := loadCache(cachePath)
	if strings.Contains(string(c.Lists["tools/list"]), "nextCursor") {
		t.Errorf("cache keeps a cursor: %s", c.Lists["tools/list"])
	}
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	if got := toolNames(t, s.reply(s.request("tools/list", map[string]any{}))); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("tools = %v", got)
	}
	s.close()
}

func TestOtherProtocolVersionStartsEagerly(t *testing.T) {
	cachePath, marker := setup(t)
	prime(t, cachePath)
	s := newSession(t, cachePath)
	init := s.initialize("2025-03-26")
	if !strings.Contains(string(init.Result), `"protocolVersion":"2025-03-26"`) {
		t.Errorf("initialize should come from the backend: %s", init.Result)
	}
	s.close()
	if n := starts(t, marker); n != 2 {
		t.Errorf("starts = %d, want 2", n)
	}
	if c := loadCache(cachePath); c.ClientProtocolVersion != "2025-03-26" {
		t.Errorf("cache version = %s", c.ClientProtocolVersion)
	}
}

func TestStdinCloseStopsTheBackend(t *testing.T) {
	cachePath, _ := setup(t)
	prime(t, cachePath)
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	text(t, s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})))
	s.close()
	if pids := running(t, fakeBin); len(pids) > 0 {
		ps, _ := exec.Command("ps", append([]string{"-o", "pid,ppid,stat,etime,command", "-p"}, strings.Join(pids, ","))...).CombinedOutput()
		t.Errorf("fake backend still running:\n%s", ps)
	}
}

func TestResourceListsAreCachedToo(t *testing.T) {
	cachePath, marker := setup(t)
	t.Setenv("FAKE_RESOURCES", "1")
	prime(t, cachePath)
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	if m := s.reply(s.request("resources/list", map[string]any{})); !strings.Contains(string(m.Result), `"fake://one"`) {
		t.Errorf("resources/list = %s", describe(m))
	}
	if m := s.reply(s.request("resources/templates/list", map[string]any{})); !strings.Contains(string(m.Result), `"fake://{id}"`) {
		t.Errorf("resources/templates/list = %s", describe(m))
	}
	s.close()
	if n := starts(t, marker); n != 1 {
		t.Errorf("resource discovery started the backend: %d starts", n)
	}
}

func TestBannerLinesFromTheBackendAreIgnored(t *testing.T) {
	cachePath, _ := setup(t)
	t.Setenv("FAKE_BANNER", "1")
	prime(t, cachePath)
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	if got := text(t, s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}))); got != "echo:{}" {
		t.Errorf("call = %q", got)
	}
	s.close()
	if !strings.Contains(s.stderr.String(), "starting up") {
		t.Errorf("the banner should be reported on stderr, got:\n%s", s.stderr.String())
	}
}

func TestBackendRequestsReachTheClientAndBack(t *testing.T) {
	cachePath, _ := setup(t)
	prime(t, cachePath)
	t.Setenv("FAKE_ASK_ROOTS", "1")
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	id := s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})
	ask := s.next()
	if !ask.isRequest() || ask.Method != "roots/list" {
		t.Fatalf("expected the backend's roots/list request, got %s", describe(ask))
	}
	s.send(response(ask.ID, map[string]any{"roots": []any{map[string]any{"uri": "file:///a"}, map[string]any{"uri": "file:///b"}}}))
	if got := text(t, s.reply(id)); got != "echo:{} roots=2" {
		t.Errorf("call = %q", got)
	}
	s.close()
}

func TestCancelledQueuedRequestNeverReachesTheBackend(t *testing.T) {
	cachePath, _ := setup(t)
	t.Setenv("FAKE_COUNT", "1")
	prime(t, cachePath)
	t.Setenv("FAKE_INIT_DELAY", "300")
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	first := s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})
	s.send(&message{JSONRPC: "2.0", Method: "notifications/cancelled", Params: mustJSON(map[string]any{"requestId": json.RawMessage(first)})})
	second := s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})
	if got := text(t, s.reply(second)); got != "echo#1:{}" {
		t.Errorf("the cancelled call still reached the backend: second call = %q", got)
	}
	s.close()
}

func TestStdinCloseWhileTheWrapperWaitsStopsTheWholeGroup(t *testing.T) {
	cachePath, _ := setup(t)
	prime(t, cachePath)
	// A wrapper stuck on a child, the way a script waits on a credential
	// prompt: sh defers the signal until sleep returns, the group does not.
	s := newSessionFor(t, cachePath, []string{"sh", "-c", "sleep 30.7; exit 1"})
	s.initialize("2025-06-18")
	s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})
	started := time.Now()
	s.close()
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("close took %v", took)
	}
	waitGone(t, "sleep 30.7")
}

// waitGone waits for no process to match pattern.
func waitGone(t testing.TB, pattern string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		pids := running(t, pattern)
		if len(pids) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running: %s pids %v", pattern, pids)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestCLI(t *testing.T) {
	var out, errOut strings.Builder
	if code := run([]string{}, &out, &errOut); code != 64 || !strings.Contains(errOut.String(), "usage:") {
		t.Errorf("no command: exit %d, stderr %q", code, errOut.String())
	}
	out.Reset()
	if code := run([]string{"-version"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) == "" {
		t.Errorf("-version: exit %d, stdout %q", code, out.String())
	}
	if code := run([]string{"-bogus"}, &out, &errOut); code != 64 {
		t.Errorf("unknown flag: exit %d, want 64", code)
	}
	errOut.Reset()
	if code := run([]string{"-name", "../x", "true"}, &out, &errOut); code != 64 || !strings.Contains(errOut.String(), "not a path") {
		t.Errorf("a path as -name: exit %d, stderr %q", code, errOut.String())
	}
}

// BenchmarkToolCall measures a round trip through the proxy to a
// running backend: parse, route, forward, and the same on the way back.
func BenchmarkToolCall(b *testing.B) {
	cachePath, _ := setup(b)
	s := newSession(b, cachePath)
	s.initialize("2025-06-18")
	args := map[string]any{"name": "echo", "arguments": map[string]any{"payload": strings.Repeat("x", 4096)}}
	text(b, s.reply(s.request("tools/call", args)))
	b.ResetTimer()
	for range b.N {
		s.reply(s.request("tools/call", args))
	}
	b.StopTimer()
	s.close()
}

func TestSignalToTheProxyStopsTheGroup(t *testing.T) {
	cachePath, _ := setup(t)
	prime(t, cachePath)
	cmd := exec.Command(proxyBin, "-name", "fake", "-cache", filepath.Dir(cachePath), "--", "sh", "-c", "sleep 30.9; exit 1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = io.Discard
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []*message{
		request(json.RawMessage("1"), "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}}),
		notification("notifications/initialized"),
		request(json.RawMessage("2"), "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}),
	} {
		b, _ := json.Marshal(m)
		if _, err := stdin.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(running(t, "sleep 30.9")) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the wrapper never started; stderr:\n%s", stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil { // what a terminal or client sends first
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("proxy did not exit on SIGINT; stderr:\n%s", stderr.String())
	}
	waitGone(t, "sleep 30.9")
}

func TestInitializeErrorIsReportedAsItself(t *testing.T) {
	cachePath, _ := setup(t)
	prime(t, cachePath)
	t.Setenv("FAKE_INIT_ERROR", "1")
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	m := s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}))
	if !strings.Contains(string(m.Error), "Unsupported protocol version") {
		t.Errorf("the server's own error should be reported, got %s", describe(m))
	}
	if strings.Contains(string(m.Error), "declined") {
		t.Errorf("a handshake error is not a declined prompt: %s", describe(m))
	}
	s.close()
}

func TestHugeStderrLineDoesNotStallTheBackend(t *testing.T) {
	cachePath, _ := setup(t)
	prime(t, cachePath)
	t.Setenv("FAKE_BIG_STDERR", "1")
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	if got := text(t, s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}))); got != "echo:{}" {
		t.Errorf("call = %q", got)
	}
	s.close()
	if n := len(s.stderr.String()); n < 2<<20 {
		t.Errorf("stderr should have passed the whole line through, got %d bytes", n)
	}
}

func TestDetachedGrandchildDoesNotHangShutdown(t *testing.T) {
	cachePath, _ := setup(t)
	prime(t, cachePath)
	t.Setenv("FAKE_DETACH", "1")
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", "sleep 31.3").Run() })
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	text(t, s.reply(s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})))
	started := time.Now()
	s.close()
	if took := time.Since(started); took > 8*time.Second {
		t.Errorf("close took %v with a grandchild holding stderr", took)
	}
}

func TestBackendRequestOnInitializedIsAnswered(t *testing.T) {
	cachePath, _ := setup(t)
	prime(t, cachePath)
	t.Setenv("FAKE_ROOTS_ON_INIT", "1")
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	id := s.request("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})
	ask := s.next()
	if !ask.isRequest() || ask.Method != "roots/list" {
		t.Fatalf("expected roots/list right after initialized, got %s", describe(ask))
	}
	s.send(response(ask.ID, map[string]any{"roots": []any{map[string]any{"uri": "file:///a"}}}))
	if got := text(t, s.reply(id)); got != "echo:{} roots=1" {
		t.Errorf("call = %q", got)
	}
	s.close()
}

func TestCorruptCacheStartsEagerly(t *testing.T) {
	cachePath, marker := setup(t)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSession(t, cachePath)
	s.initialize("2025-06-18")
	s.close()
	if n := starts(t, marker); n != 1 {
		t.Errorf("starts = %d, want 1", n)
	}
	if loadCache(cachePath) == nil {
		t.Error("the cache should have been rewritten")
	}
}

func TestWithListChangedTolerantOfShape(t *testing.T) {
	for _, in := range []string{`{}`, `{"capabilities":null}`, `{"capabilities":{"tools":{}}}`, `not json`} {
		out := withListChanged(json.RawMessage(in))
		if in == `{"capabilities":{"tools":{}}}` && !strings.Contains(string(out), `"listChanged":true`) {
			t.Errorf("%s -> %s", in, out)
		}
		if in != `{"capabilities":{"tools":{}}}` && string(out) != in && in != `{"capabilities":null}` {
			t.Errorf("%s should pass through unchanged, got %s", in, out)
		}
	}
}

func TestSameJSONIgnoresKeyOrder(t *testing.T) {
	if !sameJSON(json.RawMessage(`{"a":1,"b":[{"c":2,"d":3}]}`), json.RawMessage(`{"b":[{"d":3,"c":2}],"a":1}`)) {
		t.Error("key order should not matter")
	}
	if sameJSON(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`)) {
		t.Error("values should")
	}
}
