// autosk — minimal task tracker for AI coding agents (proto-v2 CLI).
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Globals set by persistent flags.
var (
	flagJSON  bool
	flagQuiet bool
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "autosk",
		Short:         "Minimal task tracker for AI coding agents",
		Long:          "autosk tracks tasks with a tiny, agent-friendly CLI backed by the autoskd daemon.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "emit machine-readable JSON")
	root.PersistentFlags().BoolVarP(&flagQuiet, "quiet", "q", false, "suppress non-essential output")

	root.AddCommand(
		newVersionCmd(),
		newCreateCmd(),
		newShowCmd(),
		newListCmd(),
		newReadyCmd(),
		newNextCmd(),
		newDoneCmd(),
		newCancelCmd(),
		newReopenCmd(),
		newUpdateCmd(),
		newBlockCmd(),
		newUnblockCmd(),
		newDepCmd(),
		newMetadataCmd(),
		newInitCmd(),
		newExtCmd(),
		newProjectCmd(),
		newSessionCmd(),
		newWorkflowCmd(),
		newEnrollCmd(),
		newResumeCmd(),
		newCommentCmd(),
		newWatchCmd(),
		newLazyCmd(),
	)
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		if errors.Is(err, errSilentExit1) {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "autosk: "+cleanRPCError(err).Error())
		os.Exit(1)
	}
}
