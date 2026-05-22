package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// newPinCmd implements `harness pin <text>` and pin ls/rm/renew.
// Pins live as files under inbox/human/ with YAML frontmatter.
func newPinCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{
		Use:   "pin",
		Short: "Manage human directive pins",
	}
	var scope, ttl string
	create := &cobra.Command{
		Use:   "create <text>",
		Short: "Create a pin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPinCreate(opts, cmd, args[0], scope, ttl)
		},
	}
	create.Flags().StringVar(&scope, "scope", "global", "T-<n>|<thread-id>|global")
	create.Flags().StringVar(&ttl, "ttl", "7d", "time-to-live (Go duration)")
	c.AddCommand(create)

	// Top-level pin "<text>" should behave like pin create
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return runPinCreate(opts, cmd, args[0], scope, ttl)
	}
	c.Flags().StringVar(&scope, "scope", "global", "T-<n>|<thread-id>|global")
	c.Flags().StringVar(&ttl, "ttl", "7d", "TTL")

	c.AddCommand(
		&cobra.Command{
			Use:   "ls",
			Short: "List active pins",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				dir := filepath.Join(root, "inbox", "human")
				entries, err := os.ReadDir(dir)
				if err != nil {
					return nil
				}
				var names []string
				for _, e := range entries {
					if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
						names = append(names, e.Name())
					}
				}
				sort.Strings(names)
				for _, n := range names {
					fmt.Fprintln(cmd.OutOrStdout(), n)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "rm <pin-id>",
			Short: "Remove a pin",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				return os.Remove(filepath.Join(root, "inbox", "human", args[0]))
			},
		},
		&cobra.Command{
			Use:   "renew <pin-id>",
			Short: "Renew a pin (touches mtime)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				p := filepath.Join(root, "inbox", "human", args[0])
				return os.Chtimes(p, time.Now(), time.Now())
			},
		},
	)
	return c
}

func runPinCreate(opts *Options, cmd *cobra.Command, text, scope, ttl string) error {
	root, err := requireInitialized(opts)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "inbox", "human")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errf(ExitValidation, "%v", err)
	}
	id := fmt.Sprintf("pin-%d.md", time.Now().UnixNano())
	body := fmt.Sprintf("---\npriority: pin\nscope: %s\nttl: %s\ncreated_at: %s\n---\n%s\n",
		scope, ttl, time.Now().UTC().Format(time.RFC3339), text)
	if err := os.WriteFile(filepath.Join(dir, id), []byte(body), 0o644); err != nil {
		return errf(ExitValidation, "%v", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), id)
	return nil
}

// newNoteCmd implements `harness note <thread-id> <text>` (delegated to
// rollup pin).
func newNoteCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "note <thread-id> <text>",
		Short: "Add a human-pinned Verbatim line to a thread rollup",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SetArgs(append([]string{"rollup", "pin"}, args...))
			return cmd.Root().Execute()
		},
	}
}
