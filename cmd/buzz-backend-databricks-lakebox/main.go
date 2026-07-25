// Command buzz-backend-databricks-lakebox is a Buzz agent provider for Databricks
// Lakebox sandboxes (PLAN.md). With no arguments it runs in provider mode:
// read one JSON request from stdin, dispatch on "op", write one JSON
// response to stdout, and always exit 0 for handled cases
// (docs/CONTRACT.md §2). With arguments it runs as an operator CLI (cobra)
// for local diagnostics and (from M1/M2 onward) lifecycle management.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/IceRhymers/buzz-lakebox/internal/deployflow"
	"github.com/IceRhymers/buzz-lakebox/internal/doctor"
	"github.com/IceRhymers/buzz-lakebox/internal/lakebox"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/provider"
	"github.com/IceRhymers/buzz-lakebox/internal/sshx"
	"github.com/IceRhymers/buzz-lakebox/internal/version"
)

func main() {
	if len(os.Args) <= 1 {
		runProvider()
		return
	}
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newDeployer builds a deployflow.Deployer wired to the real databricks
// CLI/ssh transports (C4 cleanup: previously built identically in two
// places — runProvider and newDeployCmd).
func newDeployer() *deployflow.Deployer {
	return deployflow.New(lakebox.New(), sshx.New())
}

// runProvider is provider mode: no argv, one JSON request on stdin, one
// JSON response on stdout.
func runProvider() {
	deployer := newDeployer()
	if err := provider.Run(os.Stdin, os.Stdout, deployer.Deploy); err != nil {
		// Only unhandleable I/O failures (reading stdin, writing stdout)
		// reach here — every parseable request is a "handled case" per
		// docs/CONTRACT.md §2 and already got a written {"ok":false,...}
		// response with a nil error from provider.Run.
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var profile string

	root := &cobra.Command{
		Use:           "buzz-backend-databricks-lakebox",
		Short:         "Deploys Buzz agents into Databricks Lakebox sandboxes",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().StringVar(&profile, "profile", version.DefaultProfile, "Databricks CLI profile to use")

	root.AddCommand(newVersionCmd())
	root.AddCommand(newDoctorCmd(&profile))
	root.AddCommand(newDeployCmd(&profile))
	root.AddCommand(newNotImplementedCmd("status", "M2"))
	root.AddCommand(newNotImplementedCmd("stop", "M2"))
	root.AddCommand(newNotImplementedCmd("start", "M2"))
	root.AddCommand(newNotImplementedCmd("logs", "M2"))
	root.AddCommand(newNotImplementedCmd("undeploy", "M2"))

	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the provider version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return nil
		},
	}
}

func newDoctorCmd(profile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that the environment is ready to deploy (read-only, non-destructive)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cli := lakebox.New()
			summary := doctor.Run(context.Background(), cmd.OutOrStdout(), cli, *profile)

			data, err := json.Marshal(summary)
			if err != nil {
				return fmt.Errorf("marshal doctor summary: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(data))

			if !summary.Ok {
				return fmt.Errorf("doctor found one or more problems; see checks above")
			}
			return nil
		},
	}
}

// newDeployCmd implements the operator `deploy --payload-file <f>`
// subcommand (docs/PLAN.md §3.3): the same deploy flow the provider-mode
// stdin path runs, for testing without the desktop. The file may contain
// either the full request envelope ({"agent":...,"provider_config":...})
// or a bare agent object; presence of an "agent" key disambiguates.
func newDeployCmd(profile *string) *cobra.Command {
	var payloadFile string

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy an agent from a payload file (for testing without the desktop)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if payloadFile == "" {
				return fmt.Errorf("--payload-file is required")
			}
			data, err := os.ReadFile(payloadFile)
			if err != nil {
				return fmt.Errorf("read payload file: %w", err)
			}

			req, perr := parseOperatorDeployPayload(data)
			if perr != nil {
				return printDeployResult(cmd, "", perr)
			}
			if req.ProviderConfig.Profile == "" {
				req.ProviderConfig.Profile = *profile
			}

			deployer := newDeployer()
			agentID, derr := deployer.Deploy(req)
			return printDeployResult(cmd, agentID, derr)
		},
	}
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", `path to a JSON deploy payload: either the full envelope ({"agent":...,"provider_config":...}) or a bare agent object`)
	return cmd
}

// parseOperatorDeployPayload accepts either the full request envelope or a
// bare agent object, per newDeployCmd's doc comment.
func parseOperatorDeployPayload(data []byte) (*payload.DeployRequest, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse payload file: %w", err)
	}
	if _, hasAgent := probe["agent"]; hasAgent {
		return payload.ParseDeployRequest(data)
	}
	var agent payload.Agent
	if err := json.Unmarshal(data, &agent); err != nil {
		return nil, fmt.Errorf("parse payload file as a bare agent object: %w", err)
	}
	return &payload.DeployRequest{Op: "deploy", Agent: agent}, nil
}

// printDeployResult prints the same {"ok":true,"agent_id":...} /
// {"ok":false,"error":...} response shape provider mode emits
// (docs/CONTRACT.md §4). Delegates to provider.MarshalDeployResult (C3
// cleanup) so there is exactly one rendering of the frozen wire shape,
// rather than a second hand-rolled map literal here.
func printDeployResult(cmd *cobra.Command, agentID string, err error) error {
	_, ferr := fmt.Fprintln(cmd.OutOrStdout(), string(provider.MarshalDeployResult(agentID, err)))
	return ferr
}

// newNotImplementedCmd registers a lifecycle subcommand (status, stop,
// start, logs, undeploy) that exists so `--help` and shell completion are
// accurate ahead of its real milestone, but always fails clearly until
// then.
func newNotImplementedCmd(name, milestone string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: fmt.Sprintf("%s (not implemented until %s)", name, milestone),
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("%s not implemented (%s)", name, milestone)
		},
	}
}
