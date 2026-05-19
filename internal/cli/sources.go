package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/sources"
	"github.com/victor-develop/advanced-tasker/internal/state"
)

// addConfigInitSubcommand extends `harness config` with `init <source>`.
// Called from newConfigCmd.
func addConfigInitSubcommand(c *cobra.Command, opts *Options) {
	c.AddCommand(&cobra.Command{
		Use:   "init <source>",
		Short: "Seed state/sources/<source>/config.yaml with a documented stub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			src, err := sources.ParseSource(args[0])
			if err != nil {
				return errf(ExitUsage, "%v", err)
			}
			return state.WithStateLock(root, func() error {
				created, err := sources.Init(root, src)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if !created {
					fmt.Fprintf(cmd.OutOrStdout(), "already exists: %s\n", sources.ConfigPath(root, src))
					return nil
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would seed %s\n", sources.ConfigPath(root, src))
					return nil
				}
				r := gitops.Repo{Dir: root}
				_ = r.Add(fmt.Sprintf("sources/%s/config.yaml", src))
				if _, err := r.Commit(fmt.Sprintf("config init %s: seed source stub", src)); err != nil {
					// ignore nothing-to-commit
				}
				fmt.Fprintf(cmd.OutOrStdout(), "seeded %s\n", sources.ConfigPath(root, src))
				return nil
			})
		},
	})
}

// newWatchCmd implements `harness watch slack-channel|github-repo ...`.
func newWatchCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "watch", Short: "Add a source target to the poller watch list"}
	var reason string
	slackCh := &cobra.Command{
		Use:   "slack-channel <channel-id>",
		Short: "Watch a Slack channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			return state.WithStateLock(root, func() error {
				if err := sources.WatchChannel(root, args[0], reason); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				r := gitops.Repo{Dir: root}
				_ = r.Add("sources/slack/config.yaml")
				_, _ = r.Commit(fmt.Sprintf("watch slack-channel %s", args[0]))
				fmt.Fprintf(cmd.OutOrStdout(), "watching slack/%s\n", args[0])
				return nil
			})
		},
	}
	slackCh.Flags().StringVar(&reason, "reason", "", "free-form reason")
	githubRepo := &cobra.Command{
		Use:   "github-repo <owner/repo>",
		Short: "Watch a GitHub repo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if !strings.Contains(args[0], "/") {
				return errf(ExitValidation, "repo must be in owner/repo form")
			}
			return state.WithStateLock(root, func() error {
				if err := sources.WatchRepo(root, args[0]); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				r := gitops.Repo{Dir: root}
				_ = r.Add("sources/github/config.yaml")
				_, _ = r.Commit(fmt.Sprintf("watch github-repo %s", args[0]))
				fmt.Fprintf(cmd.OutOrStdout(), "watching github/%s\n", args[0])
				return nil
			})
		},
	}
	c.AddCommand(slackCh, githubRepo)
	return c
}

// newUnwatchCmd is the symmetric remove.
func newUnwatchCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "unwatch", Short: "Remove a source target from the poller watch list"}
	c.AddCommand(
		&cobra.Command{
			Use:   "slack-channel <channel-id>",
			Short: "Unwatch a Slack channel",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				return state.WithStateLock(root, func() error {
					if err := sources.UnwatchChannel(root, args[0]); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					r := gitops.Repo{Dir: root}
					_ = r.Add("sources/slack/config.yaml")
					_, _ = r.Commit(fmt.Sprintf("unwatch slack-channel %s", args[0]))
					fmt.Fprintln(cmd.OutOrStdout(), "ok")
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "github-repo <owner/repo>",
			Short: "Unwatch a GitHub repo",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				return state.WithStateLock(root, func() error {
					if err := sources.UnwatchRepo(root, args[0]); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					r := gitops.Repo{Dir: root}
					_ = r.Add("sources/github/config.yaml")
					_, _ = r.Commit(fmt.Sprintf("unwatch github-repo %s", args[0]))
					fmt.Fprintln(cmd.OutOrStdout(), "ok")
					return nil
				})
			},
		},
	)
	return c
}

// newSourcesCmd implements `harness sources ls`.
func newSourcesCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "sources", Short: "Inspect configured sources"}
	c.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List watched targets",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			items, err := sources.Summary(root)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			for _, x := range items {
				fmt.Fprintln(cmd.OutOrStdout(), x)
			}
			return nil
		},
	})
	return c
}
