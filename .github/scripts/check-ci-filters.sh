#!/usr/bin/env bash
# ci.yaml's `changes` job selects each job through a dorny/paths-filter filter, and a change to a
# file a job reads but its filter misses skips that job, on the pull request and again on the
# merge group that lands it, after which nothing runs it. So every filter must match every file
# its job reads. For a Go job that set moves with every import, embed and test file, so this
# check recomputes it on every change to Go code, a module file or ci.yaml, and fails naming
# each file a filter misses.
#
# What a job reads is, per gate (.github/scripts/ci_job_reads.py, GATES):
#   * the closure of each go command the job runs, live: `go list -deps [-test] [-tags ...] [-race]`
#     over the command's packages, in its directory, with its GOOS/GOARCH/CGO_ENABLED and -overlay.
#     Each first-party package directory counts as `<dir>/*`, its `_test.go` files aside: those
#     belong to the runs that compile them, so a test run adds the package's test build and its
#     `testdata/**`, and a test file a build constraint keeps out of one listing - a
#     `//go:build race` test, a tagged live one - is read by the run whose -race or -tags puts
#     it in, and by no other. A directory holding a test file is therefore named file by file;
#     every run adds each embedded file and, for a `<root>/...` pattern, every directory with .go
#     files under root (the go command reads their build constraints);
#   * what a test compiled into such a run does outside its package (RULES, keyed by source file
#     and applied only when go list says the file is compiled under the job's tags): the closure
#     of a go command it starts, and each file it reads through `..`;
#   * the module files of each go command's workspace: go.work, go.work.sum and every bound
#     module's go.mod and go.sum (`go env GOWORK`, `go work edit -json`);
#   * for a claimed go command this check does not reduce (below), and for every Run of an exact
#     entry whose step no longer holds its pinned script, every Go file of the repository and
#     every file under a Go module's directory;
#   * the static reads of the job's other steps: actions, the Bun install, Biome, scripts, fixtures.
# Those globs are expanded over the repository's files (tracked, and untracked ones git does not
# ignore, so an uncommitted new file counts and a build output does not) and each file is held to
# the gate's filter as dorny/paths-filter v4.0.3 reads it (workflow_paths.PathsFilter: `some`, or
# `some-with-excludes`, where a `!` pattern excludes), anchors flattened into the list that names
# them.
#
# The table cannot drift from ci.yaml silently. Fails on:
#   1. a file a gate reads that its filter does not match (up to 20 named per gate, with why).
#      For a Bun gate those reads are hand-kept, and the TypeScript half of them is no longer:
#      .github/scripts/check-ts-filters.sh lists each such job's own tsc program with
#      `bunx tsc --listFilesOnly` and holds every file of it to that gate's filter, in ci.yaml's
#      lint job, since this one's has no Bun. A file a test reads by relative path is part of no
#      tsc program, so it stays hand-kept here,
#   2. a gate whose filter the changes job lacks, or whose job's (or step's) if: does not test it.
#      A job or a step of a workflow ci.yaml calls cannot test `needs.changes.outputs.<name>`:
#      inside a reusable workflow the only context carrying a caller's value is `inputs`. Such a
#      gate is written `inputs.<name>` and read through the calling job's `with:`, the reference
#      resolved to the expression the caller passes for that input before the filter is read off
#      it. A `with:` value a condition of the called file reads must therefore be
#      `${{ needs.changes.outputs.<filter> }}` or `${{ needs.changes.result }}` (the ci-ok
#      payload `${{ toJSON(needs.changes) }}`, which names no output, is the one other value
#      read here) wherever it names the changes job at all, and one naming it in any other
#      shape - an output inside a larger expression, a value built from one - is
#      refused naming the input, the calling job and both files, since the resolution may never
#      be guessed. A value naming the changes job nowhere carries no filter and is read as no
#      gate: dispatch-browser.yaml's `lane`, which ci.yaml builds from the event and the pull
#      request's labels, chooses which projects the browser suite runs, never whether it runs,
#      and where such a value stands in for a gate the table declares, this rule's own first
#      half refuses that gate. A `with:` value no condition reads gates nothing and is left alone,
#   3. a job or step gated on a filter the table neither lists in GATES nor EXCLUDES, in ci.yaml
#      or in a workflow it calls, where the filter is the one the resolved condition names,
#   4. a run: line of a job of ci.yaml, or of a job of a workflow ci.yaml calls - which is where
#      such a job's steps are - that starts a go command (test, vet, build, run, list, install,
#      generate) no Run of a gate on that job claims and no exact entry's step holds, or a
#      claimed line in the simple form (below) whose packages, -tags, -race or mode (test, vet,
#      or build) are not exactly its Run's. A job that calls a reusable workflow keeps its id,
#      its needs: changes and the whole if: that tests its filter in ci.yaml and holds its steps
#      in the called file, which runs them inside the caller's own run, so the steps of every one
#      of those files are read here and each refusal names the file the step is in,
#   5. a Run whose pinned text its step or script no longer holds, an exact entry whose step the
#      job no longer has, a RULES source file that is gone, or a why citing a step the job no
#      longer has. A gate's step is looked for in the file that gate names - ci.yaml, or the
#      workflow it calls that holds that job's steps - and a why's in the file its citation
#      names, so a step that moved between the two fails here until the table says where it went,
#   6. a go command that fails or a first-party package go list cannot load, since its closure
#      is then unknown, and a `!` pattern under `some` or a brace in a gate's filter,
#   7. a CI_OK_SELECTED_BY in ci.yaml's env that is not exactly {job: the output its if: tests}
#      for every job whose if: tests an output of changes, or a CI_OK_PULL_REQUEST_ONLY that is
#      not exactly the jobs whose if: restricts to github.event_name == 'pull_request': ci-ok.sh
#      fails a job that map says changes selected unless it succeeded, so a wrong entry fails a
#      job that was rightly skipped or passes one a failed-only re-run left skipped; a job but
#      changes and those CI_OK_EXEMPT names whose last step is not the ci-ok-report step, or that
#      calls a workflow with no such step reporting for it, since a merge-group run that job's
#      own result decides then gets no failing ci-ok until the ci-ok job has waited for every
#      other job of the run; a job CI_OK_EXEMPT names that does report to ci-ok, or that
#      CI_OK_SELECTED_BY also maps, or
#      that is no job of the file at all; a job named for a check main's ruleset requires (ci-ok
#      and pr-title) that is not the job of that name, and either of those two jobs renamed by a
#      `name:`, since a job posts its own check under its display name and a skipped one posts a
#      pass; a pr-title job that is missing, since nothing else reports that check on a pull
#      request or a merge group; a ci-ok job — the verdict, whose own check run is the other
#      check that ruleset requires — that is missing, that CI_OK_EXEMPT does not name, or whose
#      needs: is not every job of the file but itself and the exempt ones, since a job outside
#      that list ends after a verdict that says nothing about it; and a job of the file that is
#      in none of ci-ok's three classes, or in two of them: CI_OK_SELECTED_BY maps it to the
#      changes output its if: tests, CI_OK_ALWAYS_RUN names it (it runs on every change, no
#      changes output gates it, and ci-ok passes only when it succeeded), or CI_OK_EXEMPT names
#      it (outside ci-ok's accounting), and a job in none is one ci-ok.sh fails on every run
#      whatever it did while a job in two is read by the wrong class; a CI_OK_ALWAYS_RUN name
#      that is no job of the file, or whose if: does test an output of changes, since ci-ok.sh
#      would then hold to success a job changes rightly skipped,
#   8. a workflow ci_job_reads.py's PUSH_WORKFLOWS pairs with a filter whose on.push.paths do not
#      name exactly that filter's patterns (anchors flattened), naming each missing and extra one,
#      or that is missing or has no on.push.paths: it runs on main the work the filter's job runs
#      on pull requests and merge groups (docs-pages.yaml, the docs filter), so the two lists
#      cannot drift,
#   9. a pattern that selects nothing, or selects a job that reads nothing it matches, which are
#      the rule's two directions. The first: a pattern of a changes filter, or a read
#      ci_job_reads.py declares (a gate's or a RULES row's), that matches no file of the
#      repository, naming the pattern and the filter, gate or row it is in - a filter and a
#      declared read that name the same path which does not exist are consistent with each other
#      and select nothing, so every rule above passes while a change to the files they mean
#      starts no job. The second: a pattern of a gate's filter that matches files of the
#      repository but none its job reads, naming the pattern, the filter, how many files of the
#      repository it matches, and the job (and the step, where the gate is a step's) - the filter
#      then selects its job for a change no step of it sees, which costs a full run of that job
#      every time one of those files changes. The second direction is judged over the files the
#      gate reads, the set rule 1 holds its filter to, so a filter two gates name (envoy_go_race_docs
#      today) is judged over the union of their reads, since a pattern matching only what the
#      second job reads does start a job that reads the change. Three exemptions, and one pattern
#      judged once: a `!` pattern is exempt from both directions, since it names what to leave
#      out and so reads no file by design; an EXCLUDED filter is exempt from the second, since
#      this check computes no read set for one (worker_image, whose coverage
#      .github/scripts/check-image-trigger-paths.sh proves instead); a pattern whose every file
#      another rule of this check holds that filter to is exempt from the second, since refusing
#      it would refuse the rule that put it there - rule 8 keeps a PUSH_WORKFLOWS workflow's
#      on.push.paths equal to its filter and a workflow that does not name itself never runs on a
#      change to itself, so the docs filter holds .github/workflows/docs-pages.yaml to keep
#      main's docs build running when its own workflow changes, and rule 15 holds the filter of
#      the job that runs a package's untagged tests to every file the tag step's filter covers,
#      so envoy_go_race holds .github/scripts/go-tagged-tests.sh, which
#      listener-server-change's soak takes its -run from and no step of envoy-go-race reads; and
#      a pattern that matches
#      no file of the repository at all draws the first direction's refusal alone, never both.
#  10. an `if:` of ci.yaml, a job's or a step's, that tests `github.event_name == 'push'` or
#      `github.event_name != 'push'`, or that compares `github.ref` with a single-quoted
#      `'refs/heads/...'` literal with `==` or `!=` (either operand first), naming the file, the
#      job, the
#      step and the expression: this workflow has no `push:` trigger, so such a condition gates
#      its job or step on what no run of it ever is, and the dead code reads as live - a step
#      behind `if: github.event_name == 'push'`, or a job behind `if: github.ref ==
#      'refs/heads/main'`, runs in no run of this workflow while its text says it runs on main.
#      A cache that must be filled on a push to main belongs in .github/workflows/ci-caches.yaml;
#      a job that must run at main's tip is reached by the workflow_dispatch the bypass check
#      (.github/workflows/main-bypass-check.yaml) sends. It reads ci.yaml, and with it every
#      workflow ci.yaml calls that is ci.yaml's alone: no other workflow of .github/workflows
#      names it in a `uses: ./.github/workflows/...` and its `on:` declares workflow_call and
#      nothing else. A reusable workflow runs inside its caller's run and under its caller's
#      event, so the reason above carries to a called file only when ci.yaml is the only way
#      into it: docs-site.yaml is called by docs-pages.yaml too, which runs on a push to main,
#      and worker-image.yaml by release.yaml, so a push condition in either is live code there,
#      while one in a file that holds the steps of a single ci.yaml job is as dead as it would
#      be in ci.yaml itself. It refuses those two comparisons and
#      nothing else: `github.ref_name`, and `github.ref` outside such a comparison, are left
#      alone, since a merge_group run's github.ref really is a branch ref
#      (refs/heads/gh-readonly-queue/...) and a startsWith on it is live. As everywhere here, the
#      same test written as a function call is the review's to catch, not this check's.
#  11. a cache family .github/workflows/ci-caches.yaml saves whose `key:` or `path:` is not
#      exactly that of the matching actions/cache/restore of ci.yaml or of a workflow ci.yaml
#      calls - dispatch-browser.yaml holds the browser and apt archive restores of the
#      dispatch-browser job - (the keys compared as text,
#      trailing whitespace aside, the paths as ordered lists of stripped lines), naming the file
#      the restore is in, or that none of those files
#      restores, and an actions/cache/restore of ci-caches.yaml that is not
#      `lookup-only: true` or that carries restore-keys. ci-caches.yaml fills the entries ci.yaml
#      restores, and a key or a path that drifts apart makes every restore miss silently: nothing
#      fails, every job just rebuilds from nothing what the fill already built. A save is matched
#      to a restore by family, the key's text up to its first `${{`, which is the Go version the
#      entry is keyed on. A family ci.yaml restores that ci-caches.yaml does not save is no
#      failure: go-docs is saved by docs-pages.yaml, from the build job that workflow shares with
#      ci.yaml's docs job. Each restore of ci-caches.yaml is a lookup, which must download
#      nothing: the workflow asks only whether that exact key is already there, no step of it
#      reads a restored entry, and a prefix hit would answer for an older key and skip the build
#      that fills this one, leaving it empty.
#  12. a job of ci.yaml whose `if:` lacks `(github.event_name != 'merge_group' ||
#      github.run_attempt == '1')`, or, for a job whose `if:` admits merge_group alone (the
#      one cancel job, `cancel-started`), `github.run_attempt == '1'`: a group is
#      tested once, and `gh run rerun --failed` leaves out a `changes` that succeeded and hands
#      the attempt that job's outputs again, so a job gated on `needs.changes.outputs.<x>`
#      alone runs in full on a re-run attempt of a merge-group run and posts a ci-ok on a head
#      the queue has thrown away. Each job needs the one term and never both, and the two jobs
#      named for a check main's ruleset requires, `ci-ok` and `pr-title`, must hold neither: a
#      skipped job's check run is a passing required check, so a term that held one of them back
#      on such an attempt would hand the group that check concluded by nothing. Each of the two
#      runs on every attempt and refuses one above the first in a step.
#  13. a workspace of the root package.json that ci_job_reads.py's WORKSPACES does not name, or a
#      WORKSPACES entry the root manifest does not list, naming the workspace. A frozen
#      `bun install` reads every workspace manifest, so the table declares each of them a read of
#      every Bun job and this check holds every filter gating such a job to all of them. What the
#      rule prevents is exactly that: a workspace missing from the table is a manifest no filter
#      has to select, so a change to it starts no job. It prevents nothing else, and
#      packages/broker-client is the case that shows the difference - it was outside the table
#      from the day it was added, and its own typecheck and tests ran in no job for a second
#      reason no rule reads, that no step ran them. The other way round is a read every Bun job
#      declares of a manifest no install reads. The manifest is read as
#      .github/scripts/check-bun-workspace-manifests.sh:103-111 reads it, at every shape: a
#      package.json that is missing or is not valid JSON, and a `workspaces` that is not a list
#      or holds anything but non-empty strings, are named rather than taken for an empty list; an
#      empty array is accepted, as that script accepts it, since a root declaring no workspace
#      agrees with a table naming none; and a trailing slash is stripped on both sides, since
#      `packages/x/` and `packages/x` are one workspace to bun. A workspace's
#      sources are another matter, which this rule says nothing about either: a job can compile a
#      package whose filter never selects it, which is why ci_job_reads.py's envoy_client_dep
#      declares, in one place for every gate whose job compiles @legion/envoy-client, that it
#      compiles @legion/broker-client with it.
#  14. a workflow of .github/workflows other than ci.yaml that runs on `merge_group`, naming the
#      file: ci-ok-report.sh POSTs the failing ci-ok of a merge-group run the reporting job's
#      own result decides, so the queue learns a group is doomed without waiting for the ci-ok
#      job, and a POSTed check run joins the Actions App's first check suite at that commit
#      rather than the posting run's. That POST reaches the queue only because ci.yaml is the
#      one workflow a merge group starts, which makes its suite the first one; a second such
#      workflow can hold that suite instead, and this repository has measured that the queue
#      then does not read the POST - pull request #2847 was refused with `Required status check
#      "ci-ok" is expected` while a green ci-ok sat in PR Title's suite. Work that must run on a
#      merge group belongs in ci.yaml.
#  15. a tag step of ci.yaml, or of a workflow ci.yaml calls, that passes `-run` and whose
#      pairing in ci_job_reads.py's TAG_RUNS
#      is missing or no longer holds. A step that runs a package under a build tag and passes
#      `-run` runs only the tests the tag adds, so the package's untagged tests must run
#      elsewhere in every run that step runs. The job that runs them is declared, and its filter
#      must cover every file the tag step's filter covers; otherwise a change to a file only the
#      tag step's filter matches runs the tagged tests alone and the untagged ones nowhere. It
#      fails naming both jobs, the step and those paths (up to 20, with how many more there
#      are). The table is no entry's to opt into: every `go test` step of those files that
#      passes
#      both `-tags` and `-run` must be named by an entry, matched by the file it is in and by
#      its job and step name, and
#      one no entry names fails naming that file, the job and the step, so a tag step added
#      without an
#      entry, or an entry deleted while its step stays, is held too. Nor can the table go stale:
#      an entry naming a filter the changes job does not have, a job the file the entry names
#      does not have (the job that runs the untagged tests may be in any of those files, since
#      it keeps its caller in ci.yaml), a
#      step that job does not have, or a step whose run: holds no -run is a failure of its own.
#  16. a `go test -race` build of .github/workflows/ci-caches.yaml whose packages are not
#      ci_job_reads.py's DAEMON_RACE_PACKAGES, in that order. That build fills the cache
#      ci.yaml's race step restores, so a package it names that the lane does not is a race
#      binary no job of ci.yaml restores, and one the lane names that it does not is a binary
#      that step links from nothing on every run - in either direction nothing fails, which is
#      how ./internal/record/ stayed in the fill after the lane dropped it. It fails naming the
#      package, the job and both lists, and a ci-caches.yaml with no race build at all fails
#      too, since the lane would then be filled by nobody.
#  17. a gate's filter whose generated block is not exactly the `!` patterns that keep it off
#      every Go test file it would select and its job does not read, naming the filter, the
#      first pattern that differs and the command that writes the block; and a `!` pattern of
#      such a filter outside that block that matches a Go test file, since which test files a
#      filter excludes is this rule's to compute rather than anyone's to write. Rule 1 holds a
#      filter to the files its job reads, which is coverage; this one holds it off the files its
#      job does not, which is selection, and no other rule here can: rule 9's second direction
#      refuses a pattern matching nothing the job reads, and the pattern that selects a test
#      file - packages/envoy/internal/dispatch/api/*, packages/envoy/** - matches the sources
#      the job does read, so it stands. What that costs is a full run of a job for a change no
#      step of it compiles: a test file of a package whose tests the job never runs. The
#      attribution is already here, since ci_filter_coverage.py gives a package's TestGoFiles,
#      its XTestGoFiles, its test embeds and its testdata/** to a run that named that package in
#      test mode alone, so the read set this rule subtracts is the one rule 1 holds the filter
#      to. Two forms are written, the fewest of them that say it: `!<tree>/**/*_test.go` where
#      no test file the job reads is anywhere under <tree>, and `!<path>` where one is, the walk
#      taking the highest tree that may go whole, naming file by file the excluded files of a
#      directory that holds a read one too, and reading directories and files in sorted order,
#      so two runs over one tree write one text. They are generated because they are too many
#      to keep by hand - 520 patterns over 19 of this file's filters today, 22 for
#      daemon_go_tests and 137 for docs - and because none of them is a judgement: each follows
#      from the read sets above. The block belongs to the filter whose list holds it and never
#      to a group an anchor names, since two filters sharing an anchor need not run the same
#      tests; a filter with nothing to exclude carries no block, and so does a filter no gate
#      of this check computes a read set for (worker_image, whose test files
#      check-image-trigger-paths.sh judges with the image's build context). The write mode
#      writes the block into the on.push.paths of a PUSH_WORKFLOWS workflow as well, since
#      rule 8 holds that list equal to its filter and a hand would have to copy it otherwise.
#  18. a dorny/paths-filter step of the changes job whose `filters` string is within 32,768
#      bytes of 131,072, naming the step, the size and the cap. Actions hands a step's input to
#      the action as one environment variable, INPUT_FILTERS, and Linux caps one environment
#      string at MAX_ARG_STRLEN, 131,072 bytes: a block above that does not make the action
#      filter badly, it stops the job from starting at all, with `An error occurred trying to
#      start process '.../node24/bin/node' ... Argument list too long` and no filter output for
#      any job of the run. That is what one run gave (dispatch://INFRA-1413, pull request
#      #2945): the whole block in one step measured 137,498 bytes of this branch's text once
#      rule 17's 520 generated `!` patterns were written into it (137,494 when that issue
#      measured it), against 76,669 bytes on main. The cure is that the changes job declares
#      its filters in two paths-filter steps rather than one - 77,794 and 62,209 bytes today -
#      and this rule is what keeps the next filter from growing one of
#      them back over the cap: it fails here, on the pull request that writes it, rather than
#      on a runner. The margin is a quarter of the cap, which leaves the longer of the two
#      20,510 bytes to grow in and covers many times over the 14 bytes of `INPUT_FILTERS=` the
#      kernel counts beside the value. Every rule above reads the filters of all those steps as
#      one set, since that is what the job's outputs hand on; a group an anchor names that
#      filters of both steps share is written out in both, because a YAML anchor reaches no
#      further than the string it is in, and the two copies must be the same patterns under the
#      same quantifier or this rule's own refusal names both steps,
#  19. an output of the changes job reading `steps.<id>.outputs.<name>` where <id> is no step of
#      the job, or is a paths-filter step that does not declare filter <name>, or where <name>
#      is declared in two paths-filter steps, naming the output, the reference and the step that
#      does declare the filter. With the filters split over two steps (rule 18) an output names
#      the step its own filter is in, and one naming the other step is empty on every run, which
#      reads as a filter that never matched: the job it gates would be skipped on every change
#      it selects, and nothing else here would say so. A reference to a step that is no
#      paths-filter step carries no filter and is held only to being a step of the job: that is
#      the step merging the two steps' verdicts into the `filter_verdicts` output
#      .github/scripts/ci-ok.jq reads as one object.
#
# A claimed go command is reduced only in its simple forms, never parsed further: one line of
# `go test|vet|build`, or of `go -C <directory>` and one of those three, under a leading run of
# NAME=value assignments whose every name is in ENV_ALLOWED, then relative package paths and only
# -v, -race, -count=N, -timeout D, -buildvcs=false and one -tags T, that is its step's whole run:
# script (split at \n alone, stripped of spaces and tabs, comments and blank lines aside; a
# script holding any character outside printable ASCII, tab and \n is not simple), with no shell:
# on the step and no defaults.run.shell on the job or the workflow, no container: on the job, no
# name outside ENV_ALLOWED (test configuration go never reads; GO*, CGO_*, BASH_ENV, ENV, HOME,
# XDG_CONFIG_HOME and PATH are outside it) in the workflow's, job's or step's env or in front of
# the command itself, no step of the job writing $GITHUB_ENV or $GITHUB_PATH or running
# `go env -w` on a line of its script that is not a comment (each reaches every later go command
# unseen), and the step's working directory (its own, else the job's or workflow's defaults.run,
# else the root) joined with the command's own `go -C` the Run's. A -race line's closure is the
# one `go list -race` lists, so its Run says race too. A script_run is held exact instead: the
# script's line holding its text must be that text alone and uncontinued, under such an env,
# shell and container (the job's and every one of its steps') and such a job; the script's
# variables are the Run author's to read, and anything appended to that line falls to everything.
# A step whose go commands are in no such form can be an exact entry instead: it pins the step's
# whole run: script (compared as text after trailing whitespace, nothing parsed) and directory,
# and lists Runs that stand for what the script compiles. While the step holds exactly that, under
# such an env, shell, container and job, its go lines are claimed and the Runs' closures checked;
# any difference makes every one of its Runs fall to everything. Any other claimed command - an
# assignment of a name outside ENV_ALLOWED such as GOFLAGS or GOOS, another flag, a pipe, a
# quote, a substitution, a cd, another line in
# its step - counts as reading every Go file of the repository, every file under a Go module's
# directory (each directory holding a go.mod: testdata, embedded files and all) and every go.mod,
# go.sum, go.work and go.work.sum, and fails naming the command unless its filter selects all of
# them. So a cd, a `go -C` naming another directory than the Run's or a step in one can only
# widen what it reads. It fails wide, never narrow: a new evasion is answered by falling to
# everything, not by more parsing.
#
# What it reads, and what it does not: ci.yaml's own steps and those of the jobs of a local
# workflow one of its jobs calls (their run: text, shell, working directory and env: blocks),
# each job's own env and defaults and those of the file that holds it, and the scripts and steps
# the table pins. It does not read the environment a local action or a repository script writes
# (a composite action's own $GITHUB_ENV or $GITHUB_PATH, a script's export), nor what such a
# script runs beyond its pinned line. A pinned repository script is read the way Python splits
# lines, so a \r hidden inside one is outside what it reads too. It answers accidental drift: a
# new import, file or command that silently stops a job running on its own inputs. A committer
# who deliberately hides a command or a flag where this check does not read is the review's to
# catch.
#
# The go commands run with GOPROXY=off over an empty module cache: nothing is downloaded, no
# go.work.sum is rewritten, and a cold runner gives the answer a warm machine does. Third-party
# packages then fail to load, which loses no first-party package, since none of them imports
# one: no module outside this repository requires one of its modules (`go mod graph`), and all
# but twelve of its 185 packages are main packages or internal/ ones, which nothing outside the
# repository may import. The first-party sets matched a run over a full module cache file for
# file when this check was written.
#
# One thing that empty cache cannot hold is the Go toolchain itself. Where the installed go is
# older than the `go` directive of the module being listed, GOTOOLCHAIN=auto satisfies the
# directive by fetching the pinned toolchain into GOMODCACHE, so every go command of this check
# would try to fetch it into the empty one and GOPROXY=off would refuse it: `toolchain not
# available` on each, and every closure unknown (1,243 reported problems on an agent pod whose
# go was go1.27.1 against this repository's go1.27.3). CI never sees it, since setup-go installs
# the pinned version. The engine therefore resolves the toolchain first, once per module
# directory and under this machine's own GOMODCACHE and GOPROXY (`go env GOROOT`), and runs
# every go command with that toolchain's own binary under GOTOOLCHAIN=local.
#
# EXCLUDED leaves worker_image to check-image-trigger-paths.sh, which proves it covers every file
# its image build reads. envoy_smoke is a gate whose image inputs are that check's: this one holds
# it to the job's go commands and other step reads. The RULES and the static reads are kept by
# hand: this check proves the filters cover them, not that they are complete, so a change that
# makes a test read a new file through `..` or start a new go command adds it to ci_job_reads.py.
#
# THE TWO MODES. This check takes an optional mode argument. `check`, the default a bare call
# takes, is every refusal above. `write` is the same run with rule 17's verdict replaced by its
# remedy: it writes each gate filter's generated block into ci.yaml and refuses nothing of rule
# 17, leaving every other refusal as it is, since the blocks it writes are computed from the
# read sets the rest of this check builds and a run that cannot build them writes nothing worth
# keeping. Only ci.yaml is written, and only inside those blocks. Any other word, or more than
# one, fails naming both.
#
# Run from anywhere: .github/scripts/check-ci-filters.sh
#                    .github/scripts/check-ci-filters.sh write
# CI runs it in ci.yaml's ci-filters job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# A bare call checks: that is the contract the ci-filters job calls under, and a caller who says
# nothing is asking for the verdict, never for a rewrite of ci.yaml.
case "$#:${1:-}" in
  0: | 1:check | 1:write) ;;
  *)
    echo "::error file=.github/scripts/check-ci-filters.sh::this check takes one optional mode: \`check\` (the default) refuses a generated block of ci.yaml that is not rule 17's, and \`write\` writes those blocks instead. Run \`.github/scripts/check-ci-filters.sh\` or \`.github/scripts/check-ci-filters.sh write\`." >&2
    exit 1
    ;;
esac
export CI_FILTERS_MODE="${1:-check}"

# The engine (ci_filter_coverage.py), its data (ci_job_reads.py), the rules that read no filter
# and no go command (ci_workflow_rules.py: rules 10 to 14 above, the workspace one
# given that data's WORKSPACES by main() rather than importing the data itself, the merge-group
# one reading .github/workflows itself) and the path
# model (workflow_paths.py) sit beside this script in the repository it checks. The engine is
# imported, not run as a script, so the data's `from ci_filter_coverage import ...` binds the
# same classes.
# PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts exec python3 -c \
  'import sys, ci_filter_coverage; sys.exit(ci_filter_coverage.main())'
