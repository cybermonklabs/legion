# Fixtures of the step-models comparator's tests

These files are the inputs of `scripts/e2e/step-models-check.sh` in its tests, which read them in `scripts/e2e/lib/testdata/step-models/`. The sections "Which test row reads which file" and "Rows on the captured files" below name each row and the files it reads.

Every transcript is cut down to the fields its section names, and holds no message text. No file holds a path of this machine, a key or a private name. Each issue key is `LEGION-<n>`, and each claim is `legion-legion-legion-<n>-<role>`, except in the captured files. Those keep the keys and claims of the test that captured them.

The clock was read with `date -u` at 2026-11-23T20:59:25Z before the first capture, and at 2026-11-23T21:03:19Z before the `dispatch steps` capture.

## Files with a real source

| File | Shape | Source | Command that captured it | What was changed |
|---|---|---|---|---|
| `omp-session-fallback.jsonl` | An Oh My Pi session file (version 3, written by Oh My Pi 18.18.6, the version of the pin `18.18.6-sami.20261122-091943` that `.omp-pin` named then. Main now pins `18.18.6-sami.20261123-024918`, and between the two tags no file of the RPC mode changed) | A session of a Legion worktree under `~/.omp/agent/sessions/`, file `2026-11-19T23-59-52-466Z_01a1f640-….jsonl`, sha256 `522743cfaacd2be7…`, 483 lines | `jq -c '<reduction>' <session file>` over lines 1 to 3 and 51 to 135. The filter keeps the `type` and `timestamp` of each entry. For `message` it keeps `message.role`, and for an assistant message also `provider`, `model` and `stopReason`. For `model_change` it keeps `model`, `role` and `resolvedModelIsFallback`. For `model_usage` it keeps `purpose`, `provider`, `model` and `stopReason`. For `session` it keeps `version`. | The two providers of a private model gateway are renamed `gateway-openai` and `gateway`. Every other value is as captured. The two old names are kept out of this repository. |
| `printed-steps-main.txt` | `dispatch steps` as main prints it: a header and 12 words a line | The deployed Dispatch, one finished issue that Legion ran | `dispatch steps --issue <the issue>` | The issue's key and its claims are renamed with `sed` to `LEGION-3550`, `legion-legion-legion-3550-<role>` and `legion-legion-legion-455-architect`. The file's sha256 equals that pipeline's output (`68713666cb27cab1…`). The issue and the `sed` command are kept out of this repository. |
| `claude-stream-background-turn.jsonl` | Claude Code's `stream-json` output: an `init` line with the set model, and seven assistant lines of three responses | `packages/daemon/internal/claudebridge/testdata/background-turn.jsonl` at main `7cb1f229367`, a recorded Claude Code stream (`bridge_test.go`, the `replay <file>` mode). The command that recorded it is not in the repository. | `git show origin/main:packages/daemon/internal/claudebridge/testdata/background-turn.jsonl \| jq -c '<reduction>'`. The filter keeps the `type` of each line. For an assistant line it keeps `message.id`, `message.role`, `message.model`, `parent_tool_use_id` and `timestamp`. For an `init` line it keeps `subtype` and `model`. For a user line it keeps `parent_tool_use_id` and `timestamp`. For any other line it keeps `subtype`. | Nothing beyond the cut. |
| `claude-stream-refusal-turn.jsonl` | The same shape, with a line whose model is `<synthetic>` | `packages/daemon/internal/claudebridge/testdata/refusal-turn.jsonl` at main `7cb1f229367`, recorded against a stand-in endpoint (its ids are `msg_standin` and `req_standin`) | The same command and filter, with `message.stop_reason` kept | Nothing beyond the cut. |
| `daemon-log-envelope.jsonl` | The daemon's log: one JSON object a line, written by `slog.NewJSONHandler`, the handler `legion start` uses (`packages/daemon/cmd/legion/main.go`, `start`) | Lines 15 to 19 of the verbose output of a whole-daemon test, which printed the daemon's lines. The saved output is kept out of this repository. | `go test ./internal/daemon -run '^TestARestartedDaemonRelaunchesAPlanRolesAgentWhoseLaunchRanAnotherDefinition$' -v` in `packages/daemon`, at commit `cd8f8524f091a367cd537d7f8b138c11c0496d39` | The test's indentation is taken off. |

