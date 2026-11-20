# Strategy: systematic rename

Read it when a repository, package or URL is renamed across a codebase.

## Checklist

1. **Scope by file type.** Grep all text-bearing extensions, not just the obvious ones:
   - source code (`.ts`, `.js`, `.py`, `.sh`): functional, must be updated;
   - CI/CD configs (`.yml`, `.yaml`): functional, must be updated;
   - documentation (`.md`): correctness, should be updated;
   - config files (`.json`, `.toml`): check them, but they may be immutable.
2. **Classify each match as mutable or immutable.** Leave historical records (transcripts, test snapshots, progress logs, changelog entries) unchanged: changing them falsifies history.
3. **Check comments for semantic context.** A comment that mentions the old name may still be correct in intent: update the name, and keep the reasoning.
4. **Verify with grep before and after the edit.** Capture the state before the edit as a baseline to compare with.
5. **Verify with `jj diff --git`.** Plain `jj diff` without color concatenates old and new text (for example `old-nameNEW-name`), so you cannot see what changed.
