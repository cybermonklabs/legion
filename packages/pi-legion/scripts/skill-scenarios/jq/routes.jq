# The routes of the red-at-the-head world, which red-at-the-head.sh writes to <run>/fixtures.json
# and gh-standin.ts answers from: the pull request worker_fixture builds (other_commit_routes,
# pull_routes), the red red_fixture turns it to (red_model, run_routes, red_rewrite) and the
# base's required set (required_set), each a function of this module, and the definitions they
# share. Loaded with `jq -L <this directory>` and `include "routes";` (red-at-the-head.sh's
# red_jq). It includes nothing, so a def here may call any other: jq 1.8.2 aborts at compile on a
# def chain reached through a nested include (docs/solutions/testing/a-jq-module-reached-through-
# an-include-keeps-its-defs-flat-and-a-live-proof-compiles-every-module-on-the-hosts-jq-at-its-
# prerequisites-LEGION-578.md). Each function reads the variables its caller passes with --arg,
# --argjson and --slurpfile, named in its comment; a def reads only those its comment names.

# What every route writer here shares: how a regex names a text alone, a commit by its sha or any
# abbreviation of it, and a branch by its name; where a route looks in a call; and the errors
# GitHub and gh answer for what they cannot find. Each writer binds `$repo`, the world's
# repository. A route reads the call's request as the gh stand-in resolves it (gh-standin.ts): a
# REST route anchors on the method gh sends and the endpoint path, in the world's repository,
# which ends at its query or at the end of the argument, so no text inside another argument (a
# --jq filter, a field) and no text inside a query string makes it match, and it takes no media
# type but JSON in an `Accept` header, since another type is another answer; before this the
# generic `(check-runs|status)` route took a pull request files read whose --jq named `.status`,
# and a protection read whose --jq named `required_status_checks`. A GraphQL route anchors on the
# `graphql` endpoint, and the stand-in answers the query from the world it holds.
def literal: gsub("(?<c>[.*+?^${}()|\\[\\]\\\\])"; "\\\(.c)");
def abbreviations: .[0:4] + (.[4:] | split("") | reverse
  | reduce .[] as $digit (""; "(?:" + $digit + . + ")?"));
def branch: "(?:(?:refs/)?heads/)?" + (literal | gsub("/"; "(?:/|%2[Ff])"));
def json_only: "(?!.*\\s--header\\s[Aa]ccept:(?!\\u001f*application/(?:vnd\\.github(?:\\.v3)?\\+)?json(?:\\s|$)))";
# The repository as a route names it: GitHub matches its owner and name without regard to
# case (measured, github-answers/README.txt), so each letter is either case.
def any_case: literal | gsub("(?<c>[A-Za-z])"; "[" + (.c | ascii_upcase) + (.c | ascii_downcase) + "]");
def rest($method; $path; $headers): "^gh api " + $method + " repos/" + ($repo | any_case)
  + (if $path == "" then "" else "/" + $path end) + "(?:\\?\\S*)?(?=\\s|$)" + $headers;
def api($path): rest("GET"; $path; json_only);
def api_post($path): rest("POST"; $path; json_only);
def graphql: "^gh api POST graphql ";
# An error GitHub answers, as measured (gh-standin.ts reads github-answers/<answer>): the body
# GitHub sent and the line gh prints for it; one that names a commit names the one the group
# `sha` took.
def github($answer): {github:$answer};
def github($answer; $sha): {github:$answer, sha:$sha};
# The repository as the REST API answers it within another object (a combined status), and the
# combined status of a commit nothing posted a status to, in the order GitHub answers them.
def repository: ($repo | split("/")) as [$owner, $name]
  | {id:1, name:$name, full_name:$repo, private:false, owner:{login:$owner},
     html_url:("https://github.com/" + $repo)};
def combined_status($sha): {state:"pending", statuses:[], sha:$sha, total_count:0,
  repository:repository, commit_url:("https://api.github.com/repos/" + $repo + "/commits/" + $sha),
  url:("https://api.github.com/repos/" + $repo + "/commits/" + $sha + "/status")};
# gh's error for a run, a job or a list of runs GitHub answers 404 for, as gh 2.98.0 prints it to
# stderr, exit 1, read from gh's own source (github.com/cli/cli at v2.98.0, and github.com/cli/go-gh
# at v2.13.0, the go-gh its go.mod pins): `gh run view` wraps the error of shared.GetRun as
# `failed to get run: %w` (pkg/cmd/run/view/view.go:259) and of shared.GetJob as `failed to get
# job: %w` (view.go:233), `gh run watch` wraps GetRun the same way (pkg/cmd/run/watch/watch.go:135),
# and `gh run list` wraps the error of its runs as `failed to get runs: %w`
# (pkg/cmd/run/list/list.go:146). The error is go-gh's HTTPError, `HTTP %d: %s (%s)`: the status,
# GitHub's message, `Not Found` as github-answers' measured 404s carry it, and the URL of the
# request (pkg/api/errors.go:31-38). That URL is the one each GET names: GetRun's
# `repos/<repo>/actions/runs/<id>` with `exclude_pull_requests=true`
# (pkg/cmd/run/shared/shared.go:562-576), GetJob's `repos/<repo>/actions/jobs/<id>`
# (shared.go:525-526), and the list's `actions/runs` with `per_page` its --limit, 20 unless it
# says, and `exclude_pull_requests=true` (shared.go:380-385, list.go's defaultLimit). No 404 of
# these was measured: the ids of the world's runs and jobs are the rig's own.
def no_run($in; $id): {stderr:("failed to get run: HTTP 404: Not Found (https://api.github.com/repos/"
  + $in + "/actions/runs/" + $id + "?exclude_pull_requests=true)\n"), exit:1};
def no_job($in; $id): {stderr:("failed to get job: HTTP 404: Not Found (https://api.github.com/repos/"
  + $in + "/actions/jobs/" + $id + ")\n"), exit:1};
def no_runs($in): {stderr:("failed to get runs: HTTP 404: Not Found (https://api.github.com/repos/"
  + $in + "/actions/runs?exclude_pull_requests=true&per_page=20)\n"), exit:1};
def no_repo($name): {stderr:("GraphQL: Could not resolve to a Repository with the name \u0027"
  + $name + "\u0027. (repository)\n"), exit:1};
def no_pr($number): {stderr:("GraphQL: Could not resolve to a PullRequest with the number of "
  + $number + ". (repository.pullRequest)\n"), exit:1};
def until_posted($post): {follows:$post, replies:[{stdout:[]}, {unmodeled:true}]};
# The node id GitHub gives an object: its kind, `_`, and the url-safe base64, unpadded, of the
# MessagePack array of a number and the ids (each in its shortest MessagePack form). Read from
# the node ids the reviewer measured on cybermonklabs/legion (2026-11-19,
# infra-1303-github-fields-measured): a check run CR_kwDORFy7ds8AAAAfaVpVaA is [0, 1146927990,
# 134911513960], the repository id and its own; a run WFR_kwLORFy7ds8AAAAKUpvh-Q is [2, the
# repository id, the run id]; a check suite CS_kwDO… [0, …]; the repository R_kgDORFy7dg [0,
# 1146927990]. Each comes back byte for byte from node_id.
def big_endian($n; $count): [range($count - 1; -1; -1) as $i | ($n / pow(2; 8 * $i) | floor) % 256];
def msgpack: if . < 128 then [.] elif . < 256 then [204, .]
  elif . < 65536 then [205] + big_endian(.; 2) elif . < 4294967296 then [206] + big_endian(.; 4)
  else [207] + big_endian(.; 8) end;
