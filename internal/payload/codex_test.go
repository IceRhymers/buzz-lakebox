package payload

import (
	"strings"
	"testing"
)

func TestRuntimeFor_CodexAliases(t *testing.T) {
	for _, alias := range []string{"codex", "codex-acp", "codex-cli"} {
		rt, ok := RuntimeFor(alias)
		if !ok {
			t.Errorf("alias %q should be accepted", alias)
			continue
		}
		if rt != RuntimeCodex {
			t.Errorf("alias %q resolved to %q, want %q", alias, rt, RuntimeCodex)
		}
		// The canonicalization is the safety property: whatever the payload
		// said, only "codex-acp" reaches the sandbox.
		if got := rt.SpawnCommand(); got != "codex-acp" {
			t.Errorf("alias %q spawn command = %q, want codex-acp", alias, got)
		}
	}
}

func codexReq(envVars map[string]string, inferenceAuth string) DeployRequest {
	return DeployRequest{
		Op: "deploy",
		Agent: Agent{
			RelayURL:       "wss://r",
			PrivateKeyNsec: "nsec1x",
			AuthTag:        "t",
			AgentCommand:   "codex-acp",
			EnvVars:        envVars,
		},
		ProviderConfig: ProviderConfig{InferenceAuth: inferenceAuth},
	}
}

// TestValidate_CodexRejectsAdapterEnvVarsInSandboxMode is the payload half of
// the codex credential-egress defense, and it must cover the adapter's
// COMPLETE env surface rather than the obvious one or two.
//
// nest.CodexEnvSnippet's gate operates on a FILE, and agent.env_vars render
// BEFORE that snippet and are exported into the agent's process — so no
// file-based gate can see any of these. Under inference_auth="sandbox" the
// credential in play is the sandbox's baked workspace-OWNER PAT, which the
// payload never supplied, so forwarding it to an owner-chosen endpoint is a
// privilege escalation this provider would be performing on their behalf.
//
// The deploy would also report healthy: the inference probe reads whichever
// config the owner pointed CODEX_HOME at, so it would validate the
// substituted endpoint rather than catch it.
func TestValidate_CodexRejectsAdapterEnvVarsInSandboxMode(t *testing.T) {
	// Every variable @agentclientprotocol/codex-acp@1.1.7 reads (probe S10).
	// Listed literally rather than ranged over codexAdapterEnvVars, so that
	// dropping one from the production list fails this test instead of
	// silently shrinking its own coverage.
	for _, key := range []string{
		"DEFAULT_AUTH_REQUEST",
		"CODEX_HOME",
		"CODEX_CONFIG",
		"MODEL_PROVIDER",
		"CODEX_PATH",
	} {
		t.Run(key+"/sandbox rejected", func(t *testing.T) {
			err := codexReq(map[string]string{key: "/tmp/mine"}, "sandbox").Validate()
			if err == nil {
				t.Fatalf("env_vars.%s must be rejected in sandbox mode: it redirects where codex sends the workspace-owner credential", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("rejection should name the offending key, got: %v", err)
			}
		})

		// In env mode the token is the owner's own and pointing it wherever
		// they like is their business. That asymmetry is the whole rule —
		// a blanket ban would be a different, worse design.
		t.Run(key+"/env permitted", func(t *testing.T) {
			req := codexReq(map[string]string{
				key:                "/tmp/mine",
				"DATABRICKS_HOST":  "https://h",
				"DATABRICKS_TOKEN": "dapi-owner-own",
			}, "env")
			if err := req.Validate(); err != nil {
				t.Fatalf("env mode uses the owner's own credential and must permit %s: %v", key, err)
			}
		})
	}
}

// TestValidate_CodexAdapterEnvVarsListIsComplete pins the production list
// against the adapter's actual env surface. If a future adapter version
// reads a new variable, this is where the omission should be noticed.
func TestValidate_CodexAdapterEnvVarsListIsComplete(t *testing.T) {
	want := map[string]bool{
		"DEFAULT_AUTH_REQUEST": true,
		"CODEX_HOME":           true,
		"CODEX_CONFIG":         true,
		"MODEL_PROVIDER":       true,
		"CODEX_PATH":           true,
	}
	if len(codexAdapterEnvVars) != len(want) {
		t.Errorf("codexAdapterEnvVars has %d entries, want %d: %v", len(codexAdapterEnvVars), len(want), codexAdapterEnvVars)
	}
	for _, k := range codexAdapterEnvVars {
		if !want[k] {
			t.Errorf("unexpected entry %q — if the adapter genuinely reads it, update this test too", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("codexAdapterEnvVars is missing %q; a payload could use it to redirect the owner credential", k)
	}
}

// TestValidate_CodexSandboxModeOtherwiseAccepted keeps the guard scoped: an
// ordinary sandbox-mode codex deploy needs nothing extra, because the host
// and token are both derived in-sandbox.
func TestValidate_CodexSandboxModeOtherwiseAccepted(t *testing.T) {
	if err := codexReq(nil, "sandbox").Validate(); err != nil {
		t.Fatalf("a plain sandbox-mode codex deploy must be accepted: %v", err)
	}
	if err := codexReq(map[string]string{"HTTP_PROXY": "http://p"}, "sandbox").Validate(); err != nil {
		t.Fatalf("unrelated env_vars must not trip the codex guard: %v", err)
	}
}

// TestValidate_CodexGuardDoesNotAffectOtherRuntimes keeps the rejection from
// leaking onto runtimes that do not read these variables at all.
func TestValidate_CodexGuardDoesNotAffectOtherRuntimes(t *testing.T) {
	for _, cmd := range []string{"buzz-agent", "claude-code"} {
		req := codexReq(map[string]string{"CODEX_HOME": "/tmp/mine"}, "sandbox")
		req.Agent.AgentCommand = cmd
		if err := req.Validate(); err != nil {
			t.Errorf("%s must not be subject to the codex adapter-env guard: %v", cmd, err)
		}
	}
}

// TestValidate_CodexErrorNamesNoValues keeps the rejection safe to log: it
// may name keys, never their values.
func TestValidate_CodexErrorNamesNoValues(t *testing.T) {
	err := codexReq(map[string]string{"CODEX_HOME": "/tmp/super-secret-path"}, "sandbox").Validate()
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), "super-secret-path") {
		t.Fatalf("error text must not contain an env_vars value: %v", err)
	}
}

// TestUnsupportedRuntimeError_ListsCodex checks the message an operator sees
// after a typo now that codex is routable.
func TestUnsupportedRuntimeError_ListsCodex(t *testing.T) {
	_, err := RuntimeFor("codexx")
	if err {
		t.Fatal("codexx must not resolve")
	}
	msg := unsupportedRuntimeError("codexx").Error()
	for _, want := range []string{"codexx", "codex-acp", "claude-agent-acp", "buzz-agent"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %s", want, msg)
		}
	}
	// The pointer to the tracking issue is stale now that codex ships.
	if strings.Contains(msg, "issues/3") {
		t.Error("error should no longer point at issue #3 for codex support")
	}
}
