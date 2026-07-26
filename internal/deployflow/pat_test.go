package deployflow

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/nest"
)

// The PAT opt-out matrix (docs/PLAN.md §4.4 step 4, §5, Decision 2;
// M3 accept: "deploy with keep_workspace_pat:true → in-sandbox
// current-user me succeeds as creator").
//
// The live half of that acceptance is in docs/RUNBOOK.md — only a real
// sandbox can prove `databricks current-user me` still authenticates.
// What is provable in CI is everything that decides the outcome: with
// the default, the stub is written as the FIRST in-sandbox action and
// re-asserted on every relaunch; with the opt-out, neither happens on
// either path.

func TestDeploy_PATResetIsTheFirstInSandboxAction(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	// The ordering claim that matters: nothing runs inside the sandbox
	// before the baked creator PAT is neutralized — in particular not
	// the network-fetching install script.
	var firstSSH event
	for _, e := range h.events() {
		if e.kind == "SSH" {
			firstSSH = e
			break
		}
	}
	if firstSSH.sshTag != "pat-reset" {
		t.Fatalf("first in-sandbox command must be the PAT reset, got %q", firstSSH.sshTag)
	}
	// TrimSpace: the shim's `$(cat)` capture drops the trailing newline.
	if got := firstSSH.stdin(t); strings.TrimSpace(got) != strings.TrimSpace(nest.PATStub) {
		t.Fatalf("PAT reset must write the comment-only stub, got:\n%s", got)
	}
}

func TestDeploy_KeepWorkspacePAT_NeitherResetsNorReAsserts(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	if _, err := h.dep.Deploy(buildReq(reqOpts{keepWorkspacePAT: true})); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	launchScript := launchScriptStdin(t, h)
	if strings.Contains(launchScript, `cat > "$HOME/.databrickscfg"`) {
		t.Fatal("keep_workspace_pat=true must not re-assert the stub from launch.sh — it runs on every start, so it would clobber the kept PAT on the first relaunch")
	}
	assertNotContains(t, callSequence(h.events()), "SSH:pat-reset")
}

func TestDeploy_DefaultReAssertsStubOnEveryRelaunch(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	// launch.sh is the single relaunch entrypoint (deploy, `start`, and
	// any future supervisor), so the stub living there is what makes the
	// unprobed "does sandbox start restore the baked PAT?" question moot.
	launchScript := launchScriptStdin(t, h)
	if !strings.Contains(launchScript, `cat > "$HOME/.databrickscfg"`) {
		t.Fatal("launch.sh must re-assert the PAT stub by default")
	}
	if !strings.Contains(launchScript, nest.PATStub) {
		t.Fatal("launch.sh's re-asserted content must be the same stub the deploy wrote")
	}
}

// launchScriptStdin returns the launch.sh content the deploy shipped.
func launchScriptStdin(t *testing.T, h *harness) string {
	t.Helper()
	for _, e := range h.events() {
		if e.sshTag == "launch-write" {
			return e.stdin(t)
		}
	}
	t.Fatal("deploy never wrote launch.sh")
	return ""
}
