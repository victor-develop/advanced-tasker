// Package cli wires the harness subcommand tree using spf13/cobra and
// glues the verbs to the state-mutation packages. Each subcommand here
// follows the same shape:
//
//  1. Resolve state root and confirm `harness init` has run (where it
//     matters).
//  2. Acquire the state lock for mutating verbs (read-only verbs skip).
//  3. Validate args, mutate files, git commit with the
//     `<verb> <object>: <summary>` message format from design/02.
//
// Exit codes follow design/03 §Output and exit codes.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is overwritten at build time via -ldflags. For dev builds we
// fall back to "dev".
var Version = "dev"

// Exit codes per design/03.
const (
	ExitOK          = 0
	ExitUsage       = 1
	ExitValidation  = 2
	ExitLock        = 3
	ExitGit         = 4
)

// Options collects the global flags every subcommand respects.
type Options struct {
	StateRoot string
	JSON      bool
	DryRun    bool
}

// New builds the cobra command tree.
func New() *cobra.Command {
	opts := &Options{}

	root := &cobra.Command{
		Use:           "harness",
		Short:         "Long-horizon LLM task harness",
		Long:          "harness is the single sanctioned mutator of a state/ directory used by the long-horizon-task harness. All side effects (task mutations, dispatches, outbox sends) flow through this CLI.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.StateRoot, "state-dir", "", "override state directory (default $HARNESS_STATE or ./state)")
	root.PersistentFlags().BoolVar(&opts.JSON, "json", false, "emit machine-readable output")
	root.PersistentFlags().BoolVar(&opts.DryRun, "dry-run", false, "print intended actions without applying")

	root.AddCommand(
		newInitCmd(opts),
		newBootCmd(opts),
		newVersionCmd(opts),
		newConfigCmd(opts),
		newGoalCmd(opts),
		newTaskCmd(opts),
		newLinkCmd(opts),
		newUnlinkCmd(opts),
		newDepsCmd(opts),
		newRenderCmd(opts),
		newPickupCmd(opts),
		newInboxCmd(opts),
		newTriageCmd(opts),
		newDispatchCmd(opts),
		newJobCmd(opts),
		newSubmitReportCmd(opts),
		newReviewCmd(opts),
		newClaimCmd(opts),
		newReleaseCmd(opts),
		newOutboxCmd(opts),
		newRollupCmd(opts),
		newTickCmd(opts),
		newTickLogCmd(opts),
		newEngineCmd(opts),
		newTelemetryCmd(opts),
		newDoctorCmd(opts),
		newAutopilotCmd(opts),
		newAuditCmd(opts),
		newAuditDaemonCmd(opts),
		newWatchCmd(opts),
		newUnwatchCmd(opts),
		newSourcesCmd(opts),
		newPinCmd(opts),
		newNoteCmd(opts),
		newThreadCmd(opts),
	)
	return root
}

// Execute runs the CLI. The returned int is the process exit code.
func Execute() int {
	cmd := New()
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	if err := cmd.Execute(); err != nil {
		// All handlers wrap their non-usage errors in cliError so we can
		// surface a deterministic exit code.
		fmt.Fprintln(os.Stderr, err)
		var ce cliError
		if asCLIError(err, &ce) {
			return ce.code
		}
		return ExitUsage
	}
	return ExitOK
}

// cliError carries an exit code through cobra without losing the message.
type cliError struct {
	code int
	msg  string
}

func (e cliError) Error() string { return e.msg }

func errf(code int, format string, args ...any) error {
	return cliError{code: code, msg: fmt.Sprintf(format, args...)}
}

func asCLIError(err error, out *cliError) bool {
	if err == nil {
		return false
	}
	if ce, ok := err.(cliError); ok {
		*out = ce
		return true
	}
	return false
}
