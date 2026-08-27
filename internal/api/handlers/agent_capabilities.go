package handlers

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"workflow-ai/server/internal/executor"
)

const agentCapabilityPolicyVersion = 2

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

type AgentCapabilityResource struct {
	NodeID         string `json:"nodeId"`
	Label          string `json:"label"`
	PinnedSettings string `json:"pinnedSettings,omitempty"`
}

type AgentIntegrationCapability struct {
	NodeType          executor.NodeType          `json:"nodeType"`
	Label             string                     `json:"label"`
	OperationField    string                     `json:"operationField,omitempty"`
	Operations        []AgentOperationCapability `json:"operations"`
	OverridableFields []string                   `json:"overridableFields"`
	Resources         []AgentCapabilityResource  `json:"resources"`
}

type AgentNodeGrant struct {
	NodeID                string   `json:"nodeId"`
	AllowedOperations     []string `json:"allowedOperations"`
	AllowedOverrideFields []string `json:"allowedOverrideFields"`
}

type AgentIntegrationGrant struct {
	NodeType              executor.NodeType `json:"nodeType"`
	NodeIDs               []string          `json:"nodeIds"`
	AllowedOperations     []string          `json:"allowedOperations"`
	AllowedOverrideFields []string          `json:"allowedOverrideFields"`
}

func (grant AgentIntegrationGrant) MarshalJSON() ([]byte, error) {
	type grantJSON AgentIntegrationGrant
	encoded := grantJSON(grant)
	if encoded.NodeIDs == nil {
		encoded.NodeIDs = []string{}
	}
	if encoded.AllowedOperations == nil {
		encoded.AllowedOperations = []string{}
	}
	if encoded.AllowedOverrideFields == nil {
		encoded.AllowedOverrideFields = []string{}
	}
	return json.Marshal(encoded)
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
	Version      int                     `json:"version"`
	Integrations []AgentIntegrationGrant `json:"integrations,omitempty"`
	// Nodes is the version-1 format retained for existing deployments and jobs.
	Nodes []AgentNodeGrant `json:"nodes,omitempty"`
}

// MarshalJSON applies the same collection guarantee to an entirely closed
// policy, which has no node grants.
func (policy AgentCapabilityPolicy) MarshalJSON() ([]byte, error) {
	if policy.Version >= agentCapabilityPolicyVersion {
		integrations := policy.Integrations
		if integrations == nil {
			integrations = []AgentIntegrationGrant{}
		}
		return json.Marshal(struct {
			Version      int                     `json:"version"`
			Integrations []AgentIntegrationGrant `json:"integrations"`
		}{policy.Version, integrations})
	}
	nodes := policy.Nodes
	if nodes == nil {
		nodes = []AgentNodeGrant{}
	}
	return json.Marshal(struct {
		Version int              `json:"version"`
		Nodes   []AgentNodeGrant `json:"nodes"`
	}{policy.Version, nodes})
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

func agentIntegrationCapabilities(ast executor.WorkflowAST) []AgentIntegrationCapability {
	byType := map[executor.NodeType]*AgentIntegrationCapability{}
	byNode := map[string]executor.WorkflowASTNode{}
	for _, node := range ast.Nodes {
		byNode[node.ID] = node
	}
	for _, nodeCapability := range agentWorkflowCapabilities(ast) {
		group := byType[nodeCapability.NodeType]
		if group == nil {
			label := humanizeAgentOperation(string(nodeCapability.NodeType))
			if entry := catalogEntry(string(nodeCapability.NodeType)); entry != nil {
				if catalogLabel, ok := entry["label"].(string); ok && strings.TrimSpace(catalogLabel) != "" {
					label = catalogLabel
				}
			}
			group = &AgentIntegrationCapability{
				NodeType: nodeCapability.NodeType, Label: label, OperationField: nodeCapability.OperationField,
				Operations:        append([]AgentOperationCapability{}, nodeCapability.Operations...),
				OverridableFields: append([]string{}, nodeCapability.OverridableFields...),
				Resources:         []AgentCapabilityResource{},
			}
			byType[nodeCapability.NodeType] = group
		}
		node := byNode[nodeCapability.NodeID]
		group.Resources = append(group.Resources, AgentCapabilityResource{
			NodeID: nodeCapability.NodeID, Label: nodeCapability.Label,
			PinnedSettings: agentResourcePinnedSettings(node.Data),
		})
	}
	groups := make([]AgentIntegrationCapability, 0, len(byType))
	for _, group := range byType {
		sort.Slice(group.Resources, func(i, j int) bool {
			if group.Resources[i].Label == group.Resources[j].Label {
				return group.Resources[i].NodeID < group.Resources[j].NodeID
			}
			return group.Resources[i].Label < group.Resources[j].Label
		})
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Label < groups[j].Label })
	return groups
}

