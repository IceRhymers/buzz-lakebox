package deployflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/state"
)

// TestTaxonomy_EveryCodeHasARemedy + TestTaxonomy_NoOrphanRemedies keep
// AllCodes and the remedies table in lockstep: a new failure mode must
// arrive with the operator instruction that makes its ok:false
// actionable (docs/PLAN.md §6 M3).
func TestTaxonomy_EveryCodeHasARemedy(t *testing.T) {
	for _, code := range AllCodes {
		remedy, ok := remedies[code]
		if !ok || strings.TrimSpace(remedy) == "" {
			t.Errorf("code %q has no remedy in the remedies table", code)
		}
	}
}

func TestTaxonomy_NoOrphanRemedies(t *testing.T) {
	declared := make(map[Code]struct{}, len(AllCodes))
	for _, code := range AllCodes {
		if _, dup := declared[code]; dup {
			t.Errorf("code %q is listed twice in AllCodes", code)
		}
		declared[code] = struct{}{}
	}
	for code := range remedies {
		if _, ok := declared[code]; !ok {
			t.Errorf("remedies has an entry for %q, which is not in AllCodes", code)
		}
	}
}

// TestTaxonomy_RenderCarriesCodeAndRemedy pins the rendered shape
// operators (and docs/RUNBOOK.md) match on.
func TestTaxonomy_RenderCarriesCodeAndRemedy(t *testing.T) {
	err := failf(CodeVerifyRelayDenied, "verify: relay connection failed")
	msg := err.Error()
	if !strings.HasPrefix(msg, "["+string(CodeVerifyRelayDenied)+"]") {
		t.Fatalf("error must lead with its code, got %q", msg)
	}
	if !strings.Contains(msg, remedies[CodeVerifyRelayDenied]) {
		t.Fatalf("error must carry its remedy, got %q", msg)
	}
	if CodeOf(err) != CodeVerifyRelayDenied {
		t.Fatalf("CodeOf = %q, want %q", CodeOf(err), CodeVerifyRelayDenied)
	}
}

// TestTaxonomy_RedactedPreservesCodeWithoutDoubleRendering: Deploy
// rebuilds its error from already-rendered, scrubbed text — the code
// must survive that round trip without the code/remedy appearing twice.
func TestTaxonomy_RedactedPreservesCodeWithoutDoubleRendering(t *testing.T) {
	original := failf(CodeInstallExec, "install: sha256 mismatch")
	rebuilt := Redacted(CodeOf(original), original.Error())

	if CodeOf(rebuilt) != CodeInstallExec {
		t.Fatalf("CodeOf(rebuilt) = %q, want %q", CodeOf(rebuilt), CodeInstallExec)
	}
	if got := strings.Count(rebuilt.Error(), "["+string(CodeInstallExec)+"]"); got != 1 {
		t.Fatalf("code should appear exactly once, got %d in %q", got, rebuilt.Error())
	}
	if got := strings.Count(rebuilt.Error(), "remedy:"); got != 1 {
		t.Fatalf("remedy should appear exactly once, got %d in %q", got, rebuilt.Error())
	}
}

func TestTaxonomy_RedactedWithoutCodeIsPlain(t *testing.T) {
	err := Redacted("", "some non-taxonomy failure")
	if err.Error() != "some non-taxonomy failure" {
		t.Fatalf("unexpected message %q", err.Error())
	}
	if CodeOf(err) != "" {
		t.Fatalf("CodeOf = %q, want empty", CodeOf(err))
	}
}

// --- failure injection: each distinct failure mode → its own code ----------

