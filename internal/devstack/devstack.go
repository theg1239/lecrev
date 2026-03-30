package devstack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/theg1239/lecrev/internal/apikey"
	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/build"
	"github.com/theg1239/lecrev/internal/coordinator"
	"github.com/theg1239/lecrev/internal/dispatch"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/firecracker"
	"github.com/theg1239/lecrev/internal/firecracker/localnode"
	"github.com/theg1239/lecrev/internal/firecracker/microvm"
	"github.com/theg1239/lecrev/internal/httpapi"
	"github.com/theg1239/lecrev/internal/nodeagent"
	"github.com/theg1239/lecrev/internal/recovery"
	"github.com/theg1239/lecrev/internal/regions"
	"github.com/theg1239/lecrev/internal/scheduler"
	"github.com/theg1239/lecrev/internal/secrets"
	"github.com/theg1239/lecrev/internal/store"
	memstore "github.com/theg1239/lecrev/internal/store/memory"
	pgstore "github.com/theg1239/lecrev/internal/store/postgres"
	"github.com/theg1239/lecrev/internal/transport"

	"google.golang.org/grpc"
)

type Config struct {
	LoadEnv             bool
	APIAddr             string
	ExecutionRegions    []string
	CoordinatorBasePort int
	PostgresDSN         string
	NATSURL             string
	S3Region            string
	S3Endpoint          string
	S3AccessKey         string
	S3SecretKey         string
	S3Bucket            string
	SecretsBackend      string
	AWSRegion           string
	AWSAccessKey        string
	AWSSecretKey        string
	SecretsProxyToken   string
	EnableMTLS          bool
	ExecutionDriver     string

	FirecrackerBinary      string
	JailerBinary           string
	FirecrackerUseJailer   bool
	FirecrackerKernelPath  string
	FirecrackerRootFSPath  string
	FirecrackerWorkspace   string
	FirecrackerSnapshotDir string
	FirecrackerChrootBase  string
	FirecrackerGuestInit   string
	FirecrackerTapDevice   string
	FirecrackerGuestMAC    string
	FirecrackerGuestIP     string
	FirecrackerGatewayIP   string
	FirecrackerNetmask     string
	FirecrackerVCPUCount   int
	FirecrackerMemoryMB    int
}

func Run(ctx context.Context, cfg Config) error {
	cfg.LoadEnv = true
	if err := prepareConfig(&cfg); err != nil {
		return err
	}
	return runNetworked(ctx, cfg)
}

