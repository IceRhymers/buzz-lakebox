package lakebox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
		sbs, _, err := cli.SandboxList(context.Background(), "DEFAULT")
		if err != nil {
			t.Fatalf("SandboxList() error: %v", err)
		}
		if len(sbs) != 0 {
			t.Fatalf("SandboxList() = %+v, want empty", sbs)
		}
	})

	t.Run("failure", func(t *testing.T) {
		t.Setenv("FAKE_SANDBOX_LIST_EXIT", "1")
		if _, _, err := cli.SandboxList(context.Background(), "DEFAULT"); err == nil {
			t.Fatal("expected SandboxList to fail")
		}
	})
}

// writeFakeDatabricksFull is a richer shim covering the M1 sandbox
// lifecycle methods (create/list/status/start/delete/config), controlled
// via FAKE_* env vars, mirroring writeFakeDatabricks's pattern.
func writeFakeDatabricksFull(t *testing.T, dir string) string {
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
  sandbox)
    case "$2" in
      register)
        exit "${FAKE_REGISTER_EXIT:-0}"
        ;;
      create)
        name="$3"
        if [ "${FAKE_CREATE_EXIT:-0}" != "0" ]; then
          echo "create failed" >&2
          exit "${FAKE_CREATE_EXIT}"
        fi
        printf '{"sandboxId":"%s","name":"%s","status":"%s"}' "${FAKE_CREATE_ID:-sandbox-1}" "$name" "${FAKE_CREATE_STATUS:-Running}"
        exit 0
        ;;
      list)
        if [ "${FAKE_LIST_EXIT:-0}" != "0" ]; then
          echo "list failed" >&2
          exit "${FAKE_LIST_EXIT}"
        fi
        printf '%s' "${FAKE_LIST_JSON:-[]}"
        exit 0
        ;;
      status)
        id="$3"
        if [ "${FAKE_STATUS_EXIT:-0}" != "0" ]; then
          echo "status failed" >&2
          exit "${FAKE_STATUS_EXIT}"
        fi
        printf '{"sandboxId":"%s","name":"fake","status":"%s"}' "$id" "${FAKE_STATUS_STATUS:-Running}"
        exit 0
        ;;
      start)
        exit "${FAKE_START_EXIT:-0}"
        ;;
      delete)
        echo "$3" >> "${FAKE_DELETE_LOG:-/dev/null}"
        exit "${FAKE_DELETE_EXIT:-0}"
        ;;
      config)
        shift 2
        echo "$*" >> "${FAKE_CONFIG_LOG:-/dev/null}"
        exit "${FAKE_CONFIG_EXIT:-0}"
        ;;
      *)
        exit 1
        ;;
    esac
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

func TestCLI_SandboxRegister(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}

	t.Setenv("FAKE_REGISTER_EXIT", "0")
	if err := cli.SandboxRegister(context.Background(), "DEFAULT"); err != nil {
		t.Fatalf("SandboxRegister() error: %v", err)
	}
}

func TestCLI_SandboxCreate(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}

	t.Setenv("FAKE_CREATE_ID", "sandbox-abc")
	sb, err := cli.SandboxCreate(context.Background(), "DEFAULT", "buzz-npub12-agent")
	if err != nil {
		t.Fatalf("SandboxCreate() error: %v", err)
	}
	if sb.ID != "sandbox-abc" || sb.Name != "buzz-npub12-agent" || sb.Status != "Running" {
		t.Fatalf("SandboxCreate() = %+v", sb)
	}
}

func TestCLI_SandboxCreate_Failure_EmbedsVersion(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}
	t.Setenv("FAKE_VERSION", "1.8.0")
	t.Setenv("FAKE_CREATE_EXIT", "1")

	_, err := cli.SandboxCreate(context.Background(), "DEFAULT", "buzz-x")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "1.8.0") {
		t.Fatalf("error %q should embed the CLI version", err.Error())
	}
}

func TestCLI_SandboxList_ParsesJSONArray(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}
	t.Setenv("FAKE_LIST_JSON", `[{"sandboxId":"a","name":"buzz-1-x","status":"Running"},{"sandboxId":"b","name":"buzz-2-y","status":"Stopped"}]`)

	sbs, _, err := cli.SandboxList(context.Background(), "DEFAULT")
	if err != nil {
		t.Fatalf("SandboxList() error: %v", err)
	}
	if len(sbs) != 2 || sbs[0].ID != "a" || sbs[1].Status != "Stopped" {
		t.Fatalf("SandboxList() = %+v", sbs)
	}
}

func TestCLI_SandboxStatus(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}
	t.Setenv("FAKE_STATUS_STATUS", "Stopped")

	sb, err := cli.SandboxStatus(context.Background(), "DEFAULT", "sandbox-1")
	if err != nil {
		t.Fatalf("SandboxStatus() error: %v", err)
	}
	if sb.Status != "Stopped" {
		t.Fatalf("SandboxStatus() = %+v", sb)
	}
}

