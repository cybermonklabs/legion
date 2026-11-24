# Review threads

Read it when you reply on, list or resolve a review thread, or judge a bot's finding.
With no reviewer step, the agent of the implementation step lists unresolved bot threads before each completion, and the retro's agent does at retro.
It replies to and resolves each it fixes or declines, and records it in `Threads`.
It names each declined thread to the task's architect, by node id with its reason, in its completion summary when it implements.
The retro's agent names those it declined at retro in the Dispatch message of `skill://legion-retro` step 5, since a retro summary holds two sentences.
At retro it answers the threads it finds; a run still going may open more, so retro's message names each such run (`skill://legion-retro` step 5).

- Reply on each thread you answer, one disposition reply per thread:
  the implementer's `Fixed in <commit>: <one line>` or `Declined: <reason>`, the reviewer's reason in its own words.
  No reply resolves anything.

- Only the pull request's author can resolve its threads.
  The agent of the task's implementation step opens the pull request, as the implement App.
  So that agent resolves each thread it answered, and each bot thread the reviewer accepted and named to it by node id.
  It resolves them one call per thread, never in bulk and never a thread it has not read:

  ```bash
  gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -F id=<thread node id>
  ```

  When GitHub refuses, send the thread and GitHub's message to the architect with `envoy_publish`; never skip it.
  Where the owner does that step (`implementer:owner`), its `gh` acts as the implement App while it does that step.
  It does so at its retro too, where the task's last implementation step is its own, and resolves those threads there.

- The implementation step's agent, or at retro the retro's agent, records each thread in the PR body's `Threads` section.
  Each is stamped with the head it just pushed, or at retro with the head retro found.

- The reviewer lists the threads, replies, and names the threads it accepted by node id.
  It names them to the agent of the task's implementation step that works on next, or in its review body where the task has none.
  It reaches that agent on its own topic (its `Legion addressing` line or `dispatch steps --issue <KEY>` names it: an issue can have two implementer agents), the architect's where the step is `implementer:owner`, or names them in its review body:

  ```bash
  gh api graphql --paginate -f query='query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=<owner> -F r=<repo> -F n=<number>
  ```

  `--paginate` prints one JSON object per page; add `--slurp` for one document.

- GitHub cannot tell a CI bot from a person whose `gh` acts as an App, so accept such a thread only when its finding is settled.

- A reviewer who disagrees with a resolution replies and unresolves it.
  A finding it cannot accept becomes its own `REQUEST_CHANGES`, or at `reviewer:owner` the owner's `fail`.

- The reviewer verifies thread state in `gh api graphql`, never from the `Threads` section, and its approval never waits on a resolution.
  The merger resolves nothing.
