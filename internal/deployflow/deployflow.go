// Package deployflow orchestrates docs/PLAN.md §4.4's 11-step deploy flow:
// validate, preflight, reuse-or-create, PAT reset, install+verify runtime,
// provision the nest, hand over secrets, kill-then-launch, verify, and
// finally relax the autostop policy — with the §4.3 failure-teardown
// wrapper around every in-sandbox step. This is the internal/provider.DeployFunc
// implementation wired into main.go.
package deployflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/identity"
	"github.com/IceRhymers/buzz-lakebox/internal/install"
	"github.com/IceRhymers/buzz-lakebox/internal/lakebox"
	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/redact"
	"github.com/IceRhymers/buzz-lakebox/internal/sshx"
)

const (
	// deployTimeout bounds the whole flow well inside buzz's 600s
	// invocation budget (docs/CONTRACT.md §1).
	deployTimeout = 550 * time.Second

	// defaultProfile is used when provider_config.profile is empty.
	defaultProfile = "DEFAULT"

	// defaultWaitRunningTimeout bounds how long we wait for a
	// created/started sandbox to reach Running (docs/M05_PROBE_RESULTS.md:
	// create->Running ~1.1s, start ~20.5s observed live; generous margin
	// here for slower workspaces).
	defaultWaitRunningTimeout = 90 * time.Second
	defaultPollInterval       = 2 * time.Second

	// defaultVerifyDelay is N=10s (docs/PLAN.md §4.4 step 10,
	// docs/M05_PROBE_RESULTS.md §3: terminal failure lands ~1s after
	// launch, so 10s is ample).
	defaultVerifyDelay = 10 * time.Second

	// installVerifyTimeoutSeconds bounds the ACP initialize handshake
	// (docs/M05_PROBE_RESULTS.md §6: "<1s ... without touching the LLM").
	installVerifyTimeoutSeconds = 10

	// Log vocabulary verified live (docs/M05_PROBE_RESULTS.md §3).
	agentPoolReadyMarker = "agent_pool_ready"
	terminalErrorLine    = "initial relay connect failed with terminal error"

	verifyEnvFilePath = "$HOME/.buzz-backend/.env.verify"
)

// Deployer implements the deploy flow and satisfies
// internal/provider.DeployFunc via its Deploy method.
type Deployer struct {
	CLI *lakebox.CLI
	SSH *sshx.Client

	// Sleep is used for the step-10 post-launch wait; injectable so
	// tests avoid a real wall-clock wait (PLAN.md §7: no network/real
	// timing in CI).
	Sleep func(time.Duration)

	// VerifyDelay is N in docs/PLAN.md §4.4 step 10 (default 10s).
	VerifyDelay time.Duration

	// WaitRunningTimeout/PollInterval bound polling a sandbox to Running
	// after create/start.
	WaitRunningTimeout time.Duration
	PollInterval       time.Duration
}

// New returns a Deployer with production defaults.
func New(cli *lakebox.CLI, ssh *sshx.Client) *Deployer {
	return &Deployer{
		CLI:                cli,
		SSH:                ssh,
		Sleep:              time.Sleep,
		VerifyDelay:        defaultVerifyDelay,
		WaitRunningTimeout: defaultWaitRunningTimeout,
		PollInterval:       defaultPollInterval,
	}
}

func (d *Deployer) sleep(dur time.Duration) {
	if d.Sleep != nil {
		d.Sleep(dur)
		return
	}
	time.Sleep(dur)
}

// Deploy implements internal/provider.DeployFunc.
func (d *Deployer) Deploy(req *payload.DeployRequest) (string, error) {
	id, err := d.deploy(req)
	if err != nil {
		// Belt-and-suspenders redaction at this package's own boundary
		// (docs/PLAN.md §5): internal/provider also redacts every
		// DeployFunc error, but deployflow must not depend on that when
		// invoked directly (operator `deploy --payload-file`).
		secrets := redact.SecretsFromPayload(req.Agent)
		return "", errors.New(redact.Redact(err.Error(), secrets))
	}
	return id, nil
}

