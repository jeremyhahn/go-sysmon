package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var vmIndex int

var vmsCmd = &cobra.Command{
	Use:     "vms",
	Aliases: []string{"vm"},
	Short:   "Display virtual machines running on the host",
	Long: "Display virtual machines running on the host, described from the hypervisor " +
		"process: guest name, UUID, vCPUs, configured and resident memory, and the host " +
		"NICs and disk images backing the guest. Guest-internal metrics are not reported.",
	RunE: runVMsCmd,
}

func init() {
	vmsCmd.Flags().IntVar(&vmIndex, "index", -1,
		"VM index to display (0-based); omit to show all")
	rootCmd.AddCommand(vmsCmd)
}

func runVMsCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if vmIndex >= 0 {
			vm, filterErr := filterVMByIndex(snap.Virt.VMs, vmIndex)
			if filterErr != nil {
				return filterErr
			}
			snap.Virt.VMs = []types.VMInfo{vm}
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				VMs []types.VMInfo `json:"vms"`
			}{VMs: snap.Virt.VMs})
		}
		if err := cli.RenderVMs(cmd.OutOrStdout(), snap); err != nil {
			return fmt.Errorf("render vms: %w", err)
		}
		return nil
	})
}

// filterVMByIndex returns the VM with the given index.
func filterVMByIndex(vms []types.VMInfo, index int) (types.VMInfo, error) {
	for _, vm := range vms {
		if vm.Index == index {
			return vm, nil
		}
	}
	return types.VMInfo{}, &types.VMIndexNotFoundError{
		Index:     index,
		Available: len(vms),
	}
}
