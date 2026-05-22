package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/jobs"
	"github.com/victor-develop/advanced-tasker/internal/rollup"
	"github.com/victor-develop/advanced-tasker/internal/store"
	"github.com/victor-develop/advanced-tasker/internal/threads"
	"github.com/victor-develop/advanced-tasker/internal/tick"
)

// newDoctorCmd implements `harness doctor` per design/10 §Health checks.
func newDoctorCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Health check the harness state",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "state: %s\n", root)

			// Git health.
			r := gitops.Repo{Dir: root}
			clean, _ := r.IndexClean()
			fmt.Fprintf(out, "  git index clean: %v\n", clean)
			if sha, _ := r.HeadSHA(); sha != "" {
				fmt.Fprintf(out, "  HEAD: %s\n", strings.TrimSpace(sha))
			}
			if _, err := os.Stat(filepath.Join(root, ".git", "hooks", "post-commit")); err == nil {
				fmt.Fprintln(out, "  post-commit hook: installed")
			} else {
				fmt.Fprintln(out, "  post-commit hook: MISSING")
			}

			// Commander lease.
			if lease, _ := tick.CurrentLease(root); lease != nil {
				fmt.Fprintf(out, "  commander: held by %s until %s\n", lease.HeldBy, lease.LeaseUntil.Format(time.RFC3339))
			} else {
				fmt.Fprintln(out, "  commander: free")
			}

			// Job states.
			for _, st := range jobs.AllStates {
				ids, _ := jobs.ListByState(root, st)
				fmt.Fprintf(out, "  jobs/%s: %d\n", st, len(ids))
			}

			// Rollup validity sample.
			invalid := 0
			tlist, _ := threads.List(root)
			for _, id := range tlist {
				body, err := threads.ReadRollup(root, id)
				if err != nil {
					continue
				}
				r, err := rollup.Parse(body)
				if err != nil || r.Validate() != nil {
					invalid++
				}
			}
			fmt.Fprintf(out, "  threads: %d (invalid rollups: %d)\n", len(tlist), invalid)

			// Task schema sample.
			tasks, _ := store.LoadAll(root)
			badTasks := 0
			for _, t := range tasks {
				if err := t.Validate(); err != nil {
					badTasks++
				}
			}
			fmt.Fprintf(out, "  tasks: %d (invalid: %d)\n", len(tasks), badTasks)

			fmt.Fprintln(out, "doctor: OK")
			return nil
		},
	}
}
