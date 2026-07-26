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
	"strconv"
	"strings"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/identity"
	"github.com/IceRhymers/buzz-lakebox/internal/install"
	"github.com/IceRhymers/buzz-lakebox/internal/lakebox"
	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/redact"
	"github.com/IceRhymers/buzz-lakebox/internal/sshx"
	"github.com/IceRhymers/buzz-lakebox/internal/state"
	"github.com/IceRhymers/buzz-lakebox/internal/version"
)

const (
	// deployTimeout bounds the whole flow well inside buzz's 600s
	// invocation budget (docs/CONTRACT.md §1).
	deployTimeout = 550 * time.Second

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

	// teardownTimeout bounds teardown's OWN context (BUG 4 fix): teardown
	// must be able to run even when it is triggered by the deploy
	// timeout expiring, so it gets an independent budget rather than
	// inheriting the (possibly already-expired) deploy context.
	teardownTimeout = 60 * time.Second

	// maxErrorLogBytes bounds how much of any remote log/output is ever
	// interpolated into an error string (BUG 5 fix): an unbounded
	// acp.log (or other remote output) must never blow up an error
	// message.
	maxErrorLogBytes = 4096
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

	// DeployTimeout bounds the whole deploy() flow (default
	// deployTimeout); injectable so tests can force the deploy context to
	// expire mid-provision, proving teardown still completes on its own
	// independent budget (BUG 4 regression coverage).
	DeployTimeout time.Duration

	// State is the provider-side npub→sandbox mapping — the ONLY
	// durable reuse key (see internal/state's package doc: the desktop
	// never sends backend_agent_id and Lakebox does not persist
	// caller-set names, so without this every redeploy orphans a
	// still-billing --no-autostop sandbox). nil disables persistence
	// (lookups skipped, nothing recorded); tests point it at a temp
	// file.
	State *state.Store
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
		DeployTimeout:      deployTimeout,
		State:              state.NewDefault(),
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
	ctx, cancel := context.WithTimeout(context.Background(), d.deployTimeoutOrDefault())
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
		profile = version.DefaultProfile
	}

	// Step 2: preflight. CachedVersion (not Version) so the fetch shares
	// lakebox.CLI's sync.Once cache (BUG 9 fix: no duplicate `databricks
	// version` subprocess). The returned version is what wrap() — the
	// single stamping boundary for the §4.3 sandbox-id + CLI-version
	// error annotations — stamps onto every error below.
	version, err := d.CLI.CachedVersion(ctx)
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

	var sandboxID string
	var freshlyCreated bool

	// Step 3a: provider-side state lookup — the PRIMARY reuse key. The
	// desktop never echoes backend_agent_id back in deploy payloads, and
	// Lakebox does not persist caller-set sandbox names (live-verified
	// 2026-07-26: list/status return the id as "name"), so the name-prefix
	// match below cannot find a sandbox created by an earlier process.
	// Without this lookup, every redeploy would orphan the previous
	// sandbox — still running, --no-autostop, billing forever. A stale
	// mapping (sandbox deleted out-of-band) fails the status probe and
	// falls through to the legacy match / create path.
	stateKey := state.Key(profile, npub)
	if d.State != nil {
		if entry, ok, serr := d.State.Lookup(stateKey); serr != nil {
			return "", d.wrap("", version, fmt.Errorf("read sandbox state file: %w", serr))
		} else if ok {
			if sb, perr := d.CLI.SandboxStatus(ctx, profile, entry.SandboxID); perr == nil {
				sandboxID = sb.ID
				if !strings.EqualFold(sb.Status, lakebox.StatusRunning) {
					if serr := d.CLI.SandboxStart(ctx, profile, sandboxID); serr != nil {
						return "", d.wrap(sandboxID, version, fmt.Errorf("sandbox start: %w", serr))
					}
					if werr := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); werr != nil {
						return "", d.wrap(sandboxID, version, fmt.Errorf("waiting for reused sandbox to reach Running: %w", werr))
					}
				}
			}
		}
	}

	if sandboxID == "" {
		var merr error
		sandboxID, freshlyCreated, merr = d.matchOrCreate(ctx, profile, prefix, npub, agent.Name)
		if merr != nil {
			// sandboxID may carry a partial id (e.g. a matched sandbox
			// that failed to start) for wrap's annotation, or be empty.
			return "", d.wrap(sandboxID, version, merr)
		}
	}

	// Persist the mapping BEFORE provisioning: a mapping that cannot be
	// written means every future redeploy leaks a sandbox, so fail fast
	// (the teardown wrapper below cleans up a freshly created sandbox on
	// provisioning failure, and a torn-down id left in the state file is
	// self-healing — the next deploy's status probe rejects it).
	if d.State != nil {
		if rerr := d.State.Record(stateKey, state.Entry{SandboxID: sandboxID, Profile: profile}); rerr != nil {
			if freshlyCreated {
				d.teardown(profile, sandboxID, freshlyCreated)
			}
			return "", d.wrap(sandboxID, version, fmt.Errorf("persist sandbox mapping (required to reuse this sandbox on redeploy): %w", rerr))
		}
	}

	// Steps 4-11: provisioning, wrapped by failure teardown. A
	// *postVerifyFailure means launch verification already succeeded
	// (BUG 6 fix): the agent is healthy, so destructive teardown must be
	// skipped entirely — only a genuinely unhealthy/unverified deploy
	// gets torn down.
	if err := d.provision(ctx, profile, sandboxID, freshlyCreated, req); err != nil {
		var pvf *postVerifyFailure
		if !errors.As(err, &pvf) {
			d.teardown(profile, sandboxID, freshlyCreated)
		}
		return "", d.wrap(sandboxID, version, err)
	}

	return sandboxID, nil
}

