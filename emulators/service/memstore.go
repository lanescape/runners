package service

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lanescape/runners/internal/store"
)

// memStore is store.Objects in memory, with Cloud Storage's generations,
// for the emulator's own records.
type memStore struct {
	mu   sync.Mutex
	next int64
	objs map[string]memObject
}

type memObject struct {
	data []byte
	gen  int64
	at   time.Time
}

func newMemStore() *memStore { return &memStore{objs: map[string]memObject{}} }

func (m *memStore) Get(_ context.Context, name string) ([]byte, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[name]
	if !ok {
		return nil, 0, store.ErrNotFound
	}
	return slices.Clone(o.data), o.gen, nil
}

func (m *memStore) put(name string, data []byte) int64 {
	m.next++
	m.objs[name] = memObject{data: slices.Clone(data), gen: m.next, at: time.Now()}
	return m.next
}

func (m *memStore) Create(_ context.Context, name string, data []byte) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[name]; ok {
		return 0, store.ErrPrecondition
	}
	return m.put(name, data), nil
}

func (m *memStore) Replace(_ context.Context, name string, data []byte, gen int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objs[name]; !ok || o.gen != gen {
		return 0, store.ErrPrecondition
	}
	return m.put(name, data), nil
}

func (m *memStore) Delete(_ context.Context, name string, gen int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[name]
	if !ok {
		return store.ErrNotFound
	}
	if o.gen != gen {
		return store.ErrPrecondition
	}
	delete(m.objs, name)
	return nil
}

func (m *memStore) List(_ context.Context, prefix string) ([]store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Object
	for n, o := range m.objs {
		if strings.HasPrefix(n, prefix) {
			out = append(out, store.Object{Name: n, Generation: o.gen, Updated: o.at})
		}
	}
	slices.SortFunc(out, func(a, b store.Object) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
