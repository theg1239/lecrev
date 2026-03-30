package dispatch

import (
	"context"
	"sync"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
)

type MemoryBus struct {
	mu     sync.Mutex
	buffer int
	queues map[string]chan domain.Assignment
	closed bool
}

func NewMemoryBus(buffer int) *MemoryBus {
	if buffer <= 0 {
		buffer = 256
	}
	return &MemoryBus{
		buffer: buffer,
		queues: make(map[string]chan domain.Assignment),
	}
}

func (b *MemoryBus) PublishExecution(ctx context.Context, region string, assignment domain.Assignment) error {
	queue, err := b.queue(region)
	if err != nil {
		return err
	}
	select {
	case queue <- assignment:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *MemoryBus) ConsumeExecution(ctx context.Context, region, _ string, handler Handler) error {
	queue, err := b.queue(region)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case assignment, ok := <-queue:
			if !ok {
				return nil
			}
			if err := handler(ctx, assignment); err != nil {
				go func(requeue domain.Assignment) {
					timer := time.NewTimer(500 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return
					case <-timer.C:
					}
					_ = b.PublishExecution(context.Background(), region, requeue)
				}(assignment)
			}
		}
	}
}

func (b *MemoryBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for region, queue := range b.queues {
		close(queue)
		delete(b.queues, region)
	}
	return nil
}

func (b *MemoryBus) queue(region string) (chan domain.Assignment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, context.Canceled
	}
	queue, ok := b.queues[region]
	if !ok {
		queue = make(chan domain.Assignment, b.buffer)
		b.queues[region] = queue
	}
	return queue, nil
}
