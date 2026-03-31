package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	regionv1 "github.com/theg1239/lecrev/lecrev/region/v1"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/firecracker"
	"github.com/theg1239/lecrev/internal/secrets"
	"github.com/theg1239/lecrev/internal/timetrace"
	"github.com/theg1239/lecrev/internal/transport"
)

type Service struct {
	hostID          string
	region          string
	driverName      string
	coordinatorAddr string
	driver          firecracker.Driver
	objects         artifact.Store
	secrets         secrets.ExecutionResolver
	dialOptions     []grpc.DialOption

	mu             sync.Mutex
	maxSlots       int
	maxFullNetwork int
	sendMu         sync.Mutex
	availableSlots int
	availableFull  int
	blankWarm      int
	functionWarm   map[string]int
	bundleMu       sync.RWMutex
	bundleCache    map[string][]byte
}

const (
	maxExecutionLogBytes    = 8 << 20
	maxExecutionOutputBytes = 32 << 20
)

type EmbeddedCoordinator interface {
	UpdateEmbeddedHeartbeat(ctx context.Context, msg *regionv1.HostHeartbeat) error
	ApplyAssignmentUpdate(ctx context.Context, msg *regionv1.AssignmentUpdate) error
}

type Config struct {
	MaxConcurrentAssignments     int
	MaxConcurrentFullNetworkJobs int
}

func (c Config) withDefaults() Config {
	if c.MaxConcurrentAssignments <= 0 {
		c.MaxConcurrentAssignments = 4
	}
	hostCPUs := runtime.NumCPU()
	if hostCPUs <= 0 {
		hostCPUs = 1
	}
	if c.MaxConcurrentFullNetworkJobs <= 0 {
		c.MaxConcurrentFullNetworkJobs = hostCPUs
	}
	if c.MaxConcurrentFullNetworkJobs > c.MaxConcurrentAssignments {
		c.MaxConcurrentFullNetworkJobs = c.MaxConcurrentAssignments
	}
	return c
}

func New(hostID, region, coordinatorAddr string, driver firecracker.Driver, objects artifact.Store, secrets secrets.ExecutionResolver, dialOptions ...grpc.DialOption) *Service {
	return NewWithConfig(Config{}, hostID, region, coordinatorAddr, driver, objects, secrets, dialOptions...)
}

func NewWithConfig(cfg Config, hostID, region, coordinatorAddr string, driver firecracker.Driver, objects artifact.Store, secrets secrets.ExecutionResolver, dialOptions ...grpc.DialOption) *Service {
	cfg = cfg.withDefaults()
	baseDialOptions := []grpc.DialOption{
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(transport.GRPCMaxMessageBytes),
			grpc.MaxCallSendMsgSize(transport.GRPCMaxMessageBytes),
		),
	}
	if len(dialOptions) == 0 {
		baseDialOptions = append(baseDialOptions, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		baseDialOptions = append(baseDialOptions, dialOptions...)
	}
	driverName := "unknown"
	if driver != nil && strings.TrimSpace(driver.Name()) != "" {
		driverName = strings.TrimSpace(driver.Name())
	}
	return &Service{
		hostID:          hostID,
		region:          region,
		driverName:      driverName,
		coordinatorAddr: coordinatorAddr,
		driver:          driver,
		objects:         objects,
		secrets:         secrets,
		dialOptions:     append([]grpc.DialOption(nil), baseDialOptions...),
		maxSlots:        cfg.MaxConcurrentAssignments,
		maxFullNetwork:  cfg.MaxConcurrentFullNetworkJobs,
		availableSlots:  cfg.MaxConcurrentAssignments,
		availableFull:   cfg.MaxConcurrentFullNetworkJobs,
		blankWarm:       1,
		functionWarm:    map[string]int{},
		bundleCache:     map[string][]byte{},
	}
}

