// Command srchash prints the canonical source hash for the bzmux binary.
//
// The hash covers the non-test Go source files that compile into the bzmux
// binary: cmd/bzmux/*.go and internal/muxcfg/*.go, excluding *_test.go.
// It is used by `make bzmux` to update internal/muxbin/bzmux.srchash.
//
// Algorithm (must match TestEmbeddedBzmux_MatchesSource in embed_test.go):
//  1. Collect cmd/bzmux/*.go + internal/muxcfg/*.go, excluding *_test.go.
//  2. Sort the relative paths lexicographically.
//  3. For each path in order: write "path\n" + file bytes into SHA-256.
//  4. Print the lowercase hex digest.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	paths, err := collectPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "srchash: %v\n", err)
		os.Exit(1)
	}

	h := sha256.New()
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "srchash: read %s: %v\n", p, err)
			os.Exit(1)
		}
		_, _ = fmt.Fprintf(h, "%s\n", p)
		h.Write(data)
	}
	fmt.Println(hex.EncodeToString(h.Sum(nil)))
}

// collectPaths returns the sorted list of non-test .go source files that
// compile into bzmux (relative to the repo root, which is where srchash
// is invoked from by `make bzmux`).
func collectPaths() ([]string, error) {
	var paths []string
	for _, dir := range []string{"cmd/bzmux", "internal/muxcfg"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("readdir %s: %w", dir, err)
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
	return paths, nil
}
