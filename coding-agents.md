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

Open internet access is the default so Codex can use live search, documentation, and package registries. Owners can opt into a Daytona domain allowlist. Daytona counts Fernary's callback, Codex/OpenAI, npm, the repository provider, and owner-added domains together toward its 20-domain ceiling; the editor conservatively accepts up to ten owner-added entries. Daytona plans that do not support custom network policies cannot enforce this restricted mode.

For faster startup, create a Daytona snapshot containing:

- Node.js and npm
- Git
- `@openai/codex@0.149.1`

Never put an OpenAI, GitHub, or Daytona credential in that snapshot.

## User flow

1. Add a **Coding Agent** node from the workflow element sidebar.
2. In the node configuration, choose **Connect Codex**.
3. Open the displayed OpenAI device-login URL and enter the one-time code. Fernary stores only the resulting encrypted authentication cache; it never receives the user's password.
4. Select a repository from the user's connected GitHub or GitLab resources, then set the optional branch, task, workspace mode, maximum duration, repository-write policy, and network policy.
5. Optionally grant other workflow nodes as tools. Each grant names exact operations and fields Codex may override. Unselected nodes, operations, and fields are denied.
6. Run the node or graph. The ordinary workflow run owns billing/rate-limit admission; coding-agent callbacks continue settling against that run. The coding task continues if the browser disconnects.
7. Inspect status, command activity, workflow-tool approvals/results, Git status, the baseline-relative patch, and retained untracked text files. Active jobs can be cancelled. Reusable sandboxes can be reset.

Private repositories use the user's existing Fernary GitHub connection. GitHub credentials are written to temporary mode-0600 files for clone/fetch and removed before Codex starts.

## Safety and recovery properties

- A job is idempotent per workflow-run/node and is never automatically replayed after runtime execution begins.
- PostgreSQL row locks and `SKIP LOCKED` provide multi-replica job claiming. Organization queue capacity is serialized with an advisory transaction lock.
- The same cross-replica authority lock used by hosted agents is held from repository credential injection through durable completion. Codex connection completion, credential disconnect, and membership removal use that lock too, so revocation commits either before a task starts or after an already-authorized task finishes and cannot be undone by a late credential refresh.
- Each job gets a fresh repository checkout, including in a reusable sandbox. Reusable mode retains the Daytona environment and explicit Codex conversation, not unreviewed filesystem changes from an earlier job.
- Codex receives `never` for its own shell approval policy inside a read-only or workspace-write sandbox. It has no repository credential while it runs and cannot push or deploy through shell commands.
- Workflow-node authority is frozen with the exact submitted graph. New grants are operation- and field-specific. Older node-id grants are migrated only when the job has a frozen graph, and become pinned read-only access.
- Workflow-tool writes are persisted before execution and require owner approval. Equivalent pending, executing, or outcome-unknown mutations are blocked. Uncertain outcomes require explicit reconciliation before retry.
- Daytona command metadata never receives credential environment variables. Codex and GitHub credentials use short-lived files and are removed in background cleanup contexts.
- The workflow-tool bearer token is job-scoped, stored only as a digest server-side, revoked on cancellation/completion/member removal, and excluded from shell child environments.
- Codex writes a job-specific completion marker before returning. If the PostgreSQL completion transaction fails or Fernary crashes, startup and the background reconciler recover that result from the sandbox without invoking Codex again. An unresolved run receives a longer grace period so a transient Daytona outage does not destroy a recoverable outcome.
- Job status, output metadata, artifacts, session continuity, and the environment lease finalize in one transaction. Artifact bodies are retained separately and are not duplicated in the job JSON.
- Conversation keys are hashed before persistence. Raw command output is excluded from progress events, and common credential patterns are redacted from diagnostics.
- Persistent sandbox policy changes produce a new workspace identity, so a broader old network policy cannot silently survive a node edit.
- Codex cannot use repository credentials directly. Branches, commits, pull requests, messages, or deployments are possible only when the workflow contains matching integration nodes, the owner grants their exact capabilities, and each write is approved.
- Closing the browser does not cancel a manual workflow. A full server-process restart recovers the coding job and its event history, but the current in-memory workflow executor cannot resume arbitrary downstream graph nodes after that restart; the run remains inspectable rather than replaying completed side effects.

## Current scope

The runtime is Codex-only. The provider and runtime interfaces are deliberately separate so Claude Code or OpenCode can be added without changing job, session, environment, or workflow-node semantics. Attachments, browser/computer use, direct repository credentials, automatic remote publishing, and arbitrary shell credentials are not included. Explicitly granted workflow tools can provide separately approved GitHub/GitLab publishing operations.
