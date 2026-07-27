// Package payload defines the deploy-request wire shapes documented in
// docs/CONTRACT.md §3 and validates them. Unknown JSON fields are always
// tolerated (buzz's payload may grow fields we don't yet know about).
package payload

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// SupportedAgentCommand is the only agent_command value v0 of this provider
// accepts. PLAN.md §4.2 / Decision 4: goose/claude/codex are v0.1 scope.
const SupportedAgentCommand = "buzz-agent"

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
	if a.AgentCommand != SupportedAgentCommand {
		return fmt.Errorf(
			"agent_command %q is not supported by this provider yet; v0 only supports %q, see v0.1 roadmap (https://github.com/IceRhymers/buzz-lakebox/issues/1) for goose/claude/codex support",
			a.AgentCommand, SupportedAgentCommand,
		)
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
	return nil
}
