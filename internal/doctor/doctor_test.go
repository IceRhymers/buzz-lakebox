package doctor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/lakebox"
)

// writeFakeDatabricks mirrors internal/lakebox's test shim: a shell script
// standing in for the real `databricks` CLI, controlled via FAKE_* env
// vars, so doctor's checks can be exercised without a real installation
// (PLAN.md §7).
func writeFakeDatabricks(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake databricks shim is a POSIX shell script")
	}
	script := `#!/bin/sh
case "$1" in
  version)
    echo "Databricks CLI v${FAKE_VERSION:-1.9.0}"
    exit 0
    ;;
  current-user)
    exit "${FAKE_CURRENT_USER_EXIT:-0}"
    ;;
  sandbox)
    if [ "$2" = "list" ]; then
      if [ "${FAKE_SANDBOX_LIST_EXIT:-0}" != "0" ]; then
        echo "Error: sandbox not enabled for this workspace region (403)" >&2
      else
        echo '[]'
      fi
      exit "${FAKE_SANDBOX_LIST_EXIT:-0}"
    fi
    exit 1
    ;;
  *)
    exit 1
    ;;
esac
`
	path := filepath.Join(dir, "databricks")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake databricks: %v", err)
	}
	return path
}

func TestRun_MissingCLI(t *testing.T) {
	cli := &lakebox.CLI{Bin: filepath.Join(t.TempDir(), "databricks-does-not-exist")}
	var out bytes.Buffer
	summary := Run(context.Background(), &out, cli, "DEFAULT")

	if summary.Ok {
		t.Fatal("expected Ok=false when the CLI is missing")
	}
	if len(summary.Checks) != 4 {
		t.Fatalf("expected 4 checks reported, got %d: %+v", len(summary.Checks), summary.Checks)
	}
	if summary.Checks[0].Status != StatusFail {
		t.Fatalf("expected first check (cli_on_path) to fail, got %+v", summary.Checks[0])
	}
	for _, c := range summary.Checks[1:] {
		if c.Status != StatusFail {
			t.Fatalf("expected downstream check %q to degrade to fail, got %+v", c.Name, c)
		}
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("expected human-readable FAIL output, got %q", out.String())
	}
}

func TestRun_OldVersion(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	t.Setenv("FAKE_VERSION", "1.2.0")
	t.Setenv("FAKE_CURRENT_USER_EXIT", "0")
	t.Setenv("FAKE_SANDBOX_LIST_EXIT", "0")

	cli := &lakebox.CLI{Bin: filepath.Join(dir, "databricks")}
	var out bytes.Buffer
	summary := Run(context.Background(), &out, cli, "DEFAULT")

	if summary.Ok {
		t.Fatal("expected Ok=false for an old CLI version")
	}
	if summary.CLIVersion != "1.2.0" {
		t.Fatalf("CLIVersion = %q, want %q", summary.CLIVersion, "1.2.0")
	}
	versionCheck := findCheck(t, summary.Checks, "cli_version")
	if versionCheck.Status != StatusFail {
		t.Fatalf("expected cli_version check to fail, got %+v", versionCheck)
	}
	if !strings.Contains(out.String(), "below the minimum") {
		t.Fatalf("expected guidance about the minimum version in output, got %q", out.String())
	}
}

func TestRun_GoodPath(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	t.Setenv("FAKE_VERSION", "1.8.0")
	t.Setenv("FAKE_CURRENT_USER_EXIT", "0")
	t.Setenv("FAKE_SANDBOX_LIST_EXIT", "0")

	cli := &lakebox.CLI{Bin: filepath.Join(dir, "databricks")}
	var out bytes.Buffer
	summary := Run(context.Background(), &out, cli, "DEFAULT")

	if !summary.Ok {
		t.Fatalf("expected Ok=true, got summary %+v, output %q", summary, out.String())
	}
	for _, c := range summary.Checks {
		if c.Status != StatusPass {
			t.Fatalf("expected all checks to pass, check %q failed: %+v", c.Name, c)
		}
	}
}

func TestRun_ProfileFails(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	t.Setenv("FAKE_VERSION", "1.8.0")
	t.Setenv("FAKE_CURRENT_USER_EXIT", "1")
	t.Setenv("FAKE_SANDBOX_LIST_EXIT", "0")

	cli := &lakebox.CLI{Bin: filepath.Join(dir, "databricks")}
	var out bytes.Buffer
	summary := Run(context.Background(), &out, cli, "DEFAULT")

	if summary.Ok {
		t.Fatal("expected Ok=false when profile resolution fails")
	}
	if findCheck(t, summary.Checks, "profile_resolves").Status != StatusFail {
		t.Fatal("expected profile_resolves check to fail")
	}
	// Sandbox check still ran independently (each check degrades
	// gracefully; profile failure doesn't block the rest).
	if findCheck(t, summary.Checks, "sandbox_group").Status != StatusPass {
		t.Fatal("expected sandbox_group check to still run and pass")
	}
}

func TestRun_SandboxGroupUnreachable_RegionGuidance(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	t.Setenv("FAKE_VERSION", "1.8.0")
	t.Setenv("FAKE_CURRENT_USER_EXIT", "0")
	t.Setenv("FAKE_SANDBOX_LIST_EXIT", "1")

	cli := &lakebox.CLI{Bin: filepath.Join(dir, "databricks")}
	var out bytes.Buffer
	summary := Run(context.Background(), &out, cli, "DEFAULT")

	if summary.Ok {
		t.Fatal("expected Ok=false when sandbox group is unreachable")
	}
	if findCheck(t, summary.Checks, "sandbox_group").Status != StatusFail {
		t.Fatal("expected sandbox_group check to fail")
	}
	if !strings.Contains(out.String(), "region") {
		t.Fatalf("expected region-gate guidance in output, got %q", out.String())
	}
}

func findCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, checks)
	return Check{}
}
