package install

import (
	"strings"
	"testing"
)

// oneBin is a minimal valid entry.
func oneBin() ExtraBinaryInstall {
	return ExtraBinaryInstall{
		URL:    "https://example.com/dl/shellbox-mcp",
		SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Bin:    "shellbox-mcp",
	}
}

func TestBuildExtraBinariesInstallScript_Empty(t *testing.T) {
	script, err := BuildExtraBinariesInstallScript(nil)
	if err != nil {
		t.Fatalf("empty slice error: %v", err)
	}
	if script != "" {
		t.Fatalf("empty slice must render \"\" so the caller skips the round trip, got: %q", script)
	}
	script, err = BuildExtraBinariesInstallScript([]ExtraBinaryInstall{})
	if err != nil || script != "" {
		t.Fatalf("zero-length slice must render \"\" with no error, got %q / %v", script, err)
	}
}

func TestBuildExtraBinariesInstallScript_Header(t *testing.T) {
	script, err := BuildExtraBinariesInstallScript([]ExtraBinaryInstall{oneBin()})
	if err != nil {
		t.Fatalf("build error: %v", err)
	}
	if !strings.HasPrefix(script, "#!/bin/sh\nset -eu\numask 077\n") {
		t.Fatalf("script header must be #!/bin/sh / set -eu / umask 077, got:\n%s", script)
	}
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "set -x" || strings.HasPrefix(trimmed, "set -ex") || strings.Contains(trimmed, "set -xe") {
			t.Fatalf("script must never enable shell tracing, found: %q", line)
		}
	}
	if !strings.Contains(script, `mkdir -p "$EXTRA_BIN_DIR" "$EXTRA_BIN_MARKER_DIR"`) {
		t.Fatal("script must create both the bin dir and the sibling marker dir")
	}
}

// TestBuildExtraBinariesInstallScript_QuotesPayloadValues pins P3: url/sha/bin
// are always shellquote.Single-quoted into shell vars, and the filename is
// "$EXTRA_BIN_DIR/$BIN" — never a Go-interpolated literal. A hostile bin would
// be rejected by validation, but a hostile URL must be quoted, not raw.
func TestBuildExtraBinariesInstallScript_QuotesPayloadValues(t *testing.T) {
	hostile := ExtraBinaryInstall{
		URL:    "https://evil.example.com/x';touch /tmp/pwned;'",
		SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Bin:    "tool",
	}
	script, err := BuildExtraBinariesInstallScript([]ExtraBinaryInstall{hostile})
	if err != nil {
		t.Fatalf("build error: %v", err)
	}
	// The URL's embedded single quotes must be escaped via the standard
	// close/escape/reopen trick, never appear as a raw break-out.
	if strings.Contains(script, `touch /tmp/pwned`) && !strings.Contains(script, `'\''`) {
		t.Fatalf("hostile URL must be single-quote escaped, got:\n%s", script)
	}
	if !strings.Contains(script, `URL='https://evil.example.com/x'\'';touch /tmp/pwned`) {
		t.Fatalf("URL must be assigned via shellquote.Single, got:\n%s", script)
	}
	if !strings.Contains(script, `TARGET="$EXTRA_BIN_DIR/$BIN"`) {
		t.Fatal("target filename must be built from the $BIN shell var, never a Go literal")
	}
	if !strings.Contains(script, `MARKER="$EXTRA_BIN_MARKER_DIR/$BIN"`) {
		t.Fatal("marker path must be built from the $BIN shell var in the sibling marker dir")
	}
	if !strings.Contains(script, `BIN='tool'`) {
		t.Fatal("bin must be assigned via a single-quoted shell var")
	}
}

