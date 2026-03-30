package devstack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/theg1239/lecrev/internal/apikey"
	"github.com/theg1239/lecrev/internal/build"
	"github.com/theg1239/lecrev/internal/coordinator"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/httpapi"
	"github.com/theg1239/lecrev/internal/leadership"
	"github.com/theg1239/lecrev/internal/nodeagent"
	"github.com/theg1239/lecrev/internal/recovery"
	"github.com/theg1239/lecrev/internal/scheduler"
	"github.com/theg1239/lecrev/internal/secrets"
)

func RunControlPlane(ctx context.Context, cfg Config) error {
	cfg.LoadEnv = true
	if err := prepareConfig(&cfg); err != nil {
		return err
	}
	if stringsEmpty(cfg.CoordinatorBindHost) {
		cfg.CoordinatorBindHost = "0.0.0.0"
	}

	metaStore, cleanup, err := buildMetadataStore(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer cleanup()
	objectStore, closeObjectStore, err := buildArtifactStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeObjectStore()
	artifactReplicator, closeReplicator, err := buildArtifactReplicator(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeReplicator()
	secretProvider, err := buildSecretsProvider(ctx, cfg)
	if err != nil {
		return err
	}
	secretResolver := secrets.NewScopedResolver(metaStore, secretProvider)
	secretsProxy := secrets.NewProxyHandler(secretResolver, cfg.SecretsProxyToken)
	builder := build.New(metaStore, objectStore)
	builder.SetArtifactReplicator(artifactReplicator)
	buildBus, closeBuildBus, err := buildBuildBus(cfg)
	if err != nil {
		return err
	}
	defer closeBuildBus()
	builder.SetBuildBus(buildBus)
	executionBus, closeBus, err := buildExecutionBus(cfg)
	if err != nil {
		return err
	}
	defer closeBus()

	grpcServerOptions, err := buildControlPlaneGRPCServerOptions(cfg)
	if err != nil {
		return err
	}

	if err := metaStore.PutAPIKey(ctx, &domain.APIKey{
		KeyHash:     apikey.Hash(cfg.BootstrapAdminAPIKey),
		TenantID:    "tenant-dev",
		Description: "bootstrap admin key",
		IsAdmin:     true,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		return err
	}

	dispatchers := make([]scheduler.RegionDispatcher, 0, len(cfg.ExecutionRegions))
	type localRegion struct {
		name string
		addr string
		svc  *coordinator.Service
	}
	localRegions := make([]localRegion, 0, len(cfg.ExecutionRegions))
	for i, regionName := range cfg.ExecutionRegions {
		addr := coordinatorListenAddr(cfg, i, "0.0.0.0")
		svc := coordinator.New(regionName, metaStore, nil)
		svc.SetExecutionBus(executionBus)
		localRegions = append(localRegions, localRegion{name: regionName, addr: addr, svc: svc})
		dispatchers = append(dispatchers, svc)
	}
	schedulerService := scheduler.New(metaStore, dispatchers)
	builder.SetWarmPreparer(schedulerService)
	for _, region := range localRegions {
		region.svc.SetRetryer(schedulerService)
	}
	leaseRecovery := recovery.New(metaStore, schedulerService, 3*time.Second)
	admins := make([]httpapi.RegionAdmin, 0, len(localRegions))
	for _, region := range localRegions {
		admins = append(admins, region.svc)
	}

	apiHandler := httpapi.New(metaStore, objectStore, builder, schedulerService, admins...)
	controlHandler := composeControlPlaneHandler(apiHandler, secretsProxy)
	httpServer := &http.Server{Addr: cfg.APIAddr, Handler: controlHandler}

	errCh := make(chan error, len(localRegions)*3+3)
	run := func(name string, fn func() error) {
		go func() {
			if err := fn(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	for _, region := range localRegions {
		region := region
		slog.Info("control-plane coordinator configured", "region", region.name, "listen", region.addr)
		run("coordinator-"+region.name, func() error { return region.svc.Listen(ctx, region.addr, grpcServerOptions...) })
	}

	run("control-plane-background", func() error {
		return leadership.Run(ctx, leadership.Config{
			Name: "control-plane-background",
			DSN:  cfg.PostgresDSN,
		}, func(runCtx context.Context) error {
			leaderErrCh := make(chan error, len(localRegions)+2)
			runLeader := func(name string, fn func() error) {
				go func() {
					if err := fn(); err != nil && !errors.Is(err, context.Canceled) {
						leaderErrCh <- fmt.Errorf("%s: %w", name, err)
					}
				}()
			}
			for _, region := range localRegions {
				region := region
				runLeader("queue-consumer-"+region.name, func() error {
					return region.svc.RunExecutionConsumer(runCtx, "coordinator-"+sanitizeRegionToken(region.name))
				})
			}
			runLeader("lease-recovery", func() error { return leaseRecovery.Run(runCtx) })
			runLeader("global-scheduler", func() error { return schedulerService.Run(runCtx) })
			select {
			case <-runCtx.Done():
				return runCtx.Err()
			case err := <-leaderErrCh:
				return err
			}
		})
	})
	run("http-api", func() error { return httpServer.ListenAndServe() })

	slog.Info("control-plane started", "api", cfg.APIAddr, "regions", cfg.ExecutionRegions)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func RunNodeAgent(ctx context.Context, cfg Config) error {
	cfg.LoadEnv = true
	if err := prepareConfig(&cfg); err != nil {
		return err
	}
	if stringsEmpty(cfg.NodeAgentRegion) {
		return fmt.Errorf("LECREV_NODE_AGENT_REGION is required")
	}
	if stringsEmpty(cfg.NodeAgentCoordinator) {
		return fmt.Errorf("LECREV_COORDINATOR_ADDR is required")
	}
	if stringsEmpty(cfg.ControlPlaneBaseURL) {
		return fmt.Errorf("LECREV_CONTROL_PLANE_BASE_URL is required")
	}
	if stringsEmpty(cfg.S3Bucket) {
		return fmt.Errorf("LECREV_S3_BUCKET is required for a remote execution host")
	}
	if stringsEmpty(cfg.NodeAgentHostID) {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("resolve hostname for node-agent host id: %w", err)
		}
		cfg.NodeAgentHostID = hostname
	}

	objectStore, closeObjectStore, err := buildArtifactStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeObjectStore()

	driver, err := buildExecutionDriver(cfg)
	if err != nil {
		return err
	}
	dialOptions, err := buildNodeAgentGRPCDialOptions(cfg)
	if err != nil {
		return err
	}
	secretsClient := secrets.NewProxyClient(controlPlaneBaseURL(cfg.ControlPlaneBaseURL), cfg.SecretsProxyToken, http.DefaultClient)

	slog.Info("node-agent starting", "hostID", cfg.NodeAgentHostID, "region", cfg.NodeAgentRegion, "coordinator", cfg.NodeAgentCoordinator, "driver", driver.Name())
	return nodeagent.NewWithConfig(nodeagent.Config{
		MaxConcurrentAssignments: cfg.ExecutionHostSlots,
	}, cfg.NodeAgentHostID, cfg.NodeAgentRegion, cfg.NodeAgentCoordinator, driver, objectStore, secretsClient, dialOptions...).Run(ctx)
}

func RunBuildWorker(ctx context.Context, cfg Config) error {
	cfg.LoadEnv = true
	if err := prepareConfig(&cfg); err != nil {
		return err
	}
	if stringsEmpty(cfg.PostgresDSN) {
		return fmt.Errorf("LECREV_POSTGRES_DSN is required for build-worker")
	}
	if stringsEmpty(cfg.NATSURL) {
		return fmt.Errorf("LECREV_NATS_URL is required for build-worker")
	}

	metaStore, cleanup, err := buildMetadataStore(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer cleanup()
	objectStore, closeObjectStore, err := buildArtifactStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeObjectStore()
	artifactReplicator, closeReplicator, err := buildArtifactReplicator(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeReplicator()
	buildBus, closeBuildBus, err := buildBuildBus(cfg)
	if err != nil {
		return err
	}
	defer closeBuildBus()

	builder := build.New(metaStore, objectStore)
	builder.SetArtifactReplicator(artifactReplicator)
	builder.SetBuildBus(buildBus)
	slog.Info("build-worker started", "regions", cfg.ExecutionRegions)
	return leadership.Run(ctx, leadership.Config{
		Name: "build-worker",
		DSN:  cfg.PostgresDSN,
	}, func(runCtx context.Context) error {
		errCh := make(chan error, len(cfg.ExecutionRegions))
		run := func(name string, fn func() error) {
			go func() {
				if err := fn(); err != nil && !errors.Is(err, context.Canceled) {
					errCh <- fmt.Errorf("%s: %w", name, err)
				}
			}()
		}

		for _, regionName := range cfg.ExecutionRegions {
			regionName := regionName
			consumer := "builder-" + sanitizeRegionToken(regionName)
			slog.Info("build-worker consumer configured", "region", regionName, "consumer", consumer)
			run("build-consumer-"+regionName, func() error {
				return builder.RunBuildConsumer(runCtx, regionName, consumer)
			})
		}

		select {
		case <-runCtx.Done():
			return runCtx.Err()
		case err := <-errCh:
			return err
		}
	})
}

func stringsEmpty(value string) bool {
	return len(strings.TrimSpace(value)) == 0
}
