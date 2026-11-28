# A second gate on tool_fallback: the tool-fallback-arm job proves the same two halves of
# the Bun install on an arm64 runner, where no other job of this repository drives the second
# half, the install from npm, and then the two halves of every other arm64 row: crane,
# changie, mise, jj and elan, each through the same action the x86_64 job drives. It drives
# the same actions over the same manifest through the same helper, so one filter selects both
# jobs, and check-ci-filters.sh keys a claim by (job, step), so the two gates claim different
# steps and do not collide.
# It runs no go command either, and reads less than the job above: the actions, the version
# Bun's pins, the manifest rows the actions and the rig both read, the helper that reads them,
# the Dockerfile install-jj reads the jj tool spec from, and the rig and the fault proxy that
# put the release hosts out of reach.
GATE = gate("tool_fallback", "tool-fallback-arm", reads=reads(
    (".github/actions/setup-bun/**", at("tool-fallback-arm", "./.github/actions/setup-bun")),
    (".github/actions/install-crane/**", at("tool-fallback-arm", "./.github/actions/install-crane")),
    (".github/actions/install-changie/**", at("tool-fallback-arm", "./.github/actions/install-changie")),
    (".github/actions/install-jj/**", at("tool-fallback-arm", "./.github/actions/install-jj")),
    (".github/actions/install-elan/**", at("tool-fallback-arm", "./.github/actions/install-elan")),
    (f"{DAEMON}/docker/worker.Dockerfile", f"{at('tool-fallback-arm', './.github/actions/install-jj')}: the "
                                           "action reads ARG JJ_TOOL, the tool spec mise installs, and the jj "
                                           "row the jj-aarch64 row carries the version of is pinned to it by "
                                           ".github/scripts/check-ci-tools.sh"),
    (".bun-version", f"{at('tool-fallback-arm', TOOL_ARM_READ_STEP)}: the rig holds what bun answers to the "
                     "version that file pins, and .github/actions/setup-bun installs the Bun it pins"),
    (".github/ci-tools.yaml", f"{at('tool-fallback-arm', TOOL_ARM_READ_STEP)}: the rig reads the "
                              "bun-linux-aarch64 row's version and digest, as the action reads that row to "
                              "verify what it installed, and "
                              f"{at('tool-fallback-arm', TOOL_ARM_ROWS_STEP)} reads the version of each other "
                              "-aarch64 row"),
    (".github/scripts/ci-tools.sh", f"{at('tool-fallback-arm', TOOL_ARM_READ_STEP)}: the rig reads that row "
                                    "through it, which is the PyYAML read this job proves the arm64 runner can "
                                    "make"),
    (".github/scripts/ci-tools-proof.sh", f"{at('tool-fallback-arm', TOOL_ARM_READ_STEP)} runs it, as the fault "
                                          "and probe steps of this job do"),
    (".github/scripts/ci-fault-proxy.py", f"{at('tool-fallback-arm', TOOL_FAULT_STEP)}: the rig starts it in "
                                          "front of GitHub's release assets"),
    (".github/scripts/disk_watch.py", f"{at('tool-fallback-arm', TOOL_ARM_WATCH_STEP)} runs it, and "
                                      f"{at('tool-fallback-arm', TOOL_REPORT_STEP)} runs it again"),
))
