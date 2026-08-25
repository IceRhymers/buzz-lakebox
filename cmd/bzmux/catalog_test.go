package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// validateToolName
// ---------------------------------------------------------------------------

func TestValidateToolName_RejectsDoubleUnderscore(t *testing.T) {
	cases := []string{"bad__name", "a__b", "__leading", "trailing__"}
	for _, name := range cases {
		if err := validateToolName(name); err == nil {
			t.Errorf("validateToolName(%q): expected error, got nil", name)
		} else if !strings.Contains(err.Error(), "__") {
			t.Errorf("validateToolName(%q): error %q does not mention __", name, err)
		}
	}
}

func TestValidateToolName_RejectsBudgetOverflow(t *testing.T) {
	// len("bzmux__") = 7; maxQualifiedNameLen = 64 → tool name > 57 chars overflows.
	long := strings.Repeat("x", 58) // 7+58 = 65 > 64
	if err := validateToolName(long); err == nil {
		t.Errorf("validateToolName(58-char name): expected budget error, got nil")
	}
	// Exactly at budget (57 chars → 7+57 = 64) must pass.
	ok := strings.Repeat("x", 57)
	if err := validateToolName(ok); err != nil {
		t.Errorf("validateToolName(57-char name): expected nil, got %v", err)
	}
}

func TestValidateToolName_AcceptsValidNames(t *testing.T) {
	cases := []string{"shell", "search", "list", "a", "tool_name", "tool-name"}
	for _, name := range cases {
		if err := validateToolName(name); err != nil {
			t.Errorf("validateToolName(%q): unexpected error: %v", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// parseTools
// ---------------------------------------------------------------------------

func TestParseTools_SchemaOversizeWarns(t *testing.T) {
	// Build a schema that exceeds maxSchemaBytes (4096).
	bigSchema := `{"type":"object","properties":{"x":{"description":"` +
		strings.Repeat("a", 4100) + `"}}}`
	raw := json.RawMessage(`{"name":"bigschema","inputSchema":` + bigSchema + `}`)

	entries, err := parseTools("srv", []json.RawMessage{raw})
	if err != nil {
		t.Fatalf("parseTools with oversize schema returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].name != "bigschema" {
		t.Errorf("tool name: want %q, got %q", "bigschema", entries[0].name)
	}
	// The tool entry should still carry the original raw bytes verbatim.
	if string(entries[0].raw) != string(raw) {
		t.Error("parseTools mutated the raw bytes for an oversize schema")
	}
}

func TestParseTools_RejectsDoubleUnderscoreInToolName(t *testing.T) {
	raw := json.RawMessage(`{"name":"bad__tool","inputSchema":{"type":"object"}}`)
	_, err := parseTools("srv", []json.RawMessage{raw})
	if err == nil {
		t.Fatal("parseTools with __ tool name: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// mergeCatalogs
// ---------------------------------------------------------------------------

func makeFakeChild(t *testing.T, name string, toolNames ...string) *child {
	t.Helper()
	c := &child{name: name, deadCh: make(chan struct{})}
	for _, tn := range toolNames {
		raw, _ := json.Marshal(map[string]interface{}{
			"name":        tn,
			"description": "tool " + tn,
			"inputSchema": map[string]interface{}{"type": "object"},
		})
		c.tools = append(c.tools, toolEntry{name: tn, raw: raw})
	}
	return c
}

func TestMergeCatalogs_Disjoint(t *testing.T) {
	a := makeFakeChild(t, "srv-a", "search", "list")
	b := makeFakeChild(t, "srv-b", "create", "delete")

	routes, catalog, err := mergeCatalogs([]*child{a, b})
	if err != nil {
		t.Fatalf("mergeCatalogs: unexpected error: %v", err)
	}

	// All four tools present in catalog.
	if len(catalog) != 4 {
		t.Errorf("catalog length: want 4, got %d", len(catalog))
	}

	// Routing map resolves each name to the correct child.
	for _, name := range []string{"search", "list"} {
		if got := routes[name]; got != a {
			t.Errorf("routes[%q]: want srv-a, got %v", name, got)
		}
	}
	for _, name := range []string{"create", "delete"} {
		if got := routes[name]; got != b {
			t.Errorf("routes[%q]: want srv-b, got %v", name, got)
		}
	}

	// Tool names must be unchanged (no renaming).
	nameSet := make(map[string]bool)
	for _, e := range catalog {
		nameSet[e.name] = true
	}
	for _, want := range []string{"search", "list", "create", "delete"} {
		if !nameSet[want] {
			t.Errorf("tool %q missing from merged catalog", want)
		}
	}
}

func TestMergeCatalogs_CollisionFailsLoud(t *testing.T) {
	a := makeFakeChild(t, "srv-a", "shared_tool")
	b := makeFakeChild(t, "srv-b", "other", "shared_tool")

	_, _, err := mergeCatalogs([]*child{a, b})
	if err == nil {
		t.Fatal("mergeCatalogs with collision: expected error, got nil")
	}
	msg := err.Error()
	// Must name both servers in the error.
	if !strings.Contains(msg, "srv-a") {
		t.Errorf("collision error does not mention srv-a: %q", msg)
	}
	if !strings.Contains(msg, "srv-b") {
		t.Errorf("collision error does not mention srv-b: %q", msg)
	}
	if !strings.Contains(msg, "shared_tool") {
		t.Errorf("collision error does not mention the colliding tool: %q", msg)
	}
}

// ---------------------------------------------------------------------------
// buildToolsListResult
// ---------------------------------------------------------------------------

func TestBuildToolsListResult_RoundTrips(t *testing.T) {
	a := makeFakeChild(t, "srv", "alpha", "beta")
	_, catalog, err := mergeCatalogs([]*child{a})
	if err != nil {
		t.Fatal(err)
	}
	result, err := buildToolsListResult(catalog)
	if err != nil {
		t.Fatalf("buildToolsListResult: %v", err)
	}
	var out struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(out.Tools) != 2 {
		t.Errorf("tools count: want 2, got %d", len(out.Tools))
	}
}
