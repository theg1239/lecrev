package devstack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	regionv1 "github.com/theg1239/lecrev/lecrev/region/v1"

	"github.com/theg1239/lecrev/internal/apikey"
	"github.com/theg1239/lecrev/internal/build"
	"github.com/theg1239/lecrev/internal/coordinator"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/firecracker/localnode"
	"github.com/theg1239/lecrev/internal/httpapi"
	"github.com/theg1239/lecrev/internal/nodeagent"
	"github.com/theg1239/lecrev/internal/recovery"
	"github.com/theg1239/lecrev/internal/scheduler"
	"github.com/theg1239/lecrev/internal/secrets"
	"github.com/theg1239/lecrev/internal/transport"
)

type EmbeddedStack struct {
	Handler http.Handler
	Regions []string
	APIKey  string

	cancel context.CancelFunc
	close  func()
	once   sync.Once
	errCh  chan error
}

func (s *EmbeddedStack) Close() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.close != nil {
			s.close()
		}
	})
}

func (s *EmbeddedStack) Errors() <-chan error {
	return s.errCh
}

func StartEmbedded(parent context.Context, cfg Config) (*EmbeddedStack, error) {
	if err := prepareConfig(&cfg); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	metaStore, cleanupStore, err := buildMetadataStore(ctx, cfg.PostgresDSN)
	if err != nil {
		cancel()
		return nil, err
	}
	objectStore, cleanupObjects, err := buildArtifactStore(ctx, cfg)
	if err != nil {
		cancel()
		cleanupStore()
		return nil, err
	}
	secretProvider, err := buildSecretsProvider(ctx, cfg)
	if err != nil {
		cancel()
		cleanupObjects()
		cleanupStore()
		return nil, err
	}
	secretResolver := secrets.NewScopedResolver(metaStore, secretProvider)
	secretsProxy := secrets.NewProxyHandler(secretResolver, cfg.SecretsProxyToken)
	builder := build.New(metaStore, objectStore)
	buildBus, closeBuildBus, err := buildBuildBus(cfg)
	if err != nil {
		cancel()
		cleanupObjects()
		cleanupStore()
		return nil, err
	}
	builder.SetBuildBus(buildBus)
	executionBus, cleanupBus, err := buildExecutionBus(cfg)
	if err != nil {
		cancel()
		_ = buildBus.Close()
		cleanupObjects()
		cleanupStore()
		return nil, err
	}
	cleanup := func() {
		closeBuildBus()
		cleanupBus()
		cleanupObjects()
		cleanupStore()
	}

	if err := metaStore.PutAPIKey(ctx, &domain.APIKey{
		KeyHash:     apikey.Hash("dev-root-key"),
		TenantID:    "tenant-dev",
		Description: "local development root key",
		IsAdmin:     true,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		cancel()
		cleanup()
		return nil, err
	}

	type localRegion struct {
		name string
		host string
		svc  *coordinator.Service
	}
	dispatchers := make([]scheduler.RegionDispatcher, 0, len(cfg.ExecutionRegions))
	localRegions := make([]localRegion, 0, len(cfg.ExecutionRegions))
	for _, regionName := range cfg.ExecutionRegions {
		hostID := fmt.Sprintf("host-%s-a", sanitizeRegionToken(regionName))
		svc := coordinator.New(regionName, metaStore, nil)
		svc.SetExecutionBus(executionBus)
		localRegions = append(localRegions, localRegion{
			name: regionName,
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

	apiHandler := httpapi.New(metaStore, builder, schedulerService, admins...)
	controlHandler := composeControlPlaneHandler(apiHandler, secretsProxy)
	stack := &EmbeddedStack{
		Handler: controlHandler,
		Regions: append([]string(nil), cfg.ExecutionRegions...),
		APIKey:  "dev-root-key",
		cancel:  cancel,
		close:   cleanup,
		errCh:   make(chan error, len(localRegions)*2+1),
	}
	run := func(name string, fn func() error) {
		go func() {
			if err := fn(); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case stack.errCh <- fmt.Errorf("%s: %w", name, err):
				default:
				}
				cancel()
			}
		}()
	}

	for _, region := range localRegions {
		region := region
		run("build-consumer-"+region.name, func() error {
			return builder.RunBuildConsumer(ctx, region.name, "embedded-builder-"+sanitizeRegionToken(region.name))
		})
		run("queue-consumer-"+region.name, func() error {
			return region.svc.RunExecutionConsumer(ctx, "embedded-"+sanitizeRegionToken(region.name))
		})

		agentSecretsClient := secrets.NewProxyClient("http://embedded.lecrev", cfg.SecretsProxyToken, transport.NewHandlerClient(controlHandler))
		agent := nodeagent.New(region.host, region.name, "", localnode.New(), objectStore, metaStore, agentSecretsClient)
		if err := region.svc.RegisterEmbeddedHost(ctx, agent.RegistrationMessage(), func(execCtx context.Context, assignment *regionv1.ExecutionAssignment) {
			go agent.ExecuteEmbeddedAssignment(execCtx, region.svc, assignment)
		}); err != nil {
			stack.Close()
			return nil, err
		}
		run("embedded-heartbeat-"+region.name, func() error {
			return agent.RunEmbeddedHeartbeatLoop(ctx, region.svc)
		})
	}
	run("lease-recovery", func() error { return leaseRecovery.Run(ctx) })
	return stack, nil
}
