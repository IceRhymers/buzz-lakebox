// Package muxcfg defines the frozen JSON schema for mcp-mux.json, the
// configuration file read by the bzmux multiplexer at runtime and written by
// the provider renderer at deploy time.
//
// Each child MCP server's process environment is governed by a per-child
// allowlist (Decision B1, least-privilege):
//
//   - Inherit lists the names of environment variables that bzmux should
//     forward from its own process environment to the child. Use this for
//     secrets that bzmux already receives via the agent's PASSTHROUGH_ENV
//     (e.g. BUZZ_RELAY_URL, NOSTR_PRIVATE_KEY). Only variables explicitly
//     listed are forwarded — bzmux never performs a blanket os.Environ()
//     pass-through, so a secret never reaches a child that did not opt in.
//
//   - Set carries variables that must reach the child regardless of whether
//     bzmux inherited them (e.g. a token a third-party server needs). Values
//     may carry secrets; the provider renderer must redact them in logs and
//     the config file is written 0600. Set overrides Inherit: if a variable
//     name appears in both, the Set value wins.
//
// This package is importable by both cmd/bzmux (reader) and
// internal/install (writer). It is deliberately stdlib-only and carries no
// dependencies outside the standard library.
package muxcfg

// Config is the top-level structure of mcp-mux.json. It lists every child MCP
// server that bzmux should spawn and multiplex.
type Config struct {
	// Servers is the ordered list of child MCP servers. Each entry describes
	// one child process: how to start it and what environment it receives.
	Servers []Server `json:"servers"`
}

// Server describes one child MCP server that bzmux spawns and communicates
// with over stdio JSON-RPC.
type Server struct {
	// Name is a human-readable label for this server used in log output and
	// error messages. It must be unique across all entries in Config.Servers.
	Name string `json:"name"`

	// Command is the executable to run (bare name resolved via PATH, or an
	// absolute path). Required.
	Command string `json:"command"`

	// Args is the argument list passed to Command. May be omitted when the
	// server requires no arguments.
	Args []string `json:"args,omitempty"`

	// Env controls the environment delivered to this child. bzmux never
	// performs a blanket os.Environ() forward; the child receives only the
	// variables the allowlist grants.
	Env ServerEnv `json:"env"`
}

// ServerEnv is the per-child environment allowlist. It separates forwarded
// inherited variables from explicitly supplied values so that secrets from
// bzmux's own environment can be selectively delegated without wholesale
// leaking to every child process.
type ServerEnv struct {
	// Inherit is a list of environment variable names that bzmux should
	// forward to this child from its own process environment. Only names
	// present in bzmux's env are forwarded; missing names are silently
	// skipped. Omit entirely when no inheritance is needed.
	Inherit []string `json:"inherit,omitempty"`

	// Set is a map of environment variables supplied explicitly to this
	// child, independent of bzmux's own environment. Values may carry
	// secrets and must be redacted in logs. Set takes precedence over
	// Inherit: if a variable name appears in both, the Set value is used.
	// Omit entirely when no explicit variables are needed.
	Set map[string]string `json:"set,omitempty"`
}
