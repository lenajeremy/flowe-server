package hub

import (
	"sync"

	"workflow-ai/server/internal/executor"
)

// RunHub is a simple in-memory pub/sub for workflow run events.
type RunHub struct {
	mu   sync.Mutex
	subs map[string][]chan executor.ExecutionEvent
}

var Global = &RunHub{subs: make(map[string][]chan executor.ExecutionEvent)}

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
		}
	}
}

// Subscribe returns a buffered channel that will receive events published for runID.
func (h *RunHub) Subscribe(runID string) chan executor.ExecutionEvent {
	ch := make(chan executor.ExecutionEvent, 256)
	h.mu.Lock()
	h.subs[runID] = append(h.subs[runID], ch)
	h.mu.Unlock()
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
			break
		}
	}
	if len(h.subs[runID]) == 0 {
		delete(h.subs, runID)
	}
}

// Publish sends an event to all subscribers for runID (non-blocking; slow consumers are dropped).
func (h *RunHub) Publish(runID string, event executor.ExecutionEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs[runID] {
		select {
		case ch <- event:
		default:
		}
	}
}