def base64url: . as $b | "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" as $abc
  | [range(0; $b | length; 3) as $i | $b[$i:$i + 3] as $c
     | ($c[0] * 65536 + ($c[1] // 0) * 256 + ($c[2] // 0)) as $n
     | [18, 12, 6, 0][0:($c | length) + 1] | map($abc[($n / pow(2; .) | floor) % 64:][:1]) | join("")]
  | join("");
def node_id($prefix; $kind; $ids):
  $prefix + "_" + ([144 + ($ids | length) + 1, $kind] + ($ids | map(msgpack) | add) | base64url);
# The program a route runs over its answer, by its name in programs.jq, which gh-standin.ts loads
# from this directory.
def program($name): "include \"programs\"; " + $name;

# A workflow of red_workflows' model as worker_fixture renders it: a step's name as GitHub gives
# it (its own `name`, else `Run <command>`, as the jobs of cybermonklabs/legion run 44586483868
# name theirs: ip-jobs, requests.log line 9), and a workflow as the YAML file worker_fixture
# commits, every scalar a JSON string, which YAML reads as written, each job on the runner label
# the jobs of that run carry (ubuntu-24.04, ip-jobs).
def step_name: .name // ("Run " + .run);
def yaml_map($indent): to_entries | map("\n" + $indent + .key + ": " + (.value | tojson)) | join("");
def workflow_yaml:
  "name: " + (.name | tojson) + "\non:\n" + (.on | map("  " + . + ":\n") | join(""))
  + "jobs:\n"
  + (.jobs | map("  " + .id + ":\n    name: " + (.name | tojson) + "\n"
      + (if .needs != null then "    needs: " + (.needs | tojson) + "\n    if: " + ("always()" | tojson) + "\n" else "" end)
      + "    runs-on: " + ("ubuntu-24.04" | tojson) + "\n    steps:\n"
      + (.steps | map("      - " + ([
          (if .name != null then "name: " + (.name | tojson) else empty end),
          (if .uses != null then "uses: " + (.uses | tojson) else empty end),
          (if .with != null then "with:" + (.with | yaml_map("          ")) else empty end),
          (if .env != null then "env:" + (.env | yaml_map("          ")) else empty end),
          (if .run != null then "run: " + (.run | tojson) else empty end)] | join("\n        ")) + "\n")
        | join(""))) | join(""));

# other_commit_routes: what a read of a commit names, as one JSON object, for the head, $head, of
# pull request $pr, and for the base, $base_sha on $base ($branch is the head's, $repo the world's):
# `forms`, the regex of each commit's names in a REST path; `heads`, the matches of the head's own
# check runs, combined status and statuses; `rest`, the routes that answer those three reads of
# every other commit; and `graphql`, the names a GraphQL read of the repository gives a commit (an
# object by its oid or its expression, a ref by its qualified name, the default branch), each
# naming the node it resolves to in the world worker_fixture writes (`$refs`: head, base, head_ref,
# base_ref), and every other name of an object or a ref answering null, GitHub's answer for a name
# the repository does not resolve. worker_fixture puts `rest` ahead of the routes that answer the
# head's own reads, which red_fixture turns red in place. Before these routes a read of the base, or
# of a commit nobody made, was answered the head's own checks: in the tester's round-9 batch at
# c0ace596, trial-j-nos1-4 read main's check runs at 77671688…, was answered the head's
# `test (notify)` failure, and declined the re-run, since the base read as failing the same test
# (red-at-the-head.md, "When the cause is another tree's"); runs of rounds 6 and 9 that read
# `commits/null/…` or `commits/trunk/…` were answered the same red. A GraphQL rollup read that
# named the base by its default branch, its ref or a variable was answered the head's red too
# until the round-5 review at 4728c2f7: the world now resolves every name itself.
# A call names the head by $head or an abbreviation of it down to four digits, as git resolves
# one, by $branch (also as heads/<branch> or refs/heads/<branch>, its slash also as %2F in a
# path), or by the pull request's head ref (pull/<pr>/head). A call names the base by $base_sha or
# an abbreviation of it, by $base as a branch, or by `HEAD`, which GitHub resolves to the default
# branch. GraphQL's `object(oid:)` takes a full sha alone. The REST routes, in the order they are
# matched:
#   the base's check runs: none, since the rig runs nothing on the base, under any query the
#     check runs of a commit read (check_runs_program), and refused under any other
#   the base's combined status: GitHub's answer for a commit nothing posted a status to, `pending`
#     with no status
#   the base's statuses: none, for the same reason
#   any other commit, as GitHub answers a ref it does not hold (measured, github-answers/): its
#     check runs and the commit itself `No commit found for SHA: <ref>` (422); a full sha's
#     combined status `pending` with no status and its statuses none, as for any commit nothing
#     posted a status to, and its check suites 422 too; any other ref's combined status `Ref not
#     found`, and its statuses and check suites `Not Found` (404). The stand-in puts the ref the
#     call named in place of the measured answer's commit, and of `$<commit>` (gh-standin.ts)
def other_commit_routes:
  program("check_runs_program") as $check_runs
  | ("(?:" + ($head | abbreviations) + "|" + ($branch | branch)
   + "|(?:refs/)?pull/" + $pr + "/head)") as $heads
  | ("(?:" + ($base_sha | abbreviations) + "|" + ($base | branch) + "|HEAD)") as $bases
  # Every name GraphQL resolves a commit or a ref by, keyed as gh-graphql.ts keys a field.
  | def names($sha; $ref): [range(4; 41) as $n | $sha[0:$n]]
      + [$ref, "refs/heads/" + $ref, "heads/" + $ref];
    def object($field; $value): "object(" + $field + ":" + ($value | tojson) + ")";
    def qualified($value): "ref(qualifiedName:" + ($value | tojson) + ")";
  {forms:{head:$heads, base:$bases},
   heads:{"check-runs":api("commits/" + $heads + "/check-runs"),
          "check-suites":api("commits/" + $heads + "/check-suites"),
          status:api("commits/" + $heads + "/status"),
          statuses:api("commits/" + $heads + "/statuses")},
   rest:[
     {match:api("commits/" + $bases + "/check-runs"), stdout:{total_count:0, check_runs:[]},
      program:$check_runs},
     {match:api("commits/" + $bases + "/status"), stdout:combined_status($base_sha)},
     {match:api("commits/" + $bases + "/statuses"), stdout:[]},
     ({match:api("commits/(?!" + $heads + "/)(?<commit>[^\\s?]*)/check-runs")}
      + github("checkruns-422"; "commit")),
     {match:api("commits/(?!" + $heads + "/)(?<commit>[0-9a-f]{40})/status"),
      stdout:combined_status("$<commit>")},
     {match:api("commits/(?!" + $heads + "/)(?<commit>[0-9a-f]{40})/statuses"), stdout:[]},
     ({match:api("commits/(?!" + $heads + "/)(?<commit>[0-9a-f]{40})/check-suites")}
      + github("suites-422"; "commit")),
     ({match:api("commits/(?!" + $heads + "/)[^\\s?]*/status")} + github("status-ref-404")),
     ({match:api("commits/(?!" + $heads + "/)[^\\s?]*/statuses")} + github("statuses-ref-404")),
     ({match:api("commits/(?!" + $heads + "/)[^\\s?]*/check-suites")} + github("suites-ref-404")),
     ({match:api("commits/(?!(?:" + $heads + "|" + $bases + ")(?=[?\\s]|$))(?<commit>[^\\s?/]+)")}
      + github("commit-422"; "commit"))],
   graphql:(([[$head, "head"], [$base_sha, "base"]] | map({(object("oid"; .[0])): {"$ref":.[1]}}) | add)
     + ([names($head; $branch)[], "refs/pull/" + $pr + "/head", "pull/" + $pr + "/head"]
        | map({(object("expression"; .)): {"$ref":"head"}}) | add)
     + (names($base_sha; $base) + ["HEAD"] | map({(object("expression"; .)): {"$ref":"base"}}) | add)
     + {(qualified("refs/heads/" + $branch)): {"$ref":"head_ref"}, (qualified($branch)): {"$ref":"head_ref"},
        (qualified("refs/heads/" + $base)): {"$ref":"base_ref"}, (qualified($base)): {"$ref":"base_ref"},
        "object(*)": null, "ref(*)": null, defaultBranchRef:{"$ref":"base_ref"}})};

# pull_routes: the routes the gh stand-in answers worker_fixture's world from, and the template of
# the comment a role posts, as fixtures.json holds them. The head reads green in all four places
# an agent reads it: the `gh pr view` rollup, `gh pr checks`, the GraphQL rollup of the head, and
# the commit's check runs. Ahead of the last go the routes for a read of every other commit
# ($others, other_commit_routes): the base, $base_sha, as GitHub answers a commit nothing ran on,
# and any other commit as GitHub answers one it cannot find; GraphQL resolves each name in the
# world itself. Each route answers only from what the world holds and only the request it names;
# anything else stays unmodeled, which the stand-in refuses (deny by default, in each route as
# across them). It reads the pull request ($number, $url, $title, $body, its head $head on $ref
# into $base at $base_sha, created at $date), the issue $key, the repository $repo and what it
# holds ($snapshot, repo_snapshot's), and the tester-proof world's workflows ($green,
# red_workflows').
def pull_routes:
  program("pr_list_program") as $pr_list
  | program("pr_view_program") as $pr_view_text
  | program("pulls_query_program") as $pulls_query
  | program("check_runs_program") as $check_runs
  | program("trees_query_program") as $trees_query
  | $snapshot as {$diff, $files, $commits, $trees}
  | ($number|tostring) as $n
  | ($repo | split("/")) as [$owner, $name]
  # The green head checks of this world, the jobs of the `ci` workflow of the tester-proof
  # world (red_workflows), each with the link `gh pr checks` prints for it.
  | ($green.workflows[0].jobs | length) as $count
  | ($green.workflows[0].jobs | to_entries
     | map({name:.value.name, link:("https://example.invalid/" + ($count - .key | tostring))}))
    as $checks
  # The pull request in the fields gh 2.98.0 prints for `gh pr view --json` and `gh pr list
  # --json` (measured), each one the world holds: its files in the shape gh prints them, its
  # rollup as gh flattens it (a node per check run, its workflow named), and its author, the
  # App of the rig. REST names that account rig[bot]; GitHub GraphQL names a bot by its bare
  # login, rig, with no [bot] (as this repository records for the pull requests of its own App:
  # the comment on searchPullRequestNode in packages/envoy/internal/dispatch/delivery/github_prs.go),
  # and gh 2.98.0 prints a Bot author as is_bot true and `app/` before that login (measured
  # against a local GraphQL stand-in). A field gh lists that the world does not hold is not here,
  # and the stand-in refuses a read of it. The merge state is CLEAN (the MergeStateStatus of
  # GitHub, https://docs.github.com/en/graphql/reference/enums#mergestatestatus: "Mergeable and
  # passing commit status"): no check of this head fails and the base of this world requires none.
  | {id:("PR_" + $n), number:$number, url:$url, title:$title, state:"OPEN", isDraft:false,
     closed:false, closedAt:null, mergedAt:null, headRefName:$ref, headRefOid:$head,
     baseRefName:$base, baseRefOid:$base_sha, body:$body, mergeable:"MERGEABLE",
     mergeStateStatus:"CLEAN", reviewDecision:"", createdAt:$date, isCrossRepository:false,
     author:{is_bot:true, login:"app/rig"}, labels:[], assignees:[], reviewRequests:[],
     milestone:null, autoMergeRequest:null,
     additions:($files | map(.additions) | add), deletions:($files | map(.deletions) | add),
     changedFiles:($files | length),
     files:($files | map({path:.filename, additions, deletions,
                          changeType:({added:"ADDED", modified:"MODIFIED", removed:"DELETED"}[.status])})),
     statusCheckRollup:($checks | map({__typename:"CheckRun", completedAt:$date, conclusion:"SUCCESS",
                                       detailsUrl:.link, name, startedAt:$date, status:"COMPLETED",
                                       workflowName:"ci"}))} as $pr
  # The same pull request as the REST API answers it, the fields it holds at every moment of the
  # run (no count of comments, which the stand-in does not keep); its head and its base are of
  # the one repository of the world, which each names as the pull requests of GitHub do.
  | {url:("https://api.github.com/repos/" + $repo + "/pulls/" + $n), id:$number, node_id:("PR_" + $n),
     html_url:$url, diff_url:($url + ".diff"), patch_url:($url + ".patch"), number:$number,
     state:"open", locked:false, title:$title, user:{login:"rig[bot]", type:"Bot"}, body:$body,
     labels:[], milestone:null, draft:false, created_at:$date, closed_at:null, merged_at:null,
     merge_commit_sha:null, assignee:null, assignees:[], requested_reviewers:[], requested_teams:[],
     head:{label:($owner + ":" + $ref), ref:$ref, sha:$head, user:{login:$owner}, repo:repository},
     base:{label:($owner + ":" + $base), ref:$base, sha:$base_sha, user:{login:$owner},
           repo:repository},
     author_association:"NONE", auto_merge:null, merged:false, mergeable:true,
     mergeable_state:"clean", merged_by:null, maintainer_can_modify:false, commits:1,
     additions:$pr.additions, deletions:$pr.deletions, changed_files:$pr.changedFiles} as $pull
  | def tree_of($sha): $trees | map(select(.sha == ($commits[] | select(.sha == $sha) | .commit.tree.sha))) | first;
    def ref_of($sha; $branch): {ref:("refs/heads/" + $branch),
      url:("https://api.github.com/repos/" + $repo + "/git/refs/heads/" + $branch),
      object:{sha:$sha, type:"commit",
              url:("https://api.github.com/repos/" + $repo + "/git/commits/" + $sha)}};
    # The listing of a directory as the contents API of GitHub answers it at a ref (measured on
    # cybermonklabs/legion, 2026-11-19, contents-dir-workflows, contents-dir-github and
    # contents-dir-noref, requests.log lines 73, 75 and 79): each entry
    # directly under the directory, by name, a file or a directory, its URLs naming the ref as the
    # call gave it, or the default branch where it gave none, and its git URL its own blob or tree.
    def listing($sha; $dir; $ref): tree_of($sha).tree
      | map(select((.path | split("/") | .[:-1] | join("/")) == $dir))
      | sort_by(.path | split("/") | last)
      | map(("https://api.github.com/repos/" + $repo + "/contents/" + .path + "?ref=" + $ref) as $self
            | ("https://github.com/" + $repo + "/" + (if .type == "tree" then "tree" else "blob" end)
               + "/" + $ref + "/" + .path) as $html
            | .url as $git
            | {name:(.path | split("/") | last), path, sha, size:(.size // 0), url:$self,
               html_url:$html, git_url:$git,
               download_url:(if .type == "blob"
                             then "https://raw.githubusercontent.com/" + $repo + "/" + $ref + "/" + .path
                             else null end),
               type:(if .type == "tree" then "dir" else "file" end),
               _links:{self:$self, git:$git, html:$html}});
    # The reads of the contents of a commit: with no ref the default branch, the base, else the base or
    # the head by any name GitHub resolves it by, each as the group `ref` takes it.
    ([{sha:$base_sha, ref:$base, q:"(?=(?:\\?(?!\\S*\\bref=)\\S*)?(?:\\s|$))"},
      {sha:$base_sha, ref:"$<ref>",
       q:("(?=\\?(?:\\S*&)?ref=(?<ref>" + $others.forms.base + ")(?:&\\S*)?(?:\\s|$))")},
      {sha:$head, ref:"$<ref>",
       q:("(?=\\?(?:\\S*&)?ref=(?<ref>" + $others.forms.head + ")(?:&\\S*)?(?:\\s|$))")}]) as $at_refs
  | ([$at_refs[] as $r | tree_of($r.sha).tree as $entries
      | ([""] + ($entries | map(select(.type == "tree") | .path)))[] as $dir
      | {match:api("contents" + (if $dir == "" then "" else "/" + ($dir | literal) end) + "/?" + $r.q),
         stdout:listing($r.sha; $dir; $r.ref)}]) as $listings
  # The contents of a file are not modeled; a path the commit does not hold answers the 404 of GitHub.
  | ([$at_refs[] as $r | tree_of($r.sha).tree[] | select(.type == "blob")
      | {match:api("contents/" + (.path | literal) + $r.q), unmodeled:true}]
     + [$at_refs[] as $r | (tree_of($r.sha).tree | map(.path | literal) | join("|")) as $known
        | {match:api("contents/(?!(?:" + $known + ")/?(?:[?\\s]|$))[^?\\s]+" + $r.q)}
          + github("contents-404")]) as $contents_beyond
  |
  # Each route answers the one read or write it models, and a call no route models is refused
  # by the stand-in as unmodeled. `head_read` marks the reads of the head that red_fixture
  # turns red where they stand: the pull request (`pr`, the gh object, and `pull` and `pulls`,
  # the REST ones, whose merge state follows the checks), its checks (`checks`), the GraphQL
  # world (`graphql`), and its check runs by any name of the head (`check-runs`). The combined
  # status of the head is GitHub own answer for a commit no status was posted to, whatever its
  # check runs say, since a combined status holds statuses and no check run.
  # `repo_refusal` marks the answers for a repository other than the world, which GitHub
  # cannot see: REST answers its 404, the commands gh runs through GraphQL the words gh 2.98.0
  # prints for a repository GraphQL cannot resolve (measured), and the run commands the 404 of
  # the run, the job or the list they read. red_fixture keeps them ahead of every route.
  # A pull request command names this pull request by its number; another number is answered
  # what gh 2.98.0 prints for a pull request GraphQL cannot resolve (measured), and a branch, a
  # URL or no number at all (gh then reads the branch of its working directory) is not modeled.
  # The reviews a role sends, with their inline comments, are kept by the stand-in, through the
  # routes that carry `review`, the template of each ($review: the head, the login of the rig,
  # which is no member or collaborator of the repository and so has the association NONE, the
  # pull request, and the lines of each file the diff adds, the lines a comment can be on),
  # read back through those that carry `reviews`, and changed, a body updated or a review
  # dismissed, through those that carry `review_edit` (the header of gh-standin.ts). A comment a
  # role posts on a line is not kept: GitHub opens a review of its own for a line comment posted
  # alone, so from then on the reviews and their comments are a state the rig cannot state too.
  # A read or a change of them answers what was kept until the role has posted one, and from
  # then on is unmodeled. The one comment a role posts on the conversation of the pull request
  # is kept (INFRA-1303, D9: e-head-3 of the batch of record at 9c3bd9df read back the comment it
  # had posted), under $comment, its keys in the order GitHub answers them (measured on
  # cybermonklabs/legion, github-answers/README.txt): the stand-in fills its body and its two
  # times, and answers its read back, its edit and the list of the comments of the pull request
  # from what it kept (`issue_comment`, the header of gh-standin.ts). Its user is the reviewer of
  # the world as $review names it, and it names no App, which the world holds none of. The one
  # reviewer login is the one `gh api user` answers, and the viewer of GraphQL is that login too.
    {id:6002, comment_id:6101, commit_id:$head, user:{login:"rig[bot]"}, author_association:"NONE",
     html_url:$url, pull_request_url:("https://api.github.com/repos/" + $repo + "/pulls/" + $n),
     added_lines:($files | map(select(.status == "added") | {(.filename):.additions}) | add // {})}
    as $review
  | ("https://api.github.com/repos/" + $repo + "/issues/comments/6001") as $comment_url
  | {url:$comment_url, html_url:($url + "#issuecomment-6001"),
     issue_url:("https://api.github.com/repos/" + $repo + "/issues/" + $n), id:6001,
     node_id:node_id("IC"; 0; [1, 6001]), user:$review.user, created_at:null, updated_at:null,
     author_association:$review.author_association, body:null,
     reactions:{url:($comment_url + "/reactions"), total_count:0, "+1":0, "-1":0, laugh:0,
                hooray:0, confused:0, heart:0, rocket:0, eyes:0},
     performed_via_github_app:null, minimized:null, pin:null} as $comment
  | api_post("pulls/" + $n + "/comments(?:/\\d+/replies)?") as $line_comment
  | api_post("issues/" + $n + "/comments") as $issue_comment
  # The world GraphQL reads (gh-graphql.ts): the repository (its owner and name in lower case,
  # as gh-graphql.ts matches them), its pull request, the head and the base under every name
  # GraphQL resolves them by, and the viewer. The page info of the rollup ends in the cursor
  # GitHub gives its last node (gh-graphql.ts), and in none where it holds no node.
  | {graphql:{
       ("repository(name:" + ($name|ascii_downcase|tojson) + ",owner:" + ($owner|ascii_downcase|tojson) + ")"):
         ({("pullRequest(number:" + $n + ")"):{"$ref":"pr"},
           "pullRequest(*)":{"$error":{type:"NOT_FOUND",
             message:"Could not resolve to a PullRequest with the number of {number}."}},
           name:$name, nameWithOwner:$repo, owner:{login:$owner}, url:("https://github.com/" + $repo)}
          + $others.graphql),
       "repository(*)":{"$error":{type:"NOT_FOUND",
         message:"Could not resolve to a Repository with the name \u0027{owner}/{name}\u0027."}},
       viewer:{login:"rig[bot]"}},
     "$refs":{
       head:{__typename:"Commit", oid:$head,
             statusCheckRollup:{state:"SUCCESS",
               contexts:{totalCount:($checks|length),
                         pageInfo:{hasNextPage:false, endCursor:(if $checks == [] then null
                           else $checks | length | tostring | @base64 | sub("=+$"; "") end)},
                         nodes:($checks | map({__typename:"CheckRun", name, status:"COMPLETED",
                                               conclusion:"SUCCESS", detailsUrl:.link}))}}},
       base:{__typename:"Commit", oid:$base_sha, statusCheckRollup:null},
       head_ref:{__typename:"Ref", name:$ref, prefix:"refs/heads/", target:{"$ref":"head"}},
       base_ref:{__typename:"Ref", name:$base, prefix:"refs/heads/", target:{"$ref":"base"}},
       pr:{__typename:"PullRequest", id:$pr.id, number:$number, title:$title, state:"OPEN",
           url:$url, body:$body, isDraft:false, mergeable:"MERGEABLE", mergeStateStatus:"CLEAN",
           reviewDecision:null, headRefName:$ref, headRefOid:$head, baseRefName:$base,
           baseRefOid:$base_sha, createdAt:$date, headRef:{"$ref":"head_ref"},
           baseRef:{"$ref":"base_ref"},
           commits:{totalCount:1, nodes:[{commit:{"$ref":"head"}}]},
           reviewThreads:{"$keep":"threads", until:$line_comment}}}} as $graphql
  | {routes:[
    # A request of another repository answers what GitHub answers for a repository it does not
    # hold, measured for each request the archived runs make of one (github-answers/repo-404-*:
    # the 404 of the endpoint asked, which names the documentation of that endpoint); any other
    # request of another repository is not modeled. The read of a branch takes no path below
    # the branch, whose protection reads answer their own 404.
    ("repos/(?!" + ($repo | any_case) + "(?:[/?\\s]|$))[^/\\s]+/[^/\\s]+") as $other
    | def other($method; $path; $answer):
        {match:("^gh api " + $method + " " + $other + $path + "(?:\\?\\S*)?(?=\\s|$)"),
         repo_refusal:true} + github($answer);
    other("GET"; ""; "repo-404-repo"),
    other("GET"; "/pulls"; "repo-404-pulls-list"),
    other("GET"; "/pulls/\\d+"; "repo-404-pulls"),
    other("PATCH"; "/pulls/\\d+"; "repo-404-pull-patch"),
    other("GET"; "/pulls/\\d+/reviews"; "repo-404-reviews"),
    other("POST"; "/pulls/\\d+/reviews"; "repo-404-review-post"),
    other("GET"; "/commits/[^/\\s?]+/check-runs"; "repo-404-checkruns"),
    other("GET"; "/actions/runs"; "repo-404-runs-list"),
    other("GET"; "/actions/runs/\\d+"; "repo-404-runs"),
    other("GET"; "/actions/runs/\\d+/jobs"; "repo-404-run-jobs"),
    other("GET"; "/actions/jobs/\\d+"; "repo-404-job"),
    other("GET"; "/rules/branches/[^\\s?]+"; "repo-404-rules"),
    other("GET"; "/branches/[^\\s?]+/protection"; "repo-404-protection"),
    other("GET"; "/branches/[^\\s?]+/protection/required_status_checks"; "repo-404-required-checks"),
    other("GET"; "/branches/(?![^\\s?]*/protection(?:/|[?\\s]|$))[^\\s?]+"; "repo-404-branch"),
    {match:("^gh api [A-Z]+ repos/(?!" + ($repo | any_case) + "(?:[/?\\s]|$))[^/\\s]+/[^/\\s]+"),
     repo_refusal:true, unmodeled:true},
    ({match:("^gh (?:pr|repo|issue) .*\\s--repo\\s(?!(?:[^/\\s]+/)?" + ($repo | any_case)
             + "(?:\\s|$))(?:[^/\\s]+/)?(?<other>[^/\\s]+/[^/\\s]+)(?:\\s|$)"), repo_refusal:true}
     + no_repo("$<other>")),
    ({match:("^gh repo view (?!(?:[^/\\s]+/)?" + ($repo | any_case)
             + "(?:\\s|$))(?:[^/\\s]+/)?(?<other>[^/\\s]+/[^/\\s]+)(?:\\s|$)"), repo_refusal:true}
     + no_repo("$<other>")),
    ({match:("^gh run (?:view|rerun)(?=.*\\s--repo\\s(?!(?:[^/\\s]+/)?" + ($repo | any_case)
             + "(?:\\s|$))(?:[^/\\s]+/)?(?<other>[^/\\s]+/[^/\\s]+)(?:\\s|$))(?=.*\\s--job\\s(?<job>\\d+)(?:\\s|$))"),
      repo_refusal:true} + no_job("$<other>"; "$<job>")),
    ({match:("^gh run (?:view|rerun) (?<run>\\d+)(?=.*\\s--repo\\s(?!(?:[^/\\s]+/)?" + ($repo | any_case)
             + "(?:\\s|$))(?:[^/\\s]+/)?(?<other>[^/\\s]+/[^/\\s]+)(?:\\s|$))"), repo_refusal:true}
     + no_run("$<other>"; "$<run>")),
    ({match:("^gh run list(?=.*\\s--repo\\s(?!(?:[^/\\s]+/)?" + ($repo | any_case)
             + "(?:\\s|$))(?:[^/\\s]+/)?(?<other>[^/\\s]+/[^/\\s]+)(?:\\s|$))"), repo_refusal:true}
     + no_runs("$<other>")),
    {match:"^gh (?:--version|version)$",
     stdout:"gh version 2.98.0 (2026-08-20)\nhttps://github.com/cli/cli/releases/tag/v2.98.0\n"},
    ({match:("^gh pr (?:view|checks|diff|comment|review|edit) (?!" + $n + "(?:\\s|$))(?<number>\\d+)(?:\\s|$)")}
     + no_pr("$<number>")),
    {match:("^gh pr view " + $n + "(?=.*\\s--json\\s)"), head_read:"pr", stdout:$pr, reviews:"pr",
     comments:$issue_comment},
    {match:("^gh pr view " + $n + "(?=\\s|$)(?!.*\\s--(?:json|jq)\\s)"), stdout:$pr, reviews:"pr",
     program:$pr_view_text},
    # `gh pr checks --json` as gh 2.98.0 prints it (D2; fields and values measured on
    # cybermonklabs/legion#3514, 2026-11-19, prchecks-json-bogus and prchecks-json-all,
    # requests.log lines 37 and 71; exit 0 whatever the checks say: checks.go:189-190, line 51),
    # ahead of the text route, which takes no --json or --jq. Each green check ran at $date.
    {match:("^gh pr checks " + $n + "(?=.*\\s--json\\s)"), head_read:"checks_json",
     stdout:($checks | map({bucket:"pass", completedAt:$date, description:"", event:"pull_request",
                            link, name, startedAt:$date, state:"SUCCESS", workflow:"ci"}))},
    # The rows gh 2.98.0 prints to no terminal (output.go:56-61, requests.log line 53; measured on
    # #3514, ip-prchecks, line 33): the check, its bucket, its elapsed time, `0` where it started
    # and completed in the same second, as every check of the world does, its link, and its
    # description, empty, after a tab. A watch of the checks (--watch, and the --fail-fast and
    # --interval of one) is no plain read: red_fixture models it where the head is red, and in
    # this world, which models none, it is refused as unmodeled.
    {match:("^gh pr checks " + $n + "(?=\\s|$)(?!.*\\s--(?:json|jq)\\s)(?!.*\\s--(?:watch|fail-fast|interval)(?:\\s|$))"), head_read:"checks",
     stdout:($checks | map(.name + "\tpass\t0\t" + .link + "\t") | join("\n"))},
    {match:"^gh pr list(?: --|$)", stdout:$pr, program:$pr_list},
    {match:("^gh pr diff " + $n + "(?=.*\\s--name-only(?:\\s|$))"), stdout:($files | map(.filename) | join("\n"))},
    {match:("^gh pr diff " + $n + "(?:\\s|$)"), stdout:$diff},
    {match:("^gh pr comment " + $n + "(?:\\s|$)"), stdout:($url+"#issuecomment-6001"), counts_as:$issue_comment,
     issue_comment:"post"},
    {match:("^gh pr review " + $n + "(?:\\s|$)"), stdout:"", counts_as:api_post("pulls/" + $n + "/reviews"),
     review:$review},
    {match:("^gh pr edit " + $n + "(?:\\s|$)"), stdout:$url},
    {match:graphql, graphql:true, head_read:"graphql", stdout:$graphql},
    {match:$issue_comment, issue_comment:"post"},
    {match:api("issues/comments/(?<id>\\d+)"), issue_comment:"read"},
    {match:rest("PATCH"; "issues/comments/(?<id>\\d+)"; json_only), issue_comment:"edit"},
    {match:api_post("pulls/" + $n + "/reviews"), review:$review},
    {match:$line_comment, stdout:{id:6003, html_url:($url+"#discussion_r6003")}},
    # The list of the comments of the pull request answers every kept comment whatever its
    # `per_page` and `page` ask, as the list of the reviews below does.
    {match:api("issues/" + $n + "/comments"), issue_comment:"list"},
    # A limit, ruled off the measured path (the architect, 2026-11-19): the list answers every kept
    # review whatever its `per_page` and `page` ask, where GitHub answers that page (no archived
    # read of it carried a query, 0 of 126).
    ({match:api("pulls/" + $n + "/reviews"), reviews:"list"} + until_posted($line_comment)),
    ({match:api("pulls/" + $n + "/reviews/(?<id>\\d+)"), reviews:"one"} + until_posted($line_comment)),
    ({match:api("pulls/" + $n + "/reviews/(?<id>\\d+)/comments"), reviews:"review_comments"}
     + until_posted($line_comment)),
    ({match:api("pulls/" + $n + "/comments"), reviews:"comments"} + until_posted($line_comment)),
    # The edit of a review the role sent and its dismissal (INFRA-1303: trial-i-head-4 of the
    # batch of the tester at 585b19a3 updated the body of its review with a PUT of reviews/6002
    # and dismissed it with a PUT of reviews/6002/dismissals, and the rig refused both as
    # unmodeled). Each answers as changedReview of review-keep.ts states it, from the kept review
    # the `id` names, and the reads above answer the change from then on. Where each field comes
    # from:
    #   - the answer is the review object, 200 (the REST docs of GitHub, "Update a review for a
    #     pull request" and "Dismiss a review for a pull request"), its keys and their order
    #     those of a review GitHub answers (measured on 2026-11-20 through the read-only
    #     measure-gh.sh of fold13, a GET of an APPROVED, a CHANGES_REQUESTED and a COMMENTED
    #     review of cybermonklabs/legion#4606), less is_copilot_code_review and
    #     copilot_code_review_effort, which the kept review has never carried ($review, the
    #     answer of the POST above);
    #   - the `body` of an update is the `body` the call sends, required (the docs);
    #   - the `state` of a dismissal is DISMISSED (the example answer of the docs), with the
    #     `message` the call sends required and `event` DISMISS or none (the docs). A COMMENTED
    #     review answers 422 (the docs list the 422) with the message "Can not dismiss a
    #     commented pull request review", github-answers/review-dismiss-422. The message is not
    #     measured: a dismissal is a write, which the read-only token cannot make, and no
    #     archived call dismisses a COMMENTED review (the one archived dismissal, trial-i-head-4's,
    #     dismisses review 6002, which is APPROVED), so the text decides no run;
    #   - every other field is that of the kept review: the example answer of each in the docs
    #     carries the id, user, html_url, submitted_at and commit_id the review was submitted
    #     with;
    #   - the login of the rig may dismiss an APPROVED or a CHANGES_REQUESTED review: people with
    #     write access can (the docs, "Dismissing a pull request review"), and the one limit on
    #     who can is an option of a protection that requires reviews ("About protected
    #     branches"), which the protection of this base does not (required_set_fixture: its
    #     required checks alone).
    # A change whose answer from GitHub is not measured is refused as unmodeled: of a review the
    # role did not send, of a pending or a dismissed review, to an empty body, or a dismissal
    # with no message or another event. Like the reads, the two answer what was kept until the
    # role posts a line comment of its own.
    ({match:rest("PUT"; "pulls/" + $n + "/reviews/(?<id>\\d+)"; json_only), review_edit:"update"}
     + until_posted($line_comment)),
    ({match:rest("PUT"; "pulls/" + $n + "/reviews/(?<id>\\d+)/dismissals"; json_only),
      review_edit:"dismiss"} + until_posted($line_comment)),
    $others.rest[],
    # The check runs of the head read their query (check_runs_program): this world holds one
    # check run of each name, so every filter answers both.
    {match:$others.heads["check-runs"], head_read:"check-runs", program:$check_runs,
     stdout:{total_count:2, check_runs:[{name:"test", status:"completed", conclusion:"success"}, {name:"lint", status:"completed", conclusion:"success"}]}},
    # The check suites of the head, one per workflow run, which red_fixture writes from the runs
    # of its world; this world holds no run.
    {match:$others.heads["check-suites"], head_read:"check-suites", unmodeled:true},
    {match:$others.heads.status, stdout:combined_status($head)},
    {match:$others.heads.statuses, stdout:[]},
    # The repository, its commits, trees, refs and root contents, and its one pull request.
    {match:api(""), stdout:(repository + {default_branch:$base})},
    {match:("^gh repo view(?: (?:[^/\\s]+/)?" + ($repo | any_case) + ")?(?=.*\\s--json\\s)"),
     stdout:{name:$name, nameWithOwner:$repo, owner:{login:$owner},
             url:("https://github.com/" + $repo), defaultBranchRef:{name:$base}, isPrivate:false,
             visibility:"PUBLIC"}},
    {match:api("commits/" + $others.forms.base), stdout:($commits[0])},
    {match:api("commits/" + $others.forms.head), stdout:($commits[1])},
    # The comparison of the base with the head (trial-l-ablate-4), as GitHub answers it, its keys
    # in the order measured on cybermonklabs/legion (2026-11-19, compare, requests.log line 67):
    # its URLs name the two refs as the call gave them and its permalink each
    # commit by its owner and seven digits; the head is one commit ahead of the base, the merge
    # base is the base, and the commit and the files are those of the pull request, each as the
    # other reads of this world answer it. A ref the world does not hold, on either side, is
    # the 404 of GitHub (compare-404, measured with an unknown base, requests.log line 69);
    # another pair of refs the world holds (the head before the base, a commit with itself) is
    # not modeled.
    (("https://github.com/" + $repo + "/compare/$<cbase>...$<chead>") as $compare_html
     | ("(?:" + $others.forms.base + "|" + $others.forms.head + ")") as $either
     | {match:api("compare/(?<cbase>" + $others.forms.base + ")\\.\\.\\.(?<chead>" + $others.forms.head + ")"),
        stdout:{url:("https://api.github.com/repos/" + $repo + "/compare/$<cbase>...$<chead>"),
                html_url:$compare_html,
                permalink_url:("https://github.com/" + $repo + "/compare/" + $owner + ":" + $base_sha[0:7]
                               + "..." + $owner + ":" + $head[0:7]),
                diff_url:($compare_html + ".diff"), patch_url:($compare_html + ".patch"),
                base_commit:$commits[0], merge_base_commit:$commits[0], status:"ahead",
                ahead_by:1, behind_by:0, total_commits:1, commits:[$commits[1]], files:$files}},
       ({match:api("compare/(?!" + $either + "\\.\\.\\.)[^?\\s]*\\.\\.\\.[^?\\s]+")} + github("compare-404")),
       ({match:api("compare/" + $either + "\\.\\.\\.(?!" + $either + "(?:[?\\s]|$))[^?\\s]+")}
        + github("compare-404"))),
    {match:api("git/trees/" + $others.forms.base), stdout:tree_of($base_sha), program:$trees_query},
    {match:api("git/trees/" + $others.forms.head), stdout:tree_of($head), program:$trees_query},
    {match:api("git/refs?/heads/" + ($base | literal)), stdout:ref_of($base_sha; $base)},
    {match:api("git/refs?/heads/" + ($ref | literal)), stdout:ref_of($head; $ref)},
    ({match:api("git/ref/heads/\\S+")} + github("gitref-404")),
    ({match:api("git/refs/heads/\\S+")} + github("gitrefs-404")),
    $listings[],
    $contents_beyond[],
    {match:api("branches/" + ($ref | literal | gsub("/"; "(?:/|%2[Ff])"))),
     stdout:{name:$ref, commit:{sha:$head, url:("https://api.github.com/repos/" + $repo + "/commits/" + $head)},
             protected:false}},
    ({match:api("branches/(?!" + ($base | literal) + "(?:[/?\\s]|$))[^/?\\s]+")} + github("branch-404")),
    {match:api("rules/branches/(?!" + ($base | literal) + "(?:[?\\s]|$))[^?\\s]+"), stdout:[]},
    # GitHub serves no list of check runs and no `runs` under a repository (workflow runs are
    # under actions/runs, check runs under check-runs and a commit), so a read of either answers
    # its 404 for a route it does not have (round 6 trial-c-ablate-3 read check run 9001008 so).
    ({match:api("check-runs")} + github("checkruns-list-404")),
    ({match:api("runs(?:/\\S*)?")} + github("noroute-404")),
    # The diff as GitHub serves it, which ends with the newline git ends a diff with ($diff is
    # read through $(…), which drops it); gh api prints a body as it stands.
    {match:rest("GET"; "pulls/" + $n;
                "(?=.*\\s--header\\s[Aa]ccept:\\u001f*application/vnd\\.github(?:\\.v3)?\\.diff(?:\\s|$))"),
     stdout:($diff + "\n")},
    # A limit, ruled off the measured path (the architect, 2026-11-19): the files answer whole
    # whatever `per_page` and `page` ask, where GitHub answers that page (0 of 4 archived reads
    # carried a query).
    {match:api("pulls/" + $n + "/files"), stdout:$files},
    {match:api("pulls"), head_read:"pulls", stdout:[$pull], program:$pulls_query},
    # The pull request answers whatever parameter its read carries: GitHub ignores a parameter
    # the endpoint does not take, so the answer is GitHub, but it is no read of every parameter
    # (the architect, 2026-11-19, named a limit).
    {match:api("pulls/" + $n), head_read:"pull", stdout:$pull},
    {match:rest("PATCH"; "pulls/" + $n; json_only), head_read:"pull", stdout:$pull},
    # Another pull request number, its reviews or its comments: the 404 of GitHub (measured).
    ({match:api("pulls/(?!" + $n + "(?:[/?\\s]|$))\\d+")} + github("pull-404")),
    ({match:api("pulls/(?!" + $n + "/)\\d+/reviews")} + github("reviews-404")),
    ({match:api("issues/(?!" + $n + "/)\\d+/comments")} + github("comments-404")),
    {match:"^gh api GET user(?:\\s|$)", stdout:{login:"rig[bot]"}}
  ], comment_template:$comment};

# required_set: its input, fixtures.json, with the reads of the required set of the base branch
# $base ahead of every route, each answering that $base requires the checks of the JSON list
# $required and no other, in that order, as red_fixture's `gh pr checks --required` route answers
# it (red_fixture hands both the world's `required`, red_world: `["ci"]`, and `["ci", "title"]` in
# `p`; a ruleset of two checks names both, `Require ci and title`, and one rule holds both, as one
# ruleset's rule lists every check it requires): the rules GitHub applies to the base, through
# its rulesets (`rules/branches/<base>`),
# which the prompt of the reviewer reads (packages/daemon/internal/prompts/go/reviewer.md, "CI is
# yours to read"); the rulesets of the repository, their list and the one ruleset, which applies
# to the default branch and holds that same rule; the base branch, read for its protection, which
# the prompt reads too; and the protection of the base on its own endpoint, the same required
# checks in the shape that endpoint answers them in. They read nothing of the world but the
# repository ($repo), $base and $base_sha, which red_fixture reads from world.json. Each
# answers unfiltered, since the stand-in applies the `--jq` of a call itself, and the branch
# answers its name, its commit ($base_sha, so a read of `.commit.sha` names the base rather than
# null, which sent runs of rounds 6 and 9 on to read `commits/null/check-runs`) and its protection.
# The required status checks of the protection answer on their own endpoint too, and its contexts.
# They come ahead of every other route: in the batch of
# 2026-11-17 the `--jq` text of the two reads of the prompt held `required_status_checks`, so the
# generic `(check-runs|status)` route took both and each filter, run over a check-runs answer,
# exited 1, and the rules read without its `--jq` (trial-i-head-3), the protection read
# (trial-i-head-1) and the rulesets list (trial-i-head-5) each fell to the 404, so every reviewer
# read the base as requiring nothing. Each match is an `api` read (above), which names the
# repository and ends where its own path ends, so none takes the path of another: the branch
# route never takes the `/protection` below it, nor the rulesets list the one ruleset, and the
# rules path has its `rules/` between the repository and `branches/`.
def required_set:
  ("https://api.github.com/repos/" + $repo) as $api
  | ($api + "/branches/" + $base + "/protection") as $protection_url
  | {contexts:$required, checks:($required | map({context:., app_id:null}))} as $status_checks
  | ({url:($protection_url + "/required_status_checks"), strict:false,
      contexts_url:($protection_url + "/required_status_checks/contexts")} + $status_checks)
    as $required_status_checks
  | {id:1, name:("Require " + ($required | join(" and "))), target:"branch", source_type:"Repository",
     source:$repo, enforcement:"active"} as $ruleset
  | {type:"required_status_checks",
     parameters:{strict_required_status_checks_policy:false,
                 required_status_checks:($required | map({context:.}))}} as $rule
  | ("branches/" + ($base | literal)) as $branch
  | [{match:api("rules/" + $branch),
      stdout:[$rule + {ruleset_source_type:$ruleset.source_type, ruleset_source:$ruleset.source,
                       ruleset_id:$ruleset.id}]},
     {match:api("rulesets"), stdout:[$ruleset]},
     {match:api("rulesets/" + ($ruleset.id|tostring)),
      stdout:($ruleset + {bypass_actors:[],
                          conditions:{ref_name:{include:["~DEFAULT_BRANCH"], exclude:[]}},
                          rules:[$rule]})},
     {match:api($branch),
      stdout:{name:$base, commit:{sha:$base_sha, url:($api + "/commits/" + $base_sha)},
              protected:true,
              protection:{enabled:true,
                          required_status_checks:({enforcement_level:"non_admins"}
                                                  + $status_checks)},
              protection_url:$protection_url}},
     {match:api($branch + "/protection"),
      stdout:{url:$protection_url, required_status_checks:$required_status_checks,
              enforce_admins:{url:($protection_url + "/enforce_admins"), enabled:false}}},
     {match:api($branch + "/protection/required_status_checks"), stdout:$required_status_checks},
     {match:api($branch + "/protection/required_status_checks/contexts"), stdout:$required}]
    as $required_set
  | .routes = $required_set + .routes;

# What red_model, run_routes and red_rewrite share over the facts of the red, which the program
# that reads them binds ($repo, $pr, $head, $partner, $run_id, $required, $facts): the repository's API,
# whether the red has a partner, the latest attempt of each job of a run, a check suite's id, the
# counted set's verdicts and merge state, the settlement the re-run publishes, and a run and a job
# in the shape gh answers them.
def repo_api: "https://api.github.com/repos/" + $repo;
def paired: $partner != "";
# The jobs of a state of a run as its jobs read answers them: the latest attempt of each job.
def latest: .attempts as $attempts
  | reduce $attempts[1:][] as $later ($attempts[0];
      map(. as $j | ([$later[] | select(.name == $j.name)] | first) // $j));
# The check suite of a run: a rig id made as the run ids are, the run id less a million.
def check_suite_id($run): $run - 1000000;
# The app of every check run and check suite of GitHub Actions, as GitHub answered it on
# cybermonklabs/legion (check-runs/135286572101, measured by the reviewer, 2026-11-19,
# infra-1303-github-fields-measured/cr_ok.json; the same in cr_fail.json).
def actions_app: {
  id:15368,
  client_id:"Iv1.05c79e9ad1f6bdfa",
  slug:"github-actions",
  node_id:"MDM6QXBwMTUzNjg=",
  owner:{
    login:"github",
    id:9919,
    node_id:"MDEyOk9yZ2FuaXphdGlvbjk5MTk=",
    avatar_url:"https://avatars.githubusercontent.com/u/9919?v=4",
    gravatar_id:"",
    url:"https://api.github.com/users/github",
    html_url:"https://github.com/github",
    followers_url:"https://api.github.com/users/github/followers",
    following_url:"https://api.github.com/users/github/following{/other_user}",
    gists_url:"https://api.github.com/users/github/gists{/gist_id}",
    starred_url:"https://api.github.com/users/github/starred{/owner}{/repo}",
    subscriptions_url:"https://api.github.com/users/github/subscriptions",
    organizations_url:"https://api.github.com/users/github/orgs",
    repos_url:"https://api.github.com/users/github/repos",
    events_url:"https://api.github.com/users/github/events{/privacy}",
    received_events_url:"https://api.github.com/users/github/received_events",
    type:"Organization",
    user_view_type:"public",
    site_admin:false},
  name:"GitHub Actions",
  description:"Automate your workflow from idea to production",
  external_url:"https://help.github.com/en/actions",
  html_url:"https://github.com/apps/github-actions",
  created_at:"2018-07-30T09:30:17Z",
  updated_at:"2026-06-18T16:17:48Z",
  permissions:{
    actions:"write", administration:"read", artifact_metadata:"write",
    attestations:"write", checks:"write", code_quality:"write",
    contents:"write", copilot_requests:"write", deployments:"write",
    discussions:"write", drives:"write", issues:"write",
    merge_queues:"write", metadata:"read", models:"read",
    packages:"write", pages:"write", pull_requests:"write",
    repository_hooks:"write", repository_projects:"write", security_events:"write",
    statuses:"write", vulnerability_alerts:"read"},
  events:[
    "branch_protection_rule", "check_run", "check_suite", "create", "delete",
    "deployment", "deployment_status", "discussion", "discussion_comment", "fork",
    "gollum", "issues", "issue_comment", "label", "merge_group",
    "milestone", "page_build", "public", "pull_request", "pull_request_review",
    "pull_request_review_comment", "push", "registry_package", "release", "repository",
    "repository_dispatch", "status", "watch", "workflow_dispatch", "workflow_run"]};
# The workflow of a run, one of the three of the world, each with a rig id of its own (`ci`, also
# for the run by hand; `title` in `f`; `e2e` in `e`), and the URL of the attempt before an attempt
# after the first, as the runs of GitHub name it (measured on cybermonklabs/legion run
# 44406867953, 2026-11-18: `previous_attempt_url` null on attempt 1, and
# `…/actions/runs/<id>/attempts/1` on attempt 2).
def workflow_id($name): {ci:1, title:2, e2e:3}[$name];
def previous_attempt($id; $attempt): if $attempt > 1
  then repo_api + "/actions/runs/" + ($id|tostring) + "/attempts/" + ($attempt - 1 | tostring)
  else null end;
# One state of the counted set as its reads answer it: the `gh pr view` rollup, the rows of
# `gh pr checks`, and the GraphQL rollup of the head (below), all over the latest counted run
# of each name. The link each carries is the details_url of the check run, the page of the job
# that ran it, which is the link GitHub gives a check run of GitHub Actions and the one
# `gh pr checks` prints. The merge state of the pull request follows from them and from the
# checks the base requires (requires). A check run not yet completed (the attempt the re-run
# made, in the turn that pressed it) is neither failing nor passing: it is pending.
def failing($checks): $checks | map(select(.conclusion == "failure") | .name) | sort;
def passing($checks): $checks | map(select(.conclusion == "success") | .name) | sort;
def pending($checks): $checks | map(select(.status != "completed") | .name) | sort;
def verdict: if .status != "completed" then null
  elif .conclusion == "failure" then "FAILURE" else "SUCCESS" end;
def bucket: if .status != "completed" then "pending"
  elif .conclusion == "failure" then "fail" else "pass" end;
# The time gh prints for a check that completed: its completion less its start, as Go prints
# a Duration, and `0` where there is none (output.go:56-61 at v2.98.0, requests.log line 53).
def elapsed: if .status == "completed"
  then ((.completed_at | fromdate) - (.started_at | fromdate)) else 0 end
  | (. / 3600 | floor) as $h | ((. % 3600) / 60 | floor) as $m | (. % 60) as $s
  | if . <= 0 then "0" elif $h > 0 then "\($h)h\($m)m\($s)s" elif $m > 0 then "\($m)m\($s)s"
    else "\($s)s" end;
# The rows gh 2.98.0 prints to no terminal (output.go:47-63, measured on
# cybermonklabs/legion#3514, 2026-11-19, ip-prchecks, requests.log line 33): the check, its
# bucket, its time, its link and its description, which no check of the world carries, so
# every row ends in a tab.
def rows($checks): $checks
  | map(.name + "\t" + bucket + "\t" + elapsed + "\t" + .details_url + "\t")
  | join("\n");
# Its exit status: 1 while a check fails, 8 while none fails and one is pending, 0 when all
# passed (checks.go:248-251, ghcmd cmd.go:201-202 at v2.98.0, requests.log lines 51 and 61; 8
# measured on #3514, ip-prchecks, line 33).
def checks_exit($checks): if (failing($checks) | length) > 0 then 1
  elif (pending($checks) | length) > 0 then 8 else 0 end;
# Whether a check is one the base requires (the world's `required`, red_world): `ci`, and in
# `p` the title check too.
def requires: .name as $name | any($facts.required[]; . == $name);
# BLOCKED while a check the base requires is missing or has not passed, whatever the others
# say, so a red required check beside a green one blocks as a missing one does (`p`).
def merge_state($checks): ($checks | map(select(requires))) as $requires
  | if any($facts.required[]; . as $name | all($requires[]; .name != $name))
       or any($requires[]; .conclusion != "success") then "BLOCKED"
    elif any($checks[]; .conclusion == "failure") then "UNSTABLE" else "CLEAN" end;
def run_url: "https://github.com/" + $repo + "/actions/runs/" + ($run_id|tostring);
# The settlement as the Envoy listener renders one (packages/envoy/internal/cistore/render.go),
# and the envelope that carries it to the listener of the run.
def settlement($checks):
  {kind:"checks", repo:$repo, number:($pr|tostring), sha:$head,
   check_runs:($checks | map({name:.name, id:.id}) | sort_by(.name)),
   generation:2, snapshot:$head[0:8],
   failed:{count:(failing($checks)|length), checks:failing($checks)},
   running:{count:0, checks:[]},
   passed:{count:(passing($checks)|length), checks:passing($checks)},
   queued:{count:0, checks:[]},
   cancelled:{count:0, checks:[]},
   skipped:{count:0, checks:[]},
   failing_checks:(failing($checks) | map({name:., url:run_url}))};
def published($checks):
  settlement($checks) as $settled
  | ($repo | split("/")) as [$owner, $repo_name]
  | {topic:("notifications.github." + $owner + "." + $repo_name + ".pr."
            + ($pr|tostring) + ".checks"),
     source:"github",
     message:("checks settled on " + $repo + "#" + ($pr|tostring) + " @ " + $head[0:7]
              + ": " + ($settled.passed.count|tostring) + " passed, "
              + ($settled.failed.count|tostring) + " failed, 0 cancelled, 0 skipped"),
     payload:($settled|tojson)};
# A run and a job in the shape `gh run view --json` and `gh run list --json` answer them, with
# the field names gh 2.98.0 lists for those flags: a conclusion not yet reached is "", a time not
# yet reached the zero time of Go, and each step carries its start and completion (measured on
# cybermonklabs/legion run 44586483868 while it ran, 2026-11-19, ip-runview-json, requests.log
# line 27).
def zero: . // "0001-01-01T00:00:00Z";
def gh_job: {completedAt:(.completed_at | zero), conclusion:(.conclusion // ""), databaseId:.id,
  name, startedAt:(.started_at | zero), status,
  steps:(.steps | map({completedAt:(.completed_at | zero), conclusion:(.conclusion // ""), name,
                       number, startedAt:(.started_at | zero), status})),
  url:.html_url};
def gh_run($jobs): {attempt:.run_attempt, conclusion:(.conclusion // ""), createdAt:.created_at,
  databaseId:.id, displayTitle:.display_title, event, headBranch:.head_branch,
  headSha:.head_sha, jobs:($jobs | map(gh_job)), name, number:.run_number,
  startedAt:.run_started_at, status, updatedAt:.updated_at, url:.html_url,
  workflowDatabaseId:.workflow_id, workflowName:.name};

# red_model: the model of the red of $world as one JSON object, from worker_fixture's world (the
# pull request $pr, its head $head on $branch into $base at $base_sha, its title $title, the
# repository $repo and the head's commit $head_commit), the world's facts ($facts, red_world) and
# its workflows ($workflows, red_workflows; the red's $check, $job, $test_name, $log, $log_gone,
# $world_log, $run_name, $lanes from their lane), the check the base requires ($required), its
# partner ($partner) and its ids ($run_id, $job_id, $red_id): the facts of the red, the runs of
# the head, each in its three states, before the re-run, while the re-run's attempt runs (the turn
# that pressed it) and once it settled, with their jobs, and the check runs of the head in each
# state, the counted set among them as its reads answer it.
def red_model:
  repo_api as $api
  | paired as $paired
  # The objects of the head in the shape GitHub answers them, key for key and in the order GitHub
  # writes the keys (measured by the reviewer on cybermonklabs/legion, 2026-11-19,
  # infra-1303-github-fields-measured: a check run, a job and a run of GitHub Actions), each field
  # from the world: the node id GitHub gives the object (node_id, above); the user that
  # triggers every run, the one user of the world, rig[bot], the Bot that opened the pull request
  # (worker_fixture), whose id and node id and avatar the world does not hold, so they are left
  # out; the repository as a run names it, whose owner is a login and its URLs alone, for the same
  # reason; and the pull request as a check run or a run of its head names it, the minimal shape
  # GitHub gives it there (measured: url, id, number, its head and base with their repository).
  # A check run and its job share the node id of the check run; a run, its check suite and the
  # repository each have their own (the repository id is the world one, repository above, 1).
  | def node($kind; $id): node_id($kind; (if $kind == "WFR" then 2 else 0 end); [1, $id]);
  def user($login):
    ($login | @uri) as $path
    | {login:$login, gravatar_id:"", url:("https://api.github.com/users/" + $path),
       html_url:(if ($login | endswith("[bot]"))
                 then "https://github.com/apps/" + ($login | rtrimstr("[bot]"))
                 else "https://github.com/" + $login end),
       followers_url:("https://api.github.com/users/" + $path + "/followers"),
       following_url:("https://api.github.com/users/" + $path + "/following{/other_user}"),
       gists_url:("https://api.github.com/users/" + $path + "/gists{/gist_id}"),
       starred_url:("https://api.github.com/users/" + $path + "/starred{/owner}{/repo}"),
       subscriptions_url:("https://api.github.com/users/" + $path + "/subscriptions"),
       organizations_url:("https://api.github.com/users/" + $path + "/orgs"),
       repos_url:("https://api.github.com/users/" + $path + "/repos"),
       events_url:("https://api.github.com/users/" + $path + "/events{/privacy}"),
       received_events_url:("https://api.github.com/users/" + $path + "/received_events")}
      + (if ($login | endswith("[bot]")) then {type:"Bot"} else {} end)
      + {user_view_type:"public", site_admin:false};
  def actor: user("rig[bot]");
  ($repo | split("/")) as [$owner, $name]
  | def repository_full:
      {id:1, node_id:node_id("R"; 0; [1]), name:$name, full_name:$repo, private:false, owner:user($owner),
       html_url:("https://github.com/" + $repo), description:null, fork:false, url:$api}
      + ([["forks", "/forks"], ["keys", "/keys{/key_id}"],
          ["collaborators", "/collaborators{/collaborator}"], ["teams", "/teams"],
          ["hooks", "/hooks"], ["issue_events", "/issues/events{/number}"], ["events", "/events"],
          ["assignees", "/assignees{/user}"], ["branches", "/branches{/branch}"], ["tags", "/tags"],
          ["blobs", "/git/blobs{/sha}"], ["git_tags", "/git/tags{/sha}"],
          ["git_refs", "/git/refs{/sha}"], ["trees", "/git/trees{/sha}"],
          ["statuses", "/statuses/{sha}"], ["languages", "/languages"],
          ["stargazers", "/stargazers"], ["contributors", "/contributors"],
          ["subscribers", "/subscribers"], ["subscription", "/subscription"],
          ["commits", "/commits{/sha}"], ["git_commits", "/git/commits{/sha}"],
          ["comments", "/comments{/number}"], ["issue_comment", "/issues/comments{/number}"],
          ["contents", "/contents/{+path}"], ["compare", "/compare/{base}...{head}"],
          ["merges", "/merges"], ["archive", "/{archive_format}{/ref}"],
          ["downloads", "/downloads"], ["issues", "/issues{/number}"], ["pulls", "/pulls{/number}"],
          ["milestones", "/milestones{/number}"],
          ["notifications", "/notifications{?since,all,participating}"],
          ["labels", "/labels{/name}"], ["releases", "/releases{/id}"],
          ["deployments", "/deployments"]]
         | map({key:(.[0] + "_url"), value:($api + .[1])}) | from_entries);
    def repo_minimal: {id:1, url:$api, name:$name};
    def pull_minimal:
      {url:($api + "/pulls/" + ($pr|tostring)), id:$pr, number:$pr,
       head:{ref:$branch, sha:$head, repo:repo_minimal},
       base:{ref:$base, sha:$base_sha, repo:repo_minimal}};
  # A check run before the fields GitHub derives from its job (linked, below).
  def entry($id; $name; $conclusion; $at):
    {id:$id, name:$name, status:"completed", conclusion:$conclusion, started_at:$at,
     completed_at:$at};
  "2026-03-04T10:00:00Z" as $t0
  | "2026-03-04T10:15:00Z" as $t_hand
  | "2026-03-04T10:31:08Z" as $t1
  | (if $facts.after == "green" then "success" else "failure" end) as $after
  | (if $run_name == "ci" then $run_id else $run_id + 2 end) as $ci_run
  | (if $facts.over_lane then [$red_id + 3, $red_id] else [$red_id, $red_id + 3] end)
    as [$lane_check, $aggregate_check]
  # The workflow each run of the head belongs to, by the run id.
  | ({($run_id|tostring):$run_name, ($ci_run|tostring):"ci", ($run_id - 1 | tostring):"e2e",
      ($run_id + 1 | tostring):"ci"}) as $workflow_name_of
  # The jobs of the head, each the job of one check run (its check_run_url), each with an id of
  # its own. The run of the red holds the failing lane and, beside it, lint and typecheck (in `f`
  # and `p` those two are the jobs of the `ci` run, the title check running in a run of its own)
  # and, where the head carries an aggregate over the lane, the aggregate `ci` job that needs the
  # lanes; the `ci` run of `p` holds the aggregate over lint and typecheck, green ($green_aggregate),
  # with the id the aggregate has where it is red. The spent e2e run of `e` holds its two
  # attempts, and the by-hand run of `k` its one
  # job. A job carries the node id of its check run (measured: the two share it), runs on a
  # hosted runner of the label cybermonklabs/legion runs its CI on (measured, ubuntu-24.04),
  # whose id the world does not hold: it is made from the job id in the range GitHub gives
  # hosted runners (10 digits, measured), and named `GitHub Actions <id>` as GitHub names one.
  # Its steps are GitHub own `Set up job`, then the steps of the job of that name in the world
  # workflows (red_workflows, which worker_fixture committed), in their order and by the name
  # GitHub gives each, so a job and its workflow file name the same steps.
  | def model_job($name): first($workflows[].jobs[] | select(.name == $name));
    def job($id; $run; $attempt; $name; $conclusion; $at; $check_id):
      (1000000000 + $id) as $runner
      | {id:$id, run_id:$run, workflow_name:$workflow_name_of[($run|tostring)], head_branch:$branch,
         run_url:($api + "/actions/runs/" + ($run|tostring)), run_attempt:$attempt,
         node_id:node("CR"; $check_id), head_sha:$head,
         url:($api + "/actions/jobs/" + ($id|tostring)),
         html_url:("https://github.com/" + $repo + "/actions/runs/" + ($run|tostring) + "/job/"
                   + ($id|tostring)),
         status:"completed", conclusion:$conclusion, created_at:$at, started_at:$at,
         completed_at:$at, name:$name,
         steps:([{name:"Set up job", status:"completed", conclusion:"success", number:1,
                  started_at:$at, completed_at:$at}]
                + (model_job($name).steps | to_entries
                   | map({name:(.value | step_name), status:"completed", conclusion:$conclusion,
                          number:(.key + 2), started_at:$at, completed_at:$at}))),
         check_run_url:($api + "/check-runs/" + ($check_id|tostring)),
         labels:["ubuntu-24.04"], runner_id:$runner,
         runner_name:("GitHub Actions " + ($runner|tostring)), runner_group_id:0,
         runner_group_name:"GitHub Actions"};
    # A job of the re-run attempt while it runs, in the turn that pressed it (measured on
    # cybermonklabs/legion run 44586483868 while it ran, 2026-11-19, ip-job and ip-jobs,
    # requests.log lines 11 and 9): in progress, its set-up step done, its first own step in
    # progress and the rest pending, no
    # conclusion and no completion; and a job that waits for the jobs it needs, the aggregate,
    # queued, started when it was made, with no step and no runner yet.
    def running_job: .status = "in_progress" | .conclusion = null | .completed_at = null
      | .steps |= (.[:1] + (.[1:] | to_entries
          | map(.value + (if .key == 0 then {status:"in_progress", conclusion:null, completed_at:null}
                          else {status:"pending", conclusion:null, started_at:null, completed_at:null} end))));
    def queued_job: .status = "queued" | .conclusion = null | .started_at = .created_at
      | .completed_at = null | .steps = [] | .runner_id = null | .runner_name = null
      | .runner_group_id = null | .runner_group_name = null;
    ($facts.aggregate and ($paired | not)) as $green_aggregate
  | ([job($job_id + 1; $ci_run; 1; "lint"; "success"; $t0; $red_id - 2),
      job($job_id + 2; $ci_run; 1; "typecheck"; "success"; $t0; $red_id - 1)]
     + (if $green_aggregate then [job($job_id + 3; $ci_run; 1; $required; "success"; $t0; $red_id + 3)]
        else [] end))
    as $ci_jobs
  | ([job($job_id; $run_id; 1; $job; "failure"; $t0; $lane_check)]
     + (if $run_name == "ci" then $ci_jobs else [] end)
     + (if $paired then [job($job_id + 3; $run_id; 1; $required; "failure";
                            $t0; $aggregate_check)] else [] end)) as $red_jobs
  # Attempt 2 of the run of the red is the re-run of its failed jobs, the lane and the
  # aggregate, and it lists every job of the run, as GitHub lists one (measured on
  # cybermonklabs/legion run 44406867953, 2026-11-19, github-answers/README.txt: 58
  # jobs at attempt 2, 2 re-run and 56 carried). A job it did not re-run is carried to attempt 2
  # with an id and a check run of its own (the commit lists 122 check runs where 64 names stand,
  # github-answers/README.txt), its attempt-1 conclusion, steps, runner and times, and the time
  # the re-run made it (created_at), in the order of attempt 1.
  | def carried($job; $id; $check_id):
      $job + {id:$id, run_attempt:2, node_id:node("CR"; $check_id),
              url:($api + "/actions/jobs/" + ($id|tostring)),
              html_url:("https://github.com/" + $repo + "/actions/runs/" + ($job.run_id|tostring)
                        + "/job/" + ($id|tostring)),
              created_at:$t1, check_run_url:($api + "/check-runs/" + ($check_id|tostring))};
    (if $run_name == "ci"
     then [carried($ci_jobs[0]; $job_id + 7; $red_id + 5), carried($ci_jobs[1]; $job_id + 8; $red_id + 6)]
     else [] end) as $carried_jobs
  | ([job($job_id + 4; $run_id; 2; $job; $after; $t1; $lane_check + 1)]
     + $carried_jobs
     + (if $paired then [job($job_id + 5; $run_id; 2; $required; $after; $t1;
                            $aggregate_check + 1)] else [] end)) as $rerun_jobs
  # The same attempt while it runs: the lane in progress, the jobs it carried as they stand, and
  # the aggregate queued behind the lane.
  | ([$rerun_jobs[0] | running_job] + $carried_jobs
     + (if $paired then [$rerun_jobs[-1] | queued_job] else [] end)) as $running_jobs
  # Each run is one check suite of GitHub Actions, as GitHub gives a workflow run one: its id is a
  # rig id made as the run ids are (the run id less a million). It names its workflow and the
  # attempt before it (previous_attempt), the file of its workflow under .github/workflows, which the
  # world commits (red_workflows: `ci.yaml`, as cybermonklabs/legion names its CI;
  # `pr-title.yaml`; `e2e.yaml`), no workflow it calls, the
  # commit of the head as the remote holds it, the user that triggered it, and the pull request,
  # whose head the run ran at, whatever triggered it: a `workflow_dispatch` run of the head of
  # an open pull request names it too (measured on cybermonklabs/legion, 2026-11-19,
  # github-answers/README.txt: every one of the 30 latest workflow_dispatch runs lists one pull
  # request; run 44457284390, at the current head of #3729, lists #3729, and so do its check runs).
  | def run($id; $name; $event; $attempt; $conclusion; $at):
      ($api + "/actions/runs/" + ($id|tostring)) as $url
      | {id:$id, name:$name, node_id:node("WFR"; $id), head_branch:$branch, head_sha:$head,
         path:(".github/workflows/" + first($workflows[] | select(.name == $name) | .file)),
         display_title:$title, run_number:$pr, event:$event, status:"completed",
         conclusion:$conclusion, workflow_id:workflow_id($name),
         check_suite_id:check_suite_id($id), check_suite_node_id:node("CS"; check_suite_id($id)),
         url:$url, html_url:("https://github.com/" + $repo + "/actions/runs/" + ($id|tostring)),
         pull_requests:[pull_minimal],
         created_at:$at, updated_at:$at, actor:actor, run_attempt:$attempt,
         referenced_workflows:[], run_started_at:$at, triggering_actor:actor,
         jobs_url:($url + "/jobs"), logs_url:($url + "/logs"),
         check_suite_url:($api + "/check-suites/" + (check_suite_id($id)|tostring)),
         artifacts_url:($url + "/artifacts"), cancel_url:($url + "/cancel"),
         rerun_url:($url + "/rerun"), previous_attempt_url:previous_attempt($id; $attempt),
         workflow_url:($api + "/actions/workflows/" + (workflow_id($name)|tostring)),
         head_commit:$head_commit, repository:repository_full, head_repository:repository_full};
    # The runs of the head, each in the three states of the head, before the re-run, while the
    # re-run runs and once it settled: only the run of the red moves, to its attempt 2. While
    # that attempt runs the run reads queued where a job of it is queued and in progress
    # otherwise, as GitHub reads one (measured: run 44586483868 read queued beside its queued
    # job, ip-run and ip-jobs, requests.log lines 7 and 9), with no conclusion.
    ([{id:$run_id, states:[
        {run:run($run_id; $run_name; "pull_request"; 1; "failure"; $t0), attempts:[$red_jobs]},
        {run:(run($run_id; $run_name; "pull_request"; 2; null; $t0)
              + {status:(if any($running_jobs[]; .status == "queued") then "queued" else "in_progress" end),
                 updated_at:$t1}),
         attempts:[$red_jobs, $running_jobs]},
        {run:(run($run_id; $run_name; "pull_request"; 2; $after; $t0) + {updated_at:$t1}),
         attempts:[$red_jobs, $rerun_jobs]}]}]
     + (if $run_name == "ci" then [] else
          [{id:$ci_run, states:([{run:run($ci_run; "ci"; "pull_request"; 1; "success"; $t0),
                                  attempts:[$ci_jobs]}] | . + . + .)}] end)
     + (if $facts.spent then
          [{id:($run_id - 1),
            states:([{run:run($run_id - 1; "e2e"; "pull_request"; 2; "success"; $t0),
                      attempts:[[job($job_id - 2; $run_id - 1; 1; "e2e"; "failure";
                                     $t0; $red_id - 4)],
                                [job($job_id - 1; $run_id - 1; 2; "e2e"; "success";
                                     $t0; $red_id - 3)]]}] | . + . + .)}]
        else [] end)
     + (if $facts.by_hand then
          [{id:($run_id + 1),
            states:([{run:(run($run_id + 1; "ci"; "workflow_dispatch"; 1; "failure"; $t_hand)
                           + {display_title:"ci"}),
                      attempts:[[job($job_id + 6; $run_id + 1; 1; $job; "failure";
                                     $t_hand; $red_id + 2)]]}] | . + . + .)}]
        else [] end)) as $runs
  | ([$runs[].states[-1].attempts[][]] | unique_by(.id)) as $all_jobs
  | ($all_jobs | map({key:(.check_run_url | split("/") | last), value:.}) | from_entries)
    as $job_of
  # A check run in the shape GitHub answers a check run of GitHub Actions (measured): its node id
  # (its job carries the same), the head it ran at, an external id in the shape of GitHub (a
  # UUID, which the world does not hold: made from the check run id), its page, which is the page
  # of its job, an output with no title, summary or text and the count of its annotations (the
  # annotations route below answers one for a check that failed, none for one that passed), the
  # check suite of the run its job belongs to, the GitHub Actions app, and the pull requests of
  # that run: the pull request while the head is its head, which it is for the whole run (no
  # push moves it), the by-hand `workflow_dispatch` run of `k` among them (measured, above).
  | ($runs | map({key:(.id|tostring), value:.states[0].run.pull_requests}) | from_entries)
    as $pull_requests_of
  | def linked: $job_of[(.id|tostring)] as $job
      | {id, name, node_id:node("CR"; .id), head_sha:$head,
         external_id:("00000000-0000-5000-8000-" + ("000000000000" + (.id|tostring))[-12:]),
         url:($api + "/check-runs/" + (.id|tostring)), html_url:$job.html_url,
         details_url:$job.html_url, status, conclusion, started_at, completed_at,
         output:{title:null, summary:null, text:null,
                 annotations_count:(if .conclusion == "failure" then 1 else 0 end),
                 annotations_url:($api + "/check-runs/" + (.id|tostring) + "/annotations")},
         check_suite:{id:check_suite_id($job.run_id)}, app:actions_app,
         pull_requests:$pull_requests_of[($job.run_id|tostring)]};
    ($runs | map({key:(.id|tostring), value:.states[0].run.name}) | from_entries) as $workflow_of
  # The superseded runs the commit endpoint carries in a world where the one re-run of the head
  # is spent (`spent`, world `e`): the e2e check of attempt 1 that failed and the one of attempt
  # 2 that replaced it, as `filter=all` lists them. They decide nothing — the page lets no role
  # judge the spend on that endpoint — and the spend itself is in the runs of the head.
  | (if $facts.spent then [entry($red_id - 4; "e2e"; "failure"; $t0), entry($red_id - 3; "e2e"; "success"; $t0)]
     else [] end) as $earlier
  # The check paired with the red ($partner): the aggregate `ci` the base requires, red over the
  # failing lane (`i`, `j`, `m`, `n`, `o`, `r`, `s`, `t`), the shape of the base of
  # cybermonklabs/legion itself, which requires the aggregate `ci-ok` over its test lanes; or, in
  # `g` and `q`, the failing lane under the red aggregate. Its id comes after the by-hand run of
  # `k`, so no two checks share one. The aggregate of `p`, green over lint and typecheck, takes
  # that id, and the re-run there, of the run of the title check alone, never re-runs it.
  | (if $paired then [entry($red_id + 3; $partner; "failure"; $t0)] else [] end) as $partners
  | ([entry($red_id - 2; "lint"; "success"; $t0), entry($red_id - 1; "typecheck"; "success"; $t0)]
     + $earlier
     + [entry($red_id; $check; "failure"; $t0)]
     + $partners
     + (if $green_aggregate then [entry($red_id + 3; $required; "success"; $t0)] else [] end)
     | map(linked)) as $red_checks
  # The attempt the re-run makes of each check it re-runs, which `filter=all` lists beside the
  # attempt it replaces: a pass where the world settles green, the same failure where it stays
  # red. It re-runs the red and its partner, the failed jobs of the one run of the red; the check
  # runs of the jobs it carried to attempt 2 keep their attempt-1 conclusions and times. While
  # that attempt runs, each of its check runs stands as its job stands: the lane in progress and
  # the aggregate queued, with no conclusion and no completion (measured: a check run in
  # progress, ip-checkruns, requests.log line 41), and the carried ones as they stand.
  | def attempt($id; $name): entry($id; $name; $after; $t1);
    def attempt_running($id; $name):
      {id:$id, name:$name, status:(if $paired and $name == $required then "queued" else "in_progress" end),
       conclusion:null, started_at:$t1, completed_at:null};
    (if $run_name == "ci"
     then [entry($red_id + 5; "lint"; "success"; $t0), entry($red_id + 6; "typecheck"; "success"; $t0)]
     else [] end) as $carried_checks
  | ($red_checks + [attempt($red_id + 1; $check)]
     + ($partners | map(attempt(.id + 1; .name)))
     + $carried_checks
     | map(linked)) as $settled_checks
  | ($red_checks + [attempt_running($red_id + 1; $check)]
     + ($partners | map(attempt_running(.id + 1; .name)))
     + $carried_checks
     | map(linked)) as $running_checks
  | ($settled_checks | group_by(.name) | map(max_by(.id))) as $latest
  | ($running_checks | group_by(.name) | map(max_by(.id))) as $running_latest
  | (($red_checks | group_by(.name) | map(max_by(.id)))) as $shown
  # The check of the by-hand `workflow_dispatch` run at that head, in a world whose commit
  # endpoint carries one (`by_hand`, world `k`): the counted set never holds it. It is the later
  # run of that check name there, in a check suite of its own, so a role that reads the spend
  # from the commit endpoint reads the one re-run of the head as already spent.
  | (if $facts.by_hand then [entry($red_id + 2; $check; "failure"; $t_hand) | linked]
     else [] end) as $uncounted
  # What the counted set answers, in order: the state the role first reads, the state while the
  # re-run it pressed runs (the rest of the turn that pressed it), and the state the head settles
  # at once that turn has ended and the rig settles the re-run (rerun_settle). A counted set that
  # has not reported the red when the role first reads it (`report` among the facts, worlds `l`
  # and `t`) answers one more first, with no red: neither the red lane nor, in `t`, the aggregate
  # over it, which needs the lane and cannot report before it. red_report drops it between the
  # turns, which is the rig reporting the check. counted(reply) (below) is the replies
  # of a route that answers the counted set, one per state, and in such a world the mark
  # red_report trims that route by, `unreported`.
  | ((if $facts.report then [($shown | map(select(.name != $check and .name != $partner)))] else [] end)
     + [$shown, $running_latest, $latest]) as $counted
  | {world:$world, head:$head, pr:$pr, repo:$repo, branch:$branch, base:$base,
     base_sha:$base_sha, check:$check, job:$job, test_name:$test_name, world_log:$world_log,
     log:$log, log_gone:$log_gone, facts:$facts, lanes:$lanes, required:$required,
     partner:$partner, run_id:$run_id, job_id:$job_id, runs:$runs, all_jobs:$all_jobs,
     job_of:$job_of, workflow_of:$workflow_of, red_checks:$red_checks,
     running_checks:$running_checks, settled_checks:$settled_checks, latest:$latest,
     shown:$shown, uncounted:$uncounted, counted:$counted, workflows:$workflows};

# What run_routes and red_rewrite share over the model red_model printed, which the program that
# reads it takes with `--slurpfile model`, beside the facts of the red the shared defs above read
# ($repo, $pr, $head, $partner, $run_id, $required): the rollups of the counted set, the routes
# that follow the re-run and the press they follow, and a route of a run, of a job, of a check run
# and of a log. Each def reads the fields of the model it names.
def model: $model[0];
# The counted set in GitHub own shape, as the page reads it at a named head: the rollup of the
# commit, its contexts on one page, and one CheckRun node per counted check, each naming its
# job page, its times, its app and the workflow run it belongs to; and the same set as
# `gh pr view --json statusCheckRollup` flattens it, a node per check run with its workflow
# named (measured). A by-hand run is in no node of either. A check run not yet completed is
# its status in capitals with no conclusion, which gh flattens to "" and its completion to the
# zero time of Go, and the rollup of a head with one pending and none failing is PENDING
# (measured on cybermonklabs/legion#3514 while its CI ran, 2026-11-19: ip-prview-rollup and
# ip-graphql, the page query, requests.log lines 39 and 47).
def workflow: model as {$workflow_of, $job_of} | $workflow_of[($job_of[(.id|tostring)].run_id|tostring)];
def rollup($checks): $checks | map({__typename:"CheckRun",
  completedAt:(.completed_at // "0001-01-01T00:00:00Z"),
  conclusion:(verdict // ""), detailsUrl:.details_url, name, startedAt:.started_at,
  status:(.status | ascii_upcase), workflowName:workflow});
def graphql_rollup($checks): model.job_of as $job_of | {
  state:(if (failing($checks)|length) > 0 then "FAILURE"
         elif (pending($checks)|length) > 0 then "PENDING" else "SUCCESS" end),
  contexts:{totalCount:($checks|length),
    # The cursor GitHub gives the last node (gh-graphql.ts), none where the page holds none.
    pageInfo:{hasNextPage:false, endCursor:(if $checks == [] then null
      else $checks | length | tostring | @base64 | sub("=+$"; "") end)},
    nodes:($checks | map({__typename:"CheckRun", name:.name, databaseId:.id,
      status:(.status | ascii_upcase), conclusion:verdict, detailsUrl:.details_url,
      startedAt:.started_at, completedAt:.completed_at,
      checkSuite:{databaseId:check_suite_id($job_of[(.id|tostring)].run_id),
                  app:{slug:"github-actions", databaseId:15368},
                  workflowRun:{event:"pull_request",
                               databaseId:$job_of[(.id|tostring)].run_id,
                               workflow:{name:workflow}}}}))}};
# A check as `gh pr checks --json` prints it, every field gh lists (D2; measured on
# cybermonklabs/legion#3514, 2026-11-19, prchecks-json-all, requests.log line 71): its bucket and
# state as the rows and the rollup read it, its times (the zero time of Go for one not
# completed), no description, the event of its run and the name of its workflow.
def check_json: {bucket:bucket, completedAt:(.completed_at // "0001-01-01T00:00:00Z"),
  description:"", event:"pull_request", link:.details_url, name, startedAt:.started_at,
  state:(verdict // (.status | ascii_upcase)), workflow:workflow};
# The replies of a route that answers the counted set, one per state, the last of them the
# settled one, which the rig puts in place of the running state once it settles the re-run
# (rerun_settle; gh-standin.ts, `settled`), and in a world whose counted set the rig reports
# the mark red_report trims that route by, `unreported`.
def counted(reply): model as {$counted, $facts} | ($counted | map(reply)) as $all
  | {replies:$all[:-1], settled:$all[-1], unsettled:true}
    + (if $facts.report then {unreported:true} else {} end);
# Every read of the head below follows the re-run route (gh-standin.ts, `follows`): it answers
# the first of its replies however many times a role reads it before the re-run, the re-run
# still running from the moment it is pressed, and the settled state once the rig settled it.
# moving(match; replies) is such a route, or one that answers the one reply where the three
# states agree.
# The press of the one re-run of the head: a re-run of the failed jobs of the run of the red,
# by its id with --failed (rerun), or of the own job of the red by its id with --job (that of
# attempt 1 or of the re-run; rerun_job, which counts as a call of rerun), which GitHub re-runs
# with the jobs that need it, the aggregate among them. A call naming both a run and a job, or
# --job with --failed, is one gh refuses in words the rig has not measured, and no route takes
# it.
def rerun: "^gh run rerun " + ($run_id|tostring) + "(?=.*\\s--failed(?:\\s|$))(?!.*\\s--job\\s)(?=\\s|$)";
def rerun_job: model.job_id as $job_id
  | "^gh run rerun (?=--)(?=(?:\\S+ )*--job (?<job>" + ($job_id|tostring) + "|"
    + ($job_id + 4 | tostring) + ")(?:\\s|$))(?!(?:\\S+ )*--failed(?:\\s|$))";
# pressed: whether the role's press of the re-run has reached its input, a run's fixtures.json:
# the route red_fixture marks `press` has answered a call (the gh stand-in's count, `answered`).
# red-at-the-head.sh's rerun_pressed reads it between the turns, and score.ts's unscored reads it
# of a finished run, so the rig and the scorer hold one rule.
def pressed: .routes | any(.press == true and (.answered // 0) > 0);
def moving($match; $replies):
  {match:$match} + (if ($replies | unique | length) == 1 then $replies[0]
                    else {follows:rerun, replies:$replies[:-1], settled:$replies[-1],
                          unsettled:true} end);
def ids($xs): "(?:" + ($xs | map(tostring) | join("|")) + ")";
# A `gh run view` that names its run as the positional argument, and no job.
def view($id): "^gh run view " + ($id|tostring) + "(?=\\s|$)(?!.*\\s--job\\s)";
# A `gh run view` or `gh run rerun` that names its job with --job and no run.
def job_flag($id): "(?=--)(?=(?:\\S+ )*--job " + $id + "(?:\\s|$))";
# A `gh run view` that names its job with --job, with or without a run: gh reads the job, then
# the run the job belongs to, whatever run the call names (measured: `gh run view 9001002 --job
# 8001003` asks for job 8001003 and then for run 7001003).
def view_job($id): "^gh run view(?: \\d+)?(?=.*\\s--job " + $id + "(?:\\s|$))";
def logs: "(?=.*\\s--log(?:-failed)?(?:\\s|$))(?!.*\\s--json\\s)";
def plain: "(?!.*\\s--(?:log|log-failed|json)(?:\\s|$))";
# A `gh run watch` that names its run, and what it answers in a state of the head: a run
# that has completed at once, exiting 1 under --exit-status ($exit_status) where it did not
# succeed, and a run that has not as the frames gh prints while it waits on it (`watch`,
# gh-standin.ts), over the jobs of its latest attempt (programs.jq's run_watch_program).
def watching($id): "^gh run watch " + ($id|tostring) + "(?=\\s|$)";
def watch_reply($exit_status): if .run.status == "completed"
  then {stdout:{run:.run}} + (if $exit_status and .run.conclusion != "success" then {exit:1} else {} end)
  else {stdout:{run:.run, jobs:latest}, watch:true} end;
# The log a run or a job prints, as gh does, a line per log line, `<job>\t<step>\t<line>`: the
# failing line of the world in the failing step of each failed job, and no line of a job that
# passed, which is all the log the world holds. Each log line carries the time GitHub stamps
# on it, seven digits after the second, and the first log line of each step the BOM GitHub
# puts there, which gh 2.98.0 prints as it stands (measured on a live job of
# cybermonklabs/legion, 2026-11-18, runview-job-log of the measurements); the world logs no
# ESC byte, so gh never refuses one for its terminal escape sequences. The log of attempt 1 of
# the run of the red is past retention in world `h`, so its read answers what gh 2.98.0 prints
# for the 410 GitHub answers there (measured on a run past retention): the URL of the log of
# the run gh asked for, or of the attempt where the call names one. The failing line of a job
# is the one its workflow model gives (red_workflows). A log read while the job, or the run it
# belongs to, has not completed is refused as gh refuses it (view.go:314-319 at v2.98.0,
# requests.log line 49; measured on cybermonklabs/legion run 44586483868 while it ran,
# ip-runview-logfailed and ip-runview-joblog, requests.log lines 23 and 25).
def log_line: model as {$workflows, $log}
  | .name as $n | (first($workflows[].jobs[] | select(.name == $n)).log // $log);
def log_expired($run; $attempt):
  {stderr:("failed to get run log: HTTP 410: Server Error (https://api.github.com/repos/"
           + $repo + "/actions/runs/" + ($run|tostring)
           + (if $attempt == null then "" else "/attempts/" + ($attempt|tostring) end)
           + "/logs)\n"),
   exit:1};
def still($what; $id):
  {stderr:($what + " " + ($id|tostring)
           + " is still in progress; logs will be available when it is complete\n"), exit:1};
def log_reply($jobs):
  if model.log_gone and any($jobs[]; .run_id == $run_id and .run_attempt == 1
                                     and .conclusion == "failure")
  then log_expired($run_id; null)
  else {stdout:([$jobs[] | select(.conclusion == "failure")
                 | .name + "\t" + .steps[1].name + "\t" + "\ufeff"
                   + (.completed_at | sub("Z$"; ".0000000Z")) + " " + log_line]
                | join("\n"))} end;
# A job as a state of the head holds it, with the run of that state it belongs to; a job that
# state does not hold yet (an attempt 2 before the re-run) answers $miss, the error gh prints
# for a job it cannot find or the 404 of GitHub.
def job_in($state; $id):
  [model.runs[].states[$state] | .run as $run | .attempts[][] | select(.id == $id)
   | {job:., run:$run}] | first;
def job_route($match; $id; reply; $miss):
  moving($match; [0, 1, 2] | map(job_in(.; $id) as $j | if $j != null then ($j | reply) else $miss end));
def run_route($match; $w; reply): moving($match; $w.states | map(reply));
def check_in($state; $id):
  model as {$red_checks, $running_checks, $settled_checks, $uncounted}
  | [[$red_checks, $running_checks, $settled_checks][$state][], $uncounted[]
     | select(.id == $id)] | first;
def check_route($match; $id; reply; $miss):
  moving($match; [0, 1, 2] | map(check_in(.; $id) as $c
                                | if $c == null then $miss else ($c | reply) end));
# Each program over the model below (run_routes, red_rewrite) binds the model's fields, and what
# the defs above give, as the variables its body reads: a def binds no variable for its caller.

# run_routes: as one JSON list, the routes of the head's runs, jobs and check runs in the world of
# the model: the press of the re-run and gh's help and usage around it ($rerun_help,
# $rerun_usage, $view_usage), each read of a run, a job, a check run or a log of the head as the
# state of the head has it, and GitHub's answer, or gh's refusal, for each of those reads of an id
# the head has not. red_fixture puts them ahead of worker_fixture's routes.
def run_routes:
  model as {$head, $pr, $repo, $branch, $log, $log_gone, $facts, $lanes, $required, $partner,
            $run_id, $job_id, $runs, $all_jobs, $job_of, $workflow_of, $red_checks,
            $running_checks, $settled_checks, $latest, $shown, $uncounted, $counted, $workflows}
  | repo_api as $api | paired as $paired | rerun as $rerun | rerun_job as $rerun_job
  | program("run_view_program") as $run_view_program | program("run_list_program") as $run_list
  | program("runs_query_program") as $runs_query | program("jobs_program") as $jobs_query
  | program("attempt_jobs_program") as $attempt_jobs | program("run_program") as $run_program
  | program("annotations_program") as $annotations | program("run_watch_program") as $run_watch_program
  | ($runs[0]) as $red_run
| ([$runs[].id]) as $run_ids
| ([$settled_checks[], $uncounted[] | .id] | unique) as $check_ids
| [{match:"^gh run rerun(?=.*\\s--help(?:\\s|$))", stdout:$rerun_help},
   # A re-run presses the re-run of the head only where it re-runs the failed jobs of the run
   # the red of the world belongs to, or the own job of the red: the re-run route takes those calls
   # alone, so its count, which every read of the head follows, moves on them and on nothing
   # else. Before these routes any `gh run rerun` pressed the head, so a re-run of a check run
   # id (trial-d-head-5, trial-e-head-2, trial-g-head-2) and `gh run rerun --help`
   # (trial-i-nos1-1, trial-j-nos1-1) each settled the head and published its settlement. A
   # re-run of every other run or job of the head (the spent e2e run of `e`, the by-hand run of
   # `k`, the `ci` run of `f`, the lint job) would make a state of it this world does not hold,
   # and is not modeled; a run or a job the head has none of is answered the 404 gh prints.
   ({match:("^gh run rerun " + job_flag("(?<job>\\d+)") + "(?!(?:\\S+ )*--job " + ids($all_jobs | map(.id))
            + "(?:\\s|$))")} + no_job($repo; "$<job>")),
   ({match:("^gh run rerun (?!" + ids($run_ids) + "(?:\\s|$))(?<run>\\d+)(?=\\s|$)(?!.*\\s--job\\s)")}
    + no_run($repo; "$<run>")),
   {match:"^gh run rerun(?! \\d)(?!.*\\s--job\\s)", stderr:($rerun_usage + "\n"), exit:1},
   # gh 2.98.0 prints nothing for a re-run it requested where no terminal is there (measured).
   # The press makes the attempt of the re-run, which runs for the rest of the turn that pressed it
   # and settles only once that turn has ended (rerun_settle): until then every read of the head
   # answers it running, and a second press is refused as GitHub refuses one while the run
   # runs, its 403 "This workflow is already running" (INFRA-1478, retro-addendum-md line 38:
   # `gh run rerun --failed` on run 44186145691, in progress) as gh 2.98.0 prints it, `run <id>
   # cannot be rerun; <message>` for a run (rerun.go:206-211) and `job <id> cannot be rerun` for
   # a job (rerun.go:228-233), exit 1 (ghcmd cmd.go); rerun.go and cmd.go read from the source
   # of gh v2.98.0, requests.log lines 1 and 61. Once settled, a press answers as the first
   # did. `press` marks the route rerun_pressed reads.
   {match:$rerun, press:true,
    replies:[{stdout:""},
             {stderr:("run " + ($run_id|tostring)
                      + " cannot be rerun; This workflow is already running\n"), exit:1}],
    settled:{stdout:""}, unsettled:true},
   {match:$rerun_job, follows:$rerun, counts_as:$rerun,
    replies:[{stdout:""}, {stderr:"job $<job> cannot be rerun\n", exit:1}],
    settled:{stdout:""}, unsettled:true},
   # The same re-run through the REST API, a POST of the failed jobs of the run of the red or
   # of the own job of the red: it presses the re-run as `gh run rerun` does, counting as a call of
   # that route (gh-standin.ts, `counts_as`), and answers what GitHub answers, an empty object.
   # The status and the body GitHub answers such a POST while the run runs are not measured, so
   # a second press this way is refused as unmodeled until the rig settles the re-run.
   {match:api_post("(?:actions/runs/" + ($run_id|tostring) + "/rerun-failed-jobs"
                   + "|actions/jobs/" + ids([$job_id, $job_id + 4]) + "/rerun)"),
    follows:$rerun, counts_as:$rerun, replies:[{stdout:{}}, {unmodeled:true}],
    settled:{stdout:{}}, unsettled:true},
   # Every read of a run or of a job answers the runs and the jobs of the head alone, each as
   # the state of the head has it: its log, its fields under --json, or the text gh 2.98.0
   # prints for it (measured, $run_view_program); a run or a job of any other id is refused as
   # gh refuses it, and its REST read answers the 404 of GitHub. A job read under --json is
   # not modeled. A read that names an attempt (--attempt) of a run of the head reads the log
   # of that attempt, from the same model as the log read of the run: the failing line of each
   # failed job of that attempt, as the state of the head holds it (INFRA-1303, D10: the
   # red-again re-run of world j, whose attempt-2 log j-head-3 read at 9c3bd9df). An attempt the state
   # holds before the last has ended; the last is refused as gh refuses a log read while its run
   # runs. In world `h` attempt 1 of the run of the red is past retention, whose 410 names that
   # attempt (measured). An attempt the state does not hold yet, and every other read of an
   # attempt (`--json` among them), is not modeled: gh asks GitHub for an attempt it does not
   # have, and that answer is not measured.
   (if $log_gone
    then {match:(view($run_id) + "(?=.*\\s--attempt 1(?:\\s|$))" + logs)} + log_expired($run_id; 1)
    else empty end),
   ($runs[] | . as $w
    | range(1; ($w.states[-1].attempts | length) + 1) as $n
    | run_route(view($w.id) + "(?=.*\\s--attempt " + ($n|tostring) + "(?:\\s|$))" + logs; $w;
                if .attempts[$n - 1] == null then {unmodeled:true}
                elif $n == (.attempts | length) and .run.status != "completed"
                then still("run"; .run.id)
                else log_reply(.attempts[$n - 1]) end)),
   {match:"^gh run view(?=.*\\s--attempt\\s)", unmodeled:true},
   ($all_jobs[] | .id as $id
    | job_route(view_job($id|tostring) + logs; $id;
                if .job.status != "completed" then still("job"; .job.id)
                elif .run.status != "completed" then still("run"; .run.id)
                else log_reply([.job]) end; no_job($repo; $id|tostring)),
      (job_route(view_job($id|tostring) + plain; $id;
                 {stdout:.}; no_job($repo; $id|tostring)) + {program:$run_view_program})),
   ({match:view_job("(?<job>\\d+)")} + no_job($repo; "$<job>")),
   ({match:("^gh run view (?!" + ids($run_ids) + "(?:\\s|$))(?<run>\\d+)(?=\\s|$)(?!.*\\s--job\\s)")}
    + no_run($repo; "$<run>")),
   {match:"^gh run view(?! \\d)(?!.*\\s--job\\s)", stderr:($view_usage + "\n"), exit:1},
   ($runs[] | . as $w
    | run_route(view($w.id) + logs; $w;
                if .run.status != "completed" then still("run"; .run.id)
                else latest as $jobs | log_reply($jobs) end),
      run_route(view($w.id) + "(?=.*\\s--json\\s)(?!.*\\s--log(?:-failed)?(?:\\s|$))"; $w;
                latest as $jobs | {stdout:(.run | gh_run($jobs))}),
      (run_route(view($w.id) + plain; $w; {stdout:{run:.run, jobs:latest}})
       + {program:$run_view_program})),
   # `gh run watch`, as gh 2.98.0 answers it, read from the source of gh at the gh the worker
   # image pins (packages/daemon/docker/worker.Dockerfile, GH_TOOL=gh@2.98.0; github.com/cli/cli,
   # pkg/cmd/run/watch/watch.go at v2.98.0): an --interval that is no integer is refused first,
   # as cobra refuses a flag before the command runs (measured, $watch_usage); a run of the head
   # is answered in each state of the head (run_watch_program): one that has completed at once,
   # exiting 1 under --exit-status where it did not succeed, and the attempt the re-run made,
   # in progress for the rest of the turn that pressed it, as the frames gh prints while it
   # waits (`watch`); a run of any other id is the 404 of GitHub as gh prints it, from the
   # GetRun gh run view makes too (watch.go:132-136); and a call that names no run is the usage
   # error of gh
   # where no terminal is there to pick one (watch.go:73-77, measured). Before these routes no
   # route took `gh run watch`, and the stand-in refused it as unmodeled, a rig error.
   {match:"^gh run watch(?=.*\\s--interval (?<value>(?![+-]?[0-9]+(?:\\s|$))(?!0[xXoObB])\\S+)(?:\\s|$))",
    stderr:("invalid argument \"$<value>\" for \"-i, --interval\" flag: strconv.ParseInt: parsing \"$<value>\": invalid syntax\n\n"
            + $watch_usage + "\n"),
    exit:1},
   ($runs[] | . as $w
    | (run_route(watching($w.id) + "(?=.*\\s--exit-status(?:\\s|$))"; $w; watch_reply(true))
       + {program:$run_watch_program}),
      (run_route(watching($w.id); $w; watch_reply(false)) + {program:$run_watch_program})),
   ({match:("^gh run watch (?!" + ids($run_ids) + "(?:\\s|$))(?<run>\\d+)(?=\\s|$)")}
    + no_run($repo; "$<run>")),
   {match:"^gh run watch(?=\\s--|$)",
    stderr:("run ID required when not running interactively\n\n" + $watch_usage + "\n"), exit:1},
   # A workflow name no workflow of the world holds, in any case, is refused as gh refuses it
   # (pkg/cmd/workflow/shared/shared.go:224-226 at v2.98.0, requests.log line 57); the program
   # reads a file or an id.
   ({match:("^gh run list(?=.*\\s--workflow\\s(?<workflow>(?!(?:"
            + ($workflows | map(.name | any_case) | join("|")) + ")(?:\\s|$))(?![0-9]+(?:\\s|$))"
            + "(?!\\S*\\.[Yy][Aa]?[Mm][Ll](?:\\s|$))\\S+)(?:\\s|$))"),
     stderr:"could not find any workflows named $<workflow>\n", exit:1}),
   (moving("^gh run list(?: --|$)"; [0, 1, 2] | map(. as $s
      | {stdout:[$runs[].states[$s] | .run
                 | (gh_run([]) | del(.jobs)) + {workflowFile:(.path | split("/") | last)}]}))
    + {program:$run_list}),
   ($runs[] | . as $w | ($w.id|tostring) as $id
    | (run_route(api("actions/runs/" + $id); $w; {stdout:.run}) + {program:$run_program}),
      (run_route(api("actions/runs/" + $id + "/jobs"); $w;
                 latest as $jobs | {stdout:{jobs:$jobs, all:[.attempts[][]]}}) + {program:$jobs_query}),
      (range(1; ($w.states[-1].attempts|length) + 1) as $n
       | (run_route(api("actions/runs/" + $id + "/attempts/" + ($n|tostring)); $w;
                    if .attempts[$n - 1] == null then github("attempt-404")
                    elif $n == (.attempts|length) then {stdout:.run}
                    else {stdout:(.run + {run_attempt:$n, conclusion:"failure",
                                          updated_at:.run.created_at,
                                          previous_attempt_url:previous_attempt($w.id; $n)})} end)
          + {program:$run_program}),
         (run_route(api("actions/runs/" + $id + "/attempts/" + ($n|tostring) + "/jobs"); $w;
                    if .attempts[$n - 1] == null then github("attempt-jobs-404")
                    else {stdout:{jobs:.attempts[$n - 1]}} end) + {program:$attempt_jobs})),
      ([range(1; ($w.states[-1].attempts|length) + 1)] as $attempts
       | ({match:api("actions/runs/" + $id + "/attempts/(?!" + ids($attempts) + "(?:[/?\\s]|$))\\d+")}
          + github("attempt-404")),
         ({match:api("actions/runs/" + $id + "/attempts/(?!" + ids($attempts) + "(?:[/?\\s]|$))\\d+/jobs")}
          + github("attempt-jobs-404")))),
   # A run of no id the head has: the 404 GitHub answers for each read of it the rig answers
   # (measured), any other read of it not modeled.
   ("actions/runs/(?!" + ids($run_ids) + "(?:[/?\\s]|$))\\d+") as $other_run
   | ({match:api($other_run)} + github("run-404")),
     ({match:api($other_run + "/jobs")} + github("run-jobs-404")),
     ({match:api($other_run + "/attempts/\\d+")} + github("run-attempt-404")),
     ({match:api($other_run + "/attempts/\\d+/jobs")} + github("run-attempt-jobs-404")),
     ({match:api($other_run + "/logs")} + github("run-logs-404")),
     ({match:api_post($other_run + "/rerun-failed-jobs")} + github("rerun-404")),
     ({match:api_post($other_run + "/rerun")} + github("run-rerun-404")),
   # A job and its log through the REST API: the log of a failed job holds the failing line of
   # the world in its failing step and the close the runner writes, that of a job that passed
   # its step and no line of its own, which is all the log the world holds. The log of attempt
   # 1 of the run of the red is past retention in world `h`, which GitHub answers with a 410
   # (measured by the lead under gh 2.98.0 on a job past retention, 2026-11-18). A job the
   # state of the head does not hold yet, or of no id the head has, answers the 404 of GitHub.
   # The log is as GitHub sends a job log (measured on a live job of cybermonklabs/legion,
   # 2026-11-18, joblog-live of the measurements): one BOM at its start, each line stamped with
   # the time, seven digits after the second, and a newline after each line, the last one too.
   # The world logs no ESC byte, so gh never refuses the log for its terminal escape sequences.
   # The log of a job not yet completed is GitHub 404 with a body naming a request id and a time
   # the world does not hold (measured, ip-joblog, requests.log line 15), so that read is not
   # modeled.
   ($all_jobs[] | .id as $id
    | job_route(api("actions/jobs/" + ($id|tostring)); $id; {stdout:.job}; github("job-404")),
      job_route(api("actions/jobs/" + ($id|tostring) + "/logs"); $id;
        .job | (.completed_at // "" | sub("Z$"; ".0000000Z")) as $ts
        | if .status != "completed" then {unmodeled:true}
          elif $log_gone and .run_id == $run_id and .run_attempt == 1
             and .conclusion == "failure"
          then github("joblog-410")
          else {stdout:("\ufeff\($ts) ##[group]\(.steps[1].name)\n"
                        + (if .conclusion == "failure"
                           then "\($ts) \(log_line)\n\($ts) ##[error]Process completed with exit code 1.\n"
                           else "" end)
                        + "\($ts) ##[endgroup]\n")} end; github("joblog-404"))),
   ("actions/jobs/(?!" + ids($all_jobs | map(.id)) + "(?:[/?\\s]|$))\\d+") as $other_job
   | ({match:api($other_job)} + github("job-404")),
     ({match:api($other_job + "/logs")} + github("joblog-404")),
     ({match:api_post($other_job + "/rerun")} + github("jobrerun-404")),
   # A check run by its id, and its annotations: none for a check that passed, and for one
   # that failed the annotation the runner writes for any failed step, which names no test;
   # the annotations read takes its page (annotations_program).
   ($check_ids[] | . as $id
    | check_route(api("check-runs/" + ($id|tostring)); $id; {stdout:.}; github("checkrun-404")),
      (check_route(api("check-runs/" + ($id|tostring) + "/annotations"); $id;
                   {stdout:(if .conclusion == "failure"
                            then [{path:".github", start_line:1, end_line:1,
                                   start_column:null, end_column:null,
                                   annotation_level:"failure", title:"",
                                   message:"Process completed with exit code 1.",
                                   raw_details:""}]
                            else [] end)}; github("annotations-404"))
       + {program:$annotations})),
   ("check-runs/(?!" + ids($check_ids) + "(?:[/?\\s]|$))\\d+") as $other_check
   | ({match:api($other_check)} + github("checkrun-404")),
     ({match:api($other_check + "/annotations")} + github("annotations-404")),
   (moving(api("actions/runs"); [0, 1, 2] | map(. as $s
      | [$runs[].states[$s].run] as $all
      | {stdout:{total_count:($all|length), workflow_runs:$all}}))
    + {program:$runs_query})];

# red_rewrite: its input, worker_fixture's fixtures.json, turned red over the model: the routes
# run_routes printed ($routes, read with --slurpfile) ahead of worker_fixture's, the reads of the
# head worker_fixture marked rewritten, and the envelopes red_report and rerun_settle publish
# (`reported`, `settlement`), which red_fixture moves to files of their own.
def red_rewrite:
  model as {$head, $pr, $repo, $branch, $log, $log_gone, $facts, $lanes, $required, $partner,
            $run_id, $job_id, $runs, $all_jobs, $job_of, $workflow_of, $red_checks,
            $running_checks, $settled_checks, $latest, $shown, $uncounted, $counted, $workflows}
  | repo_api as $api | paired as $paired | rerun as $rerun | rerun_job as $rerun_job
  | program("check_suites_program") as $check_suites | program("checks_watch_program") as $checks_watch
  | $routes[0] as $new
  # The reads of the head worker_fixture marks `head_read` answer the red where they stand, and
  # what the head settles at once the re-run is pressed: the pull request (its rollup and its
  # merge state, through gh and through REST), its checks, the GraphQL world (the rollup of the
  # head and the merge state of the pull request), and its check runs. The commit endpoint answers
  # every run at the head, the by-hand one among them, and the attempt each re-run made once it
  # is pressed. The check suites of the head are one of GitHub Actions per run of the head, each
  # with what the run holds: its suite id and node id, branch, commit, status and conclusion,
  # pull requests and times. A field of a suite a run does not hold (before, after, its count of
  # check runs, whether it can be re-requested) is in no answer. Their read takes its query
  # (check_suites_program), over the names of the check runs each suite holds in that state.
  | def check_suite: {id:.check_suite_id, node_id:.check_suite_node_id, head_branch,
      head_sha, status, conclusion, url:($api + "/check-suites/" + (.check_suite_id|tostring)),
      pull_requests, app:actions_app, created_at, updated_at,
      check_runs_url:($api + "/check-suites/" + (.check_suite_id|tostring) + "/check-runs")};
    .routes |= map(
      if .head_read == "pr"
      then (.stdout as $view
            | . + counted({stdout:($view + {statusCheckRollup:rollup(.), mergeStateStatus:merge_state(.)})})
            | .follows = $rerun | del(.stdout))
      elif .head_read == "pull"
      then (.stdout as $pull
            | . + counted({stdout:($pull + {mergeable_state:(merge_state(.) | ascii_downcase)})})
            | .follows = $rerun | del(.stdout))
      elif .head_read == "pulls"
      then (.stdout as $pulls
            | . + counted(merge_state(.) as $state
                          | {stdout:($pulls | map(. + {mergeable_state:($state | ascii_downcase)}))})
            | .follows = $rerun | del(.stdout))
      # `gh pr checks --required` answers the checks the base requires and no other, as GitHub
      # does, from a route of its own ahead of the one that answers every check. The base
      # requires the aggregate check `ci`, and in `p` the title check too (requires), which the
      # heads of worlds `g`, `i`, `j` and `m` to `t` report: every other head reports no check
      # the base requires, which gh answers on
      # stderr, exiting 1 (\u0027 is the quote gh puts around the branch), with --json too, since
      # gh reads the checks before it prints any
      # (checks.go:183-190 and 306-308 at v2.98.0, requests.log line 51). The rows exit as gh
      # exits on them (checks_exit), and --json exits 0 whatever the checks say
      # (checks.go:189-190).
      # `gh pr checks --watch` (pkg/cmd/pr/checks/checks.go at v2.98.0, read from the source of
      # gh at the gh the worker image pins): to no terminal, while a check it reads is pending, a
      # frame each --interval (10 by default), `Refreshing checks status every <n> seconds. Press
      # Ctrl+C to quit.`, a blank line and the rows (checks.go:205-216, `watch`, gh-standin.ts);
      # once none is pending the rows, then the rows again as its final table, exiting as the
      # rows do (checks.go:218-251). --fail-fast ends the watch on a failed check while one is
      # pending, which no state of any world holds (a pending state is the re-run attempt, with
      # nothing failed), so it answers as --watch does. --required and --watch together read the
      # required checks so; with no required check gh refuses before it watches (checks.go:306-308).
      # gh refuses a watch with --json, --fail-fast or --interval without --watch, and an
      # --interval that is no integer, before any request (checks.go:80-101 and cobra; measured,
      # checks_usage, red-at-the-head.sh); those routes stand ahead of the --json route, which
      # every --json call takes. A watch reads the counted set as `gh pr checks` does, so its
      # routes carry the mark of a read of the head (`checks_watch`), and in world `l` the mark
      # red_report trims (counted).
      elif .head_read == "checks" or .head_read == "checks_json"
      then ((.head_read == "checks_json") as $json
            | def answer($checks):
                if $json then {stdout:($checks | map(check_json))}
                else {stdout:rows($checks), exit:checks_exit($checks)} end;
              def none_required: {stdout:{none:true}, stderr:("no required checks reported on the \u0027"
                                  + $branch + "\u0027 branch\n"), exit:1};
              def watch_answer($checks): {stdout:{rows:rows($checks), pending:((pending($checks) | length) > 0)}}
                + (if (pending($checks) | length) > 0 then {watch:true} else {exit:checks_exit($checks)} end);
              def refused($match; $line): {match:$match, stderr:($line + "\n\n" + $checks_usage + "\n"), exit:1};
              ("^gh pr checks " + ($pr|tostring) + "(?=\\s|$)(?!.*\\s--(?:json|jq)\\s)(?=.*\\s--watch(?:\\s|$))")
              as $watching
            | if $json
              then refused("^gh pr checks(?=.*\\s--interval (?<value>(?![+-]?[0-9]+(?:\\s|$))(?!0[xXoObB])\\S+)(?:\\s|$))";
                           "invalid argument \"$<value>\" for \"-i, --interval\" flag: strconv.ParseInt: parsing \"$<value>\": invalid syntax"),
                   refused("^gh pr checks(?=.*\\s--json\\s)(?=.*\\s--watch(?:\\s|$))";
                           "cannot use `--watch` with `--json` flag"),
                   refused("^gh pr checks(?=.*\\s--fail-fast(?:\\s|$))(?!.*\\s--watch(?:\\s|$))";
                           "cannot use `--fail-fast` flag without `--watch` flag"),
                   refused("^gh pr checks(?=.*\\s--interval\\s)(?!.*\\s--watch(?:\\s|$))";
                           "cannot use `--interval` flag without `--watch` flag")
              else ({match:($watching + "(?=.*\\s--required(?:\\s|$))"), follows:$rerun,
                     head_read:"checks_watch", program:$checks_watch}
                    + counted(map(select(requires)) | if . == [] then none_required else watch_answer(.) end)),
                   ({match:$watching, follows:$rerun, head_read:"checks_watch", program:$checks_watch}
                    + counted(watch_answer(.)))
              end,
            ({match:(.match + "(?=.*\\s--required(?:\\s|$))"), follows:$rerun}
             + counted(map(select(requires))
               | if . == []
                 then {stderr:("no required checks reported on the \u0027" + $branch
                               + "\u0027 branch\n"), exit:1}
                 else answer(.) end)),
            (. + counted(answer(.)) | .follows = $rerun | del(.stdout)))
      elif .head_read == "graphql"
      then (.stdout as $world
            | . + counted(. as $checks
                          | {stdout:($world
                                     | .["$refs"].head.statusCheckRollup = graphql_rollup($checks)
                                     | .["$refs"].pr.mergeStateStatus = merge_state($checks))})
            | .follows = $rerun | del(.stdout))
      elif .head_read == "check-runs"
      then (. + {follows:$rerun}
            + ([$red_checks, $running_checks, $settled_checks]
               | map(. + $uncounted | {stdout:{total_count:length, check_runs:.}})
               | {replies:.[:-1], settled:.[-1], unsettled:true})
            | del(.stdout))
      elif .head_read == "check-suites"
      then (moving(.match; [0, 1, 2] | map(. as $state
              | [$runs[].states[$state].run | check_suite] as $suites
              | ([$red_checks, $running_checks, $settled_checks][$state] + $uncounted
                 | group_by(.check_suite.id)
                 | map({key:(.[0].check_suite.id|tostring), value:map(.name)}) | from_entries)
                as $names
              | {stdout:{check_suites:$suites, names:$names}}))
            + {head_read:"check-suites", program:$check_suites})
      else . end)
  # The answers for a repository other than the world (`repo_refusal`, worker_fixture) stand
  # ahead of every route, as GitHub refuses such a read whatever it asks.
  | .routes = ([.routes[] | select(.repo_refusal)] + $new + [.routes[] | select(.repo_refusal | not)])
  # The envelope red_report publishes in a world whose counted set the rig reports (world `l`),
  # the settlement the head would raise when the counted set reports the red; and the settlement
  # the head raises once the re-run settled, which rerun_settle publishes after the turn that
  # pressed the re-run has ended.
  | . + (if $facts.report then {reported:published($shown)} else {} end)
  | . + {settlement:published($latest)};