// matchOrCreate is the legacy step-3 path when the state file has no
// usable mapping: match on the npub-derived name prefix (kept for
// sandboxes created before the naming regression and for services that
// persist names again), else create fresh.
func (d *Deployer) matchOrCreate(ctx context.Context, profile, prefix, npub, agentName string) (string, bool, error) {
	sandboxes, _, err := d.CLI.SandboxList(ctx, profile)
	if err != nil {
		return "", false, fmt.Errorf("sandbox list: %w", err)
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
		name, nerr := identity.SandboxName(npub, agentName)
		if nerr != nil {
			return "", false, nerr
		}
		sb, cerr := d.CLI.SandboxCreate(ctx, profile, name)
		if cerr != nil {
			return "", false, fmt.Errorf("sandbox create: %w", cerr)
		}
		sandboxID = sb.ID
		freshlyCreated = true
		if !strings.EqualFold(sb.Status, lakebox.StatusRunning) {
			if werr := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); werr != nil {
				d.teardown(profile, sandboxID, freshlyCreated)
				return "", false, fmt.Errorf("waiting for freshly created sandbox to reach Running: %w", werr)
			}
		}
	case 1:
		sandboxID = matches[0].ID
		if !strings.EqualFold(matches[0].Status, lakebox.StatusRunning) {
			if serr := d.CLI.SandboxStart(ctx, profile, sandboxID); serr != nil {
				return sandboxID, false, fmt.Errorf("sandbox start: %w", serr)
			}
			if werr := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); werr != nil {
				return sandboxID, false, fmt.Errorf("waiting for reused sandbox to reach Running: %w", werr)
			}
		}
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return "", false, fmt.Errorf(
			"ambiguous identity: %d sandboxes match prefix %q (%s); refusing to guess — manually delete the stale sandbox(es) and redeploy",
			len(matches), prefix, strings.Join(ids, ", "),
		)
	}

	return sandboxID, freshlyCreated, nil
}

