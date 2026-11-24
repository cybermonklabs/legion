# The step-models-chain job: the two e2e-tagged tests of internal/daemon that
# STEP_MODELS_CHAIN_TESTS names, in a file of its own
# (.github/workflows/ci-job-step-models-chain.yaml). TestStepModelsLive runs the pinned Oh My Pi
# and the pinned Claude Code behind the real shim and the real daemon, over two issues, and judges
# each step record's set model and counts against the agents' own transcripts;
# TestTheStandInGivesEachResponseItsOwnID holds the stand-ins those counts rest on to one id per
# response. The step joins -tags e2e with -run, so it runs those two alone and the package's
# untagged tests stay the daemon-go race step's (ci_job_reads.py's TAG_RUNS, rule 15).
#
# The reads the two tests make are declared where the tests are, in the //ci: lines of
# internal/daemon/step_models_live_test.go and of live_agent_proof_test.go, which this job
# compiles under the e2e tag. What is declared here is what the job's own steps read: the two
# harness installs, the test images the package's broker pulls, the Postgres sizing every lane
# that starts a Dispatch process runs, and the Bun install and Go setup the step is built on.
GATE = gate("step_models_chain", STEP_MODELS_CHAIN_JOB, runs=[
    # The chain starts a Dispatch process, so the job sizes its Postgres service as daemon-go-race
    # does.
    ceiling_run(STEP_MODELS_CHAIN_JOB),
], exact=[
    # check-ci-filters.sh reduces no step that joins -tags with -run under a job whose install
    # actions write $GITHUB_ENV, so the step's whole script is pinned. What it compiles is every
    # test file of internal/daemon the e2e tag builds, since -run leaves a test out of the run and
    # never out of the build.
    pinned_step(STEP_MODELS_CHAIN_JOB, STEP_MODELS_CHAIN_STEP, DAEMON, STEP_MODELS_CHAIN_SCRIPT, [
        (DAEMON, [f"./{STEP_MODELS_CHAIN_PACKAGE}/"], "go test -tags e2e -run over the package the two tests are in",
         {"tags": "e2e"}),
    ]),
], reads=reads(
    (".github/actions/install-claude-code/**", at(STEP_MODELS_CHAIN_JOB, "./.github/actions/install-claude-code")),
    (".claude-code-pin", ".github/actions/install-claude-code/action.yml:16 pin=$(<.claude-code-pin), the release "
                         "the chain's tester panes run"),
    (".github/actions/pull-test-images/**", at(STEP_MODELS_CHAIN_JOB, "Pull the NATS test container images")),
    (f"{DAEMON}/internal/testnats/nats.go", f"{at(STEP_MODELS_CHAIN_JOB, 'Pull the NATS test container images')} "
                                            f"const Image, the image of the package's own broker"),
) + ceiling_reads(STEP_MODELS_CHAIN_JOB) + install_jj(STEP_MODELS_CHAIN_JOB) + install_omp(STEP_MODELS_CHAIN_JOB)
  + setup_go(STEP_MODELS_CHAIN_JOB, f"{DAEMON}/go.mod") + bun_job(STEP_MODELS_CHAIN_JOB))
