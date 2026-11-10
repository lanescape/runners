// Command standin is the GitHub emulator's runner stand-in as a program, in
// place of the runner's run.sh: configured from its environment as the runner
// is, it takes one job, runs its job-started hook and steps, reports it and
// exits.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	ghemu "github.com/lanescape/runners/emulators/github"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := ghemu.StandInFromEnv().Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "runner stand-in:", err)
		os.Exit(1)
	}
}
