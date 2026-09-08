package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// listSpec is one discovery request the proxy answers from the cache
// while the backend is down: the key its result keeps the items under,
// and the capability a server must declare for it.
type listSpec struct {
	method, key, capability string
}

var lists = []listSpec{
	{"tools/list", "tools", "tools"},
	{"prompts/list", "prompts", "prompts"},
	{"resources/list", "resources", "resources"},
	{"resources/templates/list", "resourceTemplates", "resources"},
}

func listByMethod(method string) (listSpec, bool) {
	for _, l := range lists {
		if l.method == method {
			return l, true
		}
	}
	return listSpec{}, false
}

// listsChangedBy are the lists a list_changed notification refreshes.
func listsChangedBy(notificationMethod string) []listSpec {
	var out []listSpec
	for _, l := range lists {
		if notificationMethod == "notifications/"+l.capability+"/list_changed" {
			out = append(out, l)
		}
	}
	return out
}

const (
	errBackend = -32000 // JSON-RPC server error: the backend is down or failed to start
	errMethod  = -32601 // method not found

	maxPages = 1000 // a list longer than this is a server that never ends its cursor
)

// startCooldown is how long a failed start keeps the proxy from trying
// again: a wrapper that prompts for a credential must not be driven
// into a prompt storm by a client that retries.
var startCooldown = 5 * time.Second

// Timeouts around a backend on its way out.
const (
	finishWait   = 2 * time.Second // for a handshake or refresh to complete before SIGINT
	exitWait     = 2 * time.Second // after SIGINT, before SIGKILL; after SIGKILL, before the pipes are closed
	stderrLinger = time.Second     // after stdout ends, for stderr to end too
	tailKeep     = 3               // stderr lines kept for the error text
	tailWidth    = 4096            // bytes of each kept line
)

type backendState int

const (
	starting    backendState = iota // spawned, handshake under way
	initialized                     // handshake done, queued requests draining
	ready                           // everything forwards straight through
	dead
)

// backend is one run of the wrapped command.
type backend struct {
	cmd        *exec.Cmd
	in         *writer
	pipes      []io.Closer // stdout and stderr read ends, closed by force when nothing else ends them
	state      backendState
	ready      chan struct{} // closed once the queue has drained
	done       chan struct{} // closed once the process has been waited for
	failure    error         // a handshake error, recorded before the process is killed
	err        error         // why it is down, valid after done
	stderr     *tail
	stderrDone chan struct{} // closed once stderr has been read to its end
}

// proxy is the MCP server the client talks to. It answers discovery
// from the cache and starts the backend for everything else.
type proxy struct {
	command   []string
	cachePath string
	log       *log.Logger
	stderr    io.Writer // the backend's stderr goes here unchanged
	client    *writer

	jobs      sync.WaitGroup // handshakes, refreshes, deferred replies: finish these before stopping
	readers   sync.WaitGroup // backend readers: run returns after them
	refreshMu sync.Mutex     // one refresh at a time, so an older list can't overwrite a newer one
	stopOnce  sync.Once

	mu         sync.Mutex
	closing    bool            // the client has gone away
	failedAt   time.Time       // when a start last failed; no retry within startCooldown
	cache      *cache          // replaced, never mutated in place
	initParams json.RawMessage // the client's initialize params, replayed to the backend
	backend    *backend
	queue      []*message                 // client requests waiting for the backend to be ready
	inflight   map[string]json.RawMessage // client request ids forwarded to the backend
	calls      map[string]chan *message   // the proxy's own requests to the backend
	seq        int
}

func newProxy(command []string, cachePath string, stderr io.Writer) *proxy {
	return &proxy{
		command:   command,
		cachePath: cachePath,
		log:       log.New(stderr, "mcp-defer: ", 0),
		stderr:    stderr,
		cache:     loadCache(cachePath),
		inflight:  map[string]json.RawMessage{},
		calls:     map[string]chan *message{},
	}
}

// run serves the client on in/out until in closes, then stops the
// backend if it is running.
func (p *proxy) run(in io.Reader, out io.Writer) {
	p.client = &writer{w: out}
	rd := newReader(in)
	for {
		m, junk, err := rd.next()
		if junk != nil {
			p.log.Printf("ignoring non-message line from client: %.200s", junk)
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.log.Printf("client: %v", err)
			}
			break
		}
		p.handleClient(m)
	}
	p.stop()
}