func (d *Deployer) deployTimeoutOrDefault() time.Duration {
	if d.DeployTimeout > 0 {
		return d.DeployTimeout
	}
	return deployTimeout
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

// wrap embeds the sandbox id and CLI version (each when known) in err,
// per docs/PLAN.md §4.3: "Every {ok:false} error embeds the sandbox id
// (when one exists) and the recorded CLI version." This is the SINGLE
// stamping boundary for BOTH annotations (review round 2, refining
// BUG 9): lakebox.wrapErr no longer stamps a version, so every error
// leaving deploy() — lakebox-originated or not (sshx install/verify
// failures, identity errors, ambiguous-identity, semver parse) — gets
// exactly one "(databricks cli X)" here, from the version CachedVersion
// already fetched at preflight (no extra subprocess). Failures before
// preflight fetched a version stamp what exists: empty → omitted.
func (d *Deployer) wrap(sandboxID, version string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case sandboxID == "" && version == "":
		return err
	case sandboxID == "":
		return fmt.Errorf("%w (databricks cli %s)", err, version)
	case version == "":
		return fmt.Errorf("%w (sandbox %s)", err, sandboxID)
	default:
		return fmt.Errorf("%w (sandbox %s, databricks cli %s)", err, sandboxID, version)
	}
}

// postVerifyFailure marks an error that occurred AFTER launch
// verification already succeeded (today, only setAutostopPolicy) — BUG 6
// fix. deploy() must not run destructive teardown for these: the agent
// is healthy, and docs/PLAN.md §4.3's teardown intent ("never strand a
// secret-bearing env file or a runaway sandbox") is about UNHEALTHY
// deploys, not a healthy one whose autostop policy merely failed to
// apply. Pre-verify failures are unaffected and keep exact prior
// behavior.
type postVerifyFailure struct {
	err error
}

func (e *postVerifyFailure) Error() string { return e.err.Error() }
func (e *postVerifyFailure) Unwrap() error { return e.err }

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
	// install-write (below) already `mkdir -p "$HOME/.buzz-backend"`, and
	// launch.sh (step 9) `mkdir -p`s the full nest working-dir set, so the
	// former standalone "nest working dirs" SSH round trip (step 6) was a
	// redundant extra round trip and has been removed (C5 cleanup).
	if err := d.installAndVerify(ctx, profile, sandboxID, req.ProviderConfig.BuzzVersion, envContent); err != nil {
		return err
	}

	// Step 7: secret handover — env file via stdin only, 0600 at rest.
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("env-write", fmt.Sprintf(`set -eu; umask 077; cat > %s && chmod 600 %s`, dquote(nest.EnvFilePath), dquote(nest.EnvFilePath))),
		strings.NewReader(envContent),
	); err != nil {
		return fmt.Errorf("write env file: %w", err)
	}

	// Step 8: update-in-place guard — kill any existing buzz-acp process
	// group before relaunching (tolerate no-match). Pattern is
	// '[b]uzz-acp', NOT 'buzz-acp' (BUG 2 fix): the literal pattern
	// 'buzz-acp' also appears in this pkill invocation's OWN argv (as
	// seen by the remote shell), so pkill -f would SIGTERM its own
	// invoking shell and abort the happy path with a non-zero exit. The
	// bracket idiom's regex still matches the real "buzz-acp" process
	// name but the argv text "[b]uzz-acp" does not match itself.
	if _, err := d.SSH.Run(ctx, profile, sandboxID, step("prelaunch-kill", `pkill -f '[b]uzz-acp' 2>/dev/null; true`)); err != nil {
		return fmt.Errorf("kill existing buzz-acp process group: %w", err)
	}

	// Step 9: write + run launch.sh.
	if _, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("launch-write", fmt.Sprintf(`set -eu; umask 077; cat > %s && chmod 700 %s`, dquote(nest.LaunchScriptPath), dquote(nest.LaunchScriptPath))),
		strings.NewReader(nest.RenderLaunchScript(req.ProviderConfig.KeepWorkspacePAT)),
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

	// Step 11: autostop policy, set LAST. A failure here is a
	// postVerifyFailure (BUG 6): launch already verified healthy, so the
	// caller must not tear anything down for this specific failure. The
	// message deliberately does NOT interpolate the sandbox id itself:
	// deploy()'s wrap() appends "(sandbox <id>, databricks cli <v>)" to
	// every error, and embedding the id here too duplicated it (review
	// round 2) — the remedy names a <sandbox-id> placeholder that wrap's
	// annotation resolves.
	if err := d.setAutostopPolicy(ctx, profile, sandboxID, req.ProviderConfig); err != nil {
		return &postVerifyFailure{err: fmt.Errorf(
			"the agent launched and verified successfully, but the autostop policy could not be set: %w; the sandbox remains on the default 10-minute idle autostop — redeploy, or run `databricks sandbox config <sandbox-id> --no-autostop` manually (the sandbox id is in this error's trailing annotation)",
			err,
		)}
	}

	return nil
}

