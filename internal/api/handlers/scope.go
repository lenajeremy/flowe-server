package handlers

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"workflow-ai/server/internal/auth"
)

// Tenant scoping.
//
// Every query against an org-owned table must be restricted to the requesting
// org. Missing one predicate is a cross-tenant data leak, not a bug, so the
// restriction is expressed once here and applied by name everywhere else — a
// reviewer can then check that a handler calls orgScope, rather than reading each
// WHERE clause and reasoning about whether it is sufficient.
//
// Reads moved from user_id to organization_id. Inside a personal org — one member,
// which is every org today — the two are exactly equivalent. The difference only
// appears when an org has a second member, and then sharing is the point.

// orgScope returns a query restricted to the org the request acts within.
//
// If the org is somehow empty the predicate matches nothing rather than
// everything: a missing scope must fail closed. RequireAuth always sets it, so an
// empty value means a route was mounted outside the auth group and returning no
// rows is the correct, loud failure.
func (h *WorkflowHandler) orgScope(c *gin.Context) *gorm.DB {
	return h.db.DB.Where("organization_id = ?", orgIDOrDeny(c))
}

// orgScopeTx is orgScope against an explicit transaction or session.
func orgScopeTx(db *gorm.DB, c *gin.Context) *gorm.DB {
	return db.Where("organization_id = ?", orgIDOrDeny(c))
}

// orgIDOrDeny yields the request's org, or a value that cannot match any row.
// Returning "" would make GORM compare against the empty string, which is also
// unmatchable for a uuid column, but being explicit documents the intent.
func orgIDOrDeny(c *gin.Context) string {
	if id := auth.OrgID(c); id != "" {
		return id
	}
	return "00000000-0000-0000-0000-000000000000"
}

// currentOrgID is the org to stamp on rows being created.
func currentOrgID(c *gin.Context) string {
	return auth.OrgID(c)
}
