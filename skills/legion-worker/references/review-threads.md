# Review threads

Read it when you reply on, list or resolve a review thread, or judge a bot's finding.
With no reviewer step, the implementer lists unresolved bot threads before each completion, including retro;
it replies to and resolves each it fixes, declines or, at retro, reports with `report_defect`, and records it in `Threads`.
It names each declined thread to the task's architect, by node id with its reason, in its completion summary when it implements.
It names them at retro in the Dispatch message of `skill://legion-retro` step 5, since a retro summary holds two sentences.
At retro it answers the threads it finds; a run still going may open more, so retro's message names each such run (`skill://legion-retro` step 5).

- Reply on each thread you answer, one disposition reply per thread:
  the implementer's `Fixed in <commit>: <one line>`, `Declined: <reason>` or, at retro, `Reported as defect <id>: <one line>`, and the reviewer's reason in its own words.
  No reply resolves anything.

- Only the pull request's author can resolve its threads, and the implementer opens every Legion pull request.
  So the implementer resolves each thread it answered, and each bot thread the reviewer accepted and named to it by node id, one call per thread, never in bulk and never a thread it has not read:

  ```bash
  gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -F id=<thread node id>
  ```

  When GitHub refuses, send the thread and GitHub's message to the architect with `envoy_publish`; never skip it.

- The implementer records each thread in the PR body's `Threads` section, stamped with the head it just pushed, or at retro with the head retro found.

- The reviewer lists the threads, replies, and names the threads it accepted to the implementer by node id (on the implementer's role topic, or in its review body):

  ```bash
  gh api graphql --paginate -f query='query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=<owner> -F r=<repo> -F n=<number>
  ```

  `--paginate` prints one JSON object per page; add `--slurp` for one document.

- GitHub cannot tell a CI bot from a person whose `gh` acts as an App, so accept such a thread only when its finding is settled.

- A reviewer who disagrees with a resolution replies and unresolves it; a finding it cannot accept becomes its own `REQUEST_CHANGES`.

- The reviewer verifies thread state in `gh api graphql`, never from the `Threads` section, and its approval never waits on a resolution.
  The merger resolves nothing.
