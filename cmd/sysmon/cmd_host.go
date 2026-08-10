package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var hostCmd = &cobra.Command{
	Use:   "host",
	Short: "Display host information",
	Long:  "Display hostname, OS, platform, kernel version, uptime, and boot time.",
	RunE:  runHostCmd,
}

func init() {
	rootCmd.AddCommand(hostCmd)
}

func runHostCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(snap.Host)
		}
		if err := cli.RenderHost(cmd.OutOrStdout(), snap); err != nil {
			return fmt.Errorf("render host: %w", err)
		}
		return nil
	})
}
