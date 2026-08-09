package handlers

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"workflow-ai/server/internal/executor"
)

const agentCapabilityPolicyVersion = 1

type AgentEffect string

const (
	AgentEffectRead        AgentEffect = "read"
	AgentEffectWrite       AgentEffect = "write"
	AgentEffectDestructive AgentEffect = "destructive"
)

type AgentOperationCapability struct {
	ID        string      `json:"id"`
	Label     string      `json:"label"`
	Effect    AgentEffect `json:"effect"`
	Sensitive bool        `json:"sensitive,omitempty"`
}

type AgentNodeCapability struct {
	NodeID            string                     `json:"nodeId"`
	NodeType          executor.NodeType          `json:"nodeType"`
	Label             string                     `json:"label"`
	OperationField    string                     `json:"operationField,omitempty"`
	Operations        []AgentOperationCapability `json:"operations"`
	OverridableFields []string                   `json:"overridableFields"`
}

type AgentNodeGrant struct {
	NodeID                string   `json:"nodeId"`
	AllowedOperations     []string `json:"allowedOperations"`
	AllowedOverrideFields []string `json:"allowedOverrideFields"`
}

// MarshalJSON keeps the public policy contract stable when a least-privilege
// grant has no editable fields. encoding/json normally renders nil slices as
// null, but clients should always receive JSON arrays for collection fields.
func (grant AgentNodeGrant) MarshalJSON() ([]byte, error) {
	type grantJSON AgentNodeGrant
	encoded := grantJSON(grant)
	if encoded.AllowedOperations == nil {
		encoded.AllowedOperations = []string{}
	}
	if encoded.AllowedOverrideFields == nil {
		encoded.AllowedOverrideFields = []string{}
	}
	return json.Marshal(encoded)
}

type AgentCapabilityPolicy struct {
	Version int              `json:"version"`
	Nodes   []AgentNodeGrant `json:"nodes"`
}

// MarshalJSON applies the same collection guarantee to an entirely closed
// policy, which has no node grants.
func (policy AgentCapabilityPolicy) MarshalJSON() ([]byte, error) {
	type policyJSON AgentCapabilityPolicy
	encoded := policyJSON(policy)
	if encoded.Nodes == nil {
		encoded.Nodes = []AgentNodeGrant{}
	}
	return json.Marshal(encoded)
}

type AgentAuthorizedCall struct {
	Node      executor.WorkflowASTNode
	Operation AgentOperationCapability
	Overrides map[string]any
	Reason    string
}

var quotedOperation = regexp.MustCompile(`'([^']+)'`)

func agentWorkflowCapabilities(ast executor.WorkflowAST) []AgentNodeCapability {
	capabilities := make([]AgentNodeCapability, 0, len(ast.Nodes))
	for _, node := range ast.Nodes {
		if capability, ok := agentNodeCapability(node); ok {
			capabilities = append(capabilities, capability)
		}
	}
	return capabilities
}

func agentNodeCapability(node executor.WorkflowASTNode) (AgentNodeCapability, bool) {
	// Hosted turns have their own requester-bound approval mechanism, so calling
	// a canvas Human Approval node would open a second web-run gate and block the
	// queue worker. Image inputs are deferred with attachment support in the
	// text-only first release. Neither is a hosted model tool.
	if agentSkipNode(node.Data.NodeType) || node.Data.NodeType == executor.NodeTypeHumanApproval ||
		node.Data.NodeType == executor.NodeTypeImageInput {
		return AgentNodeCapability{}, false
	}

	capability := AgentNodeCapability{
		NodeID: node.ID, NodeType: node.Data.NodeType, Label: node.Data.Label,
		Operations: []AgentOperationCapability{}, OverridableFields: []string{},
	}
	entry := catalogEntry(string(node.Data.NodeType))
	fieldDocs := map[string]any{}
	if entry != nil {
		fieldDocs, _ = entry["dataFields"].(map[string]any)
	}
	for field := range fieldDocs {
		if _, exists := flowDataFieldType(field); !exists || agentFieldContainsSecret(field) {
			continue
		}
		if field != "label" && field != "nodeType" && field != "integrationToken" {
			capability.OverridableFields = append(capability.OverridableFields, field)
		}
	}
	sort.Strings(capability.OverridableFields)

	switch node.Data.NodeType {
	case executor.NodeTypeHTTPRequest:
		capability.OperationField = "method"
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			// An arbitrary URL can mutate on GET and may sit behind saved auth
			// headers. Unlike a typed integration operation, its semantics cannot
			// be proven from the method, so every generic HTTP call is approval-gated.
			effect := AgentEffectWrite
			if method == "DELETE" {
				effect = AgentEffectDestructive
			}
			capability.Operations = append(capability.Operations, AgentOperationCapability{
				ID: method, Label: method + " request", Effect: effect,
			})
		}
	case executor.NodeTypeData:
		capability.OperationField = "dataOp"
		for _, operation := range []string{"get", "query", "count", "set", "increment", "append", "update", "delete", "clear"} {
			capability.Operations = append(capability.Operations, AgentOperationCapability{
				ID: operation, Label: humanizeAgentOperation(operation), Effect: classifyAgentOperation(operation),
			})
		}
	case executor.NodeTypeEmailSend:
		capability.Operations = []AgentOperationCapability{{
			ID: "send", Label: "Send email", Effect: AgentEffectWrite,
		}}
	default:
		operationDoc, hasOperations := fieldDocs["integrationOp"].(string)
		if hasOperations {
			capability.OperationField = "integrationOp"
			seen := map[string]bool{}
			for _, match := range quotedOperation.FindAllStringSubmatch(operationDoc, -1) {
				operation := match[1]
				if operation == "" || seen[operation] {
					continue
				}
				// Operations whose result can contain credentials are never hosted
				// model tools. A default-off toggle is insufficient: an owner could
				// enable it and the provider response would then enter model context.
				if sensitiveAgentReadOperation(operation) {
					continue
				}
				seen[operation] = true
				capability.Operations = append(capability.Operations, AgentOperationCapability{
					ID: operation, Label: humanizeAgentOperation(operation), Effect: classifyAgentOperation(operation),
				})
			}
		} else {
			capability.Operations = []AgentOperationCapability{{
				ID: "run", Label: "Run", Effect: AgentEffectRead,
			}}
		}
	}

	return capability, len(capability.Operations) > 0
}

