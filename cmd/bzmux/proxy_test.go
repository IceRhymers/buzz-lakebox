package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

// ---------------------------------------------------------------------------
// Test harness helpers
// ---------------------------------------------------------------------------

// msgReader drains a lineScanner into a buffered channel so recv calls are
// non-destructive on timeout: a timed-out recv does not swallow a message
// that a subsequent recv should see.
type msgReader struct {
	ch chan *msg
}

func newMsgReader(sc *lineScanner) *msgReader {
	mr := &msgReader{ch: make(chan *msg, 128)}
	go func() {
		defer close(mr.ch)
		for {
			m, err := sc.next()
			if err != nil {
				return
			}
			mr.ch <- m
		}
	}()
	return mr
}

func (mr *msgReader) recv(ctx context.Context) (*msg, error) {
	select {
	case m, ok := <-mr.ch:
		if !ok {
			return nil, io.EOF
		}
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// proxyHarness wires agent↔proxy over in-memory pipes and manages the proxy
// goroutine lifetime.
type proxyHarness struct {
	w      *lineWriter
	r      *msgReader
	pipeW  io.WriteCloser
	ctx    context.Context
	cancel context.CancelFunc
	done   chan error
}

func newProxyHarness(t *testing.T, cfg *muxcfg.Config) *proxyHarness {
	t.Helper()
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()

	p := newProxy(inputR, outputW, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	done := make(chan error, 1)
	go func() {
		err := p.run(ctx)
		_ = outputW.Close()
		done <- err
	}()

	h := &proxyHarness{
		w:      newLineWriter(inputW),
		r:      newMsgReader(newLineScanner(outputR)),
		pipeW:  inputW,
		ctx:    ctx,
		cancel: cancel,
		done:   done,
	}
	t.Cleanup(func() {
		_ = inputW.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("proxy did not stop within cleanup timeout")
		}
	})
	return h
}

func (h *proxyHarness) mustSend(t *testing.T, m *msg) {
	t.Helper()
	if err := h.w.send(m); err != nil {
		t.Fatalf("proxyHarness.mustSend: %v", err)
	}
}

func (h *proxyHarness) mustRecv(t *testing.T, d time.Duration) *msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, d)
	defer cancel()
	m, err := h.r.recv(ctx)
	if err != nil {
		t.Fatalf("proxyHarness.mustRecv: %v", err)
	}
	return m
}

// initialize performs the MCP agent initialize handshake and returns the
// initialize response.  The caller must separately call toolsList to wait for
// catalog readiness.
func (h *proxyHarness) initialize(t *testing.T) *msg {
	t.Helper()
	params, _ := json.Marshal(map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "test-agent", "version": "0.1"},
	})
	h.mustSend(t, &msg{
		JSONRPC: "2.0",
		ID:      rawIntID(1),
		Method:  "initialize",
		Params:  params,
	})
	resp := h.mustRecv(t, 5*time.Second)
	if resp.Error != nil {
		t.Fatalf("initialize error: %s", resp.Error.Message)
	}
	// Send notifications/initialized notification.
	h.mustSend(t, &msg{JSONRPC: "2.0", Method: "notifications/initialized"})
	return resp
}

// toolsList sends tools/list (id=2) and waits for the response, blocking until
// the proxy's catalog is ready (i.e., all children have initialized).
func (h *proxyHarness) toolsList(t *testing.T) *msg {
	t.Helper()
	h.mustSend(t, &msg{JSONRPC: "2.0", ID: rawIntID(2), Method: "tools/list"})
	resp := h.mustRecv(t, 15*time.Second) // generous: waits for child init
	if resp.Error != nil {
		t.Fatalf("tools/list error: %s", resp.Error.Message)
	}
	return resp
}

// toolNames decodes the tool names from a tools/list response.
func toolNames(t *testing.T, resp *msg) []string {
	t.Helper()
	var r struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("toolNames: parse result: %v", err)
	}
	names := make([]string, len(r.Tools))
	for i, tool := range r.Tools {
		names[i] = tool.Name
	}
	return names
}

// toolCallMsg builds a tools/call msg.
func toolCallMsg(id int64, toolName string) *msg {
	params, _ := json.Marshal(map[string]interface{}{
		"name":      toolName,
		"arguments": map[string]interface{}{},
	})
	return &msg{JSONRPC: "2.0", ID: rawIntID(id), Method: "tools/call", Params: params}
}

