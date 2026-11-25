# Review threads

With no reviewer step, the implementer lists unresolved bot threads before each completion, including retro;
it replies to and resolves each it fixes or declines, and records it in `Threads`.

- The implementer records each thread in the PR body's `Threads` section, stamped with the head it just pushed, or at retro with the head retro found.

- The reviewer lists the threads, replies, and names the threads it accepted to the implementer by node id (on the implementer's role topic, or in its review body):

  ```bash
  gh api graphql --paginate -f query='query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=<owner> -F r=<repo> -F n=<number>
  ```

  `--paginate` prints one JSON object per page; add `--slurp` for one document.
