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
1. Schema (`chat_sessions`) + per-workflow chat settings
2. Single-node executor entry (with override merge) + orchestrator tool loop
   + SSE endpoint
3. In-app chat mode (editor "Chat with workflow" button → chat UI: streaming
   markdown, tool activity chips, session resume)
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
