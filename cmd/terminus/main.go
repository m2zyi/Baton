package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"terminus/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	termination := make(chan os.Signal, 1)
	signal.Notify(termination, syscall.SIGTERM)
	defer signal.Stop(termination)
	done := make(chan int, 1)
	go func() { done <- cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr) }()
	select {
	case code := <-done:
		return code
	case <-termination:
		// Cancel the foreground exec before exiting the parent. Menu input may
		// be blocked on a terminal read, so shutdown has a bounded grace period.
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		return 128 + int(syscall.SIGTERM)
	}
}
