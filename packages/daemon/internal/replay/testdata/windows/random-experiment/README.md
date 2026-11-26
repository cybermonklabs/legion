# The window `random-experiment`

A recorded window of project ACME, written by hand by the conventions of `../README.md`:
four hours, from 2001-03-05T09:00Z to 13:00Z, an admission cap of 2, four issues, recorded under the rule `main`.
Its log and its events are those of `priority-inversion`, byte for byte, and so are their hashes in `files`.
Its `window.json` differs in one key: `experiments` lists one workflow experiment, `review-depth`, in the `random` mode, over the unit `issue`, with two variants, `short` and `long`, of weight 1 each and of no plan, no settings files and no instructions.
`collect` writes no experiment into a window, so this window is the only kind of window that lists one: one written by hand.

It holds one fact: a run draws an experiment of the `random` mode from one source of its seed, in the order it records its issues, and keys that draw to no issue and no stretch.
So a run names those draws in `run.json` as draws that it does not key (`unkeyedDraws`), and `compare` pairs no run of the window: it calls no rule better or worse than `main` there, and gives each difference on it as not paired.
`expect.json` holds the scorecard of a replay under `main` on the main branch and the count of those draws, worked by hand below.

## The issues, what happens and the replay under `main`

As in `priority-inversion`'s README: four roots, ACME-1 filed at 09:00 and ranked `a2`, ACME-2 at 09:05 and `a0`, ACME-3 at 09:30 and `a1`, and ACME-4 at 10:00 and `a3`, each filed `todo` with the `legion` label.
The replay under `main` is the record.

## The draws of the experiment, worked by hand

The daemon draws an experiment of the unit `issue` for an issue when it first records it: the admission pass records a root that Dispatch hands to Legion when the root's filing reaches it, and draws there (admit's `putNewRoot`).
Each of the four roots is filed inside the window and recorded at its filing, so the run draws `review-depth` four times: for ACME-1 at 09:00, ACME-2 at 09:05, ACME-3 at 09:30 and ACME-4 at 10:00.
A random draw reads one value of the run's source each, and no other draw reads that source: the window lists no experiment of the unit `tree`, whose draw a tree's opening would make, and no experiment in the `on` or `off` mode, which reads no value.
`run.json`'s `unkeyedDraws` is `{"experiments": 4}`.

Each draw records `short` or `long` for its issue and decides nothing else: no variant names a plan, so no issue runs in a group of its own, and no variant names a settings file or instructions, which only a launch would read.
No draw decides a slot (`window.go`, `Experiments`).
So every measure of the run is that of `priority-inversion`, and the scorecard under `main` is the one its README works out by hand, as `expect.json` gives it.

The source starts afresh from the run's seed at each boot and is read in the order the engine and the admission pass record their issues.
A rule that records the same issues in another order, or at another boot, draws another variant for one of them, and no key names the draw of one issue alike under two rules.
So `compare` gives each difference of a rule from `main` on this window as not paired.

## The signals, worked by hand

As in `priority-inversion`'s README: the window holds no record of any of R11's signals, so at 09:00 the run under `main` writes each signal unrecorded (`null`) in `signalValues`, and it echoes the six limits its checks give.
