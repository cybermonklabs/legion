# envoy-infra

- This module is outside the root `go.work`: run its go commands from `packages/envoy/infra`, whose own `go.work` binds it and `packages/envoy`.
  Change its dependencies in its own `go.mod` and `go.sum`, and never add it to the root `go.work`, which would move the versions the Envoy binaries and the Legion daemon build with to whatever Pulumi needs.

- Every package of `packages/envoy` that the plugin or the `envoy-infra` command compiles in is a path a plugin release is cut from, so each fix or feature to that package cuts one.
  So `core` copies a small listener rule rather than import its package, and a test that imports the package holds the copy to the original (`TestCoreAndTheListenerAgreeOnAnAllowedHost`).

- A component's field of a plain Go type takes only a value the consumer's program knows when it runs.
  A value from another resource needs a `pulumi.*Input` field and a Construct test: Construct refuses such a value in a plain field, and `pulumi.WithMocks`, which the direct tests run on, never takes that path.

Where this file and the code disagree, the code is right: follow the code and correct this file.
