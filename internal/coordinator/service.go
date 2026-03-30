package coordinator

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"

	regionv1 "github.com/theg1239/lecrev/lecrev/region/v1"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/dispatch"
	"github.com/theg1239/lecrev/internal/domain"
	"github.com/theg1239/lecrev/internal/store"
)

type Service struct {
	regionv1.UnimplementedCoordinatorServer

	region string
	store  store.Store
	retry  Retryer
	bus    dispatch.ExecutionBus
	now    func() time.Time

	staleHostAfter time.Duration
	reapInterval   time.Duration

	mu    sync.Mutex
	hosts map[string]*hostSession
}

type hostSession struct {
	host     domain.Host
	sendCh   chan *regionv1.CoordinatorMessage
	executor EmbeddedExecutor
	preparer EmbeddedPreparer
}

type EmbeddedExecutor func(context.Context, *regionv1.ExecutionAssignment)
type EmbeddedPreparer func(context.Context, *regionv1.PrepareSnapshot)

type Retryer interface {
	RetryExecution(ctx context.Context, jobID string) (*domain.ExecutionJob, error)
}

func New(region string, store store.Store, retry Retryer) *Service {
	return &Service{
		region:         region,
		store:          store,
		retry:          retry,
		now:            func() time.Time { return time.Now().UTC() },
		staleHostAfter: 20 * time.Second,
		reapInterval:   5 * time.Second,
		hosts:          make(map[string]*hostSession),
	}
}

func (s *Service) SetRetryer(retry Retryer) {
	s.retry = retry
}

func (s *Service) SetExecutionBus(bus dispatch.ExecutionBus) {
	s.bus = bus
}

func (s *Service) Region() string {
	return s.region
}

func (s *Service) Stats() domain.RegionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := domain.RegionStats{}
	for _, session := range s.hosts {
		if session.host.State != domain.HostStateActive {
			continue
		}
		if session.host.AvailableSlots > 0 {
			stats.AvailableHosts++
		}
		stats.BlankWarm += session.host.BlankWarm
		for _, count := range session.host.FunctionWarm {
			stats.FunctionWarm += count
		}
	}
	return stats
}

func (s *Service) Listen(ctx context.Context, addr string, opts ...grpc.ServerOption) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := grpc.NewServer(opts...)
	regionv1.RegisterCoordinatorServer(server, s)

	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()
	go s.reapHostLoop(ctx)

	return server.Serve(lis)
}

func (s *Service) EnqueueExecution(ctx context.Context, assignment domain.Assignment) error {
	if s.bus != nil {
		return s.bus.PublishExecution(ctx, s.region, assignment)
	}
	return s.assignExecution(ctx, assignment)
}

func (s *Service) RunExecutionConsumer(ctx context.Context, consumer string) error {
	if s.bus == nil {
		return nil
	}
	return s.bus.ConsumeExecution(ctx, s.region, consumer, s.assignExecution)
}

func (s *Service) DrainHost(ctx context.Context, hostID, reason string) error {
	s.mu.Lock()
	session, ok := s.hosts[hostID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("host %s not registered in region %s", hostID, s.region)
	}
	session.host.State = domain.HostStateDraining
	session.host.AvailableSlots = 0
	host := session.host
	sendCh := session.sendCh
	s.mu.Unlock()

	if err := s.store.UpdateHost(ctx, &host); err != nil {
		return err
	}
	if err := s.store.ReplaceWarmPoolsForHost(ctx, host.Region, host.ID, nil); err != nil {
		return err
	}
	if err := s.persistRegion(); err != nil {
		return err
	}
	if sendCh == nil {
		return nil
	}

	msg := &regionv1.CoordinatorMessage{
		Body: &regionv1.CoordinatorMessage_Drain{
			Drain: &regionv1.DrainHost{
				HostId: hostID,
				Reason: reason,
			},
		},
	}
	select {
	case sendCh <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("host %s control channel full", hostID)
	}
}

