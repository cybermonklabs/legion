# A finding outside your issue

## Fix it yourself, else send it to the controller

File no issue for a problem you find. Take the first step that fits:

1. A defect that stops your work, and a clear defect, are your tree's to fix, in any repository your credentials reach: in your own pull request, or in a separate pull request of your tree. The implementer makes the fix. A tester or a reviewer sends it to the implementer with its evidence. A phase worker whose own pull request cannot carry the fix sends it to the architect that owns its issue with `envoy_publish`, and that architect adds it to an open pull request or files a child. If your fix touches files that another tree's open pull request changes, tell that tree's architect that your pull request makes the fix ([Write by a mention](#write-by-a-mention)). Do not wait for an answer.
2. A clear defect in a repository that your token does not reach, a defect that needs a person's decision, a deploy window or work much larger than your own, and a finding that needs a person's authority, taste or risk appetite: send it to the controller with `envoy_publish`, with its evidence ([Send it to the controller](#send-it-to-the-controller)). Open no `dispatch ask` for it, and post it on no report issue.

An open issue that tracks the problem is no reason to wait. Wait only when it is claimed, it is `in_progress`, and its holder said on Envoy that it works on it now. Then tell that holder what you need and by when, and fix it yourself once that time has passed. Comment what you fixed on that issue.

A to-do that only a person may do is a `dispatch ask` on your own issue. A problem inside your tree but outside your issue goes to your architect. A fault of Legion's own machinery that holds your phase is a finding too, never a request to another session to act on your agent or your session. A failed production check look is reported as `fail`, and the tester sends each defect that it found as above.

## Write by a mention

Write on Dispatch: a comment on the other tree's issue that mentions the role of its root's architect. Dispatch delivers the mention and retries it, so check no liveness first. Read the steps and the comment's form in `skill://legion-worker/references/findings-steps.md#write-by-a-mention` before you write.

## Another agent that needs an action

The actions on an agent (stop, retry, suspend, resume the agent, compact, and start it in a fresh session) are the project's controller's alone: neither you nor your architect can take one. When another agent needs one, ask the controller with a `dispatch comment` on that agent's issue that mentions the controller's role. Read its form in `skill://legion-worker/references/findings-steps.md#another-agent-that-needs-an-action` before you write it. Send the controller no `envoy_publish` for it. The controller replies under your comment with what it ran, or why it ran nothing. Wait for nothing.

## Send it to the controller

Publish it to the controller's role topic: the one that your Legion addressing line names, or `notifications.role.legion-<project>-controller` for a clear defect whose repository Dispatch maps to another Legion project. Read the message's form and what to do with each reply of the controller in `skill://legion-worker/references/findings-steps.md#send-it-to-the-controller` before you send it. The controller files a clear defect. It asks a person only for what needs that person's authority.
