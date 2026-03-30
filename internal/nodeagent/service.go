package nodeagent

import (
	"context"
	"fmt"
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
	sendMu         sync.Mutex
	availableSlots int
	blankWarm      int
	functionWarm   map[string]int
}

type EmbeddedCoordinator interface {
	UpdateEmbeddedHeartbeat(ctx context.Context, msg *regionv1.HostHeartbeat) error
	ApplyAssignmentUpdate(ctx context.Context, msg *regionv1.AssignmentUpdate) error
}

func New(hostID, region, coordinatorAddr string, driver firecracker.Driver, objects artifact.Store, store store.Store, secrets secrets.ExecutionResolver, dialOptions ...grpc.DialOption) *Service {
	if len(dialOptions) == 0 {
		dialOptions = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	return &Service{
		hostID:          hostID,
		region:          region,
		driverName:      "local-node",
		coordinatorAddr: coordinatorAddr,
		driver:          driver,
		objects:         objects,
		store:           store,
		secrets:         secrets,
		dialOptions:     append([]grpc.DialOption(nil), dialOptions...),
		availableSlots:  1,
		blankWarm:       1,
		functionWarm:    map[string]int{},
	}
}

func (s *Service) Run(ctx context.Context) error {
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
		case *regionv1.CoordinatorMessage_Drain:
			s.mu.Lock()
			s.availableSlots = 0
			s.blankWarm = 0
			s.functionWarm = map[string]int{}
			s.mu.Unlock()
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
		s.availableSlots++
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
		Region:         s.region,
		HostID:         s.hostID,
	})
	if execErr != nil {
		stopRunningHeartbeats()
		logs := ""
		var output []byte
		exitCode := 1
		if result != nil {
			logs = result.Logs
			output = result.Output
			exitCode = result.ExitCode
		}
		errMsg := execErr.Error()
		if archiveErr := s.archiveExecutionArtifacts(ctx, msg.JobId, msg.AttemptId, logs, output); archiveErr != nil {
			errMsg = fmt.Sprintf("%s; archive execution artifacts: %v", errMsg, archiveErr)
		}
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, logs, output, errMsg, exitCode)
		releaseSlot()
		sendHeartbeat()
		return
	}
	stopRunningHeartbeats()
	if err := s.archiveExecutionArtifacts(ctx, msg.JobId, msg.AttemptId, result.Logs, result.Output); err != nil {
		emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, result.Logs, result.Output, fmt.Sprintf("archive execution artifacts: %v", err), result.ExitCode)
		releaseSlot()
		sendHeartbeat()
		return
	}
	if result.SnapshotEligible {
		s.mu.Lock()
		s.functionWarm[msg.FunctionVersionId] = 1
		s.mu.Unlock()
	}
	emitUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED, result.Logs, result.Output, "", result.ExitCode)
	releaseSlot()
	sendHeartbeat()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return &regionv1.RegisterHost{
		HostId:         s.hostID,
		Region:         s.region,
		Driver:         s.driverName,
		AvailableSlots: int32(s.availableSlots),
		BlankWarm:      int32(s.blankWarm),
		FunctionWarm:   warmMetrics(s.functionWarm),
	}
}

func (s *Service) send(stream grpc.BidiStreamingClient[regionv1.AgentMessage, regionv1.CoordinatorMessage], msg *regionv1.AgentMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return stream.Send(msg)
}

func (s *Service) heartbeatMessage() *regionv1.HostHeartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &regionv1.HostHeartbeat{
		HostId:         s.hostID,
		Region:         s.region,
		AvailableSlots: int32(s.availableSlots),
		BlankWarm:      int32(s.blankWarm),
		FunctionWarm:   warmMetrics(s.functionWarm),
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
