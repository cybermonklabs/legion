# Committed patches and drop texts of the label manifests

`rig.sh label-tree` applies a manifest's lines to a fresh tree of the label's commit (`patch`,
`unpatch`, `drop`; see `scored-runs-contracts.ts`, `parseManifest`). This file names the commits
each committed patch and each drop text was read from. A path in a manifest is relative to the
label's tree; a patch or a text file is relative to `packages/pi-legion/scripts/skill-scenarios/`.

## `owner-walk-D.patch` (`controls/owner-walk-D.txt`, `controls/owner-walk-D-strict.txt`)

The plan guidance change's own change of `packages/daemon/internal/prompts/go/architect-common.md`:
the paragraph that names the read of earlier plans (`dispatch plans`), and its blank line. Form D
takes it out with `unpatch`, so D's tree is the head with the owner's prompt part as it was before
the change.

The change was #4902's. Since #4902 closed, the pull request of branch `legion/INFRA-4115` carries
it, and the patch and the fixtures below were read at the change's head there:

- The change's head: `64d99a04992535295b81d43cdf976359b1bf80a9`, the first commit of branch
  `legion/INFRA-4115` after #4902's, which merges `main` into #4902's last head
  `9c0fe93f325968129f8bb15de4a500fd9c950717`.
- `main` when read: `1a37a9ffe738c7e7053ce2f4e1c13ef78dd14d66`, the merge's second parent, so the
  merge base of the two is `main` itself, `1a37a9ffe738c7e7053ce2f4e1c13ef78dd14d66`.
- The command, in a checkout that holds both commits:

  ```bash
  git fetch origin main legion/INFRA-4115
  git diff --no-color --no-ext-diff --src-prefix=a/ --dst-prefix=b/ \
    "$(git merge-base 1a37a9ffe738c7e7053ce2f4e1c13ef78dd14d66 64d99a04992535295b81d43cdf976359b1bf80a9)" \
    64d99a04992535295b81d43cdf976359b1bf80a9 \
    -- packages/daemon/internal/prompts/go/architect-common.md
  ```

- `git apply --numstat` of the patch reads `2 0`, the same as `git diff --numstat` of the two
  commits. Its sha256 is `60b238aa1bbeeac91e9ae34e315ab77836e83525aade0af39a6dc0ecf84b19ef`.
- `git apply --cached -R --check` of the patch passes on an index of the change's head, and fails
  on an index of `main` (`patch does not apply`), which holds no such paragraph.

The rig holds the patch against the label's own commit when it builds D's tree (scored-job.sh runs
`rig.sh unpatch-holds` on it): D differs from that commit in the change's lines alone.

## The change's head's two files (`controls/pr-4902-head/`)

`architect-common.md` and `plan.md` are `packages/daemon/internal/prompts/go/architect-common.md`
and `skills/legion-architect/references/plan.md` at the change's head
`64d99a04992535295b81d43cdf976359b1bf80a9`, byte for byte: their blobs there are
`815a575fe91e47d3bf1d4f23dea5e890e4c02df4` and `d29ab7a6a895a01c7ab054d8904cb176a9be1045`, and
their sha256 are `45a2ae2f4087bbcee83a068d7fff0caf9e92267fe3dcb09e30ad7ca9993bc61e` and
`3751d0f77c40c6ce163eecd20f950b3cc905eb54a04d86fc2e9f5db823d0dee0`. The directory keeps the name
of #4902, whose change the files hold. They are the only files the three control manifests change,
and no manifest names a commit (no `file` or `reverse` line), so rig.test.ts's rows of the
manifests ("the owner-walk manifests apply to #4902's head's two files, committed as fixtures",
and the D row after it) build a repository of one commit from them and hold each to its blob
(`PR_4902_FILES`): the rows read no network and no ref that moves. The command, in a checkout
that holds the commit:

```bash
git archive --format=tar 64d99a04992535295b81d43cdf976359b1bf80a9 \
  packages/daemon/internal/prompts/go/architect-common.md \
  skills/legion-architect/references/plan.md | tar -x -C <dir>
```

## The drop texts (`controls/drops/`)

Each file holds the exact bytes a `drop` line takes out, with no final newline. Where a text
starts with one or two newlines, those are the end of the line before it and the blank line after
the paragraph above it, so the file keeps one blank line between the paragraphs that stay. Each
text was read with `git show <commit>:<path>` and occurs once in its file at the change's head
`64d99a04992535295b81d43cdf976359b1bf80a9`. Each count below was read there and at `main`
`1a37a9ffe738c7e7053ce2f4e1c13ef78dd14d66`, and each line number is the text's line at the head.

| Drop text | Its file | What it takes out | Count at the change's head `64d99a04992` | Count at `main` `1a37a9ffe738` |
| --- | --- | --- | --- | --- |
| `owner-walk-E-choice-paragraph.txt` | `packages/daemon/internal/prompts/go/architect-common.md` | the paragraph "For each step, decide whether a fresh agent works it or the agent of an earlier step, …" (line 6) | 1 | 1 |
| `owner-walk-E-choice-sentence.txt` | `skills/legion-architect/references/plan.md` | the sentence "For each step, a fresh agent or the agent of an earlier step, and why." of the task block's example (line 18) | 1 | 1 |

E took out a third text until the skills trim (#5564): the bullet of `plan.md` that said this
release cannot carry out the choice of a fresh or an earlier agent, "write the choice all the same"
(lines 25-26 at the head). The trim deletes that bullet, so a drop of it occurs 0 times at any
label that carries the trim, and the Job stops building E's tree. Its drop is gone, and nothing
replaces it: at #5564's head `e42a6ceaf799`, `git grep -i -E 'fresh agent|agent of an earlier|fresh
or an earlier|earlier agent'` over `packages/daemon/internal/prompts`, `skills`,
`packages/pi-legion/src` and `packages/pi-shared/src` finds the two texts E drops and no other. At a
label without the trim, E's tree keeps the bullet. `rig.test.ts`'s row "E's manifest applies to this
checkout's own two files" holds E to the texts a label of this checkout carries, so a change that
deletes one of E's two texts fails that row before it reaches a Job.

| Drop text | Its file | What it takes out | Count at the change's head `64d99a04992` | Count at `main` `1a37a9ffe738` |
| --- | --- | --- | --- | --- |
| `owner-walk-D-strict-plans-read.txt` | `skills/legion-architect/references/plan.md` | the three lines of "Reading how plans went" that name `dispatch plans` and ask for its read (lines 80-82) | 1 | 0 |

D strict removes `plan.md:80-82` at the change's pinned head, three lines the change wrote in place
of two of `main`'s, so it does not apply to the older main version. The manifests leave the
Dispatch CLI's `plans` help unchanged. `../pr-4902-head/README.md` names what else must change
with these lines.
