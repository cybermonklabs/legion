# Caller identity providers

`internal/cipher/identity` holds what every caller identity provider of the broker implements
(`identity.go`) and the registry the broker reads them through (`registry.go`). A provider is the
code for one kind of caller token: a pod's projected service-account token, a Google identity
token, a GitHub workflow run's token.

## Where a provider's code goes

Each provider is two things, and nothing else in the module names it:

- **One package, `internal/cipher/identity/<kind>`** (`kubernetes`, `google` and `github` today).
  It holds the provider's verifier, the token types it adds to the RFC 8693 exchange, its
  decision, its principals and subjects, and the parsing of its own `BROKER_*` variables, each
  documented there: cipher-refgen reads that package for the broker's configuration page. Its
  `contract_test.go` calls `identitytest.Contract`.
- **One file in `internal/cipher/identity/boot`, `<kind>.go`**, behind
  `//go:build !cipher_no_<kind>`, which registers the provider. `-tags cipher_no_<kind>` builds the
  broker without it.

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
