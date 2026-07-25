// Command buzz-backend-databricks is a Buzz agent provider for Databricks
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

	"github.com/IceRhymers/buzz-lakebox/internal/doctor"
	"github.com/IceRhymers/buzz-lakebox/internal/lakebox"
	"github.com/IceRhymers/buzz-lakebox/internal/provider"
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

// runProvider is provider mode: no argv, one JSON request on stdin, one
// JSON response on stdout. deploy is nil at M0 (stub); M1 wires the real
// deployflow.DeployFunc in here.
func runProvider() {
	if err := provider.Run(os.Stdin, os.Stdout, nil); err != nil {
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
		Use:           "buzz-backend-databricks",
		Short:         "Deploys Buzz agents into Databricks Lakebox sandboxes",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().StringVar(&profile, "profile", "DEFAULT", "Databricks CLI profile to use")

	root.AddCommand(newVersionCmd())
	root.AddCommand(newDoctorCmd(&profile))
	root.AddCommand(newDeployCmd())
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

func newDeployCmd() *cobra.Command {
	var payloadFile string

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy an agent from a payload file (not implemented until M1)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("deploy not implemented (M1)")
		},
	}
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", "path to a JSON deploy payload, for testing without the desktop (M1)")
	return cmd
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