func TestBuildExtraBinariesInstallScript_FetchAndVerify(t *testing.T) {
	script, err := BuildExtraBinariesInstallScript([]ExtraBinaryInstall{oneBin()})
	if err != nil {
		t.Fatalf("build error: %v", err)
	}
	for _, want := range []string{
		`TMP=$(mktemp "${TMPDIR:-/tmp}/buzz-extrabin-XXXXXX")`,
		`trap 'rm -f "$TMP"' EXIT`,
		`curl -q -fL --proto '=https' --proto-redir '=https' --retry 2 -o "$TMP" "$URL"`,
		`echo "$SHA  $TMP" | sha256sum -c -`,
		`chmod +x "$TMP"`,
		`mv "$TMP" "$TARGET"`,
		`printf '%s' "$SHA" > "$MARKER"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing fetch/verify line %q, got:\n%s", want, script)
		}
	}
}

// TestBuildExtraBinariesInstallScript_SkipKeyedOnSha pins the marker skip.
func TestBuildExtraBinariesInstallScript_SkipKeyedOnSha(t *testing.T) {
	script, err := BuildExtraBinariesInstallScript([]ExtraBinaryInstall{oneBin()})
	if err != nil {
		t.Fatalf("build error: %v", err)
	}
	if !strings.Contains(script, `if [ -f "$MARKER" ] && [ "$(cat "$MARKER")" = "$SHA" ] && [ -x "$TARGET" ]; then`) {
		t.Fatal("skip branch must be keyed on the marker sha AND the target still existing (self-heal)")
	}
	if !strings.Contains(script, `echo "$BIN already installed; skipping"`) {
		t.Fatal("skip branch must echo a per-bin already-installed line")
	}
}

// TestBuildExtraBinariesInstallScript_PostInstallAssert pins the executable
// assertion that catches a truncated placement.
func TestBuildExtraBinariesInstallScript_PostInstallAssert(t *testing.T) {
	script, err := BuildExtraBinariesInstallScript([]ExtraBinaryInstall{oneBin()})
	if err != nil {
		t.Fatalf("build error: %v", err)
	}
	if !strings.Contains(script, `if [ ! -x "$TARGET" ]; then`) {
		t.Fatal("script must assert the placed file is executable")
	}
}

// TestBuildExtraBinariesInstallScript_ShadowWarningNonFatal pins the loud but
// non-fatal shadow warning: it must be present and must NOT exit.
func TestBuildExtraBinariesInstallScript_ShadowWarningNonFatal(t *testing.T) {
	script, err := BuildExtraBinariesInstallScript([]ExtraBinaryInstall{oneBin()})
	if err != nil {
		t.Fatalf("build error: %v", err)
	}
	// command -v is captured once (|| true) so the guarded warning can never
	// trip set -e via an unguarded substitution in the echo argument.
	if !strings.Contains(script, `SHADOW=$(command -v "$BIN" 2>/dev/null || true)`) {
		t.Fatalf("script must capture the shadow path once via command -v, got:\n%s", script)
	}
	const guard = `if [ -n "$SHADOW" ] && [ "$SHADOW" != "$TARGET" ]; then`
	idx := strings.Index(script, guard)
	if idx < 0 {
		t.Fatalf("script must probe for a system shadow, got:\n%s", script)
	}
	// The shadow branch must be non-fatal: no `exit` between its `then` and
	// its closing `fi`.
	rest := script[idx+len(guard):]
	fi := strings.Index(rest, "\nfi")
	if fi < 0 {
		t.Fatal("shadow warning branch is not closed with fi")
	}
	if strings.Contains(rest[:fi], "exit") {
		t.Fatalf("shadow warning must be non-fatal (no exit in its branch), got:\n%s", rest[:fi])
	}
	if !strings.Contains(rest[:fi], "warning:") || !strings.Contains(rest[:fi], ">&2") {
		t.Fatal("shadow warning must echo a warning to stderr")
	}
	// The warning must ALSO be persisted to the durable WARN_LOG, because a
	// successful deploy discards stderr (sshx returns only stdout on success and
	// deployflow discards that) — stderr alone would reach no one. The operator
	// recovers it from the file.
	if !strings.Contains(rest[:fi], `>> "$WARN_LOG"`) {
		t.Fatalf("shadow warning must be appended to the durable WARN_LOG, got:\n%s", rest[:fi])
	}
	// WARN_LOG is bound to the exported path and truncated once up front, so it
	// reflects only the current deploy rather than accumulating across redeploys.
	if !strings.Contains(script, `WARN_LOG="`+ExtraBinWarningsFile+`"`) {
		t.Fatal("script must bind WARN_LOG to ExtraBinWarningsFile")
	}
	if !strings.Contains(script, `: > "$WARN_LOG"`) {
		t.Fatal("script must truncate WARN_LOG once up front so it reflects the current deploy")
	}
}

// TestBuildExtraBinariesInstallScript_MultipleEntries: each entry renders its
// own independent marker/skip block.
func TestBuildExtraBinariesInstallScript_MultipleEntries(t *testing.T) {
	bins := []ExtraBinaryInstall{
		{URL: "https://example.com/a", SHA256: strings.Repeat("a", 64), Bin: "alpha"},
		{URL: "https://example.com/b", SHA256: strings.Repeat("b", 64), Bin: "beta"},
	}
	script, err := BuildExtraBinariesInstallScript(bins)
	if err != nil {
		t.Fatalf("build error: %v", err)
	}
	if !strings.Contains(script, `BIN='alpha'`) || !strings.Contains(script, `BIN='beta'`) {
		t.Fatal("both entries must render a BIN assignment")
	}
	if n := strings.Count(script, `if [ -f "$MARKER" ]`); n != 2 {
		t.Fatalf("expected 2 independent skip blocks, got %d", n)
	}
	// The mkdir is emitted exactly once, not per entry.
	if n := strings.Count(script, "mkdir -p"); n != 1 {
		t.Fatalf("expected a single mkdir, got %d", n)
	}
}

// TestBuildExtraBinariesInstallScript_DuplicateBinRejected pins the
// deterministic rejection (#18's validator has no cross-entry dedup).
func TestBuildExtraBinariesInstallScript_DuplicateBinRejected(t *testing.T) {
	bins := []ExtraBinaryInstall{
		{URL: "https://example.com/a", SHA256: strings.Repeat("a", 64), Bin: "dup"},
		{URL: "https://example.com/b", SHA256: strings.Repeat("b", 64), Bin: "dup"},
	}
	if _, err := BuildExtraBinariesInstallScript(bins); err == nil {
		t.Fatal("duplicate bin across entries must be rejected")
	} else if !strings.Contains(err.Error(), "dup") {
		t.Fatalf("error should name the duplicate bin, got: %v", err)
	}
}

// TestBuildExtraBinariesInstallScript_RejectsBadBinNames is the defensive
// belt-and-suspenders re-validation.
func TestBuildExtraBinariesInstallScript_RejectsBadBinNames(t *testing.T) {
	for _, bin := range []string{"", ".", "..", "-rf", "a/b", "a b", "a;b", "a`b"} {
		bins := []ExtraBinaryInstall{{URL: "https://example.com/a", SHA256: strings.Repeat("a", 64), Bin: bin}}
		if _, err := BuildExtraBinariesInstallScript(bins); err == nil {
			t.Errorf("bin %q should have been rejected", bin)
		}
	}
}

// TestBuildExtraBinariesInstallScript_MarkerOutsideBinDir pins that markers
// render into the sibling dir, not inside the PATH-exposed extra-bin dir.
func TestBuildExtraBinariesInstallScript_MarkerOutsideBinDir(t *testing.T) {
	if !strings.HasPrefix(extraBinMarkerDir, "$HOME/.buzz-backend/") {
		t.Fatalf("marker dir must be under .buzz-backend, got %q", extraBinMarkerDir)
	}
	if strings.HasPrefix(extraBinMarkerDir, ExtraBinDir+"/") || extraBinMarkerDir == ExtraBinDir {
		t.Fatalf("marker dir %q must not live inside the PATH-exposed bin dir %q", extraBinMarkerDir, ExtraBinDir)
	}
}
