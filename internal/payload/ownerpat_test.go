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

// TestValidate_KeepWorkspacePATEnvModePermitsCredentialPair is the
// regression test for a fix that over-corrected.
//
// A first pass gated BOTH lists on OwnerPATInSandbox(). That broke the only
// working inference configuration for buzz-agent under
// keep_workspace_pat: true + inference_auth: "env" — it reads
// DATABRICKS_HOST/DATABRICKS_TOKEN directly and has no bring-your-own
// endpoint alternative, so rejecting them left it undeployable, and made
// the RUNBOOK §9 PAT-opt-out acceptance step unrunnable.
//
// The distinction the two tiers encode: in "env" mode the provider renders
// no SandboxAuthSnippet and derives nothing, so the token in the agent's
// environment is the owner's own. There is no provider-supplied credential
// to misdirect, and the escalation is structurally impossible.
func TestValidate_KeepWorkspacePATEnvModePermitsCredentialPair(t *testing.T) {
	for _, runtime := range []string{"buzz-agent", "claude-code", "codex-acp"} {
		req := ownerPATReq(runtime, map[string]string{
			"DATABRICKS_HOST":  "https://mine.example",
			"DATABRICKS_TOKEN": "dapi-my-own",
		}, ProviderConfig{InferenceAuth: "env", KeepWorkspacePAT: true})
		if err := req.Validate(); err != nil {
			t.Errorf("%s: keep_workspace_pat in env mode derives nothing, so the owner's own credential pair must be permitted: %v", runtime, err)
		}
	}
}

// TestValidate_KeepWorkspacePATStillBlocksCodeSelection pins the other half:
// keep_workspace_pat leaves the baked PAT in ~/.databrickscfg, where the
// image's ~/.codex symlink reads it back out (probe S2). So variables that
// choose what CODE runs are still refused there, even though the credential
// pair is not.
func TestValidate_KeepWorkspacePATStillBlocksCodeSelection(t *testing.T) {
	for _, key := range []string{"NODE_OPTIONS", "LD_PRELOAD", "PATH", "HOME", "BUZZ_ACP_AGENT_COMMAND", "HTTPS_PROXY"} {
		req := ownerPATReq("codex-acp", map[string]string{key: "x"},
			ProviderConfig{InferenceAuth: "env", KeepWorkspacePAT: true})
		if err := req.Validate(); err == nil {
			t.Errorf("env_vars.%s must still be refused under keep_workspace_pat: the baked PAT is reachable in the sandbox", key)
		}
	}
}

// TestValidate_ProviderEmittedCommandVarsRejected covers the most direct
// lever in the whole surface, and the one an adapter-derived denylist would
// never have contained.
//
// RenderEnv emits these itself, in the fixed block, BEFORE env_vars — so an
// owner-supplied value wins on `.`-source. buzz-acp then spawns exactly what
// they name, as a child holding the derived credential.
// BUZZ_ACP_AGENT_COMMAND=sh with BUZZ_ACP_AGENT_ARGS=-c,<exfiltrate> needs no
// loader trick and nothing written to disk, and it also defeats the
// spawn-command canonicalization the ucode-wrapper defense depends on.
func TestValidate_ProviderEmittedCommandVarsRejected(t *testing.T) {
	for _, key := range []string{"BUZZ_ACP_AGENT_COMMAND", "BUZZ_ACP_AGENT_ARGS", "BUZZ_ACP_MCP_COMMAND"} {
		for _, cfg := range []ProviderConfig{
			{InferenceAuth: "sandbox"},
			{InferenceAuth: "env", KeepWorkspacePAT: true},
		} {
			req := ownerPATReq("codex-acp", map[string]string{key: "sh"}, cfg)
			if err := req.Validate(); err == nil {
				t.Errorf("env_vars.%s must be rejected (inference_auth=%q keep_pat=%v): it names the program buzz-acp spawns beside the credential",
					key, cfg.InferenceAuth, cfg.KeepWorkspacePAT)
			}
		}
	}
}

