# A finding outside your issue

## Fix it yourself, else write to its owner, and ask only for a decision

File no issue for a problem you find. Take the first step that fits:

1. A clear defect is your tree's to fix, in whichever repository it is: in your own pull request, or in a separate pull request of your tree. The implementer makes the fix. A tester or a reviewer sends it to the implementer with its evidence. A phase worker whose own pull request cannot carry the fix sends it to the architect that owns its issue with `envoy_publish`, and that architect adds it to an open pull request or files a child.
2. Write a clear defect to its owner only when your fix would collide with another tree's open pull request, or when its repository is one that your token does not reach. When no tree owns that code, write it to the controller of the project that the repository maps to, or to your own project's controller when Dispatch maps it to none, and that controller files it. How: [Write to the owner](#write-to-the-owner).
3. A defect that needs a person's decision, a deploy window or work much larger than your own, a finding that needs a person's authority, taste or risk appetite, and a to-do that only a person may do: a `dispatch ask` with the evidence and one recommendation.

A fix you are not sure of is not step 1: take step 2 when it holds, else step 3. A problem inside your tree but outside your issue goes to your architect. A fault of Legion's own machinery that holds your phase is a finding too, never a request to another session to act on your agent or your session. A failed production check look is reported as `fail`, and the tester writes each clear defect that it found to its owner as above.

## Write to the owner

Write to the owner on Dispatch: a comment on the issue that owns the subject, which mentions the owner's role. Dispatch delivers the mention and retries it, so check no liveness first. Mention the root architect of the tree that owns the code, on its issue. When no tree owns it, mention the controller of the project that the repository maps to, on that project's report issue ([Where it goes](#where-it-goes)). Read the steps, the comment's form and what to do with each reply of the controller in `skill://legion-worker/references/findings-steps.md#write-to-the-owner` before you write.

## Another agent that needs an action

The actions on an agent (stop, retry, suspend, resume the agent, compact, and start it in a fresh session) are the project's controller's alone: neither you nor your architect can take one. When another agent needs one, ask the controller with a `dispatch comment` on that agent's issue that mentions the controller's role. Read its form in `skill://legion-worker/references/findings-steps.md#another-agent-that-needs-an-action` before you write it. Send the controller no `envoy_publish` for it. The controller replies under your comment with what it ran, or why it ran nothing. Wait for nothing.

## Where it goes

The report issue is the one that the deployment instructions name for Legion's reports, else the project's `Legion daily report` (`dispatch search --query '"Legion daily report"' --project <PROJECT>`). An ask goes there while it is open. With no open report issue, post the evidence and the ask on your own issue, and publish the ask's reference to the architect that owns your issue.
