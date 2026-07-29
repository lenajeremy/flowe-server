package hub

import (
	"sync"

	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/telemetry"
)

// RunHub is a simple in-memory pub/sub for workflow run events.
// It also buffers past events so late subscribers (e.g. approval page opened
// after node_waiting was already emitted) receive the full history.
type RunHub struct {
	mu      sync.Mutex
	subs    map[string][]chan executor.ExecutionEvent
	buffers map[string][]executor.ExecutionEvent // events emitted so far per run
}

var Global = &RunHub{
	subs:    make(map[string][]chan executor.ExecutionEvent),
	buffers: make(map[string][]executor.ExecutionEvent),
}

// WorkflowHub notifies frontend subscribers the instant a run starts for a workflow.
// It sends the run ID as a string so the frontend can immediately attach to the run stream.
type WorkflowHub struct {
	mu   sync.Mutex
	subs map[string][]chan string // workflowID → []chan runID
}

var Workflow = &WorkflowHub{subs: make(map[string][]chan string)}

func (h *WorkflowHub) Subscribe(workflowID string) chan string {
	ch := make(chan string, 4)
	h.mu.Lock()
	h.subs[workflowID] = append(h.subs[workflowID], ch)
	h.mu.Unlock()
	telemetry.AddHubSubscriber("workflow", 1)
	return ch
}

func (h *WorkflowHub) Unsubscribe(workflowID string, ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.subs[workflowID]
	for i, s := range list {
		if s == ch {
			h.subs[workflowID] = append(list[:i], list[i+1:]...)
			close(ch)
			telemetry.AddHubSubscriber("workflow", -1)
			break
		}
	}
	if len(h.subs[workflowID]) == 0 {
		delete(h.subs, workflowID)
	}
}

func (h *WorkflowHub) Publish(workflowID string, runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs[workflowID] {
		select {
		case ch <- runID:
		default:
			telemetry.HubDropped("workflow")
		}
	}
}

// ── Data store changes ────────────────────────────────────────
// Lets open canvases watch the stores their Data nodes point at, so a value
// written by a scheduled run shows up live without a trip to the Data page.
// Topic is the store ID; the SSE handler only subscribes a client to stores it
// has already proved the caller owns.

// DataChange describes one write to a store. Value carries the new value for
// kv/text writes; Count carries the row count for collection writes. Deleted
// marks a key (or the whole store's data) being cleared.
type DataChange struct {
	StoreID string `json:"store_id"`
	Key     string `json:"key,omitempty"`
	Value   string `json:"value,omitempty"`
	Count   *int64 `json:"count,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

type DataHub struct {
	mu   sync.Mutex
	subs map[string][]chan DataChange // storeID → subscribers
}

var Data = &DataHub{subs: make(map[string][]chan DataChange)}

// Subscribe adds ch to a store's watchers. Unlike the other hubs the caller
// owns the channel and passes it in, because one client watches several stores
// at once — so Unsubscribe must never close it.
func (h *DataHub) Subscribe(storeID string, ch chan DataChange) {
	h.mu.Lock()
	h.subs[storeID] = append(h.subs[storeID], ch)
	h.mu.Unlock()
	telemetry.AddHubSubscriber("data", 1)
}

func (h *DataHub) Unsubscribe(storeID string, ch chan DataChange) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.subs[storeID]
	for i, s := range list {
		if s == ch {
			h.subs[storeID] = append(list[:i], list[i+1:]...)
			telemetry.AddHubSubscriber("data", -1)
			break
		}
	}
	if len(h.subs[storeID]) == 0 {
		delete(h.subs, storeID)
	}
}

func (h *DataHub) Publish(ev DataChange) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs[ev.StoreID] {
		select {
		case ch <- ev:
		default:
			telemetry.HubDropped("data")
		}
	}
}

// Subscribe returns a buffered channel pre-loaded with any events already emitted
// for this run, followed by live events as they arrive.
func (h *RunHub) Subscribe(runID string) chan executor.ExecutionEvent {
	h.mu.Lock()
	past := make([]executor.ExecutionEvent, len(h.buffers[runID]))
	copy(past, h.buffers[runID])
	ch := make(chan executor.ExecutionEvent, 256+len(past))
	for _, ev := range past {
		ch <- ev
	}
	h.subs[runID] = append(h.subs[runID], ch)
	h.mu.Unlock()
	telemetry.AddHubSubscriber("run", 1)
	return ch
}

// Unsubscribe removes ch from the subscriber list and closes it.
func (h *RunHub) Unsubscribe(runID string, ch chan executor.ExecutionEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.subs[runID]
	for i, s := range list {
		if s == ch {
			h.subs[runID] = append(list[:i], list[i+1:]...)
			close(ch)
			telemetry.AddHubSubscriber("run", -1)
			break
		}
	}
	if len(h.subs[runID]) == 0 {
		delete(h.subs, runID)
	}
}

// Publish sends an event to all subscribers for runID and appends it to the buffer
// so late joiners receive the full history.
func (h *RunHub) Publish(runID string, event executor.ExecutionEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buffers[runID] = append(h.buffers[runID], event)
	for _, ch := range h.subs[runID] {
		select {
		case ch <- event:
		default:
			telemetry.HubDropped("run")
		}
	}
}

// ClearBuffer drops the in-memory event buffer for a run once it has been
// persisted to the database. Call this after writing events to DB.
func (h *RunHub) ClearBuffer(runID string) {
	h.mu.Lock()
	delete(h.buffers, runID)
	h.mu.Unlock()
}
