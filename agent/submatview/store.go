package submatview

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Store struct {
	lock  sync.RWMutex
	byKey map[string]*Materializer
}

func NewStore() *Store {
	return &Store{byKey: make(map[string]*Materializer)}
}

func (s *Store) Fetch(ctx context.Context, opts FetchOptions) (FetchResult, error) {
	s.lock.RLock()
	mat, ok := s.byKey[opts.Key]
	s.lock.RUnlock()

	if !ok {
		s.lock.Lock()
		mat, ok := s.byKey[opts.Key]
		if !ok {
			mat = opts.NewMaterializer()
			s.byKey[opts.Key] = mat
		}
		s.lock.Unlock()
	}

	// TODO: track last used for expiration
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	return mat.Fetch(ctx, opts)
}

// makeEntryKey matches agent/cache.makeEntryKey
func makeEntryKey(t, dc, token, key string) string {
	return fmt.Sprintf("%s/%s/%s/%s", t, dc, token, key)
}

type FetchOptions struct {
	// TODO: needs to use makeEntryKey
	Key string

	// MinIndex is the index previously seen by the caller. If MinIndex>0 Fetch
	// will not return until the index is >MinIndex, or Timeout is hit.
	MinIndex uint64

	// TODO: maybe remove and use a context deadline.
	Timeout time.Duration

	// NewMaterializer returns a new Materializer to be used if the store does
	// not have one already running for the given key.
	NewMaterializer func() *Materializer
}

type FetchResult struct {
	// Value is the result of the fetch.
	Value interface{}

	// Index is the corresponding index value for this data.
	Index uint64
}
