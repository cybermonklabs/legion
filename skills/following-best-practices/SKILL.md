---
name: following-best-practices
description: "Use before choosing a design, writing options, a spec or plan, or requesting approval."
---

# Following best practices

A design a person sends back usually skipped practices that everyone who builds such things knows to follow, or stopped short of a world-class answer for no reason the case required. Its author knew both when asked, but nobody asked until the person did, after the work was done.

- **Best practice** is every practice that people who build this kind of thing know they should follow, and often skip. It is the floor.
- **World-class** is the best answer for this problem, with time and money no object. It is not over-engineering, and it is not built for a scale the work does not have. It runs its own infrastructure when that is the better answer, takes on complexity where the problem earns it, and takes on hard engineering gladly. It is the target.

## 1. Before you design

Before you write a design, a recommendation or options, start a fresh subagent and send it only the problem: what must work, for whom, the constraints and numbers, the person's words that describe the problem, and the setup. Copy the problem statement and its given facts verbatim. Strip every proposed mechanism, solution shape, option, recommendation or leaning from the brief, so a design you already have in mind does not choose the answers. The brief contains no design.

```text
<The problem only.>

From this problem alone, research and answer three questions. Do not ask for or propose a design.
1. What is best practice here? Every practice that applies to this kind of problem.
2. What is the world-class answer? The criteria that make it the best answer for this problem, and each action they ask for (a drill or a test included).
3. Which fact of this setup, if any, rules out a practice or the world-class answer? A cost in time, money or effort is not such a fact.

Return only one numbered list, 1 to N. Each line is `1. {"kind":"practice","action":"verify","target":"webhook signatures","source":"https://...","fact":"<exact words from the problem>","why":"<why that fact makes this action matter>"}`: one JSON object on one line, with exactly these six string fields. `kind` is `practice`, `world-class` or `ruled-out`. Each item is exactly one action, not one topic containing several actions. `action` is one lowercase verb from the check below. `target` is one noun phrase of at most 16 words, not a second instruction or list of actions. Noun modifiers such as `audit logs`, `test plan` and `record retention period` are valid. Split configuring a protection from testing it, writing a runbook from exercising it, and a timeout setting from investigating transactions. For example, logging policy, injection protection and clock synchronization need three items: `define` + `log field policy`; `sanitize` + `log fields`; `synchronize` + `producer clocks`. Keep all the actions; never delete one to fit the format.

Each `source` is a link you opened and read (the tool's documentation or a well-regarded project's documentation). Each `fact` quotes one short fragment of this problem, not an assumed design or an invented constraint. `why` explains the connection and, for a world-class action, the criterion it serves; for a ruled-out action, why that fact prevents it. Each prose field is a single clause: no second sentence, semicolon, colon or conjunction joining another clause. A decimal or dotted identifier is not a sentence boundary. Put no additional instruction in `fact` or `why`: each additional action gets its own item, source and reason, even a drill or test. State that no fact rules anything out when that is the answer, in a final item with action `retain` and target `all applicable practices`. Do not add headings, a preface, code fences, continuation lines or other fields. Before returning, split every compound action and check your list against the schema and check below.
Check the mechanical schema, but do not create `atomicity.json` or certify your own item granularity: the author must obtain that clearance from a different fresh agent after your reply.
Prepositions and noun coordination are not second actions: `mean time to recovery`, `compatibility with PostgreSQL`, `delay before retry`, `latency after failover` and `read and write permissions` can each be one target. The fresh atomicity checker decides whether a preposition or conjunction actually introduces another instruction; word occurrence alone does not decide it.
```
Include the exact check script below in the research brief, so the researcher has its verb vocabulary and schema before answering.

Wait for its answers in the same turn: in Claude Code, start it with `run_in_background: false`; in omp, `wait` for its task. Never end your turn while it runs: in a headless session, the end of your turn stops every subagent still running, and its answers never arrive.

