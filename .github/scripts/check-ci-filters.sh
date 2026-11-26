#!/usr/bin/env bash
# `.github/workflows/ci.yaml#changes` job selects each job through a dorny/paths-filter filter, and a change to a
# file a job reads but its filter misses skips that job, on the pull request and again on the
# merge group that lands it, after which nothing runs it. So every filter must match every file
# its job reads. No file of the repository holds the filters: the changes job computes them when
# it runs (`generate`, below, through .github/scripts/ci-filters-generate.sh, which falls open to
# selecting every job when the computation fails), from what each job reads, and this check holds
# what they are computed from to ci.yaml.
#
# What a job reads is, per gate (a file of .github/ci-gates, loaded by .github/scripts/ci_job_reads.py):
#   * the closure of each go command the job runs, live: `go list -deps [-test] [-tags ...] [-race]`
#     over the command's packages, in its directory, with its GOOS/GOARCH/CGO_ENABLED and -overlay.
#     Each first-party package directory counts as `<dir>/*`, its `_test.go` files aside: those
#     belong to the runs that compile them, so a test run adds the package's test build and its
#     `testdata/**`, and a test file a build constraint keeps out of one listing - a
#     `//go:build race` test, a tagged live one - is read by the run whose -race or -tags puts
#     it in, and by no other. A directory holding a test file is therefore named file by file;
#     every run adds each embedded file and, for a `<root>/...` pattern, every directory with .go
#     files under root (the go command reads their build constraints);
#   * what a test compiled into such a run does outside its package (the //ci: lines at the top
#     of the test file, applied only when go list says the file is compiled under the job's tags,
#     .github/scripts/ci_filter_sources.py): the closure of a go command it starts, and each file
#     it reads through `..`;
#   * the module files of each go command's workspace: go.work, go.work.sum and every bound
#     module's go.mod and go.sum, as `go env GOWORK` and `go work edit -json` report them;
#   * for a claimed go command this check does not reduce (below), every Go file of the
#     repository and every file under a Go module's directory;
#   * the static reads of the job's other steps: actions, the Bun install, Biome, scripts,
#     fixtures, and the job's own gate file, which its filter is computed from;
#   * what each gate on one of the job's steps reads, since the step runs only in a run the
#     job's filter selected.
# Those globs are expanded over the repository's files (tracked, and untracked ones git does not
# ignore, so an uncommitted new file counts and a build output does not), and the filter is
# written from the files (ci_filter_coverage.compute_filters): `<dir>/**`, `<dir>/*` or the path,
# and `!` patterns keeping it off the Go test files those select and the job does not compile,
# read under dorny/paths-filter v4.0.3's `some-with-excludes` (workflow_paths.PathsFilter). Each
# filter is checked against its read set before it is written: it must select exactly those
# files. For a Bun gate the TypeScript half of its reads is listed by the tools that compile it:
# where a filter names a tree file by file (.github/scripts/ci_ts_programs.py, LISTED), the
# generation lists the tsc programs and bundles with Bun into a listed block (`# BEGIN listed` ...
# `# END listed`), and .github/scripts/check-ts-filters.sh holds every filter to them again in
# ci.yaml's lint job.
#
# The table cannot drift from ci.yaml silently. Fails on:
#   1. retired: it held the committed filters file to the read sets, which the filters are now
#      computed from,
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
#      gate: `.github/workflows/dispatch-browser.yaml#lane`, which each of ci.yaml's three callers of it passes as
#      a literal, chooses which projects the browser suite runs, never whether it runs,
#      and where such a value stands in for a gate the table declares, this rule's own first
#      half refuses that gate. A `with:` value no condition reads gates nothing and is left alone.
#      A step may read its filter out of the changes job's one list of the filters that matched
#      instead, `contains(needs.changes.outputs.matched_filters, '"<filter>"')` once resolved, as
#      the test job's steps do (rule 23 holds that shape): the filter each `contains` names is a
#      gate of the step, and the list itself is none. The list hands on every filter that
#      matched, so a filter a step reads out of it needs no output of the changes job of its own,
#      as one read through an output does,
#   3. a job or step gated on a filter the table neither lists in GATES nor EXCLUDES, in ci.yaml
#      or in a workflow it calls, where the filter is the one the resolved condition names, or
#      the one it reads out of the list,
#   4. a run: line of a job of ci.yaml, or of a job of a workflow ci.yaml calls - which is where
#      such a job's steps are - that starts a go command (test, vet, build, run, list, install,
#      generate) no Run of a gate on that job claims and no exact entry's step holds, or a
#      claimed line in the simple form (below) whose packages, -tags, -race or mode (test, vet,
#      or build) are not exactly its Run's. A job that calls a reusable workflow keeps its id,
#      its needs: changes and the whole if: that tests its filter in ci.yaml and holds its steps
#      in the called file, which runs them inside the caller's own run, so the steps of every one
#      of those files are read here and each refusal names the file the step is in,
#   5. a Run whose pinned text its step or script no longer holds, an exact entry whose step the
#      job no longer has, a step its exact entry pins whose run: script or directory is not the
#      entry's, or whose env, shell, container or job could move what it compiles (below), or a
#      why citing a step the job no longer has. A gate's
#      step is looked for in the file that gate names - ci.yaml, or the workflow it calls that
#      holds that job's steps - and a why's in the file its citation names, so a step that moved
#      between the two fails here until the table says where it went,
#   6. a go command that fails or a first-party package go list cannot load, since its closure
#      is then unknown, and a `!` pattern of a computed filter under `some`, or a brace in a
#      pattern written by hand,
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
#      and pr-title), or named by an expression that can evaluate to one, that is not the job of
#      that name; ci-ok carrying any `name:` but `${{ github.event_name == 'workflow_dispatch' &&
#      needs.changes.outputs.ref_is_main != 'true' && 'ci-ok (dispatch)' || 'ci-ok' }}`, pr-title
#      any but `${{ github.event_name == 'workflow_dispatch' && 'pr-title (dispatch)' ||
#      'pr-title' }}`, since a job posts its own check under its display name, a skipped one posts
#      a pass, and a dispatch of any ref but exactly refs/heads/main tests that ref's tree and
#      lands its check runs on the branch's head, a pull request's head, beside the pull
#      request's own, where a reader by name takes the newer; the ci-ok job's step "Set every
#      other ci-ok on this head to this job's conclusion" carrying any `if:` but `${{ always() &&
#      (github.event_name != 'workflow_dispatch' || needs.changes.outputs.ref_is_main == 'true')
#      }}`, since on any other dispatch it would set a pull request's ci-ok check runs to that
#      run's verdict; the changes job lacking the output `ref_is_main: ${{
#      steps.ref.outputs.is_main }}` or its one step `ref`, whose `run:` is exactly `if [
#      "$GITHUB_REF" = refs/heads/main ]; then echo is_main=true; else echo is_main=false; fi >>
#      "$GITHUB_OUTPUT"` with no `if:`, since an expression's `==` and `!=` ignore case and read
#      refs/heads/Main as main where the shell's `=` does not; a pr-title job that is missing, since nothing else
#      reports that check on a pull request or a merge group, or whose `if:` is any but `${{
#      !github.event.pull_request.draft }}`, since GitHub names a job it skips by its name
#      expression's literal text, so a dispatch must run it to name it pr-title (dispatch); a
#      ci-ok job — the verdict, whose own
#      check run is the other check that ruleset requires — that is missing, that CI_OK_EXEMPT
#      does not name, or whose
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
#      name exactly that computed filter's patterns, naming each missing and extra one, or that
#      is missing or has no on.push.paths: it runs on main the work the filter's job runs on pull
#      requests and merge groups (docs-pages.yaml, the docs filter), and GitHub reads a push
#      trigger before any job runs, so that list is the one copy of a filter the repository
#      keeps; `write` writes it, and the workflow joins its own filter, since a workflow that
#      does not name itself never runs on a change to itself,
#   9. a pattern written by hand that matches no file of the repository: a gate's read, a //ci:
#      line's glob, or a pattern of an EXCLUDED filter but its `!` ones, naming the pattern and
#      the file it is in; and a //ci:reads line whose `!` globs leave none of the files its glob
#      matches. Such a pattern holds its filter to nothing, so a change to the files it means
#      starts no job. Its other half, a filter pattern matching nothing its job reads, has no
#      subject left: a computed filter selects exactly what its job reads.
#  10. an if: condition of ci.yaml, a job's or a step's, that tests `github.event_name == 'push'` or
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
#  11. a cache family .github/workflows/ci-caches.yaml saves whose `key:` is neither exactly that
#      of the matching actions/cache/restore nor that key followed by `-${{ github.sha }}`, or
#      whose `path:` is not exactly that restore's, for a restore of ci.yaml or of a workflow
#      ci.yaml calls - dispatch-browser.yaml's binaries, multiprocess, embedder and visual jobs
#      restore go-envoy and go-envoy-test for the dispatch-browser job - or of any other workflow
#      of .github/workflows that restores it (the release builds, the nightly browser re-record,
#      ci-caches.yaml's own restores), since a drift there misses as silently (the keys compared
#      as text, trailing whitespace aside, the paths as ordered lists of stripped lines), naming
#      the file the restore is in, or that none of ci.yaml's files restores; a family saved both
#      under `-${{ github.sha }}` and without it; a save of ci-caches.yaml whose key holds
#      hashFiles(), or names `steps.<id>.outputs.cache-primary-key` of no actions/cache/restore
#      of its job (a save under that output takes that restore's key text); an
#      actions/cache/restore of ci-caches.yaml of a family it saves under `-${{ github.sha }}`
#      that sets lookup-only at all, or whose restore-keys are not exactly its key followed by
#      `-`; any other actions/cache/restore of ci-caches.yaml that is not `lookup-only: true`
#      or that carries restore-keys; and an actions/cache/restore of any workflow of the
#      directory whose family no actions/cache/save of a workflow or of a composite action of
#      .github/actions writes (a save that names its restore's cache-primary-key writes that
#      restore's family; one whose family cannot be read is named in the refusal, which then says
#      that no save it can read writes the family). ci-caches.yaml fills
#      the entries ci.yaml restores, and a key or a path that drifts apart makes every restore
#      miss silently: nothing fails, every job just rebuilds from nothing what the fill already
#      built. A save is matched to a restore by
#      family, the key's text up to its first `${{`, which is the Go version the entry is keyed
#      on. A family ci.yaml restores that ci-caches.yaml does not save is no failure when another
#      workflow saves it: go-docs is saved by docs-site.yaml, the build job docs-pages.yaml shares
#      with ci.yaml's docs job. A family no save writes is one, since a misspelled family name
#      (go-envoy-tests- for go-envoy-test) would otherwise be a family of its own that nothing
#      holds, and its restore would miss on every run.
#      A save's own hashFiles() is evaluated after the build, which can change the files it
#      hashes (a go command may add the checksums it fetched to go.sum or go.work.sum), so it can save under
#      a key no restore asks for: go-envoy-infra's did, and every push missed its lookup. A
#      family saved under the commit is filled again on every push, each entry under a key of its
#      own; no entry carries the restores' key itself, so GitHub gives each restore the newest
#      entry whose key starts with it. The fill builds on that newest entry, so its own restore
#      must download it, and its restore-keys must hold its Go version. Every other restore of
#      ci-caches.yaml is a lookup, which must download nothing: the workflow asks only whether
#      that exact key is already there, no step of it reads a restored entry, and a prefix hit
#      would answer for an older key and skip the build that fills this one, leaving it empty.
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
#      packages/cipher-client is the case that shows the difference - it was outside the table
#      from the day it was added, and its own typecheck and tests ran in no job for a second
#      reason no rule reads, that no step ran them. The other way round is a read every Bun job
#      declares of a manifest no install reads. The manifest is read as
#      `.github/scripts/check-bun-workspace-manifests.sh#root_manifest` reads it, at every shape: a
#      package.json that is missing or is not valid JSON, and a `workspaces` that is not a list
#      or holds anything but non-empty strings, are named rather than taken for an empty list; an
#      empty array is accepted, as that script accepts it, since a root declaring no workspace
#      agrees with a table naming none; and a trailing slash is stripped on both sides, since
#      `packages/x/` and `packages/x` are one workspace to bun. A workspace's
#      sources are another matter, which this rule says nothing about either: a job can compile a
#      package whose filter never selects it, which is why ci_job_reads.py's envoy_client_dep
#      declares, in one place for every gate whose job compiles @legion/envoy-client, that it
#      compiles @legion/cipher-client with it.
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
#      pairing in ci_job_reads.py's TAG_RUNS is missing or no longer holds. A step that runs a
#      package under a build tag and passes `-run` runs only the tests the tag adds, so the
#      package's untagged tests must run elsewhere in every run that step runs. The job that runs
#      them is declared, and the generation puts into its filter every file the tag step reads:
#      the read set of the gates on the tag step's filter, or, for a filter EXCLUDED names, which
#      has none, every file that filter selects; and every file of the tag step filter's listed
#      block: envoy_go takes the contracts modules of dispatch_two_builds_go's block, and
#      envoy_go_race takes .github/scripts/go-tagged-tests.sh, which listener-server-change's soak
#      takes its -run from. This check holds the computed filters to it again and reports each
#      pairing. The table is no entry's to opt into: every `go test` step of those files that
#      passes both `-tags` and `-run` must be named by an entry, matched by the file it is in and
#      by its job and step name, and one no entry names fails naming that file, the job and the
#      step, so a tag step added without an entry, or an entry deleted while its step stays, is
#      held too. Nor can the table go stale: an entry naming a filter the changes job does not
#      have, a job the file the entry names does not have (the job that runs the untagged tests
#      may be in any of those files, since it keeps its caller in ci.yaml), a step that job does
#      not have, or a step whose run: holds no -run is a failure of its own.
#  16. a `go test -race` build of .github/workflows/ci-caches.yaml's job ci_job_reads.py's
#      DAEMON_RACE_FILL_JOB names whose packages are not ci_job_reads.py's
#      DAEMON_RACE_PACKAGES, in that order. That build fills the cache ci.yaml's race step
#      restores, so a package it names that the lane does not is a race binary no job of
#      ci.yaml restores, and one the lane names that it does not is a binary that step links
#      from nothing on every run - in either direction nothing fails, which is how
#      ./internal/record/ stayed in the fill after the lane dropped it. It fails naming the
#      package, the job and both lists; that job with no race build at all fails too, and so
#      does a ci-caches.yaml with no such job, since the lane would then be filled by nobody. A
#      race build of another job fills another family and is not this rule's. And a fill job
#      ci_job_reads.py's RACE_FILL_SCRIPTS names whose steps in the script's directory do not run
#      that script's compile line once and exactly, or that run a `go test` with -race of their
#      own: such a fill reaches its race step's packages and flags through the script that step
#      runs (packages/envoy/infra/scripts/race-unit.sh for envoy-infra-build), and a fill that calls
#      it otherwise or copies its build compiles objects the step does not link, with nothing
#      failing. It fails naming the job and the line.
#  17. retired: the `!` patterns that keep a filter off the Go test files its job does not compile
#      are computed with the filter (above), where they were a committed block this rule held.
#  18. retired: the `filters` input is now a path, which cannot reach the environment-variable cap.
#  19. an output of the changes job reading `steps.<id>.outputs.<name>` where <id> is no step of
#      the job, or is a step that is not its dorny/paths-filter step, or where <name> is no
#      computed filter (no file of .github/ci-gates declares it), naming the output and the
#      reference. Such an output is empty on every run, which reads as a filter that never
#      matched: the job it gates would be skipped on every change it selects, and nothing else
#      here would say so. The one other reference an output may carry is the every-job term's
#      `steps.scope.outputs.every_job`, written by the job's scope step, which
#      ci-router-outputs.test.sh runs and holds to what it selects, and only beside a filter's
#      verdict: an output that reads it alone is true only on a run that selects every job. Also
#      a changes job with no step of id `scope`, and an output but ref_is_main that does not open
#      with `${{ steps.scope.outputs.every_job && `, since one written with an older
#      `github.event_name == 'workflow_dispatch' && 'true'` term selects its job on every dispatch
#      of a branch. The one name no filter holds that an output may read is the paths
#      filter's own `changes`, the list of the filters that matched, in the output
#      matched_filters alone, which must be exactly `${{ steps.scope.outputs.every_job && 'all' ||
#      steps.generate.outputs.every_filter == 'true' && 'all' || steps.filter.outputs.changes }}`
#      - a list that is not `all` on a run whose filter step is skipped, or on one whose filters
#      fell open, whose fallback file holds no filter a step reads out of the list, skips there
#      every step it gates, and #5309's `github.event_name == 'workflow_dispatch'` term, `all` on
#      every dispatch, runs every step on a dispatch of a branch whose diff reaches few - in a job
#      whose one step of id `generate` runs the generation (ci-filters-generate.sh writes
#      every_filter=true to that step's outputs when it writes the fallback file), while every
#      filter's name is letters, digits, `_` and `-` (which JSON writes as they are) and no two
#      names differ only in case (GitHub's `contains` ignores case). And with or without the
#      list, a gate declaring a filter whose name the action writes another output under:
#      `changes`, under which it would write that filter's verdict in place of the list, and which
#      the fallback file leaves out, or `<another filter>_count` or `_files`,
#  20. a step's vets for another platform - the Runs of mode vet whose env sets GOOS, one per
#      tag set, today the three darwin vet steps ci_job_reads.py's darwin_vet pins - that
#      between them pass over a file go builds for that platform under some tags, naming the
#      file, the job, the step and the tags that build it. Such a step is its module's one
#      compile for that platform, and a file outside every set is never read while the vet still
#      passes: the miss a hand-kept tag list makes the day a tag lands, and the one a single set
#      makes beside a `!tag` file. The files are those on disk under the Runs' patterns, so a
#      package whose every file a missing tag excludes is among them; go list says which each
#      Run compiles, and of the rest, each whose constraint names a tag (no platform or
#      architecture feature name, none of go_build_tags.py's NOT_TAGS: those the toolchain sets,
#      and odatrace, which only forced-order.sh's patched build sets) is listed under every
#      choice of its own tags, and one go list then compiles is the miss. The steps read their
#      sets off the module with .github/scripts/go_build_tags.py at run time and ci_job_reads.py
#      gives their Runs what it prints now, so this rule fails when that script stops printing a
#      set a file needs,
#  21. a dorny/paths-filter step of any workflow of .github/workflows whose `filters` input is
#      not exactly `${{ runner.temp }}/ci-filters.yaml`, or that no earlier step of its job writes
#      with `.github/scripts/ci-filters-generate.sh "$RUNNER_TEMP/ci-filters.yaml"`, naming the
#      file, the job and the step. Every filter is computed into that file, so a filter written
#      into a workflow's own text, or a file nothing wrote, would gate its job on patterns no rule
#      here computed, and nothing would say so.
#  22. a gate that reads a harness under .github/scripts (`*.test.sh`) and does not read a script
#      that harness reaches: one it links into its fixtures or runs as `$script_dir/<name>`
#      (directly or through a `for <var> in ...` list), one its Python imports, each module those
#      import at their top level, and every gate file where one of them loads the gate files
#      (ci_job_reads.py), naming the gate, the harness, the file and how it is reached. A harness
#      tests what it reaches, so a change to one of those files alone must start its step.
#  23. a condition of ci.yaml or of a workflow it calls that names the changes job's list output
#      matched_filters, resolved through the caller's `with:`, and is not on a step or is not, as a
#      whole, `needs.changes.result != 'success' || needs.changes.outputs.matched_filters == 'all'
#      || contains(needs.changes.outputs.matched_filters, '"<filter>"')`, with more `||
#      contains(...)` terms for a step that runs when any of several filters matched (spaces, and
#      the case of `contains`, free), naming the file, the job and the step. `!= ''` is true on
#      every run; a `!` before the `contains` runs the step when its filter did not match, and a
#      term after it can stop it running at all; a `contains` without the quotes matches every
#      name holding the filter's (`ci_tools_test` inside `mirror_ci_tools_test`); a condition
#      without the `all` term is skipped on every run that selects every job, and one without the
#      result term on every run whose changes failed; and a job-level read selects a job by an
#      output that is never 'true', which ci-ok's CI_OK_SELECTED_BY cannot map. Also a condition, a
#      `with:` value of ci.yaml, or any other `${{ }}` of a job of ci.yaml or of a workflow it
#      calls (but ci.yaml's ci-ok job), that names the changes job other than as
#      needs.changes.result, needs.changes.outputs.<name> or needs.changes, or an input other than
#      as inputs.<name> (`needs['changes']`, `outputs['x']`, `INPUTS.x`, `inputs['x']`,
#      `inputs. x`, `toJSON(inputs)`): GitHub reads those spellings as the same values, and no rule
#      here reads them, so a read so spelt would be held to nothing. An input's name is read in any
#      case, as GitHub reads it. Also a condition reading the ci-ok payload
#      (`toJSON(needs.changes)`, which holds the list and filter_verdicts), filter_verdicts,
#      `toJSON(needs)` or every job's outputs (`needs.*` but `needs.*.result`) other than as the
#      payload's `!= ''`, which asks only whether ci.yaml is the caller; and a `run:`, `env:`,
#      `with:` or other text of a job that reads any of them, but ci.yaml's own `with:` to a
#      workflow it calls, the ci-ok report action's `changes:` and ci.yaml's ci-ok job: each of
#      these names every filter on every run, so a step reading one for a filter
#      (`case '${{ inputs.matched-filters }}' in *apx*)`) is gated on a filter no rule reads. Rules
#      2 and 3 read the filter out of each `contains`
#      (.github/ci-notes/ci.md#one-list-for-the-test-jobs-step-gates).
#  24. a spec file the `references` lane of .github/workflows/dispatch-browser.yaml runs - an entry
#      of the `lane_files="..."` that .github/scripts/dispatch-browser-shard-suite.sh, the script
#      its shard job's browser step runs, sets in its `references)` arm, read as a path from the
#      working directory of the step that runs the script - that the dispatch_references filter,
#      whose patterns .github/ci-gates/dispatch-references.py keeps by hand, does not match, naming
#      the entry and the path: that filter starts the job calling the lane, so a spec file it
#      misses is a change to the lane's own rows that starts no run of them. A script with no such
#      arm, with two, or with one that lists no file, a dispatch-browser.yaml that runs the script
#      from no step or from two directories, and no file of .github/ci-gates that declares that
#      filter, fail too, rather than holding the filter to nothing.
#  25. a job of ci.yaml gated on a filter that no file of .github/ci-gates declares a gate of for
#      that job, or a step gated on a filter no gate of which names the step's job, naming the job
#      and the jobs the filter's gates do name. A filter is computed from the gates that declare
#      it, each for one job, so a job gated on another job's filter runs on that job's reads while
#      nothing holds its own: deleting tool-fallback-arm.py, whose filter tool_fallback
#      tool-fallback.py declares too, passed every other rule (dispatch://INFRA-5794/spec#b-r2). A
#      step that readies a gated step (a cache restore, a sysctl, a fetch) shares its gate, so a
#      gate of the filter on the step's job is enough.
#  26. a Go test file, or a Go file that holds a //ci: line, that reads a path outside its Go module
#      through `..` (a filepath.Join or path.Join whose leading arguments are string literals
#      starting with `..`, taken as far as its literals go, or a `"../..."` literal, comments
#      aside) while no //ci:reads, //ci:reads-set or //ci:builds line of that file names the path,
#      naming the file, the path and the source text. go list sees no such read and the filters
#      are computed from those lines, so a line taken out drops the read from every filter, and a
#      change to the file starts no job that runs the test: taking out a //ci:reads line of
#      packages/daemon/internal/prompts/roles_test.go passed every other rule
#      (dispatch://INFRA-5794/spec#b-r2). A path no file of the repository holds (an escape a
#      test refuses, a fixture it writes), the module's own root or a directory around it (a path
#      the source finishes with a value it computes, which the source cannot name), and a read
#      inside the file's own module, which the go list closure of the packages the job compiles
#      covers, are not this rule's.
#
# A branch made before the gates and the //ci: lines moved out of ci_job_reads.py keeps its old
# RULES and GATES there, and .github/ci-filters.yaml, when it merges main: the table refuses to
# load while it binds one of those names, and the check refuses the old file, each naming
# .github/scripts/ci-reads-convert, which moves what the branch added.
#
# Every rule reads the filters from the same run of the engine that computes them, in memory, and
# names the gate file, the //ci: line or the workflow a fault is in. The go commands of every gate
# and every //ci:builds line run at once, in a pool of CI_FILTERS_JOBS (default: the machine's
# processors, at most 8; 1 runs them one after another), each answer keyed by what it asks, so the
# filters are the same text whatever the pool's size.
#
# A claimed go command is reduced only in its simple forms, never parsed further: one line of
# `go test|vet|build`, or of `go -C <directory>` and one of those three, under a leading run of
# NAME=value assignments whose every name is in ENV_ALLOWED, then relative package paths and only
# -v, -race, -count=N, -timeout D, -buildvcs=false and one -tags T, that is its step's whole run:
# script (split at \n alone, stripped of spaces and tabs, comments and blank lines aside; a
# script holding any character outside printable ASCII, tab and \n is not simple), with no shell:
# on the step and no defaults.run.shell on the job or the workflow, no container: on the job, no
# name outside ENV_ALLOWED (test configuration go never reads, and GOPROXY and GOCACHE; any other
# GO* name, CGO_*, BASH_ENV, ENV, HOME,
# XDG_CONFIG_HOME and PATH are outside it) in the workflow's, job's or step's env or in front of
# the command itself, no step of the job up to and including its own writing $GITHUB_ENV or
# $GITHUB_PATH or running `go env -w` on a line of its script that is not a comment (each reaches
# every later go command unseen, and a write after the step reaches none of its), and the step's
# working directory (its own, else the job's or workflow's defaults.run, else the root) joined
# with the command's own `go -C` the Run's. A -race line's closure is the one `go list -race`
# lists, so its Run says race too. A script_run is held exact instead: the script's line holding
# its text must be that text alone and uncontinued, under such an env, shell and container - the
# workflow's, the job's, and those of every step of each job that may run the script, each job of
# a workflow it calls read the same way - and with no step of those jobs writing $GITHUB_ENV or
# $GITHUB_PATH or running `go env -w`, before the script's start or after it. Which step starts a
# script is nothing this check can prove: a step can start it through another script, from another
# directory or by a test it compiles, and a step that names its path may only chmod or lint it. So
# a write after the last step that shows a start still counts, which errs wide, never narrow. The
# script's variables are the Run author's to read, and anything appended to that line falls to
# everything.
# A step whose go commands are in no such form can be an exact entry instead: it pins the step's
# whole run: script (compared as text after trailing whitespace, nothing parsed) and directory,
# and lists Runs that stand for what the script compiles. While the step holds exactly that, under
# such an env, shell, container and job, its go lines are claimed and the Runs' closures checked.
# A step whose script or directory differs fails, naming the file, the job, the step and the
# first line that differs, or both directories; where all that differs on that line is an
# anchored -run or -skip list (`'^(Name|Name)$'`), it names each test the step's -run leaves out
# or adds, or its -skip no longer skips or also skips. The pin is also the table's word for which
# tests the step runs, and ci_job_reads.py builds several pins from one list: TEXT_CHECK_TESTS
# writes the text-check step's -run and is the set envoy-go's step of what the race build cannot
# run leaves out, so a test that list names and the step drops runs in no job, and one the step
# names and the list does not runs in both. Falling to everything, which is what a step off its
# pin once did, says nothing where the filter already selects every Go file, as text_checks'
# does. For the same reason a pinned step fails, naming what does it, when its env, shell,
# container or job could move what it compiles or which tests it runs: an env of the step, the
# job or the workflow naming anything outside ENV_ALLOWED, a step of the job before it writing
# $GITHUB_ENV or $GITHUB_PATH or running `go env -w` (a write in its own script is the pin's to
# hold, and a write after it reaches none of its commands), a shell: or defaults.run.shell, or a
# container: - `GOFLAGS: -skip=^TestOnlyTheBrokerInsertsEvents$` in the guard step's env would
# skip that test though its -run names it, and its pin would still match. Put such a setting on
# the step's own script line instead, where the pin holds it and the entry's Runs can say it. A
# name with which go compiles the same files and runs the same tests, such as GOMAXPROCS, may join
# ENV_ALLOWED; until it does, it refuses the step. Any other claimed command - an assignment of a
# name outside ENV_ALLOWED such as GOFLAGS or GOOS, another flag, a pipe, a quote, a substitution,
# a cd, another line in its step - counts as reading every Go file of the repository, every file
# under a Go module's directory (each directory holding a go.mod: testdata, embedded files and
# all) and every go.mod, go.sum, go.work and go.work.sum, and fails naming the command unless its
# filter selects all of them. So a cd, a `go -C` naming another directory than the Run's or a step
# in one can only widen what it reads. It fails wide, never narrow: a new evasion is answered by
# falling to everything, not by more parsing.
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
# The engine's go commands, the go list runs every closure above comes from, run with GOPROXY=off
# over an empty module cache: they download nothing, rewrite no go.sum or go.work.sum, and a cold
# runner gets from them the answer a warm machine does. Third-party
# packages then fail to load, which loses no first-party package, since none of them imports
# one: no module outside this repository requires one of its modules (`go mod graph`), and all
# but twelve of its 185 packages are main packages or internal/ ones, which nothing outside the
# repository may import. The first-party sets matched a run over a full module cache file for
# file when this check was written.
#
# Two kinds of go command run under this machine's own module cache instead. One is `go env
# GOROOT`, which resolves the toolchain (below) and writes nothing. The other is
# .github/scripts/go_build_tags.py's: ci_job_reads.py runs its tag_sets as it is imported, to give
# each darwin vet step one Run per tag set, and rule 20 runs its platforms. Its `go list -find`
# still resolves every import, so under the machine's GOPROXY it fetched every module a cold
# runner's cache lacks, about half of the changes job's generation, and in a workspace wrote into
# go.work.sum the checksum of each go.mod the committed files lacked (packages/envoy/infra/
# go.work.sum lacked two). It runs with GOPROXY=off, so it fetches nothing. A run of any mode that
# changes a file of the tree, but those its mode writes (the PUSH_WORKFLOWS workflows, and
# generate's output and the .inputs file beside it, below), still fails naming each: for a
# go.work.sum, commit what the run wrote. The tree is read before the first go
# command and after the last: each file git lists, tracked or untracked and not ignored (the files
# the engine reads), by its bytes and execute bit, read off the file rather than taken as unchanged
# because git says so (an assume-unchanged mark, a normalized line ending), so a file edited before
# the run is compared with itself. The first reading hashes every file; the second hashes again
# only a file whose lstat is not the one the first saw, since every write sets a file's change
# time. The first reading is made before the audit hook below starts recording, so the files it
# opens are not among the computation's inputs the generate mode writes. A tree git cannot list,
# or in which it lists no file, fails the run saying so, since a reading of nothing would pass any
# run.
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
# EXCLUDED leaves worker_image and browser_image to check-image-trigger-paths.sh, which proves
# each covers every file its image build reads; each is a file of .github/ci-gates holding its
# patterns. It holds dispatch_references, a file of .github/ci-gates too, to no job's reads (rule
# 24 holds it to the references lane's spec files alone): that filter gates ci.yaml's
# dispatch-references job, a pull-request preview of the reference rows that ci-ok does not need
# (CI_OK_EXEMPT), and it names less than the job reads on purpose, since the dispatch_browser
# gate's job runs those rows on the merge queue's full lane and that filter is computed from every
# read. EXCLUDED also skips the go-command claims of a job its filters alone gate, and those of
# the jobs of a workflow that job calls, read as its own; dispatch-browser.yaml's jobs, which the
# dispatch-references call reaches too, are read under the gates of its other two callers,
# dispatch_browser and dispatch_browser_pr. envoy_smoke is a gate whose image inputs are that
# check's: this one holds it to the job's go commands and other step reads. The //ci: lines and
# the static reads are kept by hand: the filters are computed from them, and nothing proves them
# complete, so a change that makes a test read a new file through `..` or start a new go command
# adds the line to that test file.
#
# THE THREE MODES. This check takes an optional mode argument. `check`, the default a bare call
# takes, is every refusal above. `write` is the same run with rule 8's refusal replaced by its
# remedy: it writes each PUSH_WORKFLOWS workflow's on.push.paths. `generate <path>` computes every
# filter and writes them to <path>, running only what they are computed from - the table, the go
# commands, the //ci: lines and the listed blocks, which need Bun and the workspace's node_modules
# - and fails, writing nothing, on any problem of that work, since the filters would then be
# unknown; ci-filters-generate.sh turns that failure into a file that selects every job. A run of
# any mode that changes a file of the tree but those that mode writes - the PUSH_WORKFLOWS
# workflows for `write`, <path> and <path>.inputs for `generate` - fails as the header says above,
# and a `generate` run that fails so removes what it wrote. Any other word fails naming the three.
#
# Run from anywhere: .github/scripts/check-ci-filters.sh
#                    .github/scripts/check-ci-filters.sh write
#                    .github/scripts/check-ci-filters.sh generate "$RUNNER_TEMP/ci-filters.yaml"
# CI runs it in ci.yaml's ci-filters job, and its tests in ci.yaml's test job.
#
# On a machine whose `go` is a bootstrap that switches to a toolchain in the module cache - every
# Legion worker pod is one - every closure above fails with `toolchain not available`, because the
# go commands run over an emptied GOMODCACHE and so cannot reach the toolchain module either. The
# real findings then sit among those, where a reader judging this by its tail or its exit status
# sees nothing. Run it on the binary of the toolchain go.work pins instead, and it answers as CI
# does: GOTOOLCHAIN=local then keeps that binary, where it would pin the bootstrap, whose refusal
# reads as a version mismatch rather than as the setup it is. Unset GOROOT too. The engine asks
# `go env GOROOT` which go to run, and an exported GOROOT is that answer whichever go PATH finds
# first, so a GOROOT another install left (mise's go1.27.0 on a dev box) runs that go, and every
# closure fails with `go.work requires go >= 1.27.3 (running go 1.27.0; GOTOOLCHAIN=local)`: 1618
# problems where CI finds none, measured on one tree. go itself names that toolchain's root: the
# go PATH finds, where it already is go.work's version (as setup-go and mise install it), else
# the toolchain module in this machine's module cache, fetched there once if it is not. Where go
# can have neither, it says why, and the recipe stops there.
#   v=$(sed -n 's/^go //p' go.work)
#   goroot=$(env -u GOROOT GOTOOLCHAIN="go$v" go env GOROOT) &&
#     env -u GOROOT PATH="$goroot/bin:$PATH" GOTOOLCHAIN=local .github/scripts/check-ci-filters.sh
# Either way, read its output by grepping for the problem rather than by its last line.
set -euo pipefail

