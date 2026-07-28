package deployflow

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/nest"
)

// These tests execute the generated shell through a real POSIX shell,
// following the precedent of internal/nest/{launch,sandbox_auth,claude}_exec_test.go.
//
// They exist because the fake-SSH-shim tests cannot run this code: the shim
// replays canned output per step tag, so an awk program or a case statement
// inside a generated script is never executed by it. Both pieces below are
// load-bearing safety logic that a substring assertion would let you delete
// or invert while leaving every other test green.
func requireSh(t *testing.T, tools ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	for _, tool := range append([]string{"sh"}, tools...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
}

// runLogScoper executes ONLY the log-selection half of
// acpLivenessProbeFor against a fixture acp.log, and returns what the
// provider's Go side would receive.
func runLogScoper(t *testing.T, launchID, logContent string) string {
	t.Helper()
	home := t.TempDir()
	backend := filepath.Join(home, ".buzz-backend")
	if err := os.MkdirAll(backend, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if logContent != "" {
		if err := os.WriteFile(filepath.Join(backend, "acp.log"), []byte(logContent), 0o600); err != nil {
			t.Fatalf("write acp.log: %v", err)
		}
	}

	// Strip the liveness-function preamble and the pgrep line; what remains
	// is the log command under test.
	probe := acpLivenessProbeFor(launchID)
	idx := strings.Index(probe, `echo "BUZZ_PGREP_RC=$?"; `)
	if idx < 0 {
		t.Fatalf("probe shape changed; cannot isolate the log command:\n%s", probe)
	}
	logCmd := probe[idx+len(`echo "BUZZ_PGREP_RC=$?"; `):]

	cmd := exec.Command("sh", "-c", "set -eu\n"+logCmd)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("log command must never fail the sourcing shell, got %v (stderr: %s)", err, stderr.String())
	}
	return stdout.String()
}

// TestLogScoper_SelectsOnlyThisLaunch is the behavioral proof for the
// stale-readiness defense. acp.log is append-only, so a previous deploy's
// "agent_pool_ready" sits in the same file; scoping is the only thing that
// stops it from satisfying this deploy's verification.
func TestLogScoper_SelectsOnlyThisLaunch(t *testing.T) {
	requireSh(t, "awk")

	logContent := "" +
		nest.LaunchEpochPrefix + "oldlaunch\n" +
		"buzz-acp starting: relay=wss://x\n" +
		"agent_pool_ready agents=1\n" +
		nest.LaunchEpochPrefix + "newlaunch\n" +
		"buzz-acp starting: relay=wss://x\n" +
		"initial relay connect failed with terminal error\n"

	got := runLogScoper(t, "newlaunch", logContent)

	if strings.Contains(got, "agent_pool_ready") {
		t.Fatalf("the PREVIOUS launch's readiness line must not reach the verifier:\n%s", got)
	}
	if !strings.Contains(got, "terminal error") {
		t.Fatalf("this launch's own output must reach the verifier:\n%s", got)
	}
	if !strings.Contains(got, nest.LaunchEpochPrefix+"newlaunch") {
		t.Fatalf("the marker line itself must be kept, so a stamped-but-quiet launch is distinguishable from one that never happened:\n%s", got)
	}
}

func TestLogScoper_EdgeCases(t *testing.T) {
	requireSh(t, "awk")

	t.Run("marker absent yields empty output", func(t *testing.T) {
		// The stale-agent case: launch.sh declined to spawn, so it never
		// stamped. An older readiness line must not leak through.
		got := runLogScoper(t, "mylaunch", nest.LaunchEpochPrefix+"other\nagent_pool_ready agents=1\n")
		if strings.TrimSpace(got) != "" {
			t.Fatalf("a missing marker must yield nothing, got %q", got)
		}
	})

	t.Run("stamped but not yet logging keeps the marker", func(t *testing.T) {
		got := runLogScoper(t, "mylaunch", "old noise\n"+nest.LaunchEpochPrefix+"mylaunch\n")
		if strings.TrimSpace(got) == "" {
			t.Fatal("a launch that stamped must not be indistinguishable from one that never started")
		}
	})

	t.Run("missing log file is not an error", func(t *testing.T) {
		if got := runLogScoper(t, "mylaunch", ""); strings.TrimSpace(got) != "" {
			t.Fatalf("a missing acp.log should yield nothing, got %q", got)
		}
	})

	t.Run("printf verbs in log content are not interpreted", func(t *testing.T) {
		got := runLogScoper(t, "mylaunch", nest.LaunchEpochPrefix+"mylaunch\n100%s done %d\n")
		if !strings.Contains(got, "100%s done %d") {
			t.Fatalf("log content must be emitted literally, got %q", got)
		}
	})

	t.Run("unscoped form returns the whole log", func(t *testing.T) {
		got := runLogScoper(t, "", "line one\nagent_pool_ready agents=1\n")
		if !strings.Contains(got, "agent_pool_ready") || !strings.Contains(got, "line one") {
			t.Fatalf("the unscoped form must not filter, got %q", got)
		}
	})
}

// TestClaudeInferenceProbe_OnlyAuthFailuresFail executes the probe's
// classification with a stub curl, pinning the decision that keeps the
// probe from breaking valid deployments.
//
// The rule under test: the probe claims only "this endpoint is reachable
// and accepted this credential". Any HTTP answer other than 401/403 already
// proves both — including 400/404, which against a bring-your-own endpoint
// most likely just means the probe's placeholder model id is not served
// there. Failing those would break a configuration payload validation
// explicitly permits, and would delete a freshly-created sandbox to do it.
func TestClaudeInferenceProbe_OnlyAuthFailuresFail(t *testing.T) {
	requireSh(t)

	for _, tc := range []struct {
		status   string
		wantFail bool
	}{
		{"200", false},
		{"201", false},
		{"400", false}, // model not served here — not a misconfiguration
		{"404", false}, // route/model unknown on a BYO endpoint
		{"429", false}, // transient
		{"500", false}, // transient
		{"401", true},  // credential refused
		{"403", true},  // credential refused
	} {
		t.Run(tc.status, func(t *testing.T) {
			home := t.TempDir()
			// Stub curl: ignores its arguments, prints the status the
			// real one would write via -w '%{http_code}'.
			stub := filepath.Join(home, "curl")
			if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s' "+tc.status+"\n"), 0o755); err != nil {
				t.Fatalf("write curl stub: %v", err)
			}

			env := "export ANTHROPIC_BASE_URL=https://gw.example/v1\nexport ANTHROPIC_AUTH_TOKEN=dapi-test\n"
			cmd := exec.Command("sh", "-c", claudeInferenceProbeScript())
			cmd.Stdin = strings.NewReader(env)
			cmd.Env = []string{"HOME=" + home, "PATH=" + home + ":" + os.Getenv("PATH")}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()

			failed := err != nil
			if failed != tc.wantFail {
				t.Fatalf("HTTP %s: probe failed=%v, want %v (stdout=%q stderr=%q)", tc.status, failed, tc.wantFail, stdout.String(), stderr.String())
			}
			if tc.wantFail {
				if cause := probeCause(stdout.String(), claudeProbeCauseMarkerPrefix); cause != "auth" {
					t.Fatalf("HTTP %s should report cause %q, got %q", tc.status, "auth", cause)
				}
				if got := probeCause(stdout.String(), "BUZZ_CLAUDE_PROBE_STATUS="); got != tc.status {
					t.Fatalf("probe should report the real status, got %q", got)
				}
			}
		})
	}
}

