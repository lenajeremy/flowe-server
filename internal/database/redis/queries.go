package redis

import "fmt"

type Entity string

const (
	WorkflowRunEntity Entity = "workflow_run"
)

// CacheKey builds a namespaced Redis key.
// Example: "workflow_run:550e8400-e29b-41d4-a716-446655440000"
type CacheKey struct {
	Entity     Entity
	Identifier string
}

func (k CacheKey) String() string {
	return fmt.Sprintf("%s:%s", k.Entity, k.Identifier)
}
