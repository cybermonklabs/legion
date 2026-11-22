GitHub's answers to the requests the rig answers with an error, each as gh 2.98.0 (2026-08-20)
printed it on 2026-11-18 against api.github.com, as the legion-implementer App on
cybermonklabs/legion: <name>.stdout is the body GitHub sent and <name>.stderr gh's line for it,
byte for byte; gh exited 1 for each. The request each answered:
branch-404      gh api repos/cybermonklabs/legion/branches/no-such-branch-infra-1303
commit-422      gh api repos/cybermonklabs/legion/commits/1111111111111111111111111111111111111111
checkruns-422   gh api repos/cybermonklabs/legion/commits/1111...1111/check-runs
suites-422      gh api repos/cybermonklabs/legion/commits/1111...1111/check-suites
noroute-404     gh api repos/cybermonklabs/legion/runs/1   (no such endpoint; CRLF pretty body)
checkruns-list-404     gh api repos/cybermonklabs/legion/check-runs   (exit 1)
joblog-410      gh api repos/cybermonklabs/legion/actions/jobs/90713847067/logs   (job past log retention)
repo-404-pulls  gh api repos/cybermonklabs/no-such-repo-infra-1303/pulls/1
repo-404-checkruns gh api repos/cybermonklabs/no-such-repo-infra-1303/commits/abc/check-runs
repo-404-runs          gh api repos/cybermonklabs/no-such-repo-infra-1303/actions/runs/1   (exit 1)
repo-404-branch        gh api repos/cybermonklabs/no-such-repo-infra-1303/branches/main   (exit 1)
pull-404        gh api repos/cybermonklabs/legion/pulls/999999
reviews-404     gh api repos/cybermonklabs/legion/pulls/999999/reviews
comments-404    gh api repos/cybermonklabs/legion/issues/999999/comments
run-404         gh api repos/cybermonklabs/legion/actions/runs/1
job-404         gh api repos/cybermonklabs/legion/actions/jobs/1
rerun-404       gh api -X POST repos/cybermonklabs/legion/actions/runs/1/rerun-failed-jobs
checkrun-404    gh api repos/cybermonklabs/legion/check-runs/1
gitref-404             gh api repos/cybermonklabs/legion/git/ref/heads/no-such-branch-infra-1303   (exit 1)
gitrefs-404            gh api repos/cybermonklabs/legion/git/refs/heads/no-such-branch-infra-1303   (exit 1)
annotations-404        gh api repos/cybermonklabs/legion/check-runs/1/annotations   (exit 1)
attempt-404            gh api repos/cybermonklabs/legion/actions/runs/44366233978/attempts/99   (exit 1)
attempt-jobs-404       gh api repos/cybermonklabs/legion/actions/runs/44366233978/attempts/99/jobs   (exit 1)
run-jobs-404           gh api repos/cybermonklabs/legion/actions/runs/1/jobs   (exit 1)
run-attempt-404        gh api repos/cybermonklabs/legion/actions/runs/1/attempts/1   (exit 1)
run-attempt-jobs-404   gh api repos/cybermonklabs/legion/actions/runs/1/attempts/1/jobs   (exit 1)
run-logs-404           gh api repos/cybermonklabs/legion/actions/runs/1/logs   (exit 1)
run-rerun-404          gh api -X POST repos/cybermonklabs/legion/actions/runs/1/rerun   (exit 1)
joblog-404             gh api repos/cybermonklabs/legion/actions/jobs/1/logs   (exit 1)
jobrerun-404           gh api -X POST repos/cybermonklabs/legion/actions/jobs/1/rerun   (exit 1)
review-404             gh api repos/cybermonklabs/legion/pulls/4435/reviews/1   (exit 1)
review-comments-404    gh api repos/cybermonklabs/legion/pulls/4435/reviews/1/comments   (exit 1)
status-ref-404         gh api repos/cybermonklabs/legion/commits/no-such-ref-infra-1303/status   (exit 1)
suites-ref-404         gh api repos/cybermonklabs/legion/commits/trunk/check-suites   (exit 1)
statuses-ref-404       gh api repos/cybermonklabs/legion/commits/trunk/statuses   (exit 1)
repo-404-rules             gh api repos/cybermonklabs/no-such-repo-infra-1303/rules/branches/main   (exit 1)
repo-404-protection        gh api repos/cybermonklabs/no-such-repo-infra-1303/branches/main/protection   (exit 1)
repo-404-required-checks   gh api repos/cybermonklabs/no-such-repo-infra-1303/branches/main/protection/required_status_checks   (exit 1)
repo-404-runs-list         gh api repos/cybermonklabs/no-such-repo-infra-1303/actions/runs?head_sha=1111111111111111111111111111111111111111&per_page=100   (exit 1)
repo-404-run-jobs          gh api repos/cybermonklabs/no-such-repo-infra-1303/actions/runs/1/jobs   (exit 1)
repo-404-reviews           gh api repos/cybermonklabs/no-such-repo-infra-1303/pulls/1/reviews   (exit 1)
repo-404-repo              gh api repos/cybermonklabs/no-such-repo-infra-1303   (exit 1)
repo-404-job               gh api repos/cybermonklabs/no-such-repo-infra-1303/actions/jobs/1   (exit 1)
repo-404-pulls-list        gh api repos/cybermonklabs/no-such-repo-infra-1303/pulls   (exit 1)
(repo-404-rules was answered byte for byte the same with --paginate, and repo-404-pulls-list with a ?state=all&head= query.)
repo-404-pull-patch   gh api -X PATCH repos/cybermonklabs/no-such-repo-infra-1303/pulls/1 -f title=x
repo-404-review-post  gh api -X POST repos/cybermonklabs/no-such-repo-infra-1303/pulls/1/reviews -f event=COMMENT -f body=x
Measured the same way on 2026-11-19 (gh 2.98.0 against api.github.com, through INFRA-1303 fold13's
read-only measure-gh.sh, which logs each request it sends to its requests.log before sending it):
contents-404   gh api repos/cybermonklabs/legion/contents/.github/no-such-dir-infra-1303   (exit 1; requests.log line 63)
compare-404    gh api repos/cybermonklabs/legion/compare/no-such-ref-infra-1303...b2cf98a035b469bb6eb1d5314a106c18d754f451   (exit 1; requests.log line 69)
Measured the same way on 2026-11-20 by the INFRA-1303 tester, as the legion-reviewer App (a read):
issue-comment-404   gh api repos/cybermonklabs/legion/issues/comments/1   (exit 1)
GraphQL requests GitHub refuses before it reads anything (gh prints the body and its message, exit 1;
gh-graphql.ts writes these errors for the call's own query, in this shape):
graphql-variable-null     gh api graphql -f query='query($n:Int!){repository(owner:"cybermonklabs",name:"legion"){pullRequest(number:$n){number}}}'   (no n given)
graphql-variable-coerce   the same query with -f n=1003 (a String for Int!)
graphql-literal-type      gh api graphql -f query='query{repository(owner:"cybermonklabs",name:"legion"){pullRequest(number:"1003"){number}}}'
graphql-variable-unused   gh api graphql -F n=1003 -f query='query($n:Int!,$u:String){repository(owner:"cybermonklabs",name:"legion"){pullRequest(number:$n){number}}}'
graphql-parse-end         gh api graphql -f query='query{repository(owner:"example",name:"widgets"){pullRequest(number:1002){reviewThreads(first:100){nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}'   (trial-t-head-2's query of round 18, one `}` short; GitHub parses before it resolves a name, so it answers this query of the rig's repository as it answers any; measured on 2026-11-21, line 21 of that day's log, below)
  The same day the same error answered four more queries that end where a selection or its `}`
  goes, each at the line and the column the query's last token starts at: `query{viewer{login}`
  at [1, 19] (line 23), `query{viewer{login` at [1, 14] (line 25),
  `query {\n  viewer {\n    login\n  }\n` at [4, 3] (line 27), and `query{viewer{login}` with
  two blanks after it at [1, 19] (line 29).
Stated rather than measured (INFRA-1303, 2026-11-20): a write the measuring token, read-only,
cannot make. The file pair holds what gh 2.98.0 prints for it, in the shape of the measured
errors above (the message, the documentation URL of the endpoint, the status as a string; gh's
`gh: <message> (HTTP <status>)`):
review-dismiss-422   gh api -X PUT repos/<owner>/<repo>/pulls/<n>/reviews/<id of a COMMENTED review>/dismissals -f message=x
  the 422 status: GitHub's REST docs, "Dismiss a review for a pull request" (status codes);
  the message, "Can not dismiss a commented pull request review": not measured, since a
  dismissal is a write, which the read-only token cannot make; no archived call dismisses a
  COMMENTED review (the one archived dismissal, trial-i-head-4's, dismisses review 6002, which
  is APPROVED), so the text decides no run; the documentation URL: that docs section's anchor,
  as review-404 names its own.
Measured the same way on 2026-11-21 for INFRA-1303's round-18 fixes, through the same read-only
measure-gh.sh, into a request log of that day's own, whose line each entry names:
contents-ref-404    gh api repos/cybermonklabs/legion/contents/.github/workflows/tests-closed.yaml?ref=1111111111111111111111111111111111111111   (exit 1; line 5; with ?ref=no-such-ref-infra-1303, line 7, the same bytes but for the ref they name)
workflow-runs-404   gh api repos/cybermonklabs/legion/actions/workflows/no-such-workflow-infra-1303.yaml/runs   (exit 1; line 15; the workflow id 1, which the repository holds no workflow of, the same bytes, line 35)
(A workflow file the repository does not hold, at a ref it does,
`contents/.github/workflows/no-such-file-infra-1303.yaml?ref=8b339a1020aced4bbc126a7a774aa23312cb2a71`,
line 9, answered contents-404's bytes.) These reads answered with exit 0, each <name>.stdout what gh
printed and <name>.stderr empty:
contents-file           gh api repos/cybermonklabs/legion/contents/.github/workflows/tests-closed.yaml?ref=8b339a1020aced4bbc126a7a774aa23312cb2a71   (line 1; its content decodes to the blob d2fb956e… byte for byte)
contents-file-raw       the same read with -H 'Accept: application/vnd.github.raw+json'   (line 3; the blob's own bytes)
workflow-runs-main      gh api 'repos/cybermonklabs/legion/actions/workflows/tests-closed.yaml/runs?branch=main&per_page=1'   (line 11; with ?branch=no-such-branch-infra-1303, line 13, the same)
pr-view-title-commits   gh pr view 4814 --repo cybermonklabs/legion --json title,commits   (line 33)
With no file, since the page holds the runs of the repository: `actions/workflows/ci.yaml/runs?
branch=main&per_page=1` (line 17) and `actions/runs?branch=main&per_page=1` (line 19) each answered
a page whose run holds the same 35 keys in the same order, the run of the workflow list in the
shape the runs list gives one.
Measured on 2026-11-21 for INFRA-1303's round-19 fixes with the pod's own gh 2.98.0, as GraphQL
queries (no mutation) and GETs, not through measure-gh.sh, so no request log holds them, against
cybermonklabs/legion#3514, at 21:20:43Z to 21:21:33Z: the query of
trial-i-head-2's review threads read of round 19, `query($o:String!,$r:String!,$n:Int!,$endCursor:
String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:
$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login}
body}}}}}}}` (one line), each <name>.stdout what gh printed and <name>.stderr what it printed there:
threads-paginate-slurp   gh api graphql --paginate --slurp -f query=<that query> -F o=cybermonklabs -F r=legion -F n=3514   (exit 0; one page, 52 threads and hasNextPage false, in a list of one)
threads-paginate         the same with --paginate alone   (exit 0; the same bytes without the list's `[` and `]`)
slurp-jq                 the same as threads-paginate-slurp with trial-i-head-2's own --jq, `[.[].data.repository.pullRequest.reviewThreads.nodes[]] | length as $c | "threads: \($c)"`   (exit 1, nothing on stdout; gh's refusal before any request, its message, a blank line and its usage: pkg/cmd/api/api.go:256-264 at v2.98.0)
(The same query with first:20 took three pages, 20, 20 and 12 threads, hasNextPage true on the
first two: with --slurp gh printed `[<page 1>,<page 2>,<page 3>]`, and with --paginate alone the
three pages one after another, nothing between them and no newline after, each page the body
GitHub sent, which is compact JSON. gh asks each next page with the variable endCursor set to the
page's pageInfo.endCursor while its hasNextPage is true: findEndCursor in pkg/cmd/api/
pagination.go and api.go:456-460, read from raw.githubusercontent.com at v2.98.0.)
Measured on 2026-11-22 for INFRA-1303's round-20 fixes with the pod's own gh 2.98.0, as GraphQL
queries (no mutation) and one GET of gh's source, not through measure-gh.sh (gone with the pod's
disk), each request written with its time to a log of the session before it was sent, at
01:16:21Z to 01:18:27Z; gh exited 1 for each, <name>.stdout what it printed and <name>.stderr what
it printed there:
pr-view-404        gh pr view 999999 --repo cybermonklabs/legion   (nothing on stdout; `gh pr view` with --json headRefOid --jq .headRefOid, `gh pr checks 999999` and `gh pr diff 999999`, each with that --repo, the same bytes)
repo-404-pr-list   gh pr list --repo cybermonklabs/no-such-repo-infra-1303 --head legion/LWEVAL-1 --state open --json number,headRefOid   (nothing on stdout; `gh pr view 1` with --json headRefOid --jq .headRefOid and `gh issue view 1`, each with that --repo, and `gh repo view cybermonklabs/no-such-repo-infra-1303`, the same bytes)
graphql-pr-404     gh api graphql -f query='query{repository(owner:"cybermonklabs",name:"legion"){pullRequest(number:999999){number}}}'
graphql-repo-404   gh api graphql -f query='query{repository(owner:"cybermonklabs",name:"no-such-repo-infra-1303"){name}}'
graphql-parse-end-jq   trial-s-head-5's review threads read of round 20 as it sent it, one `}` short with its own --jq: gh api graphql -f query='query{repository(owner:"example",name:"widgets"){pullRequest(number:1005){reviewThreads(first:100){nodes{id isResolved comments(last:1){nodes{author{login}}}}}}}' --jq '.data.repository.pullRequest.reviewThreads.nodes[] | [.id, (if .isResolved then "resolved" else "open" end), .comments.nodes[0].author.login] | @tsv'
  (trial-t-head-4's read of round 20, graphql-parse-end's query of pull request 1004 with
  `--jq '.data.repository.pullRequest.reviewThreads.nodes | length'`, answered graphql-parse-end's
  bytes. The same two queries of cybermonklabs/legion#3514 with the same filters answered the same
  bytes but for the place, [1, 166] and [1, 171], and the first with no --jq answered what it
  answered with it: gh runs no --jq over an error GitHub answers, and prints the body GitHub sent
  and `gh: <message>` on stderr, pkg/cmd/api/api.go:488-561 at v2.98.0, read with
  `gh api repos/cli/cli/contents/pkg/cmd/api/api.go?ref=v2.98.0` and the raw media type.)

What GitHub answered to the reads below, measured the same way on 2026-11-18, which the rig's
answers follow (no file: each is stated here and in the comment of the code that follows it):
- Check runs of a commit (05e64382b833c8dfdfbd8584261a2e2f3b7deaa5, whose CI run 44406867953 has
  two attempts): `commits/<sha>/check-runs` with no filter, and with `filter=latest`, 64 check
  runs, one per name (`ci-ok` its attempt-2 success alone); `filter=all` 122 (`ci-ok` its success
  and its attempt-1 failure); `filter=bogus` the same 64 as no filter, exit 0; `check_name=ci-ok`
  1, with `filter=all` 2; `status=completed` 64, `status=in_progress` 0.
- Jobs of that run: `actions/runs/44406867953/jobs` 58, all of attempt 2; with `filter=all` 116.
- A page past the end: `pulls?state=all&per_page=100&page=99999` answers `[]`;
  `actions/runs?...&page=2` past its runs answers `{"total_count":3,"workflow_runs":[]}`.
- `actions/runs?exclude_pull_requests=true`: a pull_request run that lists 1 pull request lists 0.
- A run's `previous_attempt_url`: null on attempt 1, `.../actions/runs/<id>/attempts/1` on attempt 2.
- The GraphQL rollup of 05e64382 (`statusCheckRollup{contexts(first:100, after:$endCursor){...}}`):
  64 contexts, `pageInfo` `{"endCursor":"NjQ","hasNextPage":false}` with no endCursor given;
  `first:10` gives `{"endCursor":"MTA","hasNextPage":true}` (a cursor is base64 of the 1-based
  index of the page's last node, unpadded); `after:"NjQ"` gives no node and
  `{"endCursor":null,"hasNextPage":false}`, totalCount still 64.
- GraphQL: a variable given that the query does not declare (`-f extra=1`) is ignored, the query
  answered; `repository(owner:"CyberMonkLabs",name:"Legion")` answers the repository, as does the
  REST `repos/CyberMonkLabs/Legion`: owner and name match without regard to case;
  `object(expression:"main~1")` answers a commit (a revision expression the rig does not model).
- Measured on 2026-11-19 through the fold's read-only measure-gh.sh (its request log names each):
  - `--paginate` of a REST read: `pulls/4148/comments?per_page=3` (six comments, two pages) printed
    one JSON array of the six, the second page's items joined to the first's with `,`;
    `commits/05e64382.../check-runs?check_name=ci-ok&filter=all&per_page=1` (two pages) printed the
    two objects one after the other with nothing between and no newline after; and a paged read that
    names no per_page, `--paginate pulls/4148/comments`, is sent as `pulls/4148/comments?per_page=100`
    (GH_DEBUG=api).
  - The jobs of each attempt of run 44406867953, whose attempt 2 re-ran its failed jobs:
    attempt 1 lists 58 jobs (2 failure, 41 success, 15 skipped), attempt 2 lists 58 (43 success,
    15 skipped), all at run_attempt 2. The 2 failed jobs were re-run; each of the 56 others is
    carried to attempt 2 with an id and a check run of its own (its check_run_url names its own id),
    its attempt-1 conclusion, steps, runner and started_at and completed_at, and a new created_at.
- A comment on a pull request's conversation (measured by the INFRA-1303 tester on 2026-11-20,
  `gh api repos/cybermonklabs/legion/issues/comments/6846500183`, a comment of #3514): its keys in
  GitHub's order are url, html_url, issue_url, id, node_id, user, created_at, updated_at,
  author_association, body, reactions, performed_via_github_app, minimized and pin; its node_id
  IC_kwDORFy7ds8AAAABmBVNVw is [0, the repository id, the comment id]; its html_url is the pull
  request's URL and `#issuecomment-<id>`, its issue_url the issue of the pull request; reactions
  holds its url and nine counts, each 0; minimized and pin are null.
