# Checks and their results

## What a result is

A check is a `check` block of the issue's Verification document: `dispatch results --issue <issue>` lists each, with its `run`, `when` and `negative_control`, and every result recorded against it.

A result is one run of one check on a production-like surface, recorded with `dispatch result --issue <issue> --check <block id> --role <your role> --verdict pass|fail --commit <sha>` and its `--surface`, `--run`, `--observed` and `--negative-control`. Record it only at a commit the issue branch holds on GitHub; Dispatch refuses another (`RESULT_COMMIT_UNKNOWN`). A unit test is no result.

A `before_merge` result stands only at its commit. After a push, record each such check again: run it again where the change can reach what it drives, and otherwise record the earlier pass with `--observed 'carried from <sha>: <why the change cannot affect it>'`.

A check that carries `ci_check` is CI's: no role runs or records it (`skill://legion-worker/references/ci-checks.md`).
