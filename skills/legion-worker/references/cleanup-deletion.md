# Strategy: cleanup and deletion pull requests

Read it when you delete deprecated code or stale docs, or consolidate references.

## When you delete a CLI command, check all four

1. Its implementation file or files.
2. Its `package.json` or `pyproject.toml` script entry.
3. Every reference to it in the project's documentation (`AGENTS.md` or its equivalent): check the command tables and the section headings.
4. The wrapper scripts or CI jobs that invoke it.

## A project doc's headings are documentation too

When you update a command reference, grep for its section heading and update the heading in the same commit. A heading that names a path (`## Foo (meta/bar/)`) goes stale when the path changes.

## A deletion pull request is almost entirely deletions

Resist opportunistic refactors. A diff with significant additions means the scope has crept. The value of a cleanup pull request is its tight, reviewable scope.

## Complete the deletion chain

When a feature has an implementation, a CLI wrapper, a `package.json` entry and docs, remove all of them together. A partial deletion leaves broken references.

## Historical records stay

A codebase-wide rename or deletion leaves historical records (transcripts, test snapshots, changelog entries) unchanged.
