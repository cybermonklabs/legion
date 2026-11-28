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
    (".github/ci-tools.yaml", "`.github/actions/install-pulumi/action.yml#ci_tools_fetch pulumi-cli` installs the pulumi-cli row's "
                              "tarball, and `.github/actions/install-docker-plugin/action.yml#ci_tools_fetch pulumi-docker` downloads the "
                              "pulumi-docker row's"),
    (".github/scripts/ci-tools.sh", "`.github/actions/install-pulumi/action.yml#$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh` and "
                                    "`.github/actions/install-docker-plugin/action.yml#$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh` source it, and "
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
    (".bun-version", "`packages/envoy/docker/Dockerfile#COPY .bun-version .bun-version`, and the RUN right after it holds the web stage's own Bun to it"),
    ("package.json", "`packages/envoy/docker/Dockerfile#COPY package.json bun.lock ./`"),
    ("bun.lock", "`packages/envoy/docker/Dockerfile#COPY package.json bun.lock ./`"),
    ("patches/**", "`packages/envoy/docker/Dockerfile#COPY patches patches`"),
    ("packages/cipher-client/package.json", "`packages/envoy/docker/Dockerfile#COPY packages/cipher-client/package.json`"),
    ("packages/envoy-client/package.json", "`packages/envoy/docker/Dockerfile#COPY packages/envoy-client/package.json`"),
    ("packages/pi-envoy/package.json", "`packages/envoy/docker/Dockerfile#COPY packages/pi-envoy/package.json`"),
    ("packages/pi-legion/package.json", "`packages/envoy/docker/Dockerfile#COPY packages/pi-legion/package.json`"),
    ("packages/pi-shared/package.json", "`packages/envoy/docker/Dockerfile#COPY packages/pi-shared/package.json`"),
    ("packages/claude-envoy/package.json", "`packages/envoy/docker/Dockerfile#COPY packages/claude-envoy/package.json`"),
    ("packages/test-fixtures/package.json", "`packages/envoy/docker/Dockerfile#COPY packages/test-fixtures/package.json`"),
    ("docs/site/package.json", "`packages/envoy/docker/Dockerfile#COPY docs/site/package.json` and "
                               '`packages/envoy/docker/Dockerfile#retry 600 "bun install"`, '
                               "the web stage's RUN whose bun install --frozen-lockfile reads every manifest above"),
    ("packages/contracts/**", "`packages/envoy/docker/Dockerfile#COPY packages/contracts/package.json`, `packages/envoy/docker/Dockerfile#COPY packages/contracts packages/contracts` and `packages/envoy/docker/Dockerfile#RUN bun run gen:go` copies the manifest and then the package, which it runs gen:go in"),
    ("packages/dispatch/**", "`packages/envoy/docker/Dockerfile#COPY packages/dispatch/package.json`, `packages/envoy/docker/Dockerfile#COPY packages/dispatch packages/dispatch` and `packages/envoy/docker/Dockerfile#RUN bun run build:web` copies the manifest and then the package, which it runs build:web in"),
    ("packages/proof-editor/**", "`packages/envoy/docker/Dockerfile#COPY packages/proof-editor/package.json`, `packages/envoy/docker/Dockerfile#COPY packages/proof-editor/proof-sdk-*.tgz` and `packages/envoy/docker/Dockerfile#COPY packages/proof-editor packages/proof-editor` copies the manifest, the upstream tarball and then the package, which the SPA build aliases"),
    ("scripts/third-party-notices.ts", "`packages/envoy/docker/Dockerfile#COPY scripts/third-party-notices.ts`, which the SPA build writes its notices with"),
    ("scripts/go-third-party-notices.sh", "`packages/envoy/docker/Dockerfile#COPY scripts/go-third-party-notices.sh`, `packages/envoy/docker/Dockerfile#go-third-party-notices.sh --install` and `packages/envoy/docker/Dockerfile#go-third-party-notices.sh /tmp/go-notices` copies it and runs it twice"),
    ("scripts/assemble-third-party-notices.sh", "`packages/envoy/docker/Dockerfile#scripts/assemble-third-party-notices.sh /usr/local/bin/` and `packages/envoy/docker/Dockerfile#assemble-third-party-notices.sh /out/THIRD_PARTY_NOTICES` copies it and runs it"),
    # The image's Go stage copies the Envoy module whole
    # (`packages/envoy/docker/Dockerfile#COPY packages/envoy ./`), and the held-port image's build
    # copies it whole with the envoy-infra module under it
    # (`packages/envoy/infra/host/heldportwrapper/Dockerfile#COPY packages/envoy /src/packages/envoy`).
    # Each reads every file of it but what its
    # .dockerignore drops: test files, internal/integration, internal/smoke (the smoke step
    # compiles that one), deploy and the changelog fragments, and infra for the first. No go
    # command of this gate reaches most of those files, so rule 9 of check-ci-filters.sh reads
    # the filter's module pattern as matching unread files without these rows, while
    # check-image-trigger-paths.sh refuses the filter without it: the read is the image
    # builds', and this is where the two checks meet. listenerwire's envelope_generated.go is
    # among them, which the Go stage then replaces with the web stage's fresh generation
    # (`packages/envoy/docker/Dockerfile#COPY --from=web /repo/packages/envoy/listenerwire`).
    *[(path, "`packages/envoy/docker/Dockerfile#COPY packages/envoy ./`, and "
             "`packages/envoy/infra/host/heldportwrapper/Dockerfile#COPY packages/envoy /src/packages/envoy`")
      for path in files_but_tests(ENVOY, suffix="", tree=True, skip=(".changes",))
      if not path.startswith(tuple(f"{ENVOY}/{d}/" for d in ("internal/integration", "internal/smoke", "deploy")))],
    *ci_retry(f"{at('envoy-smoke', GO_DOWNLOAD_STEP)} sources it: the step downloads both of this job's modules through it"),
) + setup_go("envoy-smoke", f"{ENVOY}/go.mod"))