func (s *Service) Run(ctx context.Context) error {
	if err := s.prepareDriver(ctx); err != nil {
		return err
	}

	conn, err := grpc.DialContext(ctx, s.coordinatorAddr, s.dialOptions...)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := regionv1.NewCoordinatorClient(conn)
	stream, err := client.Control(ctx)
	if err != nil {
		return err
	}

	if err := stream.Send(&regionv1.AgentMessage{
		Body: &regionv1.AgentMessage_Register{
			Register: s.registrationMessage(),
		},
	}); err != nil {
		return err
	}

	go s.heartbeatLoop(ctx, stream)

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		switch body := msg.Body.(type) {
		case *regionv1.CoordinatorMessage_Registered:
			continue
		case *regionv1.CoordinatorMessage_Assignment:
			go s.executeAssignment(ctx, body.Assignment, func(update *regionv1.AssignmentUpdate) {
				_ = s.send(stream, &regionv1.AgentMessage{
					Body: &regionv1.AgentMessage_AssignmentUpdate{
						AssignmentUpdate: update,
					},
				})
			}, func() {
				_ = s.send(stream, &regionv1.AgentMessage{
					Body: &regionv1.AgentMessage_Heartbeat{
						Heartbeat: s.heartbeatMessage(),
					},
				})
			})
		case *regionv1.CoordinatorMessage_Prepare:
			go s.prepareSnapshot(ctx, body.Prepare, func() {
				_ = s.send(stream, &regionv1.AgentMessage{
					Body: &regionv1.AgentMessage_Heartbeat{
						Heartbeat: s.heartbeatMessage(),
					},
				})
			})
		case *regionv1.CoordinatorMessage_Drain:
			s.applyDrain()
			_ = s.send(stream, &regionv1.AgentMessage{
				Body: &regionv1.AgentMessage_Heartbeat{
					Heartbeat: s.heartbeatMessage(),
				},
			})
		case *regionv1.CoordinatorMessage_Terminate:
			continue
		}
	}
}

func (s *Service) heartbeatLoop(ctx context.Context, stream grpc.BidiStreamingClient[regionv1.AgentMessage, regionv1.CoordinatorMessage]) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.send(stream, &regionv1.AgentMessage{
				Body: &regionv1.AgentMessage_Heartbeat{
					Heartbeat: s.heartbeatMessage(),
				},
			})
		}
	}
}

func (s *Service) RegistrationMessage() *regionv1.RegisterHost {
	return s.registrationMessage()
}

func (s *Service) RunEmbeddedHeartbeatLoop(ctx context.Context, coordinator EmbeddedCoordinator) error {
	if err := s.prepareDriver(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := coordinator.UpdateEmbeddedHeartbeat(ctx, s.heartbeatMessage()); err != nil {
				return err
			}
		}
	}
}

func (s *Service) ExecuteEmbeddedAssignment(ctx context.Context, coordinator EmbeddedCoordinator, msg *regionv1.ExecutionAssignment) {
	s.executeAssignment(ctx, msg, func(update *regionv1.AssignmentUpdate) {
		_ = coordinator.ApplyAssignmentUpdate(ctx, update)
	}, func() {
		_ = coordinator.UpdateEmbeddedHeartbeat(ctx, s.heartbeatMessage())
	})
}

func (s *Service) PrepareEmbeddedSnapshot(ctx context.Context, coordinator EmbeddedCoordinator, msg *regionv1.PrepareSnapshot) {
	s.prepareSnapshot(ctx, msg, func() {
		_ = coordinator.UpdateEmbeddedHeartbeat(ctx, s.heartbeatMessage())
	})
}

