# Coding agents

Fernary's first coding-agent runtime executes Codex inside an isolated Daytona sandbox. A workflow's `codingAgent` node creates a durable job; background workers own the sandbox lifecycle, credential injection, progress events, result retention, and cancellation.

## Production configuration

Required server variables:

- `TOKEN_ENC_KEY`: a stable base64-encoded 32-byte key. Coding agents fail closed without it because the portable Codex `auth.json` cache must never be stored as plaintext.
- `DAYTONA_API_KEY`: a Daytona key that can create, read, start, and delete sandboxes.

Optional variables:

- `DAYTONA_API_URL` and `DAYTONA_TARGET` for a self-hosted endpoint or explicit target.
- `DAYTONA_CODING_AGENT_SNAPSHOT` for a prebuilt snapshot. Without it, sandboxes use `node:22-bookworm` and install Codex when necessary.
- `CODEX_CLI_VERSION`, pinned to `0.149.1` by default. Device login and task execution intentionally use the same version.

Daytona custom per-sandbox domain policies require a tier that permits them. Fernary sends a domain allowlist—not `networkBlockAll` at the same time, because Daytona treats those modes as mutually exclusive. Eight entries are reserved for OpenAI, ChatGPT, GitHub, GitHub content, and npm; a node can add up to twelve more. The resulting allowlist is deny-by-default.

For faster startup, create a Daytona snapshot containing:

- Node.js and npm
- Git
- `@openai/codex@0.149.1`

Never put an OpenAI, GitHub, or Daytona credential in that snapshot.

## User flow

1. Add a **Coding Agent** node from the workflow element sidebar.
2. In the node configuration, choose **Connect Codex**.
3. Open the displayed OpenAI device-login URL and enter the one-time code. Fernary stores only the resulting encrypted authentication cache; it never receives the user's password.
4. Set a GitHub `owner/repository`, optional branch, task, workspace mode, maximum duration, write policy, and any extra allowed domains.
5. Run the node or graph. The ordinary workflow run owns billing/rate-limit admission; the coding task then continues as a durable background job even if the browser disconnects.
6. Inspect recent status, errors, Git status, tracked-file patch, and retained untracked text files in the node sidebar. Active jobs can be cancelled. Reusable workspaces can be reset.

Private repositories use the user's existing Fernary GitHub connection. GitHub credentials are written to temporary mode-0600 files for clone/fetch and removed before Codex starts.

## Safety and recovery properties

- A job is idempotent per workflow-run/node and is never automatically replayed after runtime execution begins.
- PostgreSQL row locks and `SKIP LOCKED` provide multi-replica job claiming. Organization queue capacity is serialized with an advisory transaction lock.
- The same cross-replica authority lock used by hosted agents is held from repository credential injection through durable completion. Codex connection completion, credential disconnect, and membership removal use that lock too, so revocation commits either before a task starts or after an already-authorized task finishes and cannot be undone by a late credential refresh.
- Codex receives `never` approval policy inside its own read-only or workspace-write sandbox. It is explicitly instructed not to push, deploy, publish, open a pull request, or contact people.
- Daytona command metadata never receives credential environment variables. Codex and GitHub credentials use short-lived files and are removed in background cleanup contexts.
- Codex writes a job-specific completion marker before returning. If the PostgreSQL completion transaction fails or Fernary crashes, startup and the background reconciler recover that result from the sandbox without invoking Codex again. An unresolved run receives a longer grace period so a transient Daytona outage does not destroy a recoverable outcome.
- Job status, output metadata, artifacts, session continuity, and the environment lease finalize in one transaction. Artifact bodies are retained separately and are not duplicated in the job JSON.
- Conversation keys are hashed before persistence. Raw command output is excluded from progress events, and common credential patterns are redacted from diagnostics.
- Persistent sandbox policy changes produce a new workspace identity, so a broader old network policy cannot silently survive a node edit.
- The runtime does not push repository changes. Its external effect is limited to the isolated sandbox and Fernary's durable records. Publishing changes should be a separate, explicitly approved future action.

## Current scope

The runtime is Codex-only. The provider and runtime interfaces are deliberately separate so Claude Code or OpenCode can be added without changing job, session, environment, or workflow-node semantics. Attachments, browser/computer use, automatic commits, pushes, pull requests, deployments, and arbitrary shell credentials are not included in this release.
