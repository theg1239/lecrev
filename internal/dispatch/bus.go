package dispatch

import (
	"context"

	"github.com/theg1239/lecrev/internal/domain"
)

type Handler func(context.Context, domain.Assignment) error

type ExecutionBus interface {
	PublishExecution(ctx context.Context, region string, assignment domain.Assignment) error
	ConsumeExecution(ctx context.Context, region, consumer string, handler Handler) error
	Close() error
}
