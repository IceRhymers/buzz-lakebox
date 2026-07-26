// Package doctor implements the `doctor` operator subcommand: a
// non-destructive, read-only sequence of checks that a Databricks
// environment is ready for this provider (PLAN.md §6 M0).
package doctor

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/lakebox"
)

// Status is the outcome of a single check.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
)

// Check is one doctor check's structured result, included in the final
// JSON summary line.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Summary is the final JSON line doctor prints after its human-readable
// output.
type Summary struct {
	Ok         bool    `json:"ok"`
	Profile    string  `json:"profile"`
	CLIPath    string  `json:"cli_path,omitempty"`
	CLIVersion string  `json:"cli_version,omitempty"`
	Checks     []Check `json:"checks"`
}

const skippedNoCLI = "skipped: databricks CLI not found"

// Run executes the doctor checks in order — CLI on PATH, version gate,
// profile resolution, sandbox command group reachability — writing
// human-readable progress lines to w as it goes. Each check degrades
// gracefully: a failure is reported with guidance and doctor continues to
// the next check where meaningful (PLAN.md §6 M0). Callers print the
// returned Summary as the final JSON line.
func Run(ctx context.Context, w io.Writer, cli *lakebox.CLI, profile string) Summary {
	summary := Summary{Profile: profile, Ok: true}

	path, err := cli.LookPath()
	if err != nil {
		_, _ = fmt.Fprintf(w, "FAIL databricks CLI: not found on PATH (%v)\n", err)
		_, _ = fmt.Fprintf(w, "     guidance: install the Databricks CLI (>= %s) and ensure it is on PATH: https://docs.databricks.com/dev-tools/cli/install.html\n", lakebox.MinCLIVersion)
		summary.Ok = false
		summary.Checks = append(summary.Checks,
			Check{Name: "cli_on_path", Status: StatusFail, Detail: err.Error()},
			Check{Name: "cli_version", Status: StatusFail, Detail: skippedNoCLI},
			Check{Name: "profile_resolves", Status: StatusFail, Detail: skippedNoCLI},
			Check{Name: "sandbox_group", Status: StatusFail, Detail: skippedNoCLI},
		)
		return summary
	}
	_, _ = fmt.Fprintf(w, "PASS databricks CLI found at %s\n", path)
	summary.CLIPath = path
	summary.Checks = append(summary.Checks, Check{Name: "cli_on_path", Status: StatusPass, Detail: path})

	if v, err := cli.Version(ctx); err != nil {
		_, _ = fmt.Fprintf(w, "FAIL databricks CLI version: could not determine version (%v)\n", err)
		summary.Ok = false
		summary.Checks = append(summary.Checks, Check{Name: "cli_version", Status: StatusFail, Detail: err.Error()})
	} else {
		summary.CLIVersion = v
		meets, cmpErr := lakebox.MeetsMinVersion(v)
		switch {
		case cmpErr != nil:
			_, _ = fmt.Fprintf(w, "FAIL databricks CLI version: %v\n", cmpErr)
			summary.Ok = false
			summary.Checks = append(summary.Checks, Check{Name: "cli_version", Status: StatusFail, Detail: cmpErr.Error()})
		case !meets:
			_, _ = fmt.Fprintf(w, "FAIL databricks CLI version %s is below the minimum %s; upgrade the CLI\n", v, lakebox.MinCLIVersion)
			summary.Ok = false
			summary.Checks = append(summary.Checks, Check{Name: "cli_version", Status: StatusFail, Detail: fmt.Sprintf("have %s, want >= %s", v, lakebox.MinCLIVersion)})
		default:
			_, _ = fmt.Fprintf(w, "PASS databricks CLI version %s (>= %s)\n", v, lakebox.MinCLIVersion)
			summary.Checks = append(summary.Checks, Check{Name: "cli_version", Status: StatusPass, Detail: v})
		}
	}

	if _, err := cli.CurrentUser(ctx, profile); err != nil {
		_, _ = fmt.Fprintf(w, "FAIL profile %q does not resolve: %v\n", profile, err)
		_, _ = fmt.Fprintf(w, "     guidance: check ~/.databrickscfg has a [%s] profile with valid credentials (`databricks auth login -p %s`)\n", profile, profile)
		summary.Ok = false
		summary.Checks = append(summary.Checks, Check{Name: "profile_resolves", Status: StatusFail, Detail: err.Error()})
	} else {
		_, _ = fmt.Fprintf(w, "PASS profile %q resolves\n", profile)
		summary.Checks = append(summary.Checks, Check{Name: "profile_resolves", Status: StatusPass})
	}

	if _, out, err := cli.SandboxList(ctx, profile); err != nil {
		_, _ = fmt.Fprintf(w, "FAIL sandbox command group unreachable for profile %q: %v\n", profile, err)
		_, _ = fmt.Fprintf(w, "     guidance: Lakebox Sandboxes is a region-gated Beta feature (PLAN.md §4.4 step 2); confirm your workspace's region and Beta enrollment. CLI output: %s\n", strings.TrimSpace(out))
		summary.Ok = false
		summary.Checks = append(summary.Checks, Check{Name: "sandbox_group", Status: StatusFail, Detail: err.Error()})
	} else {
		_, _ = fmt.Fprintf(w, "PASS sandbox command group reachable for profile %q\n", profile)
		summary.Checks = append(summary.Checks, Check{Name: "sandbox_group", Status: StatusPass})
	}

	return summary
}
