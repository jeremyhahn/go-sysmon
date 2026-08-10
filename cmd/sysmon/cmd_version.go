package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Long:  "Print the sysmon version, git commit, and build date.",
	RunE:  runVersionCmd,
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

func runVersionCmd(cmd *cobra.Command, _ []string) error {
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{
			"version":   version,
			"gitCommit": gitCommit,
			"buildDate": buildDate,
		})
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "sysmon %s (commit: %s, built: %s)\n",
		version, gitCommit, buildDate)
	return err
}