func (p *proxy) handleClient(m *message) {
	switch {
	case m.isRequest() && m.Method == "initialize":
		p.handleInitialize(m)
	case m.isNotification() && m.Method == "notifications/initialized":
		// The proxy runs its own handshake with the backend.
	case m.isNotification() && m.Method == "notifications/cancelled":
		if !p.dropQueued(m) {
			p.forwardIfUp(m)
		}
	case m.isResponse():
		// The client's answer to a backend request: whoever asked is
		// alive and waiting, whatever state the handshake is in.
		p.forwardIfAlive(m)
	case m.isNotification():
		// A roots change or a progress note: for an initialized backend.
		p.forwardIfUp(m)
	case m.Method == "ping":
		if !p.forwardIfUp(m) {
			p.send(response(m.ID, map[string]any{}))
		}
	default:
		if _, isList := listByMethod(m.Method); isList {
			if !p.forwardIfUp(m) {
				p.serveList(m)
			}
			return
		}
		p.forwardWhenReady(m)
	}
}

func (p *proxy) send(m *message) {
	if err := p.client.send(m); err != nil {
		p.log.Printf("client write: %v", err)
	}
}

// handleInitialize answers from the cache when it was recorded against
// the same client protocol version; otherwise, or when the backend is
// already up, from a live handshake.
func (p *proxy) handleInitialize(m *message) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(m.Params, &params)

	p.mu.Lock()
	p.initParams = m.Params
	c, b := p.cache, p.backend
	if b == nil && c != nil && c.ClientProtocolVersion == params.ProtocolVersion {
		p.mu.Unlock()
		p.log.Printf("answering from the cache recorded %s ago (%s)", time.Since(c.SavedAt).Round(time.Second), p.cachePath)
		p.send(&message{JSONRPC: "2.0", ID: m.ID, Result: c.Initialize})
		return
	}
	if b == nil {
		b = p.startLocked("initialize")
	}
	if b == nil {
		p.mu.Unlock()
		p.send(errorResponse(m.ID, errBackend, "mcp-defer: "+p.startError().Error()))
		return
	}
	p.jobs.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.jobs.Done()
		select {
		case <-b.ready:
			p.mu.Lock()
			c := p.cache
			p.mu.Unlock()
			p.send(&message{JSONRPC: "2.0", ID: m.ID, Result: c.Initialize})
		case <-b.done:
			p.send(errorResponse(m.ID, errBackend, "mcp-defer: "+b.err.Error()))
		}
	}()
}

// serveList answers a discovery request from the cache.
func (p *proxy) serveList(m *message) {
	l, _ := listByMethod(m.Method)
	p.mu.Lock()
	c := p.cache
	p.mu.Unlock()
	if c == nil {
		p.forwardWhenReady(m)
		return
	}
	if !c.declares(l.capability) {
		p.send(errorResponse(m.ID, errMethod, "Method not found"))
		return
	}
	if result, ok := c.Lists[m.Method]; ok {
		p.send(&message{JSONRPC: "2.0", ID: m.ID, Result: result})
		return
	}
	p.forwardWhenReady(m)
}

// dropQueued honours a cancellation for a request the backend has not
// seen yet, and reports whether there was one.
func (p *proxy) dropQueued(cancel *message) bool {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(cancel.Params, &params) != nil || len(params.RequestID) == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, m := range p.queue {
		if m.idKey() == string(params.RequestID) {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			return true
		}
	}
	return false
}

// forwardIfAlive passes a message to any backend that has not died.
func (p *proxy) forwardIfAlive(m *message) {
	p.mu.Lock()
	b := p.backend
	alive := b != nil && b.state != dead
	p.mu.Unlock()
	if alive {
		p.forward(b, m)
	}
}

// forwardIfUp passes a message to an initialized backend and reports
// whether it did.
func (p *proxy) forwardIfUp(m *message) bool {
	p.mu.Lock()
	b := p.backend
	up := b != nil && (b.state == initialized || b.state == ready)
	p.mu.Unlock()
	if up {
		p.forward(b, m)
	}
	return up
}

