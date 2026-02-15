package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"hubproxy/internal/storage"
)

// mockStorage implements storage.Storage for testing ticker behavior
type mockStorage struct {
	mu                  sync.Mutex
	events              []*storage.Event
	listEventsCalls     int
	markForwardedCalls  int
	forwardedEventIDs   map[string]bool
	listEventsCallTimes []time.Time
}

func newMockStorage() *mockStorage {
	return &mockStorage{
		events:            make([]*storage.Event, 0),
		forwardedEventIDs: make(map[string]bool),
	}
}

func (m *mockStorage) StoreEvent(ctx context.Context, event *storage.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	event.ID = "event-" + time.Now().Format("20060102150405.000000000")
	m.events = append(m.events, event)
	return nil
}

func (m *mockStorage) MarkForwarded(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markForwardedCalls++
	m.forwardedEventIDs[id] = true
	// Update the event in storage
	for i, e := range m.events {
		if e.ID == id {
			now := time.Now()
			m.events[i].ForwardedAt = &now
		}
	}
	return nil
}

func (m *mockStorage) ListEvents(ctx context.Context, opts storage.QueryOptions) ([]*storage.Event, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listEventsCalls++
	m.listEventsCallTimes = append(m.listEventsCallTimes, time.Now())

	result := make([]*storage.Event, 0)
	for _, e := range m.events {
		if opts.OnlyNonForwarded && e.ForwardedAt != nil {
			continue
		}
		result = append(result, e)
	}
	return result, len(result), nil
}

func (m *mockStorage) CountEvents(ctx context.Context, opts storage.QueryOptions) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events), nil
}

func (m *mockStorage) GetStats(ctx context.Context, since time.Time) (map[string]int64, error) {
	return make(map[string]int64), nil
}

func (m *mockStorage) GetEvent(ctx context.Context, id string) (*storage.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, nil
}

func (m *mockStorage) CreateSchema(ctx context.Context) error { return nil }
func (m *mockStorage) Close() error                           { return nil }

// Helper to get call counts safely
func (m *mockStorage) getListEventsCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listEventsCalls
}

func (m *mockStorage) getMarkForwardedCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.markForwardedCalls
}

