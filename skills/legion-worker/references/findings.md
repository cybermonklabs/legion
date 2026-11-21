# A finding outside your issue

Read it when you find a problem your issue does not cover (a defect in another component, repository or tool, a gap in Legion itself) or a production check look fails. It holds for every Legion role and its subagents, and replaces the filing step of `skill://dispatch-first` and `skill://dispatch`. A child's own architect opens no ask itself: it sends the finding to the architect above it with `envoy_publish` (`skill://legion-architect`, "Every turn"), and the root's architect opens the ask.

## Fix it where you are, else hand it over, and ask only for a decision

A finding outside your issue never becomes a `dispatch issue` you file. Take the first step that fits:

1. **Fix it here.** A defect with a clear fix, in code your tree touches or depends on, goes into your tree's own pull request while you are in that code. The fix needs no choice a person must make, and the evidence shows the defect. Say so in your handoff and the PR body. A fix deferred to another issue is a fix nobody makes.
2. **Hand it over.** Any other clear defect goes to the controller with its evidence (*Send a finding to the controller*). It gives the work to the tree that owns the code, or files a Legion root (a root issue labelled `legion`, which the daemon admits) when no tree owns it.
3. **Ask.** A finding that needs a person's authority, taste or risk appetite (whether it is wanted, which design, who owns it, a change of scope, cost or risk) is a `dispatch ask` with one recommendation (*What the ask says*).

A fix you are not sure of is not step 1: take step 2, or step 3 when a person must decide. A problem inside your issue is fixed in your change. A problem inside your tree is work its root spec's acceptance needs. A phase worker's problem inside its tree but outside its issue goes to the architect its `Legion addressing` line names. That architect adds it to an open pull request of an issue it owns, or files a child under its own issue when it is a separately deliverable change. It sends a problem in an issue it does not own to the architect above it. Anything else, in the same repository or not, is a finding.

Search first (`skill://dispatch-first`). An open issue that tracks the problem gets your evidence as a comment, and you send or ask nothing more. An open ask on the report issue about it (`dispatch open-asks --project <PROJECT>`) gets your evidence as a reply (`dispatch comment --issue <REPORT KEY> --reply-to-ask '<ask id>' --body-file -`); open no second one.

## Send a finding to the controller

1. Post the evidence as a `dispatch comment` on the report issue (*Where it goes*), its first line exactly `Clear defect from <your role token>`. Your role token is your role topic, as your `Legion addressing` line names it, without `notifications.role.`. It prints `Posted comment <id>`.
2. `envoy_publish` one message to the controller topic your `Legion addressing` line names, with `expects_reply: false`: your role token; the defect (what went wrong, where, to whom, the numbers, `dispatch://<KEY>`); the repository and component where the fix belongs; the fix; and `dispatch://<REPORT KEY>/comment/<id>`.
3. File nothing and wait for nothing. The controller replies under your evidence comment with where the work went. When the report issue is missing or closed, put the evidence in the message itself. A production check's recorded result names the defect as sent to the controller.

### When no controller takes it

A publish to a role answers with its live `holder`. When no live session holds the controller role, the answer says Envoy kept the message for one hour (`kept_until`); a keep is not a delivery. An `unresponsive` holder is not a stopped one: release nothing, take nothing from it, and publish no second copy. Treat a kept answer as a controller not taking it, and make the finding an ask on the first of these that holds:

1. **The report issue is open:** ask there, the question opening `Clear defect from <your role token>` and `--ref` naming your evidence comment, then reply `Asked dispatch://<REPORT KEY>/ask/<id>` under that comment.
2. **The report issue is missing or closed:** post the evidence on your own issue and ask there, then publish the ask's reference to the architect that owns your issue; a root's architect holds its own ask. The architect holds your issue's acceptance until a controller takes the ask, since an open ask on a closed issue drops out of the controller's listing.
3. **Your own issue is closed too** (`409 ISSUE_CLOSED`): put the report issue back in reach, then post and ask there, and say in your summary that you did. This is the one lifecycle status you write, and never through the `legion` tool's `set_status`.
   - It exists and is closed: `dispatch issue-update --issue <REPORT KEY> --status icebox`, the status alone (a closed issue refuses a reopening status beside any other field).
   - None exists: `dispatch issue --project <PROJECT> --title 'Legion daily report' --spec-file -`, with the Summary `skill://legion-controller/references/daily-report.md` gives, then `dispatch issue-update --issue <NEW KEY> --status icebox`. It carries no `legion` label.

## Where it goes

An ask goes on the controller's report issue while it is open: the issue the deployment instructions name for Legion's reports, else the project's `Legion daily report` (`dispatch search --query '"Legion daily report"' --project <PROJECT>`). It stays open in `icebox`, so its asks stay answerable; a closed issue withdraws its open asks and refuses answers. When the project has no report issue yet, or a person closed it, do not create or reopen one yourself (the controller does): send the finding to the controller as text, with `envoy_publish` to the controller topic your `Legion addressing` line names (`expects_reply: false`), holding the problem, the issue it came from, the evidence and the decision it needs. The controller opens the ask. When no controller takes that publish, follow step 2 or 3 above.

## What the ask says

The question stands alone in at most 2,000 characters, the `Ref:` line `--ref` appends included. Its first line is `Clear defect from <your role token>` for a clear defect no controller took, else `Finding from <your role token>`. Then the concrete case: what went wrong, where and to whom (`dispatch://<KEY>`, the machine, file, command or session, the numbers); where the fix belongs, the repository and the component, saying plainly when it is not Legion's (a problem is not Legion's because you work in Legion: name the component that misbehaved); the decision a person must make, each option's effect and your recommendation. A clear defect carries the fix instead, with `Hand it to the owning tree` as the first, recommended option. Evidence the decision does not need goes in a comment posted first and cited by `--ref`:

```bash
dispatch comment --issue '<ASK KEY>' --body-file - <<'EOF'
<the evidence: commands, output with its time, file:line, links>
EOF
# Keep `File it for Legion` only when your Legion addressing line lists the repository where the fix belongs.
# For a clear defect no controller took, replace the `File an issue` option below with
# 'Hand it to the owning tree: <the tree, when you know it>', the first option and recommended.
dispatch ask --issue '<ASK KEY>' --ref 'dispatch://<ASK KEY>/comment/<id>' --question-file - \
  --option 'File an issue: <what it would track, and who would own it>' \
  --option 'File it for Legion: Legion works it once it is filed' \
  --option 'Do not file: <what stays as it is>' <<'EOF'
Finding from <your role token>
<what went wrong, where and to whom, with dispatch://<KEY> and the numbers; where the fix belongs; what each option does to whom; your recommendation and why>
EOF
```

Read `skill://legion-worker/references/findings-tell-the-controller.md` once the ask is open: it says how the controller learns of it, what it holds, and where you name it.
