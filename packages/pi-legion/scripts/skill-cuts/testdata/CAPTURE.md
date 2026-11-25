# How each fixture was captured

Every fixture here came from a real system on 2026-11-20, with made-up skills (`alpha`, `beta`,
`tool-loop-alpha`) and made-up prompts, but for these: F10, F11 and F15 are GitHub's answers, a
pull request's body and a pull request's diff of real repositories, read on 2026-11-22, with the
names their sections give replaced; F16 is files of the public dotfiles repository read on
2026-11-25, as its section says; and F12, F13, F14, the style texts and the control skills are
made up, read from no system, in the shapes their sections give.
A test that reads a fixture cites its section of this file in its header. The scan at the end says
how each file was checked for keys and private names.

Versions: Oh My Pi 18.18.6 (`/opt/omp/bin/omp`), Claude Code 2.1.311, Bun 1.4.4, gh 2.98.0. `jq` in
the commands below is the pod's `jq`, which is jaq 3.1.1.

`$R` is the scratch directory `/legion/scratch/tmp/acme-1/t1-runs` and `$TOOLS` is
`/legion/scratch/tmp/acme-1/t1-tools`, both scratch outside the repository ("The scratch
directory's name" says why each fixture writes `acme-1` there). Every Oh My Pi and Claude Code run
below starts from `env -i`, so none of the pod's own Oh My Pi settings (its profile, its config
overlays, its session store) reaches the run.

## Words replaced in the Oh My Pi fixtures

The pod's public push check refuses a line that holds the model provider's name together with one
of the words customer, work order, deliver (in any of its forms), contract or invoice, in any case.
Oh My Pi's own system prompt, tool texts and job notices use some of those words on lines that also
carry the provider's name (a message's `provider` and `api` fields, or the model id). So in each
line of a fixture here that holds the provider's name, each such word was replaced by a neutral
word of the same grammar, in the same case, and every other byte was kept:

| Word | Replaced by |
| --- | --- |
| deliver, delivers | arrive, arrives |
| delivered, delivering | handed back, handing back |
| delivery, deliveries | handover, handovers |
| contract, contracts | agreement, agreements |
| customer, customers | client, clients |
| invoice, invoices | bill, bills |
| work order, work orders | job ticket, job tickets |

The replacement was the word replacement of "The scratch tools" (`fixturewords.ts --write
<file>...`) over every file here but the F7 text files and `F1b.jsonl`, run after the captures
below; then F1b was made again from the changed F1 with its command below. 33 words were replaced,
all in five files (no customer, invoice or work order occurred):

| File | Words replaced |
| --- | --- |
| `F1/…-59f275bdff80.jsonl` | 3: deliver 1, Delivery 1, delivered 1 |
| `F1/…-59f275bdff80/ProductiveChicken.jsonl` | 5: contract 2, contracts 1, Delivery 1, deliver 1 |
| `F1b.jsonl` (made again from F1) | 3: deliver 1, Delivery 1, delivered 1 |
| `F2-request.json` | 11: contract 3, Contract 1, contracts 1, deliver 3, Delivery 1, delivery 1, delivered 1 |
| `F2-request-thinking-low.json` | 11: the same as `F2-request.json` |

So these files differ from what Oh My Pi wrote and sent in those words, and in the scratch
directory's name (next section), only: F1's `system-prompt-digest` is the digest of the prompt
before the change, and the F5 bodies were made from F2 before it.

## The scratch directory's name

The scratch directories of these captures (`$R`, `$TOOLS`, `$O`, and Claude Code's install
directory) sat in one directory whose name was the issue key of the work that made them. Every
fixture here, and this file, writes `acme-1` in its place, F4's directory name too. The
replacement, from `packages/pi-legion/scripts/skill-cuts`, with `KEY` that name (left out of this
file):

```sh
mv "testdata/F4/projects/-legion-scratch-tmp-$KEY-t1-runs-f3-work" \
  testdata/F4/projects/-legion-scratch-tmp-acme-1-t1-runs-f3-work
rg -l --null "$KEY" testdata -g '!CAPTURE.md' | xargs -0 sed -i "s/$KEY/acme-1/g"
```

Claude Code had cut F4's three `lastPrompt` values (lines 19, 30 and 39) short inside that name,
so the `sed` did not match them; each was rewritten by hand to end in `/legion/scratch/tmp/acme…`.
Then every F7 file was captured again with the commands its section names, since F4's path hash,
the files' byte counts and the pull's second offset follow from the replaced bytes. The values
computed from the bytes before the replacement are kept as recorded: F1's `system-prompt-digest`,
the `shapeHash`, `hash` and `tail` digests Claude Code wrote in F4, and each headers file's
`content-length`. Every other byte is as captured, but for the words of the section above.

## The scratch tools

These lived in `$TOOLS` and are described here by what they do; the runner's recorder and replay
hook are built from them. The scan and the word replacement are in the repository, beside this
directory, and run from `packages/pi-legion`.

- **recorder** (`bun recorder.ts <port> <out dir> <reply.sse>...`): a stand-in Anthropic Messages
  endpoint on `127.0.0.1`. It stores each POST body byte for byte as `<out>/request-<n>.json`, and
  the method, path and every request header except the auth headers (any header named
  `authorization`, `cookie`, or holding `key`, `token`, `secret` or `auth`) as
  `<out>/request-<n>.headers.json`. It answers the n-th request with the n-th reply file (the last
  one again once the list runs out) as `text/event-stream`. It holds no key and reaches no host.
- **end-turn reply** (`end-turn.sse`): an Anthropic Messages SSE stream of one text block,
  `Recorded.`, with `stop_reason: end_turn`.
- **tool-use reply** (`tool-use-bash.sse`): the same stream shape holding one `tool_use` block, a
  `bash` call whose command is `touch $R/f2/marker-the-block-must-stop`, with
  `stop_reason: tool_use`.
- **replay hook** (`replay-hook.ts`): `REPLAY_MESSAGES` names a JSON file holding an array of Oh
  My Pi message objects. Its `context` handler returns that array in place of the run's own first
  message (the placeholder prompt), followed by every message the run itself added after it. Its
  `tool_call` handler returns `{block: true, reason: "blocked by the replay hook: no tool runs in a
  capture"}` for every call. Each event is logged as one JSON line to `REPLAY_LOG`.
  `REPLAY_CONTROL_NO_BLOCK=1` leaves the `tool_call` handler out (the control run below only).
- **recording proxy** (`ANTHROPIC_BASE_URL=<gateway> bun proxy.ts <port> <out dir>`): listens on
  `127.0.0.1` and forwards each request to the real gateway at `$ANTHROPIC_BASE_URL`. It drops the
  caller's auth headers and signs the forwarded request with the gateway key, which it reads from
  the pod's key file inside its own process and never prints or stores. It stores each request
  body and its headers less the auth ones as the recorder does, with the reply's status, and
  streams the reply back less the gateway's `x-litellm-*` headers.
- **send** (`bun send.ts <body.json> [<recorded request headers.json>]`): posts the body to
  `$ANTHROPIC_BASE_URL/v1/messages` as a plain Messages call (the reply streams because the body
  sets `stream`), with `anthropic-version: 2023-06-01`, the recorded request's `anthropic-beta`
  header when a headers file is given, and the key read from the key file inside the script.
  stdout is the reply body as received; the first stderr line is the status and the kept response
  headers (`content-type`, `request-id`, and `llm_provider-request-id`, the gateway's copy of the
  same id); the second stderr line lists only the names of the headers left out.
- **scan** (`bun scripts/skill-cuts/fixturescan.ts --key-file <file> --terms <file> <dir>`): the
  check in "The scan" below.
- **word replacement** (`bun scripts/skill-cuts/fixturewords.ts [--write] <file>...`): in each
  line that holds the model provider's name (any case), replaces each word of the table in "Words
  replaced in the Oh My Pi fixtures" with its neutral word, in the word's case, keeping every other
  byte. A word right after a JSON escape (`\n`, `\u2014`) counts as a word, as it does once the
  JSON is decoded. It prints, per file, the count of each word it replaced; without `--write` it
  changes nothing. Run over every file here, it replaces none now.

