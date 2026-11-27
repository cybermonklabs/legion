# The claim model's job: its two tests by name (CLAIM_MODEL_TESTS), pinned whole since a -run
# is outside the simple form. Its Run is the plain go test of the package, which compiles every
# test file of internal/dispatch/api whichever two the -run selects, so this gate reads what
# the API's plain tests read, the module's source that its guards walk included. The rules'
# test opens the job's Postgres; the search opens none.
GATE = gate("envoy_go_claim_model", CLAIM_MODEL_JOB, exact=[
    pinned_step(CLAIM_MODEL_JOB, CLAIM_MODEL_STEP, ENVOY, CLAIM_MODEL_SCRIPT, [
        (ENVOY, [CLAIM_MODEL_PACKAGE], f"go test -run of the sender claim model's two tests in {CLAIM_MODEL_PACKAGE}", {}),
    ]),
], reads=service_settings_reads(CLAIM_MODEL_JOB) + setup_go(CLAIM_MODEL_JOB, f"{ENVOY}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "filter envoy_go_claim_model held packages/envoy/** by hand before the filters were computed: it selected 1564 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
