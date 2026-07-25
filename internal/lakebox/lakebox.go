// Package lakebox wraps the stock `databricks` CLI's `sandbox` command
// group (PLAN.md §3.1: shell out to the CLI, never raw REST). At M0 this is
// limited to the version-gate helper doctor needs; deploy orchestration
// (M1) will extend CLI with sandbox create/list/start/ssh/etc methods.
package lakebox

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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

// SandboxList runs `databricks sandbox list -p <profile>` to confirm the
// sandbox command group is reachable (i.e. Lakebox is enabled/region-gated
// in for this profile's workspace). Returns the raw command output and any
// error.
func (c *CLI) SandboxList(ctx context.Context, profile string) (string, error) {
	cmd := exec.CommandContext(ctx, c.binName(), "sandbox", "list", "-p", profile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("run %s sandbox list -p %s: %w", c.binName(), profile, err)
	}
	return string(out), nil
}

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