## F1: an Oh My Pi session directory (`F1/`)

The scratch profile `fixture` under the scratch HOME `$R/f1/home` held `agent/models.yml`, a copy
of the pod profile's `models.yml` (its anthropic provider is the gateway, and its `apiKey` line is
a command that reads the key file, so the copy carries no key), and two made-up skills:
`agent/skills/alpha/SKILL.md`, `agent/skills/alpha/references/x.md` and
`agent/skills/beta/SKILL.md`. Their text is in F1's read results, after each `[Skill file: <path>]`
header.

```sh
cd $R/f1/work && env -i HOME=$R/f1/home PATH=/usr/local/bin:/usr/bin:/bin TMPDIR=$R PI_NO_TITLE=1 \
  /opt/omp/bin/omp --profile fixture -p --session-dir $R/f1/sessions \
  --model anthropic/claude-sonnet-5-5 --smol anthropic/claude-sonnet-5-5 \
  --slow anthropic/claude-sonnet-5-5 --thinking off --max-time 5m \
  "This is a test of skill reads in a made-up fixture project. Do these steps in order. Make exactly one tool call per message and wait for its result before the next step. Step 1: read skill://alpha with the read tool. Step 2: read skill://alpha/references/x.md with the read tool. Step 3: read skill://gamma with the read tool (that skill may not exist; a failed read is expected, do not retry it). Step 4: call the task tool once, with one task whose assignment is: read skill://beta with the read tool, then reply with the greeting line it gives. Do not read skill://beta yourself. Then reply with the single word DONE."
cp -a $R/f1/sessions/. F1/
```

The directory is as Oh My Pi wrote it, but for the words of "Words replaced in the Oh My Pi
fixtures" and the scratch directory's name: the session file
`2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl` (reads of `skill://alpha`,
of `skill://alpha/references/x.md`, and of `skill://gamma`, whose result is the error
`Unknown skill: gamma`; then a `task` call and a `wait`), and under the directory of the same
name less `.jsonl` the subagent's transcript `ProductiveChicken.jsonl` (its one read, of
`skill://beta`) and its output `ProductiveChicken.md`. No other path was rewritten: the scratch
paths name neither the pod nor a person.

## F1b: F1's session with its `alpha` read copied a day later (`F1b.jsonl`)

From this directory. The `alpha` read's assistant line and its `toolResult` line are copied to the
end of the file, each with its line `timestamp` and its `message.timestamp` moved 24 hours later.
Every other byte is F1's (jaq writes each copied line back byte for byte but the two timestamps).

```sh
S=F1/2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl
{ cat "$S"; jq -c -n '[inputs] as $l | ($l[] | select(.type == "message" and .message.role == "assistant" and any(.message.content[]; .type == "toolCall" and .name == "read" and .arguments.path == "skill://alpha"))) as $call | ($call.message.content[] | select(.type == "toolCall") | .id) as $id | ($call, ($l[] | select(.type == "message" and .message.role == "toolResult" and .message.toolCallId == $id))) | .timestamp |= ((capture("^(?<s>[^.]+)(?<f>[.][0-9]+Z)$")) as $t | ($t.s + "Z" | fromdateiso8601 + 86400 | todateiso8601 | sub("Z$"; "")) + $t.f) | .message.timestamp += 86400000' "$S"; } > F1b.jsonl
```

## F2: the request body of a replayed history (`F2-request*.json`)

The same scratch HOME and profile as F1, with `agent/models.yml` replaced by one that points the
anthropic provider at the recorder and holds no key:

```yaml
providers:
  anthropic:
    baseUrl: http://127.0.0.1:18431
    auth: apiKey
    api: anthropic-messages
    apiKey: placeholder-recorder-holds-no-key
    compat:
      replayUnsignedThinking: false
      supportsLongCacheRetention: true
    models:
      - {id: claude-sonnet-5-5, name: Claude Sonnet 5.5, reasoning: true, input: [text, image], contextWindow: 1000000, maxTokens: 128000, cost: {input: 2, output: 10, cacheRead: 0.1, cacheWrite: 2.5}}
```

The replayed history (a user message, the assistant's `read` of `skill://alpha`, and F1's own
`[Skill file: …]` result for it), made from F1:

```sh
jq -n --slurpfile l F1/2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl '($l | map(select(.type == "message" and .message.role == "toolResult" and .message.toolName == "read")) | .[0].message) as $r | [ {role: "user", content: [{type: "text", text: "Write the alpha checklist of the made-up fixture project. Use the alpha skill."}], attribution: "user", timestamp: 1795139983655}, {role: "assistant", content: [{type: "toolCall", id: "toolu_replay_alpha_1", name: "read", arguments: {path: "skill://alpha"}}], api: "anthropic-messages", provider: "anthropic", model: "claude-sonnet-5-5", usage: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0}}, stopReason: "toolUse", timestamp: 1795139984111}, {role: "toolResult", toolCallId: "toolu_replay_alpha_1", toolName: "read", content: $r.content, isError: false, timestamp: 1795139988974} ]' > $R/f2/history.json
```

The capture, one run at each of two thinking levels against one recorder:

```sh
bun $TOOLS/recorder.ts 18431 $R/f2/rec-d $TOOLS/end-turn.sse &
cd $R/f1/work && for t in off low; do env -i HOME=$R/f1/home PATH=/usr/local/bin:/usr/bin:/bin TMPDIR=$R PI_NO_TITLE=1 \
  REPLAY_MESSAGES=$R/f2/history.json REPLAY_LOG=$R/f2/hook-d.log \
  /opt/omp/bin/omp --profile fixture -p --mode json --no-session --model anthropic/claude-sonnet-5-5 \
  --thinking $t --hook $TOOLS/replay-hook.ts "placeholder prompt"; done
cp $R/f2/rec-d/request-1.json F2-request.json
cp $R/f2/rec-d/request-1.headers.json F2-request.headers.json
cp $R/f2/rec-d/request-2.json F2-request-thinking-low.json
cp $R/f2/rec-d/request-2.headers.json F2-request-thinking-low.headers.json
```

`F2-request.json` is `--thinking off` (`"thinking": {"type": "between_tools"}`);
`F2-request-thinking-low.json` is `--thinking low` (adaptive thinking, `output_config.effort: low`,
`context_management`, and two more `anthropic-beta` values in its headers file). Their `system`,
`tools` and `messages` are the same. Each body's `messages` are the hook's three messages (Oh My Pi
puts its date and working-directory reminder before the first user text), its system prompt lists
the installed skills `alpha` and `beta` under `<skills>`, and the placeholder prompt is in neither.
The bodies are kept byte for byte as Oh My Pi sent them, but for the words of "Words replaced in
the Oh My Pi fixtures" (biome does not read this directory).

The hook's `tool_call` block was proven with two more runs of the same command at
`--thinking off`, against a recorder whose replies were `tool-use-bash.sse` then `end-turn.sse`:
with the hook as it is, the `bash` call ended as an error result holding the hook's reason and the
marker file was not made; with `REPLAY_CONTROL_NO_BLOCK=1`, the same call ran and made it. Neither
run's output is a fixture.

## F3 and F4: a Claude Code run that calls `Skill` and `Read` (`F3-*`, `F4/`)

Claude Code was installed under the scratch directory only:
`npm install --prefix /legion/scratch/tmp/acme-1/claude-code @anthropic-ai/claude-code@2.1.311`.
`$F` is `$R/f3`, and `claude` is `/legion/scratch/tmp/acme-1/claude-code/node_modules/.bin/claude`.

A made-up local marketplace `$F/marketplace`: `.claude-plugin/marketplace.json` names the
marketplace `acme-market` and its one plugin `acme-tools` at `./plugins/acme-tools`;
`plugins/acme-tools/.claude-plugin/plugin.json` gives `{"name": "acme-tools", "version": "0.1.0"}`
with a description and author; `plugins/acme-tools/skills/alpha/SKILL.md` and
`plugins/acme-tools/skills/beta/SKILL.md` are F1's two skills. It was installed into the scratch
config:

