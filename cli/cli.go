// Package cli is kosh's command line: serve, id and version.
package cli

import (
	"fmt"

	ucli "github.com/urfave/cli/v2"
)

// Version is what kosh version prints. The build sets it.
var Version = "dev"

// App returns the command line: kosh serve, kosh id and kosh version.
// Its Run returns the error of a command and does not exit: main prints
// it and exits.
func App() *ucli.App {
	return &ucli.App{
		Name:    "kosh",
		Usage:   "a terminal UI for the cluster it runs in, served over SSH and iroh",
		Version: Version,
		// An error is returned to main, which prints it, rather than
		// printed here and the process ended.
		ExitErrHandler: func(*ucli.Context, error) {},
		Commands: []*ucli.Command{
			idCommand(),
			{
				Name:  "version",
				Usage: "print the version",
				Action: func(c *ucli.Context) error {
					_, err := fmt.Fprintln(c.App.Writer, Version)
					return err
				},
			},
		},
	}
}