func (s *Service) PrepareFunctionWarm(ctx context.Context, version *domain.FunctionVersion) error {
	if version == nil {
		return fmt.Errorf("function version is required")
	}
	artifactMeta, err := s.store.GetArtifact(ctx, version.ArtifactDigest)
	if err != nil {
		return err
	}
	hostID, session, alreadyWarm, err := s.pickWarmHost(version.ID)
	if err != nil {
		return err
	}
	if alreadyWarm {
		return nil
	}
	msg := &regionv1.PrepareSnapshot{
		HostId:            hostID,
		SnapshotKind:      regionv1.SnapshotKind_SNAPSHOT_KIND_FUNCTION,
		FunctionVersionId: version.ID,
		ArtifactBundleKey: artifactMeta.BundleKey,
		Entrypoint:        version.Entrypoint,
		NetworkPolicy:     string(version.NetworkPolicy),
		TimeoutSec:        int32(version.TimeoutSec),
		MemoryMb:          int32(version.MemoryMB),
	}
	if session.preparer != nil {
		go session.preparer(context.Background(), msg)
		return nil
	}
	control := &regionv1.CoordinatorMessage{
		Body: &regionv1.CoordinatorMessage_Prepare{
			Prepare: msg,
		},
	}
	select {
	case session.sendCh <- control:
		return nil
	case <-ctx.Done():
		s.restorePreparationReservation(hostID)
		return ctx.Err()
	default:
		s.restorePreparationReservation(hostID)
		return fmt.Errorf("host %s control channel full", hostID)
	}
}

func (s *Service) assignExecution(ctx context.Context, assignment domain.Assignment) error {
	hostID, session, startMode, err := s.pickHost(assignment)
	if err != nil {
		return err
	}

	attempt, err := s.store.GetAttempt(ctx, assignment.AttemptID)
	if err != nil {
		return err
	}
	attempt.HostID = hostID
	attempt.StartMode = startMode
	attempt.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateAttempt(ctx, attempt); err != nil {
		return err
	}

	execAssignment := &regionv1.ExecutionAssignment{
		AttemptId:         assignment.AttemptID,
		JobId:             assignment.JobID,
		FunctionVersionId: assignment.FunctionVersionID,
		ArtifactDigest:    assignment.ArtifactDigest,
		ArtifactBundleKey: assignment.ArtifactBundleKey,
		Entrypoint:        assignment.Entrypoint,
		PayloadJson:       assignment.Payload,
		EnvRefs:           assignment.EnvRefs,
		NetworkPolicy:     string(assignment.NetworkPolicy),
		TimeoutSec:        int32(assignment.TimeoutSec),
		MemoryMb:          int32(assignment.MemoryMB),
	}
	msg := &regionv1.CoordinatorMessage{
		Body: &regionv1.CoordinatorMessage_Assignment{
			Assignment: execAssignment,
		},
	}
	if session.executor != nil {
		go session.executor(context.Background(), execAssignment)
		return nil
	}

	select {
	case session.sendCh <- msg:
		return nil
	case <-ctx.Done():
		s.restoreHostReservation(hostID, assignment.FunctionVersionID, startMode)
		return ctx.Err()
	default:
		s.restoreHostReservation(hostID, assignment.FunctionVersionID, startMode)
		return fmt.Errorf("host %s control channel full", hostID)
	}
}

