package payload

import (
	"strings"
	"testing"
)

func TestRuntimeFor_ClaudeAliases(t *testing.T) {
	for _, alias := range []string{"claude-agent-acp", "claude-code-acp", "claude-code", "claudecode"} {
		rt, ok := RuntimeFor(alias)
		if !ok {
			t.Errorf("alias %q should be accepted", alias)
			continue
		}
		if rt != RuntimeClaude {
			t.Errorf("alias %q resolved to %q, want %q", alias, rt, RuntimeClaude)
		}
		if got := rt.SpawnCommand(); got != "claude-agent-acp" {
			t.Errorf("alias %q spawn command = %q, want claude-agent-acp", alias, got)
		}
	}
}

// TestRuntimeFor_BareClaudeRejected pins a deliberate exclusion: "claude"
// names the underlying CLI binary, not the ACP adapter, and appears in
// neither upstream alias list. Accepting it would spawn a program that does
// not speak ACP over stdio — an agent that starts and then never answers.
func TestRuntimeFor_BareClaudeRejected(t *testing.T) {
	if _, ok := RuntimeFor("claude"); ok {
		t.Fatal(`bare "claude" must not be accepted as an agent_command`)
	}
}

// TestRuntimeCodex_SpawnCommandNeverBareCodex pins the canonicalization the
// codex runtime's safety rests on. The sandbox image ships
// /usr/local/bin/codex — a Databricks `ucode` wrapper that takes no
// arguments and launches an interactive TUI (probe S1) — and the adapter's
// own node_modules/.bin ships a second, real `codex`. Neither speaks ACP on
// stdio the way buzz-acp needs. Only "codex-acp" may ever be spawned.
func TestRuntimeCodex_SpawnCommandNeverBareCodex(t *testing.T) {
	if got := RuntimeCodex.SpawnCommand(); got != "codex-acp" {
		t.Errorf("RuntimeCodex spawn command = %q, want codex-acp", got)
	}
}

func TestRuntimeFor_ExactCaseOnly(t *testing.T) {
	for _, v := range []string{"Claude-Code", "CLAUDE-CODE", "Buzz-Agent", " claude-code"} {
		if _, ok := RuntimeFor(v); ok {
			t.Errorf("%q should not be accepted; matching is exact-case by design", v)
		}
	}
}

func claudeReq(envVars map[string]string, inferenceAuth string) DeployRequest {
	return DeployRequest{
		Op: "deploy",
		Agent: Agent{
			RelayURL:       "wss://r",
			PrivateKeyNsec: "nsec1x",
			AuthTag:        "t",
			AgentCommand:   "claude-code",
			EnvVars:        envVars,
		},
		ProviderConfig: ProviderConfig{InferenceAuth: inferenceAuth},
	}
}

// TestValidate_ClaudeRequiresInferenceSource is the payload half of the
// credential-egress defense. Without an endpoint, Claude Code falls back to
// the public Anthropic API and sends its bearer token there — so a deploy
// that names no endpoint must be rejected before anything is provisioned.
func TestValidate_ClaudeRequiresInferenceSource(t *testing.T) {
	cases := []struct {
		name     string
		req      DeployRequest
		wantErr  bool
		errMatch string
	}{
		{
			name:     "token but no host is rejected",
			req:      claudeReq(map[string]string{"DATABRICKS_TOKEN": "dapi1"}, ""),
			wantErr:  true,
			errMatch: "inference endpoint",
		},
		{
			name:    "no env at all is rejected",
			req:     claudeReq(nil, ""),
			wantErr: true,
		},
		{
			name: "DATABRICKS_HOST is sufficient",
			req:  claudeReq(map[string]string{"DATABRICKS_HOST": "https://h", "DATABRICKS_TOKEN": "dapi1"}, ""),
		},
		{
			name:     "ANTHROPIC_BASE_URL without a token is rejected",
			req:      claudeReq(map[string]string{"ANTHROPIC_BASE_URL": "https://byo"}, ""),
			wantErr:  true,
			errMatch: "without env_vars.ANTHROPIC_AUTH_TOKEN",
		},
		{
			name: "ANTHROPIC_BASE_URL with its own token is sufficient",
			req:  claudeReq(map[string]string{"ANTHROPIC_BASE_URL": "https://byo", "ANTHROPIC_AUTH_TOKEN": "byo-token"}, ""),
		},
		{
			// Bring-your-own endpoint suppresses derivation even in
			// sandbox mode, so the paired-token rule applies there too.
			name:    "ANTHROPIC_BASE_URL without a token is rejected in sandbox mode too",
			req:     claudeReq(map[string]string{"ANTHROPIC_BASE_URL": "https://byo"}, "sandbox"),
			wantErr: true,
		},
		{
			name: "sandbox mode derives its own host",
			req:  claudeReq(nil, "sandbox"),
		},
		{
			name:    "empty-valued host does not count",
			req:     claudeReq(map[string]string{"DATABRICKS_HOST": ""}, ""),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected rejection")
				}
				if tc.errMatch != "" && !strings.Contains(err.Error(), tc.errMatch) {
					t.Fatalf("error should mention %q, got: %v", tc.errMatch, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}

// TestValidate_BuzzAgentUnaffectedByClaudeGuard keeps the new guard scoped:
// buzz-agent has always been allowed to deploy without DATABRICKS_HOST (it
// can be configured by other means), and that must not change.
func TestValidate_BuzzAgentUnaffectedByClaudeGuard(t *testing.T) {
	req := claudeReq(nil, "")
	req.Agent.AgentCommand = "buzz-agent"
	if err := req.Validate(); err != nil {
		t.Fatalf("buzz-agent must not be subject to the claude inference guard: %v", err)
	}
}

// TestValidate_ClaudeErrorNamesNoValues keeps the rejection safe to log:
// it may name keys, never their values.
func TestValidate_ClaudeErrorNamesNoValues(t *testing.T) {
	req := claudeReq(map[string]string{"DATABRICKS_TOKEN": "dapi-super-secret-value"}, "")
	err := req.Validate()
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), "dapi-super-secret-value") {
		t.Fatalf("error text must not contain an env_vars value: %v", err)
	}
}