// forwardWhenReady passes a request to the backend, starting it first
// if needed; requests that arrive before it is ready wait in order.
func (p *proxy) forwardWhenReady(m *message) {
	p.mu.Lock()
	b := p.backend
	if b == nil {
		b = p.startLocked(m.Method)
	}
	if b != nil && b.state != ready {
		p.queue = append(p.queue, m)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	if b == nil {
		p.send(errorResponse(m.ID, errBackend, "mcp-defer: "+p.startError().Error()))
		return
	}
	p.forward(b, m)
}

// forward writes a client message to the backend. No lock is held
// across the write: a full pipe must never stall the reader that
// drains the other direction.
func (p *proxy) forward(b *backend, m *message) {
	if m.isRequest() {
		p.mu.Lock()
		p.inflight[m.idKey()] = m.ID
		p.mu.Unlock()
	}
	err := b.in.send(m)
	if err == nil || !m.isRequest() {
		return
	}
	// The write failed. Answer only if the exit path has not already
	// answered this id on the backend's way down.
	p.mu.Lock()
	_, owed := p.inflight[m.idKey()]
	delete(p.inflight, m.idKey())
	p.mu.Unlock()
	if owed {
		p.send(errorResponse(m.ID, errBackend, "mcp-defer: backend write: "+err.Error()))
	}
}

// startLocked spawns the backend and begins its handshake; p.mu held.
// It returns nil when the process could not be started, when the last
// start failed too recently to try again, or when the proxy is closing.
func (p *proxy) startLocked(why string) *backend {
	if p.closing || (!p.failedAt.IsZero() && time.Since(p.failedAt) < startCooldown) {
		return nil
	}
	p.log.Printf("starting %s for %s", p.command[0], why)
	cmd := exec.Command(p.command[0], p.command[1:]...)
	cmd.WaitDelay = stderrLinger
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		p.log.Printf("start: %v", err)
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		p.log.Printf("start: %v", err)
		return nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		p.log.Printf("start: %v", err)
		return nil
	}
	if err := cmd.Start(); err != nil {
		p.log.Printf("start: %v", err)
		return nil
	}
	b := &backend{
		cmd:        cmd,
		in:         &writer{w: stdin},
		pipes:      []io.Closer{stdout, stderr},
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
		stderr:     &tail{keep: tailKeep, width: tailWidth},
		stderrDone: make(chan struct{}),
	}
	p.backend = b
	p.readers.Add(1)
	go func() {
		defer p.readers.Done()
		b.stderr.copy(stderr, p.stderr)
		close(b.stderrDone)
	}()
	p.readers.Add(1)
	go func() {
		defer p.readers.Done()
		p.readBackend(b, stdout)
	}()
	p.jobs.Add(1)
	go func() {
		defer p.jobs.Done()
		p.handshake(b)
	}()
	return b
}

// startError explains a refused or failed start to the client.
func (p *proxy) startError() error {
	p.mu.Lock()
	closing, since := p.closing, time.Since(p.failedAt)
	p.mu.Unlock()
	switch {
	case closing:
		return errors.New("shutting down")
	case since < startCooldown:
		return fmt.Errorf("%s failed to start %s ago; not trying again for %s. The user may have declined a prompt; ask before retrying",
			p.command[0], since.Round(time.Second), (startCooldown - since).Round(time.Second))
	}
	return fmt.Errorf("could not start %s (see stderr)", p.command[0])
}

// handshake initialises the backend, releases the queued requests, then
// brings the cache up to date.
func (p *proxy) handshake(b *backend) {
	p.mu.Lock()
	params := p.initParams
	previous := p.cache
	p.mu.Unlock()
	if params == nil {
		params = mustJSON(map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "mcp-defer", "version": version},
		})
	}
	result, err := p.call(b, "initialize", params)
	if err == nil {
		// From the backend's point of view the session starts with this
		// notification, and it may answer with requests of its own at
		// once; the client's replies must already get through.
		p.mu.Lock()
		if b.state == starting {
			b.state = initialized
		}
		p.mu.Unlock()
		err = b.in.send(notification("notifications/initialized"))
	}
	if err != nil {
		p.mu.Lock()
		b.failure = fmt.Errorf("initialize: %w", err)
		p.mu.Unlock()
		_ = kill(b.cmd)
		return
	}
	var req struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &req)

	c := &cache{ClientProtocolVersion: req.ProtocolVersion, Initialize: withListChanged(result), Lists: map[string]json.RawMessage{}}
	if previous != nil {
		c.Lists = previous.clone().Lists
		if previous.ClientProtocolVersion == req.ProtocolVersion && !sameJSON(previous.Initialize, c.Initialize) {
			p.log.Printf("the server's initialize result changed since it was cached; the client keeps the cached one for this session")
		}
	}
	p.mu.Lock()
	p.cache = c
	p.mu.Unlock()

	// Release what waited, in order; requests that arrive meanwhile
	// join the queue and go out in the next round. Only an empty
	// queue, seen under the lock, flips the state to ready.
	for {
		p.mu.Lock()
		if b.state == dead {
			p.mu.Unlock()
			return
		}
		if len(p.queue) == 0 {
			b.state = ready
			close(b.ready)
			p.mu.Unlock()
			break
		}
		batch := p.queue
		p.queue = nil
		p.mu.Unlock()
		for _, m := range batch {
			p.forward(b, m)
		}
	}

	var declared []listSpec
	for _, l := range lists {
		if c.declares(l.capability) {
			declared = append(declared, l)
		}
	}
	p.refresh(b, declared, previous != nil)
}

