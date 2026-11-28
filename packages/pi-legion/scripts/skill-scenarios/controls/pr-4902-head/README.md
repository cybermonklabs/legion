# COPIES of two files of #4902's plan guidance change, at its head `64d99a04992`

`architect-common.md` and `plan.md` here are copies, byte for byte, of
`packages/daemon/internal/prompts/go/architect-common.md` and
`skills/legion-architect/references/plan.md` at the change's head
`64d99a04992535295b81d43cdf976359b1bf80a9`, the first commit after #4902's on branch
`legion/INFRA-4115`, whose pull request carries the change since #4902 closed. This directory's
name is #4902's, whose change it holds. The files are fixtures of the owner-walk manifests of every
form (`owner-walk-head.txt` for A, B and C, `owner-walk-D.txt` for D and `owner-walk-E.txt` for
E), not guidance: do not edit them. `../patches/README.md` names the commits, their blobs, their
sha256 and the command that extracts them.

The manifests depend on the exact bytes of the originals. A change to
`skills/legion-architect/references/plan.md` lines 80-82 ("Reading how plans went") or to the
change's paragraph of `architect-common.md` that names `dispatch plans` also needs changes here: in
`../drops/owner-walk-plans-read.txt`, in this directory's `plan.md` or `architect-common.md`, and in
the drop table and blob list of `../patches/README.md`. Every form's manifest takes that drop text
out, so it must occur once in `plan.md`, and D's patch must apply to `architect-common.md`.