func (d *Deployer) deploy(req *payload.DeployRequest) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()

	agent := req.Agent

	// Step 1: validate (defense-in-depth; internal/provider already
	// validates before invoking DeployFunc, but deployflow may also be
	// invoked directly by the operator `deploy --payload-file` path).
	if err := req.Validate(); err != nil {
		return "", err
	}

	profile := req.ProviderConfig.Profile
	if profile == "" {
		profile = defaultProfile
	}

	// Step 2: preflight.
	version, err := d.CLI.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("preflight: could not determine databricks CLI version: %w", err)
	}
	meets, err := lakebox.MeetsMinVersion(version)
	if err != nil {
		return "", d.wrap("", version, fmt.Errorf("preflight: %w", err))
	}
	if !meets {
		return "", d.wrap("", version, fmt.Errorf("preflight: databricks CLI %s is below the minimum supported %s; upgrade the CLI", version, lakebox.MinCLIVersion))
	}
	if _, err := d.CLI.CurrentUser(ctx, profile); err != nil {
		return "", d.wrap("", version, fmt.Errorf("preflight: profile %q does not resolve: %w", profile, err))
	}
	if err := d.CLI.SandboxRegister(ctx, profile); err != nil {
		return "", d.wrap("", version, fmt.Errorf("preflight: sandbox register: %w", err))
	}

	// Step 3: reuse-or-create, keyed on the agent's npub identity.
	npub, err := identity.NsecToNpub(agent.PrivateKeyNsec)
	if err != nil {
		return "", d.wrap("", version, fmt.Errorf("derive identity from private_key_nsec: %w", err))
	}
	prefix, err := identity.PrefixFor(npub)
	if err != nil {
		return "", d.wrap("", version, err)
	}

	sandboxes, _, err := d.CLI.SandboxList(ctx, profile)
	if err != nil {
		return "", d.wrap("", version, fmt.Errorf("sandbox list: %w", err))
	}

	var matches []lakebox.Sandbox
	for _, sb := range sandboxes {
		if strings.HasPrefix(sb.Name, prefix) {
			matches = append(matches, sb)
		}
	}

	var sandboxID string
	var freshlyCreated bool

	switch len(matches) {
	case 0:
		name, nerr := identity.SandboxName(npub, agent.Name)
		if nerr != nil {
			return "", d.wrap("", version, nerr)
		}
		sb, cerr := d.CLI.SandboxCreate(ctx, profile, name)
		if cerr != nil {
			return "", d.wrap("", version, fmt.Errorf("sandbox create: %w", cerr))
		}
		sandboxID = sb.ID
		freshlyCreated = true
		if !strings.EqualFold(sb.Status, lakebox.StatusRunning) {
			if werr := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); werr != nil {
				d.teardown(ctx, profile, sandboxID, freshlyCreated)
				return "", d.wrap(sandboxID, version, fmt.Errorf("waiting for freshly created sandbox to reach Running: %w", werr))
			}
		}
	case 1:
		sandboxID = matches[0].ID
		if !strings.EqualFold(matches[0].Status, lakebox.StatusRunning) {
			if serr := d.CLI.SandboxStart(ctx, profile, sandboxID); serr != nil {
				return "", d.wrap(sandboxID, version, fmt.Errorf("sandbox start: %w", serr))
			}
			if werr := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); werr != nil {
				return "", d.wrap(sandboxID, version, fmt.Errorf("waiting for reused sandbox to reach Running: %w", werr))
			}
		}
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return "", d.wrap("", version, fmt.Errorf(
			"ambiguous identity: %d sandboxes match prefix %q (%s); refusing to guess — manually delete the stale sandbox(es) and redeploy",
			len(matches), prefix, strings.Join(ids, ", "),
		))
	}

	// Steps 4-11: provisioning, wrapped by failure teardown.
	if err := d.provision(ctx, profile, sandboxID, freshlyCreated, req); err != nil {
		d.teardown(ctx, profile, sandboxID, freshlyCreated)
		return "", d.wrap(sandboxID, version, err)
	}

	return sandboxID, nil
}

func (d *Deployer) waitTimeout() time.Duration {
	if d.WaitRunningTimeout > 0 {
		return d.WaitRunningTimeout
	}
	return defaultWaitRunningTimeout
}

func (d *Deployer) pollInterval() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return defaultPollInterval
}

func (d *Deployer) verifyDelay() time.Duration {
	if d.VerifyDelay > 0 {
		return d.VerifyDelay
	}
	return defaultVerifyDelay
}

// wrap embeds the sandbox id (when known) and CLI version in err, per
// docs/PLAN.md §4.3: "Every {ok:false} error embeds the sandbox id (when
// one exists) and the recorded CLI version."
func (d *Deployer) wrap(sandboxID, version string, err error) error {
	if err == nil {
		return nil
	}
	if sandboxID == "" {
		return fmt.Errorf("%w (databricks cli %s)", err, version)
	}
	return fmt.Errorf("%w (sandbox %s, databricks cli %s)", err, sandboxID, version)
}

