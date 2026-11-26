# The steps of a finding's routes

## Write to the owner

1. Find the owning issue: the issue of an open pull request on a `legion/<KEY>` branch that changes that code, or the open issue that `dispatch search` finds for it.
2. Walk its `child_of` links up to the tree's root. `dispatch read --issue <KEY>` names each parent under `Links:`.
3. Post the evidence on the owning issue, and mention the root's architect. `<project>` is the Legion project that runs the tree. For your own project, it is the word after `legion-` in your role token.

   ```bash
   dispatch comment --issue '<owning KEY>' --mention 'role:legion-<project>-<root key, lower case>-architect' --body-file - <<'EOF'
   Clear defect from <your role token>
   <what went wrong, where and to whom, with the numbers; dispatch://<KEY> it came from; the repository and component; the fix; why you do not make it yourself>
   EOF
   ```

A production check's result names your comment, `dispatch://<KEY>/comment/<id>`.

## Send it to the controller

The message's first line is `Clear defect from <your role token>` for a clear defect that no tree owns, else `Finding from <your role token>`. Then it gives what went wrong, where and to whom, with the numbers; `dispatch://<KEY>` it came from; the repository and component; where the fix belongs; why you do not make it yourself; and, for a decision, the decision and your recommendation.

The controller replies with a comment on the issue that the message names, and mentions your role:

- `Filed dispatch://<KEY>`: it filed the issue.
- `Asked dispatch://<KEY>/ask/<id>`: it asked a person.
- `Not filed: dispatch://<ROOT> owns it; write to role:<token>` or `Not filed: tracked by dispatch://<KEY>; write to role:<token>`: write to that owner as [Write to the owner](#write-to-the-owner) says, on the issue it names, and mention that role.
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
