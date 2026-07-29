// Package payload defines the deploy-request wire shapes documented in
// docs/CONTRACT.md §3 and validates them. Unknown JSON fields are always
// tolerated (buzz's payload may grow fields we don't yet know about).
package payload

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// envVarKeyPattern is a valid POSIX shell/environment variable name. Keys
// that don't match this are rejected by Agent.Validate() because
// internal/nest's RenderEnv writes the KEY of every env_vars entry raw
// (unquoted) into a file that gets `.`-sourced by a shell — an attacker-
// controlled key containing shell metacharacters or a newline would
// achieve command execution in that shell, which has just exported the
// agent's nsec, auth tag, and DATABRICKS_TOKEN.
var envVarKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// DeployRequest is the top-level envelope for an {"op":"deploy",...} request
// (docs/CONTRACT.md §3).
type DeployRequest struct {
	Op             string         `json:"op"`
	RequestID      string         `json:"request_id"`
	Agent          Agent          `json:"agent"`
	ProviderConfig ProviderConfig `json:"provider_config"`
}

// Agent is the exhaustive agent payload (docs/CONTRACT.md §3, "agent
// fields"). Field set and names are frozen against buzz's
// deploy_payload_json(); do not add/rename without re-verifying upstream.
type Agent struct {
	Name                string            `json:"name"`
	RelayURL            string            `json:"relay_url"`
	PrivateKeyNsec      string            `json:"private_key_nsec"`
	AuthTag             string            `json:"auth_tag"`
	AgentCommand        string            `json:"agent_command"`
	AgentArgs           []string          `json:"agent_args"`
	SystemPrompt        string            `json:"system_prompt"`
	Model               *string           `json:"model"`
	Provider            *string           `json:"provider"`
	TurnTimeoutSeconds  int               `json:"turn_timeout_seconds"`
	IdleTimeoutSeconds  int               `json:"idle_timeout_seconds"`
	MaxTurnDurationSecs int               `json:"max_turn_duration_seconds"`
	Parallelism         int               `json:"parallelism"`
	RespondTo           string            `json:"respond_to"`
	RespondToAllowlist  []string          `json:"respond_to_allowlist"`
	EnvVars             map[string]string `json:"env_vars"`
}

// ProviderConfig is the provider_config object (docs/CONTRACT.md §3). All
// fields are optional; buzz's desktop already validates it secret-free
// before it reaches us.
type ProviderConfig struct {
	Profile          string `json:"profile"`
	IdleTimeout      string `json:"idle_timeout"`
	KeepWorkspacePAT bool   `json:"keep_workspace_pat"`
	BuzzVersion      string `json:"buzz_version"`
	// InferenceAuth selects how the sandboxed agent authenticates to the
	// workspace AI Gateway. Allowed values: "" and "env" (default: the
	// agent uses DATABRICKS_HOST/DATABRICKS_TOKEN supplied via
	// agent.env_vars, today's behavior) or "sandbox" (zero-token: the
	// agent instead derives its credential at launch time from the
	// sandbox's own baked ~/.databrickscfg, so no token ever needs to be
	// minted or transmitted). The key is named "inference_auth" rather
	// than something containing token/key/secret/password/credential
	// deliberately, so it passes the desktop's secret-word config filter.
	InferenceAuth string `json:"inference_auth"`

	// ClaudeAdapterVersion pins the @agentclientprotocol/claude-agent-acp
	// version installed for the Claude runtime; empty means that adapter's
	// AdapterSpec.DefaultVersion. Expert-only: deliberately NOT
	// advertised in the provider's config_schema (same posture as
	// BuzzVersion and KeepWorkspacePAT), so Buzz Desktop's create-agent
	// dialog stays unchanged. Every segment of the key name avoids the
	// desktop's secret-word filter (token/key/secret/password/credential).
	ClaudeAdapterVersion string `json:"claude_adapter_version"`

	// CodexAdapterVersion is the codex twin of ClaudeAdapterVersion, with
	// the same expert-only posture and the same secret-word-safe naming.
	//
	// Bumping it is a deliberate act with a consequence beyond the version
	// number: the codex adapter — not this provider's config.toml — is
	// what governs the agent's sandbox and approval policy
	// (docs/M3_CODEX_PROBE_RESULTS.md S7/S10), so a new version must be
	// re-verified against that finding rather than assumed equivalent.
	CodexAdapterVersion string `json:"codex_adapter_version"`
}

// SandboxInferenceAuth reports whether provider_config opts the deploy into
// zero-token inference auth (InferenceAuth == "sandbox"); see the field's
// doc comment above.
func (c ProviderConfig) SandboxInferenceAuth() bool {
	return c.InferenceAuth == "sandbox"
}

// ParseDeployRequest unmarshals a raw deploy request body (the "agent" and
// "provider_config" sub-objects of the envelope already routed by
// internal/provider). Unknown fields anywhere are tolerated by default
// (encoding/json ignores them unless DisallowUnknownFields is used, which we
// deliberately never call).
func ParseDeployRequest(data []byte) (*DeployRequest, error) {
	var req DeployRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("parse deploy request: %w", err)
	}
	return &req, nil
}

