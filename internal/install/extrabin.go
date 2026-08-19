package install

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
)

// ExtraBinDir is the $HOME-scoped directory BuildExtraBinariesInstallScript
// places fetched binaries into. It is deliberately NOT $HOME/.buzz-backend/bin
// (the dir launch.sh PREPENDS to PATH): a prepended dir would let an unlisted
// extra binary shadow a real system binary, which the merged #18 contract
// (capabilities.go / docs/CONTRACT.md §3) assigns to this step to prevent.
// internal/nest appends this dir to PATH instead, so an extra binary can never
// win against a system binary nor against a provider binary in BinDir — see the
// placement ADR in .omc/plans/issue-15-extra-binaries.md §4.
//
// extraBinMarkerDir is the sibling dir holding per-binary sha256 markers. It
// lives OUTSIDE the PATH-exposed dir so extra-bin contains only executables,
// preserving the install.go/adapter.go convention that the skip marker never
// sits in the exposed bin dir.
const (
	ExtraBinDir       = "$HOME/.buzz-backend/extra-bin"
	extraBinMarkerDir = "$HOME/.buzz-backend/extra-bin-markers"
)

// extraBinNameCharset is the install-local re-validation of a bin name. #18's
// payload validator already enforces this at the boundary, but internal/install
// must not trust its caller blindly (belt-and-suspenders): the name is expanded
// into a filename inside the rendered script, so an unexpected value must fail
// here rather than reach the sandbox. Mirrors payload.extraBinaryNamePattern.
var extraBinNameCharset = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ExtraBinaryInstall is one entry BuildExtraBinariesInstallScript renders. It
// is the install-local twin of payload.ExtraBinary, kept decoupled so
// internal/install never imports internal/payload — the caller maps the struct
// across the boundary, exactly as it does for AdapterSpec/VerifySpec.
type ExtraBinaryInstall struct {
	// URL is the https source the binary is fetched from (payload data).
	URL string
	// SHA256 is the pinned lowercase-hex digest the fetched bytes must match
	// (payload data). It is also the per-bin marker content: a re-pin changes
	// the sha and forces a refetch, an identical pin skips.
	SHA256 string
	// Bin is the bare filename the executable is placed under in ExtraBinDir
	// (payload data). NEVER interpolated into a filename literal — it flows
	// through a shell $BIN variable via shellquote.Single.
	Bin string
}

