#!/bin/bash
set -euo pipefail

# skills - a made-up installer of a fixture. It links each skill of this repository's skills/
# into the user skill directories of Claude Code and Oh My Pi, so both hosts list it, and links
# the Oh My Pi settings that leave some skills out.
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

mkdir -p "$HOME/.claude/skills" "$HOME/.omp/agent/skills"
for skill in "$REPO_DIR"/skills/*/; do
    [ -f "${skill}SKILL.md" ] || continue
    name=$(basename "$skill")
    ln -sfn "${skill%/}" "$HOME/.claude/skills/$name"
    ln -sfn "${skill%/}" "$HOME/.omp/agent/skills/$name"
done
ln -sfn "$REPO_DIR/omp/config.yml" "$HOME/.omp/agent/config.yml"