```sh
E="env -i HOME=$F/home PATH=/usr/local/bin:/usr/bin:/bin TMPDIR=$F CLAUDE_CONFIG_DIR=$F/config DISABLE_AUTOUPDATER=1 DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"
cd $F/work && $E claude plugin marketplace add $F/marketplace && $E claude plugin install acme-tools@acme-market
```

The run, through the recording proxy, so the gateway's model really answered. The command's
environment also set ANTHROPIC_API_KEY to a placeholder string, which never reached the gateway:
the proxy replaced it with the real key on each forwarded request.

```sh
ANTHROPIC_BASE_URL=<the gateway> bun $TOOLS/proxy.ts 18432 $F/proxy &
cd $F/work && env -i HOME=$F/home PATH=/usr/local/bin:/usr/bin:/bin TMPDIR=$F CLAUDE_CONFIG_DIR=$F/config \
  DISABLE_AUTOUPDATER=1 DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  ANTHROPIC_BASE_URL=http://127.0.0.1:18432 \
  ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-sonnet-5-5 ANTHROPIC_SMALL_FAST_MODEL=claude-sonnet-5-5 \
  claude -p --allowedTools Skill,Read --output-format stream-json --verbose --model claude-sonnet-5-5 \
  "This is a test of skill loading in a made-up fixture project. Step 1: call the Skill tool once to load the acme-tools:alpha skill. Step 2: call the Read tool once on the file $F/config/plugins/cache/acme-market/acme-tools/0.1.0/skills/beta/SKILL.md. Make exactly one tool call per message and wait for its result. Then reply with the single word DONE." > $F/stream.jsonl
cp $F/proxy/request-1.json F3-request-first.json
cp $F/proxy/request-1.headers.json F3-request-first.headers.json
cp $F/proxy/request-3.json F3-request-last.json
cp $F/proxy/request-3.headers.json F3-request-last.headers.json
cp $F/stream.jsonl F3-stream.jsonl
mkdir -p F4/projects && cp -a $F/config/projects/-legion-scratch-tmp-acme-1-t1-runs-f3-work F4/projects/
```

The run made three requests (`POST /v1/messages?beta=true`, each answered 200): the first holds
the task and the skill listing (`acme-tools:alpha`, `acme-tools:beta`); the second (not kept) adds
the `Skill` call with its result; the third holds both calls, the `Skill` call's injected skill
text and the `Read` result. `F3-stream.jsonl` is the run's stdout. `F4/projects/<dir>/<id>.jsonl`
is the transcript as Claude Code wrote it under `$F/config/projects/`; the empty `memory/`
directory beside it is left out.

## F5: the gateway's replies to bodies made from F2 (`F5-*.sse`, `F5-*.headers.json`)

Each body is F2 as captured, before the word replacement above, with one text block added to its
last user message (the one holding the `tool_result`):

```sh
jq --arg t "<added text>" '.messages[-1].content += [{type: "text", text: $t}]' <F2 body> > $R/f5/body-<shape>.json
bun $TOOLS/send.ts $R/f5/body-<shape>.json <that F2 body's headers file> > $R/f5/reply-<reply>.sse 2> $R/f5/reply-<reply>.err
cp $R/f5/reply-<reply>.sse F5-<reply>.sse
head -1 $R/f5/reply-<reply>.err | jq . > F5-<reply>.headers.json
```

In the order sent (one call each; each shape came back on its first send):

| Reply | F2 body | Added text | What came back |
| --- | --- | --- | --- |
| `F5-skill-read` | `F2-request.json` | `Now read skill://alpha/references/x.md with the read tool. Make that one tool call and no other.` | one `read` of `skill://alpha/references/x.md`; `cache_creation_input_tokens` 4836 |
| `F5-skill-read-and-other` | `F2-request.json` | `Now make two tool calls in this one message: read skill://beta with the read tool, and run the command ls with the bash tool.` | a text block, then `read` of `skill://beta`, `bash` with `ls`, and `read` of `skill://alpha/references/x.md` |
| `F5-text` | `F2-request.json` | `Now reply with the alpha checklist as three short lines. Call no tool.` | one text block, `end_turn` |
| `F5-thinking` | `F2-request-thinking-low.json` | `Think the three alpha steps through before you answer, then reply with the alpha checklist as three short lines. Call no tool.` | a `thinking` block with its signature, then a text block |
| `F5-cache-read` | `F2-request.json` | none: `body-skill-read.json`, the body of `F5-skill-read`, sent again | one `read`; `cache_read_input_tokens` 12912, `cache_creation_input_tokens` 0 |

`F5-judge` is the judge reply, captured once `prompts/judge.md` existed. Its body
is the one the runner's judge builds (`judgeBody` of `judge.ts`) with `prompts/judge.md` as it
was then (git blob `14175de667`): before its `<step>` line gained the sentence "When the agent
first read files of its instructions, the step begins with those read calls, whose results are
not shown." So the fixture answers that older prompt, not the one this commit holds; no test
rebuilds the body (judge.test.ts reads the reply alone), and its inputs below (`$R`, `$TOOLS`)
are gone, so it was not captured again. The body is for a case made from F2's replayed history
(`$R/f2/history.json`, made by the command of F2 above): the criterion "Pass: The step reads
references/x.md (a read of skill://alpha/references/x.md) before it writes any alpha step or its
result." and "Fail: The step
writes the alpha steps or their results without reading references/x.md first.", the line
"1. Read \`references/x.md\` before the first step.", the history as the messages before the step
(its read result of `skill://alpha` left out by the judge), and `F5-text`'s reply as the step. A
scratch script, `judge-body.ts`, printed that body with those inputs, from a scratch directory `$D`
outside the repository, and it was sent as the others were:

```sh
bun $D/judge-body.ts $R/f2/history.json F5-text.sse > $D/body-judge.json
bun $TOOLS/send.ts $D/body-judge.json > $D/reply-judge.sse 2> $D/reply-judge.err
cp $D/reply-judge.sse F5-judge.sse
head -1 $D/reply-judge.err | jq . > F5-judge.headers.json
```

It came back on its first send: a `thinking` block, then the text
`{"verdict": "fail", "reason": "The step makes no tool call to read …"}`, `end_turn`;
`cache_creation_input_tokens` 710, all of them a one-hour cache write.

## F6: GitHub's answers for the lister, and made-up manifests (`F6-*`)

From the public cybermonklabs/legion only, with read-only `gh` calls. This repository is public,
so the people GitHub's answers name are replaced: the commit's author (a bot account named after a
person), the person in its `Co-authored-by` trailer, the fork parent's owner, and, inside file
contents, a person's full name and the handle that is also the npm scope of the packages (it
becomes `acme`, so `@acme/pi-legion`). The commands take the two values as `--arg` and leave them
out of this file. Edited contents keep GitHub's `sha` (the blob id of the original) and get the new
`size`; the commit's signature is dropped (`verification` set to unsigned) since it signed the
original message.

