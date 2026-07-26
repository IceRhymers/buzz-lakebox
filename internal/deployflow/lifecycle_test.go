package deployflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/redact"
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

// TestUndeploy_DeleteSucceedsButForgetSandboxFails_ReportsResidueNotError
// pins the HIGH-severity fix: a successful, irreversible delete must not
// be reported as a failed undeploy just because the best-effort mapping
// cleanup that follows it could not write. The operator must still see
// the deletion succeeded (main.go prints the success line unconditionally);
// the residue is surfaced via UndeployResult, not via a non-nil error.
func TestUndeploy_DeleteSucceedsButForgetSandboxFails_ReportsResidueNotError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Running")

	// State lives in its OWN temp dir, separate from newHarness's dir
	// (which also holds the fake databricks binary and the call log):
	// chmod'ing that shared dir read-only would silently break the
	// shim's own `>> "$LOG"` appends too, and this test needs the call
	// log intact to assert CLI:delete happened.
	stateDir := t.TempDir()
	h.dep.State = &state.Store{Path: filepath.Join(stateDir, "agents.json")}

	key := state.Key("DEFAULT", "npub1example")
	if err := h.dep.State.Record(key, state.Entry{SandboxID: "sandbox-1", Profile: "DEFAULT"}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	// Make the state directory unwritable so ForgetSandbox's save (a
	// temp-file + rename, same discipline as Record) fails, without
	// touching the sandbox delete path at all.
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	res, err := h.dep.Undeploy("DEFAULT", "sandbox-1")
	if err != nil {
		t.Fatalf("Undeploy() must return a nil error when only the post-delete state cleanup fails, got: %v", err)
	}
	if res.StateResidue == "" {
		t.Fatal("StateResidue must report the cleanup failure")
	}
	if res.StateEntriesRemoved != 0 {
		t.Fatalf("StateEntriesRemoved = %d, want 0 (the write never completed)", res.StateEntriesRemoved)
	}
	assertOrder(t, callSequence(h.events()), []string{"CLI:delete"})
}

// TestStatus_UnparseableOutput_UsesStatusUnparseableCode pins the
// MEDIUM-severity fix: Status's own parse failure must NOT reuse
// CodeVerifyUnparseable, whose remedy ("run status and logs") is
// circular when the failure came from status itself.
func TestStatus_UnparseableOutput_UsesStatusUnparseableCode(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_STATUS_STATUS", "Running")
	t.Setenv("FAKE_PGREP_EXIT", "not-a-number")

	_, err := h.dep.Status("DEFAULT", "sandbox-1")
	if err == nil {
		t.Fatal("Status() must fail when the probe output is unparseable")
	}
	if got := CodeOf(err); got != CodeStatusUnparseable {
		t.Fatalf("code = %q, want %q", got, CodeStatusUnparseable)
	}
	if strings.Contains(err.Error(), "run `status") {
		t.Fatalf("remedy must not point back at status when the failure came from status, got: %v", err)
	}
}

// TestLifecycleErr_ScrubsCredentialShapedTailAndPreservesCode pins the
// MEDIUM-severity redaction-boundary fix directly against lifecycleErr,
// the single point every lifecycle op (Start/Status/Logs/Stop/Undeploy)
// routes its error through. CodeLaunchExec's real-world shape is
// exactly this: sshx.Client.run wraps the sandbox's full, unbounded,
// unscrubbed combined stdout+stderr into the error via "(output: ...)"
// (internal/sshx/sshx.go), and that text is never passed through
// remoteText before this boundary.
func TestLifecycleErr_ScrubsCredentialShapedTailAndPreservesCode(t *testing.T) {
	raw := failf(CodeLaunchExec, "run launch.sh: %w",
		fmt.Errorf("ssh sandbox-1 -p DEFAULT: exit status 1 (output: DATABRICKS_TOKEN='dapi1234567890abcdef' still starting)"))

	got := lifecycleErr(raw)
	if got == nil {
		t.Fatal("lifecycleErr(non-nil) must not return nil")
	}
	if code := CodeOf(got); code != CodeLaunchExec {
		t.Fatalf("code = %q, want %q (error: %v)", code, CodeLaunchExec, got)
	}
	if strings.Contains(got.Error(), "dapi1234567890abcdef") {
		t.Fatalf("lifecycleErr must scrub credential-shaped output, got: %v", got)
	}
	if !strings.Contains(got.Error(), redact.Placeholder) {
		t.Fatalf("expected the redaction placeholder in the scrubbed error, got: %v", got)
	}
}

// TestLifecycleErr_Nil pins the nil short-circuit: a healthy lifecycle
// op (which returns a nil error) must not have that nil turned into a
// non-nil *Failure by the deferred redaction wrapper.
func TestLifecycleErr_Nil(t *testing.T) {
	if err := lifecycleErr(nil); err != nil {
		t.Fatalf("lifecycleErr(nil) = %v, want nil", err)
	}
}
