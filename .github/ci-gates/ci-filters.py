# This check itself: it lists every module's packages (vet mode: each package directory, its
# dependencies' and the test files, without the files tests read), reads the target lists
# the runs above expand, reads every workflow of .github/workflows, and is its own script,
# engine, data, rules and path model. The job runs the workflow-file size check beside it,
# which reads that same directory and is a script of its own.
GATE = gate("ci_filters", "ci-filters", runs=[
    run(DAEMON, ["./..."], f"{at('ci-filters', CHECK_STEP)}: go list over the daemon module", mode="vet", step=CHECK_STEP,
        text=".github/scripts/check-ci-filters.sh"),
    run(ENVOY, ["./..."], f"{at('ci-filters', CHECK_STEP)}: go list over the envoy module", mode="vet", step=CHECK_STEP,
        text=".github/scripts/check-ci-filters.sh"),
    run(INFRA, ["./..."], f"{at('ci-filters', CHECK_STEP)}: go list over the envoy-infra module", mode="vet", step=CHECK_STEP,
        text=".github/scripts/check-ci-filters.sh"),
], reads=reads(
    ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.go", "the pi_legion gate's go run -overlay compiles it"),
    (".github/scripts/check-ci-filters.sh", at("ci-filters", CHECK_STEP)),
    # Its go list closures, every command's and every self-starting test binary's of the
    # daemon and envoy modules, are inside the daemon and envoy runs above.
    (".github/scripts/testcontainers_links.py", at("ci-filters", TC_LINKS_STEP)),
    (".github/ci-gates/**", f"{at('ci-filters', CHECK_STEP)} reads every gate from them, which every filter is "
                            f"computed from"),
    (".github/scripts/ci_filter_sources.py", "ci_job_reads.py reads the gate files and the //ci: lines through it"),
    (".github/scripts/check-workflow-files.sh",
     at("ci-filters", "Check every workflow file GitHub can read and its size")),
    (".github/scripts/postgres-service-settings.sh", "check-workflow-files.sh runs its --command for rule 7, "
                                                     "the command line every Postgres service must carry"),
    # Rule 8 of that check (.github/scripts/go_test_steps.py, which it imports) reads every
    # repository script a step of ci.yaml's run runs, sources or hands a shell, the harnesses
    # harness-pool.sh starts among them, and the scripts those run, and refuses a step whose script
    # this filter does not select: a go test added to such a script would otherwise merge on a pull
    # request that starts no ci-filters job, and fail the next one that does on a step it never
    # touched. Of the scripts it reads, the reads below and two this gate holds besides
    # (.github/actions/** and packages/envoy/**, which hold the architecture-check action's harness
    # and envoy's deploy, loadtest and infra scripts) select every one.
    (".github/scripts/**", "check-workflow-files.sh's rule 8 reads the scripts the steps of ci.yaml's run reach here"),
    ("scripts/**", "check-workflow-files.sh's rule 8 reads the scripts the steps of ci.yaml's run reach here"),
    ("packages/*/scripts/**", "check-workflow-files.sh's rule 8 reads the scripts the steps of ci.yaml's run reach here"),
    ("packages/dispatch/e2e/visual-run.sh", "check-workflow-files.sh's rule 8 reads it: dispatch-browser.yaml's visual job runs it"),
    (".github/actionlint.yaml", f"{at('ci-filters', ACTIONLINT_STEP)}: actionlint reads its configuration "
                                "from the project it runs in"),
    (".github/actions/**", f"{at('ci-filters', ACTIONLINT_STEP)}: actionlint reads the action.yml of every "
                           "local action a step's `uses:` names, for the inputs it checks"),
    (".github/scripts/ci_filter_coverage.py", "check-ci-filters.sh's engine"),
    (".github/scripts/ci_job_reads.py", "check-ci-filters.sh's data"),
    (".github/scripts/ci_workflow_rules.py", "check-ci-filters.sh's rules about ci.yaml's conditions and the "
                                             "cache fill workflows, which ci_filter_coverage.main() runs"),
    (".github/scripts/workflow_paths.py", "ci_filter_coverage.py reads filters through it, and "
                                          "check-workflow-files.sh imports its repeated-key test"),
    (BUILD_TAGS, "ci_job_reads.py runs it for each darwin vet's tag sets, and ci_filter_coverage.py imports "
                 "its directory walk, its constraint-name reader, is_tag, platforms and its go list "
                 "stream decoder"),
    ("package.json", "ci_job_reads.py reads the workspace list from the root manifest"),
    (GO_TARGETS, "ci_job_reads.py expands the notices and pack runs over it"),
    (CLIENT_TARGETS, "ci_job_reads.py expands the notices runs over it"),
    (INFRA_TARGETS, "ci_job_reads.py expands the notices runs over it"),
    (DOCS_SITE, "ci_filter_coverage.py looks the docs gate's step citations up in it"),
    (BROWSER_SITE, "ci_filter_coverage.py looks the dispatch_browser gate's step citations up in it"),
    (BROWSER_NIGHTLY, "dispatch-browser.yaml's other caller: a change to either must re-run this check, which "
                      "holds the dispatch_browser filter to that gate"),
    (CACHE_WORKFLOW, "ci_workflow_rules.py holds its cache saves, and ci-caches-changes.yaml's, to every "
                     "workflow's restores"),
    (".github/workflows/**", "ci_workflow_rules.py reads every workflow of the directory: "
                             "check_one_merge_group_workflow, which refuses a new one and so has to run on the change "
                             "that adds it, check_cache_round_trip, which holds every restore of a filled family to "
                             "the fill's key, and ci_only_documents, which finds the files only ci.yaml calls"),
    # Every filter is computed from two sets this check reads in every Go file: the //ci: lines
    # of the tests, and the repository's Go test files the `!` patterns keep a filter off. A test
    # file added, deleted or given a //ci: line anywhere changes what the changes job computes, so
    # the change that does it has to start this job.
    ("**/*_test.go", "check-ci-filters.sh reads the //ci: lines of the tests and computes each filter's `!` "
                     "patterns out of the repository's Go test files, so one added, deleted or edited changes "
                     "this check's own answer"),
) + reads(*((path, f"check-ci-filters.sh holds its on.push.paths equal to the {name} filter (PUSH_WORKFLOWS)")
            for name, path in PUSH_WORKFLOWS.items()))
  + setup_go("ci-filters", f"{DAEMON}/go.mod"))
GATE = with_reads(GATE, [
    Read('**/*.go', "filter ci_filters held **/*.go by hand before the filters were computed: it selected 78 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('**/go.mod', "filter ci_filters held **/go.mod by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares"),
    Read('**/go.sum', "filter ci_filters held **/go.sum by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares"),
    Read('packages/daemon/**', "filter ci_filters held packages/daemon/** by hand before the filters were computed: it selected 808 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/**', "filter ci_filters held packages/envoy/** by hand before the filters were computed: it selected 1536 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