What the real files settle:

- An Oh My Pi assistant entry carries `timestamp`, an ISO time with milliseconds, written when the response ended. In the captured session it is 2 ms after the message's own `completedAt` (entries 52 and 118 of the source).
- An Oh My Pi session holds `model_usage` entries. These are upkeep calls with a `purpose` such as `title`, and they name models that never answered the conversation. The window holds 14 of them, over 14 model names: some never answered (`anthropic/claude-opus-5-5`), and some are the two models that did answer (`gateway-openai/openai/gpt-6.1-sol`, `gateway/openai/gpt-6.1-sol`). They count for no model. A reader that counts them fails rows 1 and 9 below.
- A response that ended in an error is an assistant entry with `stopReason: "error"`. In this session it is the last answer of the set model before Oh My Pi took its fallback (`model_change` with `role: "fallback"`).
- A Claude Code assistant line carries `timestamp`, an ISO time with milliseconds (`refusal-turn.jsonl` line 2, `background-turn.jsonl` lines 4 to 6). The lines of one response share `message.id` and each has its own time.
- `dispatch steps` prints a start with at most six fraction digits and trailing zeros cut (`18:44:47.53359Z`), and an end with nine digits (`23:38:54.063900605Z`).
- The daemon's JSON log writes `time` with nine fraction digits and trailing zeros cut, and numbers and booleans as JSON values.

## Files made for the comparator: MADE

Each file in this section is **made**, not captured. These shapes did not exist until the change was built. Each file uses the real values above. This copy in the repository keeps this section and these marks. The section "Files captured from the whole-chain test in CI" holds a real capture of each shape, and each made file stays a made case.

