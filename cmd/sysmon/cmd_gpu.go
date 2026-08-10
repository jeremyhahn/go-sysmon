package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var gpuIndex int

var gpuCmd = &cobra.Command{
	Use:   "gpu",
	Short: "Display GPU information",
	Long:  "Display NVIDIA GPU utilization, memory, clocks, power, PCIe throughput, and ECC status.",
	RunE:  runGPUCmd,
}

func init() {
	gpuCmd.Flags().IntVar(&gpuIndex, "index", -1, "GPU index to display (0-based); omit to show all GPUs")
	rootCmd.AddCommand(gpuCmd)
}

func runGPUCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if gpuIndex >= 0 {
			gpu, filterErr := filterGPUByIndex(snap.GPUs, gpuIndex)
			if filterErr != nil {
				return filterErr
			}
			snap.GPUs = []types.GPUInfo{gpu}
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				GPUs interface{} `json:"gpus"`
			}{
				GPUs: snap.GPUs,
			})
		}
		if err := cli.RenderGPU(cmd.OutOrStdout(), snap); err != nil {
			return fmt.Errorf("render gpu: %w", err)
		}
		return nil
	})
}

func filterGPUByIndex(gpus []types.GPUInfo, index int) (types.GPUInfo, error) {
	for _, g := range gpus {
		if g.Index == index {
			return g, nil
		}
	}
	return types.GPUInfo{}, &types.GPUIndexNotFoundError{
		Index:     index,
		Available: len(gpus),
	}
}
