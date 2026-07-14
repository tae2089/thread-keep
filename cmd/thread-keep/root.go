package main

import (
	"github.com/spf13/cobra"
	"github.com/tae2089/thread-keep/internal/cli"
)

func NewRoot(runner *cli.Runner) *cobra.Command {
	root := &cobra.Command{
		Use:           "thread-keep",
		Short:         "Versioned local code context",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("repo", "", "path to the Git worktree (default: current directory)")
	root.PersistentFlags().Bool("json", false, "emit versioned JSON output")
	root.AddCommand(cli.Commands(runner)...)
	return root
}
