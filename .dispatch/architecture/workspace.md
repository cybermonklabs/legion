---
title: Workspace provisioning
parent: daemon
depends_on: [daemon-supervision]
paths: [packages/daemon/internal/workspace]
---
Issue workspace provisioning for phase workers: the shared clone of each repository and a jj workspace of it per issue, with the repository credential. The daemon runs it on its own host; a Sandbox pod runs it as `legion workspace-init` in its init container, which times each step of the provision on one step clock (`Steps`, carried by `Request.Steps` into the issue pod's fill) and logs one line per step as it ends, the names `docs/kubernetes.md` lists. The daemon's own provisions pass no step clock and log no step. Before `jj sparse reset` into a freshly registered workspace or one already marked, `Provision` writes `.jj/legion-checkout` without following a link and removes it once the checkout succeeds (and removes any leftover marker when `CheckedOut` answers true); a retry that finds no sparse patterns and that marker clears every entry of the workspace except `.jj` and `.git` (`rolefile`, step `checkout-clear`) before checking out again, while a workspace without the marker keeps its untracked files.