func (s *Service) executeAssignment(ctx context.Context, msg *regionv1.ExecutionAssignment, sendUpdate func(*regionv1.AssignmentUpdate), sendHeartbeat func()) {
	overallStarted := time.Now()
	trace := timetrace.New()
	var (
		bundleLoadMs    int64
		secretsLoadMs   int64
		driverExecuteMs int64
		warmPrepareMs   int64
		terminalState   = "unknown"
		terminalErr     string
	)
	defer func() {
		slog.Info("node-agent assignment timing",
			"hostID", s.hostID,
			"region", s.region,
			"jobID", msg.JobId,
			"attemptID", msg.AttemptId,
			"functionVersionID", msg.FunctionVersionId,
			"state", terminalState,
			"error", terminalErr,
			"bundleLoadMs", bundleLoadMs,
			"secretsLoadMs", secretsLoadMs,
			"driverExecuteMs", driverExecuteMs,
			"warmPrepareMs", warmPrepareMs,
			"totalMs", time.Since(overallStarted).Milliseconds(),
		)
	}()
	networkPolicy := msg.NetworkPolicy
	s.reserveAssignmentSlots(networkPolicy)
	released := false
	holdDeferredRelease := false
	releaseSlot := func() {
		if released {
			return
		}
		released = true
		s.releaseAssignmentSlots(networkPolicy)
	}
	defer func() {
		if holdDeferredRelease {
			return
		}
		releaseSlot()
	}()

	emitUpdate := func(state regionv1.AssignmentState, logs string, output []byte, errMsg string, exitCode int) {
		sendUpdate(&regionv1.AssignmentUpdate{
			HostId:       s.hostID,
			Region:       s.region,
			AttemptId:    msg.AttemptId,
			JobId:        msg.JobId,
			State:        state,
			Logs:         logs,
			OutputJson:   output,
			ErrorMessage: errMsg,
			ExitCode:     int32(exitCode),
		})
	}

	emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_STARTING, "", nil, "", 0)

	bundleKey := strings.TrimSpace(msg.ArtifactBundleKey)
	if bundleKey == "" {
		terminalState = "failed"
		terminalErr = "assignment missing artifact_bundle_key"
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, trace.String(), nil, "assignment missing artifact_bundle_key", 1)
		return
	}
	loadBundleStarted := time.Now()
	bundle, err := s.loadBundle(ctx, bundleKey)
	bundleLoadMs = time.Since(loadBundleStarted).Milliseconds()
	trace.Add("load_bundle", time.Duration(bundleLoadMs)*time.Millisecond)
	if err != nil {
		terminalState = "failed"
		terminalErr = err.Error()
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, trace.String(), nil, err.Error(), 1)
		return
	}
	resolveSecretsStarted := time.Now()
	env, err := s.secrets.ResolveExecution(ctx, secrets.ExecutionRequest{
		HostID:            s.hostID,
		Region:            s.region,
		FunctionVersionID: msg.FunctionVersionId,
		SecretRefs:        msg.EnvRefs,
	})
	secretsLoadMs = time.Since(resolveSecretsStarted).Milliseconds()
	trace.Add("resolve_secrets", time.Duration(secretsLoadMs)*time.Millisecond)
	if err != nil {
		terminalState = "failed"
		terminalErr = err.Error()
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, trace.String(), nil, err.Error(), 1)
		return
	}

	emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, "", nil, "", 0)
	runCtx, stopRunningHeartbeats := context.WithCancel(ctx)
	defer stopRunningHeartbeats()
	go s.assignmentHeartbeatLoop(runCtx, func() {
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, "", nil, "", 0)
	})

	executeReq := firecracker.ExecuteRequest{
		AttemptID:      msg.AttemptId,
		JobID:          msg.JobId,
		FunctionID:     msg.FunctionVersionId,
		Entrypoint:     msg.Entrypoint,
		ArtifactBundle: bundle,
		Payload:        msg.PayloadJson,
		Env:            env,
		Timeout:        time.Duration(msg.TimeoutSec) * time.Second,
		MemoryMB:       int(msg.MemoryMb),
		NetworkPolicy:  msg.NetworkPolicy,
		Region:         s.region,
		HostID:         s.hostID,
	}
	if isDirectStreamAttempt(msg.AttemptId) {
		executeReq.EnableStreaming = true
		executeReq.StreamKind = firecracker.StreamKindHTTP
		executeReq.HTTPStream = func(event firecracker.HTTPStreamEvent) error {
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, "", encoded, "", 0)
			return nil
		}
	}

	driverExecuteStarted := time.Now()
	var (
		result        *firecracker.ExecuteResult
		execErr       error
		deferredClean func()
	)
	if deferredDriver, ok := s.driver.(firecracker.DeferredCleanupDriver); ok {
		result, deferredClean, execErr = deferredDriver.ExecuteDeferred(ctx, executeReq)
	} else {
		result, execErr = s.driver.Execute(ctx, executeReq)
	}
	driverExecuteMs = time.Since(driverExecuteStarted).Milliseconds()
	trace.Add("driver_execute", time.Duration(driverExecuteMs)*time.Millisecond)
	resultLogs := ""
	logs := ""
	var output []byte
	exitCode := 1
	var limitErr error
	if result != nil {
		resultLogs = timetrace.Combine(result.PlatformTrace, result.Logs)
		logs, output, limitErr = enforceExecutionResultLimits(timetrace.Combine(trace.String(), resultLogs), result.Output)
		exitCode = result.ExitCode
	} else {
		logs, output, limitErr = enforceExecutionResultLimits(trace.String(), nil)
	}
	if execErr != nil {
		stopRunningHeartbeats()
		errMsg := combineErrorMessages(execErr, limitErr)
		terminalState = "failed"
		terminalErr = errMsg
		s.archiveExecutionArtifactsAsync(msg.JobId, msg.AttemptId, logs, output)
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, logs, output, errMsg, terminalExitCode(exitCode))
		if deferredClean != nil {
			deferredClean()
		}
		releaseSlot()
		sendHeartbeat()
		return
	}
	stopRunningHeartbeats()
	if limitErr != nil {
		errMsg := limitErr.Error()
		terminalState = "failed"
		terminalErr = errMsg
		s.archiveExecutionArtifactsAsync(msg.JobId, msg.AttemptId, logs, output)
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, logs, output, errMsg, terminalExitCode(exitCode))
		if deferredClean != nil {
			deferredClean()
		}
		releaseSlot()
		sendHeartbeat()
		return
	}
	terminalState = "succeeded"
	s.archiveExecutionArtifactsAsync(msg.JobId, msg.AttemptId, logs, output)
	emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED, logs, output, "", exitCode)
	if deferredClean != nil {
		deferredClean()
	}
	if s.shouldHoldSlotDuringWarmPrepare(executeReq) {
		holdDeferredRelease = true
		s.prepareWarmAsync(executeReq, msg.FunctionVersionId, releaseSlot, sendHeartbeat)
		return
	}
	releaseSlot()
	sendHeartbeat()
	s.prepareWarmAsync(executeReq, msg.FunctionVersionId, nil, sendHeartbeat)
}

