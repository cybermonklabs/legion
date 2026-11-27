# Committed patches and drop texts of the label manifests

`rig.sh label-tree` applies a manifest's lines to a fresh tree of the label's commit (`patch`,
`unpatch`, `drop`; see `scored-runs-contracts.ts`, `parseManifest`). This file names the commits
each committed patch and each drop text was read from. A path in a manifest is relative to the
label's tree; a patch or a text file is relative to `packages/pi-legion/scripts/skill-scenarios/`.

## The forward control patches (`controls/forward-only-*.txt`, `controls/forward-owner.txt`)

These four patches are the diff of the texts pull request's own commits, over the base
`a002d32b8a9b918908df177dd74a0252a35655fd`:

| Patch | What it is the diff of | `diff --git` hunks |
| --- | --- | --- |
| `forward-only-texts.patch` | every text the implementer in retro or the merger reads, from the base to the texts head `cda45223718e`, over the paths `scored-job.sh`'s `forward_text_paths` names | 9 |
| `forward-only-sentences.patch` | commit `f3ba646a7901`, "docs(prompts): the retro and the merger report a defect after the last step and go on" | 7 |
| `forward-only-thread.patch` | commit `3e5c890267e5`, "docs(prompts): at the retro, a review thread that needs a fix is reported, not sent back" | 4 |
| `forward-owner.patch` | commit `cda45223718e`, "docs(prompts): the architect asks no role to move reviewed work back past the last step" | 3 |

Each is made with `git diff --no-color --no-ext-diff --no-renames --unified=3 --diff-algorithm=myers
--src-prefix=a/ --dst-prefix=b/`. `scored-job.sh`'s `forward_patch_made` makes each one again at the
label's head, from the commit whose subject the table names, and holds the committed patch to those
bytes (`forward_patch_holds`, C6 row 1). A merge of the base into the texts branch moves the head,
so the patch is made from the commit's subject, not a fixed id, and the check catches a merge that
changes one of these files.

The texts head when they were read is `cda45223718e1e92e08dc7f8145a609bfa5926fc`
(`gh pr view 5640 --json headRefOid`, 2026-11-26).

## `owner-walk-D.patch` (`controls/owner-walk-D.txt`, `controls/owner-walk-D-strict.txt`)

#4902's own change of `packages/daemon/internal/prompts/go/architect-common.md`: the paragraph
that names the read of earlier plans (`dispatch plans`), and its blank line. Form D takes it out
with `unpatch`, so D's tree is the head with the owner's prompt part as it was before #4902.

- #4902's head: `6b0ece5c101b1d63bec807f90417a6c75bb0870f` (`refs/pull/4902/head`), as
  `gh pr view 4902 --json headRefOid` answered at 2026-11-24T11:58Z.
- `main` when read: `d27405712c7c60b164c7d3ecf55f44eaae3f6d2f`. The merge base of the two:
  `42b2ad4d174079f859a20e945ed75e42b109b874`.
- The command, in a checkout that holds both commits:

  ```bash
  git fetch origin main refs/pull/4902/head
  git diff --no-color --no-ext-diff --src-prefix=a/ --dst-prefix=b/ \
    "$(git merge-base origin/main 6b0ece5c101b1d63bec807f90417a6c75bb0870f)" \
    6b0ece5c101b1d63bec807f90417a6c75bb0870f \
    -- packages/daemon/internal/prompts/go/architect-common.md
  ```

- `git apply --numstat` of the patch reads `2 0`, the same as `git diff --numstat` of the two
  commits. Its sha256 is `402486ac9f917acfb3d586a9c829502e6460d31c5b6fae27172ff9e29d66bf58`.
- `git apply --cached -R --check` of the patch passes on an index of #4902's head, and fails on an
  index of `main` (`patch does not apply`), which holds no such paragraph.

The rig holds the patch against the label's own commit when it builds D's tree (scored-job.sh runs
`rig.sh unpatch-holds` on it): D differs from that commit in #4902's lines alone.

