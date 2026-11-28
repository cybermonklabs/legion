# The listener image this job builds and smokes is built from the repository root, so every
# file packages/envoy/docker/Dockerfile copies into that context is a file this job reads:
# each one below names the COPY line that reads it. Only those the job's go commands do not
# already reach are named, which is what keeps this list short - the module files, the whole
# of packages/envoy/internal and the RDS bundle are reads of the smoke and live tests
# themselves. check-image-trigger-paths.sh holds the filter to the image's inputs from the
# other side, proving the list complete; this gate proves no pattern of the filter is idle.
GATE = gate("envoy_smoke", "envoy-smoke", runs=[
    step_run("envoy-smoke", "Container smoke test (testcontainers)", "go test -tags smoke -v -timeout 5m ./internal/smoke/",
             ENVOY, ["./internal/smoke/"], tags="smoke"),
    step_run("envoy-smoke", "Live tests of envoy-infra's host:Listener", "go test -tags pulumilive -v -timeout 15m ./host/",
             INFRA, ["./host/"], tags="pulumilive"),
], exact=[
    pinned_step("envoy-smoke", SCHEMA_STEP, INFRA, SCHEMA_SCRIPT, [
        (INFRA, ["./cmd/pulumi-resource-envoy-infra"], "go build ./cmd/pulumi-resource-envoy-infra", {"mode": "build"}),
    ]),
], reads=reads(
    (".github/actions/pull-test-images/**", at("envoy-smoke", "Pull test container images")),
    (f"{ENVOY}/internal/testnats/nats.go", f"{at('envoy-smoke', 'Pull test container images')}: const HubImage"),
    (".github/actions/install-pulumi/**", at("envoy-smoke", "./.github/actions/install-pulumi")),
    (".github/actions/install-docker-plugin/**",
     at("envoy-smoke", "Download the docker provider plugin the live tests install")),
    (".github/ci-tools.yaml", ".github/actions/install-pulumi/action.yml:20-21 installs the pulumi-cli row's "
                              "tarball, and .github/actions/install-docker-plugin/action.yml:33-35 downloads the "
                              "pulumi-docker row's"),
    (".github/scripts/ci-tools.sh", ".github/actions/install-pulumi/action.yml:19 and "
                                    ".github/actions/install-docker-plugin/action.yml:30 source it, and "
                                    "ci_tools_fetch reads each row's two sources"),
    (".github/scripts/repo-paths.sh",
     f"{at('envoy-smoke', SCHEMA_STEP)}: it derives the download URL the build stamps in"),
    (".github/scripts/disk_watch.py",
     f"{at('envoy-smoke', 'Start the machine watch over the live tests')} and "
     f"{at('envoy-smoke', 'Report what the live tests ran on')} run it"),
    (f"{ENVOY}/docker/Dockerfile", f"{at('envoy-smoke', SMOKE_IMAGE_STEP)} builds it (its `file:`), and "
                                   f"{at('envoy-smoke', BASE_IMAGES_STEP)} reads its FROM lines"),
    (".dockerignore", f"{at('envoy-smoke', SMOKE_IMAGE_STEP)} builds with `context: .`, so the root .dockerignore "
                      "decides which of the files below reach any COPY of that Dockerfile"),
    (".bun-version", "packages/envoy/docker/Dockerfile:20 COPY .bun-version, which :21-27 holds the web stage's own Bun to"),
    ("package.json", "packages/envoy/docker/Dockerfile:29 COPY package.json bun.lock ./"),
    ("bun.lock", "packages/envoy/docker/Dockerfile:29 COPY package.json bun.lock ./"),
    ("patches/**", "packages/envoy/docker/Dockerfile:30 COPY patches patches"),
    ("packages/cipher-client/package.json", "packages/envoy/docker/Dockerfile:32"),
    ("packages/envoy-client/package.json", "packages/envoy/docker/Dockerfile:33"),
    ("packages/pi-envoy/package.json", "packages/envoy/docker/Dockerfile:34"),
    ("packages/pi-legion/package.json", "packages/envoy/docker/Dockerfile:35"),
    ("packages/pi-shared/package.json", "packages/envoy/docker/Dockerfile:36"),
    ("packages/claude-envoy/package.json", "packages/envoy/docker/Dockerfile:37"),
    ("packages/test-fixtures/package.json", "packages/envoy/docker/Dockerfile:43"),
    ("docs/site/package.json", "packages/envoy/docker/Dockerfile:45; :62 RUN bun install --frozen-lockfile reads every manifest above"),
    ("packages/contracts/**", "packages/envoy/docker/Dockerfile:31 and :68 COPY packages/contracts packages/contracts, which :78 runs gen:go in"),
    ("packages/dispatch/**", "packages/envoy/docker/Dockerfile:38 and :69 COPY packages/dispatch packages/dispatch, which :80 runs build:web in"),
    ("packages/proof-editor/**", "packages/envoy/docker/Dockerfile:39, :42 and :70 COPY packages/proof-editor packages/proof-editor, which the SPA build aliases"),
    ("scripts/third-party-notices.ts", "packages/envoy/docker/Dockerfile:74 COPY scripts/third-party-notices.ts, which the SPA build writes its notices with"),
    ("scripts/go-third-party-notices.sh", "packages/envoy/docker/Dockerfile:97 COPY, run by :98 and :118"),
    ("scripts/assemble-third-party-notices.sh", "packages/envoy/docker/Dockerfile:97 COPY, run by :120"),
    # The image's Go stage copies the Envoy module whole (packages/envoy/docker/Dockerfile:106
    # COPY packages/envoy ./), and the held-port image's build copies it whole with the
    # envoy-infra module under it (packages/envoy/infra/host/heldportwrapper/Dockerfile:30
    # COPY packages/envoy /src/packages/envoy). Each reads every file of it but what its
    # .dockerignore drops: test files, internal/integration, internal/smoke (the smoke step
    # compiles that one), deploy and the changelog fragments, and infra for the first. No go
    # command of this gate reaches most of those files, so rule 9 of check-ci-filters.sh reads
    # the filter's module pattern as matching unread files without these rows, while
    # check-image-trigger-paths.sh refuses the filter without it: the read is the image
    # builds', and this is where the two checks meet. listenerwire's envelope_generated.go is
    # among them, which the Go stage then replaces with the web stage's fresh generation (:110).
    *[(path, "packages/envoy/docker/Dockerfile:106 COPY packages/envoy ./, and "
             "packages/envoy/infra/host/heldportwrapper/Dockerfile:30 COPY packages/envoy /src/packages/envoy")
      for path in files_but_tests(ENVOY, suffix="", tree=True, skip=(".changes",))
      if not path.startswith(tuple(f"{ENVOY}/{d}/" for d in ("internal/integration", "internal/smoke", "deploy")))],
) + setup_go("envoy-smoke", f"{ENVOY}/go.mod"))