// provision runs docs/PLAN.md §4.4 steps 4-11 against an established
// (Running) sandbox.
func (d *Deployer) provision(ctx context.Context, profile, sandboxID string, freshlyCreated bool, req *payload.DeployRequest) error {
	agent := req.Agent
	envContent := nest.RenderEnv(agent)

	// Step 4: PAT reset — the first in-sandbox action of every deploy,
	// unless the owner opted out.
	if !req.ProviderConfig.KeepWorkspacePAT {
		if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
			step("pat-reset", "set -eu; umask 077; cat > \"$HOME/.databrickscfg\""),
			strings.NewReader(nest.PATStub),
		); err != nil {
			return fmt.Errorf("PAT reset: %w", err)
		}
	}

	// Step 5: install pinned .deb + sha256 + runtime verification.
	if err := d.installAndVerify(ctx, profile, sandboxID, req.ProviderConfig.BuzzVersion, envContent); err != nil {
		return err
	}

	// Step 6: provision the nest working dirs.
	if _, err := d.SSH.Run(ctx, profile, sandboxID,
		step("nest-dirs", `set -eu; umask 077; mkdir -p "$HOME/.buzz" "$HOME/.buzz/REPOS" "$HOME/.buzz/OUTBOX" "$HOME/.buzz-backend"`),
	); err != nil {
		return fmt.Errorf("provision nest dirs: %w", err)
	}

	// Step 7: secret handover — env file via stdin only, 0600 at rest.
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("env-write", fmt.Sprintf(`set -eu; umask 077; cat > %s && chmod 600 %s`, dquote(nest.EnvFilePath), dquote(nest.EnvFilePath))),
		strings.NewReader(envContent),
	); err != nil {
		return fmt.Errorf("write env file: %w", err)
	}

	// Step 8: update-in-place guard — kill any existing buzz-acp process
	// group before relaunching (tolerate no-match).
	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("prelaunch-kill", `pkill -f buzz-acp 2>/dev/null; true`)); err != nil {
		return fmt.Errorf("kill existing buzz-acp process group: %w", err)
	}

	// Step 9: write + run launch.sh.
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("launch-write", fmt.Sprintf(`set -eu; umask 077; cat > %s && chmod 700 %s`, dquote(nest.LaunchScriptPath), dquote(nest.LaunchScriptPath))),
		strings.NewReader(nest.RenderLaunchScript()),
	); err != nil {
		return fmt.Errorf("write launch.sh: %w", err)
	}
	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("launch-exec", fmt.Sprintf(`sh %s`, dquote(nest.LaunchScriptPath)))); err != nil {
		return fmt.Errorf("run launch.sh: %w", err)
	}

	// Step 10: verify after N=VerifyDelay, then (only on success) set
	// the autostop policy.
	if err := d.verifyLaunch(ctx, profile, sandboxID); err != nil {
		return err
	}

	// Step 11: autostop policy, set LAST.
	if err := d.setAutostopPolicy(ctx, profile, sandboxID, req.ProviderConfig); err != nil {
		return fmt.Errorf("set autostop policy: %w", err)
	}

	return nil
}

// installAndVerify runs docs/PLAN.md §4.4 step 5: fetch/verify/extract the
// pinned .deb (skipped in-script when the version marker already
// matches), then the ACP initialize runtime-verification handshake
// (docs/M05_PROBE_RESULTS.md §6). The verification env file carries the
// *actual* agent env (so BUZZ_AGENT_PROVIDER/DATABRICKS_* config
// validation and gateway auth both exercise real values) and is shredded
// immediately after use — step 7 writes the durable copy separately.
func (d *Deployer) installAndVerify(ctx context.Context, profile, sandboxID, buzzVersion, envContent string) error {
	script, err := install.BuildInstallScript(buzzVersion)
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}

	const installScriptPath = "$HOME/.buzz-backend/install.sh"
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("install-write", fmt.Sprintf(`set -eu; umask 077; mkdir -p "$HOME/.buzz-backend"; cat > %s`, dquote(installScriptPath))),
		strings.NewReader(script),
	); err != nil {
		return fmt.Errorf("install: write install script: %w", err)
	}
	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("install-exec", fmt.Sprintf(`sh %s`, dquote(installScriptPath)))); err != nil {
		return fmt.Errorf("install: %w", err)
	}

	// Runtime verification: ACP initialize handshake with the agent env
	// sourced (docs/M05_PROBE_RESULTS.md §6).
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("verify-env-write", fmt.Sprintf(`set -eu; umask 077; cat > %s && chmod 600 %s`, dquote(verifyEnvFilePath), dquote(verifyEnvFilePath))),
		strings.NewReader(envContent),
	); err != nil {
		return fmt.Errorf("install verification: write temp env: %w", err)
	}

	out, err := d.SSH.Run(ctx, profile, sandboxID, step("verify-exec", install.BuildVerifyCommand(verifyEnvFilePath, installVerifyTimeoutSeconds)))
	if err != nil {
		return fmt.Errorf("install verification: buzz-agent ACP initialize handshake failed: %w", err)
	}
	if !strings.Contains(out, install.AgentInfoMarker) {
		return fmt.Errorf("install verification: buzz-agent ACP initialize response did not contain %q: %s", install.AgentInfoMarker, strings.TrimSpace(out))
	}
	return nil
}

