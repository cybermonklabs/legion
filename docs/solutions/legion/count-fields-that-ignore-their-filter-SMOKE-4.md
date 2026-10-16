---
title: "Count fields that ignore their own filter"
category: legion
tags:
  - proof
  - negative-control
  - github-api
  - graphql
  - auto-merge
date: 2026-10-16
status: active
module: legion-worker
related_issues:
  - "SMOKE-4"
  - "sjawhar/legion-smoke#603"
---

# Count fields that ignore their own filter

Extends docs/solutions/legion/proving-facts-the-event-erases-SMOKE-3.md.

That document requires a filter-sensitivity control beside a claim of absence. It assumes the
control validates the read it sits next to. The delta: **run the control on the literal
expression your evidence line quotes, and quote a list of rows rather than a count.** An
aggregate served beside a filtered list may be computed before the filter, in which case it
confirms every filter you hand it and discriminates nothing.

- Prove an absence from the **returned rows** — a node list, or a count `jq` computes after its
  own `select` — never from a count the server computes.
- When an API offers both, the control is the identical query with the filter changed to
  something present. Equal counts there mean the count is not reading your filter; different
  node lists mean the list is.
- GitHub's GraphQL `pullRequest.timelineItems(first:…, itemTypes:[…])` is one of these:
  `totalCount` ignores `itemTypes` and reports the whole timeline. `nodes` honours it.
- GitHub's REST timeline has no such hazard, because the filter is yours:
  `--jq '[.[]|select(.event=="auto_merge_enabled")]|length'` counts the rows that survived.
  That is the form to quote.
- A control must also discriminate the dimension it is sold as. A filter you never varied is a
  filter you never tested: vary it and record the different answer.

## Evidence

SMOKE-4 (`sjawhar/legion-smoke#603`) had to prove that GitHub auto-merge was never armed on the
pull request. Read at 2026-10-16T21:40:02Z, with 7 timeline items on the pull request:

```
gh api graphql -f query='query{repository(owner:"sjawhar",name:"legion-smoke"){pullRequest(number:603){
  timelineItems(first:100,itemTypes:[AUTO_MERGE_ENABLED_EVENT,AUTO_MERGE_DISABLED_EVENT]){totalCount nodes{__typename}}}}}'
→ {"totalCount":7,"nodes":[]}

  …itemTypes:[PULL_REQUEST_COMMIT]  → {"totalCount":7,"nodes":[6 × PullRequestCommit]}
  …no itemTypes at all              → {"totalCount":7}
```

The same `7` for the auto-merge filter, for a filter matching 6 rows, and for no filter: it is
the unfiltered total every time. A phase that had quoted `totalCount: 7` as "the auto-merge
filter found rows" or a `0` from it as "none exist" would have quoted a number that proves
nothing either way. The node lists do discriminate: `[]` against 6 `PullRequestCommit` nodes.
The tester met this first at 4 items, where both filters answered `4`; the number had grown to
`7` by retro while still matching the unfiltered total, which is what identifies it as the
unfiltered count rather than a coincidence.

The claim stood because every phase's evidence line quoted node lists and REST rows:
`gh api "repos/sjawhar/legion-smoke/issues/603/timeline?per_page=100" --paginate --jq
'[.[]|select(.event=="auto_merge_enabled")]|length'` → `0`, with the sensitivity control
`select(.event=="committed")|length` → non-zero beside it, and the GraphQL `nodes` → `[]`.

The varying-control rule comes from the same run's weakest control. Line 2 of the record names
its own pull request number, defended by
`gh api 'repos/sjawhar/legion-smoke/pulls?state=all&head=sjawhar:legion/SMOKE-4' --jq '[.[].number]'`
→ `[603]`. No phase varied `head=`, so nothing showed the parameter was read at all; a server
ignoring it and returning the newest pull request would have passed. Varied at retro:
`head=sjawhar:legion/SMOKE-3` → `[599]`, `head=sjawhar:legion/NOPE` → `[]`. Three distinct
answers, so the filter discriminates — one extra call, run two phases later than it should have
been.
