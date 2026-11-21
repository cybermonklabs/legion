# The production check after the close

Part of the merge gate (`skill://legion-worker/references/merge-gate.md`). Read it when you work a look of a closed issue's production check.

- `projects.<KEY>.production_check` in `legion.yaml` is `tester` (the default) or `off`. Under `off` the close opens no check at all. A check keeps the role it opened with, and only that role may report on it (`CHECK_ROLE_MISMATCH`).
- Reach the deploy as the deployment instructions say. When they name an agent or a role for it, `envoy_publish` the merge commit to that topic with `expects_reply: false`, and await no reply. Open a `dispatch ask` only where the instructions name nobody: the production gap and why it matters, then the required install or restart step, its risk, and outcome-named options; the look still ends `pending`.
- A staging pass is no production pass: staging does not run every resource production does.
- Report each look's verdict as your role prompt says. A `fail` follows `skill://legion-worker/references/findings.md`: file no issue. The architect accepted before the merge and is asked nothing after.
