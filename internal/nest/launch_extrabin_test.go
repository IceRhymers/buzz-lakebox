package nest

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden regenerates the byte-equality golden captured below. Run
// `go test ./internal/nest -run TestRenderLaunchScript_DefaultGolden -update`
// to refresh it after a DELIBERATE change to launch.sh's default rendering.
var updateGolden = flag.Bool("update", false, "update the launch.sh golden file")

const launchDefaultGoldenPath = "testdata/launch_default.golden"

// TestRenderLaunchScript_DefaultGolden is the byte-equality regression lock the
// #15 change introduces: no such golden existed before (the pre-existing
// TestRenderLaunchScript_GoldenInvariants is substring-only). It pins that the
// hasExtraBinaries=false rendering is byte-identical to what shipped before the
// parameter was added, so adding extra-binaries support cannot silently perturb
// a deploy that uses none.
func TestRenderLaunchScript_DefaultGolden(t *testing.T) {
	got := RenderLaunchScript(false, false, "", false)

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(launchDefaultGoldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(launchDefaultGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", launchDefaultGoldenPath)
		return
	}

	want, err := os.ReadFile(launchDefaultGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Fatalf("RenderLaunchScript(false,false,\"\",false) drifted from the golden.\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
	}
}

// TestRenderLaunchScript_ExtraBinariesAppendsPathSuffix pins that the ONLY
// delta between the false and true renderings is the extra-bin PATH suffix.
func TestRenderLaunchScript_ExtraBinariesAppendsPathSuffix(t *testing.T) {
	off := RenderLaunchScript(false, false, "", false)
	on := RenderLaunchScript(false, false, "", true)

	if off == on {
		t.Fatal("hasExtraBinaries=true must change the rendering")
	}

	const suffix = ":$HOME/.buzz-backend/extra-bin"
	if strings.Contains(off, suffix) {
		t.Fatal("the false rendering must NOT contain the extra-bin PATH suffix")
	}
	if !strings.Contains(on, `export PATH="$HOME/.buzz-backend/bin:$PATH:$HOME/.buzz-backend/extra-bin"`) {
		t.Fatalf("true rendering must append the extra-bin dir to PATH, got:\n%s", on)
	}

	// The sole textual delta must be that suffix: removing it from the true
	// rendering must reproduce the false rendering exactly.
	if reverted := strings.Replace(on, suffix, "", 1); reverted != off {
		t.Fatalf("the extra-bin suffix must be the ONLY delta between the two renderings.\n--- true-minus-suffix ---\n%s\n--- false ---\n%s", reverted, off)
	}
}