func (s *Service) Control(stream regionv1.Coordinator_ControlServer) error {
	sendCh := make(chan *regionv1.CoordinatorMessage, 32)
	sendErr := make(chan error, 1)
	go func() {
		for msg := range sendCh {
			if err := stream.Send(msg); err != nil {
				sendErr <- err
				return
			}
		}
	}()
	defer close(sendCh)

	var hostID string
	for {
		select {
		case err := <-sendErr:
			if hostID != "" {
				s.markHostDown(hostID)
			}
			return err
		default:
		}

		msg, err := stream.Recv()
		if err == io.EOF {
			if hostID != "" {
				s.markHostDown(hostID)
			}
			return nil
		}
		if err != nil {
			if hostID != "" {
				s.markHostDown(hostID)
			}
			return err
		}

		switch body := msg.Body.(type) {
		case *regionv1.AgentMessage_Register:
			hostID = body.Register.HostId
			if err := s.registerHost(body.Register, sendCh); err != nil {
				return err
			}
			sendCh <- &regionv1.CoordinatorMessage{
				Body: &regionv1.CoordinatorMessage_Registered{
					Registered: &regionv1.Registered{HostId: hostID},
				},
			}
		case *regionv1.AgentMessage_Heartbeat:
			if err := s.updateHeartbeat(body.Heartbeat); err != nil {
				return err
			}
		case *regionv1.AgentMessage_AssignmentUpdate:
			if err := s.handleAssignmentUpdate(stream.Context(), body.AssignmentUpdate); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown agent message")
		}
	}
}

func (s *Service) registerHost(msg *regionv1.RegisterHost, sendCh chan *regionv1.CoordinatorMessage) error {
	return s.registerHostSession(context.Background(), msg, sendCh, nil, nil)
}

func (s *Service) RegisterEmbeddedHost(ctx context.Context, msg *regionv1.RegisterHost, executor EmbeddedExecutor, preparer EmbeddedPreparer) error {
	return s.registerHostSession(ctx, msg, nil, executor, preparer)
}

func (s *Service) registerHostSession(ctx context.Context, msg *regionv1.RegisterHost, sendCh chan *regionv1.CoordinatorMessage, executor EmbeddedExecutor, preparer EmbeddedPreparer) error {
	host := domain.Host{
		ID:             msg.HostId,
		Region:         msg.Region,
		Driver:         msg.Driver,
		State:          domain.HostStateActive,
		AvailableSlots: int(msg.AvailableSlots),
		BlankWarm:      int(msg.BlankWarm),
		FunctionWarm:   warmMap(msg.FunctionWarm),
		LastHeartbeat:  time.Now().UTC(),
	}
	s.mu.Lock()
	s.hosts[host.ID] = &hostSession{host: host, sendCh: sendCh, executor: executor, preparer: preparer}
	s.mu.Unlock()
	if err := s.store.PutHost(ctx, &host); err != nil {
		return err
	}
	if err := s.store.ReplaceWarmPoolsForHost(ctx, host.Region, host.ID, warmPoolsForHost(host, s.now())); err != nil {
		return err
	}
	return s.persistRegion()
}

func (s *Service) updateHeartbeat(msg *regionv1.HostHeartbeat) error {
	return s.updateHeartbeatContext(context.Background(), msg)
}

func (s *Service) UpdateEmbeddedHeartbeat(ctx context.Context, msg *regionv1.HostHeartbeat) error {
	return s.updateHeartbeatContext(ctx, msg)
}

func (s *Service) updateHeartbeatContext(ctx context.Context, msg *regionv1.HostHeartbeat) error {
	s.mu.Lock()
	session, ok := s.hosts[msg.HostId]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("heartbeat for unknown host %s", msg.HostId)
	}
	session.host.AvailableSlots = int(msg.AvailableSlots)
	session.host.BlankWarm = int(msg.BlankWarm)
	session.host.FunctionWarm = warmMap(msg.FunctionWarm)
	session.host.LastHeartbeat = s.now()
	host := session.host
	s.mu.Unlock()
	if err := s.store.UpdateHost(ctx, &host); err != nil {
		return err
	}
	if err := s.store.ReplaceWarmPoolsForHost(ctx, host.Region, host.ID, warmPoolsForHost(host, s.now())); err != nil {
		return err
	}
	return s.persistRegion()
}

