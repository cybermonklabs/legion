# A finding outside your issue

Read it when you find a problem your issue does not cover (a defect in another component, repository or tool, a gap in Legion itself) or a production check look fails. It holds for every Legion role and its subagents, and replaces the filing step of `skill://dispatch-first`. A child's own architect opens no ask itself: it sends the finding to the architect above it, and the root's architect opens the ask.

## Fix it where you are, else hand it over, and ask only for a decision

A finding outside your issue never becomes a `dispatch issue` you file, except the child of step 1. Take the first step that fits:

1. **Fix it here.** A defect that stops your work, and a clear defect in code your tree touches or depends on, are your tree's to fix, in whatever repository the fix belongs. You may fix it in any repository your credentials reach: no tree, team or agent owns code. An open issue that tracks the defect is no reason to wait for it (the *Search first* paragraph below). A fix in the repository of your tree's pull request goes into that pull request. A fix in another repository goes into a pull request of your tree there: a child your architect files in that repository (`skill://legion-architect/references/decomposition.md`). Say so in the PR body. If your fix touches files that another tree's open pull request changes, tell that tree's architect in a comment on the issue of that pull request that mentions its role (`--mention-role legion-<project>-<that issue's key, lower case>-architect`), and go on: do not wait for an answer.
2. **Hand it over.** Any other clear defect goes to the controller with its evidence (*Send a finding to the controller*). It puts it on the open issue that tracks it, hands it to a running tree whose pull request can carry the fix, or files a Legion root.
3. **Ask.** A finding that needs a person's authority, taste or risk appetite (whether it is wanted, which design, a change of scope, cost or risk) is a `dispatch ask` with one recommendation (*What the ask says*).

A defect that stops your work is yours to investigate until you have its fix; any other fix you are not sure of is not step 1. A problem inside your issue is fixed in your change. A phase worker's problem inside its tree but outside its issue goes to the architect its `Legion addressing` line names. A production check look's issue is closed, so its clear defect takes step 2.

Search first (`skill://dispatch-first`). An open issue that tracks the problem gets your evidence, or what you fixed, as a comment. It is no reason to wait: wait only when it is claimed, it is `in_progress`, and its holder said on Envoy that it works on it now. Then tell that holder what you need and by when, and fix it yourself once that time has passed. An open finding ask about it (`dispatch open-asks --project <PROJECT>`) gets your evidence as a reply (`dispatch comment --issue <the ask's issue> --reply-to-ask '<ask id>' --body-file -`); open no second one.

## Send a finding to the controller

1. Find the report issue: the issue the deployment instructions name for Legion's reports, else the project's `Legion daily report` (`dispatch search --query '"Legion daily report"' --project <PROJECT>`). Post the evidence on it as a `dispatch comment`, its first line exactly `Clear defect from <your role token>`. It prints `Posted comment <id>`.
2. `envoy_publish` one message to the controller topic your `Legion addressing` line names, with `expects_reply: false`: your role token; the defect (what went wrong, where, to whom, the numbers, `dispatch://<KEY>`); the repository and component where the fix belongs; the fix; and `dispatch://<REPORT KEY>/comment/<id>`.
3. File nothing and wait for nothing. The controller replies under your evidence comment with where the work went. When no report issue exists, put the evidence in the message itself.

## What the ask says

The question stands alone in at most 2,000 characters, the `Ref:` line `--ref` appends included. Its first line is `Finding from <your role token>`. Then the concrete case: what went wrong, where and to whom (`dispatch://<KEY>`, the machine, file, command or session, the numbers); where the fix belongs, saying plainly when it is not Legion's; the decision a person must make, each option's effect and your recommendation. Evidence the decision does not need goes in a comment posted first and cited by `--ref`:

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

Once the ask is open, `envoy_publish` its reference `dispatch://<ASK KEY>/ask/<id>` to the controller topic with `expects_reply: false`; the controller files what the answer asks for, and you file nothing.