- Measured by the reviewer on 2026-11-19 (infra-1303-github-fields-measured): a check run, a job and
  a run of GitHub Actions as GitHub answers each, which red_model writes key for key; and that a
  commit's default check-runs read keeps the latest of each name in each check suite
  (`commits/b789479b99.../check-runs?check_name=pr-title` answers five pr-title runs of five suites,
  as `filter=all` does).
  - The pull requests a `workflow_dispatch` run names: each of the 30 latest workflow_dispatch runs
    of cybermonklabs/legion lists one; run 44457284390 (event workflow_dispatch, head 38c89d64...
    on legion/INFRA-3135, which is the current head of the open #3729) lists #3729 in the minimal
    shape, and the check runs of its check suite (120342313542) list #3729 too.
- Measured on 2026-11-19 for INFRA-1303 fold13, gh 2.98.0 against api.github.com through the
  fold's read-only measure-gh.sh; each label is the one the rig's comments cite, and each line
  number the request's line in that measure-gh.sh's requests.log. The in-progress reads are of
  cybermonklabs/legion run 44586483868 (the CI of #3514 at 36155e1a) while it ran.
  - gh 2.98.0's own source, each `gh api repos/cli/cli/contents/<path>?ref=v2.98.0` read raw:
    pkg/cmd/run/rerun/rerun.go (line 1), pkg/cmd/run/view/view.go (49),
    pkg/cmd/pr/checks/checks.go (51), pkg/cmd/pr/checks/output.go (53), pkg/cmd/run/list/list.go
    (55), pkg/cmd/workflow/shared/shared.go (57), cmd/gh/main.go (59), internal/ghcmd/cmd.go (61),
    pkg/cmd/run/shared/shared.go (81).
  - ip-run `gh api repos/cybermonklabs/legion/actions/runs/44586483868` (7): status `queued` while
    a job of it was queued, conclusion null.
  - ip-jobs `.../actions/runs/44586483868/jobs?per_page=100` (9), ip-job `.../actions/jobs/
    135780822636` (11) and ip-job-queued `.../actions/jobs/135783168595` (13): a job in progress
    has status in_progress, conclusion and completed_at null, its steps in_progress (completed_at
    null) or pending (started_at null); a queued job has no step and no runner.
  - ip-joblog `.../actions/jobs/135780822636/logs` (15): `gh: HTTP 404`, exit 1, while the job runs.
  - ip-runview `gh run view 44586483868` (17), ip-runview-v with `-v` (19), ip-runview-job
    `gh run view --job 135780822636` (21): the first line names the pull request of the run's
    branch after the workflow (`* legion/INFRA-1303 CI cybermonklabs/legion#3514 · 44586483868`);
    a run, job or step not completed is marked `*`, a job not completed has no time.
  - ip-runview-logfailed `gh run view 44586483868 --log-failed` (23) and ip-runview-joblog
    `gh run view --job 135780822636 --log` (25): `run <id> is still in progress; logs will be
    available when it is complete`, and `job <id> is still in progress; …` for the job, exit 1.
  - ip-runview-json `gh run view 44586483868 --json attempt,conclusion,status,updatedAt,jobs`
    (27): each job and each step with its startedAt and completedAt, `0001-01-01T00:00:00Z` where
    not yet reached, conclusion "" where none.
  - ip-runlist `gh run list --commit 36155e1a... --limit 5` (29) and ip-runlist-json with `--json
    attempt,conclusion,status,databaseId,workflowName,updatedAt,startedAt` (31): a run in progress
    prints the time since it started.
  - ip-prchecks `gh pr checks 3514` (33): the rows `<name>\t<bucket>\t<elapsed>\t<link>\t<description>`,
    `0` where a check started and completed in the same second, exit 8 while a check is pending;
    ip-prchecks-json `--json name,state,bucket,link` (35), prchecks-json-bogus `--json bogus` (37,
    gh's field list, exit 1) and prchecks-json-all `--json bucket,completedAt,description,event,
    link,name,startedAt,state,workflow` (71): the fields' values, exit 0 whatever the checks say.
  - ip-prview-rollup `gh pr view 3514 --json statusCheckRollup` (39) and ip-graphql, the page's
    rollup query of 36155e1a with --paginate (47): a check run not yet completed has its status in
    capitals, conclusion "" and the zero time of Go; the rollup with one pending and none failing
    is PENDING.
  - ip-checkruns `.../commits/36155e1a.../check-runs?check_name=lint%20/%20lint` (41): a check run
    in progress has status in_progress, conclusion and completed_at null. ip-checkruns-queued
    `...?check_name=claude-envoy%20/%20private-names` (43) had completed by the time it was read,
    and ip-checksuites `.../commits/36155e1a.../check-suites` (45) is the head's suites.
  - contents-dir-workflows `.../contents/.github/workflows?ref=b2cf98a0...` (73), contents-dir-
    github `.../contents/.github?ref=b2cf98a0...` (75) and contents-dir-noref
    `.../contents/.github/workflows` (79): a directory's listing, one entry per file or directory
    by name, its URLs naming the ref the call gave (the default branch where it gave none).
  - gittree-root `.../git/trees/b2cf98a0...` (77): the top level of a tree alone, each entry's
    path, mode, type, sha, size (blobs) and url, `truncated:false`.
  - compare `.../compare/5f3d552e...b2cf98a0...` (67, its base read as main's head's parent at 65):
    the comparison's keys in GitHub's order, status ahead, ahead_by 1, behind_by 0, the merge base,
    the commits and the files.