func (s *Service) ApplyAssignmentUpdate(ctx context.Context, msg *regionv1.AssignmentUpdate) error {
	return s.handleAssignmentUpdate(ctx, msg)
}

func (s *Service) handleAssignmentUpdate(ctx context.Context, msg *regionv1.AssignmentUpdate) error {
	attempt, err := s.store.GetAttempt(ctx, msg.AttemptId)
	if err != nil {
		return err
	}
	job, err := s.store.GetExecutionJob(ctx, msg.JobId)
	if err != nil {
		return err
	}

	now := s.now()
	attempt.UpdatedAt = now
	attempt.HostID = msg.HostId

	switch msg.State {
	case regionv1.AssignmentState_ASSIGNMENT_STATE_STARTING:
		attempt.State = domain.AttemptStateStarting
		if attempt.StartedAt.IsZero() {
			attempt.StartedAt = now
		}
		attempt.LeaseExpiresAt = now.Add(30 * time.Second)
		job.State = domain.JobStateAssigned
	case regionv1.AssignmentState_ASSIGNMENT_STATE_RUNNING:
		attempt.State = domain.AttemptStateRunning
		if attempt.StartedAt.IsZero() {
			attempt.StartedAt = now
		}
		attempt.LeaseExpiresAt = now.Add(30 * time.Second)
		job.State = domain.JobStateRunning
	case regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED:
		attempt.State = domain.AttemptStateSucceeded
		attempt.LeaseExpiresAt = now
		job.State = domain.JobStateSucceeded
		job.Error = ""
		job.Result = &domain.JobResult{
			ExitCode:   int(msg.ExitCode),
			Logs:       msg.Logs,
			LogsKey:    artifact.ExecutionLogsKey(msg.JobId, msg.AttemptId),
			Output:     append([]byte(nil), msg.OutputJson...),
			OutputKey:  artifact.ExecutionOutputKey(msg.JobId, msg.AttemptId),
			HostID:     msg.HostId,
			Region:     msg.Region,
			StartedAt:  startTimeOr(now, attempt.StartedAt),
			FinishedAt: now,
		}
		s.releaseHostSlot(msg.HostId)
	case regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED:
		attempt.State = domain.AttemptStateFailed
		attempt.LeaseExpiresAt = now
		attempt.Error = msg.ErrorMessage
		job.Error = msg.ErrorMessage
		job.Result = &domain.JobResult{
			ExitCode:   int(msg.ExitCode),
			Logs:       msg.Logs,
			LogsKey:    artifact.ExecutionLogsKey(msg.JobId, msg.AttemptId),
			Output:     append([]byte(nil), msg.OutputJson...),
			OutputKey:  artifact.ExecutionOutputKey(msg.JobId, msg.AttemptId),
			HostID:     msg.HostId,
			Region:     msg.Region,
			StartedAt:  startTimeOr(now, attempt.StartedAt),
			FinishedAt: now,
		}
		s.releaseHostSlot(msg.HostId)
		if job.AttemptCount <= job.MaxRetries && s.retry != nil {
			job.State = domain.JobStateRetrying
		} else {
			job.State = domain.JobStateFailed
		}
	}

	job.UpdatedAt = now
	if err := s.store.UpdateAttempt(ctx, attempt); err != nil {
		return err
	}
	if err := s.store.UpdateExecutionJob(ctx, job); err != nil {
		return err
	}
	if msg.State == regionv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED || msg.State == regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED {
		if err := s.recordCost(ctx, attempt, job); err != nil {
			return err
		}
	}
	if msg.State == regionv1.AssignmentState_ASSIGNMENT_STATE_FAILED && job.State == domain.JobStateRetrying {
		go func(jobID string) {
			if _, err := s.retry.RetryExecution(context.Background(), jobID); err != nil {
				latest, getErr := s.store.GetExecutionJob(context.Background(), jobID)
				if getErr != nil {
					return
				}
				latest.State = domain.JobStateFailed
				latest.Error = err.Error()
				latest.UpdatedAt = s.now()
				_ = s.store.UpdateExecutionJob(context.Background(), latest)
			}
		}(job.ID)
	}
	return nil
}

