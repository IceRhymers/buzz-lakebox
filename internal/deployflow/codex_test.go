package deployflow

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/install"
	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// TestCodexProbeScript_ReadsEndpointFromArtifact pins the design choice that
// makes this probe worth running: it must read base_url out of the generated
// config.toml, not re-derive it from DATABRICKS_HOST. Re-deriving would test
// a URL the agent may not use, and would pass in exactly the case worth
// catching — a config that exists but points somewhere else.
func TestCodexProbeScript_ReadsEndpointFromArtifact(t *testing.T) {
	script := codexInferenceProbeScript()

	if !strings.Contains(script, `BUZZ_PROBE_CFG="${CODEX_HOME:-}/config.toml"`) {
		t.Error("probe must locate the generated config via CODEX_HOME")
	}
	if !strings.Contains(script, "base_url") {
		t.Error("probe must parse base_url out of the artifact")
	}
	// Re-deriving the URL from the host would defeat the point.
	if strings.Contains(script, "ai-gateway/codex/v1") {
		t.Error("probe must not hardcode the gateway path; it reads whatever the agent will actually use")
	}
}

// TestCodexProbeScript_ShapeInvariants covers the properties shared with the
// other in-sandbox probes: secrets arrive via stdin and are trapped for
// removal, and the surface is /responses because model support on this
// gateway is per-MODEL — /chat/completions is rejected outright for
// codex-class models (docs/M3_CODEX_PROBE_RESULTS.md G2).
func TestCodexProbeScript_ShapeInvariants(t *testing.T) {
	script := codexInferenceProbeScript()

	for _, want := range []string{
		"set -eu",
		`trap 'rm -f "$BUZZ_PROBE_TMP" "$BUZZ_PROBE_ERR" "$BUZZ_PROBE_HDR"' EXIT`,
		`/responses`,
		`-K "$BUZZ_PROBE_HDR"`,
		codexProbeCauseMarkerPrefix + "unset",
		codexProbeCauseMarkerPrefix + "auth",
		codexProbeCauseMarkerPrefix + "unreachable",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("probe script missing %q", want)
		}
	}
	if strings.Contains(script, "chat/completions") {
		t.Error("codex-class models reject /chat/completions on this gateway; there is no fallback surface to probe")
	}
	// Only an outright credential rejection may fail the deploy: failing on
	// 400/404 would delete a freshly-created sandbox over a model id.
	if !strings.Contains(script, "401|403") {
		t.Error("probe must fail only on 401/403")
	}
}

// TestCodexProbeMarker_DistinctFromOtherProbes pins that two probes in one
// deploy can never have their causes confused. inference_auth="sandbox"
// runs the zero-token auth probe as well as this one.
func TestCodexProbeMarker_DistinctFromOtherProbes(t *testing.T) {
	markers := []string{codexProbeCauseMarkerPrefix, claudeProbeCauseMarkerPrefix, authProbeCauseMarkerPrefix}
	for i := range markers {
		for j := range markers {
			if i != j && markers[i] == markers[j] {
				t.Errorf("probe cause markers must be distinct, got duplicate %q", markers[i])
			}
		}
	}
	// And no marker may be a prefix of another, or probeCause's
	// first-match scan could attribute one probe's line to the other.
	for i := range markers {
		for j := range markers {
			if i != j && strings.HasPrefix(markers[i], markers[j]) {
				t.Errorf("marker %q is a prefix of %q; probeCause could confuse them", markers[i], markers[j])
			}
		}
	}
}

// TestCodexProbeModel_MatchesGeneratedConfig pins the probe to the model the
// generated config.toml actually names. Unlike the claude probe — whose
// model is a pure placeholder because that runtime emits no model at all —
// a divergence here would mean the probe validated a model the agent never
// uses, so a gateway that stopped serving the configured one would deploy
// clean and fail at first mention.
func TestCodexProbeModel_MatchesGeneratedConfig(t *testing.T) {
	if codexProbeModel != nest.CodexDefaultModel {
		t.Errorf("probe model %q != generated config model %q", codexProbeModel, nest.CodexDefaultModel)
	}
	if !strings.Contains(nest.CodexEnvSnippet, `model = "`+codexProbeModel+`"`) {
		t.Errorf("probe model %q does not appear in the generated config", codexProbeModel)
	}
}

