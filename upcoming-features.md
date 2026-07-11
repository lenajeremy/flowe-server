# Upcoming Features

## Chat-with-Workflow (agent mode)

**Mental model:** a workflow with a Chat Trigger is an agent; its nodes are the
agent's tools; a conversation is a run that never ends.

Decisions (agreed 2026-07-11):
- **Execution:** orchestrator decides — the chat LLM sees every non-trigger
  node as a callable tool and invokes them on demand (lazy fetch into state).
  Edges become intent hints, not hard ordering.
- **Surface:** public share link (`/c/:token`, same pattern as webhook trigger
  pages). Owner also gets the chat in-app.
- **Modeling:** new `chatTrigger` node type. Its config is the agent identity:
  system prompt/persona, orchestrator model (reuses the multi-provider model
  registry — Claude/GPT/Gemini/Grok all stream with tools already), greeting.
- **Persistence:** sessions persisted in Postgres — messages + state JSONB,
  keyed by unguessable UUID (capability URL, like runs). Conversations are
  resumable; owners see session history alongside run history.

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
- Needs a single-node execution entry point in the executor (run one node
  with a given outputs map + owner ID) — also useful for a future "Test node"
  button.

Build order:
1. Schema (`chat_sessions`) + `chatTrigger` node type
2. Single-node executor entry + orchestrator tool loop + SSE endpoint
3. Public chat page (streaming markdown, tool chips, resume via localStorage)
4. Teach the AI builder the node + publish-flow chat URL + rate limiting

Watch-outs:
- Every public chat turn spends the owner's provider tokens → rate limit the
  share link from day one.
- Tool-schema generation for llm + integration nodes is the fiddliest part.

## Input-mapping UI (Figma frames 170–171)

Data-mapping panel that shows each upstream node's output as copyable /
draggable field chips with live previews from the last run; template tokens
render as colored pills inside config fields; a detail modal shows full
values. Requires structure-opportunistic parsing (JSON → field chips,
plain text → single `output` chip) and field-level template grammar
(`{{nodeId.output.field}}`) resolved by lazy JSON parse at run time.
