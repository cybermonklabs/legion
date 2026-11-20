# The parked-decision notice

Read this when you need what a field of a `parked-decision` notice holds (`skill://legion-controller/references/parked-decisions.md`).

Its payload is `{kind: "parked-decision", decision, root, ref, actor, version?, reason?}`:

- `decision`: `design-approved`, `design-changes-requested` (`reason` holds the person's words) or `carried-gate-opened` (an approval of the document a carried root's gate depends on), each with the document's `version`; `ask-answered` or `ask-resolved` (a person answered or resolved a question asked on `<KEY>`); `ask-reply` (a person replied on an ask of `<KEY>`, giving the turn to the agent, or replied under a closed ask).
- `root`: the tree's root, the issue you act on. `<KEY>` is where the decision was made: the root or another issue of its tree.
- `ref`: the decision in Dispatch: `dispatch://<KEY>/spec` (`dispatch://<KEY>/artifact/<slug>` for another gated document), `dispatch://<KEY>/ask/<id>` or `dispatch://<KEY>/comment/<id>`.
- `actor`: the person who made it, `user:<login>`.
