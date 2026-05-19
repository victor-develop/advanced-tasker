package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/config"
	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/state"
)

func newConfigCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Read or update state/config.yaml",
	}
	c.AddCommand(
		&cobra.Command{
			Use:   "get <dotted.key>",
			Short: "Print the value at the dotted key",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				tree, err := config.Load(filepath.Join(root, "config.yaml"))
				if err != nil {
					return errf(ExitUsage, "%v", err)
				}
				v, err := config.Get(tree, args[0])
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						return errf(ExitValidation, "config key not found: %s", args[0])
					}
					return errf(ExitValidation, "%v", err)
				}
				s, err := config.Format(v)
				if err != nil {
					return errf(ExitUsage, "%v", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), s)
				return nil
			},
		},
		&cobra.Command{
			Use:   "set <dotted.key> <value>",
			Short: "Update the value at the dotted key and commit",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				key, val := args[0], args[1]
				return state.WithStateLock(root, func() error {
					cfgPath := filepath.Join(root, "config.yaml")
					tree, err := config.Load(cfgPath)
					if err != nil {
						return errf(ExitUsage, "%v", err)
					}
					if err := config.Set(tree, key, val); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					if opts.DryRun {
						fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would set %s = %s\n", key, val)
						return nil
					}
					if err := config.Save(cfgPath, tree); err != nil {
						return errf(ExitGit, "%v", err)
					}
					r := gitops.Repo{Dir: root}
					if err := r.Add("config.yaml"); err != nil {
						return errf(ExitGit, "%v", err)
					}
					msg := fmt.Sprintf("config set %s: %s", key, val)
					if _, err := r.Commit(msg); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
						return errf(ExitGit, "%v", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "set %s\n", key)
					return nil
				})
			},
		},
	)
	addConfigInitSubcommand(c, opts)
	return c
}

// requireInitialized resolves the state root and returns an error if no
// init has been run there.
func requireInitialized(opts *Options) (string, error) {
	root, err := state.Resolve(opts.StateRoot)
	if err != nil {
		return "", errf(ExitUsage, "resolve state root: %v", err)
	}
	if !state.IsInitialized(root) {
		return "", errf(ExitUsage, "state not initialized at %s; run `harness init` first", root)
	}
	return root, nil
}
