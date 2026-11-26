gh api graphql --paginate -f query='query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:1,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=cybermonklabs -F r=legion -F n=5596
# captured 2026-11-26T10:25:43Z with gh version 2.98.0 (2026-08-20)
