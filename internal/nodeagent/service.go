package nodeagent

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	regionv1 "github.com/ishaan/eeeverc/eeeverc/region/v1"

	"github.com/ishaan/eeeverc/internal/artifact"
	"github.com/ishaan/eeeverc/internal/firecracker"
	"github.com/ishaan/eeeverc/internal/secrets"
	"github.com/ishaan/eeeverc/internal/store"
)

type Service struct {
	hostID          string
	region          string
	driverName      string
	coordinatorAddr string
	driver          firecracker.Driver
	objects         artifact.Store
	store           store.Store
	secrets         secrets.Provider
	dialOptions     []grpc.DialOption

	mu             sync.Mutex
	sendMu         sync.Mutex
	availableSlots int
	blankWarm      int
	functionWarm   map[string]int
}

func New(hostID, region, coordinatorAddr string, driver firecracker.Driver, objects artifact.Store, store store.Store, secrets secrets.Provider, dialOptions ...grpc.DialOption) *Service {
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
			go s.executeAssignment(ctx, stream, body.Assignment)
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

func (s *Service) executeAssignment(ctx context.Context, stream grpc.BidiStreamingClient[regionv1.AgentMessage, regionv1.CoordinatorMessage], msg *regionv1.ExecutionAssignment) {
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

	sendUpdate := func(state regionv1.AssignmentState, logs string, output []byte, errMsg string, exitCode int) {
		_ = s.send(stream, &regionv1.AgentMessage{
			Body: &regionv1.AgentMessage_AssignmentUpdate{
				AssignmentUpdate: &regionv1.AssignmentUpdate{
					HostId:       s.hostID,
					Region:       s.region,
					AttemptId:    msg.AttemptId,
					JobId:        msg.JobId,
					State:        state,
					Logs:         logs,
					OutputJson:   output,
					ErrorMessage: errMsg,
					ExitCode:     int32(exitCode),
				},
			},
		})
	}

	sendUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_STARTING, "", nil, "", 0)

	artifactMeta, err := s.store.GetArtifact(ctx, msg.ArtifactDigest)
	if err != nil {
		sendUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, "", nil, err.Error(), 1)
		return
	}
	bundle, err := s.objects.Get(ctx, artifactMeta.BundleKey)
	if err != nil {
		sendUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, "", nil, err.Error(), 1)
		return
	}
	env, err := s.secrets.Resolve(ctx, msg.EnvRefs)
	if err != nil {
		sendUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, "", nil, err.Error(), 1)
		return
	}

	sendUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, "", nil, "", 0)
	runCtx, stopRunningHeartbeats := context.WithCancel(ctx)
	defer stopRunningHeartbeats()
	go s.assignmentHeartbeatLoop(runCtx, stream, msg)

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
		if result != nil {
			sendUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, result.Logs, result.Output, execErr.Error(), result.ExitCode)
		} else {
			sendUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED, "", nil, execErr.Error(), 1)
		}
		releaseSlot()
		_ = s.send(stream, &regionv1.AgentMessage{
			Body: &regionv1.AgentMessage_Heartbeat{
				Heartbeat: s.heartbeatMessage(),
			},
		})
		return
	}
	stopRunningHeartbeats()
	if result.SnapshotEligible {
		s.mu.Lock()
		s.functionWarm[msg.FunctionVersionId] = 1
		s.mu.Unlock()
	}
	sendUpdate(regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED, result.Logs, result.Output, "", result.ExitCode)
	releaseSlot()
	_ = s.send(stream, &regionv1.AgentMessage{
		Body: &regionv1.AgentMessage_Heartbeat{
			Heartbeat: s.heartbeatMessage(),
		},
	})
}

func (s *Service) assignmentHeartbeatLoop(ctx context.Context, stream grpc.BidiStreamingClient[regionv1.AgentMessage, regionv1.CoordinatorMessage], msg *regionv1.ExecutionAssignment) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.send(stream, &regionv1.AgentMessage{
				Body: &regionv1.AgentMessage_AssignmentUpdate{
					AssignmentUpdate: &regionv1.AssignmentUpdate{
						HostId:    s.hostID,
						Region:    s.region,
						AttemptId: msg.AttemptId,
						JobId:     msg.JobId,
						State:     regionv1.AssignmentState_ASSIGNMENT_STATE_RUNNING,
					},
				},
			})
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
