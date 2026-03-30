package artifact

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrNotFound = errors.New("artifact not found")

type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

type MemoryStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string][]byte)}
}

func (m *MemoryStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := append([]byte(nil), data...)
	m.data[key] = cp
	return nil
}

func (m *MemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	return append([]byte(nil), data...), nil
}