func (s *Service) pickHost(assignment domain.Assignment) (string, *hostSession, domain.StartMode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type candidate struct {
		id      string
		session *hostSession
		scoreFn int
	}
	candidates := make([]candidate, 0, len(s.hosts))
	for id, session := range s.hosts {
		if session.host.State != domain.HostStateActive || session.host.AvailableSlots <= 0 {
			continue
		}
		score := session.host.FunctionWarm[assignment.FunctionVersionID]*1000 + session.host.BlankWarm*100 + session.host.AvailableSlots
		candidates = append(candidates, candidate{id: id, session: session, scoreFn: score})
	}
	if len(candidates) == 0 {
		return "", nil, "", fmt.Errorf("no active hosts available in region %s", s.region)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].scoreFn > candidates[j].scoreFn })
	chosen := candidates[0]
	startMode := classifyStartMode(chosen.session.host, assignment.FunctionVersionID)
	chosen.session.host.AvailableSlots--
	switch startMode {
	case domain.StartModeFunctionWarm:
		if chosen.session.host.FunctionWarm[assignment.FunctionVersionID] > 0 {
			chosen.session.host.FunctionWarm[assignment.FunctionVersionID]--
		}
	case domain.StartModeBlankWarm:
		if chosen.session.host.BlankWarm > 0 {
			chosen.session.host.BlankWarm--
		}
	}
	host := chosen.session.host
	_ = s.store.UpdateHost(context.Background(), &host)
	_ = s.store.ReplaceWarmPoolsForHost(context.Background(), host.Region, host.ID, warmPoolsForHost(host, s.now()))
	_ = s.persistRegionLocked()
	return chosen.id, chosen.session, startMode, nil
}

func (s *Service) pickWarmHost(functionVersionID string) (string, *hostSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type candidate struct {
		id      string
		session *hostSession
		score   int
	}
	candidates := make([]candidate, 0, len(s.hosts))
	for id, session := range s.hosts {
		if session.host.State != domain.HostStateActive || session.host.AvailableSlots <= 0 {
			continue
		}
		if session.host.FunctionWarm[functionVersionID] > 0 {
			return id, session, true, nil
		}
		score := session.host.BlankWarm*100 + session.host.AvailableSlots
		candidates = append(candidates, candidate{id: id, session: session, score: score})
	}
	if len(candidates) == 0 {
		return "", nil, false, fmt.Errorf("no active hosts available in region %s for function warm prep", s.region)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	chosen := candidates[0]
	chosen.session.host.AvailableSlots--
	host := chosen.session.host
	_ = s.store.UpdateHost(context.Background(), &host)
	_ = s.store.ReplaceWarmPoolsForHost(context.Background(), host.Region, host.ID, warmPoolsForHost(host, s.now()))
	_ = s.persistRegionLocked()
	return chosen.id, chosen.session, false, nil
}

func (s *Service) releaseHostSlot(hostID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.hosts[hostID]; ok {
		if session.host.State == domain.HostStateActive {
			session.host.AvailableSlots++
		}
		host := session.host
		_ = s.store.UpdateHost(context.Background(), &host)
		_ = s.persistRegionLocked()
	}
}

func (s *Service) restoreHostReservation(hostID, functionVersionID string, startMode domain.StartMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.hosts[hostID]; ok {
		session.host.AvailableSlots++
		switch startMode {
		case domain.StartModeFunctionWarm:
			session.host.FunctionWarm[functionVersionID]++
		case domain.StartModeBlankWarm:
			session.host.BlankWarm++
		}
		host := session.host
		_ = s.store.UpdateHost(context.Background(), &host)
		_ = s.store.ReplaceWarmPoolsForHost(context.Background(), host.Region, host.ID, warmPoolsForHost(host, s.now()))
		_ = s.persistRegionLocked()
	}
}

func (s *Service) restorePreparationReservation(hostID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.hosts[hostID]; ok {
		session.host.AvailableSlots++
		host := session.host
		_ = s.store.UpdateHost(context.Background(), &host)
		_ = s.store.ReplaceWarmPoolsForHost(context.Background(), host.Region, host.ID, warmPoolsForHost(host, s.now()))
		_ = s.persistRegionLocked()
	}
}

func (s *Service) markHostDown(hostID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.hosts[hostID]
	if !ok {
		return
	}
	session.host.State = domain.HostStateDown
	session.host.AvailableSlots = 0
	session.host.BlankWarm = 0
	session.host.FunctionWarm = map[string]int{}
	host := session.host
	_ = s.store.UpdateHost(context.Background(), &host)
	_ = s.store.ReplaceWarmPoolsForHost(context.Background(), host.Region, host.ID, nil)
	delete(s.hosts, hostID)
	_ = s.persistRegionLocked()
}

func (s *Service) reapHostLoop(ctx context.Context) {
	if s.staleHostAfter <= 0 || s.reapInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.reapStaleHosts(ctx)
		}
	}
}

