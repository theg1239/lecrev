package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/theg1239/lecrev/internal/devstack"
	"github.com/theg1239/lecrev/internal/regions"
	"github.com/theg1239/lecrev/internal/smoke"
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
			APIAddr:             strings.TrimSpace(os.Getenv("LECREV_API_ADDR")),
			CoordinatorBasePort: coordinatorBasePortFromEnv(),
			ExecutionRegions:    regions.ParseCSV(os.Getenv("LECREV_EXECUTION_REGIONS")),
		}); err != nil {
			log.Fatal(err)
		}
	case "smoke":
		if err := runSmoke(ctx, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func runSmoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	apiBaseURL := fs.String("api", "", "base URL for a running lecrev API; if empty, run an embedded in-process stack")
	apiKey := fs.String("api-key", "dev-root-key", "API key used for smoke requests")
	projectID := fs.String("project", "demo", "project identifier used for the smoke deploy")
	regionCSV := fs.String("regions", "", "comma-separated APAC execution regions")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := smoke.Config{
		BaseURL:   *apiBaseURL,
		APIKey:    *apiKey,
		ProjectID: *projectID,
		Regions:   regions.ParseCSV(*regionCSV),
	}
	if *apiBaseURL == "" {
		stack, err := devstack.StartEmbedded(ctx, devstack.Config{
			LoadEnv:          false,
			ExecutionRegions: regions.ParseCSV(*regionCSV),
			SecretsBackend:   "memory",
		})
		if err != nil {
			return err
		}
		defer stack.Close()

		cfg.BaseURL = "http://embedded.lecrev"
		cfg.APIKey = stack.APIKey
		cfg.Client = smoke.NewHandlerClient(stack.Handler)
	}

	result, err := smoke.Run(ctx, cfg)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: lecrev <devstack|smoke>")
}

func coordinatorBasePortFromEnv() int {
	raw := strings.TrimSpace(os.Getenv("LECREV_COORDINATOR_BASE_PORT"))
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		log.Fatalf("parse LECREV_COORDINATOR_BASE_PORT: %v", err)
	}
	return value
}
