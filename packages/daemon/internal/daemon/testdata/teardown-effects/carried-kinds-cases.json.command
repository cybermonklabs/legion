T0's public example (packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json) with
actions of the five carried kinds (an Envoy message, a Dispatch read, a task agent's start, a probe of
production, a diff read) interleaved among the actions of its four traces, in time order, and each
trace's actions numbered again from 1. Its worlds, its cases' actions and its rubrics are the
example's. So a daemon action keeps its place in time and moves in seq: P05's completion is action 10
of its known-good trace (it is 5 in the example) and action 8 of its known-bad one (5 in the example),
P10's refused report is action 5 and its accepted one action 7 (2 and 3 in the example), and P10's
known-bad comment is action 3 (1 in the example).

Made on 2026-11-27 with jq 1.7 (/usr/bin/jq), from the example's file, then formatted as this
repository's JSON is (./node_modules/.bin/biome format --write, Biome 2.4.11):

  jq -f carry.jq packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json

where carry.jq is:

# Interleaves actions of the carried kinds (envoy, dispatch-read, agent, probe, diff-read) among the
# actions of the example's four traces, in time order, and numbers each trace's actions again from 1.
def renumber: to_entries | map(.value + {seq: (.key + 1)});
.cases |= map(
  if .id == "P05" then
    .traces.good.actions = ([.traces.good.actions[],
      {kind: "diff-read", seq: 0, at: "2030-01-07T15:02:05Z", command: "jj diff", source: "$LEGION_WORKSPACE", paths: []},
      {kind: "dispatch-read", seq: 0, at: "2030-01-07T15:02:20Z", command: "read", target: "ACME-12", fields: {}},
      {kind: "agent", seq: 0, at: "2030-01-07T15:03:00Z", agent: "task", name: "ReviewDiff", fields: {task: "Read the diff of the pushed branch and name any file it leaves out of the step record.", solutionSpace: "one reading of the diff, no change"}, endedAt: "2030-01-07T15:03:20Z"},
      {kind: "envoy", seq: 0, at: "2030-01-07T15:03:50Z", tool: "publish", target: "notifications.role.acme.acme-12-tester", fields: {message: "ACME-12 is pushed with its pull request open; k1 passes at the pushed head.", expects_reply: false}},
      {kind: "dispatch-read", seq: 0, at: "2030-01-07T15:04:15Z", command: "handoff-read", target: "ACME-12", fields: {phase: "implement"}}
    ] | sort_by(.at) | renumber)
    | .traces.bad.actions = ([.traces.bad.actions[],
      {kind: "envoy", seq: 0, at: "2030-01-07T15:02:00Z", tool: "send", target: "ses-acme-12-tester", fields: {message: "The implement handoff is written.", expects_reply: false}},
      {kind: "dispatch-read", seq: 0, at: "2030-01-07T15:02:45Z", command: "results", target: "ACME-12", fields: {}},
      {kind: "diff-read", seq: 0, at: "2030-01-07T15:03:20Z", command: "git show", source: "$LEGION_WORKSPACE", paths: ["internal/api/doc.go"]}
    ] | sort_by(.at) | renumber)
  elif .id == "P10" then
    .traces.good.actions = ([.traces.good.actions[],
      {kind: "dispatch-read", seq: 0, at: "2030-01-08T12:02:10Z", command: "read", target: "ACME-21", fields: {}},
      {kind: "probe", seq: 0, at: "2030-01-08T12:02:20Z", target: "https://service.example/version", revision: "3b5d7f9a1c2e4a6c8e0b2d4f6a8c1e3b5d7f9a0c"},
      {kind: "envoy", seq: 0, at: "2030-01-08T12:02:50Z", tool: "publish", target: "notifications.role.acme.acme-21-merger", fields: {message: "Production runs 3b5d7f9a, older than merge 5d7f9b1d; look 1 is pending.", expects_reply: false}},
      {kind: "agent", seq: 0, at: "2030-01-08T12:03:20Z", agent: "task", name: "ReadDeploy", fields: {task: "Read the deployed revision once and report it."}, endedAt: "2030-01-08T12:03:30Z"}
    ] | sort_by(.at) | renumber)
    | .traces.bad.actions = ([.traces.bad.actions[],
      {kind: "dispatch-read", seq: 0, at: "2030-01-08T12:02:10Z", command: "steps", target: "ACME-21", fields: {}},
      {kind: "probe", seq: 0, at: "2030-01-08T12:02:20Z", target: "https://service.example/version", revision: "3b5d7f9a1c2e4a6c8e0b2d4f6a8c1e3b5d7f9a0c"}
    ] | sort_by(.at) | renumber)
  else . end)

The probe's target and revision are P10's world.production (the address its probe names and the
revision it reports), as the run's curl stand-in records a probe it answers. Each diff read names the
run's workspace as its source, as diff-recorder.ts names a checkout root.

teardown-effects.sh --controls --cases on this file replays the same four traces as on the example.
The replay makes no action of a carried kind and promises no control for one (teardownEffectsPromised),
so it must write the four replay.json and effects.json with verdict ok, each daemon call answered as
its trace records, at the seq above. controls/ holds the records of the example, not of this file.