func (s *Service) reapStaleHosts(ctx context.Context) error {
	if s.staleHostAfter <= 0 {
		return nil
	}
	cutoff := s.now().Add(-s.staleHostAfter)
	hosts, err := s.store.ListHostsByRegion(ctx, s.region)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		if host.State == domain.HostStateDown {
			continue
		}
		if host.LastHeartbeat.After(cutoff) {
			continue
		}
		if err := s.markStoredHostDown(ctx, host, cutoff); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) markStoredHostDown(ctx context.Context, host domain.Host, cutoff time.Time) error {
	s.mu.Lock()
	if session, ok := s.hosts[host.ID]; ok {
		if session.host.LastHeartbeat.After(cutoff) {
			s.mu.Unlock()
			return nil
		}
		session.host.State = domain.HostStateDown
		session.host.AvailableSlots = 0
		session.host.BlankWarm = 0
		session.host.FunctionWarm = map[string]int{}
		host = session.host
		delete(s.hosts, host.ID)
	} else {
		host.State = domain.HostStateDown
		host.AvailableSlots = 0
		host.BlankWarm = 0
		host.FunctionWarm = map[string]int{}
	}
	s.mu.Unlock()
	if err := s.store.UpdateHost(ctx, &host); err != nil {
		return err
	}
	if err := s.store.ReplaceWarmPoolsForHost(ctx, host.Region, host.ID, nil); err != nil {
		return err
	}
	return s.persistRegion()
}

func warmMap(metrics []*regionv1.WarmPoolMetric) map[string]int {
	out := make(map[string]int, len(metrics))
	for _, metric := range metrics {
		out[metric.FunctionVersionId] = int(metric.Available)
	}
	return out
}

func startTimeOr(fallback, startedAt time.Time) time.Time {
	if startedAt.IsZero() {
		return fallback
	}
	return startedAt
}

func classifyStartMode(host domain.Host, functionVersionID string) domain.StartMode {
	if host.FunctionWarm[functionVersionID] > 0 {
		return domain.StartModeFunctionWarm
	}
	if host.BlankWarm > 0 {
		return domain.StartModeBlankWarm
	}
	return domain.StartModeCold
}

