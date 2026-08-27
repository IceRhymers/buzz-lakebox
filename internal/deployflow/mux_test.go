package deployflow

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/install"
	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// mcpMuxReq returns a 2-entry mcp_servers DeployRequest (McpMux mode).
func mcpMuxReq() *payload.DeployRequest {
	req := buildReq(reqOpts{})
	req.ProviderConfig.McpServers = []string{"buzz-dev-mcp", "shellbox-mcp"}
	return req
}

// TestDeploy_McpMux_StepsInOrder pins the mux install sequence: mux-bin-write
// → mux-cfg-write → mux-selftest appear after install-exec and before
// verify-exec/launch-exec for a 2-entry mcp_servers payload.
func TestDeploy_McpMux_StepsInOrder(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-1")

	if _, err := h.dep.Deploy(mcpMuxReq()); err != nil {
		t.Fatalf("mcp mux deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{
		"SSH:install-exec",
		"SSH:mux-bin-write",
		"SSH:mux-cfg-write",
		"SSH:mux-selftest",
		"SSH:verify-exec",
		"SSH:launch-exec",
	})
}

// TestDeploy_McpMux_StepsAfterExtraBins asserts mux steps appear after
// extra-bins steps when both are present, maintaining the correct ordering:
// install-exec → adapter (if any) → extra-bins → mux → verify-exec.
func TestDeploy_McpMux_StepsAfterExtraBins(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-2")

	req := mcpMuxReq()
	req.ProviderConfig.ExtraBinaries = []payload.ExtraBinary{
		{URL: "https://example.com/dl/shellbox-mcp", SHA256: strings.Repeat("a", 64), Bin: "shellbox-mcp"},
	}

	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatalf("mcp mux + extra-bins deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{
		"SSH:install-exec",
		"SSH:extra-bins-write", "SSH:extra-bins-exec",
		"SSH:mux-bin-write", "SSH:mux-cfg-write", "SSH:mux-selftest",
		"SSH:verify-exec",
		"SSH:launch-exec",
	})
}

// TestDeploy_McpMux_AbsentForNoMcpServers guards the byte-identical regression:
// a deploy with 0 mcp_servers entries (McpNone) must issue no mux round trips.
func TestDeploy_McpMux_AbsentForNoMcpServers(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-3")

	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatalf("deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	for _, unwanted := range []string{"SSH:mux-bin-write", "SSH:mux-cfg-write", "SSH:mux-selftest"} {
		assertNotContains(t, seq, unwanted)
	}
}

// TestDeploy_McpMux_AbsentForSingleMcpServer guards the byte-identical regression:
// a deploy with 1 mcp_servers entry (McpDirect) must issue no mux round trips.
func TestDeploy_McpMux_AbsentForSingleMcpServer(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-4")

	req := buildReq(reqOpts{})
	req.ProviderConfig.McpServers = []string{"shellbox-mcp"}

	if _, err := h.dep.Deploy(req); err != nil {
		t.Fatalf("deploy failed: %v", err)
	}

	seq := callSequence(h.events())
	for _, unwanted := range []string{"SSH:mux-bin-write", "SSH:mux-cfg-write", "SSH:mux-selftest"} {
		assertNotContains(t, seq, unwanted)
	}
}

// TestResolveMcpCommand_McpMux asserts resolveMcpCommand returns "bzmux" for
// a 2-entry mcp_servers payload (McpMux mode, Increment 2).
func TestResolveMcpCommand_McpMux(t *testing.T) {
	cfg := payload.ProviderConfig{McpServers: []string{"buzz-dev-mcp", "shellbox-mcp"}}
	got, err := resolveMcpCommand(cfg)
	if err != nil {
		t.Fatalf("McpMux: unexpected error: %v", err)
	}
	if got != payload.MuxBinaryName {
		t.Fatalf("McpMux: got %q, want %q", got, payload.MuxBinaryName)
	}
}