// TestValidate_DatabricksModelStillAllowed keeps the credential-pair list
// from becoming a DATABRICKS_ prefix rule. DATABRICKS_MODEL is a legitimate
// env_vars entry that RenderEnv itself emits for buzz-agent, and a prefix
// would have rejected it.
func TestValidate_DatabricksModelStillAllowed(t *testing.T) {
	req := ownerPATReq("buzz-agent", map[string]string{"DATABRICKS_MODEL": "databricks-claude-opus-4-8"},
		ProviderConfig{InferenceAuth: "sandbox"})
	if err := req.Validate(); err != nil {
		t.Errorf("DATABRICKS_MODEL selects a model, not a credential destination: %v", err)
	}
}

// TestValidate_BuzzScratchNamespaceReserved closes the class behind a
// CRITICAL that a fix introduced: a restructuring left CodexEnvSnippet's
// buzz_codex_url readable before assignment, and because env_vars render
// BEFORE the snippets and buzz_codex_url matches the env-var key charset, a
// payload could pre-export it and drive the config write past the host
// charset gate.
//
// Each snippet now initializes its own scratch variables, which is the
// actual fix. This is the belt: the provider owns the lowercase buzz_
// namespace across three snippets, and the next snippet author will not
// remember the rule.
func TestValidate_BuzzScratchNamespaceReserved(t *testing.T) {
	for _, key := range []string{"buzz_codex_url", "buzz_codex_h", "buzz_derived_url", "buzz_host", "buzz_token"} {
		req := ownerPATReq("codex-acp", map[string]string{key: "x"}, ProviderConfig{})
		err := req.Validate()
		if err == nil {
			t.Errorf("env_vars.%s must be rejected: it collides with a provider shell scratch variable", key)
			continue
		}
		if !strings.Contains(err.Error(), "collides") {
			t.Errorf("rejection for %s should explain the namespace, got: %v", key, err)
		}
	}
	// The UPPERCASE probe scratch names are reserved too, and this half was
	// missing until round 3. The inference probes assign BUZZ_PROBE_TMP /
	// _HDR / _ERR before sourcing the rendered env, so the source overwrites
	// them: reproduced, the probe wrote the owner-PAT bearer to a
	// payload-chosen path and curl -K read it from there, while the real
	// mktemp file holding nsec + auth tag + token was orphaned on disk
	// beyond the reach of both the EXIT trap and secretShredCommand.
	for _, key := range []string{"BUZZ_PROBE_TMP", "BUZZ_PROBE_HDR", "BUZZ_PROBE_ERR", "ENVF", "OUTF"} {
		if err := ownerPATReq("codex-acp", map[string]string{key: "/tmp/attacker"}, ProviderConfig{}).Validate(); err == nil {
			t.Errorf("env_vars.%s must be rejected: it collides with a provider script's scratch variable", key)
		}
	}

	// But uppercase BUZZ_ACP_* is buzz-acp's real configuration surface and
	// must stay reachable — which is exactly why this cannot be a blanket
	// BUZZ_ prefix rule.
	for _, key := range []string{"BUZZ_ACP_DEDUP", "BUZZ_ACP_IDLE_TIMEOUT", "BUZZ_ACP_SYSTEM_PROMPT"} {
		if err := ownerPATReq("codex-acp", map[string]string{key: "x"}, ProviderConfig{}).Validate(); err != nil {
			t.Errorf("uppercase %s is buzz-acp configuration, not scratch: %v", key, err)
		}
	}
}

// TestValidate_RelayURLMustParse guards the last silent-agent path the
// provider can close by itself. buzz-acp's codex_network_env does
// Url::parse on relay_url and returns None on failure, skipping the
// CODEX_CONFIG injection that grants codex's MCP subprocess outbound
// network — and that subprocess is how a codex agent answers. A
// malformed-but-non-empty value therefore deploys clean and never answers.
func TestValidate_RelayURLMustParse(t *testing.T) {
	for _, bad := range []string{"not a url", "relay.example.com", "://nohost", "wss://"} {
		req := ownerPATReq("codex-acp", nil, ProviderConfig{})
		req.Agent.RelayURL = bad
		if err := req.Validate(); err == nil {
			t.Errorf("relay_url %q must be rejected: buzz-acp silently skips the codex network injection for URLs it cannot parse", bad)
		}
	}
	for _, good := range []string{"wss://relay.example.com", "ws://localhost:3000", "https://relay.example.com/path"} {
		req := ownerPATReq("codex-acp", nil, ProviderConfig{})
		req.Agent.RelayURL = good
		if err := req.Validate(); err != nil {
			t.Errorf("relay_url %q should be accepted: %v", good, err)
		}
	}
}
