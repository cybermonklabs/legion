# Notes for the dispatch-ui job's steps

The derivations and the history of `.github/workflows/ci-job-dispatch-ui.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that holds the derivation
or the record its job or step rests on. A value's own arithmetic stays beside the value.

## Why the checks run as two jobs

Until dispatch://INFRA-1075 measured it, one job ran every step in turn on one 4-core
`ubuntu-24.04-arm` runner. Its wall over every green merge-group run of 2026-11-27 (91 runs, the
27 that ran dispatch-ui) was p50 449 s (393 to 503). The step "Test dispatch dashboard"
ran the package's `test` script, the web lane and then the harness lane, and took p50 287 s
(245 to 331) of it; "Typecheck dispatch dashboard" took 53 s (48 to 63), "Build dispatch
dashboard" 37 s, "Test proof editor" 24 s, "Install zsh" 15 s, the checkout 14 s and
`bun install` 5 s. In 9 of those jobs' logs the web lane took 135.7/142.0/151.3/157.3/158.4/
159.9/163.1/169.5/177.4 s and the harness lane 112.7/123.3/124.7/128.3/137.7/141.7/142.7/145.6/
154.7 s.

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
editor's typecheck and tests, the web lane, the build and the asset checks;
dispatch-ui-harness runs the contracts build the harness's Playwright spawn reads, the
dashboard's typecheck, zsh's install and the harness lane. Both
lanes still run on a runner of their own with the package's `test` script's commands and its
`--parallel=8`, so no row runs under more load than it did, and the second runner's own setup
(checkout, Bun and `bun install`) is the cost. Measured on the three attempts of run
46036413948 at the head that split them: dispatch-ui 260/241/230 s, its web lane 171/149/148 s;
dispatch-ui-harness 224/256/230 s, its typecheck 55/54/55 s and its harness lane 130/162/144 s.
The check is as slow as the slower of the two, so 260/256/230 s against the one job's p50 449 s.
Each lane's own wall stays inside what it read before the split (web 135.7 to 177.4 s, harness
112.7 to 154.7 s over 9 jobs), but for the 162 s reading, whose runner also took 16 s to check
out where the other two took 4 s.
