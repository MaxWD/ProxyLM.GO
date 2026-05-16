package main

import (
	_ "embed"
	"fmt"
	"os"

	"proxylm/cmd"
)

// version перезаписывается при сборке через -ldflags "-X main.version=..."
var version = "dev"

//go:embed config.example.yaml
var defaultConfigTemplate []byte

func main() {
	cmd.SetVersion(version)
	cmd.SetDefaultConfigTemplate(defaultConfigTemplate)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