```sh
gh repo list cybermonklabs --limit 2000 --json nameWithOwner,isFork,isArchived,defaultBranchRef,parent \
  | jq '(.[] | select(.parent != null) | .parent.owner) |= {id: "U_upstream_owner", login: "upstream-owner"}' > F6-repo-list.json
gh api "repos/cybermonklabs/legion/commits?sha=main&until=2026-11-20T00:00:00Z&per_page=1" \
  | jq --arg handle "$handle" 'map(.commit.author |= (.name = "acme-agent[bot]" | .email = "acme-agent[bot]@users.noreply.github.com") | .commit.message |= gsub("Co-authored-by: [^\n]*"; "Co-authored-by: Example Owner <owner@example.com>") | .commit.verification = {verified: false, reason: "unsigned", signature: null, payload: null, verified_at: null} | .author |= (.login = "acme-agent[bot]" | .id = 0 | .node_id = "BOT_example" | .avatar_url = "https://avatars.githubusercontent.com/u/0?v=4" | with_entries(if (.value | type) == "string" then .value |= gsub($handle + "-agent"; "acme-agent") else . end)))' > F6-commits.json
gh api "repos/cybermonklabs/legion/git/trees/d7c7c531438468c93b8b77038ace48315c01bd59?recursive=1" \
  | jq '{sha, url, tree: [.tree[] | select(.path as $p | ($p | startswith("skills/")) or ($p | startswith("packages/pi-legion/agents/")) or (["packages/pi-legion/package.json", "packages/pi-envoy/package.json", "scripts/pi-plugin-prepack.sh", ".claude-plugin/marketplace.json", "packages/claude-envoy/scripts/build.ts", "packages/daemon/docker/omp-plugins/package.json"] | any(. == $p)))], truncated}' > F6-tree.json
gh api -X GET search/code -f q='"legion-worker" repo:cybermonklabs/legion' > F6-search-code.json
ref=d7c7c531438468c93b8b77038ace48315c01bd59
while read -r path name; do
  gh api "repos/cybermonklabs/legion/contents/$path?ref=$ref" \
    | jq --arg person "$person" --arg handle "$handle" '.content |= (gsub("\n"; "") | @base64d | gsub($person; "Example Owner") | gsub($handle; "acme") | @base64 | [range(0; length; 60) as $i | .[$i:$i+60]] | join("\n") + "\n") | .size = (.content | gsub("\n"; "") | @base64d | utf8bytelength)' > "F6-contents-$name.json"
done <<'EOF'
packages/pi-legion/package.json pi-legion-package
packages/pi-envoy/package.json pi-envoy-package
scripts/pi-plugin-prepack.sh pi-plugin-prepack
.claude-plugin/marketplace.json marketplace
packages/claude-envoy/scripts/build.ts claude-envoy-build
packages/daemon/docker/omp-plugins/package.json omp-plugins-package
skills/legion-worker/SKILL.md legion-worker-skill
packages/pi-legion/agents/thermonuclear-deep-review.md thermonuclear-deep-review-agent
EOF
```

What came back: both repositories of the owner are public (cybermonklabs/legion, and
cybermonklabs/oh-my-pi, a fork), so no row was dropped. `main` at the end time is
`63b3a613feb15cb191f87f4239467cdd37b62c82`, a commit after `d7c7c531`. The tree at `d7c7c531`
holds 14,265 entries with `truncated: false`; the cut keeps 105 (78 blobs, 27 trees). The search
answered `total_count: 325`, `incomplete_results: false`, 30 items on its first page. Without the
replacements, the base64 round trip leaves each `content` byte for byte as GitHub sent it. The
agent definition `packages/pi-legion/agents/thermonuclear-deep-review.md` is read as one example
of the `autoloadSkills` manifest kind beside the manifest files above.

Made-up files, each in the shape of a real file of its kind that no public repository of this owner
holds, under `F6-madeup/` as one made-up repository's tree:

| File | The real file kind it follows |
| --- | --- |
| `plugins/acme-tools/.claude-plugin/plugin.json` | a Claude Code plugin manifest (`name`, `version`, `description`, `author`, `skills`) |
| `.claude/settings.json` | a Claude Code user settings file: `enabledPlugins` (`<plugin>@<marketplace>`: true or false) and `extraKnownMarketplaces` with a GitHub source |
| `omp/config.yml` | an Oh My Pi `config.yml` whose `skills.ignoredSkills` leaves skills out, as a dotfiles repository keeps it |
| `legion/projects/10-ACME.yaml` | a Legion project file, top-level keys with a `verificationSkillsLeaveOut` flow list of plugin names |
| `installers/skills.sh` | a dotfiles installer script that links each skill directory into `~/.claude/skills` and `~/.omp/agent/skills` |
| `tree/init.py` | a Python loader whose default TOML template names each role's skills in `[roles.<role>] skills = [...]`, `tool-loop-alpha` among them |

## F1c, F1d and F7: the psql captures of the read count and the models query (`F1c.jsonl`, `F1d.jsonl`, `F7-*`)

`F1c.jsonl` is F1's subagent transcript with its `model_change` line taken out, so its model comes
from its `session_init` line (`grep -v '"type":"model_change"' <F1 subagent file> > F1c.jsonl`).
`F1d.jsonl` is F1b's first two lines, its title and session lines, so its head holds no model line
(`head -n 2 F1b.jsonl > F1d.jsonl`).
The `F7-*.txt` files are psql's output under the store command's flags for `count.sql`,
`models.sql` and the queries of the pull, the tasks and the sessions, on fixture databases that
`fixturedb.ts` loads from F1, F1b, F1c, F1d and F4. Their capture commands are in the headers of
`psql-rows.test.ts` (the count, the pull, the tasks and sessions queries and the edge shapes) and
`count.test.ts` (the models query, run next in the same shell). They run as written from
`packages/pi-legion/scripts/skill-cuts`, in a shell where `LEGION_TEST_PG_DSN` is the test
server's URL, and make, fill and drop databases of their own on it; every F7 file here was
captured with them. `count.test.ts` checks each query's rows against its capture.

## F8 and F9: Oh My Pi's Responses body for `openai/gpt-6.1-sol`, and the gateway's replies (`F8-*`, `F9-*`)

`$O` is the scratch directory `/legion/scratch/tmp/acme-1/openai-route.ByGhxu`, outside the
repository, which held two scratch scripts:

- **measure** (`bun $O/measure.ts <out dir> <thinking>`, run from `packages/pi-legion`): a stand-in
  on `127.0.0.1` that stores each request's body byte for byte as `<out>/request-<n>.json`, and
  its method, path and headers less those recorder.ts's `keptHeaders` leaves out (every auth
  header, and every header naming a key, token, secret, team or user), with the names of the left
  out ones, as `<out>/request-<n>.headers.json`; it answers 404 to every request. It writes the
  scratch profile `fixture` under `$O/home` (F1's skills `alpha` and `beta` copied in; a
  `models.yml` of the pod profile's `openai` provider only, pointed at the stand-in's `/v1`, with a
  placeholder key, `compat.statefulResponses: false` and `modelOverrides: {gpt-6.1-sol:
  {contextWindow: 922000}}`; a `config.yml` whose default and smol roles are
  `openai/gpt-6.1-sol`), and runs `omp --profile fixture -p --mode json --no-session --model
  openai/gpt-6.1-sol --thinking <thinking> --hook replay-hook.ts "placeholder prompt"` with only
  `HOME`, `PATH`, `TMPDIR`, `TZ`, `PI_NO_TITLE` and the hook's `SKILL_CUTS_MESSAGES` (the history)
  in its environment.
