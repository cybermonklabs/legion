# A workflow file holds the structure of its jobs, and nothing else

A file of `.github/workflows/` names its jobs, their triggers, their conditions, their runners and what they call.
The path-filter data is in `.github/ci-filters.yaml`, which `dorny/paths-filter` reads by path, in one step of `ci.yaml`'s `changes` job.
A `run:` body of more than 10 lines is a script under `.github/scripts/`, run by its path, with each budget comment on the line it sizes, but for two kinds of job.
A body stays inline where its job makes no checkout, and where its job holds a credential of a release workflow, as `release.yaml`'s publish job does, because `.github/scripts/check-release-credentials.sh` refuses a credentialed job that runs a script under a `scripts/` directory, `.github/scripts/` among them.
The bodies of `ci.yaml`, `dispatch-browser.yaml`, `release.yaml` and `ci-job-tool-fallback.yaml` are moved.
The bodies of the other workflow files are not moved yet, and a file that reaches the size limit moves its bodies then, as the size check's refusal says.
A derivation or a history goes in `.github/ci-notes/<the workflow file's name without its suffix>.md`, and the step it belongs to keeps one comment line, `# why: .github/ci-notes/<file>.md#<the section's anchor>`, that names it; a value's own arithmetic stays on its line in the workflow file.
`.github/scripts/check-workflow-files.sh` holds every file of that directory to 64,000 bytes, and refuses one GitHub's own parser would not read (invalid YAML, or two keys of one mapping that are equal when upper-cased) and one that calls more than 45 unique reusable workflows.
The number is 64,000 because GitHub starts no run at all for a workflow file above its cap of 500 KB and signals that only as a run with no jobs in it, so the limit a reader is held to is a readable file rather than a parser's limit.
`ci.yaml`'s `ci-filters` job runs the check, `.github/workflows/workflow-files.yaml` runs it a second time in a file small enough to start when `ci.yaml` is too large to, and `ci.yaml`'s test job runs `.github/scripts/check-workflow-files.test.sh`.
actionlint 1.7.12 checks every workflow file in those same two places, with `.github/actionlint.yaml`, and the shellcheck 0.11.0 it hands each inline `run:` body reads at warning severity, so a finding of either fails the pull request.
Run what CI runs before you push a workflow change, from the root of a git checkout, since actionlint finds no project outside one: `SHELLCHECK_OPTS=--severity=warning go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -shellcheck='uvx --from shellcheck-py@0.11.0.1 shellcheck' -verbose`