// responseEchoContent extracts the text content from an echo response.
func responseEchoContent(t *testing.T, resp *msg) string {
	t.Helper()
	if resp.Error != nil {
		return ""
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil || len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

// testBin returns the absolute path of the current test binary (used as the
// fake-child command via the TestHelperProcess trick).
func testBin(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// fakeSrv builds a muxcfg.Server that runs the test binary as a fake MCP child.
func fakeSrv(t *testing.T, name, tools string, extras map[string]string) muxcfg.Server {
	t.Helper()
	env := map[string]string{
		"GO_HELPER_PROCESS": "1",
		"FAKE_TOOLS":        tools,
		"FAKE_CHILD_NAME":   name,
	}
	for k, v := range extras {
		env[k] = v
	}
	return muxcfg.Server{
		Name:    name,
		Command: testBin(t),
		Args:    []string{"-test.run=TestHelperProcess", "--"},
		Env:     muxcfg.ServerEnv{Set: env},
	}
}

// msgID parses the integer id from a msg, returning -1 on failure.
func msgID(m *msg) int64 {
	var id int64
	if json.Unmarshal(m.ID, &id) != nil {
		return -1
	}
	return id
}

// ---------------------------------------------------------------------------
// TestProxy_IdRemapping_CrossChildCollision
// Two fake children both reply with whatever mux-id bzmux assigned.
// Bzmux must restore each original agent id and route to the right destination.
// ---------------------------------------------------------------------------
func TestProxy_IdRemapping_CrossChildCollision(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrv(t, "child-a", "toolA", nil),
			fakeSrv(t, "child-b", "toolB", nil),
		},
	}
	h := newProxyHarness(t, cfg)
	h.initialize(t)
	names := toolNames(t, h.toolsList(t))
	if len(names) != 2 {
		t.Fatalf("expected 2 tools in catalog, got %v", names)
	}

	// Send two concurrent tool calls before reading responses.
	const idA, idB = int64(100), int64(200)
	h.mustSend(t, toolCallMsg(idA, "toolA"))
	h.mustSend(t, toolCallMsg(idB, "toolB"))

	// Collect both responses (order may vary).
	responses := make(map[int64]*msg)
	for i := 0; i < 2; i++ {
		m := h.mustRecv(t, 10*time.Second)
		responses[msgID(m)] = m
	}

	respA, okA := responses[idA]
	respB, okB := responses[idB]
	if !okA {
		t.Fatalf("no response for toolA call (id=%d); got ids: %v", idA, responseIDs(responses))
	}
	if !okB {
		t.Fatalf("no response for toolB call (id=%d); got ids: %v", idB, responseIDs(responses))
	}

	// Verify content routes to the correct child (no cross-talk).
	if got := responseEchoContent(t, respA); !strings.Contains(got, "child-a") {
		t.Errorf("toolA response content %q: want 'child-a'", got)
	}
	if got := responseEchoContent(t, respB); !strings.Contains(got, "child-b") {
		t.Errorf("toolB response content %q: want 'child-b'", got)
	}
}

func responseIDs(m map[int64]*msg) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	return ids
}

// ---------------------------------------------------------------------------
// TestProxy_ToolsCallRouting
// A call for child-B's tool must reach B; an unknown tool → JSON-RPC error.
// ---------------------------------------------------------------------------
func TestProxy_ToolsCallRouting(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrv(t, "child-a", "toolA", nil),
			fakeSrv(t, "child-b", "toolB", nil),
		},
	}
	h := newProxyHarness(t, cfg)
	h.initialize(t)
	h.toolsList(t)

	// Call for toolB: response must come from child-b.
	h.mustSend(t, toolCallMsg(50, "toolB"))
	resp := h.mustRecv(t, 10*time.Second)
	if resp.Error != nil {
		t.Fatalf("toolB call returned error: %s", resp.Error.Message)
	}
	if got := responseEchoContent(t, resp); !strings.Contains(got, "child-b") {
		t.Errorf("toolB routed to wrong child; content: %q", got)
	}
	if msgID(resp) != 50 {
		t.Errorf("toolB response id: want 50, got %d", msgID(resp))
	}

	// Unknown tool → JSON-RPC error (not a hang).
	h.mustSend(t, toolCallMsg(51, "nosuchtool"))
	errResp := h.mustRecv(t, 5*time.Second)
	if errResp.Error == nil {
		t.Fatalf("unknown tool: expected error response, got result: %s", errResp.Result)
	}
	if errResp.Error.Code != errCodeMethodNotFound {
		t.Errorf("unknown tool error code: want %d, got %d", errCodeMethodNotFound, errResp.Error.Code)
	}
	if msgID(errResp) != 51 {
		t.Errorf("unknown tool error id: want 51, got %d", msgID(errResp))
	}
}