func warmPoolsForHost(host domain.Host, updatedAt time.Time) []domain.WarmPool {
	if host.State != domain.HostStateActive {
		return nil
	}
	pools := make([]domain.WarmPool, 0, len(host.FunctionWarm)+1)
	if host.BlankWarm > 0 {
		pools = append(pools, domain.WarmPool{
			Region:    host.Region,
			HostID:    host.ID,
			BlankWarm: host.BlankWarm,
			UpdatedAt: updatedAt,
		})
	}
	functionIDs := make([]string, 0, len(host.FunctionWarm))
	for functionID, count := range host.FunctionWarm {
		if count > 0 {
			functionIDs = append(functionIDs, functionID)
		}
	}
	sort.Strings(functionIDs)
	for _, functionID := range functionIDs {
		pools = append(pools, domain.WarmPool{
			Region:            host.Region,
			HostID:            host.ID,
			FunctionVersionID: functionID,
			FunctionWarm:      host.FunctionWarm[functionID],
			UpdatedAt:         updatedAt,
		})
	}
	return pools
}

func (s *Service) recordCost(ctx context.Context, attempt *domain.Attempt, job *domain.ExecutionJob) error {
	if job.Result == nil {
		return nil
	}
	version, err := s.store.GetFunctionVersion(ctx, job.FunctionVersionID)
	if err != nil {
		return err
	}
	project, err := s.store.GetProject(ctx, job.ProjectID)
	if err != nil {
		return err
	}
	startedAt := startTimeOr(job.Result.FinishedAt, attempt.StartedAt)
	runtime := job.Result.FinishedAt.Sub(startedAt)
	if runtime < 0 {
		runtime = 0
	}
	runtimeMs := runtime.Milliseconds()
	if runtimeMs == 0 && !job.Result.FinishedAt.IsZero() && !startedAt.IsZero() && !job.Result.FinishedAt.Before(startedAt) {
		runtimeMs = 1
	}
	warmMs := int64(0)
	if attempt.StartMode == domain.StartModeBlankWarm || attempt.StartMode == domain.StartModeFunctionWarm {
		warmMs = runtimeMs
	}
	record := &domain.CostRecord{
		ID:              attempt.ID,
		TenantID:        project.TenantID,
		ProjectID:       project.ID,
		JobID:           job.ID,
		AttemptID:       attempt.ID,
		HostID:          job.Result.HostID,
		Region:          job.Result.Region,
		StartMode:       attempt.StartMode,
		CPUMs:           runtimeMs,
		MemoryMBMs:      int64(version.MemoryMB) * runtimeMs,
		WarmInstanceMs:  warmMs,
		DataEgressBytes: int64(len(job.Result.Logs) + len(job.Result.Output)),
		CreatedAt:       s.now(),
	}
	return s.store.PutCostRecord(ctx, record)
}

func (s *Service) persistRegion() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistRegionLocked()
}

func (s *Service) persistRegionLocked() error {
	stats := domain.RegionStats{}
	lastHeartbeat := time.Time{}
	connectedActiveHosts := 0
	for _, session := range s.hosts {
		if session.host.Region != s.region {
			continue
		}
		if session.host.State == domain.HostStateActive {
			connectedActiveHosts++
		}
		if session.host.State == domain.HostStateActive && session.host.AvailableSlots > 0 {
			stats.AvailableHosts++
		}
		if session.host.State == domain.HostStateActive {
			stats.BlankWarm += session.host.BlankWarm
			for _, count := range session.host.FunctionWarm {
				stats.FunctionWarm += count
			}
		}
		if session.host.LastHeartbeat.After(lastHeartbeat) {
			lastHeartbeat = session.host.LastHeartbeat
		}
	}
	if lastHeartbeat.IsZero() {
		lastHeartbeat = s.now()
	}
	state := "active"
	lastError := ""
	if connectedActiveHosts == 0 {
		state = "degraded"
		lastError = "no active hosts connected"
	}
	return s.store.PutRegion(context.Background(), &domain.Region{
		Name:            s.region,
		State:           state,
		AvailableHosts:  stats.AvailableHosts,
		BlankWarm:       stats.BlankWarm,
		FunctionWarm:    stats.FunctionWarm,
		LastHeartbeatAt: lastHeartbeat,
		LastError:       lastError,
	})
}