caller=$PWD
cd "$(dirname "$0")/../.."

# A bare call checks: that is the contract the ci-filters job calls under, and a caller who says
# nothing is asking for the verdict, never for a rewrite of a workflow's on.push.paths.
case "$#:${1:-}" in
  0: | 1:check | 1:write) ;;
  2:generate)
    case "$2" in /*) out=$2 ;; *) out="$caller/$2" ;; esac
    export CI_FILTERS_OUT="$out"
    ;;
  *)
    echo "::error file=.github/scripts/check-ci-filters.sh::this check takes one optional mode: \`check\` (the default) refuses what is wrong, \`write\` writes each PUSH_WORKFLOWS workflow's on.push.paths instead of refusing them, and \`generate <path>\` writes the changes job's filters to <path>. Run \`.github/scripts/check-ci-filters.sh\`, \`.github/scripts/check-ci-filters.sh write\` or \`.github/scripts/check-ci-filters.sh generate <path>\`." >&2
    exit 1
    ;;
esac
export CI_FILTERS_MODE="${1:-check}"

# The engine (ci_filter_coverage.py), its data (ci_job_reads.py, which reads the files of
# .github/ci-gates and the //ci: lines of the Go files through ci_filter_sources.py), the rules
# that read no filter and no go command (ci_workflow_rules.py: rules 10 to 14 above, the workspace
# one given that data's WORKSPACES by main() rather than importing the data itself, the merge-group
# one reading .github/workflows itself) and the path
# model (workflow_paths.py) sit beside this script in the repository it checks. The engine is
# imported, not run as a script, so the data's `from ci_filter_coverage import ...` binds the
# same classes. A gate file or a //ci: line the import cannot read ends the run with one
# annotation naming it, as main() ends one on any other repository input it cannot read.
# PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__ in the tree. An audit hook
# records every file Python opens from the moment it starts, the engine's own modules among them:
# the generate mode writes the ones the filters are computed with (ci_filter_coverage.engine_inputs)
# beside its output, so the changes job can tell a change to the computation from a change to what
# it reads.
#
# Around the engine is the guard the header describes: the tree is read before the imports, since
# ci_job_reads.py runs go_build_tags.py's go commands as it is imported, and again after main().
# The first reading comes before the audit hook too, since it opens every file of the tree and none
# of those opens is the computation's. A run whose imports fail is a failure before the second
# read has anything to add.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts exec python3 -c '
import hashlib
import os
import stat
import subprocess
import sys


def held(path, info):
    """What a file holds, given its lstat (None for no file): its bytes and execute bit, read in
    blocks, the target of a symbolic link, or that it is gone."""
    if info is None:
        return "absent"
    if stat.S_ISLNK(info.st_mode):
        return "a link to " + os.readlink(path)
    if not stat.S_ISREG(info.st_mode):
        return "no regular file"
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while block := handle.read(1 << 20):
            digest.update(block)
    return digest.hexdigest() + (" executable" if info.st_mode & 0o111 else "")


def tree(first=None):
    """Each file git lists under this directory, tracked or untracked and not ignored (the files
    the engine reads), mapped to its lstat and what it holds; or, as a string, why there is no
    such map. What a file holds is read off the file, never taken as unchanged because git says
    so: git reports no change to a file marked assume-unchanged or skip-worktree, to a line ending
    it normalizes, or to an execute bit core.filemode tells it to ignore. A file edited before the
    run is compared with itself. The first reading hashes every file; a later one, given the
    first, hashes again only a file whose lstat (device, inode, mode, size, modification and
    change times) is not the one the first saw. No write leaves all of those as they were, since
    the kernel sets the change time at each write, chmod or rename, and the first write of a run
    comes after the whole first reading. A file written again with the bytes it held is no
    change."""
    try:
        result = subprocess.run(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
                                capture_output=True)
    except OSError as failure:
        return f"git cannot list them: {failure}"
    if result.returncode != 0:
        return f"git cannot list them: git ls-files failed: {os.fsdecode(result.stderr).strip()}"
    files = {}
    try:
        for path in map(os.fsdecode, result.stdout.split(b"\0")):
            if not path:
                continue
            try:
                info = os.lstat(path)
            except (FileNotFoundError, NotADirectoryError):
                info = None
            seen = info and (info.st_dev, info.st_ino, info.st_mode, info.st_size, info.st_mtime_ns,
                             info.st_ctime_ns)
            if first is not None and path in first and first[path][0] == seen:
                files[path] = first[path]
            else:
                files[path] = (seen, held(path, info))
    except OSError as failure:
        return f"a file git lists cannot be read: {failure}"
    return files or "git lists none"


before = tree()

opened = []
sys.addaudithook(lambda event, args: opened.append(args[0]) if event == "open" and isinstance(args[0], str) else None)

import secrets
import time
import warnings

token = secrets.token_hex(16)
print(f"::stop-commands::{token}", flush=True)
with warnings.catch_warnings(record=True) as caught:
    warnings.simplefilter("always")
    import ci_filter_coverage
    ci_filter_coverage.OPENED = opened
    started = time.monotonic()
    try:
        import ci_job_reads
    except ci_filter_coverage.RepositoryInputError as exc:
        print(f"::{token}::", flush=True)
        ci_filter_coverage.print_annotation(exc.annotation())
        sys.exit(1)
    loaded = time.monotonic() - started
    import ci_workflow_rules
print(f"::{token}::", flush=True)
print(f"the table, its gate files and the //ci: lines loaded in {loaded:.1f} s", flush=True)
for import_warning in caught:
    ci_filter_coverage.print_import_warning(import_warning)
status = ci_filter_coverage.main()
after = tree(before if isinstance(before, dict) else None)

mode = os.environ["CI_FILTERS_MODE"]
written = []
if mode == "write":
    written = list(getattr(ci_job_reads, "PUSH_WORKFLOWS", {}).values())
elif mode == "generate":
    out = os.environ["CI_FILTERS_OUT"]
    written = [out, out + ".inputs"]
writable = {os.path.relpath(path) for path in written}
problems = []
unread = next((side for side in (before, after) if isinstance(side, str)), None)
if unread is not None:
    problems.append(ci_filter_coverage.error(None, f"this check could not read the files of the tree it checks "
                                                   f"({unread}), so whether this run changed one is unknown, and a "
                                                   f"run that cannot see the tree is no pass"))
else:
    may = "may change only " + " and ".join(sorted(writable)) if writable else "may change no file of the tree"
    for path in sorted(before.keys() | after.keys()):
        if path in writable or (before.get(path) or (None, None))[1] == (after.get(path) or (None, None))[1]:
            continue
        if os.path.basename(path) == "go.work.sum":
            why = ("a go command this check runs recorded module checksums the committed file lacks (go records the "
                   "checksum of each go.mod it fetches in the go.work.sum of its workspace). Commit the file as this "
                   "run leaves it")
        else:
            why = ("something this check runs wrote it, so this run judged a tree it was changing. Restore the "
                   "file and find what wrote it")
        problems.append(ci_filter_coverage.error(path, f"{path} changed while this check ran, and a {mode} run "
                                                       f"{may}: {why}"))
for problem in problems:
    ci_filter_coverage.print_annotation(problem)
if problems and mode == "generate":
    # The filters of a run that changed the tree it computed them from are unknown, and generate
    # writes nothing then.
    for path in written:
        try:
            os.remove(path)
        except FileNotFoundError:
            pass
sys.exit(1 if problems else status)
'
