# The tool-fallback job: the live proof that every tool install reads the second source
# .github/ci-tools.yaml names when the first one does not serve the bytes. It runs no go
# command, so the gate is its reads alone: the manifest, the fetch helper, the filler and
# the gate beside it, the rig and the fault proxy it drives, the nine actions it installs
# through, and the two files a row's version is pinned in that a step of it reads.
GATE = gate("tool_fallback", "tool-fallback", reads=reads(
    (".github/ci-tools.yaml", f"{at('tool-fallback', TOOL_BLOB_STEP)} and {at('tool-fallback', TOOL_BLOB_ARM_STEP)} "
                              "read the digest of every ghcr-mirror row, each the rows of one machine, "
                              f"and {at('tool-fallback', TOOL_ROWS_STEP)} reads every row"),
    (".github/scripts/ci-tools.sh", f"{at('tool-fallback', TOOL_STAGE_STEP)}: the rig fetches each row's correct "
                                    "bytes through it, and the three steps above source it for a row's fields"),
    (".github/scripts/ci-tools-proof.sh", f"{at('tool-fallback', TOOL_STAGE_STEP)} runs it, as every other step "
                                          "of this job that drives the proof does; tool-fallback-arm drives it "
                                          "too, and the test job runs its ladder-report verb over logs of its "
                                          "own"),
    (".github/scripts/ci-fault-proxy.py", f"{at('tool-fallback', TOOL_FAULT_STEP)}: the rig starts it in front "
                                          "of GitHub's release assets"),
    *ci_retry("`.github/actions/install-jj/action.yml#Install the worker image's jj` step sources it and retries "
              "`mise install` through it, in each of this job's install-jj steps"),
    (".github/scripts/mirror-ci-tools.sh", f"{at('tool-fallback', TOOL_BLOB_STEP)} and "
                                           f"{at('tool-fallback', TOOL_BLOB_ARM_STEP)} refuse a row that script has "
                                           "not filled, and state the rule that is the opposite of its own"),
    (".github/scripts/check-ci-tools.sh", f"{at('tool-fallback', TOOL_ROWS_STEP)} prints the one literal both "
                                          "paths are held to, which that gate holds every row and every call "
                                          "site of the tree to"),
    (".github/scripts/tool-fallback-package-blobs.sh",
     f"{at('tool-fallback', TOOL_BLOB_STEP)} is that script, which asks the registry for the "
     "blob of every ghcr-mirror row"),
    (".github/scripts/tool-fallback-row-call-sites.sh",
     f"{at('tool-fallback', TOOL_ROWS_STEP)} is that script, which prints each row beside the "
     "manifest line and the call sites that read its digest"),
    (".github/scripts/tool-fallback-nowhere-controls.sh",
     f"{at('tool-fallback', TOOL_NOWHERE_STEP)} is that script, which reads the outcome and "
     "the window of each install with nowhere to go"),
    (".github/scripts/tool-fallback-installed-versions.sh",
     f"{at('tool-fallback', TOOL_VERSIONS_STEP)} is that script, which holds every tool the "
     "faulted installs left to the version its row pins"),
    (".github/scripts/tool-fallback-final-state.sh",
     f"{at('tool-fallback', TOOL_FINAL_STEP)} is that script, which reads what this runner "
     "holds after the eighteen refusals"),
    (".github/actions/install-crane/**", at("tool-fallback", "./.github/actions/install-crane")),
    (".github/actions/install-changie/**", at("tool-fallback", "./.github/actions/install-changie")),
    (".github/actions/install-jj/**", at("tool-fallback", "./.github/actions/install-jj")),
    (".github/actions/setup-bun/**", at("tool-fallback", "./.github/actions/setup-bun")),
    (".github/actions/install-elan/**", at("tool-fallback", "./.github/actions/install-elan")),
    (".github/actions/install-uv/**", at("tool-fallback", "./.github/actions/install-uv")),
    (".github/actions/install-pulumi/**", at("tool-fallback", "./.github/actions/install-pulumi")),
    (".github/actions/install-docker-plugin/**",
     at("tool-fallback", "./.github/actions/install-docker-plugin")),
    (".github/actions/install-helm/**", at("tool-fallback", "./.github/actions/install-helm")),
    (f"{DAEMON}/docker/worker.Dockerfile", f"{at('tool-fallback', TOOL_JJ_SEED_STEP)}: the rig reads ARG JJ_TOOL "
                                           "to name mise's own install directory for that tool, and "
                                           ".github/actions/install-jj reads that same ARG, while the mise row "
                                           "that action installs mise from is pinned to this file's "
                                           "ARG MISE_VERSION and ARG MISE_SHA256 by .github/scripts/check-ci-tools.sh"),
    (".bun-version", f"{at('tool-fallback', TOOL_BUN_BYTE_STEP)} reads the pinned version out of it, and "
                     ".github/actions/setup-bun installs the Bun it pins"),
    (".github/scripts/disk_watch.py", f"{at('tool-fallback', TOOL_WATCH_STEP)} runs it, and "
                                      f"{at('tool-fallback', TOOL_REPORT_STEP)} runs it again"),
))
