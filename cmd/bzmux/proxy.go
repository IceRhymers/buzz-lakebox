package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

// proxy multiplexes an MCP agent stdio session across N child MCP servers.
// It owns the JSON-RPC envelope (id remapping, routing, fan-out) but passes
// tool names, schemas, params, and result content through verbatim.
type proxy struct {
	agentIn  *lineScanner
	agentOut *lineWriter

	// cfg is the parsed mcp-mux.json; set before run.
	cfg *muxcfg.Config

	// children, routes, and catalog are written once by initChildren (before
	// close(catalogReady)) and read-only afterwards.  Go's channel happens-before
	// guarantees safe reads after <-catalogReady.
	children []*child
	routes   map[string]*child
	catalog  []toolEntry

	// catalogReady is closed once the catalog is merged (or init fails).
	catalogReady chan struct{}
	catalogErr   error // non-nil if initialization failed

	// inflight request tracking (agent→child and child→agent).
	inflightMu    sync.Mutex
	inflightByID  map[int64]*inflightEntry
	inflightByKey map[string]int64 // originalKey → muxID (for cancelled remapping)
	nextID        atomic.Int64
}

// inflightEntry tracks one in-flight JSON-RPC request that bzmux forwarded.
type inflightEntry struct {
	muxID       int64
	dst         *lineWriter     // where to route the response (nil if replyCh set)
	originalID  json.RawMessage // id to restore in the response
	replyCh     chan *msg       // non-nil for synchronous init/fan-out calls
	originalKey string          // for reverse lookup in notifications/cancelled
	toChild     *child          // non-nil when forwarded to a child (for drain)
}

func newProxy(in io.Reader, out io.Writer, cfg *muxcfg.Config) *proxy {
	return &proxy{
		agentIn:       newLineScanner(in),
		agentOut:      newLineWriter(out),
		cfg:           cfg,
		catalogReady:  make(chan struct{}),
		inflightByID:  make(map[int64]*inflightEntry),
		inflightByKey: make(map[string]int64),
	}
}

// run reads agent messages until EOF or ctx cancellation.
func (p *proxy) run(ctx context.Context) error {
	for {
		m, err := p.agentIn.next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if m.isRequest() {
			p.handleAgentRequest(ctx, m)
		} else if m.isNotification() {
			p.handleAgentNotification(m)
		} else if m.isResponse() {
			// Agent responded to a child-initiated request.
			p.handleAgentResponse(m)
		}
	}
}

// handleAgentRequest dispatches a request from the agent.
func (p *proxy) handleAgentRequest(ctx context.Context, m *msg) {
	switch m.Method {
	case "initialize":
		p.handleInitialize(ctx, m)
	case "tools/list":
		go func() {
			if !p.waitCatalog(ctx, m.ID) {
				return
			}
			result, err := buildToolsListResult(p.catalog)
			if err != nil {
				_ = p.agentOut.send(errorResp(m.ID, errCodeInternalError, err.Error()))
				return
			}
			_ = p.agentOut.send(&msg{JSONRPC: "2.0", ID: m.ID, Result: result})
		}()
	case "tools/call":
		go func() {
			if !p.waitCatalog(ctx, m.ID) {
				return
			}
			p.handleToolsCall(ctx, m)
		}()
	default:
		_ = p.agentOut.send(errorResp(m.ID, errCodeMethodNotFound, "method not found: "+m.Method))
	}
}

// waitCatalog waits for catalogReady, sends an error to the agent if it fails,
// and returns false when the caller should abort.
func (p *proxy) waitCatalog(ctx context.Context, reqID json.RawMessage) bool {
	select {
	case <-p.catalogReady:
		if p.catalogErr != nil {
			_ = p.agentOut.send(errorResp(reqID, errCodeInternalError, "initialization failed: "+p.catalogErr.Error()))
			return false
		}
		return true
	case <-ctx.Done():
		_ = p.agentOut.send(errorResp(reqID, errCodeInternalError, "shutting down"))
		return false
	}
}