// TestDeploy_FailureModesAreDistinctlyCoded drives one injected failure
// per deploy-flow step through the fake CLI shim and asserts the code
// that comes back. This is the executable half of the M3 error taxonomy:
// the table below is the contract, and a step that starts returning
// someone else's code fails here.
func TestDeploy_FailureModesAreDistinctlyCoded(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want Code
	}{
		{
			name: "unsupported runtime",
			want: CodeValidation,
		},
		{
			name: "cli too old",
			env:  map[string]string{"FAKE_VERSION": "1.0.0"},
			want: CodeCLIVersionOld,
		},
		{
			name: "profile does not resolve",
			env:  map[string]string{"FAKE_CURRENT_USER_EXIT": "1"},
			want: CodeProfileUnresolved,
		},
		{
			name: "sandbox register fails",
			env:  map[string]string{"FAKE_REGISTER_EXIT": "1"},
			want: CodeSandboxRegister,
		},
		{
			name: "sandbox list fails",
			env:  map[string]string{"FAKE_LIST_EXIT": "1"},
			want: CodeSandboxList,
		},
		{
			name: "sandbox create fails",
			env:  map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_CREATE_EXIT": "1"},
			want: CodeSandboxCreate,
		},
		{
			name: "install exec fails",
			env:  map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_INSTALL_EXIT": "1"},
			want: CodeInstallExec,
		},
		{
			name: "runtime verification handshake fails",
			env:  map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_VERIFY_EXEC_EXIT": "1"},
			want: CodeRuntimeVerify,
		},
		{
			name: "runtime verification response missing agentInfo",
			env:  map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_VERIFY_OUTPUT": "{}"},
			want: CodeRuntimeVerify,
		},
		{
			name: "launch.sh exits non-zero",
			env:  map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_LAUNCH_EXIT": "1"},
			want: CodeLaunchExec,
		},
		{
			name: "buzz-acp not running after launch",
			env:  map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_PGREP_EXIT": "1"},
			want: CodeVerifyProcessDead,
		},
		{
			name: "relay rejects the key",
			env: map[string]string{
				"FAKE_LIST_JSON": "[]",
				"FAKE_ACP_LOG":   "buzz-acp starting: relay=wss://x\n" + terminalErrorLine + ": Auth failed\n",
			},
			want: CodeVerifyRelayDenied,
		},
		{
			name: "agent pool never reports ready",
			env: map[string]string{
				"FAKE_LIST_JSON": "[]",
				"FAKE_ACP_LOG":   "buzz-acp starting: relay=wss://x\nstill warming up\n",
			},
			want: CodeVerifyNotReady,
		},
		{
			name: "verification output unparseable",
			env:  map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_PGREP_EXIT": "not-a-number"},
			want: CodeVerifyUnparseable,
		},
		{
			name: "autostop config fails after a healthy launch",
			env:  map[string]string{"FAKE_LIST_JSON": "[]", "FAKE_CONFIG_EXIT": "1"},
			want: CodeAutostopConfig,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			setHappyPathEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			req := buildReq(reqOpts{})
			if tc.want == CodeValidation {
				req.Agent.AgentCommand = "goose"
			}

			_, err := h.dep.Deploy(req)
			if err == nil {
				t.Fatalf("expected deploy to fail for %q", tc.name)
			}
			if got := CodeOf(err); got != tc.want {
				t.Fatalf("code = %q, want %q (error: %v)", got, tc.want, err)
			}
			if !strings.Contains(err.Error(), "remedy:") {
				t.Fatalf("error must carry an operator remedy, got: %v", err)
			}
		})
	}
}

