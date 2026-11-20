# Review threads

Read it when you reply on, list or resolve a review thread, or judge a bot's finding.

- Reply on each thread you answer, one disposition reply per thread: the implementer's `Fixed in <commit>: <one line>` or `Declined: <reason>`, the reviewer's reason in its own words. No reply resolves anything.
- The reviewer resolves each thread it opened and each bot thread it accepted, once it has read the answer: one call per thread, never in bulk and never a thread it has not read. The implementer resolves none:

  ```bash
  gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -F id=<thread node id>
  ```

  When GitHub refuses, send the thread and GitHub's message to the architect with `envoy_publish`; never skip it.
- The implementer records each thread in the PR body's `Threads` section, stamped with the head it just pushed.
- The reviewer lists the threads:

  ```bash
  gh api graphql --paginate -f query='query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=<owner> -F r=<repo> -F n=<number>
  ```

  `--paginate` prints one JSON object per page; add `--slurp` for one document.
- GitHub cannot tell a CI bot from a person whose `gh` acts as an App, so accept such a thread only when its finding is settled.
- A reviewer who disagrees with a resolution replies and unresolves it; a behavioural finding it cannot accept becomes its own `REQUEST_CHANGES`.
- The reviewer verifies thread state in `gh api graphql`, never from the `Threads` section, and its approval never waits on a resolution. The merger resolves nothing.