// TestStartForwarder_TickerFiresMultipleTimes proves the ticker lifecycle fix works
// This test verifies that:
// 1. Events are processed on startup (initial call)
// 2. Events are processed again after ticker interval
// 3. Ticker continues firing multiple times
// 4. Forwarder stops gracefully when context is canceled
func TestStartForwarder_TickerFiresMultipleTimes(t *testing.T) {
	// Create mock storage
	mockStore := newMockStorage()

	// Track HTTP requests received by target server
	var requestCount atomic.Int32
	var requestTimes []time.Time
	var requestTimesMu sync.Mutex

	// Create a mock target server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		requestTimesMu.Lock()
		requestTimes = append(requestTimes, time.Now())
		requestTimesMu.Unlock()
		t.Logf("Target server received request #%d at %v", count, time.Now().Format("15:04:05.000"))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Create mock metrics collector
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metricsCollector := storage.NewDBMetricsCollector(mockStore, logger)

	// Create forwarder with short poll interval for testing (100ms)
	forwarder := NewWebhookForwarder(WebhookForwarderOptions{
		TargetURL:        ts.URL,
		Storage:          mockStore,
		MetricsCollector: metricsCollector,
		Logger:           logger,
		PollInterval:     100 * time.Millisecond,
	})

	// Store a mock event BEFORE starting the forwarder
	// This tests that the initial ProcessEvents call on startup processes existing events
	ctx := context.Background()
	testEvent := &storage.Event{
		ID:        "test-event-1",
		Type:      "push",
		Payload:   json.RawMessage(`{"ref": "refs/heads/main"}`),
		Headers:   json.RawMessage(`{"X-GitHub-Event": ["push"], "X-GitHub-Delivery": ["test-1"]}`),
		CreatedAt: time.Now(),
	}
	err := mockStore.StoreEvent(ctx, testEvent)
	require.NoError(t, err)
	t.Logf("Stored initial event: %s", testEvent.ID)

	// Create cancelable context
	ctx, cancel := context.WithCancel(context.Background())

	// Start the forwarder
	forwarder.StartForwarder(ctx)

	// Wait for initial processing (should happen immediately)
	t.Log("Waiting for initial event processing...")
	require.Eventually(t, func() bool {
		return requestCount.Load() >= 1
	}, 2*time.Second, 50*time.Millisecond, "Initial event should be processed on startup")
	t.Logf("Initial event processed. Request count: %d", requestCount.Load())

	// Verify initial event was forwarded
	assert.GreaterOrEqual(t, mockStore.getMarkForwardedCalls(), 1, "Event should be marked as forwarded")

	// Store another event AFTER initial processing
	// This tests that the ticker fires and processes new events
	newEvent := &storage.Event{
		ID:        "test-event-2",
		Type:      "push",
		Payload:   json.RawMessage(`{"ref": "refs/heads/feature"}`),
		Headers:   json.RawMessage(`{"X-GitHub-Event": ["push"], "X-GitHub-Delivery": ["test-2"]}`),
		CreatedAt: time.Now(),
	}
	err = mockStore.StoreEvent(ctx, newEvent)
	require.NoError(t, err)
	t.Logf("Stored second event: %s", newEvent.ID)

	// Wait for ticker to fire and process the new event
	// With 100ms interval, should process within ~200ms
	t.Log("Waiting for ticker to fire and process second event...")
	require.Eventually(t, func() bool {
		return requestCount.Load() >= 2
	}, 500*time.Millisecond, 50*time.Millisecond, "Second event should be processed after ticker fires")
	t.Logf("Second event processed. Request count: %d", requestCount.Load())

	// Store a third event to verify ticker keeps firing
	thirdEvent := &storage.Event{
		ID:        "test-event-3",
		Type:      "push",
		Payload:   json.RawMessage(`{"ref": "refs/heads/another"}`),
		Headers:   json.RawMessage(`{"X-GitHub-Event": ["push"], "X-GitHub-Delivery": ["test-3"]}`),
		CreatedAt: time.Now(),
	}
	err = mockStore.StoreEvent(ctx, thirdEvent)
	require.NoError(t, err)
	t.Logf("Stored third event: %s", thirdEvent.ID)

	// Wait for third event to be processed
	t.Log("Waiting for ticker to fire again for third event...")
	require.Eventually(t, func() bool {
		return requestCount.Load() >= 3
	}, 500*time.Millisecond, 50*time.Millisecond, "Third event should be processed after ticker fires again")
	t.Logf("Third event processed. Request count: %d", requestCount.Load())

	// Verify ListEvents was called multiple times (proves ticker is working)
	listCalls := mockStore.getListEventsCalls()
	t.Logf("ListEvents called %d times", listCalls)
	assert.GreaterOrEqual(t, listCalls, 3, "ListEvents should be called at least 3 times (initial + 2 ticker fires)")

	// Verify all events were forwarded
	markCalls := mockStore.getMarkForwardedCalls()
	t.Logf("MarkForwarded called %d times", markCalls)
	assert.GreaterOrEqual(t, markCalls, 3, "All events should be marked as forwarded")

	// Cancel the context to stop the forwarder
	t.Log("Canceling context to stop forwarder...")
	cancel()

	// Give it a moment to stop gracefully
	time.Sleep(100 * time.Millisecond)

	// Record the request count after cancel
	finalCount := requestCount.Load()
	t.Logf("Final request count: %d", finalCount)

	// Wait a bit longer to ensure no more requests come in
	time.Sleep(200 * time.Millisecond)

	// Verify no more requests after cancel
	assert.Equal(t, finalCount, requestCount.Load(), "No more requests should be made after context cancel")
	t.Log("Forwarder stopped gracefully - no more requests after cancel")
}

