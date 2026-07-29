package payload

import (
	"strings"
	"testing"
)

func ownerPATReq(runtime string, envVars map[string]string, cfg ProviderConfig) DeployRequest {
	return DeployRequest{
		Op: "deploy",
		Agent: Agent{
			RelayURL: "wss://r", PrivateKeyNsec: "nsec1x", AuthTag: "t",
			AgentCommand: runtime,
			EnvVars:      envVars,
		},
		ProviderConfig: cfg,
	}
}

// TestValidate_OwnerPATHostRedirectRejected is the regression test for the
// finding this guard exists for, and it is worth stating precisely because
// the shape is not obvious.
//
// SandboxAuthSnippet used to gate derivation on DATABRICKS_TOKEN alone. A
// payload could therefore supply DATABRICKS_HOST *by itself*, and the
// provider would pair that payload-chosen endpoint with the sandbox's baked
// creator-identity token — a workspace-owner credential the payload never
// had to carry. Every runtime then wired the pair up faithfully, and the
// deploy-time inference probe sent the token to that endpoint itself,
// before the agent ever launched. A cooperating endpoint returning anything
// but 401/403 made the deploy report healthy.
//
// It applies to EVERY runtime — buzz-agent reads DATABRICKS_HOST/TOKEN
// directly — which is why this is not in either adapter's validator.
func TestValidate_OwnerPATHostRedirectRejected(t *testing.T) {
	for _, runtime := range []string{"buzz-agent", "claude-code", "codex-acp"} {
		for _, key := range []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN"} {
			req := ownerPATReq(runtime, map[string]string{key: "https://attacker.example"},
				ProviderConfig{InferenceAuth: "sandbox"})
			err := req.Validate()
			if err == nil {
				t.Errorf("%s: env_vars.%s must be rejected in sandbox mode — it pairs a payload-chosen endpoint with the sandbox's owner token", runtime, key)
				continue
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("%s: rejection should name %s, got: %v", runtime, key, err)
			}
		}
	}
}

// TestValidate_OwnerPATLoaderVarsRejected covers the class no
// adapter-derived list would ever have caught: variables that choose what
// CODE runs inside the process holding the credential.
//
// NODE_OPTIONS with an `--import=data:text/javascript,...` URL executes
// arbitrary attacker JavaScript inside the ACP adapter with
// DATABRICKS_TOKEN in process.env, before any adapter code runs and with
// nothing written to disk. LD_PRELOAD is the same for native children.
// Neither is an adapter setting at all, which is exactly why a denylist
// built from "what the adapter reads" cannot be the whole defense.
func TestValidate_OwnerPATLoaderVarsRejected(t *testing.T) {
	for _, key := range []string{
		"NODE_OPTIONS", "NODE_EXTRA_CA_CERTS",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES",
		"PYTHONPATH", "PYTHONSTARTUP",
		"npm_config_registry", "npm_config_cafile",
		"GIT_SSH_COMMAND",
		"HTTPS_PROXY", "https_proxy", "ALL_PROXY",
		"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
		"PATH", "BASH_ENV", "IFS",
	} {
		req := ownerPATReq("codex-acp", map[string]string{key: "x"},
			ProviderConfig{InferenceAuth: "sandbox"})
		if err := req.Validate(); err == nil {
			t.Errorf("env_vars.%s must be rejected when the sandbox holds an owner credential", key)
		}
	}
}

// TestValidate_OwnerPATKeepWorkspacePATAlsoGuarded pins that the check keys
// on the CONDITION (an owner credential is reachable) rather than on one of
// its two causes. keep_workspace_pat=true leaves the baked PAT in
// ~/.databrickscfg, where the image's own ~/.codex/config.toml symlink has
// an auth.command that reads it straight back out (probe S2) — so the same
// redirect works with inference_auth left at its default.
func TestValidate_OwnerPATKeepWorkspacePATAlsoGuarded(t *testing.T) {
	req := ownerPATReq("codex-acp", map[string]string{"NODE_OPTIONS": "--import=data:..."},
		ProviderConfig{InferenceAuth: "env", KeepWorkspacePAT: true})
	if err := req.Validate(); err == nil {
		t.Error("keep_workspace_pat=true leaves an owner credential in the sandbox and must be guarded too")
	}
}

// TestValidate_OwnerPATEnvModePermitsEverything pins the asymmetry, which
// IS the rule rather than a loophole: under inference_auth="env" the token
// in the agent's environment is the owner's own, supplied in this same
// payload. Pointing it wherever they like is their business, and a blanket
// ban would be a different and worse design — it would make the safe mode
// harder to use than the dangerous one.
func TestValidate_OwnerPATEnvModePermitsEverything(t *testing.T) {
	for _, key := range []string{"DATABRICKS_HOST", "NODE_OPTIONS", "HTTPS_PROXY", "LD_PRELOAD", "PATH"} {
		env := map[string]string{
			key:                "x",
			"DATABRICKS_HOST":  "https://mine.example",
			"DATABRICKS_TOKEN": "dapi-my-own-token",
		}
		req := ownerPATReq("codex-acp", env, ProviderConfig{InferenceAuth: "env"})
		if err := req.Validate(); err != nil {
			t.Errorf("env mode uses the owner's own credential and must permit %s: %v", key, err)
		}
	}
}

// TestValidate_OwnerPATErrorNamesNoValues keeps the rejection safe to log.
func TestValidate_OwnerPATErrorNamesNoValues(t *testing.T) {
	req := ownerPATReq("codex-acp", map[string]string{"DATABRICKS_HOST": "https://super-secret-host.example"},
		ProviderConfig{InferenceAuth: "sandbox"})
	err := req.Validate()
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), "super-secret-host") {
		t.Fatalf("error must not contain an env_vars value: %v", err)
	}
}

// TestValidate_OwnerPATOrdinaryEnvVarsStillWork keeps the guard scoped: it
// must not become a general-purpose env_vars ban.
func TestValidate_OwnerPATOrdinaryEnvVarsStillWork(t *testing.T) {
	req := ownerPATReq("codex-acp", map[string]string{
		"MY_APP_SETTING": "1",
		"TZ":             "UTC",
		"LANG":           "en_US.UTF-8",
	}, ProviderConfig{InferenceAuth: "sandbox"})
	if err := req.Validate(); err != nil {
		t.Errorf("ordinary env_vars must still be accepted in sandbox mode: %v", err)
	}
}

// TestValidate_OwnerPATRejectionIsDeterministic pins that a payload setting
// several forbidden keys always reports the same one. Map iteration order
// is randomized in Go, so without the sort this error text would vary
// between otherwise-identical deploys.
func TestValidate_OwnerPATRejectionIsDeterministic(t *testing.T) {
	env := map[string]string{"NODE_OPTIONS": "a", "DATABRICKS_HOST": "b", "PATH": "c", "LD_PRELOAD": "d"}
	first := ownerPATReq("codex-acp", env, ProviderConfig{InferenceAuth: "sandbox"}).Validate()
	if first == nil {
		t.Fatal("expected rejection")
	}
	for i := 0; i < 20; i++ {
		again := ownerPATReq("codex-acp", env, ProviderConfig{InferenceAuth: "sandbox"}).Validate()
		if again == nil || again.Error() != first.Error() {
			t.Fatalf("rejection must be deterministic:\n first: %v\n again: %v", first, again)
		}
	}
}
