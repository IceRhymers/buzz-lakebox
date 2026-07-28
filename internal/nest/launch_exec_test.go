package nest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Double-launch guard proof (docs/PLAN.md §6 M3 accept: "deploy-twice
// yields exactly one process group").
//
// The golden-file tests in nest_test.go assert the guard's TEXT is
// present. That is not the same claim: a flock/pgrep guard can be
// syntactically present and still fail to hold (wrong fd, wrong exit
// path, a pattern that self-matches). These tests actually RUN the
// rendered script — twice, against a stand-in buzz-acp — and count the
// processes that survive.

// launchHarness renders launch.sh into a temp $HOME laid out the way a
// real deploy leaves it: an env file to source and a fake buzz-acp
// binary where the installer puts the real one.
type launchHarness struct {
	home   string
	script string
	marker string
}

func newLaunchHarness(t *testing.T) *launchHarness {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("launch.sh's guard uses flock/setsid (util-linux); only asserted on Linux")
	}
	for _, tool := range []string{"flock", "setsid", "pgrep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	// The guard is machine-wide: a real buzz-acp already running on this
	// host would make it trip for the wrong reason and turn a genuine
	// failure into a false pass. Zombies are excluded for the same
	// reason AliveCheckSnippet excludes them — and because these tests
	// leave their own behind when the session has no reaping init.
	if len(liveBuzzAcpPIDs()) > 0 {
		t.Skip("a live buzz-acp process is already running on this machine; the guard proof would be ambiguous")
	}

	home := t.TempDir()
	backend := filepath.Join(home, ".buzz-backend")
	bin := filepath.Join(backend, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A unique marker per test run, written by the stand-in agent, so
	// the assertions count only THIS test's launches.
	marker := filepath.Join(home, "launched")
	fake := "#!/bin/sh\necho started >> " + marker + "\nsleep 10\n"
	if err := os.WriteFile(filepath.Join(bin, "buzz-acp"), []byte(fake), 0o700); err != nil {
		t.Fatalf("write fake buzz-acp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backend, "env"), []byte("export BUZZ_RELAY_URL='wss://relay.invalid'\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	scriptPath := filepath.Join(backend, "launch.sh")
	if err := os.WriteFile(scriptPath, []byte(RenderLaunchScript(false, false, "")), 0o700); err != nil {
		t.Fatalf("write launch.sh: %v", err)
	}

	h := &launchHarness{home: home, script: scriptPath, marker: marker}
	// The stand-in agents are detached (setsid) sleepers; nothing else
	// would reap them. Matching on the temp $HOME kills only this
	// test's processes — and the wait matters: the guard these tests
	// exercise is machine-wide, so a straggler from one test would make
	// the next one skip itself.
	t.Cleanup(func() { h.kill() })
	return h
}

// kill terminates this harness's stand-in agents and waits for them to
// stop being live processes. They may linger as zombies afterwards
// (nothing reaps a detached process whose parent already exited when
// PID 1 is not a reaping init) — which is precisely the state
// AliveCheckSnippet is built to tolerate, so waiting for them to
// disappear entirely would hang.
func (h *launchHarness) kill() {
	_ = exec.Command("pkill", "-f", h.home).Run()
	for i := 0; i < 40; i++ {
		if len(liveBuzzAcpPIDs()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// liveBuzzAcpPIDs returns the non-zombie buzz-acp pids on this machine —
// the Go-side mirror of AliveCheckSnippet's semantics.
func liveBuzzAcpPIDs() []string {
	out, err := exec.Command("pgrep", "-f", "[b]uzz-acp").Output()
	if err != nil {
		return nil // pgrep exits non-zero when nothing matches
	}
	var live []string
	for _, pid := range strings.Fields(string(out)) {
		st, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
		state := strings.TrimSpace(string(st))
		if err != nil || state == "" || strings.HasPrefix(state, "Z") {
			continue
		}
		live = append(live, pid)
	}
	return live
}

// run executes launch.sh with HOME pointed at the harness, the way
// deploy's launch-exec step and the start subcommand both do.
func (h *launchHarness) run(t *testing.T) {
	t.Helper()
	cmd := exec.Command("sh", h.script)
	cmd.Env = append(os.Environ(), "HOME="+h.home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launch.sh failed: %v (output: %s)", err, out)
	}
	// The launch is detached; give it a moment to write its marker.
	time.Sleep(300 * time.Millisecond)
}

// launchCount is how many times the stand-in agent actually started.
func (h *launchHarness) launchCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(h.marker)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read marker: %v", err)
	}
	return len(strings.Fields(string(data)))
}

func TestLaunchScript_SecondRunDoesNotStartASecondAgent(t *testing.T) {
	h := newLaunchHarness(t)

	h.run(t)
	if got := h.launchCount(t); got != 1 {
		t.Fatalf("first launch started %d agents, want 1", got)
	}

	// The redeploy / concurrent-start case: launch.sh runs again while
	// the previous agent is alive.
	h.run(t)
	if got := h.launchCount(t); got != 1 {
		t.Fatalf("second launch started another agent (%d total); the pgrep guard did not hold", got)
	}
}

func TestLaunchScript_RelaunchesAfterTheAgentDies(t *testing.T) {
	h := newLaunchHarness(t)

	h.run(t)
	if got := h.launchCount(t); got != 1 {
		t.Fatalf("first launch started %d agents, want 1", got)
	}

	// The recovery case that matters more than the guard: once the agent
	// is gone, `start` must actually bring it back — a guard that never
	// releases would be worse than no guard.
	h.kill()

	h.run(t)
	if got := h.launchCount(t); got != 2 {
		t.Fatalf("relaunch after death produced %d total launches, want 2", got)
	}
}

// TestLaunchScript_ZombieDoesNotBlockRelaunch is the regression proof
// for the reason AliveCheckSnippet exists rather than a bare pgrep.
//
// buzz-acp is launched detached, so a crashed agent is reparented to the
// sandbox's PID 1 (`sandbox-daemon`; systemd is not booted — see
// docs/M05_PROBE_RESULTS.md §5), which is not guaranteed to reap it. A
// <defunct> buzz-acp still matches `pgrep -f`, so the old guard would
// refuse to relaunch a dead agent forever — silent death with no
// recovery path.
func TestLaunchScript_ZombieDoesNotBlockRelaunch(t *testing.T) {
	h := newLaunchHarness(t)

	// Make a real zombie: a process named buzz-acp that has exited and
	// whose parent (this test binary) deliberately has not reaped it.
	zombieDir := filepath.Join(h.home, "zbin")
	if err := os.MkdirAll(zombieDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	zombiePath := filepath.Join(zombieDir, "buzz-acp")
	if err := os.WriteFile(zombiePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write zombie script: %v", err)
	}
	zombie := exec.Command(zombiePath)
	if err := zombie.Start(); err != nil {
		t.Fatalf("start zombie: %v", err)
	}
	defer func() { _ = zombie.Wait() }() // reap only at the end
	time.Sleep(300 * time.Millisecond)

	// Precondition: a bare pgrep — the old guard — DOES match it.
	if err := exec.Command("pgrep", "-f", "[b]uzz-acp").Run(); err != nil {
		t.Skip("this platform's pgrep does not match the zombie; the guard's zombie case cannot be exercised here")
	}

	h.run(t)
	if got := h.launchCount(t); got != 1 {
		t.Fatalf("a zombie buzz-acp blocked the relaunch (%d launches, want 1)", got)
	}
}