// installAndVerify runs docs/PLAN.md §4.4 step 5: fetch/verify/extract the
// pinned .deb (skipped in-script when the version marker already
// matches), then the ACP initialize runtime-verification handshake
// (docs/M05_PROBE_RESULTS.md §6) as a SINGLE combined round trip (BUG 1 +
// efficiency fix): the agent env content travels over this one
// RunWithStdin call's stdin, is written to a temp file, sourced, and used
// to run the handshake, all inside install.BuildVerifyCommand's script,
// which also removes the temp file on exit via trap. This replaces what
// were previously two round trips (write the temp env file, then a
// separate exec that sourced it) — the old split was also the
// deploy-breaking bug: the write step double-quoted the path (expanding
// $HOME) while the exec step single-quoted it (not expanding), so
// sourcing always failed under `set -eu` and the exec's own trap never
// removed the real file either.
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
	// sourced (docs/M05_PROBE_RESULTS.md §6), env content shipped via
	// stdin only.
	verifyCmd, err := install.BuildVerifyCommand(verifyEnvFilePath, installVerifyTimeoutSeconds)
	if err != nil {
		return fmt.Errorf("install verification: %w", err)
	}
	out, err := d.SSH.RunWithStdin(ctx, profile, sandboxID,
		step("verify-exec", verifyCmd),
		strings.NewReader(envContent),
	)
	if err != nil {
		return fmt.Errorf("install verification: buzz-agent ACP initialize handshake failed: %w", err)
	}
	if !strings.Contains(out, install.AgentInfoMarker) {
		return fmt.Errorf("install verification: buzz-agent ACP initialize response did not contain %q: %s", install.AgentInfoMarker, truncate(strings.TrimSpace(out), maxErrorLogBytes))
	}
	return nil
}

