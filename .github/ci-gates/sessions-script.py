# The session message-wait suite, in a job of its own: it is the one test of the whole test
# job that needs a database (`scripts/sessions/__tests__/message-wait.test.ts#SERVER` refuses
# without LEGION_TEST_PG_DSN, since a store it cannot measure is a measurement of nothing),
# and a Postgres service on the test job would start one for every change that selects any
# of its harness steps. The test reads the script it spawns and nothing else: both
# are under scripts/sessions, and both import node: builtins and bun alone, so the job
# installs no dependencies and this filter names no lockfile.
GATE = gate("sessions_script", SESSIONS_JOB, reads=reads(
    ("scripts/sessions/**", f"{at(SESSIONS_JOB, SESSIONS_STEP)}: the suite, and message-wait.ts, which "
                            "`scripts/sessions/__tests__/message-wait.test.ts#SCRIPT` names and spawns"),
) + service_settings_reads(SESSIONS_JOB) + setup_bun(SESSIONS_JOB))
