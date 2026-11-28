# Captured merge queue timelines

Real output of GitHub's REST timeline API for `cybermonklabs/legion`, captured on 2026-11-26 and kept to the fields the daemon's queue timeline parser reads.

## Capture commands

```sh
gh api --paginate 'repos/cybermonklabs/legion/issues/5150/timeline?per_page=100' \
  --jq '[.[] | if .event=="added_to_merge_queue" or .event=="removed_from_merge_queue" or .event=="merged" then {id, event, created_at, url} else {event} end]' \
  | jq -s 'add' \
  > timeline-5150.json

gh api --paginate 'repos/cybermonklabs/legion/issues/4910/timeline?per_page=100' \
  --jq '[.[] | if .event=="added_to_merge_queue" or .event=="removed_from_merge_queue" or .event=="merged" then {id, event, created_at, url} else {event} end]' \
  | jq -s 'add' \
  > timeline-4910.json
```

## Files

- `timeline-5150.json`: added to the merge queue at `2026-11-23T02:27:46Z`, then merged and removed in the same second `2026-11-23T18:25:55Z`, the merge first.
- `timeline-4910.json`: added, removed, re-added two seconds later, then removed and merged three seconds after that.

Every non-queue event is cut to `event` alone so no commit message, cross-reference or user account appears in the fixture.
Each queue event keeps only `id`, `event`, `created_at` and `url`, the four fields the timeline parser reads.
