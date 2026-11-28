# Committed patches and drop texts of the label manifests

`rig.sh label-tree` applies a manifest's lines to a fresh tree of the label's commit (`patch`,
`unpatch`, `drop`; see `scored-runs-contracts.ts`, `parseManifest`). This file names the commits
each committed patch and each drop text was read from. A path in a manifest is relative to the
label's tree; a patch or a text file is relative to `packages/pi-legion/scripts/skill-scenarios/`.

## `owner-walk-D.patch` (`controls/owner-walk-D.txt`)

The plan guidance change's whole change of
`packages/daemon/internal/prompts/go/architect-common.md`, from the base to the change's head: the
paragraph that names the read of earlier plans (`dispatch plans`) and its blank line, and the
owner's paragraph on the choice of an agent, which the change wrote in place of the base's. Form D
takes it out with `unpatch`, so D's tree is the head with the base's `architect-common.md`: the
owner's prompt part as it was before the change, as `v-guidance-judge` and `v-guidance-walk` say.
It names no read of earlier plans, and it holds the base's paragraph on the choice of an agent.

The change was #4902's. Since #4902 closed, the pull request of branch `legion/INFRA-4115` carries
it, and the patch and the fixtures below were read at the change's head there:

- The change's head: `bda048a7780f9a46a0e0387f1e717425f9e8931d`, the commit of branch
  `legion/INFRA-4115` that adds a sentence to the owner's paragraph on the choice of an agent (line
  6 of `architect-common.md`) and to the `plan_task` example of `plan.md` (lines 16-22), and changes
  form E's two drop texts with them.
- The base: `main` `4d8168367e990b3952909b1b66c3643acb8c0fb4`, which
  `5cf7c997d3209af9dce06c4cd39bde811f97a1ef` merged into the branch, so the merge base of the two is
  `main` itself. Its `architect-common.md` is the one at `c8f964411c1f7236c168d57f4cfc2faa9a650c76`,
  the `main` that `4fa95e8167f5d341e8ab8dae153c56f34910c726` merged before: blob
  `d8549fa3e3e044cbbe2a85209d0a43221c28f991` at both.
- The command, from the root of a checkout that holds both commits:

  ```bash
  git diff --no-color --no-ext-diff --src-prefix=a/ --dst-prefix=b/ --abbrev=11 \
    4d8168367e990b3952909b1b66c3643acb8c0fb4 bda048a7780f9a46a0e0387f1e717425f9e8931d \
    -- packages/daemon/internal/prompts/go/architect-common.md
  ```

- `git apply --numstat` of the patch reads `3 1`, the same as `git diff --numstat` of the two
  commits. Its `index` line names the file's blob at the base and at the head. Its sha256 is
  `c75f442440e9b1ef204cf190c0b3d2f15a39373cda397907613f994565dd4ca1`.
- `git apply --cached -R --check` of the patch passes on an index of the change's head, where the
  reverse leaves the base's file byte for byte, and fails on an index of the base (`patch does not
  apply`), which holds the base's file already.

The rig holds D's tree against form A's (`head`, the tree of `controls/owner-walk-head.txt`) of the
same commit when it builds D's (scored-job.sh builds both and runs `rig.sh unpatch-holds ctl-d head`
on them): D's tree differs from A's in the lines of D's patch alone. Its numstat against A's is the
patch's own reversed, the lines it takes out are the patch's added lines, in order, and the lines
it puts back are the patch's removed lines, in order. Both take out the plans-read lines below, so
D's tree differs from its commit in those lines too.

## The change's head's two files (`controls/pr-4902-head/`)

`architect-common.md` and `plan.md` are `packages/daemon/internal/prompts/go/architect-common.md`
and `skills/legion-architect/references/plan.md` at the change's head
`bda048a7780f9a46a0e0387f1e717425f9e8931d`, byte for byte: their blobs there are
`9e440337702b9621eda5ac95e9aef3d2210e6d30` and `ad404ad4518604d99c0f0fe21cb3a6c88b2bc017`, and
their sha256 are `d5532cafbe4f4ccdcb85f6cd8d913c7f5dade51bad388491c191ba32da82c13b` and
`78e2664625ed76ebe4637db7925983c6766dd47a94df56123326b48922c185cb`. The directory keeps the name
of #4902, whose change the files hold. They are the only files the three manifests
(`owner-walk-head.txt`, `owner-walk-D.txt` and `owner-walk-E.txt`) change, and no manifest names a
commit (no `file` or `reverse` line), so rig.test.ts's rows of the manifests ("the owner-walk
manifests apply to #4902's head's two files, committed as fixtures", and "D's tree differs from
form A's tree in the patch's lines alone, at #4902's head's two files") build a repository of one
commit from them and hold each to its blob (`PR_4902_FILES`): the rows read no network and no ref
that moves. The command, in a checkout that holds the commit:

```bash
git archive --format=tar bda048a7780f9a46a0e0387f1e717425f9e8931d \
  packages/daemon/internal/prompts/go/architect-common.md \
  skills/legion-architect/references/plan.md | tar -x -C <dir>
