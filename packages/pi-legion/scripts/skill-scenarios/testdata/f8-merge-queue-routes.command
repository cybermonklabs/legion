f8-merge-queue-routes.json is the `routes` of a gh-standin fixtures.json for a merger's READY world: a pull request open on a merge-queue base, mergeable (mergeStateStatus CLEAN), not in the queue. Each route's `stdout` is what real gh 2.98.0 printed, captured on 2026-11-26 against the public cybermonklabs/legion repository (whose `main` has a merge_queue rule) and its open pull request #5676 (the watch's and the id's), with these commands (read-only GraphQL queries and REST reads):

watch (merger.md step 5), with number=5676:
gh api graphql -F number=5676 -f owner=cybermonklabs -f name=legion -f query='query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){state mergedAt isInMergeQueue mergeStateStatus headRefOid mergeQueueEntry{state position}}}}'

timeline (merger.md step 5), with number=5676, which has no queue event; the same query on merged pull request #5596 answers an AddedToMergeQueueEvent (actor login, createdAt) then a RemovedFromMergeQueueEvent (reason "merged", actor login, createdAt, beforeCommit{oid}) under the same data.repository.pullRequest.timelineItems.nodes path:
gh api graphql -F number=5676 -f owner=cybermonklabs -f name=legion -f query='query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){timelineItems(itemTypes:[ADDED_TO_MERGE_QUEUE_EVENT,REMOVED_FROM_MERGE_QUEUE_EVENT],last:20){nodes{__typename ... on AddedToMergeQueueEvent{createdAt actor{login}} ... on RemovedFromMergeQueueEvent{createdAt reason actor{login} beforeCommit{oid}}}}}}}'

pull request id (merger.md step 4), the id member of `gh pr view 5676 -R cybermonklabs/legion --json id -q .id`'s answer:
gh pr view 5676 -R cybermonklabs/legion --json id -q .id

base branch rules (merger.md step 2), raw, without the --jq:
gh api --paginate repos/cybermonklabs/legion/rules/branches/main

repository settings (merger.md step 2), the four members its --jq reads:
gh api repos/cybermonklabs/legion --jq '{default_branch, allow_merge_commit, squash_merge_commit_title, squash_merge_commit_message}'

Not captured: the enqueuePullRequest and dequeuePullRequest answers. Each is a write to GitHub, which this file's author made none of, so each is constructed from the GraphQL schema (`mergeQueueEntry{position}` of the payload, position 1: the pull request alone in the queue).
