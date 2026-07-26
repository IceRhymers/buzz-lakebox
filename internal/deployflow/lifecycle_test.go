package deployflow

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/state"
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

// TestUndeploy_ShredsBeforeDeletingAndForgetsMapping pins the ordering
// undeploy's safety rests on: the secret shred must happen while the
// sandbox is still reachable, the delete must follow it, and the reuse
// mapping must be gone afterwards so the next deploy does not probe a
// sandbox that no longer exists.
func TestUndeploy_ShredsBeforeDeletingAndForgetsMapping(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Running")

	key := state.Key("DEFAULT", "npub1example")
	if err := h.dep.State.Record(key, state.Entry{SandboxID: "sandbox-1", Profile: "DEFAULT"}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	res, err := h.dep.Undeploy("DEFAULT", "sandbox-1")
	if err != nil {
		t.Fatalf("Undeploy() error: %v", err)
	}
	if !res.Shredded {
		t.Fatal("Shredded must be true when the sandbox was Running")
	}
	if res.StateEntriesRemoved != 1 {
		t.Fatalf("StateEntriesRemoved = %d, want 1", res.StateEntriesRemoved)
	}

	assertOrder(t, callSequence(h.events()), []string{"CLI:status", "SSH:undeploy-shred", "CLI:delete"})

	if _, ok, err := h.dep.State.Lookup(key); err != nil || ok {
		t.Fatalf("mapping should be gone after undeploy (ok=%v, err=%v)", ok, err)
	}
}

// TestUndeploy_ShredCoversEverySecretBearingFile: the shred and the
// deploy-failure teardown must remove the same set of files — a secret
// that only one of them knows about is a stranded nsec.
func TestUndeploy_ShredCoversEverySecretBearingFile(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Running")

	if _, err := h.dep.Undeploy("DEFAULT", "sandbox-1"); err != nil {
		t.Fatalf("Undeploy() error: %v", err)
	}

	var shredArgs string
	for _, e := range h.events() {
		if e.sshTag == "undeploy-shred" {
			shredArgs = e.args(t)
		}
	}
	for _, want := range []string{nest.EnvFilePath, verifyEnvFilePath} {
		if !strings.Contains(shredArgs, want) {
			t.Fatalf("shred command must cover %q, got: %s", want, shredArgs)
		}
	}
}

// TestUndeploy_StoppedSandbox_SkipsShredButStillDeletes: a stopped
// sandbox cannot be SSH'd into. Deleting it anyway is the point — its
// storage (and the env file in it) dies with it, and leaving it behind
// would keep billing.
func TestUndeploy_StoppedSandbox_SkipsShredButStillDeletes(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Stopped")

	res, err := h.dep.Undeploy("DEFAULT", "sandbox-1")
	if err != nil {
		t.Fatalf("Undeploy() error: %v", err)
	}
	if res.Shredded {
		t.Fatal("Shredded must be false for a stopped sandbox")
	}
	seq := callSequence(h.events())
	assertNotContains(t, seq, "SSH:undeploy-shred")
	assertOrder(t, seq, []string{"CLI:delete"})
}

// TestUndeploy_ShredFailureStillDeletes: an unreachable sandbox must
// not strand a running, billing sandbox — the delete is what actually
// protects the owner, so a failed shred cannot abort it.
func TestUndeploy_ShredFailureStillDeletes(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Running")
	t.Setenv("FAKE_SHRED_EXIT", "1")

	res, err := h.dep.Undeploy("DEFAULT", "sandbox-1")
	if err != nil {
		t.Fatalf("Undeploy() error: %v", err)
	}
	if res.Shredded {
		t.Fatal("Shredded must be false when the shred command failed")
	}
	assertOrder(t, callSequence(h.events()), []string{"SSH:undeploy-shred", "CLI:delete"})
}

// TestUndeploy_KeepsMappingWhenDeleteFails: the sandbox still exists, so
// the next deploy should reuse it rather than orphan it.
func TestUndeploy_KeepsMappingWhenDeleteFails(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Running")
	t.Setenv("FAKE_DELETE_EXIT", "1")

	key := state.Key("DEFAULT", "npub1example")
	if err := h.dep.State.Record(key, state.Entry{SandboxID: "sandbox-1", Profile: "DEFAULT"}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	if _, err := h.dep.Undeploy("DEFAULT", "sandbox-1"); err == nil {
		t.Fatal("expected a delete failure to surface")
	}
	if _, ok, err := h.dep.State.Lookup(key); err != nil || !ok {
		t.Fatalf("mapping must survive a failed delete (ok=%v, err=%v)", ok, err)
	}
}