// verifyLaunch implements docs/PLAN.md §4.4 step 10's pass signal, waiting
// VerifyDelay before checking.
func (d *Deployer) verifyLaunch(ctx context.Context, profile, sandboxID string) error {
	d.sleep(d.verifyDelay())

	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("verify-pgrep", "pgrep -f buzz-acp")); err != nil {
		logOut, _ := d.SSH.Run(ctx, profile, sandboxID, step("verify-log", `cat "$HOME/.buzz-backend/acp.log" 2>/dev/null || true`))
		return fmt.Errorf("verify: buzz-acp process not found %s after launch: %w (acp.log: %s)", d.verifyDelay(), err, strings.TrimSpace(logOut))
	}

	logOut, err := d.SSH.Run(ctx, profile, sandboxID, step("verify-log", `cat "$HOME/.buzz-backend/acp.log" 2>/dev/null || true`))
	if err != nil {
		return fmt.Errorf("verify: could not read acp.log: %w", err)
	}

	if strings.Contains(logOut, terminalErrorLine) {
		return fmt.Errorf(
			"verify: relay connection failed (%q); this nostr key is very likely not a member of the target relay — mint/register a relay-member key for this agent and redeploy",
			terminalErrorLine,
		)
	}
	if !strings.Contains(logOut, agentPoolReadyMarker) {
		return fmt.Errorf("verify: acp.log did not contain %q within %s; log: %s", agentPoolReadyMarker, d.verifyDelay(), strings.TrimSpace(logOut))
	}
	return nil
}

// setAutostopPolicy implements docs/PLAN.md §4.4 step 11: default
// --no-autostop, or --idle-timeout <v> when provider_config.idle_timeout
// is set.
func (d *Deployer) setAutostopPolicy(ctx context.Context, profile, sandboxID string, cfg payload.ProviderConfig) error {
	opts := lakebox.SandboxConfigOptions{NoAutostop: true}
	if cfg.IdleTimeout != "" {
		opts = lakebox.SandboxConfigOptions{IdleTimeout: cfg.IdleTimeout}
	}
	return d.CLI.SandboxConfig(ctx, profile, sandboxID, opts)
}

// teardown implements docs/PLAN.md §4.3 failure teardown: best-effort
// shred of any env files written this invocation, then either delete a
// sandbox freshly created this invocation, or (for a reused sandbox) kill
// the buzz-acp process group and leave the sandbox alone. All best-effort
// — errors here are swallowed since the caller already has the real
// failure to report, and teardown itself failing must not mask it
// (docs/PLAN.md §4.3: "a human can recover if teardown itself fails").
func (d *Deployer) teardown(ctx context.Context, profile, sandboxID string, freshlyCreated bool) {
	if sandboxID == "" {
		return
	}
	shredCmd := step("teardown-shred", fmt.Sprintf(
		`shred -u %s 2>/dev/null || rm -f %s 2>/dev/null; shred -u %s 2>/dev/null || rm -f %s 2>/dev/null; true`,
		dquote(nest.EnvFilePath), dquote(nest.EnvFilePath), dquote(verifyEnvFilePath), dquote(verifyEnvFilePath),
	))
	_, _ = d.SSH.Run(ctx, profile, sandboxID, shredCmd)

	if freshlyCreated {
		_ = d.CLI.SandboxDelete(ctx, profile, sandboxID)
		return
	}
	_, _ = d.SSH.Run(ctx, profile, sandboxID, step("teardown-pkill", `pkill -f buzz-acp 2>/dev/null; true`))
}

// step prepends an inert shell comment tagging cmd with a short
// identifier (e.g. "install-exec"). It has no effect on execution — sh
// ignores comment lines — and exists purely so tests can assert
// call-order against a fake `databricks` CLI shim without depending on
// exact command text (PLAN.md §7: "assert full call ORDER").
func step(tag, cmd string) string {
	return "# buzz-step:" + tag + "\n" + cmd
}

// dquote double-quotes s for interpolation into a POSIX shell command
// string, preserving "$HOME"-style shell expansion inside s (these are
// always fixed path constants, never secrets) while still protecting
// against word splitting/globbing.
func dquote(s string) string {
	return `"` + s + `"`
}
