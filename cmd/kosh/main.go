// Command kosh serves a terminal UI for the Kubernetes cluster it runs
// in, over SSH and over iroh.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fables-for-robots/kosh/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.App().RunContext(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "kosh:", err)
		os.Exit(1)
	}
}
