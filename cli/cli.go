// Package cli is kosh's command line: serve, id and version.
package cli

import (
	ucli "github.com/urfave/cli/v2"
)

// Version is what kosh version prints. The build sets it.
var Version = "dev"

// App returns the command line.
func App() *ucli.App {
	return &ucli.App{
		Name:    "kosh",
		Usage:   "a terminal UI for the cluster it runs in, served over SSH and iroh",
		Version: Version,
	}
}
