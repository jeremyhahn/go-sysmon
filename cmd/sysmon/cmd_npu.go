package main

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var npuIndex int

var npuCmd = &cobra.Command{
	Use:   "npu",
	Short: "Display NPU information",
	Long:  "Display compute accelerator (NPU) identity, power residency, compute utilization, and per-client usage.",
	RunE:  runNPUCmd,
}

func init() {
	npuCmd.Flags().IntVar(&npuIndex, "index", -1, "NPU index to display (0-based); omit to show all NPUs")
	rootCmd.AddCommand(npuCmd)
}

func runNPUCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if npuIndex >= 0 {
			npu, filterErr := filterNPUByIndex(snap.NPUs, npuIndex)
			if filterErr != nil {
				return filterErr
			}
			snap.NPUs = []types.NPUInfo{npu}
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				NPUs interface{} `json:"npus"`
			}{
				NPUs: snap.NPUs,
			})
		}
		if err := cli.RenderNPU(cmd.OutOrStdout(), snap); err != nil {
			return &cli.RenderError{Function: "npu", Cause: err}
		}
		return nil
	})
}

func filterNPUByIndex(npus []types.NPUInfo, index int) (types.NPUInfo, error) {
	for _, n := range npus {
		if n.Index == index {
			return n, nil
		}
	}
	return types.NPUInfo{}, &types.NPUIndexNotFoundError{
		Index:     index,
		Available: len(npus),
	}
}
