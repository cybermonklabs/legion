# Caller identity providers

`internal/cipher/identity` holds what every caller identity provider of the broker implements
(`identity.go`) and the registry the broker reads them through (`registry.go`). A provider is the
code for one kind of caller token: a pod's projected service-account token, a Google identity
token, a GitHub workflow run's token.

## Where a provider's code goes

Each provider is three things, and nothing else in the module names it:

- **One package, `internal/cipher/identity/<kind>`** (`kubernetes`, `google` and `github` today).
  It holds both halves of the provider. The broker's half: the verifier, the token types it adds
  to the RFC 8693 exchange (no file outside the package spells one out), its decision, its
  principals and subjects, and the parsing of its own `BROKER_*` variables, each documented there:
  cipher-refgen reads that package for the broker's configuration page. The client's half: the
  identity sources a machine of that kind proves itself with (`Sources`, the values
  `AGENT_SECRETS_MACHINE_IDENTITY` takes), and where it has them, a detector that finds one with
  no setting and a token a service of the bus connects to the NATS auth service with. Its
  `contract_test.go` calls `identitytest.Contract`.
- **One file in `internal/cipher/identity/boot`, `<kind>.go`**, behind
  `//go:build !cipher_no_<kind>`, which registers the broker's half.
- **One file in `internal/cipher/identity/providers`, `<kind>.go`**, behind the same tag, which
  registers the client's half for `agent-secrets`, its helper, `natsprobe`, the bus and
  `envoy-dispatch settings`.

`-tags cipher_no_<kind>` takes a provider out of both registries at once.

The TypeScript client keeps its own list of the same sources, `IDENTITY_SOURCES` in
`packages/cipher-client/src/environment.ts`, which adds the device login: its broker access token
is the broker's own, which no provider verifies. A source a provider adds goes in both lists.

`internal/cipher/config` imports no provider package outside its tests. It holds the registry's
`boot.Settings` and asks it: `Settings.Serves`, `Settings.Check`, and `boot.ParseAccount` for an
account a setting names. A test that needs one provider's settings reads them with
`boot.ConfigOf`.

## What holds this

- `boot`'s `TestEachProviderStaysInItsPackage` fails when a package other than the provider's own
  and the registries imports a provider package, and when a provider imports a consumer of
  identity (`config`, `api`, `enroll` and the others it lists).
- `boot`'s `TestEachRegisteredProviderRunsTheContract` fails when a registered provider's tests
  call no `identitytest.Contract`.
- `boot`'s `TestNoTokenTypeLiteralOutsideItsProvider` fails when a file outside the provider
  packages spells out a provider's token type.
- `boot`'s `TestClientSourcesAgree` runs the TypeScript client's
  `scripts/print-identity-sources.ts` and fails when the two clients' sources differ, apart from
  the device login, or when either names a provider the broker does not hold.
