// Package lakebox wraps the stock `databricks` CLI's `sandbox` command
// group (PLAN.md §3.1: shell out to the CLI, never raw REST). At M0 this is
// limited to the version-gate helper doctor needs; deploy orchestration
// (M1) will extend CLI with sandbox create/list/start/ssh/etc methods.
package lakebox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinCLIVersion is the minimum `databricks` CLI version this provider
// supports, live-verified with the full `sandbox` command group
// (docs/CONTRACT.md §7).
const MinCLIVersion = "1.8.0"

// defaultBin is the binary name resolved via PATH when CLI.Bin is unset.
const defaultBin = "databricks"

// CLI is a thin exec wrapper over the `databricks` binary. Bin is
// exported so tests can point it at a fake PATH shim instead of a real
// installation (PLAN.md §7).
type CLI struct {
	// Bin is the binary name or path invoked for every command. Empty
	// means "databricks" resolved via PATH.
	Bin string

	versionOnce sync.Once
	versionVal  string
	versionErr  error
}

// New returns a CLI that invokes the real "databricks" binary on PATH.
func New() *CLI {
	return &CLI{Bin: defaultBin}
}

func (c *CLI) binName() string {
	if c.Bin == "" {
		return defaultBin
	}
	return c.Bin
}

// LookPath resolves the configured binary on PATH, returning its absolute
// path or the underlying exec.ErrNotFound-wrapping error.
func (c *CLI) LookPath() (string, error) {
	path, err := exec.LookPath(c.binName())
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH: %w", c.binName(), err)
	}
	return path, nil
}

// versionPattern extracts the first semver-shaped X.Y.Z token from
// `databricks version` output, tolerating a leading "v" and surrounding
// text (actual CLI output format is not itself part of the frozen
// contract, only the semver it embeds).
var versionPattern = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

// Version runs `databricks version` and returns the parsed semver string
// (without a leading "v"), e.g. "1.8.0".
func (c *CLI) Version(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, c.binName(), "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run %s version: %w (output: %s)", c.binName(), err, strings.TrimSpace(string(out)))
	}
	return parseVersion(string(out))
}

func parseVersion(output string) (string, error) {
	m := versionPattern.FindStringSubmatch(output)
	if m == nil {
		return "", fmt.Errorf("no semver version found in output: %q", strings.TrimSpace(output))
	}
	return fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3]), nil
}

// CurrentUser runs `databricks current-user me -p <profile>` to confirm the
// profile resolves to valid, authenticated credentials. Returns the raw
// command output (for diagnostics) and any error.
func (c *CLI) CurrentUser(ctx context.Context, profile string) (string, error) {
	cmd := exec.CommandContext(ctx, c.binName(), "current-user", "me", "-p", profile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("run %s current-user me -p %s: %w", c.binName(), profile, err)
	}
	return string(out), nil
}

// SandboxList is defined further below with a richer, typed signature
// (M1 deliverable 1) that both doctor's reachability check and
// deployflow's reuse-or-create logic depend on.

// CompareSemver compares two "X.Y.Z" semver strings, returning -1, 0, or 1
// as a < b, a == b, or a > b (numeric component comparison; no pre-release
// or build-metadata handling, which the CLI version string doesn't use).
func CompareSemver(a, b string) (int, error) {
	pa, err := splitSemver(a)
	if err != nil {
		return 0, err
	}
	pb, err := splitSemver(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func splitSemver(v string) ([3]int, error) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) != 3 {
		return out, fmt.Errorf("invalid semver %q: want X.Y.Z", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, fmt.Errorf("invalid semver %q: component %q not numeric: %w", v, p, err)
		}
		out[i] = n
	}
	return out, nil
}

// MeetsMinVersion reports whether version satisfies MinCLIVersion.
func MeetsMinVersion(version string) (bool, error) {
	cmp, err := CompareSemver(version, MinCLIVersion)
	if err != nil {
		return false, err
	}
	return cmp >= 0, nil
}

