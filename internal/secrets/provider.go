package secrets

import (
	"context"
	"fmt"
	"sync"
)

type Provider interface {
	Resolve(ctx context.Context, refs []string) (map[string]string, error)
}

type MemoryProvider struct {
	mu    sync.RWMutex
	items map[string]string
}

func NewMemoryProvider(items map[string]string) *MemoryProvider {
	cp := make(map[string]string, len(items))
	for k, v := range items {
		cp[k] = v
	}
	return &MemoryProvider{items: cp}
}

func (p *MemoryProvider) Resolve(_ context.Context, refs []string) (map[string]string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		value, ok := p.items[ref]
		if !ok {
			return nil, fmt.Errorf("secret ref %q not found", ref)
		}
		out[ref] = value
	}
	return out, nil
}
