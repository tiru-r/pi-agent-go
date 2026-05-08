package main

import (
	"os"

	"github.com/tiru-r/pi-agent-go/internal/cli"
)

// Version is the application version. Overridden at build time via:
//
//	go build -ldflags "-X main.Version=1.2.3"
var Version = "0.1.0"

func main() {
	cli.Version = Version
	cmd := cli.NewRootCmd()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
