# Upcoming Features

## Chat-with-Workflow (agent mode)

**Mental model:** a workflow with a Chat Trigger is an agent; its nodes are the
agent's tools; a conversation is a run that never ends.

Decisions (agreed 2026-07-11, revised 2026-07-14):
- **No chat node type.** Any workflow is chattable via a "Chat with workflow"
  button — nothing is modeled on the canvas. Agent identity (persona, model,
  greeting) lives in lightweight per-workflow chat settings, defaulting from
  the workflow's name/description so the button works with zero setup.
- **Execution: lazy, per-request, one node at a time.** Nothing runs upfront.
  The orchestrator sees each workflow node as a callable tool and executes
  individual nodes only when the user's request needs them, writing results
  into session state. Edges are intent hints, not ordering. Control-flow/
  display nodes (branch, loop, textOutput) are NOT tools — the orchestrator
  itself is the branching.
- **Per-call overrides, never workflow mutations.** A node's saved config is
  the tool's *defaults*. Each tool schema exposes the node's configurable
  fields (prompt, query, channel, …) as optional parameters: omitted → saved
  value, provided → ephemeral override for that single call. "Summarize my
  email but focus on invoices" adjusts the saved prompt for that call only;
  the canvas workflow is never modified by chatting.
- **Surface: owner-first, in-app.** The button switches to a dedicated chat
  UI where the owner talks to the workflow using all their connected nodes.
  Authenticated with the normal session — secure by default, nothing exposed.
- **Public sharing is opt-in only.** If the owner explicitly shares, a
  `/c/:token` capability link is minted (same pattern as webhook trigger
  pages). Public sessions are rate-limited from day one, and destructive ops
  (refunds, cancels, deletes, merges, trash) are excluded from the public
  tool set — or gated behind an owner approval — since the integration
  expansion (2026-07-14, 179 ops) made anonymous tool access genuinely
  dangerous. Owner sessions have no such restriction.
- **Persistence:** sessions persisted in Postgres — messages + state JSONB,
  keyed by unguessable UUID. Conversations are resumable; owners see session
  history alongside run history. Truncate large tool outputs before persisting
  (a single Drive read can be 1MB; reuse the truncateStr convention).

Architecture notes:
- The executor already threads an `outputs` map (`nodeId → output`) between
  nodes. **The session state bag IS that map, persisted per session.** A tool
  call runs one node with state as `outputs`; the result is written back to
  state. `{{nodeId.output}}` templates keep working unchanged.
- Tool schemas are generated from the canvas per node type. If a capability's
  node is absent (e.g. no email node), there is no such tool — the system
  prompt instructs the orchestrator to say the workflow can't do that.
- Streaming: SSE turn events (thinking / text / tool-start / tool-result) so
  the chat page can render "Fetching Linear issues…" activity chips.
- Needs a single-node execution entry point in the executor — mostly free:
  `executeNode(ctx, node, outputs, edges, keys, runID, ownerID, emit)` already
  has exactly this shape. Also useful for a future "Test node" button.
- Per-call overrides implement naturally there: copy the node, merge the tool
  call's override args into its FlowNodeData, execute the copy. The stored
  workflow is read-only to the chat path by construction.
- Tool schemas should be GENERATED from the AI-builder catalog in
  ai_generate.go (it already documents every op + field for all 179 ops)
  rather than hand-written — otherwise every new op needs updating in three
  places (executor, catalog, tool schemas) and they will drift.

Build order:
1. ✅ Schema (`chat_sessions`) — shipped 2026-07-14
2. ✅ Single-node executor entry (with override merge) + orchestrator tool
   loop + SSE endpoint — shipped 2026-07-14
3. ✅ In-app chat mode (editor "Chat" button → `/workflow/:id/chat`: streaming
   markdown, tool activity chips, lazy session creation, resume via
   `?session=`) — shipped 2026-07-14
4. Opt-in public share (`/c/:token` page + rate limiting + destructive-op
   gating) — only after the in-app mode is solid

Watch-outs:
- Every chat turn spends the owner's provider tokens; on public shares this
  is someone else's spending → rate limit the share link from day one.

## Input-mapping UI (Figma frames 170–171) — ✅ SHIPPED 2026-07-13

Input panel with per-field chips + live previews from the last run, colored
template pills inside config fields (contenteditable chips showing node names),
`{{nodeId.output.field}}` grammar with lazy JSON parsing in both executors,
and value inspection via hover popovers + the run-output modal.

## Persistence (Datastores) + Publish gating

**Why:** recurring/scheduled workflows have no memory. The scheduled trigger
emits only `{trigger,time}` — no counter, cursor, or dedup — and nothing the AI
"stores" is visible. (Surfaced 2026-07-29 by a "send an email every 5 min with
the current iteration index" request that was impossible to satisfy: the LLM
always answered "iteration index not available from the trigger state.") Fix:
general persistence as a first-class primitive, plus a publish flag so only
published workflows auto-fire on schedule.