// Validate checks the fields deploy provisioning depends on. It never
// returns a value derived from secrets in the error text.
func (a Agent) Validate() error {
	if a.PrivateKeyNsec == "" {
		return fmt.Errorf("agent.private_key_nsec must not be empty")
	}
	if a.RelayURL == "" {
		return fmt.Errorf("agent.relay_url must not be empty")
	}
	if _, ok := RuntimeFor(a.AgentCommand); !ok {
		return unsupportedRuntimeError(a.AgentCommand)
	}
	// env_vars keys are written raw (unquoted) into a shell-sourced env
	// file by internal/nest's RenderEnv, so a key that isn't a valid
	// shell/environment variable name is an arbitrary-command-execution
	// vector in that sandbox shell. Reject anything else here, the single
	// choke point both deploy entry points (provider.handleDeploy and
	// deployflow.Deploy) route through via DeployRequest.Validate().
	for key := range a.EnvVars {
		if !envVarKeyPattern.MatchString(key) {
			return fmt.Errorf(
				"env_vars key %q is not a valid environment variable name; only names matching ^[A-Za-z_][A-Za-z0-9_]*$ are allowed",
				key,
			)
		}
	}
	return nil
}

// validInferenceAuthValues are the only accepted provider_config.inference_auth
// values. Matching is exact-case; "SANDBOX" or similar variants are rejected
// rather than silently normalized, so callers can't be surprised by a typo
// quietly falling back to a different auth mode.
var validInferenceAuthValues = map[string]bool{
	"":        true,
	"env":     true,
	"sandbox": true,
}

// Validate validates the full deploy request: the agent sub-object plus the
// provider_config fields this provider itself constrains. It never returns a
// value derived from secrets in the error text.
func (r DeployRequest) Validate() error {
	if err := r.Agent.Validate(); err != nil {
		return err
	}
	if !validInferenceAuthValues[r.ProviderConfig.InferenceAuth] {
		return fmt.Errorf(
			"provider_config.inference_auth %q is not supported; allowed values are \"\", \"env\", \"sandbox\"",
			r.ProviderConfig.InferenceAuth,
		)
	}
	if err := r.validateClaudeInferenceSource(); err != nil {
		return err
	}
	return nil
}

// validateClaudeInferenceSource requires the Claude runtime to have exactly
// one coherent place to get its inference endpoint from, and is the
// fail-loud half of the credential-egress defense (the fail-closed half
// lives in nest.ClaudeEnvSnippet).
//
// Why this exists, from a live probe (docs/M2_CLAUDE_PROBE_RESULTS.md):
// Claude Code falls back to https://api.anthropic.com when
// ANTHROPIC_BASE_URL is unset, and a Lakebox sandbox has OPEN egress to
// that host (HTTP 401 in 147ms with a genuine Anthropic error body). So an
// inference_auth="env" deploy that supplies DATABRICKS_TOKEN but forgets
// DATABRICKS_HOST would otherwise send a live workspace PAT to a third
// party in an Authorization header — while every provider-side gate
// (install verification, launch verification) still reported success,
// because none of them touch the LLM.
//
// This must live on DeployRequest rather than Agent: it is the only level
// that sees both agent.env_vars and provider_config.inference_auth.
//
// Note there is deliberately NO check here for ANTHROPIC_API_KEY coexisting
// with ANTHROPIC_AUTH_TOKEN. That hazard was hypothesized and then
// disproven live: the adapter completes turns normally with an
// ANTHROPIC_API_KEY that is empty, and with one set to a bogus value,
// alongside ANTHROPIC_AUTH_TOKEN. An API key simply cannot work against
// this gateway (it sends x-api-key, which the gateway rejects with 401), so
// it is inert rather than dangerous.
func (r DeployRequest) validateClaudeInferenceSource() error {
	rt, ok := RuntimeFor(r.Agent.AgentCommand)
	if !ok || rt != RuntimeClaude {
		return nil
	}
	// Bring-your-own endpoint requires bring-your-own token, checked here so
	// it fails at the payload boundary rather than at the deploy-time
	// inference probe (whose "unset" diagnosis would send the operator to
	// DATABRICKS_HOST, not to the token they actually omitted).
	//
	// The requirement is not arbitrary: an explicit ANTHROPIC_BASE_URL
	// suppresses the snippet's own derivation, and the snippet deliberately
	// refuses to attach the workspace credential to an endpoint it did not
	// choose — so this combination can only ever produce an agent with an
	// endpoint and no credential.
	if r.Agent.EnvVars["ANTHROPIC_BASE_URL"] != "" {
		if r.Agent.EnvVars["ANTHROPIC_AUTH_TOKEN"] == "" {
			return fmt.Errorf(
				"agent_command %q sets env_vars.ANTHROPIC_BASE_URL without env_vars.ANTHROPIC_AUTH_TOKEN: "+
					"an endpoint this provider did not derive is never given the workspace credential, so the agent would have no token. "+
					"Supply both, or drop ANTHROPIC_BASE_URL to use the workspace AI Gateway",
				r.Agent.AgentCommand,
			)
		}
		return nil
	}
	if r.ProviderConfig.SandboxInferenceAuth() {
		// Zero-token mode derives DATABRICKS_HOST in-sandbox from the
		// baked ~/.databrickscfg, so there is nothing more to require.
		// Checked AFTER the bring-your-own-endpoint rule above, not
		// before: an explicit ANTHROPIC_BASE_URL suppresses derivation in
		// this mode too, so short-circuiting here would accept exactly the
		// endpoint-without-credential deploy that rule exists to reject.
		return nil
	}
	if r.Agent.EnvVars["DATABRICKS_HOST"] != "" {
		return nil
	}
	return fmt.Errorf(
		"agent_command %q needs an inference endpoint: set env_vars.DATABRICKS_HOST (with DATABRICKS_TOKEN) to use the workspace AI Gateway, "+
			"or set provider_config.inference_auth=\"sandbox\" to derive both from the sandbox's own credential, "+
			"or set env_vars.ANTHROPIC_BASE_URL together with env_vars.ANTHROPIC_AUTH_TOKEN to target another endpoint "+
			"(an endpoint this provider did not derive is never given the workspace credential). "+
			"Without one of these the agent would fall back to the public Anthropic API and send its token there",
		r.Agent.AgentCommand,
	)
}
