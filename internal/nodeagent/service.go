package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	regionv1 "github.com/theg1239/lecrev/lecrev/region/v1"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/firecracker"
	"github.com/theg1239/lecrev/internal/secrets"
	"github.com/theg1239/lecrev/internal/store"
)

type Service struct {
	hostID          string
	region          string
	driverName      string
	coordinatorAddr string
	driver          firecracker.Driver
	objects         artifact.Store
	store           store.Store
	secrets         secrets.ExecutionResolver
	dialOptions     []grpc.DialOption

	mu             sync.Mutex
	maxSlots       int
	sendMu         sync.Mutex
	availableSlots int
	blankWarm      int
	functionWarm   map[string]int
}

const (
	maxExecutionLogBytes    = 1 << 20
	maxExecutionOutputBytes = 1 << 20
)

type EmbeddedCoordinator interface {
	UpdateEmbeddedHeartbeat(ctx context.Context, msg *regionv1.HostHeartbeat) error
	ApplyAssignmentUpdate(ctx context.Context, msg *regionv1.AssignmentUpdate) error
}

type Config struct {
	MaxConcurrentAssignments int
}

func (c Config) withDefaults() Config {
	if c.MaxConcurrentAssignments <= 0 {
		c.MaxConcurrentAssignments = 1
	}
	return c
}

func New(hostID, region, coordinatorAddr string, driver firecracker.Driver, objects artifact.Store, store store.Store, secrets secrets.ExecutionResolver, dialOptions ...grpc.DialOption) *Service {
	return NewWithConfig(Config{}, hostID, region, coordinatorAddr, driver, objects, store, secrets, dialOptions...)
}

func NewWithConfig(cfg Config, hostID, region, coordinatorAddr string, driver firecracker.Driver, objects artifact.Store, store store.Store, secrets secrets.ExecutionResolver, dialOptions ...grpc.DialOption) *Service {
	cfg = cfg.withDefaults()
	if len(dialOptions) == 0 {
		dialOptions = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
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
		store:           store,
		secrets:         secrets,
		dialOptions:     append([]grpc.DialOption(nil), dialOptions...),
		maxSlots:        cfg.MaxConcurrentAssignments,
		availableSlots:  cfg.MaxConcurrentAssignments,
		blankWarm:       1,
		functionWarm:    map[string]int{},
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
	s.mu.Lock()
	if s.availableSlots > 0 {
		s.availableSlots--
	}
	s.mu.Unlock()
	released := false
	releaseSlot := func() {
		if released {
			return
		}
		released = true
		s.mu.Lock()
		if s.availableSlots < s.maxSlots {
			s.availableSlots++
		}
		s.mu.Unlock()
	}
	defer releaseSlot()

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

	artifactMeta, err := s.store.GetArtifact(ctx, msg.ArtifactDigest)
	if err != nil {
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, "", nil, err.Error(), 1)
		return
	}
	version, err := s.store.GetFunctionVersion(ctx, msg.FunctionVersionId)
	if err != nil {
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, "", nil, err.Error(), 1)
		return
	}
	bundle, err := s.objects.Get(ctx, artifactMeta.BundleKey)
	if err != nil {
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, "", nil, err.Error(), 1)
		return
	}
	env, err := s.secrets.ResolveExecution(ctx, secrets.ExecutionRequest{
		HostID:            s.hostID,
		Region:            s.region,
		FunctionVersionID: msg.FunctionVersionId,
		SecretRefs:        msg.EnvRefs,
	})
	if err != nil {
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, "", nil, err.Error(), 1)
		return
	}

	emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, "", nil, "", 0)
	runCtx, stopRunningHeartbeats := context.WithCancel(ctx)
	defer stopRunningHeartbeats()
	go s.assignmentHeartbeatLoop(runCtx, func() {
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, "", nil, "", 0)
	})

	result, execErr := s.driver.Execute(ctx, firecracker.ExecuteRequest{
		AttemptID:      msg.AttemptId,
		JobID:          msg.JobId,
		FunctionID:     msg.FunctionVersionId,
		Entrypoint:     msg.Entrypoint,
		ArtifactBundle: bundle,
		Payload:        msg.PayloadJson,
		Env:            env,
		Timeout:        time.Duration(msg.TimeoutSec) * time.Second,
		MemoryMB:       version.MemoryMB,
		NetworkPolicy:  msg.NetworkPolicy,
		Region:         s.region,
		HostID:         s.hostID,
	})
	logs := ""
	var output []byte
	exitCode := 1
	var limitErr error
	if result != nil {
		logs, output, limitErr = enforceExecutionResultLimits(result.Logs, result.Output)
		exitCode = result.ExitCode
	}
	if execErr != nil {
		stopRunningHeartbeats()
		errMsg := combineErrorMessages(execErr, limitErr)
		if archiveErr := s.archiveExecutionArtifacts(ctx, msg.JobId, msg.AttemptId, logs, output); archiveErr != nil {
			errMsg = fmt.Sprintf("%s; archive execution artifacts: %v", errMsg, archiveErr)
		}
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, logs, output, errMsg, terminalExitCode(exitCode))
		releaseSlot()
		sendHeartbeat()
		return
	}
	stopRunningHeartbeats()
	if limitErr != nil {
		errMsg := limitErr.Error()
		if archiveErr := s.archiveExecutionArtifacts(ctx, msg.JobId, msg.AttemptId, logs, output); archiveErr != nil {
			errMsg = fmt.Sprintf("%s; archive execution artifacts: %v", errMsg, archiveErr)
		}
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, logs, output, errMsg, terminalExitCode(exitCode))
		releaseSlot()
		sendHeartbeat()
		return
	}
	if err := s.archiveExecutionArtifacts(ctx, msg.JobId, msg.AttemptId, logs, output); err != nil {
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, logs, output, fmt.Sprintf("archive execution artifacts: %v", err), terminalExitCode(exitCode))
		releaseSlot()
		sendHeartbeat()
		return
	}
	emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED, logs, output, "", exitCode)
	sendHeartbeat()
	if warmer, ok := s.driver.(firecracker.PostExecutionWarmer); ok {
		_ = warmer.PrepareFunctionWarm(ctx, firecracker.ExecuteRequest{
			AttemptID:      msg.AttemptId,
			JobID:          msg.JobId,
			FunctionID:     msg.FunctionVersionId,
			Entrypoint:     msg.Entrypoint,
			ArtifactBundle: bundle,
			Payload:        msg.PayloadJson,
			Env:            env,
			Timeout:        time.Duration(msg.TimeoutSec) * time.Second,
			MemoryMB:       version.MemoryMB,
			NetworkPolicy:  msg.NetworkPolicy,
			Region:         s.region,
			HostID:         s.hostID,
		})
	} else if result.SnapshotEligible {
		s.mu.Lock()
		s.functionWarm[msg.FunctionVersionId] = 1
		s.mu.Unlock()
	}
	releaseSlot()
	sendHeartbeat()
}

