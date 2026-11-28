# Committed patches and drop texts of the label manifests

`rig.sh label-tree` applies a manifest's lines to a fresh tree of the label's commit (`patch`,
`unpatch`, `drop`; see `scored-runs-contracts.ts`, `parseManifest`). This file names the commits
each committed patch and each drop text was read from. A path in a manifest is relative to the
label's tree; a patch or a text file is relative to `packages/pi-legion/scripts/skill-scenarios/`.

## `owner-walk-D.patch` (`controls/owner-walk-D.txt`)

The plan guidance change's own change of `packages/daemon/internal/prompts/go/architect-common.md`:
the paragraph that names the read of earlier plans (`dispatch plans`), and its blank line. Form D
takes it out with `unpatch`, so D's tree is the head with the owner's prompt part without that
paragraph.

The change was #4902's. Since #4902 closed, the pull request of branch `legion/INFRA-4115` carries
it, and the patch and the fixtures below were read at the change's head there:

- The change's head: `25463ff115d309756d8f4fb101ec2d3c2a96cc03`, the commit of branch
  `legion/INFRA-4115` that changes the owner's paragraph on the choice of an agent (line 6 of
  `architect-common.md`), the `plan_task` example of `plan.md` (lines 18-21) and form E's two drop
  texts.
- `main` when read: `c8f964411c1f7236c168d57f4cfc2faa9a650c76`, which
  `4fa95e8167f5d341e8ab8dae153c56f34910c726` merged into the branch, so the merge base of the two is
  `main` itself. The patch is no diff of `main` and the head: the head also changes the choice
  paragraph, which D keeps.
- The command, from the root of a checkout that holds the commit: it writes the file at the head
  with its lines 4 and 5 (the paragraph and the blank line after it) taken out, and diffs that to
  the file at the head.

  ```bash
  c=25463ff115d309756d8f4fb101ec2d3c2a96cc03
  f=packages/daemon/internal/prompts/go/architect-common.md
  s=$(mktemp -d)
  mkdir -p "$s/a/${f%/*}" "$s/b/${f%/*}"
  git show "$c:$f" >"$s/b/$f"
  sed 4,5d "$s/b/$f" >"$s/a/$f"
  (cd "$s" && git diff --no-index --no-color --no-ext-diff --no-prefix --abbrev=11 "a/$f" "b/$f")
  rm -rf "$s"
  ```

- `git apply --numstat` of the patch reads `2 0`. Its `index` line names the blob of the file
  without the paragraph, which no commit holds, and the file's blob at the head. Its sha256 is
  `731310886e98bf8361c89fce4eaebde4ac8028f30e775e408a97f39d75cc45ee`.
- `git apply --cached -R --check` of the patch passes on an index of the change's head, and fails
  on an index of `main` (`patch does not apply`), which holds no such paragraph.

The rig holds D's tree against form A's (`head`, the tree of `controls/owner-walk-head.txt`) of the
same commit when it builds D's (scored-job.sh builds both and runs `rig.sh unpatch-holds ctl-d head`
on them): D differs from A in the change's lines alone. Both take out the plans-read lines below,
so D's tree differs from its commit in those lines too.

## The change's head's two files (`controls/pr-4902-head/`)

`architect-common.md` and `plan.md` are `packages/daemon/internal/prompts/go/architect-common.md`
and `skills/legion-architect/references/plan.md` at the change's head
`25463ff115d309756d8f4fb101ec2d3c2a96cc03`, byte for byte: their blobs there are
`fbb7e914ba5829ff824b65db6aacfdf4e2d854d8` and `4f4fd934e859e507cddf191b9f19be7da9e55217`, and
their sha256 are `3bdc4df1bb09f707075c98dfd63131b47d6996a8d04513506c98224fa882adba` and
`cf6e49d44ef54fb6bf897987aabb8f774a1b584e15b80bc232892571bcf6f284`. The directory keeps the name
of #4902, whose change the files hold. They are the only files the three manifests
(`owner-walk-head.txt`, `owner-walk-D.txt` and `owner-walk-E.txt`) change, and no manifest names a
commit (no `file` or `reverse` line), so rig.test.ts's rows of the manifests ("the owner-walk
manifests apply to #4902's head's two files, committed as fixtures", and "D's tree differs from
form A's tree in the patch's lines alone, at #4902's head's two files") build a repository of one
commit from them and hold each to its blob (`PR_4902_FILES`): the rows read no network and no ref
that moves. The command, in a checkout that holds the commit:

