# COPIES of two files of #4902's plan guidance change, at its head `c5b3a64f983`

`architect-common.md` and `plan.md` here are copies, byte for byte, of
`packages/daemon/internal/prompts/go/architect-common.md` and
`skills/legion-architect/references/plan.md` at the change's head
`c5b3a64f983a77b2cd37ff5d0895695e961fb483`, the commit of branch `legion/INFRA-4115` that adds a
sentence to the owner's paragraph on the choice of an agent and a paragraph after it that asks the
owner to quote the plan guidance; that branch's pull request carries the change since #4902
closed. This directory's name is #4902's, whose change it holds. The files are
fixtures of the owner-walk manifests of every form (`owner-walk-head.txt` for A, B and C,
`owner-walk-D.txt` for D and `owner-walk-E.txt` for E), not guidance: do not edit them.
`../patches/README.md` names the commits, their blobs, their sha256 and the command that extracts
them.

The manifests depend on the exact bytes of the originals. A change to
`skills/legion-architect/references/plan.md` lines 16-23 (the task block's example, whose sentences
E drops) or lines 85-87 ("Reading how plans went"), or to
`packages/daemon/internal/prompts/go/architect-common.md` lines 1-11 (the hunk of D's patch, which
holds the change's paragraph that names `dispatch plans`, the choice paragraph E drops at line 6 and
the paragraph at line 8 that asks for a quote of the plan guidance), needs
a refresh of each of these that holds the changed bytes or their blob: the texts of `../drops/`,
`../patches/owner-walk-D.patch`, this directory's `plan.md` and `architect-common.md`, the patch's
sha256, the blob list and the drop table of `../patches/README.md`, and `PR_4902_FILES` of
`../../rig.test.ts`. Every form's manifest takes `../drops/owner-walk-plans-read.txt` out, so it
must occur once in `plan.md`; E's manifest takes its two drop texts out, so each must occur once in
its file; and D's patch must apply to `architect-common.md`.