func prepareConfig(cfg *Config) error {
	if cfg.APIAddr == "" {
		cfg.APIAddr = ":8080"
	}
	if cfg.CoordinatorBasePort == 0 {
		cfg.CoordinatorBasePort = 9091
	}
	if cfg.LoadEnv && cfg.PostgresDSN == "" {
		cfg.PostgresDSN = strings.TrimSpace(os.Getenv("LECREV_POSTGRES_DSN"))
	}
	if cfg.LoadEnv && cfg.NATSURL == "" {
		cfg.NATSURL = strings.TrimSpace(os.Getenv("LECREV_NATS_URL"))
	}
	if cfg.LoadEnv && cfg.S3Region == "" {
		cfg.S3Region = strings.TrimSpace(os.Getenv("LECREV_S3_REGION"))
	}
	if cfg.LoadEnv && cfg.S3Endpoint == "" {
		cfg.S3Endpoint = strings.TrimSpace(os.Getenv("LECREV_S3_ENDPOINT"))
	}
	if cfg.LoadEnv && cfg.S3AccessKey == "" {
		cfg.S3AccessKey = strings.TrimSpace(os.Getenv("LECREV_S3_ACCESS_KEY"))
	}
	if cfg.LoadEnv && cfg.S3SecretKey == "" {
		cfg.S3SecretKey = strings.TrimSpace(os.Getenv("LECREV_S3_SECRET_KEY"))
	}
	if cfg.LoadEnv && cfg.S3Bucket == "" {
		cfg.S3Bucket = strings.TrimSpace(os.Getenv("LECREV_S3_BUCKET"))
	}
	if cfg.LoadEnv && cfg.SecretsBackend == "" {
		cfg.SecretsBackend = strings.TrimSpace(os.Getenv("LECREV_SECRETS_BACKEND"))
	}
	if cfg.LoadEnv && cfg.AWSRegion == "" {
		cfg.AWSRegion = strings.TrimSpace(os.Getenv("LECREV_AWS_REGION"))
	}
	if cfg.LoadEnv && cfg.AWSAccessKey == "" {
		cfg.AWSAccessKey = strings.TrimSpace(os.Getenv("LECREV_AWS_ACCESS_KEY_ID"))
	}
	if cfg.LoadEnv && cfg.AWSSecretKey == "" {
		cfg.AWSSecretKey = strings.TrimSpace(os.Getenv("LECREV_AWS_SECRET_ACCESS_KEY"))
	}
	if cfg.LoadEnv && cfg.SecretsProxyToken == "" {
		cfg.SecretsProxyToken = strings.TrimSpace(os.Getenv("LECREV_SECRETS_PROXY_TOKEN"))
	}
	if cfg.LoadEnv && cfg.ExecutionDriver == "" {
		cfg.ExecutionDriver = strings.TrimSpace(os.Getenv("LECREV_EXECUTION_DRIVER"))
	}
	if cfg.LoadEnv && cfg.FirecrackerBinary == "" {
		cfg.FirecrackerBinary = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_BINARY"))
	}
	if cfg.LoadEnv && cfg.JailerBinary == "" {
		cfg.JailerBinary = strings.TrimSpace(os.Getenv("LECREV_JAILER_BINARY"))
	}
	if cfg.LoadEnv {
		if raw := strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_USE_JAILER")); raw != "" {
			enabled, err := strconv.ParseBool(raw)
			if err != nil {
				return fmt.Errorf("parse LECREV_FIRECRACKER_USE_JAILER: %w", err)
			}
			cfg.FirecrackerUseJailer = enabled
		}
	}
	if cfg.LoadEnv && cfg.FirecrackerKernelPath == "" {
		cfg.FirecrackerKernelPath = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_KERNEL_IMAGE"))
	}
	if cfg.LoadEnv && cfg.FirecrackerRootFSPath == "" {
		cfg.FirecrackerRootFSPath = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_ROOTFS"))
	}
	if cfg.LoadEnv && cfg.FirecrackerWorkspace == "" {
		cfg.FirecrackerWorkspace = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_WORKSPACE_DIR"))
	}
	if cfg.LoadEnv && cfg.FirecrackerSnapshotDir == "" {
		cfg.FirecrackerSnapshotDir = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_SNAPSHOT_DIR"))
	}
	if cfg.LoadEnv && cfg.FirecrackerChrootBase == "" {
		cfg.FirecrackerChrootBase = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_CHROOT_BASE_DIR"))
	}
	if cfg.LoadEnv && cfg.FirecrackerGuestInit == "" {
		cfg.FirecrackerGuestInit = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_GUEST_INIT"))
	}
	if cfg.LoadEnv && cfg.FirecrackerTapDevice == "" {
		cfg.FirecrackerTapDevice = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_TAP_DEVICE"))
	}
	if cfg.LoadEnv && cfg.FirecrackerGuestMAC == "" {
		cfg.FirecrackerGuestMAC = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_GUEST_MAC"))
	}
	if cfg.LoadEnv && cfg.FirecrackerGuestIP == "" {
		cfg.FirecrackerGuestIP = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_GUEST_IP"))
	}
	if cfg.LoadEnv && cfg.FirecrackerGatewayIP == "" {
		cfg.FirecrackerGatewayIP = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_GATEWAY_IP"))
	}
	if cfg.LoadEnv && cfg.FirecrackerNetmask == "" {
		cfg.FirecrackerNetmask = strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_NETMASK"))
	}
	if cfg.LoadEnv && cfg.FirecrackerVCPUCount == 0 {
		if raw := strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_VCPU_COUNT")); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return fmt.Errorf("parse LECREV_FIRECRACKER_VCPU_COUNT: %w", err)
			}
			cfg.FirecrackerVCPUCount = value
		}
	}
	if cfg.LoadEnv && cfg.FirecrackerMemoryMB == 0 {
		if raw := strings.TrimSpace(os.Getenv("LECREV_FIRECRACKER_DEFAULT_MEMORY_MB")); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return fmt.Errorf("parse LECREV_FIRECRACKER_DEFAULT_MEMORY_MB: %w", err)
			}
			cfg.FirecrackerMemoryMB = value
		}
	}
	if strings.TrimSpace(cfg.SecretsProxyToken) == "" {
		cfg.SecretsProxyToken = "dev-secrets-token"
	}
	if strings.TrimSpace(cfg.ExecutionDriver) == "" {
		cfg.ExecutionDriver = "local-node"
	}
	if cfg.LoadEnv {
		if raw := strings.TrimSpace(os.Getenv("LECREV_ENABLE_MTLS")); raw != "" {
			enabled, err := strconv.ParseBool(raw)
			if err != nil {
				return fmt.Errorf("parse LECREV_ENABLE_MTLS: %w", err)
			}
			cfg.EnableMTLS = enabled
		} else if !cfg.EnableMTLS {
			cfg.EnableMTLS = true
		}
	}
	normalizedRegions, err := regions.NormalizeExecutionRegions(cfg.ExecutionRegions)
	if err != nil {
		return err
	}
	cfg.ExecutionRegions = normalizedRegions
	return nil
}

