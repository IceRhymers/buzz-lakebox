// Operator lifecycle operations (docs/PLAN.md M2 interim): start,
// status, logs, stop for an already-deployed agent sandbox. These are
// CLI-only until Buzz's provider protocol grows the matching v2 ops —
// the desktop cannot invoke them, so they exist to make recovery a
// one-command affair instead of a raw `databricks sandbox` + SSH dance.
//
// Design constraints inherited from the deploy flow:
//   - stopping a sandbox kills every process and nothing inside
//     relaunches buzz-acp (no boot hooks — lane D §5), so Start must
//     rerun launch.sh after the sandbox reaches Running;
//   - launch.sh is the single relaunch entrypoint (PLAN §4.4 step 9)
//     and is only written by deploy, so Start on a never-deployed
//     sandbox fails with a pointer to deploy instead of inventing a
//     second provisioning path.
package deployflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/IceRhymers/buzz-lakebox/internal/lakebox"
	"github.com/IceRhymers/buzz-lakebox/internal/nest"
)

// lifecycleTimeout bounds each operator lifecycle command. Start is the
// slowest (sandbox start ~20.5s + VerifyDelay), so one generous bound
// covers all four.
const lifecycleTimeout = 5 * time.Minute

// noLaunchScriptMarker is echoed by Start's remote script when
// launch.sh is absent, so the Go side can distinguish "never deployed"
// from a genuine launch failure without a second round trip.
const noLaunchScriptMarker = "BUZZ_NO_LAUNCH_SH=1"

// lifecycleErr is the single redaction boundary for every operator
// lifecycle op (Start/Status/Logs/Stop/Undeploy), mirroring the one
// Deploy has at its own package boundary (deployflow.go's Deploy
// wrapping deploy()). Unlike Deploy, these commands have no payload to
// derive known secrets from — remoteText's floor (bare nsec tokens,
// credential-shaped NAME=value pairs) is what stands between an
// unbounded, unscrubbed remote error and the operator's terminal. In
// particular, CodeLaunchExec wraps the raw sshx error, which embeds the
// sandbox's full, unbounded, unscrubbed combined stdout+stderr
// (internal/sshx's run) — without this boundary that text would reach
// the CLI verbatim.
//
// Redacted preserves the taxonomy code across the scrub (CodeOf finds
// it via errors.As), so callers can still match on .Code after this
// rewrite. Each lifecycle method applies this via a single `defer`
// at its own top, so no individual return statement has to remember to
// scrub.
func lifecycleErr(err error) error {
	if err == nil {
		return nil
	}
	return Redacted(CodeOf(err), remoteText(err.Error()))
}

// AgentStatus is the operator-facing snapshot Status returns (and the
// `status` subcommand prints as JSON).
type AgentStatus struct {
	SandboxID     string `json:"sandbox_id"`
	SandboxStatus string `json:"sandbox_status"`
	// AcpRunning reports whether a buzz-acp process is alive inside the
	// sandbox. Only probed when the sandbox is Running (SSH requires a
	// running sandbox); false otherwise.
	AcpRunning bool `json:"acp_running"`
	// LogTail is the last 4KB of acp.log when it could be read.
	LogTail string `json:"log_tail,omitempty"`
}