```

## The drop texts (`controls/drops/`)

Each file holds the exact bytes a `drop` line takes out, with no final newline. Where a text
starts with one or two newlines, those are the end of the line before it and the blank line after
the paragraph above it, so the file keeps one blank line between the paragraphs that stay. Each
text was read with `git show <commit>:<path>` and occurs once in its file at the change's head
`bda048a7780f9a46a0e0387f1e717425f9e8931d`. Each count below was read there and at `main`
`4d8168367e990b3952909b1b66c3643acb8c0fb4`, and each line number is the text's line at the head.

| Drop text | Its file | What it takes out | Forms that take it | Count at the change's head `bda048a7780` | Count at `main` `4d8168367e99` |
| --- | --- | --- | --- | --- | --- |
| `owner-walk-E-choice-paragraph.txt` | `packages/daemon/internal/prompts/go/architect-common.md` | the paragraph "For every step of every task, your own `acceptance:owner` step included, write in the task's text which agent works it, and why. …" (line 6) | E | 1 | 0 |
| `owner-walk-E-choice-sentence.txt` | `skills/legion-architect/references/plan.md` | the sentences "For every step, `acceptance:owner` included where the task has it, the agent the work needs …" and "Where this release cannot start that agent, …" of the task block's example (lines 18-22) | E | 1 | 0 |

E took out a third text until the skills trim (#5564): the bullet of `plan.md` that said this
release cannot carry out the choice of a fresh or an earlier agent, "write the choice all the same"
(lines 29-30 at the head). The trim deletes that bullet, so a drop of it occurs 0 times at any
label that carries the trim, and the Job stops building E's tree. Its drop is gone, and nothing
replaces it: at #5564's head `e42a6ceaf799`, `git grep -n -i -E 'fresh agent|agent of an
earlier|fresh or an earlier|earlier agent'` over `packages/daemon/internal/prompts`, `skills`,
`packages/pi-legion/src` and `packages/pi-shared/src` finds one line of each of the two texts E
then dropped, and no other: `architect-common.md:4`, "For each step, decide whether a fresh agent
works it or the agent of an earlier step, …", and `plan.md:11`, "For each step, a fresh agent or
the agent of an earlier step, and why.". Commit `16eba363aa98` rewrote both texts in place, commit
`25463ff115d3` rewrote them again, and E's two drop texts with them, and the change's head
`bda048a7780f` added a sentence to each, and to E's two drop texts. At the head the same grep over
the same paths finds five lines, of `architect-common.md` and of three files in
`skills/legion-architect/references/`:

- `architect-common.md:6`, the paragraph E drops ("That agent is a fresh agent, or the agent of an
  earlier step that you name, …").
- `plan.md:19`, a line of the task block's sentences E drops ("the work needs (a fresh agent, or the
  agent of a named earlier step of this or an earlier issue)").
- `plan.md:30`, the second line of the bullet that starts "This release gives each role one agent
  for each issue" and says "write the choice all the same", the bullet the trim deletes. E keeps
  it: `../owner-walk-E.txt` says E is the control with every choice text taken out only at a label
  that carries the trim, and a run of E at a label without it measures an owner that was still
  asked.
- `wake-routing-steps.md:7` and `wake-routing.md:32`, which name the fresh agent that runs a role
  after `session-lost` or `session-forgotten`. E keeps them too.

No unit test holds E to the texts a label of this checkout carries: a change that deletes one of
E's two texts stops the Job's build of E's tree, since `label_tree` refuses a drop it finds other
than once.

| Drop text | Its file | What it takes out | Forms that take it | Count at the change's head `bda048a7780` | Count at `main` `4d8168367e99` |
| --- | --- | --- | --- | --- | --- |
| `owner-walk-plans-read.txt` | `skills/legion-architect/references/plan.md` | the three lines of "Reading how plans went" that name `dispatch plans --project` and ask for its read (lines 84-86) | every form: A, B and C (`owner-walk-head.txt`), D and E | 1 | 0 |

Every form takes `plan.md:84-86` out, so the architect skill an owner loads names the read of
earlier plans in no form, and only the owner's prompt part can name it: D, whose prompt part names
no such read, is then a control of that part alone. The lines are three the change wrote in place
of two of `main`'s, so the text occurs 0 times at the older `main`, and no form's tree builds there.
The manifests leave the Dispatch CLI's `plans` help unchanged. `../pr-4902-head/README.md` names
what else must change with these lines.
