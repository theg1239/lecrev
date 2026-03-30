package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ishaan/eeeverc/internal/devstack"
	"github.com/ishaan/eeeverc/internal/regions"
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
			ExecutionRegions: regions.ParseCSV(os.Getenv("EEEVERC_EXECUTION_REGIONS")),
		}); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: eeeverc devstack")
}
