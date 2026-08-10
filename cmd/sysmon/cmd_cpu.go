package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var cpuIndex int

var cpuCmd = &cobra.Command{
	Use:   "cpu",
	Short: "Display CPU information",
	Long:  "Display CPU topology, model, per-core usage, and frequency information.",
	RunE:  runCPUCmd,
}

func init() {
	cpuCmd.Flags().IntVar(&cpuIndex, "index", -1, "CPU index to display (0-based); omit to show all")
	rootCmd.AddCommand(cpuCmd)
}

func runCPUCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if cpuIndex >= 0 {
			cpu, filterErr := filterCPUByIndex(snap.CPUs, cpuIndex)
			if filterErr != nil {
				return filterErr
			}
			snap.CPUs = []types.CPUInfo{cpu}
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				Summary interface{} `json:"cpu_summary"`
				CPUs    interface{} `json:"cpus"`
			}{
				Summary: snap.CPUSummary,
				CPUs:    snap.CPUs,
			})
		}
		if err := cli.RenderCPU(cmd.OutOrStdout(), snap); err != nil {
			return fmt.Errorf("render cpu: %w", err)
		}
		return nil
	})
}

func filterCPUByIndex(cpus []types.CPUInfo, index int) (types.CPUInfo, error) {
	for _, c := range cpus {
		if c.Index == index {
			return c, nil
		}
	}
	return types.CPUInfo{}, &types.CPUIndexNotFoundError{
		Index:     index,
		Available: len(cpus),
	}
}