func agentResourcePinnedSettings(data executor.FlowNodeData) string {
	var fields map[string]any
	if json.Unmarshal([]byte(agentSafeSavedConfig(data)), &fields) != nil {
		return ""
	}
	suffixes := []string{
		"repo", "repository", "repositoryid", "project", "projectid", "projectref", "projectkey",
		"channel", "channelid", "channelname", "folder", "folderid", "account", "accountid", "accountslug",
		"workspace", "workspaceid", "workspacename", "databaseid", "calendarid", "teamid", "teamslug",
		"siteid", "siteurl", "baseid", "baseslug", "orgid", "orgslug", "organizationid", "datastoreid",
		"pageid", "issueid", "listid", "boardid", "tableid", "githubbase",
	}
	for field, value := range fields {
		lower, isResource := strings.ToLower(field), false
		for _, suffix := range suffixes {
			if strings.HasSuffix(lower, suffix) {
				isResource = true
				break
			}
		}
		emptyString, isString := value.(string)
		if !isResource || value == nil || (isString && strings.TrimSpace(emptyString) == "") {
			delete(fields, field)
		}
	}
	if len(fields) == 0 {
		return ""
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	return truncate(string(encoded), 320)
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
	// Hosted integrations resolve the deployer's encrypted connection at run
	// time. A legacy token embedded in workflow JSON would be copied into the
	// deployment and approval records, so fail closed and require reconnecting
	// through Fernary's credential store instead.
	if strings.TrimSpace(node.Data.IntegrationToken) != "" {
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
	case executor.NodeTypeTextInput, executor.NodeTypeLLM:
		capability.Operations = []AgentOperationCapability{{
			ID: "run", Label: "Run", Effect: AgentEffectRead,
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
			// An integration without authoritative operation metadata cannot be
			// proven read-only. Do not expose it until its catalog entry describes
			// the exact operations the server can authorize.
			return AgentNodeCapability{}, false
		}
	}

	return capability, len(capability.Operations) > 0
}

func classifyAgentOperation(operation string) AgentEffect {
	normalized := strings.ToLower(strings.TrimSpace(operation))
	if normalized == "find_replace" {
		return AgentEffectWrite
	}
	if normalized == "run_sql_read_only" || normalized == "generate_types" {
		return AgentEffectRead
	}
	if normalized == "run_sql" {
		return AgentEffectDestructive
	}
	destructiveWords := []string{
		"delete", "remove", "revoke", "cancel", "disable", "archive", "refund",
		"void", "disconnect", "reset", "rollback", "clear", "purge", "destroy",
	}
	writeWords := []string{
		"create", "update", "set", "add", "send", "post", "put", "patch",
		"invite", "share", "upload", "append", "increment", "move", "rename",
		"merge", "approve", "reject", "respond", "reply", "forward", "schedule",
		"publish", "unpublish", "trigger", "execute", "run", "write", "commit",
	}
	// Mutation words take precedence even when an operation starts with a
	// read-looking verb (for example, a future list_and_delete operation). The
	// catalog evolves independently, so prefix-only classification would turn a
	// newly added mutation into an approval bypass.
	parts := strings.FieldsFunc(normalized, func(r rune) bool { return r == '_' || r == '-' })
	for _, part := range parts {
		if slices.Contains(destructiveWords, part) {
			return AgentEffectDestructive
		}
		if slices.Contains(writeWords, part) {
			return AgentEffectWrite
		}
	}
	// Composite operation names are not safe to infer from their first verb.
	// If a future catalog adds list_and_adjust_* without teaching this classifier
	// its new mutation verb, approval is still required.
	if strings.Contains(normalized, "_and_") || strings.Contains(normalized, "_or_") {
		return AgentEffectWrite
	}
	for _, prefix := range []string{
		"get", "list", "search", "find", "fetch", "check", "count", "query",
		"download", "export", "lookup", "preview", "read",
	} {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"_") {
			return AgentEffectRead
		}
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
	for _, capability := range agentIntegrationCapabilities(ast) {
		grant := AgentIntegrationGrant{NodeType: capability.NodeType}
		for _, resource := range capability.Resources {
			grant.NodeIDs = append(grant.NodeIDs, resource.NodeID)
		}
		for _, operation := range capability.Operations {
			if operation.Effect == AgentEffectRead && !operation.Sensitive && !strings.HasPrefix(strings.ToLower(operation.ID), "search") {
				grant.AllowedOperations = append(grant.AllowedOperations, operation.ID)
			}
		}
		if len(grant.AllowedOperations) > 0 {
			policy.Integrations = append(policy.Integrations, grant)
		}
	}
	return policy
}

// normalizeAgentCapabilityPolicy intersects an AI- or owner-proposed policy
// with the server catalog. Invalid identifiers are removed and reported.
func normalizeAgentCapabilityPolicy(ast executor.WorkflowAST, proposed AgentCapabilityPolicy) (AgentCapabilityPolicy, []string) {
	if len(proposed.Integrations) == 0 && len(proposed.Nodes) > 0 {
		return normalizeLegacyAgentCapabilityPolicy(ast, proposed.Nodes)
	}
	capabilities := map[executor.NodeType]AgentIntegrationCapability{}
	for _, capability := range agentIntegrationCapabilities(ast) {
		capabilities[capability.NodeType] = capability
	}
	normalized := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion}
	var warnings []string
	seenTypes := map[executor.NodeType]bool{}
	for _, grant := range proposed.Integrations {
		capability, exists := capabilities[grant.NodeType]
		if !exists || seenTypes[grant.NodeType] {
			warnings = append(warnings, fmt.Sprintf("ignored unknown or duplicate integration %q", grant.NodeType))
			continue
		}
		seenTypes[grant.NodeType] = true
		knownNodes, knownOperations, knownFields := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, resource := range capability.Resources {
			knownNodes[resource.NodeID] = true
		}
		for _, operation := range capability.Operations {
			knownOperations[operation.ID] = true
		}
		for _, field := range capability.OverridableFields {
			knownFields[field] = true
		}
		clean := AgentIntegrationGrant{NodeType: grant.NodeType}
		seenNodes := map[string]bool{}
		for _, nodeID := range grant.NodeIDs {
			if !knownNodes[nodeID] || seenNodes[nodeID] {
				warnings = append(warnings, fmt.Sprintf("integration %q: ignored unknown or duplicate resource %q", grant.NodeType, nodeID))
				continue
			}
			seenNodes[nodeID], clean.NodeIDs = true, append(clean.NodeIDs, nodeID)
		}
		seenOperations := map[string]bool{}
		for _, operation := range grant.AllowedOperations {
			if !knownOperations[operation] || seenOperations[operation] {
				warnings = append(warnings, fmt.Sprintf("integration %q: ignored unknown or duplicate operation %q", grant.NodeType, operation))
				continue
			}
			seenOperations[operation], clean.AllowedOperations = true, append(clean.AllowedOperations, operation)
		}
		seenFields := map[string]bool{}
		for _, field := range grant.AllowedOverrideFields {
			if !knownFields[field] || seenFields[field] || agentFieldContainsSecret(field) || field == capability.OperationField {
				warnings = append(warnings, fmt.Sprintf("integration %q: ignored unsafe, unknown or duplicate field %q", grant.NodeType, field))
				continue
			}
			seenFields[field], clean.AllowedOverrideFields = true, append(clean.AllowedOverrideFields, field)
		}
		sort.Strings(clean.NodeIDs)
		sort.Strings(clean.AllowedOperations)
		sort.Strings(clean.AllowedOverrideFields)
		if len(clean.NodeIDs) == 0 || len(clean.AllowedOperations) == 0 {
			warnings = append(warnings, fmt.Sprintf("integration %q: omitted because it has no valid resources or operations", grant.NodeType))
			continue
		}
		normalized.Integrations = append(normalized.Integrations, clean)
	}
	sort.Slice(normalized.Integrations, func(i, j int) bool { return normalized.Integrations[i].NodeType < normalized.Integrations[j].NodeType })
	return normalized, warnings
}

func normalizeLegacyAgentCapabilityPolicy(ast executor.WorkflowAST, grants []AgentNodeGrant) (AgentCapabilityPolicy, []string) {
	byNode := map[string]executor.WorkflowASTNode{}
	for _, node := range ast.Nodes {
		byNode[node.ID] = node
	}
	groups := map[executor.NodeType][]AgentNodeGrant{}
	var warnings []string
	for _, grant := range grants {
		node, exists := byNode[grant.NodeID]
		if !exists {
			warnings = append(warnings, fmt.Sprintf("ignored unknown legacy node %q", grant.NodeID))
			continue
		}
		groups[node.Data.NodeType] = append(groups[node.Data.NodeType], grant)
	}
	proposed := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion}
	for nodeType, grouped := range groups {
		operations := append([]string{}, grouped[0].AllowedOperations...)
		fields := append([]string{}, grouped[0].AllowedOverrideFields...)
		integration := AgentIntegrationGrant{NodeType: nodeType}
		for _, grant := range grouped {
			integration.NodeIDs = append(integration.NodeIDs, grant.NodeID)
			operations, fields = intersectStrings(operations, grant.AllowedOperations), intersectStrings(fields, grant.AllowedOverrideFields)
		}
		integration.AllowedOperations, integration.AllowedOverrideFields = operations, fields
		proposed.Integrations = append(proposed.Integrations, integration)
		if len(grouped) > 1 {
			warnings = append(warnings, fmt.Sprintf("migrated %d legacy %s node grants to their common integration permissions", len(grouped), nodeType))
		}
	}
	normalized, nextWarnings := normalizeAgentCapabilityPolicy(ast, proposed)
	return normalized, append(warnings, nextWarnings...)
}

