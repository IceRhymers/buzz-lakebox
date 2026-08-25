package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	bzmuxStem           = "bzmux"
	bzmuxPrefix         = bzmuxStem + "__"
	maxQualifiedNameLen = 64
	maxSchemaBytes      = 4096
)

// toolEntry holds one tool from a child's catalog.  raw is preserved verbatim
// so the tools/list response to the agent is byte-identical to what the child
// reported.
type toolEntry struct {
	name string
	raw  json.RawMessage // full tool JSON object (name, description, inputSchema)
}

// toolsListResult is the subset of a tools/list result we need.
type toolsListResult struct {
	Tools []json.RawMessage `json:"tools"`
}

// parseTools parses the raw tools array from a child tools/list response.
// It returns an error if any tool name is invalid (§5B), and logs a warning for
// oversized schemas.
func parseTools(serverName string, rawTools []json.RawMessage) ([]toolEntry, error) {
	entries := make([]toolEntry, 0, len(rawTools))
	for _, raw := range rawTools {
		var partial struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		if err := json.Unmarshal(raw, &partial); err != nil {
			return nil, fmt.Errorf("server %q: parse tool: %w", serverName, err)
		}
		if err := validateToolName(partial.Name); err != nil {
			return nil, fmt.Errorf("server %q tool %q: %w", serverName, partial.Name, err)
		}
		if len(partial.InputSchema) > maxSchemaBytes {
			logf("bzmux: warning: server %q tool %q inputSchema (%d bytes) exceeds %d; buzz-agent may replace it with {}\n",
				serverName, partial.Name, len(partial.InputSchema), maxSchemaBytes)
		}
		entries = append(entries, toolEntry{name: partial.Name, raw: raw})
	}
	return entries, nil
}

// validateToolName checks that name satisfies §5B rules:
//   - must not contain "__"
//   - len(bzmuxPrefix)+len(name) must not exceed maxQualifiedNameLen
func validateToolName(name string) error {
	if strings.Contains(name, "__") {
		return fmt.Errorf("contains \"__\" (buzz-agent hard-errors on this)")
	}
	if len(bzmuxPrefix)+len(name) > maxQualifiedNameLen {
		return fmt.Errorf("qualified name %q length %d exceeds %d",
			bzmuxPrefix+name, len(bzmuxPrefix)+len(name), maxQualifiedNameLen)
	}
	return nil
}

// mergeCatalogs merges tool entries from all children into a routing map and
// an ordered catalog slice.  It returns a hard error on any name collision.
func mergeCatalogs(children []*child) (map[string]*child, []toolEntry, error) {
	routes := make(map[string]*child)
	var catalog []toolEntry

	for _, c := range children {
		for _, t := range c.tools {
			if owner, exists := routes[t.name]; exists {
				return nil, nil, fmt.Errorf(
					"tool name collision: both server %q and server %q export tool %q — rename one in mcp-mux.json",
					owner.name, c.name, t.name)
			}
			routes[t.name] = c
			catalog = append(catalog, t)
		}
	}
	return routes, catalog, nil
}

// buildToolsListResult serialises the merged catalog into a tools/list result
// JSON object.
func buildToolsListResult(catalog []toolEntry) (json.RawMessage, error) {
	tools := make([]json.RawMessage, len(catalog))
	for i, t := range catalog {
		tools[i] = t.raw
	}
	type result struct {
		Tools []json.RawMessage `json:"tools"`
	}
	data, err := json.Marshal(result{Tools: tools})
	if err != nil {
		return nil, fmt.Errorf("marshal tools/list result: %w", err)
	}
	return data, nil
}