// BuildExtraBinariesInstallScript renders the `set -eu` (no `set -x`) script
// that fetches, sha256-verifies, and places each single-file executable into
// ExtraBinDir, exposing it on PATH via internal/nest's appended dir. It mirrors
// install.go/adapter.go: same curl/mktemp/sha256sum fetch, same sha-keyed
// marker skip, same umask 077, same fail-loud posture.
//
// A nil/empty slice returns "" so the caller can skip the round trip entirely
// (a deploy with no extra_binaries is byte-identical to today).
//
// Every payload-derived value (URL, SHA256, Bin) is shellquote.Single-quoted
// into a shell variable and referenced only through that variable — the Bin
// string is NEVER interpolated into a filename literal (unlike adapter.go's
// %s-interpolated BinName, which is a trusted constant; Bin here is payload
// data). A duplicate Bin across entries is rejected rather than guessed at:
// #18's validator has no cross-entry seen-set, so two entries could otherwise
// race the same target/marker.
func BuildExtraBinariesInstallScript(bins []ExtraBinaryInstall) (string, error) {
	if len(bins) == 0 {
		return "", nil
	}

	// Defensive re-validation + cross-entry duplicate rejection so rendering
	// is deterministic and never trusts the caller blindly.
	seen := make(map[string]bool, len(bins))
	for _, eb := range bins {
		switch {
		case eb.Bin == "":
			return "", fmt.Errorf("extra binary bin must not be empty")
		case eb.Bin == "." || eb.Bin == "..":
			return "", fmt.Errorf("extra binary bin %q is the current- or parent-directory entry, not a filename", eb.Bin)
		case strings.HasPrefix(eb.Bin, "-"):
			return "", fmt.Errorf("extra binary bin %q must not start with '-'", eb.Bin)
		case !extraBinNameCharset.MatchString(eb.Bin):
			return "", fmt.Errorf("extra binary bin %q is not a bare filename (allowed: ^[A-Za-z0-9._-]+$)", eb.Bin)
		}
		if seen[eb.Bin] {
			return "", fmt.Errorf("duplicate extra binary bin %q: two entries would race the same target and marker; each bin must be unique", eb.Bin)
		}
		seen[eb.Bin] = true
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n\n")
	b.WriteString(`EXTRA_BIN_DIR="` + ExtraBinDir + `"` + "\n")
	b.WriteString(`EXTRA_BIN_MARKER_DIR="` + extraBinMarkerDir + `"` + "\n\n")
	b.WriteString(`mkdir -p "$EXTRA_BIN_DIR" "$EXTRA_BIN_MARKER_DIR"` + "\n")

	for _, eb := range bins {
		fmt.Fprintf(&b, "\nURL=%s\n", shellquote.Single(eb.URL))
		fmt.Fprintf(&b, "SHA=%s\n", shellquote.Single(eb.SHA256))
		fmt.Fprintf(&b, "BIN=%s\n", shellquote.Single(eb.Bin))
		b.WriteString(`TARGET="$EXTRA_BIN_DIR/$BIN"` + "\n")
		b.WriteString(`MARKER="$EXTRA_BIN_MARKER_DIR/$BIN"` + "\n")

		// Skip branch keyed on the sha content (stamp semantics): a re-pin
		// forces a refetch, an identical pin skips. The fetch trio is
		// byte-identical to install.go's .deb download.
		b.WriteString(`if [ -f "$MARKER" ] && [ "$(cat "$MARKER")" = "$SHA" ]; then` + "\n")
		b.WriteString(`  echo "$BIN already installed; skipping"` + "\n")
		b.WriteString("else\n")
		b.WriteString(`  TMP=$(mktemp "${TMPDIR:-/tmp}/buzz-extrabin-XXXXXX")` + "\n")
		b.WriteString(`  trap 'rm -f "$TMP"' EXIT` + "\n")
		b.WriteString(`  curl -q -fL --retry 2 -o "$TMP" "$URL"` + "\n")
		b.WriteString(`  echo "$SHA  $TMP" | sha256sum -c -` + "\n")
		b.WriteString(`  chmod +x "$TMP"` + "\n")
		b.WriteString(`  mv "$TMP" "$TARGET"` + "\n")
		b.WriteString(`  trap - EXIT` + "\n")
		b.WriteString(`  printf '%s' "$SHA" > "$MARKER"` + "\n")
		b.WriteString("fi\n")

		// Post-install (outside the skip): confirm the file exists at OUR path
		// and is executable — catches a truncated/failed placement. This tests
		// file-exists-at-our-path, NOT resolves-by-name (the warning below
		// covers the shadow case).
		b.WriteString(`if [ ! -x "$TARGET" ]; then` + "\n")
		b.WriteString(`  echo "extra binary $BIN was not installed as an executable at $TARGET" >&2` + "\n")
		b.WriteString("  exit 1\n")
		b.WriteString("fi\n")

		// Shadow warning (outside the skip, NON-FATAL): the install script's
		// own PATH does not include extra-bin (only launch.sh appends it), so a
		// name that resolves to some other path is shadowed by a system/provider
		// binary and will be unreachable by that bare name under launch.sh. A
		// shadow is the operator's problem to resolve, not a deploy blocker —
		// observed loudly rather than silently swallowed.
		// SHADOW is captured ONCE (|| true so a non-resolving name never trips
		// set -e here) and reused, so no command substitution runs unguarded in
		// the echo argument under set -e.
		b.WriteString(`SHADOW=$(command -v "$BIN" 2>/dev/null || true)` + "\n")
		b.WriteString(`if [ -n "$SHADOW" ] && [ "$SHADOW" != "$TARGET" ]; then` + "\n")
		b.WriteString(`  echo "warning: extra binary $BIN is shadowed on PATH by $SHADOW and will be unreachable by that name" >&2` + "\n")
		b.WriteString("fi\n")
	}

	return b.String(), nil
}
