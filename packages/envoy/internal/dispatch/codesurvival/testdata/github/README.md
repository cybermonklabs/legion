# GitHub's answers to the run's reads

Each file is one answer of GitHub's REST API to a read the code-survival run makes (`github.go`), captured on 2026-11-26 with plain `gh api -i` and no token of its own in it: the status line, the response headers and the body, as `gh` printed them. Nothing is scrubbed, since every byte is public data of cybermonklabs/legion. The tests read them through `capture` in `github_test.go`, which splits the status, the headers and the body.

| File | Command | What it holds |
| --- | --- | --- |
| `commits-page.txt` | `gh api -i 'repos/cybermonklabs/legion/commits?sha=604ff0971f8b7afd4368134ab35cd1c1648775c4&per_page=5' > commits-page.txt` | a page of the commits list holding the two-parent merge `604ff097` (parents `ec386297`, `a5b9b863`), then the side branch's `a5b9b863`, `55beb540` and `d28a0b6f`, then the first parent `ec386297`, listed before it reaches the first parent, with GitHub's `Link` header and its `X-RateLimit-*` headers |
| `pulls-page.txt` | `gh api -i 'repos/cybermonklabs/legion/pulls?state=closed&base=main&sort=updated&direction=desc&per_page=3' > pulls-page.txt` | a page of the closed pull requests list, newest update first: #5641, #4714 and #4592, each merged, with its `merge_commit_sha`, head branch, `user.login`, title and body |
| `diff-answer.txt` | `gh api -i repos/cybermonklabs/legion/commits/604ff0971f8b7afd4368134ab35cd1c1648775c4 -H 'Accept: application/vnd.github.diff' > diff-answer.txt` | the diff route's answer for `604ff097`, its headers (`Content-Type: application/vnd.github.diff`, `Content-Length: 14635`, the `X-RateLimit-*` headers) and its body, which is byte for byte `../../../delivery/survival/testdata/diffs/merge.diff` |

The `files[]` page the fallback reads is T2's `../../../delivery/survival/testdata/diffs/files-page.json`, read by its relative path and not copied here.

No refusal of a patch is known on a real repository (dispatch://INFRA-3117/artifact/feasibility-md: the diff route answered 3,356 of 3,356 commits whole), so the tests' fake GitHub makes each refusal itself.