func runNetworked(ctx context.Context, cfg Config) error {
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
	secretProvider, err := buildSecretsProvider(ctx, cfg)
	if err != nil {
		return err
	}
	secretResolver := secrets.NewScopedResolver(metaStore, secretProvider)
	secretsProxy := secrets.NewProxyHandler(secretResolver, cfg.SecretsProxyToken)
	builder := build.New(metaStore, objectStore)
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
	var grpcServerOptions []grpc.ServerOption
	var grpcDialOptions []grpc.DialOption
	if cfg.EnableMTLS {
		bundle, err := transport.GenerateDevMTLS([]string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
		if err != nil {
			return err
		}
		grpcServerOptions = append(grpcServerOptions, grpc.Creds(bundle.Server))
		grpcDialOptions = append(grpcDialOptions, grpc.WithTransportCredentials(bundle.Client))
	}
	if err := metaStore.PutAPIKey(ctx, &domain.APIKey{
		KeyHash:     apikey.Hash("dev-root-key"),
		TenantID:    "tenant-dev",
		Description: "local development root key",
		IsAdmin:     true,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		return err
	}

	dispatchers := make([]scheduler.RegionDispatcher, 0, len(cfg.ExecutionRegions))
	type localRegion struct {
		name string
		addr string
		host string
		svc  *coordinator.Service
	}
	localRegions := make([]localRegion, 0, len(cfg.ExecutionRegions))
	for i, regionName := range cfg.ExecutionRegions {
		addr := fmt.Sprintf("127.0.0.1:%d", cfg.CoordinatorBasePort+i)
		hostID := fmt.Sprintf("host-%s-a", sanitizeRegionToken(regionName))
		svc := coordinator.New(regionName, metaStore, nil)
		svc.SetExecutionBus(executionBus)
		localRegions = append(localRegions, localRegion{
			name: regionName,
			addr: addr,
			host: hostID,
			svc:  svc,
		})
		dispatchers = append(dispatchers, svc)
	}
	schedulerService := scheduler.New(metaStore, dispatchers)
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
	secretsClient := secrets.NewProxyClient(controlPlaneBaseURL(cfg.APIAddr), cfg.SecretsProxyToken, http.DefaultClient)

	errCh := make(chan error, len(localRegions)*2+2)
	run := func(name string, fn func() error) {
		go func() {
			if err := fn(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	for _, region := range localRegions {
		region := region
		run("build-consumer-"+region.name, func() error {
			return builder.RunBuildConsumer(ctx, region.name, "builder-"+sanitizeRegionToken(region.name))
		})
		run("coordinator-"+region.name, func() error { return region.svc.Listen(ctx, region.addr, grpcServerOptions...) })
		run("queue-consumer-"+region.name, func() error {
			return region.svc.RunExecutionConsumer(ctx, "coordinator-"+sanitizeRegionToken(region.name))
		})
	}

	time.Sleep(300 * time.Millisecond)

	for _, region := range localRegions {
		region := region
		run("node-agent-"+region.name, func() error {
			driver, err := buildExecutionDriver(cfg)
			if err != nil {
				return err
			}
			return nodeagent.New(region.host, region.name, region.addr, driver, objectStore, metaStore, secretsClient, grpcDialOptions...).Run(ctx)
		})
	}
	run("lease-recovery", func() error { return leaseRecovery.Run(ctx) })
	run("global-scheduler", func() error { return schedulerService.Run(ctx) })
	run("http-api", func() error { return httpServer.ListenAndServe() })

	slog.Info("devstack started", "api", cfg.APIAddr, "regions", cfg.ExecutionRegions)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func sanitizeRegionToken(input string) string {
	replacer := strings.NewReplacer("/", "-", ".", "-", ":", "-", "_", "-")
	return replacer.Replace(input)
}

func buildMetadataStore(ctx context.Context, dsn string) (store.Store, func(), error) {
	if strings.TrimSpace(dsn) == "" {
		return memstore.New(), func() {}, nil
	}
	pg, err := pgstore.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	if err := pg.RunMigrations(ctx); err != nil {
		pg.Close()
		return nil, nil, err
	}
	return pg, func() { pg.Close() }, nil
}

func buildExecutionBus(cfg Config) (dispatch.ExecutionBus, func(), error) {
	if strings.TrimSpace(cfg.NATSURL) == "" {
		bus := dispatch.NewMemoryBus(256)
		return bus, func() { _ = bus.Close() }, nil
	}
	bus, err := dispatch.NewNATS(cfg.NATSURL)
	if err != nil {
		return nil, nil, err
	}
	if err := bus.EnsureStream(); err != nil {
		_ = bus.Close()
		return nil, nil, err
	}
	return bus, func() { _ = bus.Close() }, nil
}

func buildBuildBus(cfg Config) (build.BuildBus, func(), error) {
	if strings.TrimSpace(cfg.NATSURL) == "" {
		bus := build.NewMemoryBus(256)
		return bus, func() { _ = bus.Close() }, nil
	}
	bus, err := build.NewNATSBus(cfg.NATSURL)
	if err != nil {
		return nil, nil, err
	}
	if err := bus.EnsureStream(); err != nil {
		_ = bus.Close()
		return nil, nil, err
	}
	return bus, func() { _ = bus.Close() }, nil
}

func buildArtifactStore(ctx context.Context, cfg Config) (artifact.Store, func(), error) {
	if strings.TrimSpace(cfg.S3Bucket) == "" {
		return artifact.NewMemoryStore(), func() {}, nil
	}
	store, err := artifact.NewS3Store(ctx, artifact.S3Config{
		Region:    cfg.S3Region,
		Endpoint:  cfg.S3Endpoint,
		AccessKey: cfg.S3AccessKey,
		SecretKey: cfg.S3SecretKey,
		Bucket:    cfg.S3Bucket,
		PathStyle: strings.TrimSpace(cfg.S3Endpoint) != "",
	})
	if err != nil {
		return nil, nil, err
	}
	if err := store.EnsureBucket(ctx); err != nil {
		return nil, nil, err
	}
	return store, func() {}, nil
}

func buildSecretsProvider(ctx context.Context, cfg Config) (secrets.Provider, error) {
	switch cfg.SecretsBackend {
	case "", "memory":
		return secrets.NewMemoryProvider(map[string]string{
			"DEMO_SECRET": "shh-local-dev",
		}), nil
	case "aws-secrets-manager":
		return secrets.NewAWSSecretsManagerProvider(ctx, secrets.AWSSecretsManagerConfig{
			Region:    cfg.AWSRegion,
			AccessKey: cfg.AWSAccessKey,
			SecretKey: cfg.AWSSecretKey,
			TTL:       time.Minute,
		})
	default:
		return nil, fmt.Errorf("unsupported secrets backend %q", cfg.SecretsBackend)
	}
}

func composeControlPlaneHandler(apiHandler http.Handler, secretsProxy http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(secrets.ProxyPath, secretsProxy)
	mux.Handle("/", apiHandler)
	return mux
}

func controlPlaneBaseURL(apiAddr string) string {
	if strings.HasPrefix(apiAddr, "http://") || strings.HasPrefix(apiAddr, "https://") {
		return strings.TrimRight(apiAddr, "/")
	}
	if strings.HasPrefix(apiAddr, ":") {
		return "http://127.0.0.1" + apiAddr
	}
	host, port, err := net.SplitHostPort(apiAddr)
	if err == nil {
		switch host {
		case "", "0.0.0.0", "::":
			host = "127.0.0.1"
		}
		return "http://" + net.JoinHostPort(host, port)
	}
	return "http://" + strings.TrimRight(apiAddr, "/")
}

func buildExecutionDriver(cfg Config) (firecracker.Driver, error) {
	switch strings.TrimSpace(strings.ToLower(cfg.ExecutionDriver)) {
	case "", "local-node":
		return localnode.New(), nil
	case "firecracker":
		return microvm.New(microvm.Config{
			FirecrackerBinary: cfg.FirecrackerBinary,
			JailerBinary:      cfg.JailerBinary,
			KernelImagePath:   cfg.FirecrackerKernelPath,
			RootFSPath:        cfg.FirecrackerRootFSPath,
			WorkspaceDir:      cfg.FirecrackerWorkspace,
			SnapshotDir:       cfg.FirecrackerSnapshotDir,
			ChrootBaseDir:     cfg.FirecrackerChrootBase,
			UseJailer:         cfg.FirecrackerUseJailer,
			GuestInitPath:     cfg.FirecrackerGuestInit,
			TapDevice:         cfg.FirecrackerTapDevice,
			GuestMAC:          cfg.FirecrackerGuestMAC,
			GuestIP:           cfg.FirecrackerGuestIP,
			GatewayIP:         cfg.FirecrackerGatewayIP,
			Netmask:           cfg.FirecrackerNetmask,
			VCPUCount:         int64(cfg.FirecrackerVCPUCount),
			DefaultMemoryMB:   int64(cfg.FirecrackerMemoryMB),
		})
	default:
		return nil, fmt.Errorf("unsupported execution driver %q", cfg.ExecutionDriver)
	}
}