// handleInitialize responds to the agent's initialize immediately, then spawns
// and initializes all children concurrently in the background.
func (p *proxy) handleInitialize(ctx context.Context, m *msg) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil || params.ProtocolVersion == "" {
		params.ProtocolVersion = "2024-11-05"
	}

	// Respond immediately with bzmux's conservative capability set (§5A.3).
	// Advertise tools:{} (we are a tools provider) plus sampling/roots passthrough
	// since children may initiate those upstream.
	result, _ := json.Marshal(map[string]interface{}{
		"protocolVersion": params.ProtocolVersion,
		"capabilities": map[string]interface{}{
			"tools":    map[string]interface{}{},
			"sampling": map[string]interface{}{},
			"roots":    map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "bzmux",
			"version": "0.1",
		},
	})
	if err := p.agentOut.send(&msg{JSONRPC: "2.0", ID: m.ID, Result: result}); err != nil {
		logf("bzmux: send initialize response: %v\n", err)
	}

	go p.initChildren(ctx, params.ProtocolVersion)
}

// initChildren spawns all configured children concurrently, initializes each
// (within childInitTimeout), merges their catalogs, then closes catalogReady.
func (p *proxy) initChildren(ctx context.Context, protocolVersion string) {
	type childResult struct {
		c   *child
		err error
	}
	ch := make(chan childResult, len(p.cfg.Servers))

	for _, srv := range p.cfg.Servers {
		srv := srv
		go func() {
			env := buildChildEnv(srv.Env)
			c, err := spawn(srv.Name, srv.Command, srv.Args, env)
			if err != nil {
				ch <- childResult{err: fmt.Errorf("spawn %q: %w", srv.Name, err)}
				return
			}
			go c.readLoop(p)
			// Keep today's per-child isolation: the initialize handshake is
			// bounded by a fresh context.Background()+childInitTimeout rather
			// than the parent connection ctx (issue #17, plan step 1b — a
			// deliberate non-change to preserve the live path's byte-behaviour).
			initCtx, cancel := context.WithTimeout(context.Background(), childInitTimeout)
			err = c.initialize(initCtx, p, protocolVersion)
			cancel()
			if err != nil {
				c.markDead(err)
				ch <- childResult{c: c, err: err}
				return
			}
			ch <- childResult{c: c}
		}()
	}

	var goodChildren []*child
	var allChildren []*child // includes dead ones for cleanup
	var errParts []string
	for range p.cfg.Servers {
		r := <-ch
		if r.c != nil {
			allChildren = append(allChildren, r.c)
		}
		if r.err != nil {
			errParts = append(errParts, r.err.Error())
		} else if r.c != nil {
			goodChildren = append(goodChildren, r.c)
		}
	}

	if len(errParts) > 0 {
		// Clean up any successfully-spawned children.
		for _, c := range allChildren {
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
		p.catalogErr = fmt.Errorf("child initialization errors: %s", strings.Join(errParts, "; "))
		close(p.catalogReady)
		return
	}

	// Sort goodChildren into p.cfg.Servers order for deterministic tools/list
	// output: goroutine completion order is nondeterministic.
	configOrder := make(map[string]int, len(p.cfg.Servers))
	for i, srv := range p.cfg.Servers {
		configOrder[srv.Name] = i
	}
	sort.Slice(goodChildren, func(i, j int) bool {
		return configOrder[goodChildren[i].name] < configOrder[goodChildren[j].name]
	})

	routes, catalog, err := mergeCatalogs(goodChildren)
	if err != nil {
		for _, c := range allChildren {
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
		p.catalogErr = err
		close(p.catalogReady)
		return
	}

	p.children = goodChildren
	p.routes = routes
	p.catalog = catalog
	logf("bzmux: catalog ready: %d tools from %d children\n", len(catalog), len(goodChildren))
	close(p.catalogReady)
}

// handleToolsCall routes a tools/call request to the appropriate child, or
// fans out to all children for hook tools (_Stop, _PostCompact).
func (p *proxy) handleToolsCall(ctx context.Context, m *msg) {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		_ = p.agentOut.send(errorResp(m.ID, errCodeInternalError, "invalid tools/call params"))
		return
	}

	// §5A.4: hook fan-out for _Stop and _PostCompact.
	if params.Name == "_Stop" || params.Name == "_PostCompact" {
		go p.fanOut(ctx, m)
		return
	}

	c, ok := p.routes[params.Name]
	if !ok {
		_ = p.agentOut.send(errorResp(m.ID, errCodeMethodNotFound, "tool not found: "+params.Name))
		return
	}
	if c.isDead() {
		_ = p.agentOut.send(errorResp(m.ID, errCodeInternalError, fmt.Sprintf("child %q died", c.name)))
		return
	}

	// No arbitrary timeout is imposed on a regular tools/call: MCP tools
	// legitimately run long (e.g. a shell command executing a lengthy
	// operation), and the agent owns tool-call timeouts end-to-end.
	// Inflight cleanup relies on exactly two paths:
	//   (i)  child death: drainChildInflight removes the entry and delivers
	//        an error frame to the agent;
	//   (ii) agent cancellation: notifications/cancelled → remapCancelledID
	//        → forwardCancelledToChild removes the entry so it does not leak.
	muxID := p.nextID.Add(1)
	originalKey := "a:" + string(m.ID)
	p.inflightMu.Lock()
	p.inflightByID[muxID] = &inflightEntry{
		muxID:       muxID,
		dst:         p.agentOut,
		originalID:  m.ID,
		originalKey: originalKey,
		toChild:     c,
	}
	p.inflightByKey[originalKey] = muxID
	p.inflightMu.Unlock()

	fwd := *m
	fwd.ID = rawIntID(muxID)
	if err := c.send(&fwd); err != nil {
		p.inflightMu.Lock()
		delete(p.inflightByID, muxID)
		delete(p.inflightByKey, originalKey)
		p.inflightMu.Unlock()
		_ = p.agentOut.send(errorResp(m.ID, errCodeInternalError, fmt.Sprintf("child %q: %v", c.name, err)))
	}
}

// fanOut sends m to all live children in parallel and aggregates the responses.
// §5A.4: success iff all children succeed; any error → aggregated error response.
func (p *proxy) fanOut(ctx context.Context, m *msg) {
	var liveChildren []*child
	for _, c := range p.children {
		if !c.isDead() {
			liveChildren = append(liveChildren, c)
		}
	}
	if len(liveChildren) == 0 {
		_ = p.agentOut.send(errorResp(m.ID, errCodeInternalError, "no live children for fan-out"))
		return
	}

	type fanResult struct {
		name   string
		result json.RawMessage
		errMsg string
	}
	results := make(chan fanResult, len(liveChildren))
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for _, c := range liveChildren {
		c := c
		muxID := p.nextID.Add(1)
		replyCh := make(chan *msg, 1)
		p.inflightMu.Lock()
		p.inflightByID[muxID] = &inflightEntry{
			muxID:      muxID,
			replyCh:    replyCh,
			originalID: rawIntID(muxID),
			toChild:    c,
		}
		p.inflightMu.Unlock()

		fwd := *m
		fwd.ID = rawIntID(muxID)
		go func() {
			if err := c.send(&fwd); err != nil {
				p.inflightMu.Lock()
				delete(p.inflightByID, muxID)
				p.inflightMu.Unlock()
				results <- fanResult{name: c.name, errMsg: err.Error()}
				return
			}
			select {
			case resp := <-replyCh:
				if resp.Error != nil {
					results <- fanResult{name: c.name, errMsg: fmt.Sprintf("code %d: %s", resp.Error.Code, resp.Error.Message)}
				} else {
					results <- fanResult{name: c.name, result: resp.Result}
				}
			case <-ctx2.Done():
				p.inflightMu.Lock()
				delete(p.inflightByID, muxID)
				p.inflightMu.Unlock()
				results <- fanResult{name: c.name, errMsg: "timeout"}
			case <-c.deadCh:
				p.inflightMu.Lock()
				delete(p.inflightByID, muxID)
				p.inflightMu.Unlock()
				results <- fanResult{name: c.name, errMsg: "child died"}
			}
		}()
	}

	var errParts []string
	var contents []json.RawMessage
	for range liveChildren {
		r := <-results
		if r.errMsg != "" {
			errParts = append(errParts, r.name+": "+r.errMsg)
		} else {
			contents = append(contents, r.result)
		}
	}
	if len(errParts) > 0 {
		_ = p.agentOut.send(errorResp(m.ID, errCodeInternalError, "fan-out errors: "+strings.Join(errParts, "; ")))
		return
	}
	_ = p.agentOut.send(&msg{JSONRPC: "2.0", ID: m.ID, Result: aggregateContents(contents)})
}

// aggregateContents concatenates the "content" arrays from multiple tools/call results.
func aggregateContents(results []json.RawMessage) json.RawMessage {
	type callResult struct {
		Content []json.RawMessage `json:"content"`
	}
	var allContent []json.RawMessage
	for _, r := range results {
		var cr callResult
		if err := json.Unmarshal(r, &cr); err == nil {
			allContent = append(allContent, cr.Content...)
		}
	}
	if allContent == nil {
		allContent = []json.RawMessage{}
	}
	data, _ := json.Marshal(callResult{Content: allContent})
	return data
}

// handleAgentNotification handles a notification from the agent.
func (p *proxy) handleAgentNotification(m *msg) {
	switch m.Method {
	case "notifications/initialized":
		// Children are already being initialized in the background; nothing to do.
		return
	case "notifications/cancelled":
		// Remap the referenced request id and forward to the appropriate child.
		m = p.remapCancelledID(m, "a:")
		p.forwardCancelledToChild(m)
		return
	}
	// Generic notification broadcast: must gate on catalogReady to avoid a
	// data race on p.children — initChildren writes p.children before
	// close(catalogReady), so reads must follow the close. tools/list and
	// tools/call correctly use waitCatalog; notifications have no request id
	// to send an error on, so run in a goroutine, wait for ready, and drop
	// silently if initialization failed.
	go func() {
		<-p.catalogReady
		if p.catalogErr != nil {
			return
		}
		for _, c := range p.children {
			if !c.isDead() {
				if err := c.send(m); err != nil {
					logf("bzmux: forward notification %q to child %q: %v\n", m.Method, c.name, err)
				}
			}
		}
	}()
}

// handleAgentResponse routes an agent response back to the originating child.
func (p *proxy) handleAgentResponse(m *msg) {
	var muxID int64
	if err := json.Unmarshal(m.ID, &muxID); err != nil {
		logf("bzmux: agent response with non-integer id %s, ignoring\n", m.ID)
		return
	}
	p.routeResponse(muxID, m)
}

// routeResponse looks up the inflight entry for muxID, restores the original id,
// and delivers the response to the registered destination.
func (p *proxy) routeResponse(muxID int64, m *msg) {
	p.inflightMu.Lock()
	entry, ok := p.inflightByID[muxID]
	if ok {
		delete(p.inflightByID, muxID)
		if entry.originalKey != "" {
			delete(p.inflightByKey, entry.originalKey)
		}
	}
	p.inflightMu.Unlock()

	if !ok {
		logf("bzmux: no inflight entry for mux id %d\n", muxID)
		return
	}

	// Restore the original id before forwarding.
	m.ID = entry.originalID

	if entry.replyCh != nil {
		select {
		case entry.replyCh <- m:
		default:
			logf("bzmux: replyCh full for mux id %d\n", muxID)
		}
		return
	}
	if entry.dst != nil {
		if err := entry.dst.send(m); err != nil {
			logf("bzmux: route response: %v\n", err)
		}
	}
}

// drainChildInflight finds all inflight entries routed to c and delivers error
// responses, called when c's read loop detects the child has died (§5C.10).
func (p *proxy) drainChildInflight(c *child) {
	p.inflightMu.Lock()
	var toProcess []*inflightEntry
	for muxID, entry := range p.inflightByID {
		if entry.toChild == c {
			if entry.originalKey != "" {
				delete(p.inflightByKey, entry.originalKey)
			}
			delete(p.inflightByID, muxID)
			toProcess = append(toProcess, entry)
		}
	}
	p.inflightMu.Unlock()

	for _, entry := range toProcess {
		errMsg := errorResp(entry.originalID, errCodeInternalError, fmt.Sprintf("child %q died", c.name))
		if entry.replyCh != nil {
			select {
			case entry.replyCh <- errMsg:
			default:
			}
		} else if entry.dst != nil {
			_ = entry.dst.send(errMsg)
		}
	}
}

// childCall performs a synchronous JSON-RPC call to child c with ctx timeout.
// It registers an inflight entry with a replyCh, sends the request, and waits.
func (p *proxy) childCall(ctx context.Context, c *child, method string, params json.RawMessage) (*msg, error) {
	muxID := p.nextID.Add(1)
	replyCh := make(chan *msg, 1)

	p.inflightMu.Lock()
	p.inflightByID[muxID] = &inflightEntry{
		muxID:      muxID,
		replyCh:    replyCh,
		originalID: rawIntID(muxID),
		toChild:    c,
	}
	p.inflightMu.Unlock()

	req := &msg{JSONRPC: "2.0", ID: rawIntID(muxID), Method: method, Params: params}
	if err := c.send(req); err != nil {
		p.inflightMu.Lock()
		delete(p.inflightByID, muxID)
		p.inflightMu.Unlock()
		return nil, err
	}

	select {
	case resp := <-replyCh:
		if resp.Error != nil {
			return nil, fmt.Errorf("RPC %s error %d: %s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp, nil
	case <-c.deadCh:
		p.inflightMu.Lock()
		delete(p.inflightByID, muxID)
		p.inflightMu.Unlock()
		return nil, fmt.Errorf("child %q died during %s", c.name, method)
	case <-ctx.Done():
		p.inflightMu.Lock()
		delete(p.inflightByID, muxID)
		p.inflightMu.Unlock()
		return nil, fmt.Errorf("child %q %s timeout: %w", c.name, method, ctx.Err())
	}
}

// forwardChildRequest forwards a child-initiated request upstream to the agent.
// The id is remapped: child's original id → mux-allocated id sent to agent.
// When the agent responds (handleAgentResponse), the response is routed back.
// In selftest mode agentOut is nil; child-initiated traffic is dropped silently.
func (p *proxy) forwardChildRequest(c *child, m *msg) {
	if p.agentOut == nil {
		// selftest mode: no agent connected; drop child-initiated requests.
		return
	}
	muxID := p.nextID.Add(1)
	originalKey := fmt.Sprintf("c:%s:%s", c.name, string(m.ID))
	p.inflightMu.Lock()
	p.inflightByID[muxID] = &inflightEntry{
		muxID:       muxID,
		dst:         c.writer,
		originalID:  m.ID,
		originalKey: originalKey,
	}
	p.inflightByKey[originalKey] = muxID
	p.inflightMu.Unlock()

	fwd := *m
	fwd.ID = rawIntID(muxID)
	if err := p.agentOut.send(&fwd); err != nil {
		logf("bzmux: forward child %q request upstream: %v\n", c.name, err)
		p.inflightMu.Lock()
		delete(p.inflightByID, muxID)
		delete(p.inflightByKey, originalKey)
		p.inflightMu.Unlock()
	}
}

// forwardChildNotification forwards a notification from a child to the agent.
// tools/list_changed is silently dropped (§5B.8). notifications/cancelled ids
// are remapped through the inflight table.
// In selftest mode agentOut is nil; child-initiated traffic is dropped silently.
func (p *proxy) forwardChildNotification(c *child, m *msg) {
	if m.Method == "notifications/tools/list_changed" {
		logf("bzmux: ignoring tools/list_changed from child %q\n", c.name)
		return
	}
	if m.Method == "notifications/cancelled" {
		m = p.remapCancelledID(m, fmt.Sprintf("c:%s:", c.name))
	}
	if p.agentOut == nil {
		// selftest mode: no agent connected; drop child-initiated notifications.
		return
	}
	if err := p.agentOut.send(m); err != nil {
		logf("bzmux: forward child %q notification %q: %v\n", c.name, m.Method, err)
	}
}

// remapCancelledID rewrites the params.requestId in a notifications/cancelled
// message from the original id (used by keyPrefix's side) to the mux-allocated
// id.  Returns the original message unchanged when the id cannot be resolved.
func (p *proxy) remapCancelledID(m *msg, keyPrefix string) *msg {
	if len(m.Params) == 0 {
		return m
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return m
	}
	reqID, ok := params["requestId"]
	if !ok {
		return m
	}
	key := keyPrefix + string(reqID)
	p.inflightMu.Lock()
	muxID, ok := p.inflightByKey[key]
	p.inflightMu.Unlock()
	if !ok {
		return m // already completed, forward as-is
	}
	params["requestId"] = rawIntID(muxID)
	newParams, err := json.Marshal(params)
	if err != nil {
		return m
	}
	clone := *m
	clone.Params = newParams
	return &clone
}

// forwardCancelledToChild extracts the muxID from a remapped notifications/cancelled
// params and forwards the notification to the child the original request was routed to.
// It also removes the inflight entry so the slot does not leak: once the agent
// cancels a request, no response will be routed for it.  Any late response from the
// child will be silently dropped by routeResponse ("no inflight entry for mux id").
func (p *proxy) forwardCancelledToChild(m *msg) {
	if len(m.Params) == 0 {
		return
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return
	}
	reqID, ok := params["requestId"]
	if !ok {
		return
	}
	var muxID int64
	if err := json.Unmarshal(reqID, &muxID); err != nil {
		return
	}
	p.inflightMu.Lock()
	entry, ok := p.inflightByID[muxID]
	if ok {
		// Remove the entry now: the agent cancelled this request, so we will
		// not be routing a response for it.
		delete(p.inflightByID, muxID)
		if entry.originalKey != "" {
			delete(p.inflightByKey, entry.originalKey)
		}
	}
	p.inflightMu.Unlock()
	if !ok || entry.toChild == nil || entry.toChild.isDead() {
		return
	}
	if err := entry.toChild.send(m); err != nil {
		logf("bzmux: forward cancelled to child %q: %v\n", entry.toChild.name, err)
	}
}
