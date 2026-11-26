# Review threads

Reply on each thread you answer, one reply a thread: the implementer writes `Fixed in <commit>: <one line>` or `Declined: <reason>`. Only the pull request's author can resolve a thread, and the implementer opens every Legion pull request, so the implementer resolves each thread it answered, one call a thread, never in bulk:

```bash
gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -F id=<thread node id>
```

List every thread across every page. `gh` refuses `--slurp` together with `--jq` or `--template`, so pipe the slurped pages to `jq`; its filter receives an array of page objects. A failed read is not an empty list.

```bash
gh api graphql --paginate --slurp -f query='query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=<owner> -F r=<repo> -F n=<number> | jq '[.[].data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved | not)]'
```

The reviewer names to the implementer, by node id, each bot thread it accepted; the implementer resolves only those and its own.
