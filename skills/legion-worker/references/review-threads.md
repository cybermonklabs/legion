# Review threads

Read it when you reply on, list or resolve a review thread, or judge a bot's finding: the implementer after every push that answers a review, and the reviewer on every re-review.

- Reply on each thread you answer, one disposition reply per thread: the implementer's `Fixed in <commit>: <one line>` or `Declined: <reason>`, the reviewer's reason in its own words. No reply resolves anything.
- Only the pull request's author can resolve its threads, and the implementer opens every Legion pull request. So the implementer, after every push that answers a review and before its `handoff_complete`, resolves each thread it answered. It resolves each bot thread the reviewer accepted and named to it by node id as soon as the message reaches it, or as part of answering that review. One call per thread, never in bulk, never a thread it has not read, and never one the reviewer did not name:

  ```bash
  gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -F id=<thread node id>
  ```

  When GitHub refuses, send the thread and GitHub's message to the architect with `envoy_publish`; never skip it.
- The implementer records each thread in the PR body's `Threads` section, stamped with the head it just pushed.
- The reviewer lists the threads, replies, and names to the implementer by node id exactly the threads it accepted, never one it has not read: on the implementer's role topic (the reviewer's own topic from its `Legion addressing` line, with the trailing `-reviewer` replaced by `-implementer`), or in its review body, which the implementer reads at its next round:

  ```bash
  gh api graphql --paginate -f query='query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=<owner> -F r=<repo> -F n=<number>
  ```

  `--paginate` prints one JSON object per page; add `--slurp` for one document.
- GitHub cannot tell a CI bot from a person whose `gh` acts as an App, so such a thread may be a person's finding: accept it only when you judged its finding settled: fixed at the head, or not a defect (a genuine Minor is settled by being judged Minor), since the resolution says you judged it.
- A reviewer who disagrees with a resolution replies and unresolves it; a finding it cannot accept becomes its own `REQUEST_CHANGES`.
- The reviewer verifies thread state in `gh api graphql`, never from the `Threads` section, and its approval never waits on a resolution. The merger resolves nothing.

## The reviewer, on a re-review

When you re-review after a corrective push, answer every thread you opened, and every thread a bot opened that is none of Legion's role Apps, with one reply in your own words. A bot's finding you cannot accept becomes your own: say what stands and request changes. Approve once each of those threads carries your answer and no finding stands, read in `gh api graphql`, and quote those answers in the approval. Your approval never waits on a resolution: resolution is the pull request author's.

For a review bot that is red on its open threads, you re-run nothing: that red moves the issue nowhere, but the base branch holds the merge for a check it requires. So the implementer answers or fixes each of the bot's threads, resolves them with its own `gh` (the ones you named to it, and its own), and pushes, and the bot runs again on that push.
