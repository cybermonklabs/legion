# envoy-infra

The Go module `github.com/sjawhar/envoy/infra`:
the Pulumi plugin `pulumi-resource-envoy-infra`, whose components deploy Envoy, and the `envoy-infra` command (`seed` writes a NATS user's nkey seed into a secret store, `render` prints what the AWS components register).
Operators read `README.md`, which also has the commands and the tests; `docs/nats-hub.md` says how the NATS hubs run.

## Rules

- The module is outside the root `go.work`: work on it from `packages/envoy/infra`, whose own `go.work` binds it and `packages/envoy`.

- Change dependencies in this module's `go.mod` and `go.sum`.
  Do not add the module to the root `go.work`: it would move the versions the Envoy binaries and the Legion daemon build with to whatever Pulumi needs.

- Each Envoy package `core` imports makes every edit to that package cut a plugin release, so `core` copies small listener rules rather than import them.

- `downloadURL` in `provider/provider.go` and the archive names `release-envoy-infra.yaml` publishes must agree: changing either breaks every install.

- Change the watchdog's create and delete texts, the hub's cloud-config text and the permission sets in `core/permissions.go` only with their goldens in `testdata/`:
  a change reruns create on every host, or replaces every VM hub and rewrites every clustered hub's `auth.conf`.

- A NATS subject Envoy sends gets an exported name in `packages/envoy/internal/bus`, and the permission sets are built from those names, so a client and its grant share one spelling.

## Writing a component

- Resolve the inputs in a pure function first (`host.resolveListener`, `gcp.newSettings`), then call `providers.Require` for each provider package the component creates resources with, and only then register anything.
  No child falls back to a default provider.

- A field of a plain Go type takes only a value the consumer's program knows when it runs.
  A value from another resource needs a `pulumi.*Input` field and a Construct test.

- Every refusal is a `core.InputError` naming the input.
  A value holding an nkey seed is refused where it enters (`bus.HoldsSeed`, `core.SeedRefusal`), naming its path, never its value;
  a new plain string input of `gcp:NatsHub` or `gcp:Dispatch` joins `seededInput` or `dispatchSeededInput` in `gcp/`, with a test row.

- Every input holding a secret carries `provider:"secret"`.

- An object input type needs an `Annotate` that sets its token, or consumers see `WatchdogArgsArgs`.

- Set a component's output fields after `RegisterComponentResource`, which replaces them.

- The calls a `seed` store mode makes are the grant an operator gives: a change to them changes "The calls each mode makes" in `README.md` in the same commit.
