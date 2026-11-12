# CONCORD, driven on a real disagreement of our own review phase

Input: review 5854791039 of pull request 3522 of cybermonklabs/legion, at head e4c5e7dd96f8088caefbccbabd77ce66ef412059, on dispatch://INFRA-2551.

Requirement r7.
The library under test is `packages/ioc-concord` of `github.com/ioc-project/cognition-protocols` at clone commit `e3c7bf10f676de37ac08d2fe97e7de982f74629a`, and the specification is `protocols/concord/spec/CONCORD-SPEC.md` of the same repository at the same commit.

Our own tree is `/legion/workspaces/cybermonklabs/legion/infra-3134`, read at commit `9542c6fbbecd184ccee73523ab3defa39d6cf23a`.
`$IOC_CLONES` is `/legion/scratch/tmp/ioc-clones`.

Every command of this file and its output are in `evidence/concord-run.txt`.
The input driven through the library is `evidence/concord-input.json`.
The driver is `evidence/concord-drive.py`, the pinned corpus is `evidence/concord-prs.txt`, and `evidence/concord-build-run.sh` rebuilds `evidence/concord-run.txt` from them.

## The input, taken from our own record

The corpus is the review bodies of the last 200 merged pull requests of `cybermonklabs/legion`, read with `gh` on 2026-11-12, pinned as the number list in `evidence/concord-prs.txt`: 5160 review bodies [tc-corpus-1].

The search over that corpus for a thermonuclear-pair line carrying both graders' verdict words finds 23 lines, on 15 pull requests, where `thermonuclear-deep-review` and `thermonuclear-code-quality` returned different verdicts at one head [tc-disagree-1].

The search therefore found a real disagreement, so the substitute of two recorded review rounds on one head whose outcomes differed was not used, and no absence row is recorded for it [tc-input-3].

The input is the most recent of those lines: review `5854791039` of pull request `3522`, at head `e4c5e7dd96f8088caefbccbabd77ce66ef412059`, where the deep review returned `APPROVE` with no Medium or High and three Minor, the code quality report returned `CHANGES` with seven items, and the reviewer declined all ten, each for its own reason [tc-input-1].

The issue that head belongs to is `dispatch://INFRA-2551`, named by the review body's own `dispatch results --issue INFRA-2551` [tc-input-2].

Our record defines that shape: the `Thermo` line of the pull request body names the thermonuclear pair at the reviewed head [tc-our-1], and the two graders' verdict words are `APPROVE` and `CHANGES`, fixed by the two skills [tc-our-2].

## The library ran

The library installs from the clone with `uv` and imports as version `0.1.0` on Python 3.12.15 [tc-install-1], and it declares no dependencies at all [tc-install-2].

It asked for no key, no broker and no service, and refused nothing: the whole path ran to a commit document and the driver exited 0 [tc-run-1].

Run 1, at the library's own default threshold `0.6` with `eps=0.05` and `B=8`, ended `SUCCESS` at round 0 on the code quality grader's seed, "send the head back for changes", with a floor of 0.90 [tc-run-2].

Run 2, at threshold `0.95` and otherwise the same, ran SELECT, found no feasible candidate, named the deep review grader the bottleneck of the incumbent, asked it for a repair, took as that repair what the reviewer actually recorded at this head, and ended `SUCCESS` on it at round 1 with a floor of 1.00 [tc-run-3].

The two runs differ against our record: run 1 decided to send the head back, which is not what our record did, and run 2 decided what our record did, namely approve the head and record each reported finding as declined with its reason [tc-run-4].
The threshold is the only parameter that differs between them, and our record holds no threshold.

## What the library's model cannot hold

The raw 0-100 ratings driven into the library are not in our record.
Our record holds a verdict word and a finding count for each grader and no satisfaction rating and no anchor pair anywhere in the 155 review bodies of the corpus that carry a thermonuclear-pair line [tc-derive-1].
The ratings of `evidence/concord-input.json` are derived by the rule that file declares under `derivation`, which is this evidence's author's and not our record's, so every number of the two runs rests on it.

The library has no place for a finding's severity: our deep review grader's verdict turns on whether a finding is Medium or High, and the word Medium, High, Critical, Minor or severity appears nowhere in the library [tc-gap-sev].

It has no place for a finding at all, nor for a verdict, an abstention or a confidence: none of those words appears in the library [tc-gap-find].

A rating in the library carries a raw number and free reasoning text and nothing else [tc-gap-cite-1], while our graders are required to return findings with file and line evidence [tc-gap-cite-2].

The library gives no agent authority over another's rating.
Every agent is one entry of a flat list, the bottleneck is only the lowest rater of the incumbent, and nothing lets a reviewer decline a grader's finding and carry on [tc-gap-auth].
That is the step our record actually took at this head, and it is why run 1 reaches a different decision from ours.

## Where the library and its own specification differ

The design record under check makes two claims about this. Each was checked at the pinned commit.

The specification's message vocabulary is `concord-init`, `concord-init-reply`, `concord-score`, `concord-repair` and `concord-result`, with the control messages `concord-error`, `concord-resend` and `concord-cancel` [tc-voc-1].

The claim that the library's vocabulary is `concord-anchor`, `concord-propose-seed` and `concord-commit` is false as stated: the library names five request tags, `concord-anchor`, `concord-propose-seed`, `concord-score`, `concord-repair` and `concord-commit`, and two of them are the specification's own [tc-voc-2].

The difference the claim points at is real for the other three: `concord-anchor` against `concord-init`, `concord-propose-seed` against `concord-init-reply`, and `concord-commit` against `concord-result`, and the library's reply and notice tags `concord-anchor-reply`, `concord-candidate-reply`, `concord-score-reply`, `concord-delivery`, `concord-missing` and `concord-ack` have no counterpart in the specification at all [tc-voc-3].

The claim about arithmetic is verified in both halves.
`TOL = 1e-9` is on line 16 of `engine.py` [tc-tol-1], and `engine.norm` divides floats and rounds nothing on lines 22 to 25 [tc-tol-2].
The specification requires the opposite: each normalized score computed exactly as a rational number, rounded to 6 decimal places half up, and compared as integers in millionths so that no floating-point error can change a decision [tc-tol-3].
The library's `norm` returns `0.5000000000000001` where the exact value is `1/2` [tc-tol-4], and no `Fraction`, `Decimal` or millionth appears anywhere in the library [tc-tol-5].

Two further differences were found while running it.

The specification calls a raw value outside 0 to 100, or written as an exponent, `NaN` or `infinity`, invalid [tc-rng-1], and the library's `wire.parse_reply` accepts `1e9`, `NaN`, `-40` and `250` without complaint [tc-rng-2].

The specification's `version`, `run_id` and `uuid` attributes and its `concord-resend` and `concord-cancel` messages have no counterpart in the library [tc-ctl-1].

The two are also different protocols in shape.
The specification is peer-to-peer and says in as many words that nothing requests a score or a repair, each participant working out its own obligations, while the library builds a score request and a repair request that an initiator addresses to named agents [tc-arch-1].
