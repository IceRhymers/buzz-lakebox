package lakebox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Databricks CLI v1.8.0\n", "1.8.0"},
		{"1.9.2", "1.9.2"},
		{"databricks version 2.0.10 (build abc)", "2.0.10"},
	}
	for _, c := range cases {
		got, err := parseVersion(c.in)
		if err != nil {
			t.Fatalf("parseVersion(%q) error: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("parseVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseVersion_NoVersionFound(t *testing.T) {
	if _, err := parseVersion("no version here"); err == nil {
		t.Fatal("expected error for input with no semver")
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.8.0", "1.8.0", 0},
		{"1.9.0", "1.8.0", 1},
		{"1.7.9", "1.8.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.8.1", "1.8.0", 1},
	}
	for _, c := range cases {
		got, err := CompareSemver(c.a, c.b)
		if err != nil {
			t.Fatalf("CompareSemver(%q, %q) error: %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Fatalf("CompareSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestMeetsMinVersion(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"1.8.0", true},
		{"1.9.0", true},
		{"2.0.0", true},
		{"1.7.9", false},
		{"0.9.9", false},
	}
	for _, c := range cases {
		got, err := MeetsMinVersion(c.v)
		if err != nil {
			t.Fatalf("MeetsMinVersion(%q) error: %v", c.v, err)
		}
		if got != c.want {
			t.Fatalf("MeetsMinVersion(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

// writeFakeDatabricks writes an executable shell script named "databricks"
// into dir that fakes `version`/`current-user me`/`sandbox list` per the
// FAKE_* environment variables the test sets (PLAN.md §7: exercise the CLI
// wrapper against a fake PATH shim, not a real installation).
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

func TestCLI_Version_AgainstFakeShim(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	t.Setenv("FAKE_VERSION", "1.8.0")

	cli := &CLI{Bin: filepath.Join(dir, "databricks")}
	v, err := cli.Version(context.Background())
	if err != nil {
		t.Fatalf("Version() error: %v", err)
	}
	if v != "1.8.0" {
		t.Fatalf("Version() = %q, want %q", v, "1.8.0")
	}
}

func TestCLI_LookPath_MissingBinary(t *testing.T) {
	cli := &CLI{Bin: filepath.Join(t.TempDir(), "databricks-does-not-exist")}
	if _, err := cli.LookPath(); err == nil {
		t.Fatal("expected LookPath to fail for a nonexistent binary")
	}
}

func TestCLI_CurrentUser_AgainstFakeShim(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}

	t.Run("success", func(t *testing.T) {
		t.Setenv("FAKE_CURRENT_USER_EXIT", "0")
		if _, err := cli.CurrentUser(context.Background(), "DEFAULT"); err != nil {
			t.Fatalf("CurrentUser() error: %v", err)
		}
	})

	t.Run("failure", func(t *testing.T) {
		t.Setenv("FAKE_CURRENT_USER_EXIT", "1")
		if _, err := cli.CurrentUser(context.Background(), "DEFAULT"); err == nil {
			t.Fatal("expected CurrentUser to fail")
		}
	})
}

func TestCLI_SandboxList_AgainstFakeShim(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricks(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}

	t.Run("success", func(t *testing.T) {
		t.Setenv("FAKE_SANDBOX_LIST_EXIT", "0")
		if _, err := cli.SandboxList(context.Background(), "DEFAULT"); err != nil {
			t.Fatalf("SandboxList() error: %v", err)
		}
	})

	t.Run("failure", func(t *testing.T) {
		t.Setenv("FAKE_SANDBOX_LIST_EXIT", "1")
		if _, err := cli.SandboxList(context.Background(), "DEFAULT"); err == nil {
			t.Fatal("expected SandboxList to fail")
		}
	})
}
