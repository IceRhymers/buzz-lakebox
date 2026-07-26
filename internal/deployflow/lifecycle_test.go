package deployflow

import (
	"strings"
	"testing"
)

// TestStart_StoppedSandbox_RecoversAndVerifies: the autostop/lifetime-cap
// recovery path — start the sandbox, wait Running, rerun launch.sh,
// verify the same way deploy step 10 does.
func TestStart_StoppedSandbox_RecoversAndVerifies(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_ACP_LOG", healthyLog)
	t.Setenv("FAKE_STATUS_STATUS", "Stopped")
	t.Setenv("FAKE_STATUS_FLIP_FILE", h.logPath+".flip")

	if err := h.dep.Start("DEFAULT", "sandbox-1"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{"CLI:status", "CLI:start", "CLI:status", "SSH:start-launch", "SSH:verify-check"})
}

// TestStart_RunningSandbox_SkipsSandboxStart: a merely-dead agent in a
// running sandbox needs only launch.sh (flock/pgrep guarded) + verify.
func TestStart_RunningSandbox_SkipsSandboxStart(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_ACP_LOG", healthyLog)
	t.Setenv("FAKE_STATUS_STATUS", "Running")

	if err := h.dep.Start("DEFAULT", "sandbox-1"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	seq := callSequence(h.events())
	assertNotContains(t, seq, "CLI:start")
	assertOrder(t, seq, []string{"CLI:status", "SSH:start-launch", "SSH:verify-check"})
}

// TestStart_NeverDeployedSandbox_PointsAtDeploy: launch.sh missing must
// produce the actionable "run a deploy first" error, not a raw shell
// failure.
func TestStart_NeverDeployedSandbox_PointsAtDeploy(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Running")
	t.Setenv("FAKE_NO_LAUNCH_SH", "1")

	err := h.dep.Start("DEFAULT", "sandbox-1")
	if err == nil {
		t.Fatal("Start() must fail when launch.sh is absent")
	}
	if !strings.Contains(err.Error(), "run a deploy first") {
		t.Fatalf("error should point at deploy, got: %v", err)
	}
}

// TestStatus_RunningSandbox_ReportsAcpLiveness covers both liveness
// outcomes via the pgrep exit code.
func TestStatus_RunningSandbox_ReportsAcpLiveness(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Running")
	t.Setenv("FAKE_ACP_LOG", healthyLog)

	t.Setenv("FAKE_PGREP_EXIT", "0")
	st, err := h.dep.Status("DEFAULT", "sandbox-1")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if !st.AcpRunning || st.SandboxStatus != "Running" || !strings.Contains(st.LogTail, "agent_pool_ready") {
		t.Fatalf("unexpected status: %+v", st)
	}

	t.Setenv("FAKE_PGREP_EXIT", "1")
	st, err = h.dep.Status("DEFAULT", "sandbox-1")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if st.AcpRunning {
		t.Fatalf("AcpRunning must be false when pgrep misses: %+v", st)
	}
}

// TestStatus_StoppedSandbox_SkipsSSH: a stopped sandbox cannot be SSH'd
// into — status must report it without attempting the in-sandbox check.
func TestStatus_StoppedSandbox_SkipsSSH(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Stopped")

	st, err := h.dep.Status("DEFAULT", "sandbox-1")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if st.AcpRunning || st.SandboxStatus != "Stopped" {
		t.Fatalf("unexpected status for stopped sandbox: %+v", st)
	}
	seq := callSequence(h.events())
	assertNotContains(t, seq, "SSH:status-check")
}

func TestLogs_ReturnsTail(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_ACP_LOG", healthyLog)

	out, err := h.dep.Logs("DEFAULT", "sandbox-1", 0)
	if err != nil {
		t.Fatalf("Logs() error: %v", err)
	}
	if !strings.Contains(out, "agent_pool_ready") {
		t.Fatalf("Logs() = %q, want acp.log content", out)
	}
}

func TestStop_StopsSandbox(t *testing.T) {
	h := newHarness(t)
	if err := h.dep.Stop("DEFAULT", "sandbox-1"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	assertOrder(t, callSequence(h.events()), []string{"CLI:stop"})
}
