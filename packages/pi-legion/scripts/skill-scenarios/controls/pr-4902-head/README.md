# COPIES of two files of #4902's plan guidance change, at its head `16eba363aa9`

`architect-common.md` and `plan.md` here are copies, byte for byte, of
`packages/daemon/internal/prompts/go/architect-common.md` and
`skills/legion-architect/references/plan.md` at the change's head
`16eba363aa980d9d611cbaa0553341cda4e4a895`, the commit of branch `legion/INFRA-4115` that changes
the owner's paragraph on the choice of an agent; that branch's pull request carries the change
since #4902 closed. This directory's name is #4902's, whose change it holds. The files are
fixtures of the owner-walk manifests of every form (`owner-walk-head.txt` for A, B and C,
`owner-walk-D.txt` for D and `owner-walk-E.txt` for E), not guidance: do not edit them.
`../patches/README.md` names the commits, their blobs, their sha256 and the command that extracts
them.

The manifests depend on the exact bytes of the originals. A change to
`skills/legion-architect/references/plan.md` lines 16-21 (the task block's example, whose sentences
E drops) or lines 83-85 ("Reading how plans went"), or to
`packages/daemon/internal/prompts/go/architect-common.md` lines 1-8 (the hunk of D's patch, which
holds the change's paragraph that names `dispatch plans` and the choice paragraph E drops), needs
a refresh of each of these that holds the changed bytes or their blob: the texts of `../drops/`,
`../patches/owner-walk-D.patch`, this directory's `plan.md` and `architect-common.md`, the patch's
sha256, the blob list and the drop table of `../patches/README.md`, and `PR_4902_FILES` of
`../../rig.test.ts`. Every form's manifest takes `../drops/owner-walk-plans-read.txt` out, so it
must occur once in `plan.md`; E's manifest takes its two drop texts out, so each must occur once in
its file; and D's patch must apply to `architect-common.md`.
