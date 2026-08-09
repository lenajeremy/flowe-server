package api

import (
	"testing"

	"workflow-ai/server/internal/api/handlers"

	"github.com/gin-gonic/gin"
)

func TestAgentHostRoutesRegisterWithoutWildcardConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Registration itself is the regression assertion: Gin panics when sibling
	// route wildcards conflict, as /:provider/connect and /:id/channels did.
	registerAgentHostRoutes(router.Group("/api"), &handlers.WorkflowHandler{})

	want := map[string]bool{
		"GET /api/agent-hosts":               false,
		"GET /api/agent-hosts/slack/connect": false,
		"GET /api/agent-hosts/:id/channels":  false,
		"DELETE /api/agent-hosts/:id":        false,
	}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("expected registered route %s", route)
		}
	}
}