// ---------------------------------------------------------------------------
// TestProxy_FanOut_Parallel
// _Stop must fan-out to ALL children IN PARALLEL (not serial).
// Both fake children sleep 200ms before responding; parallel total ≈ 200ms,
// serial total ≥ 400ms.  Assert elapsed < 350ms.
// ---------------------------------------------------------------------------
func TestProxy_FanOut_Parallel(t *testing.T) {
	sleepEnv := map[string]string{"FAKE_SLEEP_BEFORE_CALL_MS": "200"}
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrv(t, "child-a", "toolA", sleepEnv),
			fakeSrv(t, "child-b", "toolB", sleepEnv),
		},
	}
	h := newProxyHarness(t, cfg)
	h.initialize(t)
	h.toolsList(t)

	start := time.Now()
	h.mustSend(t, toolCallMsg(10, "_Stop"))
	resp := h.mustRecv(t, 10*time.Second)
	elapsed := time.Since(start)

	if resp.Error != nil {
		t.Fatalf("_Stop fan-out error: %s", resp.Error.Message)
	}
	if elapsed > 350*time.Millisecond {
		t.Errorf("_Stop took %v; want <350ms; suggests serial fan-out (not parallel)", elapsed)
	}
}

// ---------------------------------------------------------------------------
// TestProxy_FanOut_OneErrorAggregated
// One child erroring on _Stop → aggregated error response naming that child.
// ---------------------------------------------------------------------------
func TestProxy_FanOut_OneErrorAggregated(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrv(t, "child-ok", "toolA", nil),
			fakeSrv(t, "child-bad", "toolB", map[string]string{"FAKE_TOOLS_CALL_ERROR": "1"}),
		},
	}
	h := newProxyHarness(t, cfg)
	h.initialize(t)
	h.toolsList(t)

	h.mustSend(t, toolCallMsg(20, "_Stop"))
	resp := h.mustRecv(t, 10*time.Second)

	if resp.Error == nil {
		t.Fatal("expected aggregated error response for _Stop with one failing child, got success")
	}
	if !strings.Contains(resp.Error.Message, "child-bad") {
		t.Errorf("error message %q does not name the failing child 'child-bad'", resp.Error.Message)
	}
}

// ---------------------------------------------------------------------------
// TestProxy_ChildInitiatedRequest
// A fake child sends sampling/createMessage upstream; bzmux forwards it to the
// agent and routes the agent's response back to the child with the original id.
// ---------------------------------------------------------------------------
func TestProxy_ChildInitiatedRequest(t *testing.T) {
	ackFile := t.TempDir() + "/sampling.ack"

	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrv(t, "child-a", "toolA", map[string]string{
				"FAKE_SEND_SAMPLING":     "1",
				"FAKE_SAMPLING_ACK_FILE": ackFile,
			}),
		},
	}
	h := newProxyHarness(t, cfg)

	// Step 1: initialize handshake (don't block on tools/list yet).
	h.initialize(t)

	// Step 2: Send tools/list so the catalog initializes in background;
	// also drain messages until we see BOTH the tools/list response and the
	// forwarded sampling/createMessage (order is non-deterministic).
	h.mustSend(t, &msg{JSONRPC: "2.0", ID: rawIntID(2), Method: "tools/list"})

	var samplingReq *msg
	var toolsListResp *msg

	for samplingReq == nil || toolsListResp == nil {
		m := h.mustRecv(t, 10*time.Second)
		var idNum int64
		switch {
		case m.Method == "sampling/createMessage":
			samplingReq = m
		case m.Method == "" && json.Unmarshal(m.ID, &idNum) == nil && idNum == 2:
			toolsListResp = m
		}
	}

	if toolsListResp.Error != nil {
		t.Fatalf("tools/list error: %s", toolsListResp.Error.Message)
	}

	// Step 3: Send sampling response back using the proxy-remapped id.
	h.mustSend(t, &msg{
		JSONRPC: "2.0",
		ID:      samplingReq.ID,
		Result:  json.RawMessage(`{"model":"test","stopReason":"endTurn","role":"assistant","content":{"type":"text","text":"ok"}}`),
	})

	// Step 4: Wait for child to acknowledge it received the response (ack file).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ackFile); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(ackFile); err != nil {
		t.Error("sampling ack file never written: child did not receive sampling response routed back to it")
	}
}