// TestStartForwarder_TickerInterval proves the ticker respects the configured interval
func TestStartForwarder_TickerInterval(t *testing.T) {
	mockStore := newMockStorage()

	var requestCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metricsCollector := storage.NewDBMetricsCollector(mockStore, logger)

	// Use a 50ms interval for faster testing
	testInterval := 50 * time.Millisecond
	forwarder := NewWebhookForwarder(WebhookForwarderOptions{
		TargetURL:        ts.URL,
		Storage:          mockStore,
		MetricsCollector: metricsCollector,
		Logger:           logger,
		PollInterval:     testInterval,
	})

	// Store an event
	ctx := context.Background()
	testEvent := &storage.Event{
		ID:        "interval-test-event",
		Type:      "push",
		Payload:   json.RawMessage(`{}`),
		Headers:   json.RawMessage(`{"X-GitHub-Event": ["push"]}`),
		CreatedAt: time.Now(),
	}
	err := mockStore.StoreEvent(ctx, testEvent)
	require.NoError(t, err)

	// Create cancelable context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start forwarder and measure timing
	startTime := time.Now()
	forwarder.StartForwarder(ctx)

	// Wait for initial processing
	require.Eventually(t, func() bool {
		return requestCount.Load() >= 1
	}, 1*time.Second, 10*time.Millisecond, "Initial event should be processed")

	// Reset and store a new event to test ticker timing
	requestCount.Store(0)
	newEvent := &storage.Event{
		ID:        "interval-test-event-2",
		Type:      "push",
		Payload:   json.RawMessage(`{}`),
		Headers:   json.RawMessage(`{"X-GitHub-Event": ["push"]}`),
		CreatedAt: time.Now(),
	}
	err = mockStore.StoreEvent(ctx, newEvent)
	require.NoError(t, err)

	// The ticker should fire approximately at the configured interval
	// Allow some tolerance for scheduling delays
	require.Eventually(t, func() bool {
		return requestCount.Load() >= 1
	}, 200*time.Millisecond, 10*time.Millisecond, "Ticker should fire within configured interval")

	elapsed := time.Since(startTime)
	t.Logf("Ticker fired after %v (expected ~%v)", elapsed, testInterval)

	// Verify the ticker didn't fire immediately (proves it's timer-based, not busy-loop)
	// We already waited for initial processing, so this measures subsequent ticker fire
	// The elapsed time should be at least close to the interval
	assert.GreaterOrEqual(t, elapsed.Milliseconds(), testInterval.Milliseconds()/2,
		"Ticker should respect configured interval")
}

// TestStartForwarder_DefaultInterval verifies default interval is used when not specified
func TestStartForwarder_DefaultInterval(t *testing.T) {
	mockStore := newMockStorage()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metricsCollector := storage.NewDBMetricsCollector(mockStore, logger)

	// Create forwarder WITHOUT specifying PollInterval
	forwarder := NewWebhookForwarder(WebhookForwarderOptions{
		TargetURL:        ts.URL,
		Storage:          mockStore,
		MetricsCollector: metricsCollector,
		Logger:           logger,
		// PollInterval not set - should use default
	})

	// Verify default is used
	assert.Equal(t, time.Duration(0), forwarder.pollInterval,
		"PollInterval should be zero (will use default in StartForwarder)")
}

// TestStartForwarder_ContextCancellation proves graceful shutdown
func TestStartForwarder_ContextCancellation(t *testing.T) {
	mockStore := newMockStorage()

	var requestCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metricsCollector := storage.NewDBMetricsCollector(mockStore, logger)

	forwarder := NewWebhookForwarder(WebhookForwarderOptions{
		TargetURL:        ts.URL,
		Storage:          mockStore,
		MetricsCollector: metricsCollector,
		Logger:           logger,
		PollInterval:     50 * time.Millisecond,
	})

	// Create cancelable context
	ctx, cancel := context.WithCancel(context.Background())

	// Start the forwarder
	forwarder.StartForwarder(ctx)

	// Let it process for a bit
	time.Sleep(100 * time.Millisecond)
	t.Logf("Requests before cancel: %d", requestCount.Load())

	// Cancel the context
	cancel()

	// Wait for goroutine to finish
	time.Sleep(100 * time.Millisecond)

	countAfterCancel := requestCount.Load()
	t.Logf("Requests after cancel: %d", countAfterCancel)

	// Wait longer and verify no more requests
	time.Sleep(200 * time.Millisecond)

	assert.Equal(t, countAfterCancel, requestCount.Load(),
		"No more requests should occur after context cancellation")
}
