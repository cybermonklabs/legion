---
title: "A test's expectation of the daemon's own contract spells the package's derived value, never the bare number"
category: legion
tags:
  - daemon-api-version
  - contract-entry
  - tests
  - worker-pod
date: 2026-10-16
status: active
module: packages/daemon/cmd/legion, packages/daemon/internal/daemon
related_issues:
  - "LEGION-636"
  - "sjawhar/legion#1984"
  - "INFRA-1569"
---

# A test's expectation of the daemon's own contract spells the package's derived value, never the bare number

Extends docs/solutions/legion/a-contract-numbers-prose-is-declaration-history-or-current-write-current-as-a-pointer-and-sweep-docs-solutions-too-LEGION-578.md.

- That document's rule for prose holds for a test's expected output, which a prose search does not
  match: `PluginContract: 24` in a fake daemon's `supervise.Deps`, a golden stdout line
  `… architect LEGION-208 suspended 1 24\n`, a sentence `this daemon requires 24`. Each states the
  daemon's own contract, a current value, and since dispatch://INFRA-1569 that value moves at every
  contract entry `main` merges, with no edit on the branch that holds the test.
- Spell each as the derived value: `api.DaemonAPIVersion()` where an int is wanted, and the
  package's derived string where the expectation is text (`cmd/legion/probe_image_test.go`'s
  `thisBinarysContract`; `fmt.Sprintf("… requires %d …", api.DaemonAPIVersion())` in
  `internal/daemon`). Find the literals with `grep -rn '\b<n>\b' packages/daemon --include='*_test.go'`,
  `<n>` being the number `bash scripts/daemon-contract.sh` prints for the working tree.
- The packages that hold these tests are the ones a worker pod cannot run (`internal/daemon`
  needs Docker for NATS; `cmd/legion`'s claims tests ran only when the package was run whole),
  so a filtered local `go test` never sees them and CI does. The grep is the local check.

## Evidence (LEGION-636, sjawhar/legion#1984)

The 24 → 25 bump touched `version.go`, the fixtures, the TypeScript mirror and the plugin's pin
together, and the Tests job still went red at 4035e7c0 and 53cb8bdf: `cmd/legion/claims_test.go`
expected `suspended 1 24` and `ready 1 24` with `PluginContract: 24` in its deps, and
`internal/daemon/plugin_test.go` expected `this daemon requires 24` twice. Each was found by CI
only, since the pod's filtered runs skip those tests; each was fixed by the constant.
