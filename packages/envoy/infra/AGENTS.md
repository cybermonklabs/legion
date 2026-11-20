# envoy-infra

The Go module `github.com/sjawhar/envoy/infra`: the Pulumi plugin `pulumi-resource-envoy-infra`, whose components deploy Envoy, and the `envoy-infra` command (`seed` writes a NATS user's nkey seed into a secret store, `render` prints what the AWS components register).
Operators read `README.md`; `docs/nats-hub.md` says how the NATS hubs run.

## Workspace

- The module is not in the root `go.work`. Run every go command from `packages/envoy/infra`, whose own `go.work` binds this module and `packages/envoy`; `./...` from `packages/envoy` does not reach it.
- `go.mod` replaces `github.com/sjawhar/envoy` with `../`, so the plugin compiles the Envoy packages it imports from this checkout.
- Change dependencies in this module's `go.mod` and `go.sum`. Do not add the module to the root `go.work`: it would move the versions the Envoy binaries and the Legion daemon build with to whatever Pulumi needs.

## Commands (from `packages/envoy/infra`)

- Format and vet: `gofmt -l .` and `go vet -tags pulumilive,natslive ./...`.
- Unit tests: `go test ./...`, with no Pulumi CLI and no Docker. One test: `go test -count=1 -run '<name>' ./gcp/`.
- Tests on a real nats-server (Docker): `go test -count=1 -timeout 30m -tags natslive ./core/ ./seed/ ./gcp/`. The `gcp` ones first `go install` helm and the NATS tools, minutes on a cold cache.
- Pulumi live tests: `go test -count=1 -tags pulumilive -v -timeout 15m ./host/`. They need the `pulumi` CLI, `ENVOY_SMOKE_HELD_PORT_IMAGE` and `ENVOY_INFRA_DOCKER_PLUGIN`; without them a test fails and prints the commands that build both. Keep `-count=1`: the test cache cannot see a rebuilt image behind a fixed tag.
- CI: `.github/workflows/ci-job-envoy-infra-go.yaml`; the `envoy-smoke` job runs the Pulumi live tests.
- `gcp` unit tests run the rendered shell scripts under `/bin/sh` (`ENVOY_TEST_SHELL=bash` for bash) and need `curl`, `jq`, `openssl`, `blkid`, `mkfs.ext4` and GNU `cmp` on the machine. Without one they fail; they do not skip.

## Release contract

- A plugin release is cut when a file that `go list -deps ./cmd/...` reaches changes, Envoy packages included. A new import of an Envoy package makes every edit to it cut a release, so `core` copies small listener rules rather than import them. `README.md`, this file and `docs/` cut none.
- `downloadURL` in `provider/provider.go` and the archive names `release-envoy-infra.yaml` publishes must agree: changing either breaks every install.

## Changes with a wide reach

- A `remote:Command` compares the watchdog's create and delete texts as strings, so a change reruns create on every host. Any change to the hub's cloud-config text replaces every VM hub on its next update. Change these only with their goldens in `testdata/`.
- A change to a permission set in `core/permissions.go` changes every hub's rendered authorization: it replaces every VM hub and rewrites every clustered hub's `auth.conf`.
- A NATS subject Envoy sends gets an exported name in `packages/envoy/internal/bus`, and the permission sets are built from those names, so a client and its grant share one spelling.

## Writing a component

- Resolve the inputs in a pure function first (`host.resolveListener`, `gcp.newSettings`), then call `providers.Require` for each provider package the component creates resources with, and only then register anything. No child falls back to a default provider: `gcp:Dispatch` and `gcp:ClusteredNatsHub` build their own Kubernetes provider.
- A field of a plain Go type takes only a value the consumer's program knows when it runs. A value from another resource needs a `pulumi.*Input` field and a Construct test (`host/construct_test.go`, `gcp/construct_test.go`).
- Every refusal is a `core.InputError` naming the input. A value holding an nkey seed is refused where it enters (`bus.HoldsSeed`, `core.SeedRefusal`), naming its path, never its value; a new plain string input of `gcp:NatsHub` or `gcp:Dispatch` joins `seededInput` or `dispatchSeededInput` in `gcp/`, with a test row.
- Every input holding a secret carries `provider:"secret"`.
- An object input type needs an `Annotate` that sets its token (`a.SetToken("host", "Watchdog")`), or consumers see `WatchdogArgsArgs`.
- Set a component's output fields after `RegisterComponentResource`, which replaces them.
- A new `seed` store implements `seed.Store`, joins the `stores` map in `cmd/envoy-infra/main.go` and runs `testStoreContract`. A change to the calls a mode makes changes "The calls each mode makes" in `README.md` in the same commit, since those calls are the grant an operator gives.
