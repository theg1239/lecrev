package build

import (
	"context"
	"sync"
)

type MemoryBus struct {
	mu       sync.Mutex
	buffer   int
	channels map[string]chan BuildAssignment
}

func NewMemoryBus(buffer int) *MemoryBus {
	if buffer <= 0 {
		buffer = 64
	}
	return &MemoryBus{
		buffer:   buffer,
		channels: make(map[string]chan BuildAssignment),
	}
}

func (b *MemoryBus) PublishBuild(ctx context.Context, region string, assignment BuildAssignment) error {
	ch := b.channel(region)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- assignment:
		return nil
	}
}

func (b *MemoryBus) ConsumeBuild(ctx context.Context, region, _ string, handler BuildHandler) error {
	ch := b.channel(region)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case assignment, ok := <-ch:
			if !ok {
				return nil
			}
			if err := handler(ctx, assignment); err != nil {
				return err
			}
		}
	}
}

func (b *MemoryBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for region, ch := range b.channels {
		close(ch)
		delete(b.channels, region)
	}
	return nil
}

func (b *MemoryBus) channel(region string) chan BuildAssignment {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.channels[region]
	if !ok {
		ch = make(chan BuildAssignment, b.buffer)
		b.channels[region] = ch
	}
	return ch
}
