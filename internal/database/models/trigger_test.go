package models

import (
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

func TestIntegrationTriggerHasUniqueLiveNodeIndex(t *testing.T) {
	t.Parallel()

	parsed, err := schema.Parse(&IntegrationTrigger{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	idx := parsed.LookIndex("idx_integration_trigger_live_node")
	if idx == nil {
		t.Fatal("IntegrationTrigger is missing its live-node unique index")
	}
	if idx.Class != "UNIQUE" {
		t.Fatalf("index class = %q, want UNIQUE", idx.Class)
	}
	if idx.Where != "deleted_at IS NULL" {
		t.Fatalf("index predicate = %q, want deleted_at IS NULL", idx.Where)
	}
	if len(idx.Fields) != 2 || idx.Fields[0].DBName != "workflow_id" || idx.Fields[1].DBName != "node_id" {
		t.Fatalf("index fields = %#v, want workflow_id then node_id", idx.Fields)
	}
}