// TestCodexProbeCauseMessage_UnsetNamesTheRealHazard checks the diagnostic
// an operator will actually read. The "unset" case is not a generic
// misconfiguration: it is the fail-closed path refusing to launch, and the
// message has to say why silence would have been dangerous.
func TestCodexProbeCauseMessage_UnsetNamesTheRealHazard(t *testing.T) {
	msg := codexProbeCauseMessage("unset", "")
	for _, want := range []string{"DATABRICKS_HOST", "NOT launched", "~/.codex/config.toml", "symlink"} {
		if !strings.Contains(msg, want) {
			t.Errorf("unset diagnosis should mention %q, got: %s", want, msg)
		}
	}
}

// TestCodexProbeCauseMessage_AuthStatusIsValidated pins that a hostile or
// malformed remote status is never echoed verbatim. This value is
// interpolated into an error that does not otherwise pass through
// remoteText, and anything that can write the sandbox user's shell startup
// files could otherwise push unbounded text at the operator's terminal.
func TestCodexProbeCauseMessage_AuthStatusIsValidated(t *testing.T) {
	hostile := "BUZZ_CODEX_PROBE_STATUS=" + strings.Repeat("A", 500) + "\n"
	msg := codexProbeCauseMessage("auth", hostile)
	if !strings.Contains(msg, "HTTP unknown") {
		t.Errorf("a non-3-digit status must render as unknown, got: %s", msg)
	}
	if strings.Contains(msg, strings.Repeat("A", 50)) {
		t.Error("remote text must not be echoed verbatim into the operator's error")
	}

	if msg := codexProbeCauseMessage("auth", "BUZZ_CODEX_PROBE_STATUS=403\n"); !strings.Contains(msg, "HTTP 403") {
		t.Errorf("a well-formed status should be reported, got: %s", msg)
	}
}

// TestAdapterSpecsMatchSpawnCommands closes a gap that exists only because
// two packages hold parallel per-runtime registries with no compile-time
// link between them: internal/payload owns spawnCommands, and
// internal/install keys adapterSpecs by spawn-command STRING specifically
// to avoid importing payload. Nothing forces the two to agree.
//
// This test lives in deployflow because it is the one package that imports
// both. A drift makes installAndVerify's
// `if spec, ok := AdapterSpecFor(rt.SpawnCommand()); ok` silently SKIP the
// adapter install — the deploy then fails later at BuildVerifyCommand with
// an error about a missing binary rather than a missing install, so this is
// defense in depth against a confusing failure, not a silent one.
func TestAdapterSpecsMatchSpawnCommands(t *testing.T) {
	// Every runtime that needs an npm adapter must have a spec reachable
	// by its canonical spawn command.
	for _, rt := range []payload.Runtime{payload.RuntimeClaude, payload.RuntimeCodex} {
		spec, ok := install.AdapterSpecFor(rt.SpawnCommand())
		if !ok {
			t.Errorf("runtime %q spawns %q but has no adapterSpecs entry; its adapter would never be installed", rt, rt.SpawnCommand())
			continue
		}
		if spec.BinName != rt.SpawnCommand() {
			t.Errorf("runtime %q spawns %q but its adapter symlinks %q — buzz-acp would spawn a name that does not exist", rt, rt.SpawnCommand(), spec.BinName)
		}
	}

	// And buzz-agent must NOT have one: it ships in the .deb, so a spec
	// would make the deploy try to npm-install a package that does not
	// exist.
	if _, ok := install.AdapterSpecFor(payload.RuntimeBuzzAgent.SpawnCommand()); ok {
		t.Error("buzz-agent ships in the .deb and must not have an npm adapter spec")
	}
}

// TestProbeScripts_TokenNeverInArgv pins that neither probe puts a bearer
// on a command line. Both run BEFORE the prelaunch kill, so a previous
// deploy's agent can still be alive in the sandbox, and /proc/<pid>/cmdline
// is readable by the same uid it runs as. The header goes through a curl
// config file created by mktemp (0600) and removed by the existing trap.
func TestProbeScripts_TokenNeverInArgv(t *testing.T) {
	for name, script := range map[string]string{
		"codex":  codexInferenceProbeScript(),
		"claude": claudeInferenceProbeScript(),
	} {
		if strings.Contains(script, `-H "Authorization: Bearer`) {
			t.Errorf("%s probe passes the bearer in argv; use the -K config file instead", name)
		}
		if !strings.Contains(script, `-K "$BUZZ_PROBE_HDR"`) {
			t.Errorf("%s probe should read its auth header from a config file", name)
		}
		if !strings.Contains(script, `"$BUZZ_PROBE_HDR"' EXIT`) {
			t.Errorf("%s probe must remove the header file on every exit path", name)
		}
	}
}
