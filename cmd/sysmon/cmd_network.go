package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var networkIndex int

var networkCmd = &cobra.Command{
	Use:   "network",
	Short: "Display network interface information",
	Long:  "Display network interface identity, addresses, flags, speed, and traffic counters.",
	RunE:  runNetworkCmd,
}

func init() {
	networkCmd.Flags().IntVar(&networkIndex, "index", -1, "network interface index to display (0-based); omit to show all")
	rootCmd.AddCommand(networkCmd)
}

func runNetworkCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if networkIndex >= 0 {
			net, filterErr := filterNetworkByIndex(snap.Networks, networkIndex)
			if filterErr != nil {
				return filterErr
			}
			snap.Networks = []types.NetworkInfo{net}
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(snap.Networks)
		}
		if err := cli.RenderNetwork(cmd.OutOrStdout(), snap); err != nil {
			return fmt.Errorf("render network: %w", err)
		}
		return nil
	})
}

func filterNetworkByIndex(networks []types.NetworkInfo, index int) (types.NetworkInfo, error) {
	for _, n := range networks {
		if n.Index == index {
			return n, nil
		}
	}
	return types.NetworkInfo{}, &types.NetworkIndexNotFoundError{
		Index:     index,
		Available: len(networks),
	}
}