func intersectStrings(left, right []string) []string {
	rightSet := map[string]bool{}
	for _, value := range right {
		rightSet[value] = true
	}
	result := make([]string, 0, len(left))
	for _, value := range left {
		if rightSet[value] {
			result = append(result, value)
		}
	}
	return result
}

func agentPolicyGrant(policy AgentCapabilityPolicy, node executor.WorkflowASTNode) (AgentIntegrationGrant, bool) {
	for _, grant := range policy.Integrations {
		if grant.NodeType == node.Data.NodeType && stringSliceContains(grant.NodeIDs, node.ID) {
			return grant, true
		}
	}
	for _, grant := range policy.Nodes {
		if grant.NodeID == node.ID {
			return AgentIntegrationGrant{NodeType: node.Data.NodeType, NodeIDs: []string{node.ID}, AllowedOperations: grant.AllowedOperations, AllowedOverrideFields: grant.AllowedOverrideFields}, true
		}
	}
	return AgentIntegrationGrant{}, false
}

// authorizeAgentToolCall is the execution guard. Reduced model schemas improve
// tool selection, but this check remains authoritative immediately before the
// executor receives a call.
func authorizeAgentToolCall(policy AgentCapabilityPolicy, node executor.WorkflowASTNode, rawInput any) (AgentAuthorizedCall, error) {
	grant, exists := agentPolicyGrant(policy, node)
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
