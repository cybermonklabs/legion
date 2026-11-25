# envoy's script tests, the merge-queue half of the old envoy-go-checks job
# (.github/ci-notes/ci.md#envoy-go-checks-envoys-script-tests-in-the-merge-queue-only): each
# step runs a bash harness of packages/envoy over fakes, and none starts a go command.
GATE = gate("envoy_go_checks", "envoy-go-checks", reads=reads(
    (f"{ENVOY}/**", "every step runs a harness under packages/envoy (scripts/, loadtest/ and deploy/scripts/), "
                    "which runs, sources and reads the scripts, fixtures and compose files beside it"),
    (f"{ENVOY}/loadtest/run-step.test.sh", at("envoy-go-checks", LOADTEST_RUN_STEP)),
    (f"{ENVOY}/loadtest/run-step.sh", "packages/envoy/loadtest/run-step.test.sh runs it"),
    (f"{ENVOY}/loadtest/run-series.sh", "packages/envoy/loadtest/run-step.test.sh runs it"),
    (f"{ENVOY}/loadtest/common.test.sh", at("envoy-go-checks", LOADTEST_LOCK)),
    (f"{ENVOY}/loadtest/common.sh", "packages/envoy/loadtest/common.test.sh sources it; run-step.sh and run-series.sh source it"),
    # The chart archive render-nats.sh renders, vendored so that the step downloads no chart.
    (f"{ENVOY}/loadtest/cluster/nats-*.tgz",
     f"{at('envoy-go-checks', LOADTEST_CLUSTER)}: cluster.test.sh runs render-nats.sh, which checks "
     "the vendored archive's sha256 and renders it"),
    # The helm the cluster suite's render runs, installed by its own step from the helm row.
    (".github/actions/install-helm/**", at("envoy-go-checks", "./.github/actions/install-helm")),
    (".github/ci-tools.yaml", ".github/actions/install-helm/action.yml:30-31 installs the tarball of the helm row"),
    (".github/scripts/ci-tools.sh", ".github/actions/install-helm/action.yml:21 sources it, and ci_tools_fetch "
                                    "reads that row's two sources"),
    # Every script harness step queues its harness through the pool, and the wait step is the
    # verdict on them, so a change to the pool must start this job.
    (".github/scripts/harness-pool.sh",
     f"{at('envoy-go-checks', HARNESS_POOL_WAIT_STEP)}, and each harness step above it queues its harness through it"),
) + setup_go("envoy-go-checks", f"{ENVOY}/go.mod") + bun_job("envoy-go-checks"))
