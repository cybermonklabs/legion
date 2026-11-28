---
title: Workspace provisioning
parent: daemon
depends_on: [daemon-supervision]
paths: [packages/daemon/internal/workspace]
---
Issue workspace provisioning for phase workers: the shared clone of each repository and a jj workspace of it per issue, with the repository credential.
The daemon runs it on its own host; a Sandbox pod runs it as `legion workspace-init` in its init container, which times each step of the provision on one step clock (`Steps`, carried by `Request.Steps` into the issue pod's fill) and logs one line per step as it ends, the names `docs/kubernetes.md` lists.
The daemon's own provisions pass no step clock and log no step.
Right after `jj workspace add` registers a fresh workspace (`createWorkspace`), before any later step of the provision, `Provision` writes `.jj/legion-checkout` without following a link, and keeps it through every retry until a `jj sparse reset` succeeds, then removes it (and removes any leftover marker when `CheckedOut` answers true); a retry that finds no sparse patterns and that marker clears every entry of the workspace except `.jj` and `.git` (`rolefile`, step `checkout-clear`) before checking out again, while a workspace without the marker (one whose patterns an agent cleared, or one an earlier release registered) keeps its untracked files.
That retry first finishes a registration that failed after `jj workspace add` (`finishUnfinishedRegistration`, under the store lock): it records the workspace's creation where no record is there or `Created` refuses the one there, logging the replacement, and sets the issue's bookmark on the working-copy commit where neither the shared clone nor `origin` has it.
Where `origin` has the branch and the shared clone does not, it refuses the workspace by name, with the way out of `origin`'s row: provision again for a conflicted row, which the next fetch settles; restore the bookmark, cancel its deletion and delete the directory, or delete the branch on GitHub, for a tracked row, a local deletion never pushed; and delete the directory for an untracked row, a branch GitHub made after the add.
