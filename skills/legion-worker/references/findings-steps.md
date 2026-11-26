# The steps of a finding's routes

Read this when `skill://legion-worker/references/findings.md` sends you here for a route's steps; that file states the rule each section here carries out.

## Write by a mention

1. Find the issue: the issue of an open pull request on a `legion/<KEY>` branch that changes that code, or the open issue `dispatch search` finds for it.
2. Walk its `child_of` links up to the tree's root: `dispatch read --issue <KEY>` names each parent under `Links:`.
3. Post the evidence on that issue, and mention the root's architect. `<project>` is the Legion project that runs the tree; for your own project it is the word after `legion-` in your role token.

   ```bash
   dispatch comment --issue '<KEY>' --mention 'role:legion-<project>-<root key, lower case>-architect' --body-file - <<'EOF'
   Clear defect from <your role token>
   <what went wrong, where and to whom, with the numbers; dispatch://<KEY> it came from; the repository and component; the fix>
   EOF
   ```

For the controller, post the same comment on that project's report issue (the issue the deployment instructions name for Legion's reports, else the project's `Legion daily report`; a closed one takes a comment too, and with none, your own issue), with `--mention 'role:legion-<project>-controller'`. The controller replies under it and mentions your role. `Filed dispatch://<KEY>` names the issue it filed. On `Not filed: dispatch://<ROOT>; write to role:<token>` or `Not filed: tracked by dispatch://<KEY>; write to role:<token>`, write the same comment on the issue it names, mentioning that role. On `Not filed: dispatch://<KEY>; role:<token> is mentioned here`, write nothing. On `Not filed: it stops your work, so it is yours to fix`, fix it. On any other `Not filed:` reply, open the ask yourself (`skill://legion-worker/references/findings.md`, step 3 of its first section).

Apart from that reply, file nothing and wait for nothing. A production check's result names your comment, `dispatch://<KEY>/comment/<id>`.

## Another agent that needs an action

`<project>` is the word after `legion-` in your role token. Name the agent by its issue and role, the action in plain words, and why, with what you observed:

```bash
dispatch comment --issue '<KEY>' --mention 'role:legion-<project>-controller' --body-file - <<'EOF'
Action on an agent
<KEY> <role>: <the action: stop, retry, suspend, resume the agent, compact, or start it in a fresh session>, because <what you observed, with the numbers and when>
EOF
```
