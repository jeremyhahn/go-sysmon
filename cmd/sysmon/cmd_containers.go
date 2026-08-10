package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var containerIndex int

var containersCmd = &cobra.Command{
	Use:     "containers",
	Aliases: []string{"container"},
	Short:   "Display containers running on the host",
	Long: "Display containers running on the host with per-container CPU, memory, " +
		"PID and I/O accounting read from cgroups. No container runtime socket is used.",
	RunE: runContainersCmd,
}

func init() {
	containersCmd.Flags().IntVar(&containerIndex, "index", -1,
		"container index to display (0-based); omit to show all")
	rootCmd.AddCommand(containersCmd)
}

func runContainersCmd(cmd *cobra.Command, _ []string) error {
	return runWithRefresh(cmd, func(cmd *cobra.Command, snap *types.Snapshot) error {
		if containerIndex >= 0 {
			ct, filterErr := filterContainerByIndex(snap.Virt.Containers, containerIndex)
			if filterErr != nil {
				return filterErr
			}
			snap.Virt.Containers = []types.ContainerInfo{ct}
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				CgroupVersion string                `json:"cgroup_version"`
				Runtimes      []string              `json:"runtimes"`
				Containers    []types.ContainerInfo `json:"containers"`
			}{
				CgroupVersion: snap.Virt.CgroupVersion,
				Runtimes:      snap.Virt.Runtimes,
				Containers:    snap.Virt.Containers,
			})
		}
		if err := cli.RenderContainers(cmd.OutOrStdout(), snap); err != nil {
			return fmt.Errorf("render containers: %w", err)
		}
		return nil
	})
}

// filterContainerByIndex returns the container with the given index.
func filterContainerByIndex(containers []types.ContainerInfo, index int) (types.ContainerInfo, error) {
	for _, c := range containers {
		if c.Index == index {
			return c, nil
		}
	}
	return types.ContainerInfo{}, &types.ContainerIndexNotFoundError{
		Index:     index,
		Available: len(containers),
	}
}
