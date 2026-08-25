package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const childInitTimeout = 30 * time.Second

// child represents one spawned child MCP server.
type child struct {
	name   string
	cmd    *exec.Cmd
	writer *lineWriter // thread-safe write to child stdin

	// tools is populated after successful initialization; read-only afterwards.
	tools []toolEntry

	// protocolVersion is what the child reported in its initialize response.
	protocolVersion string

	deadOnce sync.Once
	deadCh   chan struct{} // closed when the child process exits
	deadErr  error         // set before deadCh is closed
}

// spawn creates and starts a child process from the given server config.
// It does NOT start the read loop; call readLoop after spawn.
func spawn(serverName, command string, args []string, env []string) (*child, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = env
	// §5C.9: no new process group — the agent's killpg reaps grandchildren.
	// Do NOT set cmd.SysProcAttr.Setpgid or Setsid.

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("child %q stdin pipe: %w", serverName, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("child %q stdout pipe: %w", serverName, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("child %q stderr pipe: %w", serverName, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("child %q start: %w", serverName, err)
	}

	c := &child{
		name:   serverName,
		cmd:    cmd,
		writer: newLineWriter(stdin),
		deadCh: make(chan struct{}),
	}

	// §5C.11: prefix child stderr lines with "[name] ".
	go c.copyStderr(stderr)

	// Store stdout for the read loop (caller starts it).
	c.cmd.Stdout = nil // already piped above; store reader separately.
	// We attach the stdout reader to the child for readLoop.
	c.attachStdout(stdout)

	return c, nil
}

// stdoutR holds the stdout pipe after spawn; accessed only by readLoop.
var stdoutPipes sync.Map // *child → io.ReadCloser

func (c *child) attachStdout(r io.ReadCloser) {
	stdoutPipes.Store(c, r)
}

func (c *child) takeStdout() io.ReadCloser {
	v, _ := stdoutPipes.LoadAndDelete(c)
	if v == nil {
		return nil
	}
	return v.(io.ReadCloser)
}

// send writes a message to the child's stdin.
func (c *child) send(m *msg) error {
	select {
	case <-c.deadCh:
		return fmt.Errorf("child %q is dead", c.name)
	default:
	}
	return c.writer.send(m)
}

// isDead reports whether the child process has exited.
func (c *child) isDead() bool {
	select {
	case <-c.deadCh:
		return true
	default:
		return false
	}
}

// markDead closes deadCh idempotently and records the error.
func (c *child) markDead(err error) {
	c.deadOnce.Do(func() {
		c.deadErr = err
		close(c.deadCh)
	})
}

// copyStderr reads from r and writes each line to logw prefixed with the child name.
// Secret values must never appear in stderr; this is guaranteed by the per-child
// env allowlist (§5C.11 — bzmux never passes secret env to children that don't
// list them, so they can't accidentally echo them).
func (c *child) copyStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		logf("[%s] %s\n", c.name, sc.Text())
	}
}

// readLoop reads messages from the child's stdout and routes them via the proxy.
// It runs as a goroutine and calls c.markDead when the child exits.
func (c *child) readLoop(p *proxy) {
	r := c.takeStdout()
	if r == nil {
		logf("bzmux: child %q has no stdout pipe\n", c.name)
		c.markDead(fmt.Errorf("no stdout pipe"))
		return
	}
	sc := newLineScanner(r)
	for {
		m, err := sc.next()
		if err != nil {
			c.markDead(err)
			logf("bzmux: child %q read loop ended: %v\n", c.name, err)
			// Drain any pending inflight entries routed to this child.
			p.drainChildInflight(c)
			// Wait for the process to exit so we get the exit status.
			if werr := c.cmd.Wait(); werr != nil {
				logf("bzmux: child %q exited: %v\n", c.name, werr)
			}
			return
		}

		if m.isResponse() {
			// Response to a bzmux-forwarded request (agent or init).
			var muxID int64
			if err := json.Unmarshal(m.ID, &muxID); err != nil {
				logf("bzmux: child %q response id not integer: %s\n", c.name, m.ID)
				continue
			}
			p.routeResponse(muxID, m)
		} else if m.isRequest() {
			// Child-initiated upstream request (sampling/createMessage, roots/list, etc.).
			p.forwardChildRequest(c, m)
		} else if m.isNotification() {
			// Notification from child → forward upstream.
			p.forwardChildNotification(c, m)
		}
	}
}

// initialize performs the MCP initialization handshake with the child:
// sends initialize, waits for the response, sends notifications/initialized,
// then fetches the tool catalog with tools/list.
// All of this happens within childInitTimeout.
func (c *child) initialize(p *proxy, protocolVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), childInitTimeout)
	defer cancel()

	// Build initialize params.
	initParams, _ := json.Marshal(map[string]interface{}{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]interface{}{
			"sampling": map[string]interface{}{},
			"roots":    map[string]interface{}{},
		},
		"clientInfo": map[string]interface{}{
			"name":    "bzmux",
			"version": "0.1",
		},
	})

	resp, err := p.childCall(ctx, c, "initialize", initParams)
	if err != nil {
		return fmt.Errorf("child %q initialize: %w", c.name, err)
	}

	// Parse the child's reported protocol version.
	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(resp.Result, &initResult); err == nil {
		c.protocolVersion = initResult.ProtocolVersion
	}

	// Send notifications/initialized.
	if err := c.send(&msg{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		return fmt.Errorf("child %q notifications/initialized: %w", c.name, err)
	}

	// Fetch the tool catalog.
	toolsResp, err := p.childCall(ctx, c, "tools/list", nil)
	if err != nil {
		return fmt.Errorf("child %q tools/list: %w", c.name, err)
	}

	var listResult toolsListResult
	if err := json.Unmarshal(toolsResp.Result, &listResult); err != nil {
		return fmt.Errorf("child %q tools/list result: %w", c.name, err)
	}

	tools, err := parseTools(c.name, listResult.Tools)
	if err != nil {
		return err
	}
	c.tools = tools

	logf("bzmux: child %q initialized, %d tools\n", c.name, len(tools))
	return nil
}