func isDirectStreamAttempt(attemptID string) bool {
	return strings.HasPrefix(strings.TrimSpace(attemptID), "direct-attempt-")
}

func (s *Service) shouldHoldSlotDuringWarmPrepare(req firecracker.ExecuteRequest) bool {
	return false
}

func (s *Service) prepareWarmAsync(req firecracker.ExecuteRequest, functionVersionID string, releaseSlot func(), sendHeartbeat func()) {
	finish := func() {
		if releaseSlot != nil {
			releaseSlot()
		}
		sendHeartbeat()
	}
	warmer, ok := s.driver.(firecracker.PostExecutionWarmer)
	if ok {
		go func() {
			warmPrepareStarted := time.Now()
			prepareCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := warmer.PrepareFunctionWarm(prepareCtx, req); err != nil {
				slog.Warn("node-agent async warm prepare failed",
					"hostID", s.hostID,
					"region", s.region,
					"functionVersionID", functionVersionID,
					"durationMs", time.Since(warmPrepareStarted).Milliseconds(),
					"err", err,
				)
				finish()
				return
			}
			slog.Info("node-agent async warm prepare completed",
				"hostID", s.hostID,
				"region", s.region,
				"functionVersionID", functionVersionID,
				"durationMs", time.Since(warmPrepareStarted).Milliseconds(),
			)
			finish()
		}()
		return
	}
	go func() {
		s.mu.Lock()
		s.functionWarm[functionVersionID] = 1
		s.mu.Unlock()
		finish()
	}()
}

