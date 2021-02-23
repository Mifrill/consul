package submatview

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/lib/ttlcache"

	"github.com/hashicorp/consul/agent/cache"
	"github.com/hashicorp/consul/proto/pbcommon"
	"github.com/hashicorp/consul/proto/pbservice"
	"github.com/hashicorp/consul/proto/pbsubscribe"
)

func TestStore_Get(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewStore(hclog.New(nil))
	go store.Run(ctx)

	req := &fakeRequest{
		client: NewTestStreamingClient(pbcommon.DefaultEnterpriseMeta.Namespace),
	}
	req.client.QueueEvents(
		newEndOfSnapshotEvent(2),
		newEventServiceHealthRegister(10, 1, "srv1"),
		newEventServiceHealthRegister(22, 2, "srv1"))

	runStep(t, "from empty store, starts materializer", func(t *testing.T) {
		result, err := store.Get(ctx, req)
		require.NoError(t, err)
		require.Equal(t, uint64(22), result.Index)

		r, ok := result.Value.(fakeResult)
		require.True(t, ok)
		require.Len(t, r.srvs, 2)
		require.Equal(t, uint64(22), r.index)

		store.lock.Lock()
		defer store.lock.Unlock()
		require.Len(t, store.byKey, 1)
		e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		require.Equal(t, 0, e.expiry.Index())
		require.Equal(t, 0, e.requests)

		require.Equal(t, store.expiryHeap.Next().Entry, e.expiry)
	})

	runStep(t, "with an index that already exists in the view", func(t *testing.T) {
		req.index = 21
		result, err := store.Get(ctx, req)
		require.NoError(t, err)
		require.Equal(t, uint64(22), result.Index)

		r, ok := result.Value.(fakeResult)
		require.True(t, ok)
		require.Len(t, r.srvs, 2)
		require.Equal(t, uint64(22), r.index)

		store.lock.Lock()
		defer store.lock.Unlock()
		require.Len(t, store.byKey, 1)
		e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		require.Equal(t, 0, e.expiry.Index())
		require.Equal(t, 0, e.requests)

		require.Equal(t, store.expiryHeap.Next().Entry, e.expiry)
	})

	runStep(t, "blocks with an index that is not yet in the view", func(t *testing.T) {
		req.index = 23

		chResult := make(chan resultOrError, 1)
		go func() {
			result, err := store.Get(ctx, req)
			chResult <- resultOrError{Result: result, Err: err}
		}()

		select {
		case <-chResult:
			t.Fatalf("expected Get to block")
		case <-time.After(50 * time.Millisecond):
		}

		store.lock.Lock()
		e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		store.lock.Unlock()
		require.Equal(t, 1, e.requests)

		req.client.QueueEvents(newEventServiceHealthRegister(24, 1, "srv1"))

		var getResult resultOrError
		select {
		case getResult = <-chResult:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("expected Get to unblock when new events are received")
		}

		require.NoError(t, getResult.Err)
		require.Equal(t, uint64(24), getResult.Result.Index)

		r, ok := getResult.Result.Value.(fakeResult)
		require.True(t, ok)
		require.Len(t, r.srvs, 2)
		require.Equal(t, uint64(24), r.index)

		store.lock.Lock()
		defer store.lock.Unlock()
		require.Len(t, store.byKey, 1)
		e = store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		require.Equal(t, 0, e.expiry.Index())
		require.Equal(t, 0, e.requests)

		require.Equal(t, store.expiryHeap.Next().Entry, e.expiry)
	})
}

type resultOrError struct {
	Result Result
	Err    error
}

type fakeRequest struct {
	index  uint64
	client *TestStreamingClient
}

func (r *fakeRequest) CacheInfo() cache.RequestInfo {
	return cache.RequestInfo{
		Key:        "key",
		Token:      "abcd",
		Datacenter: "dc1",
		Timeout:    4 * time.Second,
		MinIndex:   r.index,
	}
}

func (r *fakeRequest) NewMaterializer() *Materializer {
	return NewMaterializer(Deps{
		View:   &fakeView{srvs: make(map[string]*pbservice.CheckServiceNode)},
		Client: r.client,
		Logger: hclog.New(nil),
		Request: func(index uint64) pbsubscribe.SubscribeRequest {
			req := pbsubscribe.SubscribeRequest{
				Topic:      pbsubscribe.Topic_ServiceHealth,
				Key:        "key",
				Token:      "abcd",
				Datacenter: "dc1",
				Index:      index,
				Namespace:  pbcommon.DefaultEnterpriseMeta.Namespace,
			}
			return req
		},
	})
}

func (r *fakeRequest) Type() string {
	return fmt.Sprintf("%T", r)
}

type fakeView struct {
	srvs map[string]*pbservice.CheckServiceNode
}

func (f *fakeView) Update(events []*pbsubscribe.Event) error {
	for _, event := range events {
		serviceHealth := event.GetServiceHealth()
		if serviceHealth == nil {
			return fmt.Errorf("unexpected event type for service health view: %T",
				event.GetPayload())
		}

		id := serviceHealth.CheckServiceNode.UniqueID()
		switch serviceHealth.Op {
		case pbsubscribe.CatalogOp_Register:
			f.srvs[id] = serviceHealth.CheckServiceNode

		case pbsubscribe.CatalogOp_Deregister:
			delete(f.srvs, id)
		}
	}
	return nil
}

func (f *fakeView) Result(index uint64) interface{} {
	srvs := make([]*pbservice.CheckServiceNode, 0, len(f.srvs))
	for _, srv := range f.srvs {
		srvs = append(srvs, srv)
	}
	return fakeResult{srvs: srvs, index: index}
}

type fakeResult struct {
	srvs  []*pbservice.CheckServiceNode
	index uint64
}

func (f *fakeView) Reset() {
	f.srvs = make(map[string]*pbservice.CheckServiceNode)
}

func TestStore_Notify(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewStore(hclog.New(nil))
	go store.Run(ctx)

	req := &fakeRequest{
		client: NewTestStreamingClient(pbcommon.DefaultEnterpriseMeta.Namespace),
	}
	req.client.QueueEvents(
		newEndOfSnapshotEvent(2),
		newEventServiceHealthRegister(10, 1, "srv1"),
		newEventServiceHealthRegister(22, 2, "srv1"))

	cID := "correlate"
	ch := make(chan cache.UpdateEvent)

	err := store.Notify(ctx, req, cID, ch)
	require.NoError(t, err)

	runStep(t, "from empty store, starts materializer", func(t *testing.T) {
		store.lock.Lock()
		defer store.lock.Unlock()
		require.Len(t, store.byKey, 1)
		e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		require.Equal(t, ttlcache.NotIndexed, e.expiry.Index())
		require.Equal(t, 1, e.requests)
	})

	// TODO: Notify with no existing entry
	// TODO: Notify with Get
	// TODO: Notify multiple times same key
	// TODO: Notify no update if index is not past MinIndex.
}

// TODO: TestStore_GetWithNotify

func runStep(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.FailNow()
	}
}
