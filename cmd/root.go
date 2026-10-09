// Package cmd holds the workspace-manager CLI.
package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/giantswarm/workspace-manager/internal/buildinfo"
)

// build is the running binary's identity: the release version, the commit
// and the build time, resolved by main from the ldflags values and the Go
// build info.
var build = buildinfo.Info{Version: buildinfo.DevVersion, Commit: buildinfo.UnknownCommit, Date: buildinfo.UnknownDate}

// SetBuild records the build identity the commands report.
func SetBuild(b buildinfo.Info) { build = b }

func newRootCmd() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:   "workspace-manager",
		Short: "Workspace service for the Agent Platform",
		Long: `workspace-manager manages the Agent Platform's workspaces: the sources an
agent session works on, and their provider sign-ins. It is one MCP server
behind muster, which forwards the person's Dex identity; every Kubernetes call
a request makes is made as that person.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	root.Version = build.Version
	root.SetVersionTemplate("workspace-manager version {{.Version}}\n")
	root.AddCommand(newServeCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "workspace-manager version %s\n  commit: %s\n  built:  %s\n", build.Version, build.Commit, build.Date)
		},
	}
}

// Execute runs the CLI.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
