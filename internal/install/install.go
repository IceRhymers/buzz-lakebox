// Package install renders the in-sandbox shell script that fetches,
// verifies, and extracts the pinned Buzz .deb (docs/PLAN.md §4.4 step 5),
// plus the runtime-verification command that pipes an ACP `initialize`
// frame into buzz-agent (docs/M05_PROBE_RESULTS.md §6). Nothing here talks
// to a sandbox directly — internal/deployflow ships the rendered text over
// internal/sshx.
package install

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
)

// DefaultVersion is the build-time-pinned Buzz release installed when a
// deploy request's provider_config.buzz_version is empty
// (docs/PLAN.md §4.4 step 5).
const DefaultVersion = "v0.4.24"

// releaseRepo is the GitHub repository Buzz release .deb assets are
// fetched from. URL template verified live 2026-07-24:
// https://github.com/block/buzz/releases/download/v0.4.24/Buzz_0.4.24_amd64.deb
// resolves (302 to the asset CDN), and the release listing shows the
// Buzz_<ver>_amd64.deb naming holds on newer tags (v0.4.25).
const releaseRepo = "block/buzz"

// pinnedSHA256 maps a known Buzz release version to its published .deb
// sha256 (docs/CONTRACT.md / M1 task: "make version→sha a small map so
// overriding version without a known sha fails loud rather than skipping
// verification").
var pinnedSHA256 = map[string]string{
	DefaultVersion: "ee9e58cf92707993f24f2eed18721ece6029e0b869c71770ad4a5d6e05f820d2",
}

// BinNames are the executables symlinked into $HOME/.buzz-backend/bin
// after extraction (docs/PLAN.md §4.4 step 5).
var BinNames = []string{"buzz-acp", "buzz", "buzz-agent", "buzz-dev-mcp", "git-credential-nostr"}

// DistDir / BinDir / versionMarkerFile are the well-known in-sandbox paths
// the rendered install script manages, exported so internal/deployflow and
// tests can refer to them without re-deriving the layout.
const (
	DistDir          = "$HOME/.buzz-backend/dist"
	BinDir           = "$HOME/.buzz-backend/bin"
	versionMarkerRel = ".installed_version"
)

// UnknownVersionError is returned by BuildInstallScript when the
// requested version has no known pinned sha256 — overriding the version
// without a known checksum must fail loud rather than silently skip
// verification.
type UnknownVersionError struct {
	Version string
}

func (e UnknownVersionError) Error() string {
	return fmt.Sprintf("no known sha256 pin for buzz version %q; refusing to install without checksum verification (known versions: %s)", e.Version, strings.Join(knownVersions(), ", "))
}

func knownVersions() []string {
	out := make([]string, 0, len(pinnedSHA256))
	for v := range pinnedSHA256 {
		out = append(out, v)
	}
	return out
}

// BuildInstallScript renders the `set -eu` (no `set -x`) install script
// for the given pinned version (DefaultVersion if empty): curl -fL the
// release .deb, verify its pinned sha256, dpkg-deb -x it into
// $HOME/.buzz-backend/dist, and symlink BinNames into
// $HOME/.buzz-backend/bin. Skips the download/extract entirely when the
// version marker file already matches (docs/PLAN.md §4.4 step 5).
func BuildInstallScript(version string) (string, error) {
	if version == "" {
		version = DefaultVersion
	}
	sha, ok := pinnedSHA256[version]
	if !ok {
		return "", UnknownVersionError{Version: version}
	}

	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/Buzz_%s_amd64.deb", releaseRepo, version, strings.TrimPrefix(version, "v"))

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n\n")
	fmt.Fprintf(&b, "BUZZ_VERSION=%s\n", shellquote.Single(version))
	fmt.Fprintf(&b, "BUZZ_SHA256=%s\n", shellquote.Single(sha))
	fmt.Fprintf(&b, "BUZZ_URL=%s\n", shellquote.Single(url))
	b.WriteString(`DIST_DIR="$HOME/.buzz-backend/dist"
BIN_DIR="$HOME/.buzz-backend/bin"
MARKER="$DIST_DIR/` + versionMarkerRel + `"

mkdir -p "$DIST_DIR" "$BIN_DIR"

if [ -f "$MARKER" ] && [ "$(cat "$MARKER")" = "$BUZZ_VERSION" ]; then
  echo "buzz $BUZZ_VERSION already installed; skipping download"
else
  TMP_DEB=$(mktemp "${TMPDIR:-/tmp}/buzz-XXXXXX.deb")
  trap 'rm -f "$TMP_DEB"' EXIT
  curl -fL --retry 2 -o "$TMP_DEB" "$BUZZ_URL"
  echo "$BUZZ_SHA256  $TMP_DEB" | sha256sum -c -
  rm -rf "$DIST_DIR"
  mkdir -p "$DIST_DIR"
  dpkg-deb -x "$TMP_DEB" "$DIST_DIR"
  rm -f "$TMP_DEB"
  trap - EXIT
  printf '%s' "$BUZZ_VERSION" > "$MARKER"
fi

`)
	for _, bin := range BinNames {
		fmt.Fprintf(&b, `SRC=$(find "$DIST_DIR" -type f -name %s 2>/dev/null | head -n1)
if [ -z "$SRC" ]; then
  echo "installed .deb did not contain expected binary: %s" >&2
  exit 1
fi
ln -sf "$SRC" "$BIN_DIR/%s"
`, shellquote.Single(bin), bin, bin)
	}

	return b.String(), nil
}