func classifyAgentOperation(operation string) AgentEffect {
	normalized := strings.ToLower(strings.TrimSpace(operation))
	if normalized == "find_replace" {
		return AgentEffectWrite
	}
	for _, prefix := range []string{
		"delete", "remove", "revoke", "cancel", "disable", "archive", "refund",
		"void", "disconnect", "reset", "rollback", "clear", "purge", "destroy",
	} {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"_") {
			return AgentEffectDestructive
		}
	}
	if normalized == "run_sql" {
		return AgentEffectDestructive
	}
	for _, prefix := range []string{
		"get", "list", "search", "find", "fetch", "check", "count", "query",
		"download", "export", "lookup", "preview", "read",
	} {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"_") {
			return AgentEffectRead
		}
	}
	if normalized == "run_sql_read_only" || normalized == "generate_types" {
		return AgentEffectRead
	}
	return AgentEffectWrite
}

func sensitiveAgentReadOperation(operation string) bool {
	normalized := strings.ToLower(operation)
	for _, fragment := range []string{
		"auth_config", "env_var", "secret", "api_key", "access_token",
		"function_body", "audit_events", "deploy_key",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func humanizeAgentOperation(operation string) string {
	words := strings.ReplaceAll(operation, "_", " ")
	if words == "" {
		return "Run"
	}
	return strings.ToUpper(words[:1]) + words[1:]
}

func agentFieldContainsSecret(field string) bool {
	return agentSecretConfigFields[strings.ToLower(field)]
}

// defaultSafeAgentPolicy is the deterministic fallback for policy analysis.
// It grants known read operations and keeps every field pinned. Search is
// excluded because it often escapes a saved folder/project boundary.
func defaultSafeAgentPolicy(ast executor.WorkflowAST) AgentCapabilityPolicy {
	policy := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion}
	for _, capability := range agentWorkflowCapabilities(ast) {
		grant := AgentNodeGrant{NodeID: capability.NodeID}
		for _, operation := range capability.Operations {
			if operation.Effect == AgentEffectRead && !operation.Sensitive && !strings.HasPrefix(strings.ToLower(operation.ID), "search") {
				grant.AllowedOperations = append(grant.AllowedOperations, operation.ID)
			}
		}
		if len(grant.AllowedOperations) > 0 {
			policy.Nodes = append(policy.Nodes, grant)
		}
	}
	return policy
}

// normalizeAgentCapabilityPolicy intersects an AI- or owner-proposed policy
// with the server catalog. Invalid identifiers are removed and reported.
func normalizeAgentCapabilityPolicy(ast executor.WorkflowAST, proposed AgentCapabilityPolicy) (AgentCapabilityPolicy, []string) {
	capabilities := map[string]AgentNodeCapability{}
	for _, capability := range agentWorkflowCapabilities(ast) {
		capabilities[capability.NodeID] = capability
	}

	normalized := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion}
	var warnings []string
	seenNodes := map[string]bool{}
	for _, grant := range proposed.Nodes {
		capability, exists := capabilities[grant.NodeID]
		if !exists || seenNodes[grant.NodeID] {
			warnings = append(warnings, fmt.Sprintf("ignored unknown or duplicate node %q", grant.NodeID))
			continue
		}
		seenNodes[grant.NodeID] = true

		knownOperations := map[string]bool{}
		for _, operation := range capability.Operations {
			knownOperations[operation.ID] = true
		}
		knownFields := map[string]bool{}
		for _, field := range capability.OverridableFields {
			knownFields[field] = true
		}

		clean := AgentNodeGrant{NodeID: grant.NodeID}
		seenOperations := map[string]bool{}
		for _, operation := range grant.AllowedOperations {
			if !knownOperations[operation] || seenOperations[operation] {
				warnings = append(warnings, fmt.Sprintf("node %q: ignored unknown or duplicate operation %q", grant.NodeID, operation))
				continue
			}
			seenOperations[operation] = true
			clean.AllowedOperations = append(clean.AllowedOperations, operation)
		}
		seenFields := map[string]bool{}
		for _, field := range grant.AllowedOverrideFields {
			if !knownFields[field] || seenFields[field] || agentFieldContainsSecret(field) || field == capability.OperationField {
				warnings = append(warnings, fmt.Sprintf("node %q: ignored unsafe, unknown or duplicate field %q", grant.NodeID, field))
				continue
			}
			seenFields[field] = true
			clean.AllowedOverrideFields = append(clean.AllowedOverrideFields, field)
		}
		sort.Strings(clean.AllowedOperations)
		sort.Strings(clean.AllowedOverrideFields)
		if len(clean.AllowedOperations) == 0 {
			warnings = append(warnings, fmt.Sprintf("node %q: omitted because it has no valid operations", grant.NodeID))
			continue
		}
		normalized.Nodes = append(normalized.Nodes, clean)
	}
	sort.Slice(normalized.Nodes, func(i, j int) bool { return normalized.Nodes[i].NodeID < normalized.Nodes[j].NodeID })
	return normalized, warnings
}

