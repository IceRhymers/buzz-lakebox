// Package sshx runs commands inside a Lakebox sandbox over
// `databricks sandbox ssh <id> -p <profile> -- <cmd>`. This transport is
// VERIFIED byte-identical for stdin (docs/M05_PROBE_RESULTS.md §1), which
// is why it is the only path secrets travel: callers MUST use
// RunWithStdin — never interpolate a secret into cmd, since cmd becomes an
// argv element of the child process (visible via /proc, ps, shell
// history on some configurations, and any argv-recording layer) whereas
// stdin is not.
package sshx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// defaultBin is the binary name resolved via PATH when Client.Bin is unset.
const defaultBin = "databricks"

// Client is a thin exec wrapper over `databricks sandbox ssh`. Bin is
// exported so tests can point it at a fake PATH shim instead of a real
// installation, mirroring internal/lakebox.CLI's injectable-Bin pattern
// (PLAN.md §7).
type Client struct {
	// Bin is the binary name or path invoked for every command. Empty
	// means "databricks" resolved via PATH.
	Bin string
}

// New returns a Client that invokes the real "databricks" binary on PATH.
func New() *Client {
	return &Client{Bin: defaultBin}
}

func (c *Client) binName() string {
	if c.Bin == "" {
		return defaultBin
	}
	return c.Bin
}

// Run executes cmd inside sandbox id via
// `databricks sandbox ssh <id> -p <profile> -- <cmd>`, with no stdin
// attached (the child sees EOF/empty input immediately, matching
// os/exec's nil-Stdin-connects-to-/dev/null behavior). cmd must never
// contain a secret; use RunWithStdin for that.
func (c *Client) Run(ctx context.Context, profile, id, cmd string) (string, error) {
	return c.run(ctx, profile, id, cmd, nil)
}

// RunWithStdin executes cmd inside sandbox id, piping stdin through to the
// remote command's standard input. This is the ONLY sanctioned path for
// secrets (docs/M05_PROBE_RESULTS.md §1, docs/PLAN.md §4.4 step 7): the
// remote cmd should read from its stdin (e.g. `cat > file`) rather than
// have the secret embedded in cmd itself.
func (c *Client) RunWithStdin(ctx context.Context, profile, id, cmd string, stdin io.Reader) (string, error) {
	return c.run(ctx, profile, id, cmd, stdin)
}

func (c *Client) run(ctx context.Context, profile, id, cmd string, stdin io.Reader) (string, error) {
	args := []string{"sandbox", "ssh", id, "-p", profile, "--", cmd}
	command := exec.CommandContext(ctx, c.binName(), args...)
	if stdin != nil {
		command.Stdin = stdin
	}

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	if err != nil {
		combined := strings.TrimSpace(stdout.String() + stderr.String())
		return stdout.String(), fmt.Errorf("ssh %s -p %s: %w (output: %s)", id, profile, err, combined)
	}
	return stdout.String(), nil
}
