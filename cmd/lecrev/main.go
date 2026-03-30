package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/theg1239/lecrev/internal/devstack"
	"github.com/theg1239/lecrev/internal/regions"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "devstack":
		if err := devstack.Run(ctx, devstack.Config{
			APIAddr:          ":8080",
			ExecutionRegions: regions.ParseCSV(os.Getenv("LECREV_EXECUTION_REGIONS")),
		}); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: lecrev devstack")
}
