# Architect escalation

Two matters of an architect are yours: a child that looks independent of its tree, and capacity. A conflict between trees is not yours: the architect that finds it writes to the other tree's architect itself. Send an issue-design question back to the architect that sent it, as a Dispatch comment on the owning issue that mentions its role. It is not yours to decide.

**Capacity.** Order is your only lever. `admission.cap` is the operator's `legion.yaml` setting, and a tree's children compete for it with every other tree's issues. Tell the architect where its tree's issues stand in `admission.waiting`, as a Dispatch comment on the owning issue that mentions its role. Open a finding ask when the deployment needs a larger cap.

## An escalated hold

The architect decided against its own retry. The issue stays at `phase: held` until you release it.

1. **What failed.** `heldFrom` names the phase that the issue was held from. That phase's claim's `failure` gives `cause`, `detail`, `at` and the counts.
2. **Which budget ran out.** Compare each count with its limit in `settings`: `launch_failure_limit` bounds `launchFailures` and `deaths`, `prompt_failure_limit` bounds `promptFailures`, and `prompt_retire_limit` bounds `promptRetires`. Name the limit when you answer the architect, because the architect cannot read it. A `launch_refused` with no count at its limit is a system prompt too large for its pod. No release helps until someone cuts the prompt.
3. **The release.**
   - The cause is gone (a refused launch, a died process, an agent that never came up), or you fixed what `detail` names: `set_status` the held issue to `todo`.
   - The provider keeps refusing the claim's session (`provider_error`, or `prompt_retires_exhausted`): `fresh_session` the claim.
   - The cause stays: keep the tree admitted, and ask whether to park it (`skill://legion-controller/references/park-asks.md`).

## A child that looks independent

Check the child and its parent in `read_state` and Dispatch. When the child still looks independent, open an ask on the report issue. Name the child, why it looks independent, and the root you would file. Give the options `File an issue`, `File it for Legion` and `Do not file`. Never label a child or cut its `child_of` link: that admits it as a root while its tree still records it. Only on a person's yes:

1. File the root with no `--parent`. Its spec cites the ask and carries its `Filed for` line. Add `--label legion` only when the answer asks Legion to run it.
2. Park the child (`set_status` to `icebox`), with a comment that names the new root and the person's answer.

## An update round that is not finishing (`update-stalled`)

This wake says that an update round's bound has passed, and `reason` names what still holds the round.

For each hold that `reason` names:

- The operator's suspension: open a finding ask that names the issue and each role that the operator suspended.
- Your own suspension: `resume_agent` the agent once its cause is gone, or keep the agent held. Open no ask.
- Anything else (a refused pod replacement, no process in the pod, two pods): open a finding ask that names the issue, the round and `reason`, so that an operator acts on it.

Send no role a message about the round, park nothing and write no status. The round ends once the hold clears.
