package localnode

import (
	"context"
	"sync"
	"time"

	"github.com/theg1239/lecrev/internal/firecracker"
	"github.com/theg1239/lecrev/internal/runtime/nodeexec"
	"github.com/theg1239/lecrev/internal/timetrace"
)

type Driver struct {
	mu           sync.Mutex
	blankWarm    int
	functionWarm map[string]int
}

func New() *Driver {
	return &Driver{
		blankWarm:    1,
		functionWarm: map[string]int{},
	}
}

func (d *Driver) Name() string {
	return "local-node"
}

func (d *Driver) WarmInventory() firecracker.WarmInventory {
	d.mu.Lock()
	defer d.mu.Unlock()

	functionWarm := make(map[string]int, len(d.functionWarm))
	for functionID, count := range d.functionWarm {
		functionWarm[functionID] = count
	}
	return firecracker.WarmInventory{
		BlankWarm:    d.blankWarm,
		FunctionWarm: functionWarm,
	}
}

func (d *Driver) WarmInventoryForSlots(freeSlots int) firecracker.WarmInventory {
	base := d.WarmInventory()
	if freeSlots <= 0 {
		base.BlankWarm = 0
		for functionID := range base.FunctionWarm {
			base.FunctionWarm[functionID] = 0
		}
		return base
	}
	if base.BlankWarm > 0 {
		base.BlankWarm = freeSlots
	}
	for functionID, count := range base.FunctionWarm {
		if count > 0 {
			base.FunctionWarm[functionID] = freeSlots
		}
	}
	return base
}

func (d *Driver) EnsureBlankWarm(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.blankWarm == 0 {
		d.blankWarm = 1
	}
	return nil
}

func (d *Driver) PrepareFunctionWarm(_ context.Context, req firecracker.ExecuteRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.functionWarm[req.FunctionID] = 1
	return nil
}

func (d *Driver) Execute(ctx context.Context, req firecracker.ExecuteRequest) (*firecracker.ExecuteResult, error) {
	trace := timetrace.New()
	executeStarted := time.Now()
	result, err := nodeexec.ExecuteBundle(ctx, nodeexec.Request{
		AttemptID:      req.AttemptID,
		JobID:          req.JobID,
		FunctionID:     req.FunctionID,
		Entrypoint:     req.Entrypoint,
		ArtifactBundle: req.ArtifactBundle,
		Payload:        req.Payload,
		Env:            req.Env,
		Timeout:        req.Timeout,
		Region:         req.Region,
		HostID:         req.HostID,
		NodeBinary:     "node",
	})
	trace.Step("localnode_execute_bundle", executeStarted)
	if err != nil {
		if result == nil {
			return nil, err
		}
		return &firecracker.ExecuteResult{
			ExitCode:         result.ExitCode,
			Logs:             result.Logs,
			Output:           result.Output,
			PlatformTrace:    timetrace.Combine(trace.String(), result.PlatformTrace),
			SnapshotEligible: false,
			StartedAt:        result.StartedAt,
			FinishedAt:       result.FinishedAt,
		}, err
	}

	return &firecracker.ExecuteResult{
		ExitCode:         result.ExitCode,
		Logs:             result.Logs,
		Output:           result.Output,
		PlatformTrace:    timetrace.Combine(trace.String(), result.PlatformTrace),
		SnapshotEligible: false,
		StartedAt:        result.StartedAt,
		FinishedAt:       result.FinishedAt,
	}, nil
}
