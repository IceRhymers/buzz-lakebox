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

// --- zero-token (inference_auth: "sandbox") ---------------------------
//
// Mirrors the PAT-reset matrix above: sandbox mode replaces the step-4
// PAT reset with an auth probe that exercises nest.SandboxAuthSnippet
// (docs/PLAN.md zero-token design). The fake shim's "auth-probe" tag
// (see fakeShimScript) lets tests drive the probe's success/failure and
// echoed BUZZ_PROBE_CAUSE independent of the script's real shell logic,
// exactly like every other in-sandbox step this flow ships.

func TestDeploy_SandboxAuth_SkipsPATResetRunsProbeBeforeInstall(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	if _, err := h.dep.Deploy(buildReq(reqOpts{inferenceAuth: "sandbox"})); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	seq := callSequence(h.events())
	assertNotContains(t, seq, "SSH:pat-reset")
	assertOrder(t, seq, []string{"SSH:auth-probe", "SSH:install-write"})

	var probe event
	for _, e := range h.events() {
		if e.sshTag == "auth-probe" {
			probe = e
			break
		}
	}
	if probe.sshTag == "" {
		t.Fatal("expected an auth-probe SSH event")
	}
	// TrimSpace: the shim's $(cat) capture drops trailing newlines (see
	// TestDeploy_PATResetIsTheFirstInSandboxAction's identical caveat).
	if got := probe.stdin(t); strings.TrimSpace(got) != strings.TrimSpace(nest.SandboxAuthSnippet) {
		t.Fatalf("auth-probe stdin must be nest.SandboxAuthSnippet verbatim, got:\n%s", got)
	}
	// Critic implementation note 1: the probe must never write to
	// nest.EnvFilePath — asserted directly on the shipped script text.
	if strings.Contains(probe.args(t), nest.EnvFilePath) {
		t.Fatalf("auth-probe script must never write to nest.EnvFilePath, got script:\n%s", probe.args(t))
	}

	var envWriteStdin string
	for _, e := range h.events() {
		if e.sshTag == "env-write" {
			envWriteStdin = e.stdin(t)
		}
	}
	// TrimRight: SandboxAuthSnippet is the LAST thing RenderEnv writes, and
	// the shim's $(cat) capture drops the whole file's trailing newlines,
	// same caveat as the stdin comparison above.
	if !strings.Contains(envWriteStdin, strings.TrimRight(nest.SandboxAuthSnippet, "\n")) {
		t.Fatal("env file must carry SandboxAuthSnippet in sandbox mode")
	}

	launchScript := launchScriptStdin(t, h)
	if strings.Contains(launchScript, `cat > "$HOME/.databrickscfg"`) {
		t.Fatal("sandbox mode must not re-assert the PAT stub from launch.sh — it needs the baked cfg intact")
	}
}

func TestDeploy_SandboxAuth_WithKeepWorkspacePAT_SameOrderAsSandboxAlone(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	if _, err := h.dep.Deploy(buildReq(reqOpts{inferenceAuth: "sandbox", keepWorkspacePAT: true})); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	seq := callSequence(h.events())
	assertNotContains(t, seq, "SSH:pat-reset")
	assertOrder(t, seq, []string{
		"SSH:auth-probe", "SSH:install-write", "SSH:install-exec",
		"SSH:verify-exec", "SSH:env-write", "SSH:prelaunch-kill",
		"SSH:launch-write", "SSH:launch-exec", "SSH:verify-check",
		"CLI:config",
	})
}

func TestDeploy_EnvModeExplicit_ByteStableOrderVsDefault(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	if _, err := h.dep.Deploy(buildReq(reqOpts{inferenceAuth: "env"})); err != nil {
		t.Fatalf("Deploy() error: %v", err)
	}

	seq := callSequence(h.events())
	assertNotContains(t, seq, "SSH:auth-probe")
	assertOrder(t, seq, []string{
		"SSH:pat-reset", "SSH:install-write", "SSH:install-exec",
		"SSH:verify-exec", "SSH:env-write", "SSH:prelaunch-kill",
		"SSH:launch-write", "SSH:launch-exec", "SSH:verify-check",
		"CLI:config",
	})
}