// ---------------------------------------------------------------------------
// TestProxy_ChildDiesMidSession
// Killing a fake child mid-session must yield a clean error frame (no hang).
// The surviving child must still serve tool calls.
// ---------------------------------------------------------------------------
func TestProxy_ChildDiesMidSession(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			// child-a exits immediately when it receives any tools/call.
			fakeSrv(t, "child-a", "toolA", map[string]string{"FAKE_EXIT_ON_TOOLS_CALL": "1"}),
			fakeSrv(t, "child-b", "toolB", nil),
		},
	}
	h := newProxyHarness(t, cfg)
	h.initialize(t)
	h.toolsList(t)

	// Call child-a's tool: it exits before responding → drain + error.
	h.mustSend(t, toolCallMsg(60, "toolA"))
	errResp := h.mustRecv(t, 10*time.Second)
	if errResp.Error == nil {
		t.Fatal("expected error after child-a died, got success")
	}
	if msgID(errResp) != 60 {
		t.Errorf("dead-child error id: want 60, got %d", msgID(errResp))
	}

	// child-b must still serve normally.
	h.mustSend(t, toolCallMsg(61, "toolB"))
	okResp := h.mustRecv(t, 10*time.Second)
	if okResp.Error != nil {
		t.Fatalf("child-b should still serve after child-a died; got error: %s", okResp.Error.Message)
	}
	if !strings.Contains(responseEchoContent(t, okResp), "child-b") {
		t.Errorf("child-b response content wrong: %q", responseEchoContent(t, okResp))
	}
}

// ---------------------------------------------------------------------------
// TestProxy_EnvAllowlist_LeastPrivilege
// A child with Inherit:[SECRET_BZMUX_A] receives it; a child without it does not.
// Set overrides Inherit for the same key.
// ---------------------------------------------------------------------------
func TestProxy_EnvAllowlist_LeastPrivilege(t *testing.T) {
	t.Setenv("SECRET_BZMUX_A", "secret_val_a")
	t.Setenv("SECRET_BZMUX_B", "secret_val_b")

	tmpDir := t.TempDir()
	fileA := tmpDir + "/envA.txt"
	fileB := tmpDir + "/envB.txt"

	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			{
				Name:    "child-a",
				Command: testBin(t),
				Args:    []string{"-test.run=TestHelperProcess", "--"},
				Env: muxcfg.ServerEnv{
					Inherit: []string{"SECRET_BZMUX_A"},
					Set: map[string]string{
						"GO_HELPER_PROCESS":  "1",
						"FAKE_TOOLS":         "toolA",
						"FAKE_CHILD_NAME":    "child-a",
						"FAKE_ECHO_ENV_FILE": fileA,
					},
				},
			},
			{
				Name:    "child-b",
				Command: testBin(t),
				Args:    []string{"-test.run=TestHelperProcess", "--"},
				Env: muxcfg.ServerEnv{
					// No Inherit for SECRET_BZMUX_A or SECRET_BZMUX_B.
					Set: map[string]string{
						"GO_HELPER_PROCESS":  "1",
						"FAKE_TOOLS":         "toolB",
						"FAKE_CHILD_NAME":    "child-b",
						"FAKE_ECHO_ENV_FILE": fileB,
						"EXPLICIT_VAR":       "explicit_val_b",
					},
				},
			},
		},
	}
	h := newProxyHarness(t, cfg)
	h.initialize(t)
	h.toolsList(t) // sync point: all children initialized, env files written

	// child-a: SECRET_BZMUX_A must be present, SECRET_BZMUX_B must not be.
	checkEnvFile(t, fileA, "child-a",
		[]string{"SECRET_BZMUX_A=secret_val_a"},
		[]string{"SECRET_BZMUX_B"},
	)
	// child-b: neither secret must be present; EXPLICIT_VAR must be.
	checkEnvFile(t, fileB, "child-b",
		[]string{"EXPLICIT_VAR=explicit_val_b"},
		[]string{"SECRET_BZMUX_A", "SECRET_BZMUX_B"},
	)
}

