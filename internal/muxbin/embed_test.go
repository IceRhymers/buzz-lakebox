package muxbin_test

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/muxbin"
)

// TestEmbeddedBzmux_MatchesSource is the portable drift gate: it recomputes
// the source hash over the non-test .go files that compile into bzmux and
// asserts it equals the committed muxbin.SrcHash. Running under `go test ./...`
// on any platform with no cross-build required. If this fails, run `make bzmux`.
//
// Algorithm (must stay in sync with internal/muxbin/srchash/main.go):
//  1. Collect cmd/bzmux/*.go + internal/muxcfg/*.go, excluding *_test.go.
//  2. Sort paths lexicographically.
//  3. For each path: write "path\n" + file bytes into SHA-256.
//  4. Compare hex digest to muxbin.SrcHash.
func TestEmbeddedBzmux_MatchesSource(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	paths := collectSrcPaths(t, root)

	h := sha256.New()
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		// Use the path as-is (same as srchash/main.go uses filepath.Join).
		// On Linux/macOS filepath.Separator is '/' so this is always forward-slash.
		_, _ = fmt.Fprintf(h, "%s\n", p)
		h.Write(data)
	}
	want := hex.EncodeToString(h.Sum(nil))
	got := strings.TrimSpace(muxbin.SrcHash)

	if got != want {
		t.Errorf("bzmux source has changed since the embedded binary was last regenerated:\n"+
			"  committed srchash : %s\n"+
			"  current srchash   : %s\n"+
			"Run `make bzmux` from the repo root to regenerate "+
			"internal/muxbin/bzmux.linux-amd64 and internal/muxbin/bzmux.srchash, "+
			"then commit the updated files.", got, want)
	}
}

// TestEmbeddedBzmux_IsLinuxAmd64ELF asserts that the embedded binary is a
// valid linux/amd64 ELF without executing it. This catches a placeholder or
// an accidentally cross-compiled binary for the wrong arch.
func TestEmbeddedBzmux_IsLinuxAmd64ELF(t *testing.T) {
	t.Parallel()

	data := muxbin.Binary
	if len(data) < 20 {
		t.Fatalf("embedded binary too short (%d bytes); expected a valid ELF", len(data))
	}

	// Parse with debug/elf for authoritative class + machine checks.
	r := strings.NewReader(string(data))
	f, err := elf.NewFile(r)
	if err != nil {
		t.Fatalf("embedded binary is not a valid ELF file: %v\n"+
			"(first 4 bytes: % x)\n"+
			"Run `make bzmux` to regenerate.", err, data[:4])
	}
	defer func() { _ = f.Close() }()

	if f.Class != elf.ELFCLASS64 {
		t.Errorf("embedded binary ELF class = %v, want ELFCLASS64", f.Class)
	}
	if f.Machine != elf.EM_X86_64 {
		t.Errorf("embedded binary machine = %v (0x%x), want EM_X86_64 (0x3e)",
			f.Machine, uint16(f.Machine))
	}
}

// repoRoot returns the absolute path to the repository root. Tests in this
// package run with os.Getwd() == "<repo>/internal/muxbin", so root is "../../".
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "../.."))
}

// collectSrcPaths returns sorted relative (from repo root) paths of the
// non-test .go files that compile into bzmux. This must match the algorithm
// in internal/muxbin/srchash/main.go exactly.
func collectSrcPaths(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	for _, dir := range []string{"cmd/bzmux", "internal/muxcfg"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("readdir %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	sort.Strings(paths)
	return paths
}