func agentPolicyGrant(policy AgentCapabilityPolicy, nodeID string) (AgentNodeGrant, bool) {
	for _, grant := range policy.Nodes {
		if grant.NodeID == nodeID {
			return grant, true
		}
	}
	return AgentNodeGrant{}, false
}

// authorizeAgentToolCall is the execution guard. Reduced model schemas improve
// tool selection, but this check remains authoritative immediately before the
// executor receives a call.
func authorizeAgentToolCall(policy AgentCapabilityPolicy, node executor.WorkflowASTNode, rawInput any) (AgentAuthorizedCall, error) {
	grant, exists := agentPolicyGrant(policy, node.ID)
	if !exists {
		return AgentAuthorizedCall{}, fmt.Errorf("node %q is not exposed by this deployment", node.ID)
	}
	capability, exists := agentNodeCapability(node)
	if !exists {
		return AgentAuthorizedCall{}, fmt.Errorf("node %q is not callable", node.ID)
	}

	overrides := map[string]any{}
	if rawInput != nil {
		inputMap, ok := rawInput.(map[string]any)
		if !ok {
			return AgentAuthorizedCall{}, fmt.Errorf("tool input must be an object")
		}
		for field, value := range inputMap {
			overrides[field] = value
		}
	}
	reason, _ := overrides["reason"].(string)
	delete(overrides, "reason")

	operationID := ""
	if capability.OperationField == "" {
		operationID = capability.Operations[0].ID
	} else if override, supplied := overrides[capability.OperationField]; supplied {
		value, ok := override.(string)
		if !ok || value == "" {
			return AgentAuthorizedCall{}, fmt.Errorf("operation field %q must be a non-empty string", capability.OperationField)
		}
		operationID = value
	} else {
		dataJSON, _ := json.Marshal(node.Data)
		var saved map[string]any
		_ = json.Unmarshal(dataJSON, &saved)
		operationID, _ = saved[capability.OperationField].(string)
	}

	allowedOperations := map[string]bool{}
	for _, operation := range grant.AllowedOperations {
		allowedOperations[operation] = true
	}
	if !allowedOperations[operationID] {
		return AgentAuthorizedCall{}, fmt.Errorf("operation %q is not allowed for node %q", operationID, node.ID)
	}
	var operation AgentOperationCapability
	for _, candidate := range capability.Operations {
		if candidate.ID == operationID {
			operation = candidate
			break
		}
	}
	if operation.ID == "" {
		return AgentAuthorizedCall{}, fmt.Errorf("operation %q is not recognized for node %q", operationID, node.ID)
	}

	allowedFields := map[string]bool{}
	for _, field := range grant.AllowedOverrideFields {
		allowedFields[field] = true
	}
	for field := range overrides {
		if field == capability.OperationField {
			continue
		}
		if !allowedFields[field] {
			return AgentAuthorizedCall{}, fmt.Errorf("field %q is pinned for node %q", field, node.ID)
		}
	}
	if operation.Effect != AgentEffectRead && strings.TrimSpace(reason) == "" {
		return AgentAuthorizedCall{}, fmt.Errorf("a reason is required for %s operation %q", operation.Effect, operation.ID)
	}

	return AgentAuthorizedCall{Node: node, Operation: operation, Overrides: overrides, Reason: strings.TrimSpace(reason)}, nil
}