// TestClaudeInferenceProbe_FailsClosedWithoutCredentials pins the other
// half: the probe refuses to launch an agent whose env produced no
// endpoint/token pair, which is what a missing DATABRICKS_HOST yields.
func TestClaudeInferenceProbe_FailsClosedWithoutCredentials(t *testing.T) {
	requireSh(t)

	home := t.TempDir()
	cmd := exec.Command("sh", "-c", claudeInferenceProbeScript())
	cmd.Stdin = strings.NewReader("# nothing derived\n")
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil {
		t.Fatal("a probe with no endpoint/token must fail the deploy")
	}
	if cause := probeCause(stdout.String(), claudeProbeCauseMarkerPrefix); cause != "unset" {
		t.Fatalf("cause = %q, want \"unset\"", cause)
	}
}

// TestSecretShredCommand_CoversVerifyOutput guards a path a prefix-matching
// assertion silently misses: verifyEnvFilePath is a prefix of
// verifyEnvFilePath+".out", so a `Contains(verifyEnvFilePath)` check passes
// even with the .out path deleted. The .out file holds the verify
// handshake's captured output, produced with the agent's full env exported
// under `set -a`.
func TestSecretShredCommand_CoversVerifyOutput(t *testing.T) {
	cmd := secretShredCommand()
	for _, want := range []string{nest.EnvFilePath, verifyEnvFilePath, verifyEnvFilePath + ".out"} {
		if !strings.Contains(cmd, `"`+want+`"`) {
			t.Errorf("shred command must cover %q, got: %s", want, cmd)
		}
	}
}
