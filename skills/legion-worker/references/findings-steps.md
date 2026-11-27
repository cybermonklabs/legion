# The steps of a finding's routes

## Write by a mention

1. Find the issue: the issue of an open pull request on a `legion/<KEY>` branch that changes that code, or the open issue that `dispatch search` finds for it.
2. Walk its `child_of` links up to the tree's root. `dispatch read --issue <KEY>` names each parent under `Links:`.
3. Post the evidence on that issue, and mention the root's architect. `<project>` is the Legion project that runs the tree. For your own project, it is the word after `legion-` in your role token.

   ```bash
   dispatch comment --issue '<THAT KEY>' --mention 'role:legion-<project>-<root key, lower case>-architect' --body-file - <<'EOF'
   Clear defect from <your role token>
   <what went wrong, where and to whom, with the numbers; dispatch://<KEY> it came from; the repository and component; the fix; your pull request that makes it, or why you do not make it yourself>
   EOF
   ```

A production check's result names your comment, `dispatch://<KEY>/comment/<id>`.

## Send it to the controller

The message's first line is `Clear defect from <your role token>` for a clear defect, else `Finding from <your role token>`. Then it gives what went wrong, where and to whom, with the numbers; `dispatch://<KEY>` it came from; the repository and component; where the fix belongs; why you do not make it yourself; and, for a decision, the decision and your recommendation.

The controller replies with a comment on the issue that the message names, and mentions your role:

- `Filed dispatch://<KEY>`: it filed the issue.
- `Asked dispatch://<KEY>/ask/<id>`: it asked a person.
- `Not filed: tracked by dispatch://<KEY>; write to role:<token>`: write to that role as [Write by a mention](#write-by-a-mention) says, on the issue it names.
- `Not filed: it stops your work, so it is yours to fix`: fix it.
- Any other `Not filed:` reply: the controller decided. Write nothing.

Apart from that reply, file nothing and wait for nothing. A production check's result names your message to the controller.

## Another agent that needs an action

`<project>` is the word after `legion-` in your role token:

```bash
dispatch comment --issue '<KEY>' --mention 'role:legion-<project>-controller' --body-file - <<'EOF'
Action on an agent
<KEY> <role>: <the action: stop, retry, suspend, resume the agent, compact, or start it in a fresh session>, because <what you observed, with the numbers and when>
EOF
```
