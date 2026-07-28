package install

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
)

// DefaultAdapterVersion is the build-time-pinned
// @agentclientprotocol/claude-agent-acp release installed for the Claude
// runtime when provider_config.claude_adapter_version is empty.
const DefaultAdapterVersion = "0.63.0"

// adapterPackage / AdapterBinName are the npm package and the executable it
// installs into node_modules/.bin. AdapterBinName must equal
// payload.RuntimeClaude.SpawnCommand(), since that is the bare name
// buzz-acp spawns.
const (
	adapterPackage = "@agentclientprotocol/claude-agent-acp"
	AdapterBinName = "claude-agent-acp"
)

// AdapterDir is the $HOME-scoped npm tree the adapter is installed into,
// kept beside dist/ and bin/ under the same .buzz-backend root.
const (
	AdapterDir         = "$HOME/.buzz-backend/npm-claude"
	adapterMarkerRel   = ".installed_adapter"
	adapterPackageJSON = `{"name":"buzz-claude-adapter","private":true,"version":"0.0.0","dependencies":{%s:%s}}`
)

//go:embed lockfiles/claude-agent-acp-0.63.0.package-lock.json
var adapterLockfile0_63_0 []byte

// adapterLockfiles maps a pinned adapter version to its committed
// package-lock.json. This is the integrity pin, and the reason the install
// uses `npm ci` rather than `npm install`: the lockfile carries a sha512 for
// every package in the tree — all 111 of them, including the ~275 MB
// platform binary that actually contains the executed Claude Code runtime —
// and `npm ci` aborts on any mismatch.
//
// Note this is registry trust-on-first-use with a long memory, NOT the same
// guarantee as the .deb's maintainer-chosen sha256 pin: it verifies that the
// bytes match what the registry served when the lockfile was generated, not
// that anyone read them. It is still strictly stronger than pinning only the
// top-level package and letting ~110 transitive deps resolve at deploy time.
//
// One lockfile covers every platform: npm records all 8
// @anthropic-ai/claude-agent-sdk-<os>-<arch> optional dependencies with
// integrity hashes and selects the matching one at install time, so no
// --os/--cpu generation flags are needed.
var adapterLockfiles = map[string][]byte{
	DefaultAdapterVersion: adapterLockfile0_63_0,
}

// UnknownAdapterVersionError is returned by BuildAdapterInstallScript when
// the requested adapter version has no committed lockfile — the structural
// twin of UnknownVersionError, and for the same reason: overriding the
// version without a pinned integrity source must fail loud rather than
// silently install whatever the registry currently serves.
type UnknownAdapterVersionError struct {
	Version string
}

func (e UnknownAdapterVersionError) Error() string {
	return fmt.Sprintf(
		"no committed package-lock.json for %s version %q; refusing to install without integrity pinning (known versions: %s)",
		adapterPackage, e.Version, strings.Join(knownAdapterVersions(), ", "),
	)
}

func knownAdapterVersions() []string {
	out := make([]string, 0, len(adapterLockfiles))
	for v := range adapterLockfiles {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// adapterStamp is the marker-file content gating the install skip. It is
// <version>+<sha256 of the lockfile>, NOT the bare version: regenerating the
// lockfile at the same adapter version (a transitive dependency bump, a
// re-pin after a registry incident) must force a reinstall, which a
// version-only marker would silently skip.
func adapterStamp(version string, lockfile []byte) string {
	return fmt.Sprintf("%s+%x", version, sha256.Sum256(lockfile))
}

// BuildAdapterInstallScript renders the `set -eu` (no `set -x`) script that
// installs the Claude Code ACP adapter into AdapterDir and exposes it as
// BinDir/claude-agent-acp.
//
// The symlink into the EXISTING BinDir is what makes this work with no PATH
// change anywhere: launch.sh already prepends $HOME/.buzz-backend/bin
// (internal/nest), buzz-acp spawns the agent by bare name with no path
// resolution or allowlist (block/buzz crates/buzz-acp/src/acp.rs:416), and
// the .deb installer only ever wipes $DIST_DIR, never $BIN_DIR — so the
// symlink survives a buzz_version bump.
//
// Measured on a live sandbox (docs/M2_CLAUDE_PROBE_RESULTS.md P4): 6s with a
// purged npm cache, 4s with --ignore-scripts, 570 MB on a 98 G filesystem.
func BuildAdapterInstallScript(version string) (string, error) {
	if version == "" {
		version = DefaultAdapterVersion
	}
	lockfile, ok := adapterLockfiles[version]
	if !ok {
		return "", UnknownAdapterVersionError{Version: version}
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n\n")
	fmt.Fprintf(&b, "ADAPTER_VERSION=%s\n", shellquote.Single(version))
	fmt.Fprintf(&b, "ADAPTER_STAMP=%s\n", shellquote.Single(adapterStamp(version, lockfile)))
	b.WriteString(`ADAPTER_DIR="` + AdapterDir + `"
BIN_DIR="` + BinDir + `"
MARKER="$ADAPTER_DIR/` + adapterMarkerRel + `"

mkdir -p "$ADAPTER_DIR" "$BIN_DIR"
`)

	// The manifest and lockfile are rewritten on every run (they are tiny
	// and fully determined by the pinned version), so a partially-written
	// tree from an interrupted deploy self-heals on the next one.
	fmt.Fprintf(&b, "\ncat > \"$ADAPTER_DIR/package.json\" <<'BUZZ_ADAPTER_PKG_EOF'\n%s\nBUZZ_ADAPTER_PKG_EOF\n",
		fmt.Sprintf(adapterPackageJSON, `"`+adapterPackage+`"`, `"`+version+`"`))
	fmt.Fprintf(&b, "\ncat > \"$ADAPTER_DIR/package-lock.json\" <<'BUZZ_ADAPTER_LOCK_EOF'\n%s\nBUZZ_ADAPTER_LOCK_EOF\n",
		strings.TrimRight(string(lockfile), "\n"))

	// `cd` rather than `npm ci --prefix`: --prefix semantics for `ci` have
	// varied across npm majors, and the probe ran the `cd` form verbatim.
	//
	// --ignore-scripts is safe here (probe P4: the tree still works and
	// node_modules/.bin/claude-agent-acp is still executable) and removes
	// package postinstall execution — worth having in inference_auth
	// "sandbox" mode, where the baked workspace credential is present.
	b.WriteString(`
if [ -f "$MARKER" ] && [ "$(cat "$MARKER")" = "$ADAPTER_STAMP" ]; then
  echo "claude adapter $ADAPTER_VERSION already installed; skipping npm ci"
else
  cd "$ADAPTER_DIR"
  if ! npm ci --omit=dev --ignore-scripts --no-audit --no-fund; then
    echo "npm ci failed: integrity mismatch (do NOT retry - the registry served different bytes than the committed lockfile pins), or no egress to registry.npmjs.org" >&2
    exit 1
  fi
  printf '%s' "$ADAPTER_STAMP" > "$MARKER"
fi

`)

	// Outside the skip branch: a damaged or missing symlink self-heals even
	// when the npm tree itself is already current.
	fmt.Fprintf(&b, `ADAPTER_BIN="$ADAPTER_DIR/node_modules/.bin/%s"
if [ ! -x "$ADAPTER_BIN" ]; then
  echo "%s did not provide an executable %s after install" >&2
  exit 1
fi
ln -sf "$ADAPTER_BIN" "$BIN_DIR/%s"
`, AdapterBinName, adapterPackage, AdapterBinName, AdapterBinName)

	return b.String(), nil
}
