# gh's variables, and the credentials your pod carries

Read it before you set a gh variable, and before you report a resource unreachable or read a GitHub `404` as no access.

- Never run `gh config set` or `gh alias set`, and leave `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` unset:
  a set variable outranks the gh directory the daemon refreshes.

- Before you report a resource unreachable, or read a GitHub `404` as no access, try the call through `skill://agent-secrets` and read the profiles in `AWS_CONFIG_FILE`.
  Request a secret only for your issue's work.
