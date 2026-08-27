package install

import (
	"fmt"
	"regexp"
)

// mcpSlotCommandCharset is the allowlist for the payload-derived slotCommand
// interpolated into BuildMcpVerifyCommand's `--command "<slot>"`. It is STRICTER
// than verifyEnvFileCharset (which permits '$' for the "$HOME" prefix in the
// trusted-literal paths): a slot command is a bare MCP command name and never
// needs '$' or '/'. Excluding '$' here makes this package self-sufficient — a
// value that reached the double-quoted interpolation with a '$' would otherwise
// parameter-expand — rather than relying on the upstream validateMcpServers gate
// to have already excluded it. Mirrors payload.extraBinaryNamePattern without
// importing internal/payload (which internal/install must not depend on).
var mcpSlotCommandCharset = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// MCP verify modes for BuildMcpVerifyCommand (issue #17).
const (
	// McpVerifyModeDirect verifies a single resolved command via
	// `bzmux --verify-mcp --command <slot>`.
	McpVerifyModeDirect = "direct"
	// McpVerifyModeMux verifies the on-disk mcp-mux.json multiplexer via
	// `bzmux --verify-mcp` (no --command).
	McpVerifyModeMux = "mux"
)

// BuildMcpVerifyCommand renders the deploy-time MCP verification script run as
// a single sshx.RunWithStdin round trip (issue #17). It mirrors
// BuildVerifyCommand's secret-safe idiom exactly: it reads the agent env
// content from its own stdin, writes it to envFile (0600), sources it under
// `set -a`, exports the launch.sh PATH so bzmux and its children resolve by
// bare name, and runs the ABSOLUTE bzmux binary with --verify-mcp under a
// `timeout` — removing envFile (and the captured output) afterward regardless
// of outcome via a trap. No secret is ever interpolated into the command
// string; the only sanctioned path for the env content is stdin.
//
// mode selects direct (single resolved command, passed as --command) or mux
// (read mcp-mux.json, no --command). timeoutSeconds bounds BOTH the outer shell
// `timeout` (a defense-in-depth backstop) and the bzmux --timeout flag (the
// primary bound the client honors on its own read deadline).
//
// muxBinPath, envFile, and slotCommand are all interpolated into the script, so
// each is validated against verifyEnvFileCharset — muxBinPath and envFile are
// trusted static "$HOME"-relative literals, and slotCommand is payload-derived
// (already constrained by validateMcpServers, re-checked here at the boundary).
func BuildMcpVerifyCommand(envFile string, timeoutSeconds int, muxBinPath, mode, slotCommand string) (string, error) {
	if !verifyEnvFileCharset.MatchString(envFile) {
		return "", fmt.Errorf("mcp-verify env file path %q contains characters outside the allowed set [A-Za-z0-9_$/.-]; BuildMcpVerifyCommand accepts trusted static literals only", envFile)
	}
	if muxBinPath == "" {
		return "", fmt.Errorf("mcp-verify has no bzmux binary path")
	}
	if !verifyEnvFileCharset.MatchString(muxBinPath) {
		return "", fmt.Errorf("mcp-verify bzmux binary path %q contains characters outside the allowed set [A-Za-z0-9_$/.-]; BuildMcpVerifyCommand accepts trusted static literals only", muxBinPath)
	}

	var slotArg string
	switch mode {
	case McpVerifyModeDirect:
		if slotCommand == "" {
			return "", fmt.Errorf("mcp-verify direct mode requires a non-empty slot command")
		}
		if !mcpSlotCommandCharset.MatchString(slotCommand) {
			return "", fmt.Errorf("mcp-verify slot command %q contains characters outside the allowed set [A-Za-z0-9._-] (a slot command is a bare MCP command name)", slotCommand)
		}
		slotArg = fmt.Sprintf(` --command "%s"`, slotCommand)
	case McpVerifyModeMux:
		if slotCommand != "" {
			return "", fmt.Errorf("mcp-verify mux mode does not accept a slot command (got %q)", slotCommand)
		}
	default:
		return "", fmt.Errorf("mcp-verify unknown mode %q (want %q or %q)", mode, McpVerifyModeDirect, McpVerifyModeMux)
	}

	return fmt.Sprintf(`set -eu
umask 077
ENVF="%s"
OUTF="$ENVF.out"
trap 'rm -f "$ENVF" "$OUTF"' EXIT
cat > "$ENVF"
chmod 600 "$ENVF"
set -a
# shellcheck disable=SC1090
. "$ENVF"
set +a
export PATH="%s:$PATH"
rc=0
timeout %d "%s" --verify-mcp%s --timeout %d > "$OUTF" 2>&1 || rc=$?
tail -c %d "$OUTF"
exit $rc
`, envFile, MuxBinDir, timeoutSeconds, muxBinPath, slotArg, timeoutSeconds, maxVerifyOutputBytes), nil
}