// Start recovers a dead agent: ensure the sandbox is Running (start +
// wait if needed), rerun launch.sh (idempotent — flock/pgrep guarded,
// no-op when buzz-acp is already alive), then run the same launch
// verification as deploy step 10.
func (d *Deployer) Start(profile, sandboxID string) (err error) {
	defer func() { err = lifecycleErr(err) }()

	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
	defer cancel()

	sb, err := d.CLI.SandboxStatus(ctx, profile, sandboxID)
	if err != nil {
		return failf(CodeSandboxStatus, "sandbox status: %w", err)
	}
	if !strings.EqualFold(sb.Status, lakebox.StatusRunning) {
		if err := d.CLI.SandboxStart(ctx, profile, sandboxID); err != nil {
			return failf(CodeSandboxStart, "sandbox start: %w", err)
		}
		if err := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); err != nil {
			return failf(CodeSandboxWait, "waiting for sandbox to reach Running: %w", err)
		}
	}

	// Single round trip: fail with a marker when launch.sh is missing
	// (never-deployed sandbox), else run it. launch.sh re-asserts the
	// PAT stub, sources the env file, and setsid-nohups buzz-acp.
	out, err := d.SSH.Run(ctx, profile, sandboxID, step("start-launch", fmt.Sprintf(
		`set -eu; [ -f %s ] || { echo "%s"; exit 9; }; sh %s`,
		dquote(nest.LaunchScriptPath), noLaunchScriptMarker, dquote(nest.LaunchScriptPath),
	)))
	if err != nil {
		if strings.Contains(out, noLaunchScriptMarker) || strings.Contains(err.Error(), noLaunchScriptMarker) {
			return failf(CodeNotDeployed, "no launch.sh in this sandbox — it has never been provisioned by a deploy; run a deploy first")
		}
		return failf(CodeLaunchExec, "run launch.sh: %w", err)
	}

	if err := d.verifyLaunch(ctx, profile, sandboxID); err != nil {
		return err
	}
	return nil
}

// Status reports the sandbox state plus in-sandbox agent liveness (the
// same non-self-matching pgrep + bounded log tail as deploy's verify).
func (d *Deployer) Status(profile, sandboxID string) (st AgentStatus, err error) {
	defer func() { err = lifecycleErr(err) }()

	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
	defer cancel()

	st = AgentStatus{SandboxID: sandboxID}
	sb, err := d.CLI.SandboxStatus(ctx, profile, sandboxID)
	if err != nil {
		return st, failf(CodeSandboxStatus, "sandbox status: %w", err)
	}
	st.SandboxStatus = sb.Status
	if !strings.EqualFold(sb.Status, lakebox.StatusRunning) {
		return st, nil // stopped sandbox: no processes, nothing to SSH into
	}

	out, err := d.SSH.Run(ctx, profile, sandboxID, step("status-check", acpLivenessProbe()))
	if err != nil {
		return st, failf(CodeStatusProbe, "status: could not check buzz-acp process/log: %w", err)
	}
	rc, logOut, perr := parsePgrepCheck(out)
	if perr != nil {
		// CodeStatusUnparseable, NOT CodeVerifyUnparseable: that code's
		// remedy tells the operator to run `status` and `logs` — circular
		// when the failure came from status itself.
		return st, failf(CodeStatusUnparseable, "status: could not parse check output: %w (output: %s)", perr, remoteText(strings.TrimSpace(out)))
	}
	st.AcpRunning = rc == 0
	st.LogTail = remoteText(strings.TrimSpace(logOut))
	return st, nil
}

// Logs returns up to tailBytes of the sandbox's acp.log (bounded
// server-side via tail -c, mirroring deploy's BUG 5 discipline).
func (d *Deployer) Logs(profile, sandboxID string, tailBytes int) (out string, err error) {
	defer func() { err = lifecycleErr(err) }()

	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
	defer cancel()

	if tailBytes <= 0 {
		tailBytes = 4096
	}
	out, err = d.SSH.Run(ctx, profile, sandboxID, step("logs-tail", fmt.Sprintf(
		`tail -c %d "$HOME/.buzz-backend/acp.log"`, tailBytes,
	)))
	if err != nil {
		return "", failf(CodeLogsRead, "read acp.log: %w", err)
	}
	return remoteText(out), nil
}