| File | Shape | How it was made | Where the real one comes from |
|---|---|---|---|
| `printed-steps-16.txt` (**made**) | `dispatch steps` of this release: 16 words a line, ending in `set <model or -> answered <value>` | The first 12 words follow `printed-steps-main.txt`. The last four follow the printer's rule (`packages/contracts/src/step-answers.ts`). The times and counts match `omp-session-fallback.jsonl`, the two Claude Code streams and `daemon-log-reads.jsonl`. | `printed-steps-16-captured-A.txt`, the real output of issue A of the whole-chain test. Row 17 of the comparator's tests also renders each record through the real `dispatch_steps` printer (`executeDispatchTool`), as `step-records-read.test.ts` renders `dispatch plans`. |
| `printed-steps-16-b.txt` (**made**) | The same shape for a second issue, `LEGION-3551`: its tester's records C1b and C3b, under the claim `legion-legion-legion-3551-tester` | C1b has C1's task, place, kind and start, so a log join that leaves out the issue finds C1's lines for it too. C1b's own end read is earlier, at 09:01:45, and it answers 2 calls where C1 answers 3. C3b starts at 09:01:46, so it matches no line of the first issue. | `printed-steps-16-captured-B.txt`, the real output of issue B of the whole-chain test. |
| `daemon-log-reads.jsonl` (**made**) | The daemon's log line of a read, `workflow: a step used a read of its agent's calls`, in the envelope of `daemon-log-envelope.jsonl`. Each end line carries `endedBy`. It also holds the lines of C1b and C3b, under issue `LEGION-3551`, after all lines of `LEGION-3550`. | The keys of that line, with `counts` as a JSON array of `{model, calls}` | `daemon-log-captured.jsonl`, the read lines of issues A and B in the daemon's log of the whole-chain test. |
| `agents.json` (**made**) | The comparator's list of agents: each claim's harness, transcript file and the model its settings give | By hand, from the definition of `--agents` in `scripts/e2e/README.md`. It names the implementer and the tester of `LEGION-3550`, and the tester of `LEGION-3551`. | The person who runs the check writes it from the deployment's settings. The whole-chain test writes its own from the settings it gave its panes: `agents-captured.json`. |
| `printed-steps-16-c.txt` (**made**) | The same shape as `printed-steps-16.txt`, for a third issue, `LEGION-3552` | Its implementer's attempts D1 to D10, its tester's attempt T1, and its architect's acceptance S1. The acceptance line follows `printed-steps-main.txt`: `5 step acceptance review`. The times and counts match the three transcripts below and `daemon-log-reads-completion.jsonl`. Row 17 does not render this file. | `printed-steps-16-captured-A.txt` and `printed-steps-16-captured-B.txt` hold its shape. No run holds an acceptance yet. |
| `daemon-log-reads-completion.jsonl` (**made**) | The same shape as `daemon-log-reads.jsonl` | The read lines of `LEGION-3552`'s records, each launch's `counts` cumulative. It also holds the start read of the implementer's step after D10, whose record no row prints. | The same as `daemon-log-reads.jsonl`. |
| `omp-session-completion.jsonl` (**made**) | The cut shape of `omp-session-fallback.jsonl`, with each message's `content` blocks cut to `type`, `name` and, on a `legion` call, `arguments.op` | `LEGION-3552`'s implementer, one minute for each of the attempts D1 to D10 | `omp-session-chain-A-implementer.jsonl` and `omp-session-chain-B-implementer.jsonl`, the sessions of the whole chain's evidence. |
| `omp-session-sign-off.jsonl` (**made**) | The same shape | `LEGION-3552`'s architect. S1's last response before its end read calls `legion` with `sign_off`, and an earlier one calls it with `read_record`. | No run holds an acceptance yet. |
| `claude-transcript-completion.jsonl` (**made**) | A Claude Code transcript's lines, cut to `type`, `message.id`, `message.role`, `message.model`, `message.content`, `isSidechain` and `timestamp` | `LEGION-3552`'s tester. Its first response has a text line and then a call line. Its third response has the call on its first line and text on its second. That call is to `mcp__legion__legion` with `handoff_complete`. | `claude-session-chain-A-tester.jsonl` and `claude-session-chain-B-tester.jsonl`, the transcripts of the whole chain's evidence. |

The records of `printed-steps-16.txt` are other cases over one transcript, so some overlap in time. Each test row prints only the records it names.

## Shapes with no real source on this box

1. **The Claude Code session transcript** (`<CLAUDE_CONFIG_DIR>/projects/<dir>/<session>.jsonl`), which `c-real` names as the Claude Code input. This box holds no Claude Code transcript: `~/.claude/projects` does not exist. The repository's `packages/daemon/internal/runtime/tmux/testdata/claude-transcript.jsonl` is made by hand (its ids are `msg_01` and `aaaaaaaa-…`, its model `claude-test`), so it is no capture. The whole-chain test captured real ones in CI, which the next section names.
2. **A Claude Code subagent's assistant line**, in either form. No capture on this box holds one. The whole-chain test captured both forms in CI. The bridge's `packages/daemon/internal/claudebridge/testdata/subagent-turn.jsonl` is now that capture's whole `stream-json` output.

## Files captured from the whole-chain test in CI

Each file in this section comes from run 45566065020 of this repository's `ci.yaml`, attempt 1, at commit `068d9d4242e18a6c154509d0a3c64731df2a4720`. Its job `step-models-chain` ran this command in `packages/daemon`:

```
go test -tags e2e -count=1 -timeout 40m -v -run '^(TestStepModelsLive|TestTheStandInGivesEachResponseItsOwnID)$' ./internal/daemon/
```

The test wrote its evidence to one directory, and the job uploaded it as the artifact `step-models-evidence-1`. GitHub records the artifact at 2026-11-25T07:19:33Z. It was read with `gh run download 45566065020 --name step-models-evidence-1 --dir <dir>`. Every model endpoint of the run was a stand-in of the test's own, so no file holds a model credential.

The test ran two issues of its own, A (`SM7DBA47-1`) and B (`SM7DBA47-2`). Then it ran one capture attempt, `capture-differ`: the pinned Claude Code 2.1.311 alone, in print mode. Its stand-in served `claude-served-model` for every response, where Claude Code asked for `claude-model`. The attempt wrote its result line, so the second attempt, `capture-same`, did not run. Claude Code exited 0. Its stderr held one line, `[claude-code:unrecognized_model] {"model":"claude-model","query_source":"sdk"}`.

| File | From the evidence file | How it was taken |
|---|---|---|
| `printed-steps-16-captured-A.txt` | `dispatch-steps-A.txt`: what `dispatch steps --issue SM7DBA47-1` printed | Copied as it is |
| `printed-steps-16-captured-B.txt` | `dispatch-steps-B.txt`: the same for `SM7DBA47-2` | Copied as it is |
| `daemon-log-captured.jsonl` | `daemon.jsonl`, the daemon's JSON log | Its 14 read lines, of both issues, copied as they are |
| `omp-session-chain-A-implementer.jsonl` | `transcript-legion-sm7dba47-sm7dba47-1-implementer.jsonl`, the session of A's implementer | The Oh My Pi filter below |
| `claude-session-chain-A-tester.jsonl` | `transcript-legion-sm7dba47-sm7dba47-1-tester.jsonl`, the transcript of A's tester | The Claude Code filter below |
| `omp-session-chain-B-implementer.jsonl` | `transcript-legion-sm7dba47-sm7dba47-2-implementer.jsonl`, the session of B's implementer | The Oh My Pi filter |
| `claude-session-chain-B-tester.jsonl` | `transcript-legion-sm7dba47-sm7dba47-2-tester.jsonl`, the transcript of B's tester | The Claude Code filter |
| `agents-captured.json` | `agents.json`, which the test wrote from the settings it gave its panes | Each `transcript` renamed to its file here |
| `compare-captured.txt` | `compare.txt`, the comparator's whole output for both issues at that commit | Copied as it is. That comparator answered `c-real: UNKNOWN`. Each step that its agent ended had its completion's response within 2 s of its end read. |
| `claude-session-capture-differ.jsonl` | `capture-differ-projects/<dir>/<session>.jsonl`, the session transcript of `capture-differ` | The Claude Code filter |
| `claude-session-capture-differ-subagent.jsonl` | `capture-differ-projects/<dir>/<session>/subagents/agent-<id>.jsonl`, the transcript of its one subagent | The Claude Code filter |
| `capture-differ-served.json` | The same name: the stand-in's list of each response it served, with the model asked for and the model served | Copied as it is. Its `turn` field quotes the start of what each request sent. |

The bridge's `subagent-turn.jsonl` is `capture-differ-stream.jsonl`, the attempt's stdout, copied whole.

The read lines were taken with jq 1.7:

```
jq -R -r 'select((try fromjson catch null | objects | .msg) == "workflow: a step used a read of its agent'\''s calls")' daemon.jsonl
```

Each transcript was cut with `jq -c -f <filter> <file>`, jq 1.7. The filters keep what the comparator reads, as the cuts of the files with a real source do. Beyond those cuts, they keep each content block's `type` and `name`, and the `op` of a `legion` call. The op tells the call that completes a step from the tool's other calls. The Claude Code filter also keeps `isSidechain` and `agentId`, which mark a subagent's line in a transcript. It keeps `requestedModel` too, the model Claude Code asked for.

The Oh My Pi filter:

```jq
def keep($names): . as $o | reduce $names[] as $k ({}; if $o | has($k) then . + {($k): $o[$k]} else . end);
def block: keep(["type", "name"]) + (if .type == "toolCall" and .name == "legion" then {arguments: {op: .arguments.op}} else {} end);
keep(["type", "timestamp"])
+ if .type == "message" then
    {message: (.message | keep(["role"])
      + if .role == "assistant" then keep(["provider", "model", "stopReason"]) + {content: [.content[]? | block]} else {} end)}
  elif .type == "model_change" then keep(["model", "role", "resolvedModelIsFallback"])
  elif .type == "model_usage" then keep(["purpose", "provider", "model", "stopReason"])
  elif .type == "session" then keep(["version"])
  else {} end