Before any action that states or leans to a design (your text, a file, a subagent brief, or a command's arguments, including an issue title in a dry run), save the whole answer unchanged to `research.md`. Write `table.md` with columns `| # | Practice | Source | Why it applies to this case |`. Copy each item's cells exactly: `kind: action target`, `source`, `fact — why`. Do not paraphrase or supply evidence yourself. If the check rejects an item, a source is missing, or a reason does not name a stated task fact, send the errors and whole answer back to a research subagent for a complete corrected list (a fresh subagent with `run_in_background: false` in Claude Code). It must preserve and split all actions, not drop any. Replace research.md with its whole corrected answer and rebuild the table. Run the exact check until it prints `OK`:

Compute `sha256sum research.md`. Send the problem, schema, whole research file copied by code and hash to a fresh atomicity checker; wait in the same turn (`run_in_background: false` in Claude Code). It checks every item for exactly one prescribed action, including actions hidden in `target`, `fact` or `why`. Distinguish noun modifiers (`audit logs`, `test plan`, `record retention period`, `after-action report`) and identifiers (`database.synchronize`) from second clauses (`runbook exercise unaided recovery`, `timeout measure transaction latency`). Require only JSON: `{"sha256":"<supplied hash>","items":[{"id":1,"actions":["verify webhook signatures"]}]}`. Include every item once in order; extract every prescribed action as an imperative, copying `action target` exactly when there is one. Save its whole JSON unchanged as `atomicity.json`; never author your own clearance. A split, missing item, empty reply or error requires a complete corrected researcher list and a fresh check on its new hash. This pre-design review is distinct from step 2 and cannot authorize publication. The script checks complete one-action coverage bound to the research hash; the checker distinguishes nouns from verbs, which a blacklist cannot do reliably.

```bash
python3 - research.md table.md <<'EOF'
import hashlib, json, re, sys
raw = open(sys.argv[1], "rb").read()
research, table = raw.decode("utf-8"), open(sys.argv[2]).read()
verbs = set("adopt allow assign audit bound compare configure decide define delete disable document encrypt enforce exercise isolate keep limit measure monitor partition persist protect redact reconcile record reject replicate retain rotate sanitize schedule set sign split store synchronize test tune use verify".split())
bad, items, expected, rows = [], [], {}, {}
def unique(pairs):
    d = {}
    for k, v in pairs:
        if k in d:
            raise ValueError("duplicate field " + k)
        d[k] = v
    return d
for i, line in enumerate(research.splitlines(), 1):
    if not line.strip():
        continue
    m = re.fullmatch(r"([1-9]\d*)\. (\{.*\})", line)
    if not m:
        bad.append(f"research.md line {i}: expected numbered one-line JSON item")
        continue
    n = int(m[1])
    items.append(n)
    try:
        d = json.loads(m[2], object_pairs_hook=unique)
        if set(d) != {"kind", "action", "target", "source", "fact", "why"}:
            raise ValueError("expected exactly kind, action, target, source, fact, why")
        if any(not isinstance(v, str) or not v.strip() or v != v.strip() or re.search(r"[|\r\n]", v) for v in d.values()):
            raise ValueError("each field must be one nonempty string without pipes or newlines")
        if d["kind"] not in {"practice", "world-class", "ruled-out"} or d["action"] not in verbs:
            raise ValueError("kind invalid or action not exactly one listed verb")
        for field in ("target", "fact", "why"):
            boundary = r"[.!?]\s+\S|[;:()\[\]{}—–]|\s--?\s|,\s*[A-Za-z]"
            if re.search(boundary, d[field]):
                raise ValueError(field + " must be one clause, not joined sentences or instructions")
        words = re.findall(r"\w+(?:[.'-]\w+)*", d["target"].lower())
        if not words or len(words) > 16 or not re.fullmatch(r"[\w`'. -]+", d["target"]):
            raise ValueError("target must be one noun phrase, not a list or clause")
        if not re.fullmatch(r"https?://[^\s<>]+", d["source"]):
            raise ValueError("source must be a source link from the researcher")
        expected[n] = [str(n), f'{d["kind"]}: {d["action"]} {d["target"]}', d["source"], f'{d["fact"]} — {d["why"]}']
    except (ValueError, TypeError) as e:
        bad.append(f"item {n}: {e}")
if not items or items != list(range(1, len(items) + 1)):
    bad.append(f"research.md numbers its items {items}, not once each from 1 to N")
for line in table.splitlines():
    cells = [c.strip() for c in re.split(r"(?<!\\)\|", line.strip().strip("|"))]
    if line.lstrip().startswith("|") and cells[0].isdigit():
        rows.setdefault(int(cells[0]), []).append(cells)
for n in sorted(set(items) | set(rows)):
    r = rows.get(n, [])
    if n not in items:
        bad.append(f"row {n}: no such item in research.md")
    elif len(r) != 1:
        bad.append(f"item {n}: {len(r)} rows, not 1")
    elif len(r[0]) != 4:
        bad.append(f"item {n}: {len(r[0])} cells, not 4 (# | Practice | Source | Why)")
    elif n in expected and r[0] != expected[n]:
        bad.append(f"item {n}: row differs from the research action, source or fact-based reason")
try:
    proof = json.loads(open("atomicity.json").read(), object_pairs_hook=unique)
    if not isinstance(proof, dict) or set(proof) != {"sha256", "items"} or not isinstance(proof["items"], list):
        raise ValueError("expected sha256 and complete items array")
    if proof["sha256"] != hashlib.sha256(raw).hexdigest():
        raise ValueError("stale research hash; obtain a fresh atomicity check")
    ids = []
    for r in proof["items"]:
        if not isinstance(r, dict) or set(r) != {"id", "actions"} or type(r["id"]) is not int or not isinstance(r["actions"], list):
            raise ValueError("each reviewed item needs integer id and actions array")
        n = r["id"]
        ids.append(n)
        if len(r["actions"]) != 1:
            bad.append(f'item {n}: atomicity checker extracted {len(r["actions"])} actions; ask the researcher to split it')
        elif n in expected and r["actions"][0] != expected[n][1].split(": ", 1)[1]:
            bad.append(f"item {n}: extracted action differs from action target")
    if ids != items:
        raise ValueError("atomicity check must cover every item once in order")
except (OSError, ValueError, TypeError) as e:
    bad.append(f"atomicity.json: {e}")
print("\n".join(bad) or f"OK: {len(items)} items, {len(items)} rows")
sys.exit(1 if bad else 0)
EOF
```

Complete ALL step-1 research, write `research.md` and `table.md` IN FULL, and get the check script's `OK` before the first design action. That gating `OK` must cover the complete, final research. Before design, any research or table change requires the mechanical check again; any research change also requires a fresh atomicity check. Once any design is written, the recorded research and table are FROZEN: do not change either file, add items, spawn another researcher or rebuild the files. A second `OK` after any design does not count. If research was incomplete or changed after the first design action, preserve and report the run's ordering violation; restarting step 1 in another directory does not repair it. The script enforces the action field, clause grammar, exact evidence copying, hash and complete one-action clearance; the independent checker supplies the narrow noun-versus-verb judgment. Neither proves a source real or a quote a stated task fact: check those against the research. Step 2's reviewers must also reject any remaining bundled practice and send it back through step 1, not endorse it. Continue the work in this turn.

Then:

- Follow every best practice unless a fact of this case rules it out, and write that fact in the design. Less work, less money, no new credential, or a precedent here that does it another way is not such a fact.
- Design the world-class answer. Fall short of it only when a fact of this case requires it, and name each shortfall with its reason. Name a cost the person must accept as a cost, for them to decide.
- Settle a disputed fact with evidence: the documentation, the code or a probe. Ask the person only for a cost to accept or a choice of priority.
- In a spec, open the `Design` part with a `### Best practice and world-class` entry copied from step 1's table: each best practice with its source, why it applies to this case and whether the design follows it, the world-class answer, and each place the design falls short with its reason. The approver then sees the comparison without asking for it.

## 2. Before any decision ask or approval request

The decision ask MUST NOT be posted until BOTH reviewers' final comparisons have returned and the paste/arrival cross-review below has cleared them. A Dispatch spec containing an `:::ask` block opens that ask when the spec is published: it is already a decision request, even without `dispatch ask` or `request-approval`. Real block ids, artifact links, an unassigned issue and a later formal approval step do not exempt it.

Run in this order: **local draft → two problem-first reviews → whole-proposal comparisons → both final replies → resolve findings and record verdicts → paste/arrival cross-review → first publication or decision ask**. Keep the proposal and its ask blocks local until that last stage; obtain live block ids and attach links afterward. Never publish first to prepare for review, and never post while a reviewer or the cross-review still runs.

Start two fresh subagents, one per brief, and wait for each answer in the same turn, as in step 1. A fresh subagent starts with no history and gets only the text you send it. In each first-round reviewer's first message, give only the problem: what must work, for whom, the constraints and numbers, the person's words that describe the problem, and the setup. Do not include the proposal's title, design, options, decisions, recommendation or conclusion, or a person's words that state a proposal: a reviewer that reads the design first tends to accept it. After it answers, generate its comparison message by code that reads the WHOLE proposal file into the message, then send that full text to the same subagent and ask it to compare. Each reviewer's actual comparison message must contain every section and every table row, unchanged and unabridged. In both omp and Claude Code, read the file into the message by code, never inline by hand; a hand paste can truncate the proposal. A file path, file name, summary, abridged table or note in place of any section is not the proposal, even when the reviewer can read files. If your harness cannot message a finished subagent and wait for its answer, give a second fresh subagent the first reviewer's answer and the whole proposal's text, read into its message by code, and ask it only to make that comparison; in Claude Code, a message to a finished subagent resumes it in the background, so start that second subagent with `run_in_background: false` instead. Where your harness offers a subagent on a different model family, use it for these reviews, because a reviewer on the author's model shares its blind spots.

Best practice:

```text
First message:
<The problem only.>

From this problem alone, list every best practice for this kind of problem: each practice that people who build such things know they should follow and often skip, with a source you read. Do not inspect or ask for a design.

Follow-up:
<The WHOLE proposal file, read into this message by code: every section and every table row, unabridged.>

List each best practice the proposal does not follow, and whether it states a fact of this case that rules the practice out. Skipping a practice to save work, time, money or a credential is a finding. End with FOLLOWS BEST PRACTICE, or the findings.
```

World-class:

```text
First message:
<The problem only.>

Assume time and money are no object. World-class is not over-engineering and is not built for a scale the work does not have; it runs its own infrastructure when that is the better answer, takes on complexity where the problem earns it, and takes on hard engineering gladly. From this problem alone, write what a world-class solution would look like. Do not inspect or ask for a design.

Follow-up:
<The WHOLE proposal file, read into this message by code: every section and every table row, unabridged.>

List each place it accepts less than that answer (a manual step, a workaround, a second system, a narrower scope, a temporary fix), and whether the proposal gives a fact of this case that requires it. End with WORLD-CLASS, or the findings.
```

- Fix each finding from either review, or record it in the `### Best practice and world-class` entry with the fact or the cost that requires it. Never request approval while a finding is neither. A NOT WORLD-CLASS verdict whose findings are all fixed or recorded does not block the request.
- Write the final round's verdicts of both reviewers in that entry, not in another section: one separate line or row per reviewer, each giving its own verdict, its OWN model (such as `anthropic/claude-opus-5`, never its role or agent name), the id of its run (the agent id or session id your harness gave it; the two reviewers are two runs with two ids) and the clock time (UTC) its final answer arrived. Never use a single shared model line for both reviewers: each reviewer's own model must appear on its own line or row, even when both used the same model. The instant each reviewer's final reply arrives, before reading it or taking any other action, run `date -u` and write that time and that reviewer's run id to a file at once. Copy that reviewer's entry time from this file. A `date -u` run after any intervening action is too late and must not supply the arrival time. Never write the time later from your own clock, from the time you edit the entry, or from text the reviewer or you wrote. Name the verdicts in the approval request, so the approver sees that the reviews ran and what they found.

  Inside `### Best practice and world-class`, record the two reviewers separately:

  ```text
  Best-practice reviewer — model: anthropic/claude-opus-5; run id: <best-practice-run-id>; verdict: FOLLOWS BEST PRACTICE; reply time (UTC): <recorded-arrival-time>.
  World-class reviewer — model: anthropic/claude-opus-5-5; run id: <world-class-run-id>; verdict: WORLD-CLASS; reply time (UTC): <recorded-arrival-time>.
  ```
- Keep reviewer records inside that entry. Use plain lines or a `#### Review verdicts` subheading beneath `### Best practice and world-class`, never a sibling `### Review verdicts`. Only after both final replies and cross-review clearance may you publish the spec/options with its decision asks, then request formal approval when the separate approval rules allow it.
- Once both final comparisons have returned, the findings are fixed or recorded, and the verdicts are in the entry, start the paste/arrival cross-review as the last stage before publication. Use one more fresh subagent and wait for it in the same turn (`run_in_background: false` in Claude Code). Send the current candidate's proposal file text as it was at each send, each whole-file follow-up generated by code from that file, both final replies with their arrival times (not progress wrappers), and any publication history for this candidate. Ask it to confirm every follow-up contains the whole send-version file including the practice table, both final replies arrived, and this candidate's proposal or embedded decision asks have not been published before their required reviews cleared. A previously cleared version may already be published; that does not block review of a local revision. Publish this candidate only after all three confirmations; repair each failure and recheck. If this candidate was published prematurely, report that ordering violation rather than treating a later audit as retroactive clearance.
- Run step 2 again before a later approval request when the design, the problem, its constraints or the evidence changed since the last review.
- Stop after three rounds of step 2 on one proposal: list each change made after the last round and each finding still open, with its cost, in the entry. Review loops between agents do not end by themselves.
- When a reviewer names a practice or an answer you did not consider, go back to step 1 with it instead of defending the design.
- When a person questions the approach, in any words, run step 1 again before you answer.