func (s *Service) prepareSnapshot(ctx context.Context, msg *regionv1.PrepareSnapshot, sendHeartbeat func()) {
	s.reserveAssignmentSlots(msg.NetworkPolicy)
	defer func() {
		s.releaseAssignmentSlots(msg.NetworkPolicy)
		sendHeartbeat()
	}()

	switch msg.SnapshotKind {
	case regionv1.SnapshotKind_SNAPSHOT_KIND_BLANK:
		if warmer, ok := s.driver.(firecracker.BlankWarmEnsurer); ok {
			_ = warmer.EnsureBlankWarm(ctx)
		}
	case regionv1.SnapshotKind_SNAPSHOT_KIND_FUNCTION:
		if strings.TrimSpace(msg.FunctionVersionId) == "" {
			return
		}
		bundleKey := strings.TrimSpace(msg.ArtifactBundleKey)
		if bundleKey == "" {
			return
		}
		warmer, ok := s.driver.(firecracker.PostExecutionWarmer)
		if !ok {
			return
		}
		bundle, err := s.loadBundle(ctx, bundleKey)
		if err != nil {
			return
		}
		_ = warmer.PrepareFunctionWarm(ctx, firecracker.ExecuteRequest{
			AttemptID:      "prepare-" + msg.FunctionVersionId,
			JobID:          "prepare-" + msg.FunctionVersionId,
			FunctionID:     msg.FunctionVersionId,
			Entrypoint:     msg.Entrypoint,
			ArtifactBundle: bundle,
			Timeout:        time.Duration(msg.TimeoutSec) * time.Second,
			MemoryMB:       int(msg.MemoryMb),
			NetworkPolicy:  msg.NetworkPolicy,
			Region:         s.region,
			HostID:         s.hostID,
		})
	}
}

func (s *Service) loadBundle(ctx context.Context, bundleKey string) ([]byte, error) {
	bundleKey = strings.TrimSpace(bundleKey)
	if bundleKey == "" {
		return nil, fmt.Errorf("artifact bundle key is required")
	}

	s.bundleMu.RLock()
	cached, ok := s.bundleCache[bundleKey]
	s.bundleMu.RUnlock()
	if ok {
		return cached, nil
	}

	bundle, err := s.objects.Get(ctx, bundleKey)
	if err != nil {
		return nil, err
	}

	s.bundleMu.Lock()
	if existing, ok := s.bundleCache[bundleKey]; ok {
		s.bundleMu.Unlock()
		return existing, nil
	}
	s.bundleCache[bundleKey] = bundle
	s.bundleMu.Unlock()
	return bundle, nil
}

func (s *Service) archiveExecutionArtifacts(ctx context.Context, jobID, attemptID, logs string, output []byte) error {
	if s.objects == nil {
		return fmt.Errorf("artifact store is not configured")
	}
	if err := s.objects.Put(ctx, artifact.ExecutionLogsKey(jobID, attemptID), []byte(logs)); err != nil {
		return err
	}
	normalizedOutput := []byte("null")
	if len(output) > 0 {
		normalizedOutput = append([]byte(nil), output...)
	}
	return s.objects.Put(ctx, artifact.ExecutionOutputKey(jobID, attemptID), normalizedOutput)
}

func (s *Service) archiveExecutionArtifactsAsync(jobID, attemptID, logs string, output []byte) {
	if s.objects == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.archiveExecutionArtifacts(ctx, jobID, attemptID, logs, output)
	}()
}

func enforceExecutionResultLimits(logs string, output []byte) (string, []byte, error) {
	normalizedLogs := []byte(logs)
	errs := make([]string, 0, 2)
	if len(normalizedLogs) > maxExecutionLogBytes {
		normalizedLogs = append([]byte(nil), normalizedLogs[:maxExecutionLogBytes]...)
		errs = append(errs, fmt.Sprintf("execution logs exceeded limit of %d bytes", maxExecutionLogBytes))
	} else {
		normalizedLogs = append([]byte(nil), normalizedLogs...)
	}

	var normalizedOutput []byte
	if len(output) > maxExecutionOutputBytes {
		errs = append(errs, fmt.Sprintf("execution output exceeded limit of %d bytes", maxExecutionOutputBytes))
	} else if len(output) > 0 {
		normalizedOutput = append([]byte(nil), output...)
	}

	if len(errs) > 0 {
		return string(normalizedLogs), normalizedOutput, errors.New(strings.Join(errs, "; "))
	}
	return string(normalizedLogs), normalizedOutput, nil
}

func combineErrorMessages(primary error, secondary error) string {
	if primary == nil && secondary == nil {
		return ""
	}
	if primary == nil {
		return secondary.Error()
	}
	if secondary == nil {
		return primary.Error()
	}
	return primary.Error() + "; " + secondary.Error()
}