```bash
git archive --format=tar 25463ff115d309756d8f4fb101ec2d3c2a96cc03 \
  packages/daemon/internal/prompts/go/architect-common.md \
  skills/legion-architect/references/plan.md | tar -x -C <dir>
```

## The drop texts (`controls/drops/`)

Each file holds the exact bytes a `drop` line takes out, with no final newline. Where a text
starts with one or two newlines, those are the end of the line before it and the blank line after
the paragraph above it, so the file keeps one blank line between the paragraphs that stay. Each
text was read with `git show <commit>:<path>` and occurs once in its file at the change's head
`25463ff115d309756d8f4fb101ec2d3c2a96cc03`. Each count below was read there and at `main`
`c8f964411c1f7236c168d57f4cfc2faa9a650c76`, and each line number is the text's line at the head.

| Drop text | Its file | What it takes out | Forms that take it | Count at the change's head `25463ff115d` | Count at `main` `c8f964411c1f` |
| --- | --- | --- | --- | --- | --- |
| `owner-walk-E-choice-paragraph.txt` | `packages/daemon/internal/prompts/go/architect-common.md` | the paragraph "For every step of every task, your own `acceptance:owner` step included, write in the task's text which agent works it, and why. …" (line 6) | E | 1 | 0 |
| `owner-walk-E-choice-sentence.txt` | `skills/legion-architect/references/plan.md` | the sentences "For every step, `acceptance:owner` included where the task has it, the agent the work needs …" and "Where this release cannot start that agent, …" of the task block's example (lines 18-21) | E | 1 | 0 |

E took out a third text until the skills trim (#5564): the bullet of `plan.md` that said this
release cannot carry out the choice of a fresh or an earlier agent, "write the choice all the same"
(lines 28-29 at the head). The trim deletes that bullet, so a drop of it occurs 0 times at any
label that carries the trim, and the Job stops building E's tree. Its drop is gone, and nothing
replaces it: at #5564's head `e42a6ceaf799`, `git grep -n -i -E 'fresh agent|agent of an
earlier|fresh or an earlier|earlier agent'` over `packages/daemon/internal/prompts`, `skills`,
`packages/pi-legion/src` and `packages/pi-shared/src` finds one line of each of the two texts E
then dropped, and no other: `architect-common.md:4`, "For each step, decide whether a fresh agent
works it or the agent of an earlier step, …", and `plan.md:11`, "For each step, a fresh agent or
the agent of an earlier step, and why.". Commit `16eba363aa98` rewrote both texts in place, and the
change's head `25463ff115d3` rewrote them again, and E's two drop texts with them. At the head the
same grep over the same paths finds five lines, of `architect-common.md` and of three files in
`skills/legion-architect/references/`:

- `architect-common.md:6`, the paragraph E drops ("That agent is a fresh agent, or the agent of an
  earlier step that you name, …").
- `plan.md:19`, a line of the task block's sentences E drops ("the work needs (a fresh agent, or the
  agent of a named earlier step of this or an earlier issue)").
- `plan.md:29`, the second line of the bullet that starts "This release gives each role one agent
  for each issue" and says "write the choice all the same", the bullet the trim deletes. E keeps
  it: `../owner-walk-E.txt` says E is the control with every choice text taken out only at a label
  that carries the trim, and a run of E at a label without it measures an owner that was still
  asked.
- `wake-routing-steps.md:7` and `wake-routing.md:32`, which name the fresh agent that runs a role
  after `session-lost` or `session-forgotten`. E keeps them too.

No unit test holds E to the texts a label of this checkout carries: a change that deletes one of
E's two texts stops the Job's build of E's tree, since `label_tree` refuses a drop it finds other
than once.

| Drop text | Its file | What it takes out | Forms that take it | Count at the change's head `25463ff115d` | Count at `main` `c8f964411c1f` |
| --- | --- | --- | --- | --- | --- |
| `owner-walk-plans-read.txt` | `skills/legion-architect/references/plan.md` | the three lines of "Reading how plans went" that name `dispatch plans --project` and ask for its read (lines 83-85) | every form: A, B and C (`owner-walk-head.txt`), D and E | 1 | 0 |

Every form takes `plan.md:83-85` out, so the architect skill an owner loads names the read of
earlier plans in no form, and only the owner's prompt part can name it: D, whose prompt part names
no such read, is then a control of that part alone. The lines are three the change wrote in place
of two of `main`'s, so the text occurs 0 times at the older `main`, and no form's tree builds there.
The manifests leave the Dispatch CLI's `plans` help unchanged. `../pr-4902-head/README.md` names
what else must change with these lines.
