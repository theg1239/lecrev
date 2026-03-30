package build

import (
	"context"
	"fmt"
	"time"

	"github.com/theg1239/lecrev/internal/artifact"
)

type RegionReplicator struct {
	stores map[string]artifact.Store
	now    func() time.Time
}

func NewRegionReplicator(stores map[string]artifact.Store) *RegionReplicator {
	return &RegionReplicator{
		stores: stores,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

func (r *RegionReplicator) Replicate(ctx context.Context, req ArtifactReplicationRequest) (map[string]time.Time, error) {
	available := make(map[string]time.Time, len(req.Regions))
	for _, region := range req.Regions {
		store, ok := r.stores[region]
		if !ok {
			return nil, fmt.Errorf("artifact replication target %s is not configured", region)
		}
		if err := store.Put(ctx, req.BundleKey, req.Bundle); err != nil {
			return nil, fmt.Errorf("replicate bundle to %s: %w", region, err)
		}
		if err := store.Put(ctx, req.StartupKey, req.Startup); err != nil {
			return nil, fmt.Errorf("replicate startup metadata to %s: %w", region, err)
		}
		available[region] = r.now()
	}
	return available, nil
}
