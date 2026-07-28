package install

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEmbeddedLockfile_PinsEveryPackage is what makes the integrity claim
// mechanical rather than aspirational: `npm ci` only verifies what the
// lockfile pins, so a lockfile entry without an integrity hash is a package
// that would be fetched on trust at deploy time. The tree is ~111 packages
// and nobody will audit it by eye.
func TestEmbeddedLockfile_PinsEveryPackage(t *testing.T) {
	for version, raw := range adapterLockfiles {
		var lf struct {
			LockfileVersion int `json:"lockfileVersion"`
			Packages        map[string]struct {
				Resolved  string `json:"resolved"`
				Integrity string `json:"integrity"`
			} `json:"packages"`
		}
		if err := json.Unmarshal(raw, &lf); err != nil {
			t.Fatalf("adapter %s: lockfile is not valid JSON: %v", version, err)
		}
		if lf.LockfileVersion < 3 {
			t.Errorf("adapter %s: lockfileVersion = %d, want >= 3 (v2 omits integrity for some entries)", version, lf.LockfileVersion)
		}

		resolved := 0
		for name, pkg := range lf.Packages {
			if pkg.Resolved == "" {
				continue // the root project entry has no tarball
			}
			resolved++
			if pkg.Integrity == "" {
				t.Errorf("adapter %s: package %q has a resolved URL but no integrity hash — npm ci would fetch it unverified", version, name)
			}
		}
		if resolved == 0 {
			t.Errorf("adapter %s: lockfile pins no packages at all", version)
		}

		// The platform package carries the actual Claude Code runtime and
		// is by far the largest thing installed; it must be pinned for the
		// host the sandbox actually runs.
		if _, ok := lf.Packages["node_modules/@anthropic-ai/claude-agent-sdk-linux-x64"]; !ok {
			t.Errorf("adapter %s: lockfile has no linux-x64 platform entry; the sandbox could resolve it outside the lockfile", version)
		}
	}
}

func TestBuildAdapterInstallScript_DefaultVersion(t *testing.T) {
	script, err := BuildAdapterInstallScript("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"set -eu",
		"npm ci --omit=dev --ignore-scripts --no-audit --no-fund",
		`cd "$ADAPTER_DIR"`,
		DefaultAdapterVersion,
		`ln -sf "$ADAPTER_BIN"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// set -x would echo every command, and this script writes a lockfile
	// and manifest inline.
	if strings.Contains(script, "set -x") {
		t.Error("install script must not enable tracing")
	}
	// The symlink must land in the dir launch.sh already prepends to PATH,
	// which is what lets buzz-acp spawn the adapter by bare name.
	if !strings.Contains(script, `BIN_DIR="`+BinDir+`"`) {
		t.Errorf("adapter must be exposed via the existing bin dir %q", BinDir)
	}
}

func TestBuildAdapterInstallScript_UnknownVersionFailsLoud(t *testing.T) {
	_, err := BuildAdapterInstallScript("9.9.9-nope")
	if err == nil {
		t.Fatal("an unpinned adapter version must be rejected, not installed on trust")
	}
	var unknown UnknownAdapterVersionError
	if !strings.Contains(err.Error(), "9.9.9-nope") {
		t.Errorf("error should name the rejected version: %v", err)
	}
	if _, ok := err.(UnknownAdapterVersionError); !ok {
		t.Errorf("error should be %T, got %T", unknown, err)
	}
}

// TestAdapterStamp_IncludesLockfileHash pins that regenerating the lockfile
// at the SAME adapter version still forces a reinstall. A version-only
// marker would silently skip it, leaving a stale tree that no longer
// matches the committed pin.
func TestAdapterStamp_IncludesLockfileHash(t *testing.T) {
	a := adapterStamp("0.63.0", []byte(`{"a":1}`))
	b := adapterStamp("0.63.0", []byte(`{"a":2}`))
	if a == b {
		t.Fatal("stamp must change when the lockfile changes at the same version")
	}
	if !strings.HasPrefix(a, "0.63.0+") {
		t.Fatalf("stamp should remain human-readable at the version prefix, got %q", a)
	}
}

func TestVerifySpecFor_PerRuntime(t *testing.T) {
	if got := VerifySpecFor("buzz-agent").Bin; got != BinDir+"/buzz-agent" {
		t.Errorf("buzz-agent spec = %q", got)
	}
	if got := VerifySpecFor(AdapterBinName).Bin; got != BinDir+"/"+AdapterBinName {
		t.Errorf("claude spec = %q", got)
	}
	// An unroutable runtime must produce an empty spec, which
	// BuildVerifyCommand then rejects — better than silently verifying the
	// wrong binary.
	if got := VerifySpecFor("goose").Bin; got != "" {
		t.Errorf("unknown runtime should yield an empty spec, got %q", got)
	}
}

func TestBuildVerifyCommand_RejectsEmptyAndHostileBins(t *testing.T) {
	if _, err := BuildVerifyCommand(`$HOME/.env.verify`, 10, VerifySpec{}); err == nil {
		t.Error("an empty verify binary must be rejected")
	}
	for _, bin := range []string{
		`$HOME/bin/x"; rm -rf /; echo "`,
		"$HOME/bin/$(whoami)",
		"$HOME/bin/`id`",
		"$HOME/bin/x y",
		"$HOME/bin/x\\ny",
	} {
		if cmd, err := BuildVerifyCommand(`$HOME/.env.verify`, 10, VerifySpec{Bin: bin}); err == nil {
			t.Errorf("hostile bin %q should have been rejected, got script: %q", bin, cmd)
		}
	}
}

// TestBuildVerifyCommand_PreservesExitStatus pins the property a naive
// `| tail -c` would silently destroy: a pipeline reports its LAST command's
// status, so bounding the output inline would make every failed handshake
// look like a success. Capturing to a file keeps both properties.
func TestBuildVerifyCommand_PreservesExitStatus(t *testing.T) {
	cmd, err := BuildVerifyCommand(`$HOME/.buzz-backend/.env.verify`, 10, VerifySpecFor(AdapterBinName))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(cmd, `| tail -c`) {
		t.Error("output must not be bounded inside the pipeline; that would mask the handshake's exit status")
	}
	if !strings.Contains(cmd, "exit $rc") {
		t.Error("script must propagate the handshake's own exit status")
	}
	if !strings.Contains(cmd, "2>&1") {
		t.Error("stderr must be captured; sshx discards it on the success path this output is scanned on")
	}
	if !strings.Contains(cmd, AdapterBinName) {
		t.Error("claude verify should invoke the adapter binary")
	}
}
