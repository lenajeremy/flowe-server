# Deployable agents — first-release implementation plan

Last updated: 2026-08-09

## Outcome

Deploy the existing chat-with-workflow agent into Slack. A teammate mentions the
Fernary bot in an allowed channel, the mention starts or continues a thread-backed
chat session, and the agent uses the deploying user's workflow nodes as tools.

The first release is Slack-first but keeps the runtime and host boundary portable
enough for Telegram to be the next adapter.

## Implementation status

The text-only Slack slice is implemented in the current uncommitted backend and
frontend worktrees: host-neutral runtime extraction, immutable deployment models,
AI-assisted policy analysis, deterministic enforcement, Slack ingress and durable
delivery processing, requester-bound write approvals, thread/session mapping,
rate/credit handling and the deployment UI. Production Slack configuration and a
real-workspace end-to-end exercise remain release gates.

## Locked product decisions

- A deployed agent acts with the deploying Fernary user's stored integration
  credentials, never the asker's Fernary identity.
- A deployment pins an immutable workflow snapshot and reviewed capability policy.
  Workflow edits require a new analysis and explicit redeployment.
- Every exposed node has an allowlist of operations and an allowlist of fields the
  model may override. All other configuration remains pinned.
- A deterministic capability catalog is the security source of truth. AI analyzes
  the workflow and Builder conversation to recommend a narrow default policy, but
  server-side validation and execution enforcement make the final decision.
- Writes always require per-call approval in the originating chat thread. Only the
  external teammate who requested the call may approve it.
- An approval states the exact operation, the agent's reason, and the effective
  redacted target/arguments that will execute.
- The first release uses the organization's existing credits plus per-deployment
  and per-requester rate limits. Credit exhaustion is reported in the thread.
- Only mentions trigger turns. The agent reads bounded text context from the Slack
  thread; attachments are out of scope for the first release.
- A deployment has a custom name and alias. One deployment may own a Slack channel
  in the first release; the mentionable Slack identity remains Fernary.

## Security invariants

1. A host can never select the Fernary user or organization a tool runs as.
2. A model-generated schema is not authorization. The effective call is validated
   again immediately before execution.
3. Unknown nodes, operations and fields fail closed.
4. A locked scope field cannot be escaped through another operation. Globally
   scoped search/list operations must be absent when they undermine a pinned scope.
5. Secrets and authorization headers never enter model tool descriptions, policy
   summaries, approval cards, logs or host messages.
6. Approval executes the persisted effective call, not a regenerated call.
7. Host deliveries and approvals are idempotent and authenticated.
8. Turns for one hosted thread are serialized so session state cannot be lost to
   concurrent updates.

## Architecture

### Host-neutral agent runtime

Extract the orchestration currently inside `AgentChatTurn` into a service that
accepts explicit identity/session/workflow/policy inputs and emits typed events.

```text
RunTurn(ctx, TurnRequest, EventSink) -> TurnOutcome
ResumeApproval(ctx, approvalID, externalActor, EventSink) -> TurnOutcome
```

The existing Gin endpoint becomes an adapter that authenticates the owner and
maps typed events to SSE. Slack consumes the same events in a background job and
maps them to thread messages.

### Durable host path

```text
Slack HTTP event
  -> verify signature + timestamp
  -> deduplicate and persist inbound job
  -> acknowledge immediately
  -> database-backed worker claims job
  -> resolve deployment by workspace/channel
  -> load bounded thread text
  -> create/load ChatSession mapping
  -> RunTurn
  -> post/update Slack thread response
```

Slack button interactions follow the same verify/persist/ack path. The worker
checks the clicking Slack user matches the original requester before resolving a
pending approval.

## Data model

### AgentHostInstallation

- organization ID
- provider (`slack` initially)
- external workspace ID/name
- encrypted bot token
- installed-by Fernary user ID
- granted scopes and health timestamps

This is distinct from action-node credentials. The host bot receives and posts
messages; workflow tools continue to use the deploying user's connections.

### AgentDeployment

