---
title: "Put jq flags inside the filter for the pane's gh wrapper"
category: legion
tags:
  - gh
  - jq
  - proof
  - tooling
date: 2026-10-23
status: active
module: smoke-proof
related_issues:
  - "LEGSMOKE-1070"
  - "sjawhar/legion-smoke#767"
---

# Put jq flags inside the filter for the pane's gh wrapper

Never write `gh api <endpoint> --jq -c '<filter>'` (or any other jq flag between `--jq` and the
filter) in a Legion worker pane. The pane's `gh` is Legion's wrapper, which reads the command
before it picks a token: it takes `-c` as `--jq`'s value, sees the filter as a positional
argument `gh api` has no place for, refuses with exit 2 and sends no request.

Get compact or single-line output from the filter itself: `--jq '<filter> | tostring'` for a
compact JSON string, `--jq '<filter> | @tsv'` or `| @json` for one line per row. Plain gh's `-q`
short flag with a filter works too; only a jq flag between `--jq` and the filter breaks.

A refusal whose text names your filter as the stray argument ("… is an argument `gh api`'s usage
has no place for, so it sends no token") is this mistake, not a missing repository: the endpoint
already names `repos/<owner>/<repo>/…`.

## Evidence

Implementing phase of LEGSMOKE-1070 (pull request sjawhar/legion-smoke#767), 2026-10-23 17:43Z,
reading `legionDiff` at the code head:

```
gh api "repos/sjawhar/legion-smoke/compare/50702e97...69ba4779" --jq -c '[.files[] | select(.filename | startswith(".legion/") | not) | {filename,status,additions,deletions}]'
legion gh: cannot tell which repository the command targets: "[.files[] | …]" is an argument `gh api`'s usage has no place for, so it sends no token; name it in the endpoint, repos/<owner>/<repo>/…
exit 2
```

The same read with `--jq '[…] | tostring'` returned
`[{"additions":1,"deletions":0,"filename":"smoke/LEGSMOKE-1070.md","status":"added"}]`. Reproduced
at retro on a harmless endpoint: `gh api repos/sjawhar/legion-smoke --jq -c '.name'` → the same
refusal, exit 2; `gh api repos/sjawhar/legion-smoke --jq '[.name,.default_branch] | tostring'` →
`["legion-smoke","main"]`; `gh api repos/sjawhar/legion-smoke -q '.name'` → `legion-smoke`.