Decisions locked 2026-07-29 (with Jeremiah):
- **Three formats:** Key–Value, Collection (table), Text/blob.
- **Collections both schemaless and schema-based** (user picks per store; typed
  column types: text · number · boolean · datetime · json).
- **Scope chosen at store creation:**
  - **run** — shared by all nodes in one run; in-memory, discarded at run end.
  - **workflow** — persists across runs; all nodes in that workflow.
  - **account** — persists across all the user's workflows.
- **AI can create stores, but creation is gated by an in-chat approval card**
  (accept / reject / redirect). Approval is for *creation only* — using an
  already-approved store needs no gate. Reads/writes never gated (except public
  chat, see guardrails).
- **Reads v1:** node-based only (`get` → reference its output). Inline
  `{{store.key}}` template reads are a fast-follow.
- **Publish = a simple `workflows.published` bool.** Gates **schedules only** —
  manual runs, webhooks, and API triggers always fire.

### Data model (Postgres, GORM auto-migrate)
- `data_stores`: BaseModel, `user_id`, `workflow_id` (null = account scope),
  `name`, `kind` (kv|collection|text), `scope` (run|workflow|account),
  `schema jsonb` (null = schemaless; else `[{name,type}]`). Unique
  (user_id, coalesce(workflow_id,''), name).
- `data_kv`: `store_id`, `key`, `value jsonb`, `updated_at`; unique
  (store_id, key). Text = one reserved-key blob.
- `data_records`: `id`, `store_id`, `record jsonb`, timestamps (collections;
  validated against schema when typed).
- `workflows.published bool default false`.
- Run-scoped stores are NOT persisted — in-memory map on the run context.
- Every query constrained by `user_id` (tenant isolation).

### Executor — Data node (`data` / NodeTypeData)
- Fields: `dataStoreId, dataOp, dataKey, dataValue(tmpl), dataAmount,
  dataRecord(JSON,tmpl), dataFilter(JSON), dataRecordId, dataLimit`.
- Ops by kind: KV `get/set/increment/delete`; Collection
  `append/query/update/delete/count/clear`; Text `get/set/append`.
- **Atomic** `increment` (`UPDATE … value = value+amt … RETURNING` in a tx) and
  `append` (insert) — overlapping scheduled runs must not clobber a counter.
- Output is JSON → `{{nodeId.output}}` works downstream (increment → email body
  references `{{counter.output}}`).
- Executor stays db-agnostic: inject a `DataStoreOps` interface (mirrors the
  `IntegrationCredsLookup` pattern), implemented in the db/handlers layer.

### API + Data panel (frontend)
- REST: `GET/POST /api/data-stores`, `GET/PATCH/DELETE /api/data-stores/:id`,
  `GET /api/data-stores/:id/entries` (+ manual entry edit/delete + `/clear`).
- "Data" section: list stores; create modal (name/kind/scope/optional schema
  builder); detail view — grid (collections) / key-value editor (kv) / textarea
  (text). AI-created stores appear here — the "see what the AI stored" win.

### AI builder integration
- `list_data_stores` tool (id/name/kind/scope/schema for user + current wf).
- `create_data_store` tool **does not create immediately** — streams a
  `data_store_proposal` event; chat UI renders an approval card. On accept →
  create + resume tool loop with the store as tool_result; reject → resume with
  "rejected"; redirect → user's new text becomes the next turn. (If pausing the
  SSE tool loop is too heavy, fall back to: AI turn ends with the proposal,
  Accept posts an approval message, AI continues next turn.)
- Data node added to `nodeCatalog()` (ops enum + dataFields). System prompt:
  prefer a persistent store for counters/dedup/cursors/accumulation; discover
  via list_data_stores; creation needs approval.

### Publish gating
- Editor Publish/Unpublish button + status badge;
  `POST /api/workflows/:id/publish` / `unpublish`.
- `runDueSchedules` query joins workflows with `published = true`. Webhook /
  API-key / manual paths unchanged. Unpublish leaves the schedule row intact but
  dormant; republish resumes.
- Migration/back-compat: auto-set `published=true` for workflows that currently
  have an **enabled** schedule (don't silently break live ones).

### Guardrails
- Size caps (value/record ≤ ~64KB; per-account collection row cap); truncate/413
  on overflow. Typed collections validate on write. Public/shared chat sessions:
  writes to account-scoped stores blocked or approval-gated (reuse the
  destructive-op exclusion set).

### Build order (commit + push each batch)
1. **Persistence core (backend):** schema + `DataStoreOps` + Data node (all ops,
   all kinds, atomic increment/append, run-scope in-memory) + unit tests.
2. **Data REST + panel (frontend):** CRUD endpoints, Data panel, node card +
   ConfigPanel block.
3. **AI integration:** list/create tools, approval-card protocol, catalog entry,
   system-prompt update.
4. **Publish flag:** `workflows.published`, endpoints + button, scheduler gate,
   migration.
