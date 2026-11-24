# The job ci.yaml gates is docs, and the steps it runs are docs-site.yaml's (at_docs), under
# a job id of its own, `build`. So this gate keeps `workflow=CI`, the file that holds a job
# named docs, where every other gate takes the called file job_file() names.
GATE = gate("docs", "docs", runs=[
    script_run(BUILD_BINARIES, 'go -C "$root/$legion_module" build -o "$bin/legion" ./cmd/legion',
               f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", DAEMON, ["./cmd/legion"], mode="build"),
    script_run(BUILD_BINARIES, 'go -C "$root/$envoy_module" build -o "$bin/envoy-dispatch" ./cmd/dispatch',
               f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", ENVOY, ["./cmd/dispatch"], mode="build"),
    script_run(BUILD_BINARIES, 'go -C "$root/$envoy_module" build -o "$bin/envoy-broker" ./cmd/cipherd',
               f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", ENVOY, ["./cmd/cipherd"], mode="build"),
    *[script_run(BUILD_BINARIES, f'go -C "$root/$envoy_module" build -o "$bin/{binary}" {package}',
                 f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", ENVOY, [package], mode="build")
      for binary, package in (("agent-secrets", "./cmd/cipher"), ("agent-secrets-helper", "./cmd/cipher-helper"),
                              ("broker-refgen", "./cmd/cipher-refgen"))],
    script_run(RUN_SERVER, DISPATCH_SERVER_BUILD, f"started by packages/dispatch/e2e/harness-listeners.ts's dispatchServer (harness-start.ts, which "
               f"docs/site/media/harness.ts:18-21,100 imports), run by {at_docs('Take the screenshots')}",
               ENVOY, ["./cmd/dispatch"], mode="build"),
], reads=reads(
    ("docs/site/**", f"{at_docs('Typecheck the docs site')}, screenshots (docs/site/media/shots.ts) and build (generate.ts + astro build) in docs/site"),
    ("packages/dispatch/**", f"{at_docs('Build the Dispatch dashboard the screenshots show')}; docs/site/media/harness.ts:18-21,100 boots packages/dispatch/e2e/harness-start.ts"),
    ("packages/proof-editor/src/**", "packages/dispatch/web/vite.config.ts:124-144 aliases (build:web)"),
    (CONTRACTS_SRC, "packages/dispatch/web/vite.config.ts:117-123; docs/site/generators/dispatch-tools.ts:7"),
    # This gate is the one reader of envoy-client's sources that does not compile
    # cipher-client with them: every generator entry resolves to src/dispatch-command.ts,
    # whose program holds no cipher-client source (`bunx tsc --listFilesOnly`, 0 of 7), so
    # the shared envoy_client_dep read would declare a read this job does not make.
    (ENVOY_CLIENT_SRC, "docs/site/generators/dispatch-tools.ts:8-12 imports ../../../packages/envoy-client/src/dispatch-command.ts"),
    ("skills/**", "docs/site/generators/legion-skills.ts:15-19 reads skills/*/SKILL.md"),
    ("deploy/kubernetes/**", "docs/site/generators/legion-config.ts's exampleFiles lists every tracked deploy/kubernetes *.example/*.yml/*.yaml and its renderExamples reads each whole"),
    ("scripts/third-party-notices.ts", "docs/site/astro.config.mjs:10-15; packages/dispatch/web/vite.config.ts:6-12"),
    (".github/scripts/check-private-names.sh", at("private-names", "Check the built site has no private name, internal host, tailnet host, or term of the deployment's own list", DOCS_SITE)),
    (".github/scripts/repo-paths.sh", "docs/site/repo-paths.ts:21,25 runs it for the site's own URL, base and edit links "
                                      f"(astro.config.mjs), read by {at_docs('Build the docs site')}"),
    (f"{ENVOY}/internal/dispatch/api/routes_table.go", "docs/site/generators/dispatch-api.ts's page sources it, checked by generators/lib/markdown.ts's writePage"),
    (f"{ENVOY}/cmd/dispatch/settings.go", "docs/site/generators/dispatch-config.ts's page sources it, checked by generators/lib/markdown.ts's writePage"),
    (f"{DAEMON}/internal/config/keytable.go", "docs/site/generators/legion-config.ts's page sources it, checked by generators/lib/markdown.ts's writePage"),
    (BUILD_BINARIES, f"docs/site/scripts/generate.ts:116 runs it ({at_docs('Build the docs site')})"),
    (RUN_SERVER, f"packages/dispatch/e2e/harness-listeners.ts's dispatchServer runs it ({at_docs('Take the screenshots')})"),
    ("scripts/e2e/lib/held-port.py", "packages/dispatch/e2e/run-server.sh:80,89 holds the server's port through it"),
    (f"{ENVOY}/scripts/lib/pg-url.sh", "packages/dispatch/e2e/run-broker.sh:62 sources it, and packages/dispatch/e2e/harness-listeners.ts:188,197 "
                                       f"start that script in a one-process run on the fake broker, which {at_docs('Take the screenshots')} is"),
    # broker-refgen (docs/site/generators/broker-reference.sh:15) parses these at run time,
    # each directory's non-test .go files alone (packages/envoy/cmd/cipher-refgen/source.go:34-45),
    # and the packages of REFGEN_API past them, whose non-test files the build of
    # ./cmd/cipherd above reads already.
    *[(path, f"broker-refgen parses it (packages/envoy/cmd/cipher-refgen/{where})") for directory, where in (
        ("internal/cipher/api", "routes.go:17,238"),
        ("internal/cipher/config", "config.go:11,36"),
        ("cmd/cipherd", "config.go:12,40"),
        ("internal/cipher/helper", "pages.go:16, codes.go:15"),
        ("cmd/cipher", "pages.go:17, codes.go:68"),
        ("internal/cipher/approval", "routes.go:699 g.in of the pageAnswerers directory named at :34, which parses it at bodies.go:26"),
        ("internal/cipher/proxy", "recorder.go:13,78"),
    ) for path in files_but_tests(f"{ENVOY}/{directory}")],
) + setup_go(DOCS_JOB, f"{DAEMON}/go.mod", DOCS_SITE)
  + bun_job(DOCS_JOB, DOCS_SITE) + CI_SELF + DOCS_SITE_SELF, workflow=CI)
GATE = with_reads(GATE, [
    Read('packages/daemon/internal/daemon/**', "filter docs held packages/daemon/internal/daemon/** by hand before the filters were computed: it selected 7 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter docs held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 277 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/delivery/**', "filter docs held packages/envoy/internal/dispatch/delivery/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
