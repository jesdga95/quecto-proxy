package keys

import (
	"sync"

	"quecto/internal/store"
)

// Registry keeps every key in memory so the proxy path never touches disk.
type Registry struct {
	st *store.Store

	mu     sync.RWMutex
	byHash map[[32]byte]*store.APIKey
	byID   map[int64]*store.APIKey
}

func NewRegistry(st *store.Store) (*Registry, error) {
	r := &Registry{st: st}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload rebuilds the cache from the database. Cheap enough to call on every write.
func (r *Registry) Reload() error {
	rows, err := r.st.ListKeys()
	if err != nil {
		return err
	}
	byHash := make(map[[32]byte]*store.APIKey, len(rows))
	byID := make(map[int64]*store.APIKey, len(rows))
	for _, k := range rows {
		var h [32]byte
		copy(h[:], k.KeyHash)
		byHash[h] = k
		byID[k.ID] = k
	}

	r.mu.Lock()
	r.byHash, r.byID = byHash, byID
	r.mu.Unlock()
	return nil
}

func (r *Registry) Lookup(hash [32]byte) (*store.APIKey, bool) {
	r.mu.RLock()
	k, ok := r.byHash[hash]
	r.mu.RUnlock()
	return k, ok
}

func (r *Registry) ByID(id int64) (*store.APIKey, bool) {
	r.mu.RLock()
	k, ok := r.byID[id]
	r.mu.RUnlock()
	return k, ok
}

func (r *Registry) All() []*store.APIKey {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*store.APIKey, 0, len(r.byID))
	for _, k := range r.byID {
		out = append(out, k)
	}
	return out
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}
