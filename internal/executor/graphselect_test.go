package executor

import (
	"context"
	"testing"
)

func node(id string) WorkflowASTNode {
	v := "x"
	return WorkflowASTNode{ID: id, Data: FlowNodeData{
		NodeType: NodeTypeTextInput, Label: id, DefaultValue: &v,
	}}
}

func chain(ids ...string) []WorkflowASTEdge {
	var edges []WorkflowASTEdge
	for i := 0; i+1 < len(ids); i++ {
		edges = append(edges, WorkflowASTEdge{ID: ids[i] + "->" + ids[i+1], Source: ids[i], Target: ids[i+1]})
	}
	return edges
}

// The canvas that prompted this: a seven-node flow from a trigger, beside a
// three-node chain of GitHub nodes that exist only for a coding agent to call.
// Those three must not fire on their own.
func TestLargestGraphRunsAndToolNodesDoNot(t *testing.T) {
	nodes := []WorkflowASTNode{}
	for _, id := range []string{"tool1", "tool2", "tool3", "trigger", "agent", "format", "out1", "out2", "out3"} {
		nodes = append(nodes, node(id))
	}
	edges := append(chain("tool1", "tool2", "tool3"), chain("trigger", "agent", "format")...)
	edges = append(edges,
		WorkflowASTEdge{ID: "f-1", Source: "format", Target: "out1"},
		WorkflowASTEdge{ID: "f-2", Source: "format", Target: "out2"},
		WorkflowASTEdge{ID: "f-3", Source: "format", Target: "out3"},
	)

	runnable := runnableNodes(nodes, edges, "")
	for _, want := range []string{"trigger", "agent", "format", "out1", "out2", "out3"} {
		if !runnable[want] {
			t.Errorf("%s is on the main graph but was excluded", want)
		}
	}
	for _, tool := range []string{"tool1", "tool2", "tool3"} {
		if runnable[tool] {
			t.Errorf("%s is a tool node on its own graph — it must not run by itself", tool)
		}
	}
}

// A trigger fires ITS graph, even when another graph on the canvas is bigger.
func TestEntryNodeBeatsSize(t *testing.T) {
	nodes := []WorkflowASTNode{}
	for _, id := range []string{"small", "smallNext", "b1", "b2", "b3", "b4"} {
		nodes = append(nodes, node(id))
	}
	edges := append(chain("small", "smallNext"), chain("b1", "b2", "b3", "b4")...)

	runnable := runnableNodes(nodes, edges, "small")
	if !runnable["small"] || !runnable["smallNext"] {
		t.Fatal("the entry node's own graph did not run")
	}
	if runnable["b1"] {
		t.Fatal("the larger graph ran even though a trigger named another one")
	}
}

// An entry node that no longer exists must not select nothing at all.
func TestUnknownEntryFallsBackToLargest(t *testing.T) {
	nodes := []WorkflowASTNode{node("a"), node("b"), node("solo")}
	edges := chain("a", "b")

	runnable := runnableNodes(nodes, edges, "deleted-node")
	if !runnable["a"] || !runnable["b"] {
		t.Fatal("a stale entry id left the run with no graph")
	}
	if runnable["solo"] {
		t.Error("the smaller graph ran too")
	}
}

// Equal-sized graphs must choose the same way every run.
func TestTiesAreStable(t *testing.T) {
	nodes := []WorkflowASTNode{node("a1"), node("a2"), node("b1"), node("b2")}
	edges := append(chain("a1", "a2"), chain("b1", "b2")...)

	first := runnableNodes(nodes, edges, "")
	for i := 0; i < 20; i++ {
		again := runnableNodes(nodes, edges, "")
		if len(again) != len(first) {
			t.Fatalf("selection size changed between runs: %d then %d", len(first), len(again))
		}
		for id := range first {
			if !again[id] {
				t.Fatalf("selection changed between runs — %s dropped out", id)
			}
		}
	}
	if !first["a1"] {
		t.Error("a tie should keep the graph that appears first on the canvas")
	}
}

// A single connected graph is unaffected — every root still starts.
func TestSingleGraphRunsWholly(t *testing.T) {
	nodes := []WorkflowASTNode{node("a"), node("b"), node("c")}
	edges := append(chain("a", "c"), WorkflowASTEdge{ID: "b-c", Source: "b", Target: "c"})

	runnable := runnableNodes(nodes, edges, "")
	for _, id := range []string{"a", "b", "c"} {
		if !runnable[id] {
			t.Errorf("%s was excluded from a single connected graph", id)
		}
	}
}

// End to end: the parked graph produces no node events at all.
func TestRunWorkflowSkipsTheOtherGraph(t *testing.T) {
	nodes := []WorkflowASTNode{node("tool1"), node("tool2"), node("main1"), node("main2"), node("main3")}
	edges := append(chain("tool1", "tool2"), chain("main1", "main2", "main3")...)

	var started []string
	RunWorkflow(context.Background(),
		WorkflowAST{Name: "Two graphs", Nodes: nodes, Edges: edges},
		APIKeys{}, "run", "owner", "org",
		func(ev ExecutionEvent) {
			if ev.Type == EventNodeStarted && ev.NodeID != nil {
				started = append(started, *ev.NodeID)
			}
		})

	if len(started) != 3 {
		t.Fatalf("started %v, want only the three-node graph", started)
	}
	for _, id := range started {
		if id == "tool1" || id == "tool2" {
			t.Fatalf("a parked tool node executed: %v", started)
		}
	}
}
