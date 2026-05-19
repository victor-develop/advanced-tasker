package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/render"
)

// newRenderCmd implements: harness render dashboard|brief
func newRenderCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{
		Use:   "render",
		Short: "Render read-only views over state/",
	}
	var budget int
	dashCmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Render the commander dashboard",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			out, err := render.Dashboard(root, render.DashboardOptions{
				Budget: budget,
				Now:    time.Now().UTC(),
			})
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
	dashCmd.Flags().IntVar(&budget, "budget", 8000, "soft token budget")

	briefCmd := &cobra.Command{
		Use:   "brief",
		Short: "Render the cold-start brief view",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			out, err := render.Brief(root)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
	c.AddCommand(dashCmd, briefCmd)
	callAttachers(opts, c)
	return c
}

// newPickupCmd implements: harness pickup
func newPickupCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "pickup",
		Short: "List available roles (no recommendation)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			out, err := render.Pickup(root)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
}