// Sandbox is the subset of `databricks sandbox` JSON fields (list/create/
// status) this provider depends on. Field casing follows the `--json`
// shape cited in docs/M05_PROBE_RESULTS.md / docs/PLAN.md §7
// ("{sandboxId, status, gatewayHost, name, idleTimeout, noAutostop}");
// this is diagnosable-from-output rather than exhaustively live-verified,
// per docs/PLAN.md §3.1's "shape drift is diagnosable from a single
// report" mitigation — every error embeds the recorded CLI version.
type Sandbox struct {
	ID     string `json:"sandboxId"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// StatusRunning is the Sandbox.Status value meaning the sandbox is up and
// reachable over SSH.
const StatusRunning = "Running"

// CachedVersion returns the CLI version string, fetching and caching it on
// first call (M1 deliverable 1: "every method records the CLI version
// string for error context; fetch once, cache on the struct"). Exported
// so callers other than wrapErr (e.g. internal/deployflow's preflight
// check) share the same sync.Once cache rather than each spawning their
// own `databricks version` subprocess (BUG 9 fix: deployflow used to call
// Version(ctx) directly, bypassing this cache entirely, causing a
// redundant subprocess spawn on top of whatever wrapErr later spawned).
func (c *CLI) CachedVersion(ctx context.Context) (string, error) {
	c.versionOnce.Do(func() {
		c.versionVal, c.versionErr = c.Version(ctx)
	})
	return c.versionVal, c.versionErr
}

// cachedVersion returns the cached CLI version string, degrading to
// "unknown" rather than masking the original error a caller is trying to
// report.
func (c *CLI) cachedVersion(ctx context.Context) string {
	v, err := c.CachedVersion(ctx)
	if err != nil || v == "" {
		return "unknown"
	}
	return v
}

// wrapErr annotates err with the recorded CLI version for diagnosability
// (docs/PLAN.md §3.1, §4.3: "every error message embeds ... the recorded
// CLI version"). This is the SINGLE stamper of CLI version onto error
// text (BUG 9 fix): callers (e.g. internal/deployflow.wrap) must not
// stamp their own version annotation on top of this one, to avoid
// duplicated "(databricks cli X)" text.
func (c *CLI) wrapErr(ctx context.Context, err error, action string) error {
	return fmt.Errorf("%s: %w (databricks cli %s)", action, err, c.cachedVersion(ctx))
}

func (c *CLI) runCombined(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.binName(), args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runSplit runs the CLI capturing stdout and stderr SEPARATELY (BUG 8
// fix): the real CLI intermittently prints advisory lines to stderr
// (observed live: "Databricks skills are not installed..."), which would
// otherwise interleave into and corrupt --json stdout when captured via
// CombinedOutput/runCombined. JSON-parsing callers must parse stdout only
// and use stderr purely for error diagnostics.
func (c *CLI) runSplit(ctx context.Context, args ...string) (stdout string, stderr string, err error) {
	cmd := exec.CommandContext(ctx, c.binName(), args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// looksLikeUnsupportedFlag heuristically detects a CLI error caused by an
// unsupported `--json` flag, so callers can fall back to parsing table
// output (M1 deliverable 1: "prefer --json flags where the CLI accepts
// them and fall back cleanly").
func looksLikeUnsupportedFlag(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "unknown flag") || strings.Contains(lower, "unknown shorthand flag")
}

// SandboxRegister runs `databricks sandbox register -p <profile>`. It is
// idempotent at the CLI/server level (docs/PLAN.md §4.4 step 2); an
// "already registered"-shaped message on a non-zero exit is tolerated
// rather than treated as failure, in case a given CLI version reports
// idempotent no-ops that way.
func (c *CLI) SandboxRegister(ctx context.Context, profile string) error {
	out, err := c.runCombined(ctx, "sandbox", "register", "-p", profile)
	if err != nil && !strings.Contains(strings.ToLower(out), "already registered") {
		return c.wrapErr(ctx, fmt.Errorf("sandbox register -p %s: %w (output: %s)", profile, err, strings.TrimSpace(out)), "sandbox register")
	}
	return nil
}

// SandboxCreate runs `databricks sandbox create <name> --json -p <profile>`
// and parses the resulting Sandbox. Parses stdout only (BUG 8 fix): a
// stray advisory line on stderr must never corrupt --json stdout parsing.
func (c *CLI) SandboxCreate(ctx context.Context, profile, name string) (Sandbox, error) {
	stdout, stderr, err := c.runSplit(ctx, "sandbox", "create", name, "--json", "-p", profile)
	if err != nil {
		return Sandbox{}, c.wrapErr(ctx, fmt.Errorf("sandbox create %s -p %s: %w (stdout: %s, stderr: %s)", name, profile, err, strings.TrimSpace(stdout), strings.TrimSpace(stderr)), "sandbox create")
	}
	var sb Sandbox
	if jerr := json.Unmarshal([]byte(stdout), &sb); jerr != nil {
		return Sandbox{}, c.wrapErr(ctx, fmt.Errorf("parse sandbox create --json output: %w (stdout: %s, stderr: %s)", jerr, strings.TrimSpace(stdout), strings.TrimSpace(stderr)), "sandbox create")
	}
	return sb, nil
}

// SandboxList runs `databricks sandbox list --json -p <profile>`, falling
// back to parsing whitespace-table output if the installed CLI doesn't
// accept --json for this subcommand. Returns the parsed sandboxes and the
// raw stdout (for diagnostics, e.g. doctor's reachability check). Parses
// stdout only (BUG 8 fix): a stray advisory line on stderr (observed
// live: "Databricks skills are not installed...") must never corrupt
// --json stdout parsing; a literal `null` stdout (observed live for an
// empty list) is tolerated as an empty slice by json.Unmarshal.
func (c *CLI) SandboxList(ctx context.Context, profile string) ([]Sandbox, string, error) {
	stdout, stderr, err := c.runSplit(ctx, "sandbox", "list", "--json", "-p", profile)
	if err != nil {
		if looksLikeUnsupportedFlag(stdout + stderr) {
			tableOut, tableErr, tErr := c.runSplit(ctx, "sandbox", "list", "-p", profile)
			if tErr != nil {
				return nil, tableOut, c.wrapErr(ctx, fmt.Errorf("sandbox list -p %s: %w (stdout: %s, stderr: %s)", profile, tErr, strings.TrimSpace(tableOut), strings.TrimSpace(tableErr)), "sandbox list")
			}
			sbs, pErr := parseSandboxTable(tableOut)
			if pErr != nil {
				return nil, tableOut, c.wrapErr(ctx, pErr, "sandbox list: parse table output")
			}
			return sbs, tableOut, nil
		}
		return nil, stdout, c.wrapErr(ctx, fmt.Errorf("sandbox list --json -p %s: %w (stdout: %s, stderr: %s)", profile, err, strings.TrimSpace(stdout), strings.TrimSpace(stderr)), "sandbox list")
	}
	sbs, pErr := parseSandboxArray(stdout)
	if pErr != nil {
		return nil, stdout, c.wrapErr(ctx, pErr, "sandbox list: parse json output")
	}
	return sbs, stdout, nil
}

func parseSandboxArray(out string) ([]Sandbox, error) {
	var sbs []Sandbox
	if err := json.Unmarshal([]byte(out), &sbs); err != nil {
		return nil, fmt.Errorf("parse sandbox list --json output: %w (output: %s)", err, strings.TrimSpace(out))
	}
	return sbs, nil
}

// parseSandboxTable parses the fallback plain-text `sandbox list` output:
// one header line followed by whitespace-separated columns, the first
// three of which are assumed to be ID, NAME, STATUS (best-effort; the
// exact column layout of a table-mode CLI is not itself part of the
// frozen contract).
func parseSandboxTable(out string) ([]Sandbox, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var sbs []Sandbox
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // header line / blank line
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		sbs = append(sbs, Sandbox{ID: fields[0], Name: fields[1], Status: fields[2]})
	}
	return sbs, nil
}

// SandboxStatus runs `databricks sandbox status <id> --json -p <profile>`
// and parses the resulting Sandbox. Parses stdout only (BUG 8 fix): a
// stray advisory line on stderr must never corrupt --json stdout parsing.
func (c *CLI) SandboxStatus(ctx context.Context, profile, id string) (Sandbox, error) {
	stdout, stderr, err := c.runSplit(ctx, "sandbox", "status", id, "--json", "-p", profile)
	if err != nil {
		return Sandbox{}, c.wrapErr(ctx, fmt.Errorf("sandbox status %s -p %s: %w (stdout: %s, stderr: %s)", id, profile, err, strings.TrimSpace(stdout), strings.TrimSpace(stderr)), "sandbox status")
	}
	var sb Sandbox
	if jerr := json.Unmarshal([]byte(stdout), &sb); jerr != nil {
		return Sandbox{}, c.wrapErr(ctx, fmt.Errorf("parse sandbox status --json output: %w (stdout: %s, stderr: %s)", jerr, strings.TrimSpace(stdout), strings.TrimSpace(stderr)), "sandbox status")
	}
	return sb, nil
}

// SandboxStart runs `databricks sandbox start <id> -p <profile>`. Callers
// should follow this with WaitRunning (docs/PLAN.md §4.1: "start and wait
// Running").
func (c *CLI) SandboxStart(ctx context.Context, profile, id string) error {
	out, err := c.runCombined(ctx, "sandbox", "start", id, "-p", profile)
	if err != nil {
		return c.wrapErr(ctx, fmt.Errorf("sandbox start %s -p %s: %w (output: %s)", id, profile, err, strings.TrimSpace(out)), "sandbox start")
	}
	return nil
}

// WaitRunning polls SandboxStatus until it reports StatusRunning, timeout
// elapses, or ctx is cancelled. sleep is injectable so tests can avoid
// real wall-clock waits (PLAN.md §7: fake shim, no real infra/timing).
func (c *CLI) WaitRunning(ctx context.Context, profile, id string, timeout, pollInterval time.Duration, sleep func(time.Duration)) error {
	if sleep == nil {
		sleep = time.Sleep
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		sb, err := c.SandboxStatus(ctx, profile, id)
		if err != nil {
			return err
		}
		if strings.EqualFold(sb.Status, StatusRunning) {
			return nil
		}
		if time.Now().After(deadline) {
			return c.wrapErr(ctx, fmt.Errorf("sandbox %s did not reach %s within %s (last status %q)", id, StatusRunning, timeout, sb.Status), "sandbox wait-running")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		sleep(pollInterval)
	}
}

// SandboxDelete runs `databricks sandbox delete <id> --auto-approve -p
// <profile>` (docs/PLAN.md §4.3 failure teardown / M2 undeploy).
func (c *CLI) SandboxDelete(ctx context.Context, profile, id string) error {
	out, err := c.runCombined(ctx, "sandbox", "delete", id, "--auto-approve", "-p", profile)
	if err != nil {
		return c.wrapErr(ctx, fmt.Errorf("sandbox delete %s -p %s: %w (output: %s)", id, profile, err, strings.TrimSpace(out)), "sandbox delete")
	}
	return nil
}

// SandboxConfigOptions selects the autostop policy applied by
// SandboxConfig (docs/PLAN.md §4.4 step 10). Exactly one of NoAutostop or
// IdleTimeout should be set by the caller; NoAutostop wins if both are.
type SandboxConfigOptions struct {
	NoAutostop  bool
	IdleTimeout string
}

// SandboxConfig runs `databricks sandbox config <id> -p <profile>` with
// either `--no-autostop` or `--idle-timeout <v>` per opts.
func (c *CLI) SandboxConfig(ctx context.Context, profile, id string, opts SandboxConfigOptions) error {
	args := []string{"sandbox", "config", id}
	switch {
	case opts.NoAutostop:
		args = append(args, "--no-autostop")
	case opts.IdleTimeout != "":
		args = append(args, "--idle-timeout", opts.IdleTimeout)
	default:
		return fmt.Errorf("sandbox config %s: one of NoAutostop or IdleTimeout must be set", id)
	}
	args = append(args, "-p", profile)

	out, err := c.runCombined(ctx, args...)
	if err != nil {
		return c.wrapErr(ctx, fmt.Errorf("%s: %w (output: %s)", strings.Join(append([]string{c.binName()}, args...), " "), err, strings.TrimSpace(out)), "sandbox config")
	}
	return nil
}
