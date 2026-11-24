# `spec-check state`

`spec-check state --scenario <dir> [--json]` derives the state of each check and approved statement in a scenario. The command reads the same scenario format as `specgraph.ReadScenario`; it does not change the scenario or its expected output.

A scenario contains `approved.json`, `files`, `history.json`, `jobs.json`, `results.json`, `subjects.json`, `ties.json`, and `uncited.json`. It can contain `units.json`; without it, no statement wait is derived. `want.txt` is test data only and is not read by the command.

The command prints one result for every subject. A statement is `met`, `met, partly checked`, `not built`, `not met`, or `unknown`. It then prints every check that reads that subject as `true`, `false`, or `unknown`, including a reason and, for a true or false result, where its counted result came from: the commit for a check of the code, the deploy for a check of production, and the graph transaction for a check of the graph. It prints no list of a check's results with their times. If `units.json` is present, it also prints whether each wait ends or holds. `--json` emits the same derived data as JSON.

Scenario input is strict. Unknown fields, duplicate fields, invalid JSON types, and `null` values are refused rather than silently read as zero values. A missing member follows its declared input mark: some missing values are valid zero values, while members whose absence would hide a fact are refused. Empty objects can be valid values; for example, an empty `uncited.json` says CI refused no check.

A result counts only for the procedure and subject it read. For checks that count at commits, a result carries to another commit only when the check has known ties at both commits and no tied path changed. A result whose current check text, current statement approval, or tied code does not match is not reused.

A program check of code needs a valid failing example before it can be true. The failing example counts from `planted_on`, never from its own `commit`, which is the commit where CI ran the fault. It needs one or more fault paths; every path must be a source file, not a test file, and where the check's tie at `planted_on` is known, that tie must cover every path. A record with no planting commit, no fault path, an all-white-space path, a blank path beside a real path, or `planted_on` equal to its own commit counts nowhere and makes the check unknown with its specific reason when it is the newest candidate. A record whose `planted_on` names no commit of the history also counts nowhere; as the newest candidate it makes the check `unknown (planted_on_outside_history <commit>)`.

A CI refusal at a commit makes its check unknown at that commit. It removes that commit's own results, including a planted failing example whose counted commit is the refused commit, but it does not remove evidence from other commits, and it does not remove a record that counts from no commit, whether CI refused the check at the commit its `planted_on` names or at its own commit.

The derivation package's scenario sweeps derive, in the test process, each scenario with one member left out, set to null, written as a name of nothing or added, and each with one more CI refusal, and fail where such an input moves a check toward true, a statement toward met or a wait toward ends beyond the right that the sweep states for it.

The command tests run the built binary against the fixture scenarios and compare its output with each fixture's `want.txt`. The derivation package is the source of truth for the state rules; this document describes the scenario interface and the invariants at its boundary.
