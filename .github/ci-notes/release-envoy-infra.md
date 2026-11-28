# Notes for the envoy-infra release workflow

The derivations behind the budgets of `.github/workflows/release-envoy-infra.yaml` that point into
this file with `# why:` lines, as `.github/ci-notes/README.md` says. Each value's own arithmetic
stays on its line in the workflow file.

<a id="the-build-step-whose-notices-script-the-gate-does-not-read"></a>

## The build step and the go-licenses install its notices script retries

`Build pulumi-resource-envoy-infra and envoy-infra` runs `scripts/go-third-party-notices.sh`
twice for each target of `.github/envoy-infra-targets.json`. The script installs go-licenses
through `.github/scripts/ci-retry.sh` at the 180 s module bound, 5 x 180 + 150 = 1050 s, and only
where its `$tool` is not already executable; the tool's directory is named by the go-licenses
version and the host's Go version, not by the GOOS or GOARCH the packages target, so every call
after the first finds that build and fetches nothing, and the step waits for one ladder.
The budget is derived by hand.

Beside the ladder the step holds the builds and the notices themselves, which its measured
duration gives: 1057 s, the slowest of the 8 runs of the step in the 20 successful runs of this
workflow created from 2026-11-20T18:55Z to 2026-11-21T08:25Z (job 136500324925 of run
44797379894; the other runs released nothing and skipped it). 1057 + 1050 = 2107 s -> 36 min.

It is not the build job's slowest retried step: that is still the `release-push.sh` steps' 2700 s,
and the job's key, 2400 + 2700 = 5100 s -> 86 min, stands above this step's 36.

## The publish job's budget

The publish job holds a credential, and the credentialed publish jobs of the three release
workflows are an exception of R2 of dispatch://INFRA-1545: no step of it retries anything. It
carries a priced key all the same, because it runs `.github/scripts/release-push.sh`, which the
build job runs too and which therefore keeps its one retried fetch, the leak engine's in
`leak_guard`, at the 60 s release-asset bound, 5 x 60 + 150 = 450 s.
Every step that runs that script is priced at the fetch at each of `leak_guard`'s six call sites,
6 x 450 = 2700 s, so such a step carries a `timeout-minutes` of its own, and its job a key a
comment prices. So `Check build's archives and write SHA256SUMS` and `Create GitHub release`,
which run the script once, carry
46 minutes, the whole minute above 2700 s; `Tag release`, which runs it twice, carries 91, above
5400 s; and the job carries 600 s, the 10 minutes it carried before that issue, plus that slowest
step's 5400 s. 600 + 5400 = 6000 s -> 101 min.

Each step that runs the script, the build job's version step and tripwire among them, sets
STEP_TIMEOUT_MINUTES to its own `timeout-minutes` in its `env:`, whichever of the script's
commands it runs: `restart`, `release` and `release-section` make their GitHub calls through
`.github/scripts/gh-read.sh`'s gh_wait_reset, which waits out a spent API budget only within
the nearer of the job's limit and the step's, and `.github/scripts/check-workflow-files.sh`
refuses a step with a limit of its own that reaches gh-read.sh and passes none
(dispatch://INFRA-1545, round 55). The entries carry no comment in the workflow, as in
release.yaml and release-envoy-listener.yaml, whose notes say the same.
