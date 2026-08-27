# Coding-agent backlog

## Reproducible Daytona sandbox image

- [ ] **Release blocker:** build a versioned Docker image and Daytona snapshot for every coding-agent sandbox. A production run currently fails while installing Codex CLI because the sandbox does not provide `npm`.

Acceptance criteria:

- Bake Go, Python, Bash/Zsh, tmux, Git, Node.js/npm, Linuxbrew, and the configured Codex CLI version into the image.
- Pin the image and major tool versions, publish immutable image tags or digests, and create Daytona environments from the corresponding snapshot instead of installing the toolchain for every job.
- Run agents as an unprivileged user with a writable workspace and explicit package/cache directories. Do not expose the Docker socket or host filesystem inside the sandbox.
- Before accepting a task, run a startup preflight for the shell, Git, Go, Python, Node.js, npm, tmux, brew, and Codex CLI. Fail early with a clear environment-readiness error when anything is missing.
- Keep a bounded, checksum/version-verified bootstrap only as a fallback for repairing older sandboxes.
- Test the actual configured Daytona snapshot by creating a fresh sandbox, checking expected tool versions, launching Codex, cloning a test repository, and running Go, Python, and Node.js commands.
