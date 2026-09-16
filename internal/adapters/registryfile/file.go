// Package registryfile provides a JSON file cache of the registry and a
// file-backed registry used by the dev fleet and tests.
package registryfile

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
)

// Cache implements ports.RegistryCache.
type Cache struct {
	Path string
}

// Load implements ports.RegistryCache; a missing file yields an empty list.
func (c Cache) Load() ([]domain.NodeRecord, error) {
	if c.Path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(c.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []domain.NodeRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// Save implements ports.RegistryCache with an atomic rename.
func (c Cache) Save(recs []domain.NodeRecord) error {
	if c.Path == "" {
		return nil
	}
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o750); err != nil {
		return err
	}
	tmp := c.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.Path)
}

// Registry implements ports.Registry on a shared JSON file (dev fleet) or in
// memory when Path is empty (tests).
type Registry struct {
	Path string
	mu   sync.Mutex
	mem  map[string]domain.NodeRecord
	// Round is what List reports; tests bump it to simulate chain progress.
	Round uint64
}

func (r *Registry) load() (map[string]domain.NodeRecord, error) {
	if r.Path == "" {
		if r.mem == nil {
			r.mem = map[string]domain.NodeRecord{}
		}
		return r.mem, nil
	}
	recs, err := Cache{Path: r.Path}.Load()
	if err != nil {
		return nil, err
	}
	m := make(map[string]domain.NodeRecord, len(recs))
	for _, rec := range recs {
		m[rec.ID] = rec
	}
	return m, nil
}

func (r *Registry) store(m map[string]domain.NodeRecord) error {
	if r.Path == "" {
		r.mem = m
		return nil
	}
	return Cache{Path: r.Path}.Save(sorted(m))
}

func sorted(m map[string]domain.NodeRecord) []domain.NodeRecord {
	out := make([]domain.NodeRecord, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// List implements ports.Registry.
func (r *Registry) List(ctx context.Context) ([]domain.NodeRecord, uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.load()
	if err != nil {
		return nil, 0, err
	}
	return sorted(m), r.Round, nil
}

// Put implements ports.Registry.
func (r *Registry) Put(ctx context.Context, rec domain.NodeRecord) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.load()
	if err != nil {
		return err
	}
	m[rec.ID] = rec
	r.Round++
	return r.store(m)
}

// Delete implements ports.Registry.
func (r *Registry) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.load()
	if err != nil {
		return err
	}
	delete(m, id)
	r.Round++
	return r.store(m)
}

// Static is a read-only registry from configuration.
type Static struct {
	Records []domain.NodeRecord
}

// List implements ports.Registry.
func (s Static) List(ctx context.Context) ([]domain.NodeRecord, uint64, error) {
	return append([]domain.NodeRecord(nil), s.Records...), 0, nil
}

// Put implements ports.Registry; static registries cannot be written.
func (s Static) Put(ctx context.Context, rec domain.NodeRecord) error {
	return errors.New("static registry is read-only")
}

// Delete implements ports.Registry.
func (s Static) Delete(ctx context.Context, id string) error {
	return errors.New("static registry is read-only")
}
