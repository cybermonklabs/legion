# A finding outside your issue

Read it when you find a problem your issue does not cover (a defect in another component, repository or tool, a gap in Legion itself) or a production check look fails. It holds for every Legion role and its subagents, and replaces the filing step of `skill://dispatch-first`. A child's own architect opens no ask itself: it sends the finding to the architect above it, and the root's architect opens the ask.

## Fix it where you are, else hand it over, and ask only for a decision

A finding outside your issue never becomes a `dispatch issue` you file, except the child of step 1. Take the first step that fits:

1. **Fix it here.** A defect that stops your work, and a clear defect in code your tree touches or depends on, are your tree's to fix, in whatever repository the fix belongs: no tree, team or agent owns code, and an open issue that tracks the defect is no reason to wait for it (the *Search first* paragraph below). A fix in the repository of your tree's pull request goes into that pull request. A fix in another repository goes into a pull request of your tree there: a child your architect files in that repository (`skill://legion-architect/references/decomposition.md`). Say so in the PR body.
2. **Hand it over.** Any other clear defect goes to the controller with its evidence (*Send a finding to the controller*). It puts it on the open issue that tracks it, hands it to a running tree whose pull request can carry the fix, or files a Legion root.
3. **Ask.** A finding that needs a person's authority, taste or risk appetite (whether it is wanted, which design, a change of scope, cost or risk) is a `dispatch ask` with one recommendation (*What the ask says*).

A defect that stops your work is yours to investigate until you have its fix; any other fix you are not sure of is not step 1. A problem inside your issue is fixed in your change. A phase worker's problem inside its tree but outside its issue goes to the architect its `Legion addressing` line names. A production check look's issue is closed, so its defect takes step 2.

Search first (`skill://dispatch-first`). An open issue that tracks the problem gets your evidence, or what you fixed, as a comment. It is no reason to wait: wait only when it is claimed, its status is `in_progress`, and its holder told you on Envoy that it is working on it now, and then tell that holder what you need and by when. An open ask on the report issue about it (`dispatch open-asks --project <PROJECT>`) gets your evidence as a reply (`dispatch comment --issue <REPORT KEY> --reply-to-ask '<ask id>' --body-file -`); open no second one.

## Send a finding to the controller

1. Post the evidence as a `dispatch comment` on the report issue (*Where it goes*), its first line exactly `Clear defect from <your role token>`. It prints `Posted comment <id>`.
2. `envoy_publish` one message to the controller topic your `Legion addressing` line names, with `expects_reply: false`: your role token; the defect (what went wrong, where, to whom, the numbers, `dispatch://<KEY>`); the repository and component where the fix belongs; the fix; and `dispatch://<REPORT KEY>/comment/<id>`.
3. File nothing and wait for nothing. The controller replies under your evidence comment with where the work went. When the report issue is missing or closed, put the evidence in the message itself.

### When no controller takes it

A publish to a role answers with its live `holder`. When no live session holds the controller role, the answer says Envoy kept the message for one hour (`kept_until`); a keep is not a delivery. An `unresponsive` holder is not a stopped one: release nothing, take nothing from it, and publish no second copy. Treat a kept answer as a controller not taking it, and make the finding an ask on the first of these that holds:

1. **The report issue is open:** ask there, the question opening `Clear defect from <your role token>` and `--ref` naming your evidence comment, then reply `Asked dispatch://<REPORT KEY>/ask/<id>` under that comment.
2. **The report issue is missing or closed:** post the evidence on your own issue and ask there, then publish the ask's reference to the architect that owns your issue. Where your task has an `acceptance:owner` step, it holds your issue's acceptance until a controller takes the ask. That hold keeps your issue open: an open ask on a closed issue drops out of the controller's listing.
3. **Your own issue is closed too** (`409 ISSUE_CLOSED`): put the report issue back in reach, then post and ask there, and say in your summary that you did. This is the one lifecycle status you write, and never through the `legion` tool's `set_status`.
   - It exists and is closed: `dispatch issue-update --issue <REPORT KEY> --status icebox`, the status alone (a closed issue refuses a reopening status beside any other field).
   - None exists: `dispatch issue --project <PROJECT> --title 'Legion daily report' --spec-file -`, with the Summary `skill://legion-controller/references/daily-report.md` gives, then `dispatch issue-update --issue <NEW KEY> --status icebox`. It carries no `legion` label.

## Where it goes

An ask goes on the controller's report issue while it is open: the issue the deployment instructions name for Legion's reports, else the project's `Legion daily report` (`dispatch search --query '"Legion daily report"' --project <PROJECT>`). It stays open in `icebox`, so its asks stay answerable; a closed issue withdraws its open asks and refuses answers. With no open report issue, send the finding to the controller as text (the problem, the issue it came from, the evidence and the decision it needs), and the controller opens the ask; when no controller takes that publish, follow step 2 or 3 above.

## What the ask says

The question stands alone in at most 2,000 characters, the `Ref:` line `--ref` appends included. Its first line is `Clear defect from <your role token>` for a clear defect no controller took, else `Finding from <your role token>`. Then the concrete case: what went wrong, where and to whom (`dispatch://<KEY>`, the machine, file, command or session, the numbers); where the fix belongs, saying plainly when it is not Legion's; the decision a person must make, each option's effect and your recommendation. A clear defect carries the fix instead, with `Hand it over` as the first, recommended option. Evidence the decision does not need goes in a comment posted first and cited by `--ref`:

```bash
dispatch comment --issue '<ASK KEY>' --body-file - <<'EOF'
<the evidence: commands, output with its time, file:line, links>
EOF
# Keep `File it for Legion` only when your Legion addressing line lists the repository where the fix belongs.
dispatch ask --issue '<ASK KEY>' --ref 'dispatch://<ASK KEY>/comment/<id>' --question-file - \
  --option 'File an issue: <what it would track, and who would work it>' \
  --option 'File it for Legion: Legion works it once it is filed' \
  --option 'Do not file: <what stays as it is>' <<'EOF'
Finding from <your role token>
<what went wrong, where and to whom, with dispatch://<KEY> and the numbers; where the fix belongs; what each option does to whom; your recommendation and why>
EOF
```

## Tell the controller about the ask

The answer reaches only the ask's followers. Once the ask is open, `envoy_publish` `dispatch://<REPORT KEY>/ask/<id>` to the controller topic with `expects_reply: false`; the controller files what the answer asks for, and you file nothing. When the listener only kept that publish, do nothing more for an ask on the report issue: the controller's catch-up finds it. An ask on the report issue blocks nothing of your own issue; one on your own issue holds its acceptance. A production check look names the message or the ask in its result and summary.
