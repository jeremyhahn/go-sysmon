package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var storageIndex int

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Display disk and storage information",
	Long:  "Display disk identity, partition layout, I/O counters, and SMART health status.",
	RunE:  runStorageCmd,
}

func init() {
	storageCmd.Flags().IntVar(&storageIndex, "index", -1, "disk index to display (0-based); omit to show all")
	rootCmd.AddCommand(storageCmd)
}

func runStorageCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if storageIndex >= 0 {
			disk, filterErr := filterDiskByIndex(snap.Disks, storageIndex)
			if filterErr != nil {
				return filterErr
			}
			snap.Disks = []types.DiskInfo{disk}
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(snap.Disks)
		}
		if err := cli.RenderStorage(cmd.OutOrStdout(), snap); err != nil {
			return fmt.Errorf("render storage: %w", err)
		}
		return nil
	})
}

func filterDiskByIndex(disks []types.DiskInfo, index int) (types.DiskInfo, error) {
	for _, d := range disks {
		if d.Index == index {
			return d, nil
		}
	}
	return types.DiskInfo{}, &types.DiskIndexNotFoundError{
		Index:     index,
		Available: len(disks),
	}
}
