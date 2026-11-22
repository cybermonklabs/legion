# The PR body lines the changed paths require

Read this at step 4 of `skill://legion-retro`. A failure here stops retro as its "When retro
stops" says. A repository can require body lines derived from the changed paths, read by a required
check; the body approved earlier may not cover every path the pull request now changes, and the
merger reports a stale body rather than rewriting it. `<owner>/<repo>` and `<n>` are the pull
request's repository and number.

1. Read your deployment instructions, the repository's agent guide (`AGENTS.md`, `CLAUDE.md`), its
   pull request template (`.github/pull_request_template.md` or `.github/PULL_REQUEST_TEMPLATE/`)
   and the workflow under `.github/workflows/` that reads the body, for lines they require per
   changed path. When they require none, stop here.
2. List the paths the pull request changes at `<head>`, the approved head your task names:

   ```bash
   base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && jj -R "$LEGION_WORKSPACE" diff --from "fork_point(${base}@origin | <head>)" --to <head> --name-only
   ```

   `@json` quotes `base` as a jj string; never quote a branch name by hand. Write the line each
   matching path pattern requires. A line that says work was done names work this pull request
   did: a line whose work is undone is a finding for the architect.
3. Make a directory outside the workspace with `dir=$(mktemp -d)` (jj would commit a file inside
   it), save the live body as `$dir/before.md` and `$dir/body.md`, and stop unless the read
   succeeded and is not empty:

   ```bash
   cd -- "$LEGION_WORKSPACE" && ( set -o pipefail && gh api repos/<owner>/<repo>/pulls/<n> --jq .body | tee "$dir/before.md" "$dir/body.md" ) && test -s "$dir/before.md"
   ```

   The parentheses keep `pipefail` to this line. In `$dir/body.md`, change only the path-derived
   lines, adding a missing one where the sources place it; leave every other line byte for byte,
   the `Checks` block included.
4. Write it back. A body edit changes no commit, so the approval stands:

   ```bash
   cd -- "$LEGION_WORKSPACE" && gh api --method PATCH repos/<owner>/<repo>/pulls/<n> -F body=@"$dir/body.md"
   ```

When the write is refused, or the check it feeds fails, report it to the architect with the
command and its output.
