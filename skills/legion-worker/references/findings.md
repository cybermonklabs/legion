# A finding outside your issue

Read it when you find a problem your issue does not cover (a defect in another component, repository or tool, a gap in Legion itself), when another agent needs an action, or when a production check look fails. It holds for every Legion role and its subagents, and replaces the filing step of `skill://dispatch-first`. A child's own architect opens no ask itself: it sends the finding to the architect above it, and the root's architect opens the ask.

## Fix it yourself, else write to the controller, and ask only for a decision

A finding outside your issue never becomes a `dispatch issue` you file. Take the first step that fits:

1. **Fix it yourself.** A defect that stops your work, and a defect with a clear fix, are your tree's to fix, in any repository your credentials reach, with judgment: in your own pull request, or in a separate pull request of your tree. The implementer makes the fix, and a tester or a reviewer sends it to the implementer with its evidence (a tester with its red test, the reviewer in its review). Say so in the PR body. A phase worker whose own pull request cannot carry the fix sends it to the architect that owns its issue with `envoy_publish`; that architect adds it to an open pull request or files a child. If your fix touches files that another tree's open pull request changes, tell that tree's architect in a comment on the issue of that pull request, which mentions its role (`--mention 'role:legion-<its project>-<its root's key, lower case>-architect'`), names your pull request and says that you make the fix, and go on: do not wait for an answer.
2. **Write to the controller.** A defect in a repository your token does not reach goes to the controller of the project that repository maps to, or of your own project when Dispatch maps it to none, which files it. After the merge, the tester writes each clear defect a production check finds to that controller. How: [Write by a mention](#write-by-a-mention).
3. **Ask.** A defect goes to a person only when it needs a person's decision, a deploy window or work much larger than your own. So does a finding that needs a person's authority, taste or risk appetite (whether it is wanted, which design, a change of scope, cost or risk). It is a `dispatch ask`, with one recommendation where its options are real choices (*What the ask says*).

A human-only to-do, such as a deploy that only a person may make, is a `dispatch ask` too.

A problem inside your issue is fixed in your change. A phase worker's problem inside its tree but outside its issue goes to the architect its `Legion addressing` line names. A fault of Legion's own machinery that holds your phase is a finding too, never a request to another session to act on your agent or your session; tell the owning architect too where a stop rule says so.

Search first (`skill://dispatch-first`). An open issue that tracks the problem gets your evidence, or what you fixed, as a comment. It is no reason to wait: wait only when it is claimed, it is `in_progress`, and its holder said on Envoy that it works on it now. Then tell that holder what you need and by when, and fix it yourself once that time has passed. An open ask about it (`dispatch open-asks --project <PROJECT>`) gets your evidence as a reply (`dispatch comment --issue <the ask's issue> --reply-to-ask '<ask id>' --body-file -`); open no second one.

## Write by a mention

Write on Dispatch: a comment that mentions a role. Dispatch delivers the mention and retries it, so check no liveness first. The controller forwards no message from one agent to another. Mention the controller of the project the repository maps to on that project's report issue, or, where a rule sends the defect to another tree, that tree's root architect on its issue. The steps, the comment's form and what to do with each reply the controller writes are in `skill://legion-worker/references/findings-steps.md#write-by-a-mention`: read them before you write.

## Another agent that needs an action

The actions on an agent (stop, retry, suspend, resume the agent, compact, and start it in a fresh session) are the project's controller's alone: neither you nor your architect can take one. When another agent needs one, ask the controller with a `dispatch comment` on that agent's issue, `<KEY>`, that mentions the controller's role. Read its form in `skill://legion-worker/references/findings-steps.md#another-agent-that-needs-an-action` before you write the comment. Send the controller no `envoy_publish` for it: Dispatch delivers the mention and retries it. The controller replies under your comment with what it ran, or why it ran nothing; wait for nothing.

## What the ask says

The question stands alone in at most 2,000 characters, the `Ref:` line `--ref` appends included. Its first line is `Finding from <your claim token>`. Then the concrete case: what went wrong, where and to whom (`dispatch://<KEY>`, the machine, file, command or session, the numbers); where the fix belongs, saying plainly when it is not Legion's; the decision a person must make, each option's effect, and your recommendation with its reason where the options are real choices (`skill://dispatch/SKILL.md#asks-and-ask-blocks`). Offer only the real choices. Evidence the decision does not need goes in a comment posted first and cited by `--ref`:

```bash
dispatch comment --issue '<ASK KEY>' --body-file - <<'EOF'
<the evidence: commands, output with its time, file:line, links>
EOF
# Keep `File it for Legion` only when both role GitHub Apps are installed on the repository where the fix belongs.
dispatch ask --issue '<ASK KEY>' --ref 'dispatch://<ASK KEY>/comment/<id>' --question-file - \
  --option 'File an issue: <what it would track, and who would work it>' \
  --option 'File it for Legion: Legion works it once it is filed' \
  --option 'Do not file: <what stays as it is, and what that costs>' <<'EOF'
Finding from <your claim token>
<what went wrong, where and to whom, with dispatch://<KEY> and the numbers; where the fix belongs; what each option does to whom; where the options are real choices, your recommendation and why>
EOF
```

## Tell the controller about the ask

The answer reaches only the ask's followers. Once the ask is open, `envoy_publish` `dispatch://<ASK KEY>/ask/<id>` to the controller topic with `expects_reply: false`; the controller files what the answer asks for, and you file nothing. When the listener only kept that publish, do nothing more: the controller's catch-up finds it. An ask on your own issue holds its acceptance. A production check look names the comment or the ask in its result and summary.
