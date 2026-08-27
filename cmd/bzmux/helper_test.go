package main

// TestHelperProcess / fakeChildMain — the fake MCP child harness.
// Uses the os.Args[0] + -test.run=TestHelperProcess trick (standard Go pattern):
// when GO_HELPER_PROCESS=1 the test binary acts as a minimal stdio JSON-RPC 2.0
// MCP server instead of running real tests.
//
// Env-var knobs (all optional):
//
//	FAKE_TOOLS               comma-separated tool names to advertise
//	FAKE_PROTOCOL_VERSION    protocolVersion in initialize response (default "2024-11-05")
//	FAKE_ECHO_ENV_FILE       path — write os.Environ() immediately on startup
//	FAKE_SLEEP_BEFORE_CALL_MS  ms to sleep before responding to any tools/call
//	FAKE_EXIT_ON_TOOLS_CALL  "1" → os.Exit(0) when tools/call is received (before reply)
//	FAKE_EXIT_BEFORE_INIT    "1" → os.Exit(0) on the initialize request (before reply)
//	FAKE_NEVER_REPLY         "1" → read stdin but never answer anything (silent child)
//	FAKE_TOOLS_CALL_ERROR    "1" → reply to tools/call with a JSON-RPC error
//	FAKE_SEND_SAMPLING       "1" → send sampling/createMessage id:42 after notifications/initialized
//	FAKE_SAMPLING_ACK_FILE   path — write "ack" when we receive a response to id:42
//	FAKE_SEND_NOTIFICATION   "1" → send a notifications/progress notification after notifications/initialized
//	FAKE_CHILD_NAME          label embedded in echo-response content
//
// When GO_RUN_SELFTEST=1 is also set alongside GO_HELPER_PROCESS=1, the
// subprocess runs runSelftest() instead of acting as a child, enabling
// TestSelftest_* to drive the selftest path end-to-end without invoking
// os.Exit in the test process itself.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is the fake-child entry point.  It is not a real test:
// when GO_HELPER_PROCESS != "1" it returns immediately with no assertions.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		return
	}
	if os.Getenv("GO_RUN_SELFTEST") == "1" {
		runSelftest() // calls os.Exit internally
		os.Exit(0)
	}
	fakeChildMain()
	os.Exit(0)
}

// fakeChildMain speaks minimal MCP (JSON-RPC 2.0 over stdio).
func fakeChildMain() {
	// Write env to file immediately — before reading any messages — so the
	// env-allowlist tests can read it after init completes.
	if f := os.Getenv("FAKE_ECHO_ENV_FILE"); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(os.Environ(), "\n")), 0600)
	}

	toolNames := fakeParseToolNames(os.Getenv("FAKE_TOOLS"))
	childName := os.Getenv("FAKE_CHILD_NAME")
	if childName == "" {
		childName = "fake"
	}
	pv := os.Getenv("FAKE_PROTOCOL_VERSION")
	if pv == "" {
		pv = "2024-11-05"
	}

	enc := json.NewEncoder(os.Stdout)
	sendRaw := func(v interface{}) { _ = enc.Encode(v) }

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method,omitempty"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}

		// Silent child: consume stdin but never answer anything, so the
		// caller hits its (short, injected) read deadline.
		if os.Getenv("FAKE_NEVER_REPLY") == "1" {
			continue
		}

		// Response from upstream (no method, has non-null id): check for
		// the sampling ack id:42.
		if m.Method == "" && len(m.ID) > 0 && string(m.ID) != "null" {
			var id int64
			if json.Unmarshal(m.ID, &id) == nil && id == 42 {
				if f := os.Getenv("FAKE_SAMPLING_ACK_FILE"); f != "" {
					_ = os.WriteFile(f, []byte("ack"), 0600)
				}
			}
			continue
		}

		switch m.Method {
		case "initialize":
			if os.Getenv("FAKE_EXIT_BEFORE_INIT") == "1" {
				os.Exit(0)
			}
			sendRaw(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      m.ID,
				"result": map[string]interface{}{
					"protocolVersion": pv,
					"capabilities":    map[string]interface{}{},
					"serverInfo":      map[string]interface{}{"name": "fake", "version": "0.1"},
				},
			})

		case "notifications/initialized":
			if os.Getenv("FAKE_SEND_SAMPLING") == "1" {
				params, _ := json.Marshal(map[string]interface{}{
					"messages":  []interface{}{},
					"maxTokens": 100,
				})
				sendRaw(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      42,
					"method":  "sampling/createMessage",
					"params":  json.RawMessage(params),
				})
			}
			if os.Getenv("FAKE_SEND_NOTIFICATION") == "1" {
				// Send a generic notification to exercise the nil-agentOut
				// guard in forwardChildNotification (selftest mode).
				sendRaw(map[string]interface{}{
					"jsonrpc": "2.0",
					"method":  "notifications/progress",
					"params":  map[string]interface{}{"progressToken": "tok", "progress": 1},
				})
			}

		case "tools/list":
			tools := fakeBuildTools(toolNames)
			sendRaw(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      m.ID,
				"result":  map[string]interface{}{"tools": tools},
			})

		case "tools/call":
			if os.Getenv("FAKE_EXIT_ON_TOOLS_CALL") == "1" {
				os.Exit(0)
			}
			if ms := os.Getenv("FAKE_SLEEP_BEFORE_CALL_MS"); ms != "" {
				if d, err := strconv.Atoi(ms); err == nil {
					time.Sleep(time.Duration(d) * time.Millisecond)
				}
			}
			if os.Getenv("FAKE_TOOLS_CALL_ERROR") == "1" {
				sendRaw(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      m.ID,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": fmt.Sprintf("fake error from %s", childName),
					},
				})
			} else {
				sendRaw(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      m.ID,
					"result": map[string]interface{}{
						"content": []interface{}{
							map[string]interface{}{
								"type": "text",
								"text": fmt.Sprintf("echo from %s", childName),
							},
						},
					},
				})
			}
			// All other methods (notifications, etc.): no response needed.
		}
	}
}

func fakeParseToolNames(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fakeBuildTools(names []string) []interface{} {
	tools := make([]interface{}, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]interface{}{
			"name":        name,
			"description": "fake tool " + name,
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		})
	}
	return tools
}
