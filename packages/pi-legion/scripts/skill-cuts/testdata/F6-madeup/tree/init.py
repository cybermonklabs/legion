"""A made-up loader of a fixture: `tree init` writes tree.toml, whose [roles.*] tables name the
skills each role's session loads at its start."""

from __future__ import annotations

import sys
from pathlib import Path

TREE_REL = Path("tree")

DEFAULT_TOML = """[spawner]
runtime = "omp"
max_time = "45m"

[roles.proposer]
tools = ["read", "glob", "grep", "bash", "task"]
skills = ["alpha", "beta"]
[roles.prover]
tools = ["read", "glob", "grep", "bash", "task"]
skills = ["alpha", "tool-loop-alpha"]
[roles.critic]
tools = ["read", "glob", "grep", "bash", "task"]
skills = ["alpha"]
"""


def cmd_init(a) -> int:
    tree = Path.cwd() / TREE_REL
    toml = tree / "tree.toml"
    if toml.exists():
        print(f"tree init: {toml} exists", file=sys.stderr)
        return 1
    tree.mkdir(parents=True, exist_ok=True)
    toml.write_text(DEFAULT_TOML)
    return 0


def register(sub) -> None:
    p = sub.add_parser("init", help="write tree.toml with each role's skills")
    p.set_defaults(func=cmd_init)
