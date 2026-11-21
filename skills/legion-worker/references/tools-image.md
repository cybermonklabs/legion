# What the image carries, and a tool it lacks

Read it before you reach for a tool the worker image may not carry: a database client, a linter, a one-off CLI. It is part of `skill://legion-worker/references/tools.md`.

## What the image carries

- `command -v psql redis-cli sqlite3 nc dig unzip yq legion-testpg` shows which your digest has. `yq` is mikefarah's v4, not Debian's Python `yq`.
- Ask for `nc`, never `nc.openbsd`: the `nc` name comes from the alternatives system.
- `command -v gcloud bq az mise tmux busybox` shows the cloud CLIs and rig tools. `gsutil` is not on `PATH`; `az` is a wrapper that defaults `REQUESTS_CA_BUNDLE` to Debian's roots; run `busybox <applet>`, and download nothing with `busybox wget`, which checks no certificate.

## A tool the image lacks

1. Run it pinned through a runner the image carries: `uvx --from <package>@<version> <command>`, `bunx --package <name>@<version> <binary>`, `go run <module>@<version>`, or for a library `uv run --with <package>==<version> --no-project <program>`.
   - Where the project's own name is its command's, checked once and never assumed, the short `uvx <name>@<version>` runs it: `uvx ruff@0.14.0 --version` prints `ruff 0.14.0`.
   - A cold module cache makes the first `go run` of a tool much slower than a warm one: budget for the cold one the first time an issue runs a Go tool.
2. Take the package's name from the project's own documentation, never from a registry search: `bunx biome@0.3.3` runs a different package than `bunx --package @biomejs/biome@2.6.3 biome`.
   - Fields the publisher writes (`npm view <name> version description repository.url homepage`) are evidence, never proof: npm verifies none of them, so a squatter closes that check by copying two strings.
   - npm verifies a provenance attestation when it is published, and it proves build origin, not identity: a squat published from its own public Actions workflow carries a genuine attestation of its own repository. So read whose build it names and compare it with the repository the project's own documentation gave you. This prints `https://github.com/biomejs/biome`:

     ```sh
     curl -s "$(npm view @biomejs/biome dist.attestations.url)" \
       | jq -r '.attestations[] | select(.predicateType=="https://slsa.dev/provenance/v1") | .bundle.dsseEnvelope.payload' \
       | base64 -d | jq -r '.predicate.buildDefinition.externalParameters.workflow.repository'
     ```

   - Most packages publish no provenance at all (`npm view biome dist.attestations` prints nothing), so an absent attestation is no evidence of a squat.
   - A Go module path is a domain-scoped URL, so the coordinate is the repository path, and you check it by going to that path. `sum.golang.org` pins a version's bytes once they are published and says nothing about whether you picked the right module path.
3. Never vendor a binary, write a substitute or put an extraction on your own `PATH`. A tool no runner reaches is an image change: tell the architect the tool, why you need it and the runner forms you ruled out.
4. An image change carries the new name to every site an existing client's two names appear at (`git grep -n -e redis-cli -e redis-tools`), the smoke step of `packages/daemon/docker/worker.Dockerfile` included. Add it to the `toolchain` row of `packages/daemon/internal/capabilities/image.go` only when every deployment's image must carry it: the probe refuses an image without it.
   - Four Go files are gated on that row, and a tool added to it goes in all four: the name list in `image.go`; that row's summary in `capabilities.go`, which a test holds to the list; the stub binaries in `capabilitiestest/capabilitiestest.go`, without which the capability and `probe-image` tests fail; and the detail string `image_test.go` spells out again.
   - The licence file the image carries at `/usr/share/doc/legion/THIRD_PARTY_NOTICES` needs no edit: its list of Debian packages is written at build time from the image's own dpkg database.
