T0's public example (packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json)
with one synthetic case added, DW1: a planner on ACME-31, in planning, whose two traces hold every
Dispatch write lane D makes. No text or id of it comes from a locked file. P05 and P10 are the
example's, unchanged: they show the checks --controls must show at least once.

DW1's world holds a comment (c-scope) and an open question (a-window) on ACME-31, a spec with the
requirement r1, verification.md with the check k1, and two other issues, ACME-32 (rank 1) and
ACME-33 (rank 2). Its known-good trace answers c-scope by reply_to and a-window by ask_id (turn
human), uploads rollout.md (version 1), edits it by a quote (version 2), writes a comment it names
note-1, links note-1 in a message, moves ACME-32 to in_progress and ACME-33 before ACME-32, and
requests the spec's approval with its Verification document. It states rollout.md at version 2 with
the body its edit leaves. Its known-bad trace writes a plain comment, moves ACME-31 to todo, uploads
plan.md with bare serves and needs pointers (version 1), approves it by a plan review, uploads its
next version with force (version 2), edits ACME-32's spec by its reference
(dispatch://ACME-32/spec), requests a person's approval of plan.md, and requests the approval of a
document ACME-31 does not hold. It states no documents.

Made on 2026-11-27: the example's file with DW1 written by hand and appended to its `cases`, then
formatted as this repository's JSON is (./node_modules/.bin/biome format --write, Biome 2.4.11).
Everything but DW1 is the example's, which this command shows by printing nothing:

  diff <(jq -S '.cases |= .[:2]' packages/daemon/internal/daemon/testdata/teardown-effects/dispatch-writes-cases.json) \
    <(jq -S . packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json)

What the replay must show. TEARDOWN_EFFECTS_EVIDENCE_DIR=<dir> bash scripts/e2e/teardown-effects.sh
--controls --cases packages/daemon/internal/daemon/testdata/teardown-effects/dispatch-writes-cases.json
replays six traces: P05 good and bad and P10 good and bad, as on the example (controls/), and DW1
good and bad. Every control it promises executes, and it exits 0. For DW1:
  - each start seeds c-scope and a-window, each under an id of the scratch Dispatch's, and no
    replay.json names a world part;
  - DW1/good/replay.json has verdict ok, no call, and nine `end` changes, one for each action, each
    `{"kind":"dispatch","source":"DW1 good trace, action <n>","what":"<command>","at":<its at>,
    "outcomeSeq":<n>}`, with no `refused`: the spec's approval request is a made write;
  - after the last change, the real Dispatch holds rollout.md at version 2 with the body the trace
    states, so the documents check passes;
  - DW1/bad/replay.json has verdict ok and eight `end` changes; action 7 carries
    `"refused":{"cause":"plan-approval","status":409,"code":"PLAN_APPROVED_BY_REVIEW",...}`, action 8
    carries `"refused":{"cause":"missing-artifact","status":null,"code":null,...}`, and no other
    change carries `refused`;
  - each of DW1/good/effects.json and DW1/bad/effects.json has no answer, no difference and no
    handoff record, the phase planning before and after, and no production check.
A HARNESS verdict of DW1 names the action and the command, and Dispatch's code where Dispatch
refused it: a write lane D does not make as the role makes it.