// TestResolveMcpCommand_McpDirect asserts resolveMcpCommand returns the single
// entry for a 1-entry mcp_servers payload (McpDirect mode).
func TestResolveMcpCommand_McpDirect(t *testing.T) {
	cfg := payload.ProviderConfig{McpServers: []string{"shellbox-mcp"}}
	got, err := resolveMcpCommand(cfg)
	if err != nil {
		t.Fatalf("McpDirect: unexpected error: %v", err)
	}
	if got != "shellbox-mcp" {
		t.Fatalf("McpDirect: got %q, want %q", got, "shellbox-mcp")
	}
}

// TestResolveMcpCommand_McpNone asserts resolveMcpCommand returns "" for a
// 0-entry mcp_servers payload (McpNone mode).
func TestResolveMcpCommand_McpNone(t *testing.T) {
	cfg := payload.ProviderConfig{}
	got, err := resolveMcpCommand(cfg)
	if err != nil {
		t.Fatalf("McpNone: unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("McpNone: got %q, want empty", got)
	}
}

// TestDeploy_McpMux_SelftestCommandIsAbsoluteAndSourcesEnv is a regression
// guard for the BLOCKER where bare "bzmux" was command-not-found in a
// non-interactive SSH shell whose PATH does not include BinDir.  It asserts
// that the mux-selftest step command (a) uses the absolute install.MuxBinPath
// rather than bare "bzmux", (b) sources the transient verifyEnvFilePath so
// BUZZ_* relay secrets reach bzmux's children, and (c) does NOT write the
// permanent nest.EnvFilePath (Critic note 1: the probe/install path must
// leave no secret env file behind on a failed deploy).  CI catches
// regressions here before a production deploy ever runs.
func TestDeploy_McpMux_SelftestCommandIsAbsoluteAndSourcesEnv(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-6")

	if _, err := h.dep.Deploy(mcpMuxReq()); err != nil {
		t.Fatalf("mcp mux deploy: %v", err)
	}

	var cmdStr string
	var found bool
	for _, ev := range h.events() {
		if ev.kind == "SSH" && ev.sshTag == "mux-selftest" {
			cmdStr = ev.args(t)
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mux-selftest SSH event not found in call log")
	}

	// Must use the absolute path — NOT bare "bzmux".
	if !strings.Contains(cmdStr, install.MuxBinPath) {
		t.Errorf("mux-selftest command must contain absolute path %q (not bare 'bzmux'):\n%s",
			install.MuxBinPath, cmdStr)
	}
	// Must source the TRANSIENT verify env file so BUZZ_* vars reach children.
	if !strings.Contains(cmdStr, verifyEnvFilePath) {
		t.Errorf("mux-selftest command must reference the transient env file %q (for BUZZ_* secrets):\n%s",
			verifyEnvFilePath, cmdStr)
	}
	// Must NOT write the permanent env file (Critic note 1): a failed
	// self-test must leave no secret env file on disk.
	if strings.Contains(cmdStr, nest.EnvFilePath) {
		t.Errorf("mux-selftest command must NOT reference the permanent env file %q "+
			"(use the transient verifyEnvFilePath instead):\n%s",
			nest.EnvFilePath, cmdStr)
	}
	// Must remove the transient file via a trap so it never lingers.
	if !strings.Contains(cmdStr, "trap") || !strings.Contains(cmdStr, "rm -f") {
		t.Errorf("mux-selftest command must trap-remove the transient env file:\n%s", cmdStr)
	}
}

// TestDeploy_McpMux_SelftestFailure asserts that a failing mux-selftest
// surfaces CodeMuxSelftest and fails the deploy.
func TestDeploy_McpMux_SelftestFailure(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-mux-5")
	t.Setenv("FAKE_MUX_SELFTEST_EXIT", "1")

	_, err := h.dep.Deploy(mcpMuxReq())
	if err == nil {
		t.Fatal("expected mux-selftest failure to fail the deploy")
	}
	if got := CodeOf(err); got != CodeMuxSelftest {
		t.Fatalf("code = %q, want %q (error: %v)", got, CodeMuxSelftest, err)
	}
}