```

The Claude Code filter:

```jq
def keep($names): . as $o | reduce $names[] as $k ({}; if $o | has($k) then . + {($k): $o[$k]} else . end);
def block: keep(["type", "name"]) + (if .type == "tool_use" and .name == "mcp__legion__legion" then {input: {op: .input.op}} else {} end);
keep(["type"])
+ if .type == "assistant" then
    {message: (.message | keep(["id", "role", "model"]) + {content: [.content[]? | block]})}
    + keep(["parent_tool_use_id", "isSidechain", "agentId", "requestedModel", "timestamp"])
  elif .type == "system" and .subtype == "init" then keep(["subtype", "model"])
  elif .type == "user" then keep(["parent_tool_use_id", "isSidechain", "agentId", "timestamp"])
  else keep(["subtype", "isSidechain", "agentId"]) end
```

What the captures settle:

- A Claude Code session transcript holds no `system` `init` line. Each assistant line names the model Claude Code asked for, as `requestedModel`. Where the set model never answered, no other kept key names it. The uncut transcript also names it in a `model` attachment and in the `params.model` of each `api-request` line.
- A transcript writes a subagent's lines to a file of their own, under `<session>/subagents/`. Each line there has the subagent's `agentId`. Its user, attachment and assistant lines also have `isSidechain: true`. The main transcript holds none of them.
- The `stream-json` output marks a subagent's assistant line with a string `parent_tool_use_id`, beside `agent_id`.
- In both forms, the lines of one response share its `message.id`. `msg_standin_81` has a text line, then a line of a `Bash` call.
- The stream holds a second `init` line and a second `result` line. Claude Code started that turn by itself when its background subagent ended.

### Rows on the captured files

| Row | Input | Expected | Does the input hold what the verdict needs? |
|---|---|---|---|
| a captured Claude Code session counts each main response once and no subagent response | `claude-session-capture-differ.jsonl`, its subagent's file and `capture-differ-served.json` | `transcriptResponses` gives one counted response of `anthropic/claude-served-model` for each of the list's 4 main ids. It gives none for the subagent's file. | Yes. The transcript holds the list's 4 main ids, `msg_standin_81` on two lines. The subagent's file holds the list's one subagent id, marked `isSidechain`. |
| a captured session holds its set model | `claude-session-capture-differ.jsonl` and `capture-differ-served.json` | `heldModels` holds `anthropic/claude-model`, which no response of the transcript names | Yes. The list shows that each main response asked for `claude-model` and was served `claude-served-model`. Each assistant line's `requestedModel` is `claude-model`. |
| the chain's captured records of both issues pass both comparisons | `printed-steps-16-captured-A.txt` and `printed-steps-16-captured-B.txt` as each issue's records, `daemon-log-captured.jsonl`, and `agents-captured.json` with the four chain transcripts | exit 0. `c-real: PASS` takes the second steps of A's implementer and A's tester, 3 calls each. `live-at-both-reads: PASS`, with the two `PASS` lines of `compare-captured.txt`. | Yes. A's implementer's and tester's second steps end by their agents, with both reads of one launch. Each end read leaves out its completion's response, 0.442 s and 0.156 s before it. Every other gap of those reads is over 3 s. A's implementer's third step and B's tester's second step end by `TaskStopped`. The log holds the read lines of all four, and the agents file names all four claims. |

## Which test row reads which file

Each row is a test of `scripts/e2e/lib/step-models-check.test.ts`. Every row prints both verdict lines, `c-real: …` and `live-at-both-reads: …`, and exits with the `c-real` verdict's code. The fake `dispatch` answers each `--issue` with that issue's records. The names are these records:

- A1 to A8: implementer attempts 1 to 8 of `LEGION-3550` (Oh My Pi, `omp-session-fallback.jsonl`). A1 to A5 end by their agent. A6 ends by `TaskStopped`. A7 and A8 have no read lines.
- C1 to C3: tester attempts 1 to 3 of `LEGION-3550` (Claude Code). C1 and C3 read the background stream, C2 the refusal stream. C1 and C2 end by their agent, C3 by `TaskStopped`.
- A0: the merge of `LEGION-3550`, which names no model.
- C1b and C3b: the tester of `LEGION-3551` (`printed-steps-16-b.txt`). C1b starts as C1 does, but its end read is earlier and it answers 2 calls. C3b starts at 09:01:46 and answers 1.
- D1 to D10: implementer attempts 1 to 10 of `LEGION-3552` (Oh My Pi, `omp-session-completion.jsonl`, `printed-steps-16-c.txt`). Each ends by its agent, except D7, which ends by `TaskStopped`. D8's last response calls `legion` with `ready_for_update`, and D9's agent calls `handoff_complete` twice.
- T1: tester attempt 1 of `LEGION-3552` (Claude Code, `claude-transcript-completion.jsonl`), ended by its agent.
- S1: the acceptance of `LEGION-3552` by its architect (Oh My Pi, `omp-session-sign-off.jsonl`), ended by its agent's `sign_off`.

The last column answers one question for each row: does the row's input hold what its expected verdict needs?

| Row | Records printed | Log | Agents | Expected | Does the input hold what the verdict needs? |
|---|---|---|---|---|---|
| 1. an Oh My Pi step that its agent ended equals its transcript | A1, A0 | `daemon-log-reads.jsonl` | the implementer | exit 0. `c-real: PASS` with `gateway-openai/openai/gpt-6.1-sol=4,gateway/openai/gpt-6.1-sol=2`. `live-at-both-reads: UNKNOWN`. A0 needs no transcript. | Yes. A1 ends by its agent, has both reads 3.044 s and 3.346 s from a response, and its counts equal the transcript's. Each model it names is held. No stopped step is printed, so the second verdict is `UNKNOWN`. |
| 2. a Claude Code step counts each response once by its id | C1 | the same | the tester, with `claude-stream-background-turn.jsonl` | exit 0, `c-real: PASS` with `anthropic/claude-sonnet-4-5=3` | Yes. C1 ends by its agent, its reads are 6.349 s and 2.418 s from a response, and the stream holds 3 ids between them. |
| 3. a synthetic response counts for no model | C2 | the same | the tester, with `claude-stream-refusal-turn.jsonl` | exit 0, `c-real: PASS` with `anthropic/claude-sonnet-4-5=1` | Yes. C2 ends by its agent, and its reads are 8.016 s and 6.974 s from a response. The stream holds one counted response and one synthetic one between them. |
| 4. both comparisons on both harnesses | A1, A6, C1, C3, A0 | the same | `agents.json` | exit 0. `c-real: PASS` takes A1 and C1. `live-at-both-reads: PASS` takes A6 and C3. | Yes. The agents file names two harnesses. Each has a step that its agent ended and that qualifies (A1, C1). Each also has a stopped step that qualifies (A6 at 6.955 s and 9.637 s, C3 at 9.916 s and 7.417 s). |
| 5. a record whose set model is not the settings' fails | A1 | the same | the implementer, with `setModel` `gateway/openai/gpt-6.1-sol` | exit 1, the `FAIL` names both models | Yes. A1 is taken, and its set model differs from that `setModel`. |
| 6. a record that counts no call fails | A2 | the same | the implementer | exit 1, the `FAIL` says the counts total 0 | Yes. A2 ends by its agent and qualifies at 3.437 s and 4.238 s, and its counts are `none`. |
| 7. a step with a response 1.236 s from its end read is not taken | A3 | the same | the implementer | exit 2, `c-real: UNKNOWN: took no step of harness omp`, and a `GAP:` line for A3. That line names 1.236 s before its end read and says that no response holds the completion call. | Yes. A3 is the only Oh My Pi step, and its end read is 1.236 s after a response. The cut file holds no `content`, so no completion call. |
| 8. a step whose end read is final totals is not taken | A5 | the same | the implementer | exit 2, a `GAP:` line for A5 that says its end read is final totals | Yes. A5 is the only Oh My Pi step, and its end line is final. |
| 9. an answering model that only a skipped record names fails | A1, A4 | the same | the implementer | exit 1, the `FAIL` names A4 and `anthropic/claude-opus-5-5` as an answering model. Only `model_usage` entries of the transcript name it. | Yes. A1 qualifies, so only the model sentence can fail. A4 answers `anthropic/claude-opus-5-5`, which no response or model change holds. |
| 10. a set model the transcript does not hold fails | A1, A7 | the same | the implementer | exit 1, the `FAIL` names A7 and `anthropic/claude-opus-5-5` as its set model | Yes. A7's set model is held by no entry that holds a model. |
| 11. a last model the transcript does not hold fails | A1, A8 | the same | the implementer | exit 1, the `FAIL` names A8 and `anthropic/claude-opus-4-8` as its last model | Yes. A8's last model is held by no entry that holds a model. |
| 12. a log with no read line exits 2 | A1 | `daemon-log-envelope.jsonl` | the implementer | exit 2, `UNKNOWN` names the missing read lines | Yes. That log holds no read line. |
| 13. a record of an agent with no transcript exits 2 | A1, C1 | `daemon-log-reads.jsonl` | the implementer alone | exit 2, `UNKNOWN: no transcript of legion-legion-legion-3550-tester` | Yes. C1 names models, and its agent is not in the agents given. |
| 14. an answer of the release before exits 2 | all of `printed-steps-main.txt` | `daemon-log-reads.jsonl` | `agents.json` | exit 2, `UNKNOWN` names the first line that has 12 words | Yes. Every line of that file has 12 words. |
| 15. `c-real` takes no step that another fact ended | A6 | the same | the implementer | exit 2. `c-real: UNKNOWN: took no step of harness omp`, with the line `saw no step that its agent ended`. `live-at-both-reads: PASS` with `gateway-openai/openai/gpt-6.1-sol=2`. | Yes. A6 is the only record, it ends by `TaskStopped`, and it qualifies for the second comparison. |
| 16. `--lower-one` fails the steps that pass (the negative control) | row 4 with `--lower-one` | the same | `agents.json` | exit 1, both verdicts `FAIL` | Yes. Row 4's input takes a step in each comparison, so each has a count to lower. |
| 17. the printer renders `printed-steps-16.txt` from its records | typed records built from each line of `printed-steps-16.txt`, rendered by the real `dispatch_steps` command | none | none | the rendered text equals the file | Yes. The printer of 16 words is part of this release. |
| 18. two issues each answer their own records | `LEGION-3550`: A1, A6, A0. `LEGION-3551`: C1b, C3b | the same | `agents.json` | exit 0. `c-real: PASS` takes A1 and C1b with `anthropic/claude-sonnet-4-5=2`. `live-at-both-reads: PASS` takes A6 and C3b. | Yes. Each issue answers its own records, and the log holds the read lines of both issues. C1b's own lines bracket 2 responses, as its record says. The agents file names all three claims, so each harness has a qualifying step of each kind. |
| Control of row 18: a join that leaves out the issue | row 18's input, against a comparator whose join leaves out the issue and takes the first matching line in log order | the same | `agents.json` | exit 1. For C1b it reads C1's lines of `LEGION-3550`, which bracket 3 responses, where C1b's record says 2. | Yes. C1's lines come first in the log and match C1b's task, place, kind and start, and their window holds 3 responses, not 2. |
| 19. an Oh My Pi step counts its completion's response, 0.050 s before its end read | D1 | `daemon-log-reads-completion.jsonl` | `LEGION-3552`'s implementer | exit 0, `c-real: PASS` with `gateway-openai/openai/gpt-6.1-sol=3`. Its `GAP:` line names the response it left out, 0.050 s before the end read. | Yes. Its third response calls `legion` with `handoff_complete` 0.050 s before its end read. The next ends 3.000 s after it, and the start read is 3.000 s from any response. |
| 20. a Claude Code step counts its completion's response once by its id | T1 | the same | `LEGION-3552`'s tester | exit 0, `c-real: PASS` with `anthropic/claude-sonnet-4-5=3` | Yes. Its third response has two lines. The first calls `mcp__legion__legion` with `handoff_complete`, and the second, 0.050 s before the end read, gives the response its time. The transcript holds 3 ids between the reads. |
| 21. another response 1.500 s before the end read refuses the step | D2 | the same | the same | exit 2. D2's `GAP:` line leaves out its completion's response, names 1.500 s before its end read, and says why it is not taken. | Yes. D2's third response, a `legion` call with `read_record`, ends 1.500 s before the end read. Its fourth, the completion's, ends 0.050 s before it. |
| 22. a response within 2 s after the end read or of the start read refuses the step | D3, D4, D5 | the same | the same | exit 2. D3 is not taken for its end read, and D4 and D5 for their start reads. | Yes. D3's next response ends 1.000 s after its end read. D4's first ends 1.000 s after its start read. D5's only response is its completion's, 1.000 s after its start read. |
| 23. the completion's response is found by its call where it ends after the end read | D6 | the same | the same | exit 0, `c-real: PASS` with `gateway-openai/openai/gpt-6.1-sol=3`. Its `GAP:` line says the response it left out ended 0.080 s after the end read. | Yes. D6's completion's response is the first response after its end read, 0.080 s after it, as an agent's clock that runs ahead writes it. Two responses lie between its reads. |
| 24. a step that another fact ended gets no exception at its end read | D7 | the same | the same | exit 2. Both verdicts `UNKNOWN`. | Yes. D7 ends by `TaskStopped`. Its response that calls `legion` with `handoff_complete` ends 0.050 s before its end read. |
| 25. a step whose transcript holds no completion call keeps the plain 2 s rule | A1 | `daemon-log-reads.jsonl` | the implementer | exit 0, `c-real: PASS` with row 1's counts. A1's `GAP:` line says that no response holds the completion call. | Yes. The cut file holds no `content`, so the end read leaves out no response. Both of A1's reads are more than 2 s from any response. |
| 26. a legion call of another op is no completion call | D8 | `daemon-log-reads-completion.jsonl` | `LEGION-3552`'s implementer | exit 2. D8's `GAP:` line says that no response holds the completion call, and names 0.050 s before its end read. | Yes. D8's last response before its end read calls `legion` with `ready_for_update`. |
| 27. an acceptance step that its architect ended with `sign_off` counts that response | S1 | the same | `LEGION-3552`'s architect | exit 0, `c-real: PASS` with `gateway-openai/openai/gpt-6.1-sol=3` | Yes. S1's third response calls `legion` with `sign_off` 0.050 s before its end read. Its second calls it with `read_record` 4.000 s before it. |
| 28. the step's completion call is the last one between its reads | D9 | `daemon-log-reads-completion.jsonl` | `LEGION-3552`'s implementer | exit 0, `c-real: PASS` with `gateway-openai/openai/gpt-6.1-sol=4`. Its `GAP:` line leaves out the response 0.050 s before the end read. | Yes. D9's second response calls `legion` with `handoff_complete` 5.000 s before the end read, and its fourth calls it again 0.050 s before it. Its third ends 3.000 s before the end read. |
| 29. a call after the end read and after the claim's next read is no completion call | D10 | the same | the same | exit 2. D10's `GAP:` line says that no response holds the completion call, and names 0.080 s after its end read. | Yes. D10's third response calls `legion` with `handoff_complete` 0.080 s after its end read. The claim's next read comes 0.040 s after the end read, before that call. |

Arithmetic each row rests on, read from the files with Python. It was checked again after the records A6 to A8, C3, C1b and C3b were added, and again after D1 to D10, T1 and S1. A gap counts every response of the agent's own conversation, error and synthetic ones too. At the end read of a step that its agent ended, the gaps leave out the response of its completion call.

- Between A1's two reads the implementer's transcript holds 4 responses of `gateway-openai/openai/gpt-6.1-sol` and 2 of `gateway/openai/gpt-6.1-sol`. It holds none between A2's reads, 2 and 1 between A3's, and 2 of `gateway-openai/openai/gpt-6.1-sol` between A6's.
- The background stream holds 3 response ids between C1's reads and 2 between C1b's. It holds 1 between C3's reads and 1 between C3b's. The refusal stream holds 1 counted response between C2's.
- A3's end read is 1.236 s after the last response before it, and A4's is 1.136 s. Every other read of rows 1 to 18 is more than 2 s from any response.
- Each record's read lines give the same counts as the transcript, except A4's, which names `anthropic/claude-opus-5-5` on purpose.
- `LEGION-3552`'s implementer: D1 counts 3 between its reads, D2 4, D3 3, D4 3 and D5 1. D6 counts 2, with its completion's response after the end read, D8 3, D9 4 and D10 2. Its completion's response ends 0.050 s before the end read in D1 to D5, D7 and D9, and 0.080 s after it in D6. The start reads of D2 to D10 come 47.000, 47.000, 49.000, 47.000, 55.950, 46.920, 47.000, 47.000 and 47.000 s after the last response. D1's has none before it.
- `LEGION-3552`'s tester holds 3 ids between T1's reads, and its architect 3 responses between S1's. Each start read is 3.000 s before the next response, and each end read 3.000 s before the response after the completion's.