- **send-openai** (`bun $O/send-openai.ts <body.json>`): posts the body to
  `$ANTHROPIC_BASE_URL/v1/responses` with `authorization: Bearer <key>`, the key read from the key
  file inside the script and printed nowhere. stdout is the reply body as received; the first
  stderr line is the status and the kept response headers (`content-type`, `request-id`,
  `llm_provider-x-request-id`, `llm_provider-openai-processing-ms`, `llm_provider-openai-version`);
  the second lists only the names of the headers left out (the other `llm_provider-*` headers, the
  organisation and project among them, the rate limits, the cookie, `date`, and the gateway's own).

The history is F2's (`$R/f2/history.json`) with its assistant message made the one an OpenAI
session holds, and its read result's path the scratch profile's:

```sh
jq --arg old $R/f1/home --arg new $O/home '(.[1] |= (.content[0].id = "call_replay_alpha_1|fc_replay_alpha_1" | .api = "openai-responses" | .provider = "openai" | .model = "gpt-6.1-sol")) | (.[2].toolCallId = "call_replay_alpha_1|fc_replay_alpha_1") | (.[2].content[0].text |= sub($old; $new))' $R/f2/history.json > $O/history.json
bun $O/measure.ts $O/rec-off off
cp $O/rec-off/request-4.json F8-request.json
jq '{method, path, headers}' $O/rec-off/request-4.headers.json > F8-request.headers.json
```

Oh My Pi ended with exit 1 after the 404s, having made four requests: `GET /v1/models`, two
`POST /v1/responses` hint requests of its smol role (one per skill), and the turn, `POST
/v1/responses` (request 4, F8). Each POST left out `authorization` and `user-agent`; F8's headers
file keeps `accept: text/event-stream`, `session_id` and `x-client-request-id` (the run's random
uuid), and Oh My Pi sent no `anthropic-*` header. F8's body is `model: "gpt-6.1-sol"`, the system
prompt as one `instructions` text (Oh My Pi joins its prompt's blocks with a blank line between),
the history as `input` items (a user message of two `input_text` parts, the date reminder and the
request; a `function_call` of `read` on `skill://alpha` whose `call_id` is the history's id before
its `|`, with no item id; a `function_call_output` holding the read result's text), 11 `tools` of
`{type: "function", name, description, parameters, strict: true}`, `stream: true`, `store:
false`, `prompt_cache_key` (the session's uuid), `stream_options: {include_obfuscation: false}`,
`max_output_tokens: 128000`, `reasoning: {effort: "low"}` (Oh My Pi's `--thinking off` on this
model) and `include: ["reasoning.encrypted_content"]`: no `cache_control` and no
`prompt_cache_options`. It is kept byte for byte; no line of it holds the provider name of "Words
replaced in the Oh My Pi fixtures", so no word was replaced.

F9's bodies are F8 with input items added, each sent once with send-openai, each answered 200:

```sh
jq -c --arg t "<added text>" '.input += [{role: "user", content: [{type: "input_text", text: $t}]}]' F8-request.json > $O/f9/body-<shape>.json
bun $O/send-openai.ts $O/f9/body-<shape>.json > $O/f9/reply-<reply>.sse 2> $O/f9/reply-<reply>.err
sed 's/"safety_identifier":"[^"]*"/"safety_identifier":"example-safety-identifier"/g' $O/f9/reply-<reply>.sse > F9-<reply>.sse
head -1 $O/f9/reply-<reply>.err | jq . > F9-<reply>.headers.json
```

| Reply | Body | What came back |
| --- | --- | --- |
| `F9-skill-read` | added text `Now read skill://alpha/references/x.md with the read tool. Make that one tool call and no other.` | one `function_call` of `read` on `skill://alpha/references/x.md`; input 7985 tokens, `cache_write_tokens` 7982 (all 30-minute), output 30 |
| `F9-skill-read-and-other` | added text `Now make two tool calls in this one message: read skill://beta with the read tool, and run the command ls with the bash tool.` | a `read` of `skill://beta` and a `bash` of `ls`; `cached_tokens` 7751, `cache_write_tokens` 237 |
| `F9-cache-read` | `body-skill-read.json` sent again | one `read`; `cached_tokens` 7982, `cache_write_tokens` 0 |
| `F9-text` | `body-skill-read.json` with F9-skill-read's output item, a `function_call_output` of x.md under its `[Skill file: …]` header, and the user text `Now reply with the alpha checklist as three short lines. Call no tool.` | a `reasoning` item with its `encrypted_content`, then a `message` of the three lines; `cached_tokens` 8083, `cache_write_tokens` 19, output 76, 43 of them reasoning |

`F9-text`'s body was made with:

```sh
X="[Skill file: $O/home/.omp/profiles/fixture/agent/skills/alpha/references/x.md]
$(cat $O/home/.omp/profiles/fixture/agent/skills/alpha/references/x.md)"
grep '^data: {' $O/f9/reply-skill-read.sse | sed 's/^data: //' | jq -c 'select(.type == "response.completed") | .response.output' > $O/f9/out-skill-read.json
jq -c --slurpfile out $O/f9/out-skill-read.json --arg x "$X" --arg t "Now reply with the alpha checklist as three short lines. Call no tool." '.input += $out[0] + [{type: "function_call_output", call_id: $out[0][0].call_id, output: $x}, {role: "user", content: [{type: "input_text", text: $t}]}]' $O/f9/body-skill-read.json > $O/f9/body-text.json
```

Three sends before it are not kept: two bodies of F8 with only a text ask (the model read
`references/x.md` first, as the skill says), and the body above less its last user text (an empty
message). The gateway took the reply's output items back as input with their ids.

Each reply is `data:` lines with no event names, `response.created` to `response.completed`, then
`data: [DONE]`. `response.completed` carries the whole response: its output items, its `usage`
(`input_tokens` counts the cached and written tokens among it, `output_tokens` the reasoning among
it, and `cost` is the gateway's own count, equal to gpt-6.1-sol's list price of the usage: 0.020261,
0.0022436, 0.0011042 and 0.0016218 in the table's order), `service_tier: "default"`, and the
request's `instructions` and `tools` echoed. The one value changed is each response's
`safety_identifier`, which the gateway derives from its caller: it is `example-safety-identifier`
in every event that holds it, and every other byte is as received.

Checked with `public_leak_check.py check-public-text` and `check-private-names.sh` (both clean),
and with the scan below, which counts the opaque values the API made as known kinds (the
Responses item ids, `item_id`, the `call_id`s, the reasoning item's `encrypted_content` and the
`llm_provider-x-request-id` header) and passes over the closing `data: [DONE]`, which is no JSON.
It finds no piece of the key, no key or token shape and no private name in them.

## F10: GitHub's answers for the pins of two skill sets (`F10-github.json`)

From public repositories only, with read-only `gh api` calls: a dotfiles repository that pins two
skill sets, the two sets' repositories, and, for a package that declares `omp.skills`, this
repository. This repository is public, so the commands take the real names as shell variables and
leave them out of this file, as F6 does with the people's names. Each variable is written below
with the made-up name that replaces it in the fixture:

| Variable | What it names | Made-up name |
| --- | --- | --- |
| `$DOTFILES` | the dotfiles repository | `acme/dotfiles` |
| `$SET_A` | the first set's repository | `acme/alpha-set` |
| `$PKG_A` | its package key in `omp/plugins/package.json` | `alpha-set` |
| `$SET_B` | the second set's repository, a fork | `acme/beta-set-plugin` |
| `$SET_B_UPSTREAM` | the repository it is a fork of | `acme-upstream/beta-set-plugin` |
| `$DIR_B` | its `ensure_vendor` directory | `beta-set` |
| `$DEP_1`, `$DEP_2` | two other dependencies of `omp/plugins/package.json` | `@acme/tools`, `knives` |
| `$DEP_2_REPO` | the repository `$DEP_2` is pinned to | `acme/knives` |
| `$STACK_REPO`, `$STACK` | an `ensure_vendor` line with no commit: its repository and directory | `acme/stack`, `stack` |
| `$OWN_POOL`, `$DEEP_POOL` | two more pools of `skills-sources.json` | `plugins/acme`, `vendor/delta/plugins/delta` |
| `$A1`, `$A2`, `$A3` | three skills of the first set | `alpha-skill-one`, `alpha-skill-two`, `alpha-skill-three` |
| `$B1`, `$B2`, `$B3` | three skills of the second set | `beta-skill-one`, `beta-skill-two`, `beta-skill-three` |
| `$B_GUIDE` | a skill of the second set's `.agents/skills/` | `beta-skill-work` |

The fixture is one JSON object. Each key is the route that sets.ts gives `gh api` (it sends each read
as `gh api [-H 'Accept: application/vnd.github.raw'] <route>`, the query in the route), and each
value is GitHub's answer to it: a file's text for a contents read, the JSON otherwise. The tests'
stand-in gh answers a route the object does not hold as GitHub answers a missing one (404). The
capture, from this directory, in a shell where each variable above holds its real value:

```sh
set -o pipefail
D=$(gh api "repos/$DOTFILES/commits/82e8f6dc" --jq .sha)
SA=$(gh api "repos/$SET_A/commits/2ce8aa15" --jq .sha)
SB=$(gh api "repos/$SET_B/commits/8a776c13" --jq .sha)
OP=$(gh api repos/cybermonklabs/legion/commits/dc7e6687 --jq .sha)
W1='since=2026-10-23T00:00:00Z&until=2026-11-20T00:00:00Z'
W2='since=2026-11-20T00:00:00Z&until=2026-11-22T00:00:00Z'
T=$(mktemp -d)
names="s#$DOTFILES#acme/dotfiles#g; s#$SET_A#acme/alpha-set#g; s#$SET_B_UPSTREAM#acme-upstream/beta-set-plugin#g; s#$SET_B#acme/beta-set-plugin#g; s#$DEP_2_REPO#acme/knives#g; s#$STACK_REPO#acme/stack#g; s#$STACK#stack#g; s#$DIR_B#beta-set#g"
skills="s#skills/$A1#skills/alpha-skill-one#g; s#skills/$A2#skills/alpha-skill-two#g; s#skills/$A3#skills/alpha-skill-three#g; s#skills/$B1#skills/beta-skill-one#g; s#skills/$B2#skills/beta-skill-two#g; s#skills/$B3#skills/beta-skill-three#g; s#skills/$B_GUIDE#skills/beta-skill-work#g; s#sample-plugin/skills/skill-one#sample-plugin/skills/beta-sample#g"
# An older pin of the second set (any 40-hex pin but $SB) keeps its first 12 hex digits.
short="/^ensure_vendor / { /$SB/! s#([0-9a-f]{12})[0-9a-f]{28}\$#\\1# }"
raw() { gh api -H 'Accept: application/vnd.github.raw' "repos/$1/contents/$2?ref=$3"; }
pkg() {
  raw "$DOTFILES" omp/plugins/package.json "$1" \
    | jq --arg a "$PKG_A" --arg d1 "$DEP_1" --arg d2 "$DEP_2" '.dependencies as $d | {name, private, dependencies: ({"@acme/tools": $d[$d1], knives: $d[$d2], "alpha-set": $d[$a]} | with_entries(select(.value != null)))}' \
    | sed "$names"
}
installer() {
  raw "$DOTFILES" installers/omp.sh "$1" \
    | grep -E "^#!|^source |^# Vendor checkouts|^ensure_vendor https://github.com/($SET_B|$STACK_REPO)\\.git " \
    | sed -E "$names; $short"
}
commits() {
  gh api "repos/$DOTFILES/commits?path=$1&$2&per_page=100&page=1" \
    | jq --arg keep "$3" '[.[] | select($keep == "" or (.sha[0:8] as $s | $keep | split(" ") | any(. == $s))) | {sha, commit: {committer: {date: .commit.committer.date}}, parents: [.parents[] | {sha}]}]'
}
text() { jq -n --arg k "$1" --rawfile v /dev/stdin '{($k): $v}'; }
json() { jq -c --arg k "$1" '{($k): .}'; }
tag=$(gh api "repos/$SET_A/git/ref/tags/v7.0.1" --jq .object.sha)
{
  pkg "$D" | text "repos/acme/dotfiles/contents/omp/plugins/package.json?ref=$D"
  installer "$D" | text "repos/acme/dotfiles/contents/installers/omp.sh?ref=$D"
  raw "$DOTFILES" skills-sources.json "$D" \
    | jq --arg own "$OWN_POOL" --arg stack "vendor/$STACK" --arg b "vendor/$DIR_B" --arg deep "$DEEP_POOL" '{sources: [.sources[] | select(.root | IN($own, $stack, $b, $deep)) | {root}]}' \
    | sed "s#\"$OWN_POOL\"#\"plugins/acme\"#; s#\"$DEEP_POOL\"#\"vendor/delta/plugins/delta\"#; $names" \
    | text "repos/acme/dotfiles/contents/skills-sources.json?ref=$D"
  gh api "repos/$SET_A/git/ref/tags/v7.0.1" | jq '{ref, object: {sha: .object.sha, type: .object.type}}' \
    | json repos/acme/alpha-set/git/ref/tags/v7.0.1
  gh api "repos/$SET_A/git/tags/$tag" | jq '{sha, tag, object: {sha: .object.sha, type: .object.type}}' \
    | json "repos/acme/alpha-set/git/tags/$tag"
  gh api "repos/$SET_A/git/ref/tags/v4.1.1" | jq '{ref, object: {sha: .object.sha, type: .object.type}}' \
    | json repos/acme/alpha-set/git/ref/tags/v4.1.1
  raw "$SET_A" package.json "$SA" | jq '{name, version, pi: {skills: .pi.skills}}' | sed "s#\"$PKG_A\"#\"alpha-set\"#" \
    | text "repos/acme/alpha-set/contents/package.json?ref=$SA"
  raw cybermonklabs/legion packages/pi-legion/package.json "$OP" | jq '{name: "gamma-set", version, omp: {skills: .omp.skills}}' \
    | text "repos/acme/gamma-set/contents/package.json?ref=$OP"
  gh api "repos/$SET_A/git/trees/$SA?recursive=1" \
    | jq --arg re "^(README.md|package.json|skills|skills/($A1|$A2|$A3)(/SKILL.md|/root-cause-tracing.md)?)\$" '{sha, tree: [.tree[] | select(.path | test($re)) | {path, mode, type, sha}], truncated}' \
    | sed "$skills" | json "repos/acme/alpha-set/git/trees/$SA?recursive=1"
  gh api "repos/$SET_B/git/trees/$SB?recursive=1" \
    | jq --arg re "^(package.json|skills|skills/($B1|$B2|$B3)(/SKILL.md|/references|/references/fix.md)?|.agents/skills/$B_GUIDE/SKILL.md|tests/fixtures/sample-plugin/skills/skill-one/SKILL.md)\$" '{sha, tree: [.tree[] | select(.path | test($re)) | {path, mode, type, sha}], truncated}' \
    | sed "$skills" | json "repos/acme/beta-set-plugin/git/trees/$SB?recursive=1"
  gh api "repos/$SET_A" | jq '{full_name, fork}' | sed "$names" | json repos/acme/alpha-set
  gh api "repos/$SET_B" | jq '{full_name, fork, parent: {full_name: .parent.full_name}, source: {full_name: .source.full_name}}' \
    | sed "$names" | json repos/acme/beta-set-plugin
  for w in "$W1" "$W2"; do
    if [ "$w" = "$W1" ]; then kp="311f0b3b dc0a7398 8c3c6cac"; ki="5218006e 371649f5 76135b6e c5f48792 1e99dfa8"; else kp=""; ki=""; fi
    commits omp/plugins/package.json "$w" "$kp" > "$T/pkg.json"
    commits installers/omp.sh "$w" "$ki" > "$T/sh.json"
    json "repos/acme/dotfiles/commits?path=omp/plugins/package.json&$w&per_page=100&page=1" < "$T/pkg.json"
    json "repos/acme/dotfiles/commits?path=installers/omp.sh&$w&per_page=100&page=1" < "$T/sh.json"
    for c in $(jq -r '.[] | .sha, .parents[0].sha' "$T/pkg.json"); do
      pkg "$c" | text "repos/acme/dotfiles/contents/omp/plugins/package.json?ref=$c"
    done
    for c in $(jq -r '.[] | .sha, .parents[0].sha' "$T/sh.json"); do
      installer "$c" | text "repos/acme/dotfiles/contents/installers/omp.sh?ref=$c"
    done
  done
} | jq -s 'add' > F10-github.json
rm -r "$T"
```

What came back, and what each cut keeps:

- At the dotfiles commit `82e8f6dc`, `omp/plugins/package.json` pins the first set by its tag
  `v7.0.1`; the cut keeps its `name`, `private` and three dependencies (`$DEP_1`, `$DEP_2`, a
  `github:` dependency of another repository, and `$PKG_A`). `installers/omp.sh` pins the second set
  by an `ensure_vendor` line with the commit `8a776c13`; the cut keeps its first line, its `source`
  line, one comment and two `ensure_vendor` lines, the second set's and `$STACK_REPO`'s, which names
  no commit. `skills-sources.json` is cut to the roots of four pools: `vendor/$DIR_B`, one at
  `vendor/$STACK`, one deeper in its vendor directory (`$DEEP_POOL`) and one outside `vendor/`.
- `git/ref/tags/v7.0.1` of the first set is an annotated tag (its object is a `tag`), whose tag
  object names the commit `2ce8aa15`; `git/ref/tags/v4.1.1` is a lightweight tag (its object is the
  commit `469a6d81`), and that commit holds no `package.json` (its contents read answered 404, so
  the object holds no answer for it: a package with no manifest). Each is cut to `ref`, `sha`, `tag`
  and `object`; the tag object's tagger, message and verification are left out.
- The first set's `package.json` at `2ce8aa15` declares `pi.skills` `./skills`; it is cut to its
  name, version and `pi.skills`. This repository's `packages/pi-legion/package.json` at `dc7e6687`
  declares `omp.skills` `dist/skills`; it is cut to its version and `omp.skills`, under the name
  `gamma-set`.
- The tree of the first set at `2ce8aa15` (300 entries) and of the second at `8a776c13` (2,061
  entries), both `truncated: false`, are cut to three skills of each and a few other entries: a
  skill's other files, and two `SKILL.md` files outside `skills/<name>/` (the second set's guide
  skill under `.agents/skills/` and a test fixture's skill). Each entry keeps its path, mode, type
  and sha.
- `repos/$SET_A` is no fork; `repos/$SET_B` is a fork of `$SET_B_UPSTREAM`. Each is cut to its full
  name, its fork flag and its parent's and source's full names.
- The commits of each manifest's path from 2026-10-23 to 2026-11-20: 51 of
  `omp/plugins/package.json`, cut to three (`dc0a7398`, which moved the first set from `v6.3.0` to
  `v7.0.1`, and two that changed other dependencies), and 29 of `installers/omp.sh`, cut to five
  (`1e99dfa8`, `c5f48792`, `76135b6e` and `371649f5`, which each moved the second set's pin, and
  `5218006e`, which did not). From 2026-11-20 to 2026-11-22, F7's window: two and three, all kept,
  none of which moves a pin. Each commit is cut to its sha, its committer's date and its parents'
  shas, and both manifests at each commit and at its first parent are cut as above.

The replacements are `names`, `skills` and the pools' roots in the script, each name by the made-up
name of the table. One more: an older pin of the second set, `b4adb2f0…` and `a1c1209c…` (40 hex
digits each), keeps its first 12 hex digits (`short`), since no answer here gives it as a `sha`
(the scan below accounts for a 40-hex run only where one does). Each text is the cut jq or grep
wrote, and every value in it is GitHub's but those names.

## F14: planted count rows (`F14-count-planted.txt`)

Made up, read from no system: count.sql's rows in `F7-count-first.txt`'s psql shape, of the store
`store_a`, for the sets of F10. Its summary row is that of the query of 2026-11-22, the day after
F7's window ends (`mtime_from` 1795305600000, `mtime_to` 1795392000000), as a count queries the
days after its window's end for the files written then; each count row is a read on 2026-11-21,
inside the window, of one file with a hash of its own. The rows: (a) `alpha-skill-one` by the
Skill call `alpha-set:alpha-skill-one`, and in that file also at `/opt/acme/fork/skills/`; (b)
`beta-skill-one` by `beta-set:beta-skill-one`; (c) `alpha-skill-two` under
`/srv/acme/.dotfiles/omp/plugins/node_modules/alpha-set/skills/`; (d) `beta-skill-one` under
`/srv/acme/.dotfiles/vendor/beta-set/skills/`, with a `reference` read of its `references/fix.md`
in the same file; (e) `beta-skill-two` at `/opt/acme/plugin/dist/skills/`; (f) `no-such-skill` at
`skill://no-such-skill`; a read of `alpha-skill-three` in Claude Code's plugin cache
(`/srv/acme/.claude/plugins/cache/acme-plugins/alpha-set/5.0.7/skills/`); a read of
`alpha-skill-two` at `skill://alpha-skill-two`, with no path; and a read of `beta-skill-two` at the
upstream's raw GitHub URL. A scratch script wrote the file: each row as psql prints count.sql's
JSON (`"key" : value`, in count.sql's key order, `calls` after `hashes` and `unpaired_calls` after
`unpaired_hashes`, as in `F7-count-first.txt`), under a header `total` centred over a rule of
dashes as wide as the widest row, and the footer `(12 rows)`. Each count row's `calls` holds the
row's one file with a call key of the row's own, `f14c000000000001` to `f14c00000000000b` in the
file's row order (so (a)'s file holds two keys, one in each of its rows, and so does (d)'s), and
its `unpaired_calls` is `{}`: no row holds an unpaired call.

## F12: a craft list, and GitHub's answers for its lines (`F12-*`)

Made up, read from no system, in the shape of the craft list a person writes (`craftlist.ts`):
two made-up sets, `alpha-set` (`acme/alpha-set`) and `beta-set` (`acme/beta-set-plugin`), each at
the commit F10 reads it at (`2ce8aa15…` and `8a776c13…`, which F10 gives as `sha` values, so the
scan accounts for them), and three properties: C1 and C3 checked by a reader, each with an
example line, and C2 checked by a behaviour test, with a record link. `F12-craft-list.json` is the
valid list. `F12-contents.json` is one JSON object: each key is the route sets.ts's
`SetReads.text` gives `gh api -H 'Accept: application/vnd.github.raw'` for a file the list cites
(`repos/<owner>/<repo>/contents/<path>?ref=<commit>`), and each value the file's text, made up;
the tests' stand-in gh answers a route it does not hold as GitHub answers a missing file (404).

## F13: two skills trees and the craft entries (`F13-tree.json`, `F13-entries.jsonl`)

Made up, read from no system. `F13-tree.json` is one JSON object of a path to the text of a file:
a `skills/` tree (`alpha-one`, `alpha-two`, the vendored `vendored-tool`, a SKILL.md nested inside
`alpha-one`, a directory with no SKILL.md and a README) and a `plugins/x/skills` tree (`beta-one`,
`beta-two`, `vendored-tool`). `coverage.test.ts` writes it into a temporary directory, so this
repository holds no SKILL.md of a made-up skill: `copies` lists every SKILL.md of a repository,
and the repository's code owners review every SKILL.md. `F13-entries.jsonl` holds lines in the
shape of `craft`'s entries, cut to the fields `craft-coverage` reads (`entry`, `skill.dir`,
`skill.source`, `answer` with its gaps): alpha-one and alpha-two of the source
`acme/skills at 1a2b3c4d`, beta-one of `acme/dotfiles at 5e6f7a8b` with every reply refused, and
beta-two of `acme/skills at 1a2b3c4d`; and a proposal's entry, which `craft-coverage` passes over.

## The style texts (`style-numbered.md`, `style-check.md`)

Made up in the shapes of the two style texts the craft reader reads, so `readers.test.ts` counts
their rule ids as it would count the originals': `style-numbered.md` follows the style section of
a dotfiles skill (a title, a `**Done when:**` line, a heading over a table of 5 parts, then
`## Rules` with 10 numbered rules, each with an indented `- Example:` and `- Source:`), and
`style-check.md` the version 1 draft of a style research note's Part 4 (a
`**Order of a SKILL.md** (…):` label over a table of 7 parts whose separator row is `|---|---|`,
`**Rules:**` with 10 bullets, and `**Check before you commit:**` with 3, "No paragraph holds more
than one rule" the second). The rules keep the originals' wording where it names nothing; each
set's name, each cited skill and line, and each source id are made up (`alpha-set`, `beta-set`,
`as`, `bs`, `alpha-skill-one`, `N1`…). Read by `styleRules` outside this repository, the two
originals give 15 and 20 ids, as these do, with the paragraph rule `S19`.

## The control skills (`control-skills.json`)

Made up, read from no system: one JSON object of a path to the text of a file, which
`cli.test.ts` writes into a temporary directory (for the reason F13 gives). `tidy-settled-change`
has the line layout of the skill that the craft reader's first two controls change: its
`## Step 4: Verify` runs from line 41 to line 46, after Step 3 (lines 29 to 39) and before Step 5's
heading (line 47) and summary (line 49); it also holds a LICENSE and one reference file.
`tracker-first` has the layout of the skill that the other two change: three one-line list items
at lines 37 to 39 under a heading at line 35. `check-before-claim` is a nearest skill of theirs.

## F11: a pull request body (`F11-body.md`)

The body of the dotfiles repository's pull request #105, at its head `ac1b11e8`, as it was read on
2026-11-22 (its last edit then, of 01:46:58 UTC; the body has been edited since, after line 323),
and the head, read with

```sh
gh api graphql -f query='{ repository(owner: "<owner>", name: "dotfiles") { pullRequest(number: 105) { userContentEdits(first: 50) { nodes { editedAt diff } } } } }' \
  --template '{{range .data.repository.pullRequest.userContentEdits.nodes}}{{if eq .editedAt "2026-11-22T01:46:58Z"}}{{.diff}}{{end}}{{end}}' \
  > pr105-body.md
gh pr view 105 --repo <owner>/dotfiles --json headRefOid --jq .headRefOid
```

(the body, 63,246 bytes, is what `gh pr view 105 --repo <owner>/dotfiles --json body --jq .body`
gave then). Cut to: the heading `## Your choices` (line 23 of the
body); the heading `### Open proposals of #41 and #75 (R5)` (line 44) and its first paragraph, less
its last sentence (three links to comments of #41); each list's line and the blank lines around
it, as the body has them; of the 45 items of `Lines this pull request can carry`, `41-1.1`
(`open`), `41-3.2` (`partly on main`), `41-10.5` (`open`) and `75-1` (`open; no ruling found`); of
the 10 of `Lines whose target is gone …`, `41-8.3` and `41-9.12` (`target gone`) and `41-9.14`
(`done in its new home`); of the six lines of `Lines this pull request carries through another
requirement`, which hold no state and so are no items of the parser's form, `41-8.2` and
`41-13.2`; and the next heading, `## What else changes` (line 118). One list is added, made up,
since the body holds no broken item: `Lines with an item in a broken form:` with the one item
`` - `41-0.1` open: … ``, whose state is not in parentheses.

Replaced: each Dispatch key of the deployment by a made-up one (the two of the first paragraph by
`ACME-12` and `ACME-13`, the one in `41-8.3` by `ACME-11`; the keys are left out of this file), and
the organization and repository of the skills repository, twice (in `41-8.3` and `41-9.14`), by
`acme/skills`. Every other byte of the lines kept is as captured.

## F15: a pull request diff (`F15-pr.diff`)

The diff of the dotfiles repository's closed pull request #41, read with

```sh
gh pr diff 41 --repo <owner>/dotfiles > pr41.diff
```

(348,532 bytes: 79 files, 101 hunks; `splitDiff` reads it whole). #41 renames no file, so the
renamed file comes from the diff of the same repository's pull request #93, read the same day (the
diff `gh pr diff` prints) with

```sh
gh api repos/<owner>/dotfiles/pulls/93 -H 'Accept: application/vnd.github.diff' > pr93.diff
```

Cut, in the path order gh gives, to: `.claude/CLAUDE.md` whole (two hunks,
lines 1 to 36 of #41's diff); the rename of `plugins/<owner>/skills/product-demos/pipeline.md` to
`…/product-demos/references/pipeline.md`, its header and the first of its three hunks (lines 4717
to 4733 of #93's diff); and the header and the first two of the three hunks of
`plugins/<owner>/skills/writing-agent-skills/SKILL.md` (lines 6866 to 6896 of #41's diff), the
hunk left out naming a private term. With `$OWNER` the owner's name (left out of this file):

```sh
{ sed -n '1,36p' pr41.diff; sed -n '4717,4733p' pr93.diff; sed -n '6866,6896p' pr41.diff; } \
  | sed "s/$OWNER/acme/g" > F15-pr.diff
```

Replaced: the owner's name in each path, `plugins/<owner>/skills/` to `plugins/acme/skills/`.
Every other byte is as captured.

## F16: the dotfiles' experiments extension and omp/config.yml (`F16-experiments/`, `F16-config.yml`)

Read on 2026-11-25 from the public dotfiles repository the configuration's `dotfilesRepo` names,
at commit 3c266c9f (2026-10-27), with `gh api repos/<dotfilesRepo>/contents/<path>?ref=<commit>
-H "Accept: application/vnd.github.raw"`:

- `F16-experiments/bash-gate.ts`, `bash-gate-core.ts`, `bash-remainder.ts`, `gates.ts` and
  `gates.json`: `omp/extensions/experiments/<file>` at that commit, byte for byte.
- `F16-experiments/index.ts`: that commit's `omp/extensions/experiments/index.ts` less the five
  import lines of its other features (`judge-log`, `proactive-memory`, `rules-refresh`,
  `selfcompact`, `skill-gate`) and their five entries of `REGISTRATIONS`, which is left empty;
  every other byte as it is. The whole directory is not copied: `proactive-memory-core.ts` holds a
  run of 40 hex digits and an id alphabet the scan below refuses, and none of those features
  hooks a read-only call (the bash gate alone does). The runner itself loads the whole directory
  at the commit it resolves, never this copy.
- `F16-config.yml`: `omp/config.yml` at that commit with each comment taken out (whole comment
  lines, and the text from a ` #` to the end of a line); it parses to the same settings, read
  with `Bun.YAML.parse`. The comments name people, Dispatch keys and private repositories.

checkout.test.ts loads the extension in the pin's sandbox (the bash gate's rows) and in an
ordinary print-mode session beside `F16-config.yml` (the row that sets the stub against such a
session); cli.test.ts copies the extension into `--repos` at the made-up dotfiles commit.

## The canned reader answers

The answers of the `craft`, `nearest`, `claim`, `proposal-target`, `proposal`, `written-case` and
`navigation` readers are made up and written in the tests, as the other readers' are:
`readers.test.ts`'s rows, each valid answer and each refused one with why, and the stand-ins of
`cli.test.ts`, `proposals.test.ts` and `build.test.ts`, which answer each request by its prompt
(and, in `proposals.test.ts`, by its item's id; in `build.test.ts`, the `written-case` reader with
its canned tasks, and the `navigation` reader and the case reader of the made-up sessions
`nav-step`, `nav-line`, `ref-read`, `nav-skill-md` and `nav-nothing` by their text).

## The scan

From `packages/pi-legion`, with the gateway key's file and a terms file of the private names:

```sh
bun scripts/skill-cuts/fixturescan.ts --key-file <key file> --terms <terms file> scripts/skill-cuts/testdata
```

It reads every file here, and the decoded content of each GitHub contents answer, and fails on
any piece of 16 characters of the key (read inside the scan and printed nowhere); on any key or
token shape (an `sk-` key, a GitHub token or fine-grained token prefix, an AWS key id, a PEM block
header, a `Bearer` credential, the key header's name); on an IP address other than `127.0.0.1`,
and an email address other than the example and no-reply ones; on any private name of the terms
file; and on any run of 32 or more hex digits, or of 16 or more characters mixing capitals, small
letters and digits, that is not the value of a JSON key of a known kind (an id, a git object id in
a GitHub URL, a thinking signature, a Responses reasoning item's encrypted content, Claude Code's
`metadata.user_id` with its random device id, or a Claude Code request digest or blob hash), nor a
run that a file here, that file too, holds as the value of such a key (a git object id a GitHub
answer gives as a `sha`, or an id in a pull row's piece of a line cut in the middle). JSON files
are read value by value, JSONL and the psql captures line by line, SSE by its data lines. A hit of
a private name gives the terms file's line, never the name.

The terms file is the operator's, one regular expression per line, matched in any case, and stays
outside the repository, since the names it lists are private: the session stores' names, the
gateway's host, the people's names, the private organization's and the company's names, the key
alias or team, the old plugin name, and the issue key "The scratch directory's name" replaces.
With those, every file here passes. From the repository's root,
`.github/scripts/check-private-names.sh packages/pi-legion/scripts/skill-cuts/testdata` runs
beside it.
