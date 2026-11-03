// Command api runs the PayCore HTTP API.
//
// Exit codes: 0 after a graceful shutdown, 1 on a runtime error, 2 on invalid
// configuration.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/romangergovskiy/paycore/internal/app"
	"github.com/romangergovskiy/paycore/internal/platform/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal restore default handling, so a second signal
	// terminates the process without waiting for the graceful shutdown.
	go func() {
		<-ctx.Done()
		stop()
	}()

	err := app.Run(ctx, os.Environ(), os.Stdout)
	stop()
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "paycore-api:", err)
	if errors.Is(err, config.ErrInvalid) {
		os.Exit(2)
	}
	os.Exit(1)
}