func terminalExitCode(exitCode int) int {
	if exitCode == 0 {
		return 1
	}
	return exitCode
}

func (s *Service) assignmentHeartbeatLoop(ctx context.Context, sendRunningHeartbeat func()) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendRunningHeartbeat()
		}
	}
}

func (s *Service) registrationMessage() *regionv1.RegisterHost {
	inventory := s.warmInventory()
	s.mu.Lock()
	availableSlots := s.availableSlots
	availableFull := s.availableFull
	s.mu.Unlock()
	return &regionv1.RegisterHost{
		HostId:                    s.hostID,
		Region:                    s.region,
		Driver:                    s.driverName,
		AvailableSlots:            int32(availableSlots),
		BlankWarm:                 int32(inventory.BlankWarm),
		FunctionWarm:              warmMetrics(inventory.FunctionWarm),
		AvailableFullNetworkSlots: int32(availableFull),
	}
}

func (s *Service) send(stream grpc.BidiStreamingClient[regionv1.AgentMessage, regionv1.CoordinatorMessage], msg *regionv1.AgentMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return stream.Send(msg)
}

func (s *Service) heartbeatMessage() *regionv1.HostHeartbeat {
	inventory := s.warmInventory()
	s.mu.Lock()
	availableSlots := s.availableSlots
	availableFull := s.availableFull
	s.mu.Unlock()
	return &regionv1.HostHeartbeat{
		HostId:                    s.hostID,
		Region:                    s.region,
		AvailableSlots:            int32(availableSlots),
		BlankWarm:                 int32(inventory.BlankWarm),
		FunctionWarm:              warmMetrics(inventory.FunctionWarm),
		AvailableFullNetworkSlots: int32(availableFull),
	}
}

func (s *Service) prepareDriver(ctx context.Context) error {
	if warmer, ok := s.driver.(firecracker.BlankWarmEnsurer); ok {
		if err := warmer.EnsureBlankWarm(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) warmInventory() firecracker.WarmInventory {
	s.mu.Lock()
	availableSlots := s.availableSlots
	s.mu.Unlock()
	if provider, ok := s.driver.(firecracker.SlotWarmInventoryProvider); ok {
		return provider.WarmInventoryForSlots(availableSlots)
	}
	if provider, ok := s.driver.(firecracker.InventoryProvider); ok {
		return provider.WarmInventory()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	functionWarm := make(map[string]int, len(s.functionWarm))
	for functionID, count := range s.functionWarm {
		functionWarm[functionID] = minInt(count, s.availableSlots)
	}
	return firecracker.WarmInventory{
		BlankWarm:    minInt(s.blankWarm, s.availableSlots),
		FunctionWarm: functionWarm,
	}
}

func (s *Service) reserveSlot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.availableSlots > 0 {
		s.availableSlots--
	}
}

func (s *Service) releaseSlot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.availableSlots < s.maxSlots {
		s.availableSlots++
	}
}

func (s *Service) applyDrain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.availableSlots = 0
	s.availableFull = 0
	s.blankWarm = 0
	s.functionWarm = map[string]int{}
}

func (s *Service) reserveAssignmentSlots(networkPolicy string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.availableSlots > 0 {
		s.availableSlots--
	}
	if isFullNetworkPolicy(networkPolicy) && s.availableFull > 0 {
		s.availableFull--
	}
}

func (s *Service) releaseAssignmentSlots(networkPolicy string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.availableSlots < s.maxSlots {
		s.availableSlots++
	}
	if isFullNetworkPolicy(networkPolicy) && s.availableFull < s.maxFullNetwork {
		s.availableFull++
	}
}

func warmMetrics(input map[string]int) []*regionv1.WarmPoolMetric {
	out := make([]*regionv1.WarmPoolMetric, 0, len(input))
	for functionID, count := range input {
		out = append(out, &regionv1.WarmPoolMetric{
			FunctionVersionId: functionID,
			Available:         int32(count),
		})
	}
	return out
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func isFullNetworkPolicy(policy string) bool {
	return strings.EqualFold(strings.TrimSpace(policy), "full")
}
