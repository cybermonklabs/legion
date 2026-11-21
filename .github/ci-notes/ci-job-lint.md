# Notes for the lint job's workflow

The derivations behind the budgets of `.github/workflows/ci-job-lint.yaml` that point into this
file with `# why:` lines, as `.github/ci-notes/README.md` says. Each value's own arithmetic stays
on its line in the workflow file.

## The notices step whose script the gate does not read

`Check every Go release archive's third-party notices can be written and the cross-built commands
compile` runs `.github/scripts/go-release-notices.sh`, which runs `scripts/go-third-party-notices.sh`
for every release target in three modules. That script installs go-licenses through
`.github/scripts/ci-retry.sh` at the 180 s module bound, 5 x 180 + 150 = 1050 s, and only where its
`$tool` is not already executable. The tool's directory is named by the go-licenses version and
the host's Go version, and the three modules name one Go, 1.27.3, so every call after the first
finds that build and fetches nothing, and the step waits for one ladder.
`.github/scripts/check-download-retry.sh` reads go-release-notices.sh but no script outside
`.github/scripts/`, so it prices none of that, and the budget is derived by hand
(dispatch://INFRA-2899 owns the reach that would make it the check's): the ladder beside the
step's measured duration, 132 s at most over its 28 runs in the 40 successful ci.yaml runs created
from 2026-11-21T07:52Z to 08:57Z (job 136791133115 of run 44885569301).
132 + 1050 = 1182 s -> 20 min, under the job's 87.
