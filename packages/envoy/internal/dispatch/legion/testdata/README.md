# The daemon's admission record

`admission_changes_test.go` reads these copies of the Legion daemon's goldens.
The daemon's own tests write each golden, so its bytes are the daemon's wire shape.
The daemon on `main` serves neither `admission.capChanges` nor `admission.signalChanges` yet, so the copies come from the branches that serve them.

| File | Golden | Commit |
| --- | --- | --- |
| `state-capchanges.json` | `state.json` | `7e86fef20b`, the branch of dispatch://INFRA-5616 (#5384) |
| `state-capchanges-empty.json` | `state-controller-claim.json` | `7e86fef20b` |
| `state-signalchanges.json` | `state.json` | `c169f46a44`, the branch of dispatch://INFRA-5899 (#5603) |
| `state-signalchanges-empty.json` | `state-controller-claim.json` | `c169f46a44` |

Each file is `git show <commit>:packages/contracts/fixtures/daemon-api/<golden>`.

To make the goldens again at a later head of either branch, run the daemon's test that writes each one there, then copy it here with `git show` or `cp`:

- `state.json`: `cd packages/daemon && go test ./internal/api -run '^TestStateGolden$' -update`
- `state-controller-claim.json`: `cd packages/daemon && go test ./internal/projection -run '^TestTheStateWithTheControllersClaimGolden$' -update`