// checkEnvFile reads an env file (one KEY=VALUE per line) and asserts that
// each entry in mustContain is present and each key in mustNotContain is absent.
func checkEnvFile(t *testing.T, path, childName string, mustContain, mustNotContain []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: read env file %s: %v", childName, path, err)
	}
	lines := strings.Split(string(data), "\n")

	for _, want := range mustContain {
		found := false
		for _, line := range lines {
			if line == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: env file missing %q", childName, want)
		}
	}
	for _, key := range mustNotContain {
		prefix := key + "="
		for _, line := range lines {
			if strings.HasPrefix(line, prefix) {
				t.Errorf("%s: env file should NOT contain %q but got: %q", childName, key, line)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// TestInitializeReportsConservativeCaps
// Drive initialize through the proxy and assert:
//   - capabilities contains at least "tools: {}"
//   - the response echoes the agent's offered protocolVersion
//
// ---------------------------------------------------------------------------
func TestInitializeReportsConservativeCaps(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrv(t, "child-a", "toolA", nil),
		},
	}
	h := newProxyHarness(t, cfg)
	resp := h.initialize(t)

	var result struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("parse initialize result: %v", err)
	}
	// Must echo the agent's offered protocolVersion.
	if result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion: got %q, want %q", result.ProtocolVersion, "2024-11-05")
	}
	// Must advertise tools: {}.
	if _, ok := result.Capabilities["tools"]; !ok {
		t.Error("capabilities.tools not present in initialize response")
	}
}

// ---------------------------------------------------------------------------
// TestProxy_GenericNotification_NoCatalogRace
// A generic notification sent while initialization is still in progress must
// not race on p.children.  Meaningful under -race (run with go test -race).
// ---------------------------------------------------------------------------
func TestProxy_GenericNotification_NoCatalogRace(t *testing.T) {
	cfg := &muxcfg.Config{
		Servers: []muxcfg.Server{
			fakeSrv(t, "child-a", "toolA", nil),
		},
	}
	h := newProxyHarness(t, cfg)

	// initialize starts child init in the background; catalog may not be ready.
	h.initialize(t)

	// Send a generic notification immediately — catalog may still be initializing.
	h.mustSend(t, &msg{
		JSONRPC: "2.0",
		Method:  "notifications/progress",
		Params:  json.RawMessage(`{"progressToken":"tok","progress":1}`),
	})

	// Wait for the catalog to become ready via tools/list; no race should fire.
	h.toolsList(t)
}

// ---------------------------------------------------------------------------
// TestSpawn_NoProcessGroup
// §5C.9: spawn must not set Setpgid or Setsid; the child must share the
// test process's process group so the agent's killpg reaps grandchildren.
// ---------------------------------------------------------------------------
func TestSpawn_NoProcessGroup(t *testing.T) {
	exe := testBin(t)
	c, err := spawn("test-child", exe,
		[]string{"-test.run=TestHelperProcess", "--"},
		[]string{"GO_HELPER_PROCESS=1", "FAKE_TOOLS=noop"},
	)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() {
		c.cmd.Process.Kill() //nolint:errcheck
		c.cmd.Wait()         //nolint:errcheck
	})

	// SysProcAttr must either be nil or must not set Setpgid/Setsid.
	if attr := c.cmd.SysProcAttr; attr != nil {
		if attr.Setpgid {
			t.Error("spawn: SysProcAttr.Setpgid=true creates a new process group (violates §5C.9)")
		}
		if attr.Setsid {
			t.Error("spawn: SysProcAttr.Setsid=true creates a new session (violates §5C.9)")
		}
	}

	// Verify at OS level: child's process group == parent's process group.
	parentPgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Skipf("Getpgid(parent): %v", err)
	}
	childPgid, err := syscall.Getpgid(c.cmd.Process.Pid)
	if err != nil {
		t.Skipf("Getpgid(child): %v", err)
	}
	if parentPgid != childPgid {
		t.Errorf("child pgid %d != parent pgid %d: spawn created a new process group", childPgid, parentPgid)
	}
}
