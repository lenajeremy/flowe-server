# Sandbox image for coding agent jobs.
#
# Without a snapshot, Daytona hands us its default image and the Codex CLI is
# npm-installed at the start of every fresh workspace — which means each new
# environment depends on reaching registry.npmjs.org before it can do anything.
# On a network-restricted Daytona tier that is a dependency you cannot widen
# from the sandbox, so the tools an agent needs are baked in here instead.
#
# Build and register:
#   daytona snapshot create fernary-coding-agent \
#     --dockerfile ./sandbox/Dockerfile --cpu 2 --memory 4
# Then set DAYTONA_CODING_AGENT_SNAPSHOT=fernary-coding-agent.
#
# Tags are explicit throughout: Daytona rejects `latest`, `lts` and `stable`.

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8

# Base toolchain. procps and file look incidental but agents shell out to `ps`
# and `file` constantly, and their absence produces confusing failures rather
# than a clear "not installed".
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git jq ripgrep unzip zip tar gzip xz-utils \
      build-essential pkg-config \
      python3 python3-pip python3-venv \
      openssh-client gnupg less vim-tiny nano procps file sudo \
    && rm -rf /var/lib/apt/lists/*

# Node, because the Codex CLI is an npm package. NodeSource rather than the
# Ubuntu archive, whose Node is far behind what Codex expects.
ARG NODE_MAJOR=22
RUN curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Codex, pinned to the same version the runtime expects. The runtime checks
# `codex --version` and only installs when it does not match, so keeping these
# in step is what makes the check a no-op rather than a reinstall every run.
# Must match DefaultCodexCLIVersion in internal/codingagent/types.go.
ARG CODEX_VERSION=0.149.1
RUN npm install -g "@openai/codex@${CODEX_VERSION}" \
    && codex --version

# Go. Fetched by architecture rather than hardcoded to amd64 so the image
# builds on arm64 hosts too.
ARG GO_VERSION=1.26.1
RUN arch="$(dpkg --print-architecture)" \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz
ENV PATH="/usr/local/go/bin:/root/go/bin:${PATH}" \
    GOPATH="/root/go"

# Homebrew refuses to run as root, so it is installed under its own account and
# that account is given passwordless sudo. An agent running as root reaches it
# with `sudo -u linuxbrew -i brew ...`; a sandbox that runs unprivileged picks
# it up from PATH directly.
RUN useradd -m -s /bin/bash linuxbrew \
    && echo 'linuxbrew ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/linuxbrew \
    && chmod 0440 /etc/sudoers.d/linuxbrew
USER linuxbrew
RUN NONINTERACTIVE=1 /bin/bash -c \
      "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
USER root
ENV PATH="/home/linuxbrew/.linuxbrew/bin:/home/linuxbrew/.linuxbrew/sbin:${PATH}"
# Written into the brew account's own profile as well as the system one:
# `sudo -u linuxbrew -i` starts a login shell that reads ~/.profile and
# ~/.bashrc, and Ubuntu's defaults do not reach /etc/bash.bashrc from there.
RUN BREW_ENV='eval "$(/home/linuxbrew/.linuxbrew/bin/brew shellenv)"' \
    && echo "$BREW_ENV" >> /etc/bash.bashrc \
    && echo "$BREW_ENV" >> /etc/profile.d/homebrew.sh \
    && echo "$BREW_ENV" >> /home/linuxbrew/.bashrc \
    && echo "$BREW_ENV" >> /home/linuxbrew/.profile \
    && chown linuxbrew:linuxbrew /home/linuxbrew/.bashrc /home/linuxbrew/.profile

WORKDIR /workspace

# Fail the build rather than the first agent run if anything is missing.
RUN git --version && curl --version >/dev/null && python3 --version \
    && node --version && codex --version && go version \
    && /home/linuxbrew/.linuxbrew/bin/brew --version \
    && sudo -u linuxbrew -i brew --version
