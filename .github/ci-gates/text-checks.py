# The text-check job: one `go test` of each module's text checks, in that module's own
# directory, the Dispatch API's source guards by name, and the supervise table's two pure
# pins by name. Two workspaces meet here - the root go.work binds packages/daemon and
# packages/envoy, packages/envoy/infra's own binds itself and packages/envoy - so each Run's
# workspace names the module files it reads. No Bun and no database: nothing here starts
# either.
GATE = gate("text_checks", TEXT_CHECK_JOB, runs=[
    *[step_run(TEXT_CHECK_JOB, TEXT_CHECK_STEPS[module], TEXT_CHECK_TEXT, module, [TEXT_CHECK_PKG])
      for module in TEXT_CHECK_MODULES],
], exact=[
    # The twenty-three static tests by name: a -run is outside the simple form, so the step is
    # pinned whole. A -run moves no closure - the whole package compiles whichever tests the
    # run selects - so the Run is the plain go test of the package, which is why this gate
    # reads everything the API's tests read.
    pinned_step(TEXT_CHECK_JOB, GUARD_STEP, ENVOY, GUARD_SCRIPT, [
        (ENVOY, [GUARD_PACKAGE], f"go test -run of the API's twenty-three source-check tests in {GUARD_PACKAGE}", {}),
    ]),
    # The supervise table's two pure pins by name (SUPERVISE_TABLE_TESTS), which the
    # daemon-go plain step skips: pinned whole for the same reason, and its Run is the plain
    # go test of the package, since the step compiles every test file of internal/supervise
    # whichever two the -run then selects.
    pinned_step(TEXT_CHECK_JOB, SUPERVISE_TABLE_STEP, DAEMON, SUPERVISE_TABLE_SCRIPT, [
        (DAEMON, [f"./{SUPERVISE_TABLE_PACKAGE}/"],
         f"go test -run of the table's two pure pins in {SUPERVISE_TABLE_PACKAGE}", {}),
    ]),
], reads=setup_go(TEXT_CHECK_JOB, f"{ENVOY}/go.mod"))
