# A finding outside your issue

Read it when you find a problem your issue does not cover (a defect in another component, repository or tool, a gap in Legion itself) or a production check look fails. It holds for every Legion role and its subagents, and replaces the filing step of `skill://dispatch-first`. A child's own architect opens no ask itself: it sends the finding to the architect above it, and the root's architect opens the ask.

## Fix it yourself, else write to its owner, and ask only for a decision

A finding outside your issue never becomes a `dispatch issue` you file. Take the first step that fits:

1. **Fix it yourself.** A defect with a clear fix is yours to fix, in whichever repository it is, with judgment: in your own pull request, or in a separate pull request of your tree. Say so in your handoff and the PR body. A phase worker whose own pull request cannot carry the fix sends it to the architect that owns its issue with `envoy_publish`; that architect adds it to an open pull request or files a child.
2. **Write to its owner.** A defect goes to another tree's architect only when your fix would collide with that tree's open pull request. A defect in a repository your token does not reach goes to the architect of the tree whose code it is. Only when no tree owns that code does it go to the controller of the project that repository maps to, or of your own project when Dispatch maps it to none, which files it. After the merge, the tester writes each clear defect a production check finds to its owner. How: [Write to the owner](#write-to-the-owner).
3. **Ask.** A defect goes to a person only when it needs a person's decision, a deploy window or work much larger than your own. So does a finding that needs a person's authority, taste or risk appetite (whether it is wanted, which design, who owns it, a change of scope, cost or risk). It is a `dispatch ask` with one recommendation (*What the ask says*). A human-only to-do, such as a deploy that only a person may make, is a `dispatch ask` too.

A fix you are not sure of is not step 1: take step 2 when it holds, else step 3. A problem inside your issue is fixed in your change. A phase worker's problem inside its tree but outside its issue goes to the architect its `Legion addressing` line names. A fault of Legion's own machinery that holds your phase is a finding too, never a request to another session to change your claim or your session; tell the owning architect too where a stop rule says so.

Search first (`skill://dispatch-first`). An open issue that tracks the problem gets your evidence as a comment. An open ask on the report issue about it (`dispatch open-asks --project <PROJECT>`) gets your evidence as a reply (`dispatch comment --issue <REPORT KEY> --reply-to-ask '<ask id>' --body-file -`); open no second one.

## Write to the owner

Write to the owner on Dispatch: a comment on the issue that owns the subject, which mentions the owner's role. Dispatch delivers the mention and retries it, so check no liveness first. The controller forwards no message from one agent to another, and it edits no repository, so a defect it finds goes to its owner this way too.

1. Find the owning issue: the issue of an open pull request on a `legion/<KEY>` branch that changes that code, or the open issue `dispatch search` finds for it.
2. Walk its `child_of` links up to the tree's root: `dispatch read --issue <KEY>` names each parent under `Links:`.
3. Post the evidence on the owning issue, and mention the root's architect. `<project>` is the Legion project that runs the tree; for your own project it is the word after `legion-` in your role token.

   ```bash
   dispatch comment --issue '<owning KEY>' --mention 'role:legion-<project>-<root key, lower case>-architect' --body-file - <<'EOF'
   Clear defect from <your role token>
   <what went wrong, where and to whom, with the numbers; dispatch://<KEY> it came from; the repository and component; the fix; why you do not make it yourself>
   EOF
   ```

When no tree owns the code, post the same comment on that project's report issue (*Where it goes*; a closed one takes a comment too, and with none, your own issue), with `--mention 'role:legion-<project>-controller'`. The controller replies under it and mentions your role. `Filed dispatch://<KEY>` names the issue it filed. On `Not filed: dispatch://<ROOT> owns it; write to role:<token>` or `Not filed: tracked by dispatch://<KEY>; write to role:<token>`, write the same comment on the issue it names, mentioning that role. On `Not filed: dispatch://<KEY> owns it; role:<token> is mentioned here`, the owner has it: write nothing. On any other `Not filed:` reply, open the ask yourself (step 3).

Apart from that reply, file nothing and wait for nothing. A production check's result names your comment, `dispatch://<KEY>/comment/<id>`.

## Where it goes

An ask goes on the controller's report issue while it is open: the issue the deployment instructions name for Legion's reports, else the project's `Legion daily report` (`dispatch search --query '"Legion daily report"' --project <PROJECT>`). It stays open in `icebox`, so its asks stay answerable; a closed issue withdraws its open asks and refuses answers. With no open report issue, open the ask on the first of these that holds:

1. **Your own issue is open:** post the evidence and ask there, then publish the ask's reference to the architect that owns your issue. It holds your issue's acceptance until a controller takes the ask, since an open ask on a closed issue drops out of the controller's listing.
2. **Your own issue is closed too** (`409 ISSUE_CLOSED`): put the report issue back in reach, then post and ask there, and say in your summary that you did. This is the one lifecycle status you write, and never through the `legion` tool's `set_status`.
   - It exists and is closed: `dispatch issue-update --issue <REPORT KEY> --status icebox`, the status alone (a closed issue refuses a reopening status beside any other field).
   - None exists: `dispatch issue --project <PROJECT> --title 'Legion daily report' --spec-file -`, with the Summary `skill://legion-controller/references/daily-report.md` gives, then `dispatch issue-update --issue <NEW KEY> --status icebox`. It carries no `legion` label.

## What the ask says

The question stands alone in at most 2,000 characters, the `Ref:` line `--ref` appends included. Its first line is `Finding from <your role token>`. Then the concrete case: what went wrong, where and to whom (`dispatch://<KEY>`, the machine, file, command or session, the numbers); where the fix belongs, saying plainly when it is not Legion's; the decision a person must make, each option's effect and your recommendation. Evidence the decision does not need goes in a comment posted first and cited by `--ref`:

```bash
dispatch comment --issue '<ASK KEY>' --body-file - <<'EOF'
<the evidence: commands, output with its time, file:line, links>
EOF
# Keep `File it for Legion` only when both role GitHub Apps are installed on the repository where the fix belongs.
dispatch ask --issue '<ASK KEY>' --ref 'dispatch://<ASK KEY>/comment/<id>' --question-file - \
  --option 'File an issue: <what it would track, and who would own it>' \
  --option 'File it for Legion: Legion works it once it is filed' \
  --option 'Do not file: <what stays as it is>' <<'EOF'
Finding from <your role token>
<what went wrong, where and to whom, with dispatch://<KEY> and the numbers; where the fix belongs; what each option does to whom; your recommendation and why>
EOF
```

## Tell the controller about the ask

The answer reaches only the ask's followers. Once the ask is open, `envoy_publish` `dispatch://<REPORT KEY>/ask/<id>` to the controller topic with `expects_reply: false`; the controller files what the answer asks for, and you file nothing. When the listener only kept that publish, do nothing more for an ask on the report issue: the controller's catch-up finds it. An ask on the report issue blocks nothing of your own issue; one on your own issue holds its acceptance. A production check look names the comment or the ask in its result and summary.
