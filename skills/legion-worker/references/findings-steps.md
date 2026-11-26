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

When no tree owns the code, post the same comment on that project's report issue, with `--mention 'role:legion-<project>-controller'`. A closed report issue takes the comment too. With none, post it on your own issue. The controller replies under it, and mentions your role:

- `Filed dispatch://<KEY>`: it filed the issue.
- `Not filed: dispatch://<ROOT> owns it; write to role:<token>` or `Not filed: tracked by dispatch://<KEY>; write to role:<token>`: write the same comment on the issue it names, and mention that role.
- `Not filed: dispatch://<KEY> owns it; role:<token> is mentioned here`: the owner has it. Write nothing.
- Any other `Not filed:` reply: open the ask yourself (step 3 of `skill://legion-worker/references/findings.md`).

Apart from that reply, file nothing and wait for nothing. A production check's result names your comment, `dispatch://<KEY>/comment/<id>`.

## Another agent that needs an action

`<project>` is the word after `legion-` in your role token:

```bash
dispatch comment --issue '<KEY>' --mention 'role:legion-<project>-controller' --body-file - <<'EOF'
Action on an agent
<KEY> <role>: <the action: stop, retry, suspend, resume the agent, compact, or start it in a fresh session>, because <what you observed, with the numbers and when>
EOF
```