// refresh fetches lists from the backend and records them; when a list
// differs from what the client may already have been served, the client
// is told through the matching list_changed notification.
func (p *proxy) refresh(b *backend, specs []listSpec, notify bool) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	changed := map[string]bool{}
	for _, l := range specs {
		result, err := p.fetchList(b, l)
		if err != nil {
			if !p.isClosing() {
				p.log.Printf("%s: %v", l.method, err)
			}
			continue
		}
		p.mu.Lock()
		c := p.cache.clone()
		if old, ok := c.Lists[l.method]; ok && !sameJSON(old, result) {
			changed[l.capability] = true
		}
		c.Lists[l.method] = result
		p.cache = c
		p.mu.Unlock()
	}
	p.mu.Lock()
	c := p.cache
	p.mu.Unlock()
	if err := saveCache(p.cachePath, c); err != nil {
		p.log.Printf("cache: %v", err)
	} else {
		p.log.Printf("cache written: %s", p.cachePath)
	}
	if !notify {
		return
	}
	for capability := range changed {
		p.send(notification("notifications/" + capability + "/list_changed"))
	}
}

// fetchList asks the backend for a whole list, following pagination,
// and returns it as one page.
func (p *proxy) fetchList(b *backend, l listSpec) (json.RawMessage, error) {
	var items []json.RawMessage
	cursor := ""
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("more than %d pages", maxPages)
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := p.call(b, l.method, params)
		if err != nil {
			return nil, err
		}
		var pageResult map[string]json.RawMessage
		if err := json.Unmarshal(result, &pageResult); err != nil {
			return nil, err
		}
		var pageItems []json.RawMessage
		if raw, ok := pageResult[l.key]; ok {
			if err := json.Unmarshal(raw, &pageItems); err != nil {
				return nil, err
			}
		}
		items = append(items, pageItems...)
		cursor = ""
		if raw, ok := pageResult["nextCursor"]; ok {
			_ = json.Unmarshal(raw, &cursor)
		}
		if cursor == "" {
			break
		}
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	return json.Marshal(map[string]any{l.key: items})
}

// call sends the proxy's own request to the backend and waits for the
// answer, or for the backend to die.
func (p *proxy) call(b *backend, method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	p.seq++
	id := json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("mcp-defer/%d", p.seq)))
	ch := make(chan *message, 1)
	p.calls[string(id)] = ch
	p.mu.Unlock()
	if err := b.in.send(request(id, method, params)); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m == nil {
			return nil, b.err
		}
		if len(m.Error) > 0 {
			return nil, fmt.Errorf("%s: %s", method, m.Error)
		}
		return m.Result, nil
	case <-b.done:
		return nil, b.err
	}
}

