package executor

// Which graph a run executes.
//
// A canvas usually holds more than one graph. Besides the flow the user is
// building there are nodes parked to one side: a half-wired branch, or — since
// coding agents can be handed other nodes as tools — nodes that exist purely to
// be CALLED and were never meant to fire on their own. Seeding every node with
// no inbound edge as a root ran all of them, so a GitHub node wired up for an
// agent to open a pull request with would also open one by itself on every run.
//
// Two rules, in order. A caller that knows where the run begins says so, and
// that graph runs whatever else is on the canvas — a trigger fires its own
// flow, not the one next to it. Otherwise the largest graph wins, on the
// reasoning that it is the one being worked on; ties go to whichever appears
// first on the canvas, so the choice is stable between runs.

// connectedComponents groups nodes by reachability, treating edges as
// undirected — direction decides execution order, not membership.
func connectedComponents(nodes []WorkflowASTNode, edges []WorkflowASTEdge) [][]string {
	adjacency := make(map[string][]string, len(nodes))
	for _, edge := range edges {
		adjacency[edge.Source] = append(adjacency[edge.Source], edge.Target)
		adjacency[edge.Target] = append(adjacency[edge.Target], edge.Source)
	}
	seen := make(map[string]bool, len(nodes))
	var components [][]string
	for _, node := range nodes {
		if seen[node.ID] {
			continue
		}
		seen[node.ID] = true
		queue := []string{node.ID}
		var component []string
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			component = append(component, current)
			for _, next := range adjacency[current] {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
		components = append(components, component)
	}
	return components
}

// runnableNodes picks the graph this run executes. An empty entryNodeID, or one
// naming a node that is no longer on the canvas, falls through to the largest.
func runnableNodes(nodes []WorkflowASTNode, edges []WorkflowASTEdge, entryNodeID string) map[string]bool {
	components := connectedComponents(nodes, edges)
	if len(components) <= 1 {
		all := make(map[string]bool, len(nodes))
		for _, node := range nodes {
			all[node.ID] = true
		}
		return all
	}

	chosen := components[0]
	if entryNodeID != "" {
		for _, component := range components {
			for _, id := range component {
				if id == entryNodeID {
					chosen = component
					break
				}
			}
		}
	}
	if entryNodeID == "" || !contains(chosen, entryNodeID) {
		// Strictly greater, so a tie keeps the earlier component and the same
		// canvas produces the same choice every run.
		for _, component := range components[1:] {
			if len(component) > len(chosen) {
				chosen = component
			}
		}
	}

	set := make(map[string]bool, len(chosen))
	for _, id := range chosen {
		set[id] = true
	}
	return set
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