// TestDeploy_SandboxAuth_ProbeFails_FreshlyCreated_DeletesWithoutShred
// pins R2's freshly-created half: a broken baked credential on a sandbox
// created this very invocation has zero value and bills forever, so it
// is deleted — but nothing was written to it, so no shred step runs
// (unlike every OTHER teardown path, which always shreds first).
func TestDeploy_SandboxAuth_ProbeFails_FreshlyCreated_DeletesWithoutShred(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FAKE_VERSION", "1.9.0")
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-authfail-fresh")
	t.Setenv("FAKE_CREATE_STATUS", "Running")
	t.Setenv("FAKE_AUTH_PROBE_EXIT", "1")
	t.Setenv("FAKE_AUTH_PROBE_CAUSE", "credential")

	_, err := h.dep.Deploy(buildReq(reqOpts{inferenceAuth: "sandbox"}))
	if err == nil {
		t.Fatal("expected the auth probe failure to surface as an error")
	}
	if !strings.Contains(err.Error(), "provision.sandbox_auth") {
		t.Fatalf("expected code provision.sandbox_auth, got: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid, expired") {
		t.Fatalf("expected the credential-cause diagnostic, got: %v", err)
	}
	if !strings.Contains(err.Error(), "sandbox-authfail-fresh") {
		t.Fatalf("expected the sandbox id annotation, got: %v", err)
	}

	seq := callSequence(h.events())
	assertOrder(t, seq, []string{"CLI:create", "SSH:auth-probe", "CLI:delete"})
	assertNotContains(t, seq, "SSH:teardown-shred")
	assertNotContains(t, seq, "SSH:teardown-pkill")
	assertNotContains(t, seq, "SSH:install-write")
}

// TestDeploy_SandboxAuth_ProbeFails_ReusedSandbox_NoTeardown_OldAgentSurvives
// pins R2's reused-sandbox half via the real-world failed env->sandbox
// switch: a first deploy in (default) env mode creates and persists the
// sandbox mapping; a second deploy of the SAME identity switched to
// sandbox mode reuses that sandbox and fails its auth probe (stub cause —
// the first deploy's own PAT reset already clobbered the baked cfg). The
// probe mutated nothing, so deploy() must run NO teardown at all: the
// first deploy's env-mode agent and env file are never touched.
func TestDeploy_SandboxAuth_ProbeFails_ReusedSandbox_NoTeardown_OldAgentSurvives(t *testing.T) {
	h := newHarness(t)
	setHappyPathEnv(t)
	t.Setenv("FAKE_LIST_JSON", "[]")
	t.Setenv("FAKE_CREATE_ID", "sandbox-switch-1")
	t.Setenv("FAKE_CREATE_STATUS", "Running")

	if _, err := h.dep.Deploy(buildReq(reqOpts{})); err != nil {
		t.Fatalf("first (env-mode) Deploy() error: %v", err)
	}
	before := len(h.events())

	t.Setenv("FAKE_AUTH_PROBE_EXIT", "1")
	t.Setenv("FAKE_AUTH_PROBE_CAUSE", "stub")
	_, err := h.dep.Deploy(buildReq(reqOpts{inferenceAuth: "sandbox"}))
	if err == nil {
		t.Fatal("expected the second deploy's auth probe to fail")
	}
	if !strings.Contains(err.Error(), "provision.sandbox_auth") {
		t.Fatalf("expected code provision.sandbox_auth, got: %v", err)
	}
	if !strings.Contains(err.Error(), "sandbox-switch-1") {
		t.Fatalf("expected the reused sandbox id in the error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unrestorable") {
		t.Fatalf("expected the stub-cause diagnostic, got: %v", err)
	}

	secondSeq := callSequence(h.events()[before:])
	assertNotContains(t, secondSeq, "SSH:teardown-shred")
	assertNotContains(t, secondSeq, "SSH:teardown-pkill")
	assertNotContains(t, secondSeq, "CLI:delete")
	if got := secondSeq[len(secondSeq)-1]; got != "SSH:auth-probe" {
		t.Fatalf("expected the auth probe to be the LAST call of the second deploy (no cleanup after a pre-mutation failure), got sequence %v", secondSeq)
	}
}

// TestDeploy_SandboxAuth_CausesAreDistinguishable drives the probe to
// each of its three causes and asserts the error text disambiguates them
// (docs/PLAN.md zero-token design, Critic note 2's hedged cause-(c)
// wording).
func TestDeploy_SandboxAuth_CausesAreDistinguishable(t *testing.T) {
	cases := []struct {
		name  string
		cause string
		want  string
	}{
		{"stub marker present", "stub", "unrestorable"},
		{"cfg missing or unparseable", "parse", "unparseable"},
		{"derived credential rejected", "credential", "invalid, expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			t.Setenv("FAKE_VERSION", "1.9.0")
			t.Setenv("FAKE_LIST_JSON", "[]")
			t.Setenv("FAKE_CREATE_STATUS", "Running")
			t.Setenv("FAKE_AUTH_PROBE_EXIT", "1")
			t.Setenv("FAKE_AUTH_PROBE_CAUSE", tc.cause)

			_, err := h.dep.Deploy(buildReq(reqOpts{inferenceAuth: "sandbox"}))
			if err == nil {
				t.Fatal("expected the deploy to fail")
			}
			if !strings.Contains(err.Error(), "provision.sandbox_auth") {
				t.Fatalf("expected code provision.sandbox_auth, got: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error to contain %q for cause %q, got: %v", tc.want, tc.cause, err)
			}
		})
	}
}

// The env_vars-precedence diagnostic that used to be pinned here is gone,
// and so is the test: payload.validateOwnerPATEnvVars now rejects
// env_vars.DATABRICKS_HOST/DATABRICKS_TOKEN under inference_auth="sandbox"
// before any provisioning happens, because supplying one of the pair let a
// payload couple its own endpoint to the sandbox's owner-level token. The
// state that note described can no longer be reached. Coverage for the
// rejection itself lives in internal/payload/ownerpat_test.go.
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
