# envoy's Go build, vet, format, generated-code and migration checks, the half of the old
# envoy-go-checks job a pull request runs
# (.github/ci-notes/ci.md#envoy-go-build-envoys-go-build-vet-format-generated-code-and-migration-checks
# says what it holds), with the tests of the
# architecture-note and spec-file rules and their commands, the note check over this
# repository's notes through the composite action, the spec-file check over its spec
# directory through the other one, and the guard that no CI file compares the spec files with
# the approved spec. Its gofmt step reads the whole module, so no closure stands for it.
GATE = gate("envoy_go_build", "envoy-go-build", runs=[
    step_run("envoy-go-build", "Build envoy services", "go build ./cmd/dispatch ./cmd/listener", ENVOY,
             ["./cmd/dispatch", "./cmd/listener"], mode="build"),
    # The architecture-note and spec-file rules and their commands: the closure of the five
    # packages, tests, testdata and all, built from the list the unit step's `grep -Ev` leaves
    # them out by.
    step_run("envoy-go-build", NOTE_AND_SPEC_STEP, NOTE_AND_SPEC_TEXT, ENVOY, NOTE_AND_SPEC_PKGS),
    # LISTENERWIRE_PACKAGES' tests (listenerwire's, its clients test's type check of the daemon's
    # listener clients among them, and internal/wirecheck's), built from the list the unit
    # step's `grep -Ev` leaves them out by.
    step_run("envoy-go-build", LISTENERWIRE_STEP, LISTENERWIRE_TEXT, ENVOY, LISTENERWIRE_PKGS),
], exact=[
    # `go run` is not one of the three forms the reducer reads, so the step is pinned whole
    # and this Run stands for the command it compiles.
    pinned_step("envoy-go-build", "Check the OpenAPI document is generated from the route table", ENVOY,
                "go run ./cmd/dispatch openapi --check", [
                    (ENVOY, ["./cmd/dispatch"],
                     "go run ./cmd/dispatch openapi --check, which emits the document from the "
                     "route table and compares it with the committed one", {"mode": "build"}),
                ]),
    # The guard that no CI file runs the comparison of the spec files with the approved spec:
    # `go run` again, so pinned whole, and the Run stands for the command it builds.
    pinned_step("envoy-go-build", "Check that no CI file of this repository compares the spec files", ENVOY,
                "go run ./cmd/spec-check guard --root ../..", [
                    (ENVOY, ["./cmd/spec-check"],
                     "go run ./cmd/spec-check guard, which builds the command and runs it over the "
                     "repository root", {"mode": "build"}),
                ]),
    pinned_step("envoy-go-build", EDITED_MIGRATIONS_STEP, ENVOY, EDITED_MIGRATIONS_SCRIPT, [
        (ENVOY, ["./internal/pgmigrate/cmd/edited-migrations"],
         "go build of ./internal/pgmigrate/cmd/edited-migrations, whose binary the step then runs to "
         "compare each store's migrations with the base's", {"mode": "build"}),
    ]),
    # One linux build of the listener per architecture the loop reads from the target list.
    pinned_step("envoy-go-build", PACK_STEP, ENVOY, PACK_SCRIPT, [
        (ENVOY, ["./cmd/listener"], f"GOOS=linux GOARCH={arch} go build ./cmd/listener",
         {"mode": "build", "env": {"GOOS": "linux", "GOARCH": arch}})
        for arch in targets(GO_TARGETS, "envoy")
    ]),
    # The display-tree fixture check: one test of pmdoc, which compiles that package.
    pinned_step("envoy-go-build", FIXTURE_STEP, ENVOY, FIXTURE_SCRIPT, [
        (ENVOY, ["./internal/dispatch/pmdoc/"], "go test ./internal/dispatch/pmdoc/", {}),
    ]),
    # The table tests of pmdoc in a build without inlining: the same package, compiled again
    # with -gcflags=all=-l.
    pinned_step("envoy-go-build", NO_INLINE_TABLE_STEP, ENVOY, NO_INLINE_TABLE_SCRIPT, [
        (ENVOY, ["./internal/dispatch/pmdoc/"], "go test -gcflags=all=-l ./internal/dispatch/pmdoc/", {}),
    ]),
    darwin_vet("envoy-go-build", ENVOY_DARWIN_VET, ENVOY),
], reads=reads(
    (f"{ENVOY}/**", f"{at('envoy-go-build', 'Check Go formatting')}: gofmt -l . in packages/envoy reads every .go file under it, infra included"),
    ("packages/contracts/scripts/gen-go.ts", f"{at('envoy-go-build', 'Check Go contracts are generated from the shared schema')} (packages/contracts/package.json check:go)"),
    ("packages/contracts/scripts/gen-go-tools.ts", "`packages/contracts/scripts/gen-go.ts#renderDispatchTools` imports renderDispatchTools, whose output --check compares"),
    ("packages/contracts/scripts/gen-go-dispatch-frame.ts", "`packages/contracts/scripts/gen-go.ts#renderDispatchFrameSchema`, `packages/contracts/scripts/gen-go.ts#renderDispatchFrameSchemaGo` and `packages/contracts/scripts/gen-go.ts#renderDispatchFrameVerdicts` import the Dispatch frame renderers, whose outputs --check compares"),
    ("packages/contracts/scripts/dispatch-frame-corpus.ts", "`packages/contracts/scripts/gen-go-dispatch-frame.ts#dispatchFrameCorpus` imports dispatchFrameCorpus, the payloads whose verdicts --check compares with fixtures/dispatch-frame-verdicts.json"),
    (f"{FIX}/dispatch-frame-verdicts.json", "`packages/contracts/scripts/gen-go.ts#outputs` holds it, rendered by `packages/contracts/scripts/gen-go.ts#renderDispatchFrameVerdicts`: an output --check reads to compare with its render"),
    ("packages/contracts/schemas/**", "packages/contracts/scripts/gen-go.ts's envelopeFile and blockSchemaFile read schemas/, and --check reads schemas/dispatch-frame.schema.json to compare"),
    # The pinned step above compares the emitter's bytes with this file, so an edit of it alone
    # must start this job: the Run stands for what the command compiles, never for what it reads.
    ("packages/contracts/schemas/dispatch-openapi.json",
     f"{at('envoy-go-build', 'Check the OpenAPI document is generated from the route table')}: "
     "`packages/envoy/cmd/dispatch/subcommands.go#writeOpenAPIDocument` and `packages/envoy/cmd/dispatch/subcommands.go#committedOpenAPIPath`: the first reads the "
     "committed document to compare it, and the second only builds a candidate path for it"),
    (CONTRACTS_SRC, "packages/contracts/scripts/gen-go.ts's imports of ../src/*"),
    (f"{ENVOY}/internal/dispatch/pmdoc/gen/**", f"{at('envoy-go-build', 'Check pmdoc fixtures are up to date')} (gen.ts --check)"),
    ("packages/proof-editor/src/**", "`packages/envoy/internal/dispatch/pmdoc/gen/gen.ts#setBlockIdGenerator`, `packages/envoy/internal/dispatch/pmdoc/gen/gen.ts#createHeadlessProof` and `packages/envoy/internal/dispatch/pmdoc/gen/gen.ts#readDocument` imports @legion/proof-editor (exports -> src/lib.ts, src/lib-headless.ts, src/document-reading.ts)"),
    # The web app's display-tree fixture, whose content the fixture step's --check compares the
    # trees it builds with, and whose existence the other test of WEB_FIXTURE_TESTS,
    # `packages/envoy/internal/dispatch/pmdoc/web_fixtures_test.go#TestTheWebFixtureFindsItsFilesFromAnyDirectory`, stats in
    # that same step. Declared on this step and not as a //ci:reads line in
    # web_fixtures_test.go: a line in that file would put the fixture into every job that
    # compiles pmdoc's tests, envoy-go's unit step among them, which skips both tests and so
    # touches the file in no way. The fixture is this job's read alone.
    ("packages/dispatch/web/src/features/refs/__fixtures__/display-trees.json",
     f"{at('envoy-go-build', FIXTURE_STEP)}: `packages/envoy/internal/dispatch/pmdoc/web_fixtures_test.go#webFixturePath` "
     "joins it under pmdocSourceDir, and --check reads its content to compare"),
    (NOTICES, f"{at('envoy-go-build', 'Pack envoy binaries')} runs it"),
    (GO_TARGETS, f"{at('envoy-go-build', 'Pack envoy binaries')}: jq -r '.envoy[]'"),
    # Each action builds its command with GOWORK=off, so it reads packages/envoy's go.mod and
    # go.sum and the command's packages, all under the packages/envoy/** read; what is outside
    # packages/envoy is the action itself, the build script both actions run, and the files
    # it judges.
    (".github/actions/architecture-check/action.yml",
     f"{at('envoy-go-build', './.github/actions/architecture-check')} runs the action it defines"),
    (".github/actions/architecture-check/check.sh",
     f"{at('envoy-go-build', './.github/actions/architecture-check')}: "
     "`.github/actions/architecture-check/action.yml#Check the Go on PATH is the release packages/envoy/go.mod names`, `.github/actions/architecture-check/action.yml#Build the architecture-check command` and `.github/actions/architecture-check/action.yml#Check the architecture notes` run it"),
    (".dispatch/architecture/**",
     f"{at('envoy-go-build', './.github/actions/architecture-check')} judges the action's default path "
     "(`.github/actions/architecture-check/action.yml#default: .dispatch/architecture`), which `.github/actions/architecture-check/check.sh#run()` "
     "resolves against GITHUB_WORKSPACE and the command it runs reads"),
    (".github/actions/spec-check/action.yml",
     f"{at('envoy-go-build', './.github/actions/spec-check')} runs the action it defines"),
    (".github/actions/spec-check/check.sh",
     f"{at('envoy-go-build', './.github/actions/spec-check')}: "
     "`.github/actions/spec-check/action.yml#Build the spec-check command` and `.github/actions/spec-check/action.yml#Check the spec files` run it"),
    (".github/scripts/build-envoy-command.sh",
     f"{at('envoy-go-build', './.github/actions/architecture-check')} and "
     f"{at('envoy-go-build', './.github/actions/spec-check')}: "
     "`.github/actions/architecture-check/check.sh#build()` and `.github/actions/spec-check/check.sh#build()` run it"),
    (EDITED_MIGRATIONS_BASE, f"{at('envoy-go-build', EDITED_MIGRATIONS_STEP)} runs it for the base it compares with"),
    (".github/scripts/fetch-depth-if-shallow.sh",
     f"{at('envoy-go-build', EDITED_MIGRATIONS_STEP)}: edited-migrations-base.sh fetches the base through it"),
    # The step judges the action's default path, .dispatch/spec, which holds no file on main;
    # the read names .dispatch whole because a read that matches no file is refused, and the
    # envoy_go_build filter names .dispatch whole too.
    (".dispatch/**",
     f"{at('envoy-go-build', './.github/actions/spec-check')} judges the action's default path "
     "(`.github/actions/spec-check/action.yml#default: .dispatch/spec`), which `.github/actions/spec-check/check.sh#run()` "
     "resolves against GITHUB_WORKSPACE and the command it runs reads"),
    *[(glob, f"{at('envoy-go-build', 'Check that no CI file of this repository compares the spec files')}: "
             "spec-check guard reads every file under it")
      for glob in (".github/workflows/**", ".github/actions/**", ".github/scripts/**", "scripts/**")],
) + setup_go("envoy-go-build", f"{ENVOY}/go.mod") + bun_job("envoy-go-build")
  + darwin_vet_reads("envoy-go-build", ENVOY_DARWIN_VET, ENVOY))
