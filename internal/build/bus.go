package build

import "context"

type BuildAssignment struct {
	BuildJobID string `json:"buildJobId"`
}

type BuildHandler func(context.Context, BuildAssignment) error

type BuildBus interface {
	PublishBuild(ctx context.Context, region string, assignment BuildAssignment) error
	ConsumeBuild(ctx context.Context, region, consumer string, handler BuildHandler) error
	Close() error
}
