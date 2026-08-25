// Command bzmux is the buzz-lakebox MCP multiplexer: a small stdio JSON-RPC
// proxy that fronts N child MCP servers behind buzz-acp's single MCP slot.
// The provider go:embed's this binary and writes it into the sandbox at
// deploy; buzz-agent launches it as BUZZ_ACP_MCP_COMMAND when
// provider_config.mcp_servers has two or more entries.
//
// The contract bzmux honours (issue #16 plan §5):
//
//	§5A. JSON-RPC envelope (the multiplexing core):
//	  - Downstream request routing via a toolName→child map built at merge
//	    time; unknown name → JSON-RPC error upstream (never a hang).
//	  - Per-child id-remapping table: allocate a mux-unique id per forwarded
//	    request, record (muxID → childConn, originalID), restore originalID on
//	    the response. Rewrite only the envelope id — never result/error content.
//	  - Bidirectional: forward child-initiated requests (sampling/createMessage,
//	    roots/list, elicitation/*) and notifications to the agent and route the
//	    agent's response back to the originating child (same id-namespacing in
//	    reverse); map notifications/cancelled ids through the table.
//	  - initialize: answer the agent immediately as bzmux with a conservative
//	    capability set + the agent's offered protocolVersion; spawn + initialize
//	    all children before the first tools/list; a child negotiating an
//	    incompatible protocolVersion fails at --selftest, never silently.
//	  - Hook fan-out: _Stop / _PostCompact arrive as ONE request; fan out to all
//	    children IN PARALLEL (never serialized — two hook timeouts SIGKILL the
//	    group) and aggregate the N responses into one (success iff all succeed;
//	    a child error or budget-exceed → one aggregated error naming it).
//
//	§5B. Catalog & naming:
//	  - Pass bare tool names through UNRENAMED (no child__tool scheme).
//	  - Reject a child tool name containing "__" (hard error).
//	  - Collision across children → hard-exit with a clear message naming both.
//	  - Validate len("bzmux__") + len(toolName) ≤ 64 for every re-exported tool
//	    (buzz-agent adds the "bzmux__" prefix; this is validation, not renaming).
//	  - Keep buzz-dev-mcp's "shell" named "shell".
//	  - Snapshot catalogs at spawn; ignore tools/list_changed. Tool schema
//	    > 4096 bytes → warn (buzz-agent silently replaces it with {}).
//
//	§5C. Process & lifecycle:
//	  - Create NO child process groups (no Setpgid/Setsid); the agent killpg's
//	    the pgid, so grandchildren are reaped for free only if bzmux makes no
//	    new groups.
//	  - Child dies mid-session → return a clear JSON-RPC error frame per call
//	    (never hang); default: stay up so one dead child doesn't kill the other.
//	  - Prefix each child's stderr lines with the child name; keep bzmux's own
//	    logs distinguishable; NO secret values in any log line.
//	  - Per-stage 30s budgets are generous; concurrent child spawn is fine.
//
// The config path is resolved executable-relative (Decision C1):
// filepath.Dir(os.Executable())/mcp-mux.json, with $HOME/.buzz-backend as a
// fallback.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

// logw is the writer for bzmux's own log lines (§5C.11: "bzmux: …" prefix).
// All logging goes to stderr; the agent captures MCP child stderr separately.
var logw = os.Stderr

// logf writes a formatted log line to logw (stderr).
// Write errors are deliberately ignored: stderr logging is fire-and-forget and
// a failed write does not affect bzmux's behaviour.
func logf(format string, a ...any) { _, _ = fmt.Fprintf(logw, format, a...) }

func main() {
	selftest := flag.Bool("selftest", false, "load mcp-mux.json, spawn+initialize every child, merge one tools/list, validate the catalog, and exit without connecting to the agent")
	flag.Parse()

	if *selftest {
		runSelftest()
		// runSelftest calls os.Exit; this return is unreachable but keeps vet happy.
		return
	}

	// Live mode: load config, build proxy, run the agent loop.
	path, err := resolveConfigPath()
	if err != nil {
		logf("bzmux: resolve config: %v\n", err)
		os.Exit(1)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		logf("bzmux: load config %s: %v\n", path, err)
		os.Exit(1)
	}

	p := newProxy(os.Stdin, os.Stdout, cfg)
	if err := p.run(context.Background()); err != nil {
		logf("bzmux: run: %v\n", err)
		os.Exit(1)
	}
}
