# Checks and their results

## What a result is

A check is a `check` block of the issue's Verification document: `dispatch results --issue <issue>` lists each, with its `run`, `when` and `negative_control`, and every result recorded against it.

A result records one check on a production-like surface.
Use `dispatch result --issue <issue> --check <block id> --role <your role> --verdict pass|fail --commit <sha>` with `--surface`, `--run`, `--observed` and `--negative-control`.
Push the commit before you record the result.
Dispatch refuses an unknown commit (`RESULT_COMMIT_UNKNOWN`).
A unit test is no result.

A `before_merge` result stands at its compared head, while its check is unchanged.
After a push, rerun each check that the change can affect.
Record each other pass at the new head with `--observed 'carried from <sha>: <why the change cannot affect it>'`.
Read `dispatch results` for the verdict at each required pull request head.

An `after_merge` result counts on the branch that received the merge or on the default branch.
Its commit must be the relevant merge or contain that merge in its history.
Follow the production check task to select the commit and record what you observed.

A check that carries `ci_check` is CI's: no role runs or records it (`skill://legion-worker/references/ci-checks.md`).
