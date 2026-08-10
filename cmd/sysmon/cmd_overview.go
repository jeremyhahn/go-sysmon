package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var overviewCmd = &cobra.Command{
	Use:   "overview",
	Short: "Display a one-shot system overview",
	Long:  "Collect and display a complete system overview including host, CPU, memory, storage, and network.",
	RunE:  runOverviewCmd,
}

func init() {
	rootCmd.AddCommand(overviewCmd)
}

func runOverviewCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(snap)
		}
		if err := cli.RenderOverview(cmd.OutOrStdout(), snap); err != nil {
			return fmt.Errorf("render overview: %w", err)
		}
		return nil
	})
}
