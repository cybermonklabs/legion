Adds `smoke/pnd20-1.md` with a single line for the pnd20-1 kind smoke run.

Dispatch: SPND20-1

## Verification

**CI:** pending — `Tests` run not yet observed at 612136deb041ce2f8b4347d0911788dc4dcfabda.

**Threads:** 0 resolved, 0 unresolved.

**E2E (implementer):** Sandbox repo PR branch `legion/SPND20-1` (pushed head 612136deb041ce2f8b4347d0911788dc4dcfabda) — ran `cd -- "$LEGION_WORKSPACE" && cat smoke/pnd20-1.md`, observed exactly `kind smoke pnd20 1 2026-09-17T14:38:19Z` (40 bytes: 39-char line + one trailing newline), at head 612136deb041ce2f8b4347d0911788dc4dcfabda. Negative control: `cat smoke/absent.md` → `No such file or directory` (nonzero exit).

**E2E (tester):** pending.

**Production:** pending — throwaway kind smoke instance; no production deploy path for this sandbox file.

**Fast-follow:** none.

**Chain:** not stacked.
