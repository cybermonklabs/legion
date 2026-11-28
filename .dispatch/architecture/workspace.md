---
title: Workspace provisioning
parent: daemon
depends_on: [daemon-supervision]
paths: [packages/daemon/internal/workspace]
---
Issue workspace provisioning for phase workers: the shared clone of each repository and a jj workspace of it per issue, with the repository credential.
The daemon runs it on its own host; a Sandbox pod runs it as `legion workspace-init` in its init container, which times each step of the provision on one step clock (`Steps`, carried by `Request.Steps` into the issue pod's fill) and logs one line per step as it ends, the names `docs/kubernetes.md` lists.
The daemon's own provisions pass no step clock and log no step.
Right after `jj workspace add` registers a fresh workspace (`createWorkspace`), before any later step of the provision, `Provision` writes `.jj/legion-checkout` without following a link, and keeps it until a `jj sparse reset` succeeds, then removes it.
A stop between the checkout's end and that removal leaves the marker on a whole workspace, and no role starts beside it: `Provision` removes it from a workspace `CheckedOut` finds whole, which an issue pod's `workspace-init` provisions at every start, and the daemon's tmux launch provisions a whole workspace that still carries the marker (`HasCheckoutMarker`, the outbox's `unprovisioned`) before its pane starts.
A retry that finds no sparse patterns and that marker comes after a provision that stopped somewhere from its `jj workspace add` to its checkout's end, before any role started, so it takes the workspace away under the store lock (`resetUnfinishedWorkspace`, step `workspace-reset`, between `checkout-check` and `workspace-registration`): it deletes the issue's bookmark where that provision set it on the workspace's working-copy commit, renames the directory aside to `<dir>.removing`, forgets the workspace in the shared clone and removes its git worktree entry.
`workspace-registration` then adds it again from nothing (`createWorkspace`), so no creation record, partial checkout or bookmark that provision set on its working-copy commit survives.
Once the store lock is let go, and before the checkout, `Provision` deletes `<dir>.removing` without following a link (`rolefile`, step `workspace-delete`), as `Remove` deletes its own, so no other issue's provision or removal waits for that delete.
A `<dir>.removing` a stopped reset or removal left is deleted before `Provision` takes the store lock, in a `workspace-delete` step that is then its first.
A workspace without the marker (one whose patterns an agent cleared, or one an earlier release registered) keeps its untracked files.