// verifyLaunch implements docs/PLAN.md §4.4 step 10's pass signal, waiting
// VerifyDelay before checking. The process-liveness check and the acp.log
// read are combined into a SINGLE SSH round trip (BUG 2 + BUG 5 + efficiency
// fix): a bare `pgrep -f buzz-acp` (or a separate `cat acp.log` call) would
// self-match this very invocation's own argv over `databricks sandbox ssh
// ... -- <cmd>`, so a dead agent could still pass the liveness check;
// pgrep now uses the non-self-matching '[b]uzz-acp' bracket idiom, its
// exit code is captured and echoed rather than relied on as this call's
// own exit code, and the log is bounded to the last 4KB server-side via
// `tail -c` so it can never blow up an error message (BUG 5).
func (d *Deployer) verifyLaunch(ctx context.Context, profile, sandboxID string) error {
	d.sleep(d.verifyDelay())

	out, err := d.SSH.Run(ctx, profile, sandboxID, step("verify-check",
		`pgrep -f '[b]uzz-acp' >/dev/null 2>&1; echo "BUZZ_PGREP_RC=$?"; tail -c 4096 "$HOME/.buzz-backend/acp.log" 2>/dev/null || true`,
	))
	if err != nil {
		return fmt.Errorf("verify: could not check buzz-acp process/log: %w", err)
	}

	rc, logOut, perr := parsePgrepCheck(out)
	if perr != nil {
		// Inconclusive, not conclusively dead: fail the deploy with a
		// distinct diagnosis rather than pretending the process check
		// itself returned rc=1 (review round 2 — an unparseable output
		// must not masquerade as a confirmed-dead agent).
		return fmt.Errorf("verify: could not parse verification output: %w (output: %s)", perr, truncate(strings.TrimSpace(out), maxErrorLogBytes))
	}
	logOut = truncate(logOut, maxErrorLogBytes)
	if rc != 0 {
		return fmt.Errorf("verify: buzz-acp process not found %s after launch (acp.log: %s)", d.verifyDelay(), strings.TrimSpace(logOut))
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

// parsePgrepCheck splits verify-check's output into the pgrep exit code
// (from its "BUZZ_PGREP_RC=<n>" marker line) and the remaining (already
// tail -c 4096 bounded) acp.log content. Lines are scanned IN ORDER and
// the FIRST line carrying the marker wins (review round 2): the remote
// echo always precedes the log tail, so first-match-in-order tolerates
// any stdout preamble (e.g. a shell profile banner) AND is
// collision-safe against the marker text appearing inside the log
// content itself. Everything after the marker line is the log. When NO
// line carries a parseable marker, a distinct error is returned rather
// than a fabricated rc=1 — an inconclusive check must not masquerade as
// a confirmed-dead agent (it still fails the deploy, with the raw
// output in the diagnosis).
func parsePgrepCheck(out string) (rc int, log string, err error) {
	const prefix = "BUZZ_PGREP_RC="
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		n, aerr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, prefix)))
		if aerr != nil {
			return 0, "", fmt.Errorf("malformed %s marker line %q: %w", prefix, trimmed, aerr)
		}
		return n, strings.Join(lines[i+1:], "\n"), nil
	}
	return 0, "", fmt.Errorf("no %s marker line found in verification output", prefix)
}

// truncate bounds s to at most max bytes (BUG 5 fix: no remote log/output
// may be interpolated into an error string unbounded), appending a marker
// when it cuts content off.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
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
//
// teardown uses its OWN independent context (BUG 4 fix), rather than the
// caller's deploy ctx: when a provisioning step fails BECAUSE the deploy
// deadline expired, the same (already-dead) ctx would make every SSH/CLI
// call here no-op immediately, silently leaking the secret-bearing env
// file and a fresh sandbox. Teardown must be able to run even after the
// deploy deadline.
func (d *Deployer) teardown(profile, sandboxID string, freshlyCreated bool) {
	if sandboxID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()

	shredCmd := step("teardown-shred", fmt.Sprintf(
		`shred -u %s 2>/dev/null || rm -f %s 2>/dev/null; shred -u %s 2>/dev/null || rm -f %s 2>/dev/null; true`,
		dquote(nest.EnvFilePath), dquote(nest.EnvFilePath), dquote(verifyEnvFilePath), dquote(verifyEnvFilePath),
	))
	_, _ = d.SSH.Run(ctx, profile, sandboxID, shredCmd)

	if freshlyCreated {
		_ = d.CLI.SandboxDelete(ctx, profile, sandboxID)
		return
	}
	// '[b]uzz-acp', not 'buzz-acp' (BUG 2 fix, applied here too for the
	// identical self-match reason as the step-8 prelaunch-kill above):
	// otherwise this teardown pkill could SIGTERM its own invoking shell
	// before ever reaching the real buzz-acp process, defeating the
	// "reused sandbox unhealthy — kill the lingering agent" teardown
	// intent.
	_, _ = d.SSH.Run(ctx, profile, sandboxID, step("teardown-pkill", `pkill -f '[b]uzz-acp' 2>/dev/null; true`))
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
//
// dquote is for TRUSTED static "$HOME"-relative literals ONLY — never
// payload/untrusted data. Untrusted or secret-adjacent data must go
// through internal/shellquote.Single instead (or, better, over stdin per
// internal/sshx's doc comment).
func dquote(s string) string {
	return `"` + s + `"`
}