## #4902's head's two files (`controls/pr-4902-head/`)

`architect-common.md` and `plan.md` are `packages/daemon/internal/prompts/go/architect-common.md`
and `skills/legion-architect/references/plan.md` at #4902's head
`6b0ece5c101b1d63bec807f90417a6c75bb0870f`, byte for byte: their blobs there are
`f77cd71515fd18717867424b23ab9994cd725504` and `15df444d14aa31cdfc192f83a047fc93164550d7`, and
their sha256 are `f3326fd35d71d2653b68c7c8ebb84dc06e4d0854506578db7989874b4a615c92` and
`d8b2e5e0ef4eb9930dd263f8512ab9297b3145c37ae760e08ec3417d16d4c93a`. They are the only files the
three control manifests change, and no manifest names a commit (no `file` or `reverse` line), so
rig.test.ts's rows of the manifests ("the owner-walk manifests apply to #4902's head's two files,
committed as fixtures", and the D row after it) build a repository of one commit from them and
hold each to its blob: the rows read no network and no ref that moves. The command, in a checkout
that holds the commit:

```bash
git archive --format=tar 6b0ece5c101b1d63bec807f90417a6c75bb0870f \
  packages/daemon/internal/prompts/go/architect-common.md \
  skills/legion-architect/references/plan.md | tar -x -C <dir>
```

## The drop texts (`controls/drops/`)

Each file holds the exact bytes a `drop` line takes out, with no final newline. Where a text
starts with one or two newlines, those are the end of the line before it and the blank line after
the paragraph above it, so the file keeps one blank line between the paragraphs that stay. Each
text was read with `git show <commit>:<path>` and occurs once in its file at #4902's head
`6b0ece5c101b1d63bec807f90417a6c75bb0870f`.

| Drop text | Its file | What it takes out | Count at #4902's head | Count at `main` `d27405712c7c` |
| --- | --- | --- | --- | --- |
| `owner-walk-E-choice-paragraph.txt` | `packages/daemon/internal/prompts/go/architect-common.md` | the paragraph "For each step, decide whether a fresh agent works it or the agent of an earlier step, …" (line 6) | 1 | 1 |
| `owner-walk-E-choice-sentence.txt` | `skills/legion-architect/references/plan.md` | the sentence "For each step, a fresh agent or the agent of an earlier step, and why." of the task block's example (line 18) | 1 | 1 |

E took out a third text until the skills trim (#5564): the bullet of `plan.md` that said this
release cannot carry out the choice of a fresh or an earlier agent, "write the choice all the same"
(lines 25-26 at #4902's head). The trim deletes that bullet, so a drop of it occurs 0 times at any
label that carries the trim, and the Job stops building E's tree. Its drop is gone, and nothing
replaces it: at #5564's head `e42a6ceaf799`, `git grep -i -E 'fresh agent|agent of an earlier|fresh
or an earlier|earlier agent'` over `packages/daemon/internal/prompts`, `skills`,
`packages/pi-legion/src` and `packages/pi-shared/src` finds the two texts E drops and no other. At a
label without the trim, E's tree keeps the bullet. `rig.test.ts`'s row "E's manifest applies to this
checkout's own two files" holds E to the texts a label of this checkout carries, so a change that
deletes one of E's two texts fails that row before it reaches a Job.

| Drop text | Its file | What it takes out | Count at #4902's head | Count at `main` `d27405712c7c` |
| --- | --- | --- | --- | --- |
| `owner-walk-D-strict-plans-read.txt` | `skills/legion-architect/references/plan.md` | the three lines of "Reading how plans went" that name `dispatch plans` and ask for its read (lines 77-79) | 1 | 0 |

D strict removes `plan.md:77-79` at the pinned #4902 head, so it does not apply to the older main
version. The manifests leave the Dispatch CLI's `plans` help unchanged. `../pr-4902-head/README.md`
names what else must change with these lines.