// InitializeFrame is the ACP `initialize` JSON-RPC request piped into
// buzz-agent for runtime verification (docs/M05_PROBE_RESULTS.md §6: no
// `--version`; config validation runs before arg parsing).
const InitializeFrame = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"0.1.0","clientInfo":{"name":"buzz-backend-databricks","version":"dev"}}}`

// AgentInfoMarker is the substring expected in buzz-agent's response to
// InitializeFrame on success (docs/M05_PROBE_RESULTS.md §6: "expect the
// agentInfo result").
const AgentInfoMarker = "agentInfo"

// BuildVerifyCommand renders the COMBINED verify script run as a single
// sshx.RunWithStdin round trip (BUG 1 fix): it reads the agent's env
// content from its own stdin, writes it to envFile (0600), sources it,
// and pipes InitializeFrame into buzz-agent with a timeout — removing
// envFile afterward regardless of outcome via a trap. No secret is ever
// interpolated into this command string itself; the only sanctioned path
// for the env content is stdin.
//
// envFile is a TRUSTED, static "$HOME"-relative literal (e.g.
// "$HOME/.buzz-backend/.env.verify") — NEVER payload/untrusted data — so
// it is interpolated via a double-quoted shell assignment (which must
// expand "$HOME") rather than shellquote.Single (which would instead
// suppress that expansion and break sourcing; this was the root cause of
// the deploy-breaking bug this function now fixes: the file was written
// with $HOME expanded but previously sourced/removed with $HOME literal,
// so sourcing always failed and the trap never removed the real file).
//
// Because envFile is interpolated inside double quotes unescaped, it is
// defensively validated against verifyEnvFileCharset: any character
// outside [A-Za-z0-9_$/.-] (double quote, backtick, $( ), backslash,
// whitespace, ...) is rejected with an error, so a future caller
// mistake can never smuggle shell syntax through this trusted-literal
// path.
func BuildVerifyCommand(envFile string, timeoutSeconds int) (string, error) {
	if !verifyEnvFileCharset.MatchString(envFile) {
		return "", fmt.Errorf("verify env file path %q contains characters outside the allowed set [A-Za-z0-9_$/.-]; BuildVerifyCommand accepts trusted static literals only", envFile)
	}
	return fmt.Sprintf(`set -eu
umask 077
ENVF="%s"
trap 'rm -f "$ENVF"' EXIT
cat > "$ENVF"
chmod 600 "$ENVF"
set -a
# shellcheck disable=SC1090
. "$ENVF"
set +a
printf '%%s\n' %s | timeout %d "$HOME/.buzz-backend/bin/buzz-agent"
`, envFile, shellquote.Single(InitializeFrame), timeoutSeconds), nil
}

// verifyEnvFileCharset is the allowlist for BuildVerifyCommand's envFile
// parameter: path characters plus '$' (for the required "$HOME" prefix),
// and nothing that carries meaning inside a double-quoted shell string
// beyond parameter expansion the caller explicitly wants.
var verifyEnvFileCharset = regexp.MustCompile(`^[A-Za-z0-9_$/.\-]+$`)