- organization ID, workflow ID and deploying user ID
- immutable workflow name/nodes/edges snapshot plus source updated-at/hash
- custom name and alias
- provider and host-installation ID
- status (`draft`, `active`, `paused`, `revoked`)
- AI analysis metadata and accepted capability policy JSONB
- timestamps and version

### AgentDeploymentTarget

- deployment ID
- external workspace/channel ID and label
- enabled flag
- unique live `(provider, workspace, channel)` target for first-release routing

### HostedAgentThread

- deployment ID and ChatSession ID
- provider/workspace/channel/thread identifiers
- latest external message timestamp
- unique external thread key

### HostedAgentDelivery

- provider + external delivery ID unique key
- event kind and sanitized payload
- status, attempt count, error, available/claimed/completed timestamps

### HostedAgentApproval

- deployment/thread/session/requester identity
- pinned deployment version and node ID
- operation, reason, effective overrides/config hash
- redacted display details
- status (`pending`, `approved`, `rejected`, `expired`, `executed`, `failed`)
- expiry and resolution timestamps

## Capability policy

Create a structured registry per executable node type. Integration operations
declare effect (`read`, `write`, `destructive`), relevant scope fields, allowable
mutable fields and a plain-language renderer. Non-integration tools such as HTTP,
email, LLM and Data receive equivalent metadata.

The policy analyzer can only select identifiers from this registry. A deterministic
normalizer removes invalid or unsafe grants and produces:

- model-facing reduced tool schemas
- server-side execution grants
- deploy-screen capability sentences
- write-approval rendering metadata

Authenticated in-app owner chat uses an explicit owner policy so the refactor does
not accidentally narrow existing functionality.

## API surface

Authenticated management endpoints:

- `POST /api/workflows/:id/agent-deployments/analyze`
- `POST /api/workflows/:id/agent-deployments`
- `GET /api/workflows/:id/agent-deployments`
- `GET/PATCH/DELETE /api/agent-deployments/:id`
- `GET /api/agent-hosts/slack/connect`
- `DELETE /api/agent-hosts/:id`

Public provider-authenticated endpoints:

- `POST /api/agent-hosts/slack/events`
- `POST /api/agent-hosts/slack/interactions`

## Deployment UI

Add a deployment flow from the workflow chat/editor:

1. Choose/connect Slack workspace.
2. Name the agent and select initially allowed channels.
3. Run AI capability analysis.
4. Review plain-language sections: can read, can propose writes, fixed targets,
   unavailable actions and warnings.
5. Customize operations/fields in an advanced area.
6. Deploy the pinned version and show health/reconnect state.

## Implementation order

1. Runtime extraction and regression tests for web SSE/tool loops.
2. Secret-safe tool description representation.
3. Capability registry, deterministic policy validation and enforcement tests.
4. Deployment/host/thread/delivery/approval schema and management API.
5. AI permission recommendation endpoint.
6. Slack OAuth scope/storage changes and host installation lifecycle.
7. Slack signed event ingestion, durable worker, routing and thread text context.
8. Durable requester-bound write approvals and continuation.
9. Deployment UI.
10. End-to-end Slack verification and operational dashboards/logging.

## First-release verification

- Existing in-app agent chat behavior and stored sessions remain compatible.
- Model schema and executor independently reject disallowed operations/fields.
- Workflow edits do not alter an active deployment.
- Duplicate Slack events cause one turn; simultaneous mentions serialize.
- Disallowed channels and unrecognized deployments do not invoke the model.
- Write calls do not execute before requester approval and cannot be approved by
  another Slack user or reused.
- Approval cards match the exact effective call with secrets redacted.
- Organization credit exhaustion and rate limits produce a thread-visible response.
- Slack signature, replay-age, URL challenge and retry behavior are tested.

## Backlog

- Attachment-aware thread context with safe download, type/size limits, provider
  authorization, redaction and model-input caps.
- A distinct Slack bot/app identity per deployment for a true `@customagentname`.
- Multiple deployments in one channel routed through `@Fernary <agent-alias>`.
- Telegram host adapter.
- Discord `/ask` HTTP interaction adapter; Gateway mentions remain a later choice.
- Google Chat adapter.
- Optional per-deployment credit budgets if organization-level credits and rate
  limits prove insufficient.
