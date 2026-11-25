# The private-name port harness, in a job of its own: one step, which builds the `legion`
# binary of this head and holds the rules of .github/private-names.toml, as that binary runs
# them, to the frozen shell script's verdicts, case for case. The Run is that build, so this
# filter is its whole closure; the reads are the harness, the helpers it sources, the rule file
# it names to the binary and the frozen script it compares against.
# The comment on PRIVATE_NAMES_PORT_BUILD above says why the step is a job.
GATE = gate("private_names_port", PRIVATE_NAMES_PORT_JOB, runs=[PRIVATE_NAMES_PORT_BUILD], reads=reads(
    (f"{SCRIPTS}/check-private-names-port.test.sh", at(PRIVATE_NAMES_PORT_JOB, PRIVATE_NAMES_PORT_STEP)),
    (TEST_LIB, "check-private-names-port.test.sh sources it for check/is/summary"),
    (".github/private-names.toml",
     "the rule file check-private-names-port.test.sh names to the binary in "
     "LEGION_PRIVATE_NAME_RULES_FILE (its `rule_file` and the line that exports it) and reads each "
     "rule's description out of (its `description=` line)"),
    (f"{SCRIPTS}/check-private-names.sh",
     "the frozen script every case of check-private-names-port.test.sh runs beside the "
     "engine, symlinked into each fixture (its four `ln -s \"$frozen_script\"` lines)"),
) + setup_go(PRIVATE_NAMES_PORT_JOB, f"{DAEMON}/go.mod") + go_modules(PRIVATE_NAMES_PORT_JOB))
GATE = with_reads(GATE, [
    Read('packages/daemon/internal/daemon/**', "filter private_names_port held packages/daemon/internal/daemon/** by hand before the filters were computed: it selected 7 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter private_names_port held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 278 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
