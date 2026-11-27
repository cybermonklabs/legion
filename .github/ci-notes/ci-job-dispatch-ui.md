# Notes for the dispatch-ui job's steps

The derivations and the history of `.github/workflows/ci-job-dispatch-ui.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that holds the derivation
or the record its job or step rests on. A value's own arithmetic stays beside the value.

## Why the checks run as two jobs

Until dispatch://INFRA-1075 measured it, one job ran every step in turn on one 4-core
`ubuntu-24.04-arm` runner. Its wall in the 20 green merge groups of 2026-11-27 02:50Z to 13:27Z
(runs 45930678805 to 46007299920) was p50 448 s (393 to 503). The step "Test dispatch dashboard"
ran the package's `test` script, the web lane and then the harness lane, and took p50 286 s
(245 to 331) of it; "Typecheck dispatch dashboard" took 53 s (48 to 63), "Build dispatch
dashboard" 37 s, "Test proof editor" 24 s, "Install zsh" 15 s, the checkout 14 s and
`bun install` 5 s. In 8 of those jobs' logs the web lane took 135.7 to 177.4 s and the harness
lane 112.7 to 154.7 s.

That step is computation, not waiting. Scratch run 46022571148 (branch
probe/dispatch-ui-time, main e3cb409d, three jobs per variant, Neoverse-N2, `nproc` 4) sampled
the whole runner's /proc/stat each second. Run one after the other, the web lane took
147.4/154.4/162.4 s at 3.67 to 3.86 busy cores (560/559/604 CPU-s) and the harness lane
117.4/130.7/158.7 s at 2.73 to 3.36 busy cores (291/337/322 CPU-s); the cores were idle only in
the tail of each lane, where its last files run alone (the web lane's useIssueDrafts.test.ts
sweep, 50 to 53 CPU-s, 34.9 to 36.8 s when run alone, and the harness lane's outside-* files,
whose rows wait out their own scripts' clocks). Run at the same time on the same runner, the
two lanes took 243.9/253.1/260.1 s against 271.8/278.1/321.1 s one after the other (each the two
lanes of one job): the six jobs' two lanes took 830 to 926 CPU-s, which four cores cannot run in
less than 207 to 232 s, so one runner gains at most the two tails. "Typecheck dispatch dashboard"
kept 1.36 to 1.40 cores busy for 51.1 to 59.1 s (all six jobs), leaving the rest of the runner
idle.

So the work is split over two runners, balanced by those medians: dispatch-ui runs the proof
editor's typecheck and tests, the web lane, the build and the asset checks (about 21 + 29 + 159
+ 37 s); dispatch-ui-harness runs the contracts build the harness's Playwright spawn reads, the
dashboard's typecheck, zsh's install and the harness lane (about 21 + 53 + 15 + 133 s). Both
lanes still run on a runner of their own with the package's `test` script's commands and its
`--parallel=8`, so no row runs under more load than it did, and the second runner's own setup
(checkout, Bun and `bun install`, about 21 s) is the cost.
