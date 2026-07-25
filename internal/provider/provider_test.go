package provider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/version"
)

// run is a test helper driving the same entrypoint main.go uses in
// provider mode, so these tests exercise real end-to-end behavior rather
// than internals.
func run(t *testing.T, input string, deploy DeployFunc) (line string, err error) {
	t.Helper()
	var out bytes.Buffer
	err = Run(strings.NewReader(input), &out, deploy)
	return out.String(), err
}

func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("response %q does not end in a single newline", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("response %q is not exactly one line", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &m); err != nil {
		t.Fatalf("response line is not valid JSON: %v (%q)", err, line)
	}
	return m
}

func TestInfo_FrozenShape(t *testing.T) {
	line, err := run(t, `{"op":"info"}`, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)

	want := map[string]any{
		"ok":          true,
		"name":        "Databricks Lakebox",
		"version":     version.Version,
		"description": "Deploys Buzz agents into Databricks Lakebox sandboxes",
		"protocol":    "v1",
		"ops":         []any{"info", "deploy"},
	}
	for k, wv := range want {
		gv, ok := m[k]
		if !ok {
			t.Fatalf("info response missing field %q; got %#v", k, m)
		}
		if !equalJSON(t, wv, gv) {
			t.Fatalf("info response field %q = %#v, want %#v", k, gv, wv)
		}
	}
}

func TestInfo_WithRequestIDIgnoresIt(t *testing.T) {
	line, err := run(t, `{"op":"info","request_id":"11111111-1111-1111-1111-111111111111"}`, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	if ok, _ := m["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %#v", m)
	}
}

func TestUnknownOp_FrozenShape(t *testing.T) {
	line, err := run(t, `{"op":"bogus"}`, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)

	wantErr := `unknown op "bogus"; supported: info, deploy`
	if ok, _ := m["ok"].(bool); ok {
		t.Fatalf("expected ok:false, got %#v", m)
	}
	if got, _ := m["error"].(string); got != wantErr {
		t.Fatalf("error = %q, want %q", got, wantErr)
	}
}

func TestDeploy_M0Stub(t *testing.T) {
	line, err := run(t, `{"op":"deploy","agent":{}}`, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	if ok, _ := m["ok"].(bool); ok {
		t.Fatalf("expected ok:false, got %#v", m)
	}
	wantErr := "deploy not implemented yet (M1)"
	if got, _ := m["error"].(string); got != wantErr {
		t.Fatalf("error = %q, want %q", got, wantErr)
	}
}

func TestMalformedJSON_HandledNotPanicked(t *testing.T) {
	cases := []string{
		``,
		`not json at all`,
		`{"op":`,
		`{"op": 5}`, // op as wrong type
		`null`,
		`[]`,
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			line, err := run(t, c, nil)
			if err != nil {
				t.Fatalf("Run returned unhandleable error for input %q: %v", c, err)
			}
			m := decodeLine(t, line)
			if ok, _ := m["ok"].(bool); ok {
				t.Fatalf("input %q: expected ok:false, got %#v", c, m)
			}
			if _, ok := m["error"].(string); !ok {
				t.Fatalf("input %q: expected string error field, got %#v", c, m)
			}
		})
	}
}

func TestDeploy_WithHandler_Success(t *testing.T) {
	deploy := func(req *payload.DeployRequest) (string, error) {
		return "sandbox-123", nil
	}
	body := `{"op":"deploy","agent":{"name":"a","relay_url":"wss://relay","private_key_nsec":"nsec1x","auth_tag":"tag","agent_command":"buzz-agent"}}`
	line, err := run(t, body, deploy)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	if ok, _ := m["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %#v", m)
	}
	if got, _ := m["agent_id"].(string); got != "sandbox-123" {
		t.Fatalf("agent_id = %q, want %q", got, "sandbox-123")
	}
}

func TestDeploy_WithHandler_ValidationRejectsUnsupportedRuntime(t *testing.T) {
	body := `{"op":"deploy","agent":{"name":"a","relay_url":"wss://relay","private_key_nsec":"nsec1secretvalue","auth_tag":"tag","agent_command":"goose"}}`
	line, err := run(t, body, func(req *payload.DeployRequest) (string, error) {
		t.Fatalf("deploy handler should not be called when validation fails")
		return "", nil
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	if ok, _ := m["ok"].(bool); ok {
		t.Fatalf("expected ok:false, got %#v", m)
	}
	errMsg, _ := m["error"].(string)
	if !strings.Contains(errMsg, "goose") {
		t.Fatalf("error %q should name the rejected runtime", errMsg)
	}
	if strings.Contains(errMsg, "nsec1secretvalue") {
		t.Fatalf("error %q leaked the nsec", errMsg)
	}
}

func TestDeploy_HandlerErrorIsRedacted(t *testing.T) {
	body := `{"op":"deploy","agent":{"name":"a","relay_url":"wss://relay","private_key_nsec":"nsec1verysecretvalue","auth_tag":"secret-auth-tag-value","agent_command":"buzz-agent","env_vars":{"DATABRICKS_TOKEN":"dapi-marker-secret-1234"}}}`
	deploy := func(req *payload.DeployRequest) (string, error) {
		return "", errFromAllSecrets(req)
	}
	line, err := run(t, body, deploy)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	m := decodeLine(t, line)
	errMsg, _ := m["error"].(string)
	for _, secret := range []string{"nsec1verysecretvalue", "secret-auth-tag-value", "dapi-marker-secret-1234"} {
		if strings.Contains(errMsg, secret) {
			t.Fatalf("error %q leaked secret %q", errMsg, secret)
		}
	}
}

func errFromAllSecrets(req *payload.DeployRequest) error {
	return errAllSecrets{
		nsec:    req.Agent.PrivateKeyNsec,
		auth:    req.Agent.AuthTag,
		envVars: req.Agent.EnvVars,
	}
}

type errAllSecrets struct {
	nsec    string
	auth    string
	envVars map[string]string
}

func (e errAllSecrets) Error() string {
	s := "deploy failed; nsec=" + e.nsec + " auth=" + e.auth
	for k, v := range e.envVars {
		s += " " + k + "=" + v
	}
	return s
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(ab) == string(bb)
}
