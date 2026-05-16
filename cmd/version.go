package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Показать версию proxylm",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("proxylm %s (%s/%s, %s)\n",
			versionString, runtime.GOOS, runtime.GOARCH, runtime.Version())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
