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
func (d *Deployer) Start(profile, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
	defer cancel()

	sb, err := d.CLI.SandboxStatus(ctx, profile, sandboxID)
	if err != nil {
		return fmt.Errorf("sandbox status: %w", err)
	}
	if !strings.EqualFold(sb.Status, lakebox.StatusRunning) {
		if err := d.CLI.SandboxStart(ctx, profile, sandboxID); err != nil {
			return fmt.Errorf("sandbox start: %w", err)
		}
		if err := d.CLI.WaitRunning(ctx, profile, sandboxID, d.waitTimeout(), d.pollInterval(), d.Sleep); err != nil {
			return fmt.Errorf("waiting for sandbox to reach Running: %w", err)
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
			return fmt.Errorf("no launch.sh in this sandbox — it has never been provisioned by a deploy; run a deploy first (from Buzz Desktop, or `deploy --payload-file`)")
		}
		return fmt.Errorf("run launch.sh: %w", err)
	}

	if err := d.verifyLaunch(ctx, profile, sandboxID); err != nil {
		return err
	}
	return nil
}

// Status reports the sandbox state plus in-sandbox agent liveness (the
// same non-self-matching pgrep + bounded log tail as deploy's verify).
func (d *Deployer) Status(profile, sandboxID string) (AgentStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
	defer cancel()

	st := AgentStatus{SandboxID: sandboxID}
	sb, err := d.CLI.SandboxStatus(ctx, profile, sandboxID)
	if err != nil {
		return st, fmt.Errorf("sandbox status: %w", err)
	}
	st.SandboxStatus = sb.Status
	if !strings.EqualFold(sb.Status, lakebox.StatusRunning) {
		return st, nil // stopped sandbox: no processes, nothing to SSH into
	}

	out, err := d.SSH.Run(ctx, profile, sandboxID, step("status-check",
		`pgrep -f '[b]uzz-acp' >/dev/null 2>&1; echo "BUZZ_PGREP_RC=$?"; tail -c 4096 "$HOME/.buzz-backend/acp.log" 2>/dev/null || true`,
	))
	if err != nil {
		return st, fmt.Errorf("status: could not check buzz-acp process/log: %w", err)
	}
	rc, logOut, perr := parsePgrepCheck(out)
	if perr != nil {
		return st, fmt.Errorf("status: could not parse check output: %w (output: %s)", perr, truncate(strings.TrimSpace(out), maxErrorLogBytes))
	}
	st.AcpRunning = rc == 0
	st.LogTail = truncate(strings.TrimSpace(logOut), maxErrorLogBytes)
	return st, nil
}

// Logs returns up to tailBytes of the sandbox's acp.log (bounded
// server-side via tail -c, mirroring deploy's BUG 5 discipline).
func (d *Deployer) Logs(profile, sandboxID string, tailBytes int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
	defer cancel()

	if tailBytes <= 0 {
		tailBytes = 4096
	}
	out, err := d.SSH.Run(ctx, profile, sandboxID, step("logs-tail", fmt.Sprintf(
		`tail -c %d "$HOME/.buzz-backend/acp.log"`, tailBytes,
	)))
	if err != nil {
		return "", fmt.Errorf("read acp.log: %w", err)
	}
	return out, nil
}

// Stop stops the sandbox's compute. All in-sandbox processes die and
// the agent goes offline on the relay; $HOME persists, so Start
// recovers it.
func (d *Deployer) Stop(profile, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
	defer cancel()
	if err := d.CLI.SandboxStop(ctx, profile, sandboxID); err != nil {
		return err
	}
	return nil
}