// Stop stops the sandbox's compute. All in-sandbox processes die and
// the agent goes offline on the relay; $HOME persists, so Start
// recovers it.
func (d *Deployer) Stop(profile, sandboxID string) (err error) {
	defer func() { err = lifecycleErr(err) }()

	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
	defer cancel()
	if err := d.CLI.SandboxStop(ctx, profile, sandboxID); err != nil {
		return failf(CodeSandboxStop, "sandbox stop: %w", err)
	}
	return nil
}

// UndeployResult reports what Undeploy actually did, so the CLI can tell
// the operator whether the secret shred ran (it cannot on an already
// stopped sandbox), whether a reuse mapping was dropped, and whether a
// dropped mapping left residue behind.
type UndeployResult struct {
	SandboxID string `json:"sandbox_id"`
	// Shredded is true when the in-sandbox secret shred ran. False means
	// the sandbox was not Running, so no SSH was possible — the secrets
	// die with the sandbox's storage on delete instead.
	Shredded bool `json:"shredded"`
	// StateEntriesRemoved counts reuse mappings dropped from the
	// provider's state file.
	StateEntriesRemoved int `json:"state_entries_removed"`
	// StateResidue is set when the sandbox was successfully deleted but
	// its reuse mapping could not be dropped afterward. Non-fatal by
	// design (see the Undeploy doc comment): the delete already
	// succeeded and is irreversible, so this field exists to surface the
	// residue to the operator, not to signal that Undeploy failed.
	StateResidue string `json:"state_residue,omitempty"`
}

// Undeploy is the destructive teardown of a deployed agent (docs/PLAN.md
// §3.2, §6 M2): shred the in-sandbox secrets first, then delete the
// sandbox, then forget the reuse mapping.
//
// Order matters and is the inverse of deploy's: the shred is
// best-effort and must be attempted while the sandbox can still be
// reached, but a shred failure must NOT abort the delete — an
// undeleted sandbox keeps billing and keeps the nsec on disk, which is
// strictly worse than an unshredded one that gets destroyed with its
// storage. The state mapping is dropped last, and only after the delete
// succeeds: a mapping outliving a failed delete still points at a real
// sandbox that a redeploy should reuse rather than orphan.
//
// A ForgetSandbox failure AFTER a successful delete is likewise
// non-fatal: the sandbox is already gone, so there is no still-billing
// resource left to protect by failing this call, and a stale mapping is
// self-healing (the next deploy's status probe rejects a mapping
// pointing at a deleted sandbox). Undeploy therefore returns a nil
// error in that case and reports the residue via
// UndeployResult.StateResidue instead — an error return here would tell
// the caller the deletion itself failed, which would be false.
func (d *Deployer) Undeploy(profile, sandboxID string) (res UndeployResult, err error) {
	defer func() { err = lifecycleErr(err) }()

	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
	defer cancel()

	res = UndeployResult{SandboxID: sandboxID}

	// Only a Running sandbox can be SSH'd into. A status failure is not
	// fatal here: the delete below is what actually protects the owner,
	// so treat "cannot tell" as "cannot shred" and carry on.
	if sb, err := d.CLI.SandboxStatus(ctx, profile, sandboxID); err == nil && strings.EqualFold(sb.Status, lakebox.StatusRunning) {
		if _, err := d.SSH.Run(ctx, profile, sandboxID, step("undeploy-shred", secretShredCommand())); err == nil {
			res.Shredded = true
		}
	}

	if err := d.CLI.SandboxDelete(ctx, profile, sandboxID); err != nil {
		return res, failf(CodeSandboxDelete, "sandbox delete: %w", err)
	}

	if d.State != nil {
		removed, ferr := d.State.ForgetSandbox(profile, sandboxID)
		if ferr != nil {
			// The sandbox is already gone; a stale mapping is
			// self-healing (the next deploy's status probe rejects it),
			// so report the residue rather than failing the undeploy —
			// the delete already succeeded and is irreversible.
			res.StateResidue = remoteText(ferr.Error())
			return res, nil
		}
		res.StateEntriesRemoved = removed
	}

	return res, nil
}