// TestDeploy_IdentityAndStateFailuresAreCoded covers the failure modes
// that are injected through the payload or the state file rather than
// the CLI shim.
func TestDeploy_IdentityAndStateFailuresAreCoded(t *testing.T) {
	t.Run("invalid nsec", func(t *testing.T) {
		h := newHarness(t)
		setHappyPathEnv(t)
		t.Setenv("FAKE_LIST_JSON", "[]")

		req := buildReq(reqOpts{nsec: "nsec1notactuallybech32"})
		_, err := h.dep.Deploy(req)
		if err == nil {
			t.Fatal("expected an invalid nsec to fail the deploy")
		}
		if got := CodeOf(err); got != CodeIdentityDerive {
			t.Fatalf("code = %q, want %q (error: %v)", got, CodeIdentityDerive, err)
		}
	})

	t.Run("ambiguous identity", func(t *testing.T) {
		h := newHarness(t)
		setHappyPathEnv(t)
		prefix := testPrefix(t)
		t.Setenv("FAKE_LIST_JSON", `[{"sandboxId":"sandbox-a","name":"`+prefix+`one","status":"Running"},{"sandboxId":"sandbox-b","name":"`+prefix+`two","status":"Running"}]`)

		_, err := h.dep.Deploy(buildReq(reqOpts{}))
		if got := CodeOf(err); got != CodeIdentityAmbiguous {
			t.Fatalf("code = %q, want %q (error: %v)", got, CodeIdentityAmbiguous, err)
		}
	})

	t.Run("unknown pinned buzz version", func(t *testing.T) {
		h := newHarness(t)
		setHappyPathEnv(t)
		t.Setenv("FAKE_LIST_JSON", "[]")

		_, err := h.dep.Deploy(buildReq(reqOpts{buzzVersion: "v9.9.9-nonexistent"}))
		if got := CodeOf(err); got != CodeInstallScript {
			t.Fatalf("code = %q, want %q (error: %v)", got, CodeInstallScript, err)
		}
	})

	t.Run("corrupt state file", func(t *testing.T) {
		h := newHarness(t)
		setHappyPathEnv(t)
		t.Setenv("FAKE_LIST_JSON", "[]")
		if err := os.WriteFile(h.dep.State.Path, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write corrupt state file: %v", err)
		}

		_, err := h.dep.Deploy(buildReq(reqOpts{}))
		if got := CodeOf(err); got != CodeStateRead {
			t.Fatalf("code = %q, want %q (error: %v)", got, CodeStateRead, err)
		}
	})

	t.Run("unwritable state file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		h := newHarness(t)
		setHappyPathEnv(t)
		t.Setenv("FAKE_LIST_JSON", "[]")

		// A read-only parent directory makes the atomic temp-file write
		// fail without needing an unwritable file itself.
		dir := t.TempDir()
		roDir := filepath.Join(dir, "readonly")
		if err := os.Mkdir(roDir, 0o500); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
		h.dep.State = &state.Store{Path: filepath.Join(roDir, "agents.json")}

		_, err := h.dep.Deploy(buildReq(reqOpts{}))
		if got := CodeOf(err); got != CodeStateWrite {
			t.Fatalf("code = %q, want %q (error: %v)", got, CodeStateWrite, err)
		}
	})
}

// TestLifecycle_FailureModesAreCoded covers the operator subcommands'
// own distinct codes.
func TestLifecycle_FailureModesAreCoded(t *testing.T) {
	t.Run("start on a never-deployed sandbox", func(t *testing.T) {
		h := newHarness(t)
		t.Setenv("FAKE_STATUS_STATUS", "Running")
		t.Setenv("FAKE_NO_LAUNCH_SH", "1")

		err := h.dep.Start("DEFAULT", "sandbox-1")
		if got := CodeOf(err); got != CodeNotDeployed {
			t.Fatalf("code = %q, want %q (error: %v)", got, CodeNotDeployed, err)
		}
	})

	t.Run("status on a missing sandbox", func(t *testing.T) {
		h := newHarness(t)
		t.Setenv("FAKE_STATUS_EXIT", "1")

		_, err := h.dep.Status("DEFAULT", "sandbox-gone")
		if got := CodeOf(err); got != CodeSandboxStatus {
			t.Fatalf("code = %q, want %q (error: %v)", got, CodeSandboxStatus, err)
		}
	})

	t.Run("stop failure", func(t *testing.T) {
		h := newHarness(t)
		t.Setenv("FAKE_STOP_EXIT", "1")

		err := h.dep.Stop("DEFAULT", "sandbox-1")
		if got := CodeOf(err); got != CodeSandboxStop {
			t.Fatalf("code = %q, want %q (error: %v)", got, CodeSandboxStop, err)
		}
	})

	t.Run("undeploy delete failure", func(t *testing.T) {
		h := newHarness(t)
		t.Setenv("FAKE_STATUS_STATUS", "Running")
		t.Setenv("FAKE_DELETE_EXIT", "1")

		_, err := h.dep.Undeploy("DEFAULT", "sandbox-1")
		if got := CodeOf(err); got != CodeSandboxDelete {
			t.Fatalf("code = %q, want %q (error: %v)", got, CodeSandboxDelete, err)
		}
	})
}

// TestRunbook_DocumentsEveryCode keeps docs/RUNBOOK.md §7 in sync with
// the taxonomy: an undocumented code is a failure an operator has to
// reverse-engineer from prose.
func TestRunbook_DocumentsEveryCode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "RUNBOOK.md"))
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	runbook := string(data)
	for _, code := range AllCodes {
		row := "| `" + string(code) + "` |"
		if !strings.Contains(runbook, row) {
			t.Errorf("docs/RUNBOOK.md has no table row for code %q", code)
		}
	}
}