// readBackend routes the backend's output: answers to the proxy's own
// requests stay here, everything else goes to the client. When the
// stream ends it settles the process and fails whatever was waiting.
func (p *proxy) readBackend(b *backend, out io.Reader) {
	rd := newReader(out)
	for {
		m, junk, err := rd.next()
		if junk != nil {
			p.log.Printf("ignoring non-message line from backend: %.200s", junk)
			continue
		}
		if err != nil {
			break
		}
		switch {
		case m.isResponse():
			p.mu.Lock()
			ch, own := p.calls[m.idKey()]
			if own {
				delete(p.calls, m.idKey())
			} else {
				delete(p.inflight, m.idKey())
			}
			p.mu.Unlock()
			if own {
				ch <- m
			} else {
				p.send(m)
			}
		default:
			p.send(m)
			if specs := listsChangedBy(m.Method); specs != nil {
				p.mu.Lock()
				if !p.closing {
					p.jobs.Add(1)
					go func() {
						defer p.jobs.Done()
						p.refresh(b, specs, false)
					}()
				}
				p.mu.Unlock()
			}
		}
	}
	// stdout has ended. Give stderr a moment to end too, then reap;
	// WaitDelay closes the pipes a grandchild may still be holding.
	select {
	case <-b.stderrDone:
	case <-time.After(stderrLinger):
	}
	err := b.cmd.Wait()

	p.mu.Lock()
	if p.backend == b {
		p.backend = nil
	}
	failedToStart := b.state == starting
	if failedToStart {
		p.failedAt = time.Now()
	}
	b.state = dead
	b.err = exitError(err, b.failure, b.stderr.lines(), failedToStart)
	inflight, queue, calls := p.inflight, p.queue, p.calls
	p.inflight, p.queue, p.calls = map[string]json.RawMessage{}, nil, map[string]chan *message{}
	p.mu.Unlock()
	close(b.done)

	for _, ch := range calls {
		close(ch)
	}
	for _, id := range inflight {
		p.send(errorResponse(id, errBackend, "mcp-defer: "+b.err.Error()))
	}
	for _, m := range queue {
		p.send(errorResponse(m.ID, errBackend, "mcp-defer: "+b.err.Error()))
	}
	if !p.isClosing() {
		p.log.Printf("%s", b.err)
	}
}

func (p *proxy) isClosing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closing
}

// exitError describes why the backend is gone, with its last words. A
// handshake that failed says so; a backend that died before it was
// ready most likely had its credential prompt declined, and the text
// says that, to the model that reads it.
func exitError(err, failure error, stderr []string, failedToStart bool) error {
	var what string
	switch {
	case failure != nil:
		what = "backend " + failure.Error()
	case err != nil:
		what = "backend " + err.Error()
	default:
		what = "backend exited"
	}
	if failedToStart && failure == nil {
		what += " before it was ready"
	}
	if len(stderr) > 0 {
		what += ": " + strings.Join(stderr, " | ")
	}
	if failedToStart && failure == nil {
		what += ". The user may have declined a prompt; ask before retrying"
	}
	return errors.New(what)
}

// stop ends the backend when the client goes away, once. A handshake
// or refresh under way gets a moment to finish, so the cache is
// written; one stuck waiting for a credential is not worth waiting
// for. The process group gets SIGINT, then SIGKILL; if even that
// leaves the pipes open, a grandchild outside the group holds them,
// and they are closed by force.
func (p *proxy) stop() {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.closing = true
		b := p.backend
		p.mu.Unlock()
		if b != nil {
			waitFor(&p.jobs, finishWait)
			_ = interrupt(b.cmd)
			if !waitDone(b.done, exitWait) {
				_ = kill(b.cmd)
				if !waitDone(b.done, exitWait) {
					for _, c := range b.pipes {
						_ = c.Close()
					}
					<-b.done
				}
			}
		}
		p.jobs.Wait()
		p.readers.Wait()
	})
}

// waitFor waits for the group, but no longer than d.
func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	c := make(chan struct{})
	go func() {
		wg.Wait()
		close(c)
	}()
	return waitDone(c, d)
}

// waitDone waits for the channel to close, but no longer than d.
func waitDone(c <-chan struct{}, d time.Duration) bool {
	select {
	case <-c:
		return true
	case <-time.After(d):
		return false
	}
}

// tail copies a stream through, line by line, and remembers the last
// few lines, each cut to a width: what a failed start gets to report.
type tail struct {
	keep, width int
	mu          sync.Mutex
	last        []string
}

func (t *tail) copy(from io.Reader, to io.Writer) {
	r := bufio.NewReader(from)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			_, _ = to.Write(line)
			t.remember(line)
		}
		if err != nil {
			return
		}
	}
}

func (t *tail) remember(line []byte) {
	s := strings.TrimRight(string(line), "\r\n")
	if len(s) > t.width {
		s = s[:t.width] + "…"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = append(t.last, s)
	if len(t.last) > t.keep {
		t.last = t.last[1:]
	}
}

func (t *tail) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.last...)
}
