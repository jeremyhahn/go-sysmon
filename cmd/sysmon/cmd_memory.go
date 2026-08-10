package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var memoryIndex int

var memoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Display memory information",
	Long:  "Display RAM usage, swap usage, and installed DIMM details.",
	RunE:  runMemoryCmd,
}

func init() {
	memoryCmd.Flags().IntVar(&memoryIndex, "index", -1, "DIMM index to display (0-based); omit to show all")
	rootCmd.AddCommand(memoryCmd)
}

func runMemoryCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if memoryIndex >= 0 {
			dimm, filterErr := filterDIMMByIndex(snap.Memory.DIMMs, memoryIndex)
			if filterErr != nil {
				return filterErr
			}
			snap.Memory.DIMMs = []types.DIMMInfo{dimm}
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(snap.Memory)
		}
		if err := cli.RenderMemory(cmd.OutOrStdout(), snap); err != nil {
			return fmt.Errorf("render memory: %w", err)
		}
		return nil
	})
}

func filterDIMMByIndex(dimms []types.DIMMInfo, index int) (types.DIMMInfo, error) {
	for _, d := range dimms {
		if d.Index == index {
			return d, nil
		}
	}
	return types.DIMMInfo{}, &types.DIMMIndexNotFoundError{
		Index:     index,
		Available: len(dimms),
	}
}