func TestCLI_SandboxStart(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}

	if err := cli.SandboxStart(context.Background(), "DEFAULT", "sandbox-1"); err != nil {
		t.Fatalf("SandboxStart() error: %v", err)
	}
}

func TestCLI_WaitRunning_SucceedsImmediately(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}
	t.Setenv("FAKE_STATUS_STATUS", "Running")

	slept := false
	err := cli.WaitRunning(context.Background(), "DEFAULT", "sandbox-1", 5*time.Second, time.Millisecond, func(time.Duration) { slept = true })
	if err != nil {
		t.Fatalf("WaitRunning() error: %v", err)
	}
	if slept {
		t.Fatal("WaitRunning() should not have slept when already Running")
	}
}

func TestCLI_WaitRunning_TimesOut(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}
	t.Setenv("FAKE_STATUS_STATUS", "Stopping")

	sleepCount := 0
	// pollInterval is irrelevant here since sleep is mocked to a no-op
	// counter instead of a real wait; the deadline is reached purely by
	// real subprocess-spawn wall time across repeated status polls.
	err := cli.WaitRunning(context.Background(), "DEFAULT", "sandbox-1", 400*time.Millisecond, time.Millisecond, func(time.Duration) { sleepCount++ })
	if err == nil {
		t.Fatal("expected WaitRunning to time out")
	}
	if sleepCount == 0 {
		t.Fatal("expected WaitRunning to have polled at least once before timing out")
	}
}

func TestCLI_SandboxDelete(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}

	if err := cli.SandboxDelete(context.Background(), "DEFAULT", "sandbox-1"); err != nil {
		t.Fatalf("SandboxDelete() error: %v", err)
	}
}

func TestCLI_SandboxConfig(t *testing.T) {
	dir := t.TempDir()
	writeFakeDatabricksFull(t, dir)
	cli := &CLI{Bin: filepath.Join(dir, "databricks")}
	logFile := filepath.Join(dir, "config.log")
	t.Setenv("FAKE_CONFIG_LOG", logFile)

	t.Run("no-autostop", func(t *testing.T) {
		if err := cli.SandboxConfig(context.Background(), "DEFAULT", "sandbox-1", SandboxConfigOptions{NoAutostop: true}); err != nil {
			t.Fatalf("SandboxConfig() error: %v", err)
		}
		data, _ := os.ReadFile(logFile)
		if !strings.Contains(string(data), "--no-autostop") {
			t.Fatalf("config log %q missing --no-autostop", data)
		}
	})

	t.Run("idle-timeout", func(t *testing.T) {
		_ = os.Remove(logFile)
		if err := cli.SandboxConfig(context.Background(), "DEFAULT", "sandbox-1", SandboxConfigOptions{IdleTimeout: "1h"}); err != nil {
			t.Fatalf("SandboxConfig() error: %v", err)
		}
		data, _ := os.ReadFile(logFile)
		if !strings.Contains(string(data), "--idle-timeout 1h") {
			t.Fatalf("config log %q missing --idle-timeout 1h", data)
		}
	})

	t.Run("neither-set-errors", func(t *testing.T) {
		if err := cli.SandboxConfig(context.Background(), "DEFAULT", "sandbox-1", SandboxConfigOptions{}); err == nil {
			t.Fatal("expected error when neither NoAutostop nor IdleTimeout set")
		}
	})
}

func TestParseSandboxTable(t *testing.T) {
	out := "ID           NAME              STATUS\n" +
		"sandbox-a    buzz-abc-one      Running\n" +
		"sandbox-b    buzz-abc-two      Stopped\n" +
		"\n"
	sbs, err := parseSandboxTable(out)
	if err != nil {
		t.Fatalf("parseSandboxTable() error: %v", err)
	}
	want := []Sandbox{
		{ID: "sandbox-a", Name: "buzz-abc-one", Status: "Running"},
		{ID: "sandbox-b", Name: "buzz-abc-two", Status: "Stopped"},
	}
	if len(sbs) != len(want) {
		t.Fatalf("parseSandboxTable() = %+v, want %+v", sbs, want)
	}
	for i := range want {
		if sbs[i] != want[i] {
			t.Fatalf("parseSandboxTable()[%d] = %+v, want %+v", i, sbs[i], want[i])
		}
	}
}

func TestSandboxList_FallsBackToTableWhenJSONUnsupported(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$2" = "list" ]; then
  if printf '%s\n' "$*" | grep -q -- '--json'; then
    echo "Error: unknown flag: --json" >&2
    exit 1
  fi
  echo "ID           NAME              STATUS"
  echo "sandbox-x    buzz-xyz-agent    Running"
  exit 0
fi
exit 1
`
	path := filepath.Join(dir, "databricks")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake databricks: %v", err)
	}
	cli := &CLI{Bin: path}

	sbs, _, err := cli.SandboxList(context.Background(), "DEFAULT")
	if err != nil {
		t.Fatalf("SandboxList() error: %v", err)
	}
	if len(sbs) != 1 || sbs[0].ID != "sandbox-x" || sbs[0].Status != "Running" {
		t.Fatalf("SandboxList() fallback = %+v", sbs)
	}
}
