// Package muxbin holds the cross-compiled bzmux multiplexer binary, embedded
// into the provider so it can be written into the sandbox at deploy without a
// separate download, host, or pin (issue #16 Increment 2, plan §2, Option A1).
//
// The embedded artifact targets linux/amd64 (the Databricks Lakebox sandbox
// arch, confirmed by internal/install/install.go's Buzz_<v>_amd64.deb fetch).
//
// Regenerate both files after changing cmd/bzmux or internal/muxcfg source:
//
//	make bzmux
//
// This cross-compiles a fresh linux/amd64 binary and rewrites bzmux.srchash.
// The staleness guard (TestEmbeddedBzmux_MatchesSource) runs under `go test ./...`
// on every platform without a cross-build, catching "edited source without
// regenerating". The CI test job also re-runs the srchash tool and diffs only
// bzmux.srchash (NOT the binary, which is Go-version-dependent; see plan §7.4).
package muxbin

import _ "embed"

// Binary is the embedded bzmux executable bytes (linux/amd64). Written into the
// sandbox and chmod +x'd at deploy. Regenerate with `make bzmux` after changing
// cmd/bzmux or internal/muxcfg source.
//
//go:embed bzmux.linux-amd64
var Binary []byte

// SrcHash is the sha256 of the bzmux source tree recorded when Binary was last
// regenerated. The staleness test (TestEmbeddedBzmux_MatchesSource) compares it
// against a freshly computed hash of the sources on every `go test ./...` run.
//
//go:embed bzmux.srchash
var SrcHash string