func (s *Service) prepareSnapshot(ctx context.Context, msg *regionv1.PrepareSnapshot, sendHeartbeat func()) {
	s.reserveSlot()
	defer func() {
		s.releaseSlot()
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
		warmer, ok := s.driver.(firecracker.PostExecutionWarmer)
		if !ok {
			return
		}
		version, err := s.store.GetFunctionVersion(ctx, msg.FunctionVersionId)
		if err != nil {
			return
		}
		artifactMeta, err := s.store.GetArtifact(ctx, version.ArtifactDigest)
		if err != nil {
			return
		}
		bundle, err := s.objects.Get(ctx, artifactMeta.BundleKey)
		if err != nil {
			return
		}
		_ = warmer.PrepareFunctionWarm(ctx, firecracker.ExecuteRequest{
			AttemptID:      "prepare-" + version.ID,
			JobID:          "prepare-" + version.ID,
			FunctionID:     version.ID,
			Entrypoint:     version.Entrypoint,
			ArtifactBundle: bundle,
			Timeout:        time.Duration(version.TimeoutSec) * time.Second,
			MemoryMB:       version.MemoryMB,
			NetworkPolicy:  string(version.NetworkPolicy),
			Region:         s.region,
			HostID:         s.hostID,
		})
	}
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
	s.mu.Unlock()
	return &regionv1.RegisterHost{
		HostId:         s.hostID,
		Region:         s.region,
		Driver:         s.driverName,
		AvailableSlots: int32(availableSlots),
		BlankWarm:      int32(inventory.BlankWarm),
		FunctionWarm:   warmMetrics(inventory.FunctionWarm),
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
	s.mu.Unlock()
	return &regionv1.HostHeartbeat{
		HostId:         s.hostID,
		Region:         s.region,
		AvailableSlots: int32(availableSlots),
		BlankWarm:      int32(inventory.BlankWarm),
		FunctionWarm:   warmMetrics(inventory.FunctionWarm),
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
	s.blankWarm = 0
	s.functionWarm = map[string]int{}
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
