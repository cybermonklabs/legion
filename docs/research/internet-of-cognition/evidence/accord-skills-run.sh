#!/bin/sh
# Installs the two ACCORD skills of cognition-protocols, as they stand at the pinned commit,
# into a new omp HOME under the run's TMPDIR, runs one headless omp session whose task makes
# the orchestrator run accord-initiator with two specialists, each a task subagent answering
# with accord-receiver, and prints what the run did, one line per fact, each line fixed in
# its form. One session plays both parties of ACCORD: the orchestrator runs the initiator's
# script, and each specialist is a task subagent of that session, which reads accord-receiver
# and replies by it. It reads the clone under IOC_CLONES with git, never its working tree,
# and the omp model configuration whose path IOC_OMP_MODELS gives at run time, which must
# reach anthropic/claude-sonnet-5-5, or the model IOC_OMP_MODEL names; the script copies it,
# private to its owner, into the new HOME and never prints it, nothing it writes outside the
# run's directory holds it, and that directory is removed when the script ends.
#
#   sh accord-skills-run.sh --max-cost <USD>
#
# --max-cost is required and is omp's own flag of that name, the one `omp --help` prints:
# omp stops sending model requests once the session's spend, its subagents' included,
# reaches the value, and the script writes no cap of its own. The script ends cannot run
# when the omp it finds prints no such flag, or none of any other flag the session passes, so
# a renamed flag is never read as a model that did not answer.
#
# omp runs under env -i, with PATH, the new HOME and a new TMPDIR, both under the run's
# TMPDIR, the two ACCORD variables below and any proxy or CA variable the caller set, so
# that no omp profile, config, session store or XDG directory of the caller's reaches the
# session. ACCORD_STATE puts the initiator's state at one absolute path, and
# ACCORD_EMBEDDING=off makes its merge use difflib on every machine, as it does wherever
# sentence-transformers is absent; both are the skill script's own variables.
#
# The last line is "result: <outcome>", and the exit status follows it:
#   0  result: locked. The model answered, omp discovered both skills under the new HOME,
#      .accord/state.json reached INTENT-LOCKED, every reply recorded for each specialist
#      carries the delivery check issued to it, and at least one subagent read
#      skill://accord-receiver.
#   1  result: not locked, or result: locked, with what is missing. The model answered and
#      the protocol did not complete as above; the state line names the phase it reached.
#   2  result: cannot run. A required input is missing, and the first line, "cannot run:
#      <name> ...", names it: --max-cost, TMPDIR, IOC_OMP_MODELS unset or unreadable, omp,
#      python3, git, tar or timeout not on PATH, or no clone holding the commit.
#   3  result: model not reached (<cause>). The gateway gave no answer, HTTP 429, HTTP 5xx
#      or a refusal of the credential, on the first request or on the last attempt of a
#      turn or of a subagent. Run by check.sh's --replay, which sets __IOC_FAILED, the
#      script first writes into that record the line check.sh's function of a program
#      writes for it, the status 3, no declared status, the fixed command text
#      "omp -p --mode json --max-cost <N>", and the "result:" line, so that check.sh reads
#      the row's failure as the model gateway not answering (its source_answer), and never
#      as a verdict on the deliverable. The cause is one of those four words and never omp's
#      own message, which can name the gateway's address.
# IOC_ACCORD_KEEP, when set to a directory, receives the session's event stream, omp's
# stderr, which may name the gateway's address, and the state file before the run's
# directory is removed.
IOC_CLONES=${IOC_CLONES:-${TMPDIR:-/tmp}/ioc-clones}
CP=$IOC_CLONES/cognition-protocols
COMMIT=e3c7bf10f676de37ac08d2fe97e7de982f74629a
MODEL=${IOC_OMP_MODEL:-anthropic/claude-sonnet-5-5}

cannot_run() {
    printf 'cannot run: %s\n' "$1"
    printf '%s\n' 'result: cannot run'
    exit 2
}

MAX_COST=
while [ "$#" -gt 0 ]; do
    case "$1" in
    --max-cost)
        [ "$#" -ge 2 ] || cannot_run '--max-cost is given no value'
        MAX_COST=$2
        shift 2
        ;;
    --max-cost=*)
        MAX_COST=${1#--max-cost=}
        shift
        ;;
    *) cannot_run "the argument $1 is none the script reads; it reads --max-cost <USD>" ;;
    esac
done
[ -n "$MAX_COST" ] || cannot_run '--max-cost is not given; pass the flag omp --help prints, as --max-cost 2'
printf '%s\n' "$MAX_COST" | grep -Eq '^[0-9]+(\.[0-9]+)?$' ||
    cannot_run '--max-cost is no number of USD, as 2 or 0.5'
[ -n "${TMPDIR:-}" ] && [ -d "$TMPDIR" ] || cannot_run 'TMPDIR is unset or no directory; the session runs in a new HOME and TMPDIR under it'
[ -n "${IOC_OMP_MODELS:-}" ] ||
    cannot_run 'IOC_OMP_MODELS is unset; set it to the path of an omp models.yml that reaches the model gateway'
[ -f "$IOC_OMP_MODELS" ] && [ -r "$IOC_OMP_MODELS" ] || cannot_run 'IOC_OMP_MODELS names no readable file'
for tool in omp python3 git tar timeout; do
    command -v "$tool" >/dev/null 2>&1 || cannot_run "$tool is not on PATH"
done
git -C "$CP" cat-file -e "$COMMIT^{commit}" 2>/dev/null ||
    cannot_run "no clone of cognition-protocols holding $COMMIT at \$IOC_CLONES/cognition-protocols"

umask 077
RUN=$(mktemp -d "$TMPDIR/accord-skills-run.XXXXXX") || cannot_run 'mktemp failed under TMPDIR'
trap 'rm -rf -- "$RUN"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
NEWHOME=$RUN/home
AGENT=$NEWHOME/.omp/agent
WORK=$RUN/work
mkdir -p "$AGENT/skills" "$WORK" "$RUN/tmp"

# The install: each skill folder copied into the directory omp discovers user skills from,
# $HOME/.omp/agent/skills/<name>/SKILL.md, as the project's install table says for a
# harness of its own ("Copy (or symlink) each skill folder into the harness's skills
# directory"). The archive is written to a file first, so that a git that fails is read as
# one and not as the status of the tar after it.
git -C "$CP" archive --format=tar -o "$RUN/skills.tar" "$COMMIT" protocols/accord/skills/accord-initiator \
    protocols/accord/skills/accord-receiver ||
    cannot_run "git archive of the two ACCORD skills at $COMMIT failed"
tar -x -C "$AGENT/skills" --strip-components=3 -f "$RUN/skills.tar" ||
    cannot_run "unpacking the two ACCORD skills of $COMMIT failed"
cp -- "$IOC_OMP_MODELS" "$AGENT/models.yml" || cannot_run 'copying IOC_OMP_MODELS failed'
# Every model role on one model; task subagents run in the foreground, so that the
# orchestrator gets each specialist's reply as the task tool's result in print mode; and no
# line of a tool's output is cut. omp cuts each line at tools.outputMaxColumns, 768 bytes by
# default, and the initiator prints each phase message as one long line of JSON, so with the
# cut the orchestrator rebuilt the messages from the script's own source and its state file:
# in a kept run of 2026-11-25, 17 bash calls, 3 of them cut, against 9 calls and none cut with
# the cap at 0, which disables it.
cat >"$AGENT/config.yml" <<EOF
modelRoles:
  default: $MODEL
  smol: $MODEL
  slow: $MODEL
  plan: $MODEL
  task: $MODEL
  tiny: $MODEL
  commit: $MODEL
async:
  enabled: false
tools:
  outputMaxColumns: 0
EOF

set -- PATH="$PATH" HOME="$NEWHOME" TMPDIR="$RUN/tmp" LANG=C.UTF-8 \
    ACCORD_STATE="$WORK/.accord/state.json" ACCORD_EMBEDDING=off
for v in HTTPS_PROXY HTTP_PROXY NO_PROXY https_proxy http_proxy no_proxy NODE_EXTRA_CA_CERTS SSL_CERT_FILE; do
    eval "val=\${$v:-}"
    [ -z "$val" ] || set -- "$@" "$v=$val"
done

(cd "$WORK" && env -i "$@" omp --help) >"$RUN/help.txt" 2>&1
for flag in --max-cost --max-time --mode --no-session --no-title --no-lsp --no-context-files \
    --approval-mode --thinking --model; do
    grep -q -E -- "(^|[^a-z-])$flag([^a-z-]|\$)" "$RUN/help.txt" ||
        cannot_run "omp --help prints no $flag flag"
done

PROMPT=$(cat <<'EOF'
You are the orchestrator for the task below, and you work with two specialists. Each specialist is a subagent you start with the task tool.

TASK: The checkout service's error rate rose after last night's deploy of the payment service. Decide whether to roll the deploy back, and state the check that shows the decision was right.

SPECIALISTS (use these ids exactly):
- telemetry: owns the metrics dashboards and the alert rules of the checkout service.
- payments: owns the payment service and its deploy history.
KEY TERMS: error_rate, rollback

Before any delegation, run the accord-initiator skill in its self-driving mode: one `init` with --task, these specialists and these key terms, then one `handle` per specialist reply, until `handle` returns status LOCKED. Every message the script gives a specialist goes to that specialist as a task subagent: the subagent's task is the message exactly as the script printed it, and its context tells it that it is that specialist and that it answers with the accord-receiver skill. Pass each subagent's reply to `handle` exactly as returned, its `<accord-delivery>` line included.

When `handle` returns LOCKED, run `execution-message` once for each specialist with the assignment "Report whether the rollback is needed.", and then stop. Do not delegate the execution step, and do not act on any system: there is none to act on. If the script refuses a command, do what its `next` says; if it refuses the same reply twice, stop and say so. Write any file you need inside the working directory, never under /tmp.

End with one line: ACCORD-RESULT: <the status of the last handle result>
EOF
)

printf '%s\n' "accord-skills-run: cognition-protocols $COMMIT, $(env -i "$@" omp --version 2>/dev/null | head -n 1), model $MODEL, --max-cost $MAX_COST"
INSTALLED=
for d in accord-initiator accord-receiver; do
    INSTALLED="$INSTALLED $d ($(find "$AGENT/skills/$d" -type f | wc -l | tr -d ' ') files)"
done
printf 'installed in ~/.omp/agent/skills of the new HOME:%s\n' "$INSTALLED"
(cd "$WORK" && env -i "$@" omp skill list --json 2>/dev/null) >"$RUN/skills.json"

# The prompt has the orchestrator write any file it needs inside the working directory, never
# under /tmp, so that whatever the session writes is under the run's own TMPDIR.
(cd "$WORK" && env -i "$@" timeout -k 30 1500 omp -p --mode json --no-session --no-title --no-lsp \
    --no-context-files --approval-mode yolo --thinking low --model "$MODEL" \
    --max-cost "$MAX_COST" --max-time 20m "$PROMPT") >"$RUN/events.jsonl" 2>"$RUN/omp.err"
OMP_STATUS=$?

python3 - "$RUN/skills.json" "$RUN/events.jsonl" "$RUN/omp.err" "$WORK/.accord/state.json" "$OMP_STATUS" >"$RUN/report" <<'PY'
import json, re, sys

skills_path, events_path, err_path, state_path, omp_status = sys.argv[1:6]
SUBS = ("init", "roster", "frame", "glossary", "verify", "lock", "execution-message", "status", "handle")
CHECK_RE = re.compile(r'<accord-delivery\b[^>]*\bcheck="([0-9a-f]+)"')
SKILL_RE = re.compile(r"accord-(initiator|receiver)")
CMD_RE = re.compile(r"(?:accord_initiator\.py[\"']?|\$\{?ACCORD\}?[\"']?)\s+(?:--state\s+\S+\s+)?(" + "|".join(SUBS) + r")\b")
SKILLS = {"accord-initiator", "accord-receiver"}


def load_json(path, default):
    try:
        with open(path, errors="replace") as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return default


TOKENS = {"cacheRead": 0, "cacheWrite": 0, "input": 0, "output": 0}


# cost_of(usage): the cost omp billed for one model call, the figure --max-cost reads; it also
# adds the call's tokens, by kind, into TOKENS, so that the report shows whether the gateway
# read the session's cache: the session's prompts cost about four times as much unread.
def cost_of(usage):
    if isinstance(usage, dict):
        for kind in TOKENS:
            n = usage.get(kind)
            TOKENS[kind] += n if isinstance(n, int) else 0
    cost = (usage or {}).get("cost") if isinstance(usage, dict) else None
    if isinstance(cost, dict):
        cost = cost.get("total")
    return cost if isinstance(cost, (int, float)) else 0.0


# What omp discovered under the new HOME.
names = sorted(s.get("name", "") for s in load_json(skills_path, {}).get("skills", []))
print("discovered: " + (" ".join(names) if names else "(none)"))

events = []
try:
    with open(events_path, errors="replace") as fh:
        for line in fh:
            try:
                events.append(json.loads(line))
            except ValueError:
                pass
except OSError:
    pass

read_by_orchestrator, subagents, receiver_readers = set(), set(), set()
calls, refused, bash_commands = [], 0, {}
answered, parent_cost, sub_cost = 0, 0.0, 0.0
last_parent_error, sub_failures = None, []
for ev in events:
    kind = ev.get("type")
    if kind == "tool_execution_start" and ev.get("toolName") == "read":
        m = SKILL_RE.search(str((ev.get("args") or {}).get("path", "")))
        if m:
            read_by_orchestrator.add(m.group(0))
    elif kind == "message_end" and (ev.get("message") or {}).get("role") == "assistant":
        msg = ev["message"]
        parent_cost += cost_of(msg.get("usage"))
        if msg.get("stopReason") == "error":
            last_parent_error = str(msg.get("errorMessage") or "")
        else:
            answered += 1
            last_parent_error = None
    elif kind in ("tool_execution_update", "tool_execution_end") and ev.get("toolName") == "task":
        details = (ev.get("result") or ev.get("partialResult") or {}).get("details") or {}
        for p in details.get("progress") or []:
            subagents.add(p.get("id"))
            for t in p.get("recentTools") or []:
                if t.get("tool") == "read" and "accord-receiver" in json.dumps(t.get("args", "")):
                    receiver_readers.add(p.get("id"))
        if kind == "tool_execution_end":
            for r in details.get("results") or []:
                subagents.add(r.get("id"))
                sub_cost += cost_of(r.get("usage"))
                if r.get("exitCode") not in (0, None):
                    rf = r.get("retryFailure") or {}
                    sub_failures.append(" ".join(str(x) for x in (r.get("error"), rf.get("errorMessage"), rf.get("kind")) if x))
    elif kind == "tool_execution_start" and ev.get("toolName") == "bash":
        bash_commands[ev.get("toolCallId")] = str((ev.get("args") or {}).get("command", ""))
    elif kind == "tool_execution_end" and ev.get("toolName") == "bash":
        # The bash tool's trace records each simple command once, with the argv of its last
        # run and how many times it ran (a loop runs one node several times).
        result = ev.get("result") or {}
        found = failed = 0
        units = ((result.get("details") or {}).get("trace") or {}).get("units") or []
        for unit in units:
            for node in unit.get("nodes") or []:
                argv = [str(a) for a in node.get("argv") or []]
                if len(argv) < 2 or not argv[0].rsplit("/", 1)[-1].startswith("python"):
                    continue
                at = [i for i, a in enumerate(argv) if a.endswith("accord_initiator.py")]
                if not at:
                    continue
                runs = node.get("runs") if isinstance(node.get("runs"), int) else 1
                found += runs
                calls.extend([next((a for a in argv[at[0] + 1:] if a in SUBS), "(none)")] * runs)
                if node.get("st") not in (0, None):
                    failed += 1
        if not units:
            # No trace of the call: read the subcommands from the command text.
            subs = CMD_RE.findall(bash_commands.get(ev.get("toolCallId"), ""))
            calls.extend(subs)
            found = len(subs)
        if found:
            # The script prints "ok": false on every refusal, a run in a loop included.
            text = "".join(c.get("text", "") for c in result.get("content") or [] if isinstance(c, dict))
            refused += max(failed, len(re.findall(r'"ok":\s*false', text)))

print("orchestrator read skills: " + (" ".join(sorted(read_by_orchestrator)) or "(none)"))
subagents.discard(None)
print(f"subagents that read accord-receiver: {len(receiver_readers & subagents)} of {len(subagents)}")
print("initiator commands: " + (" ".join(sorted(set(calls))) or "(none)"))
print(f"initiator calls: {len(calls)}, refused: {refused}")

state = load_json(state_path, None)
locked, every_check = False, False
if not isinstance(state, dict):
    print("state: no state file")
else:
    contract = state.get("locked_contract") or {}
    if contract.get("status") == "INTENT-LOCKED":
        locked = True
        print("state: INTENT-LOCKED")
    else:
        if not state.get("roster"):
            phase = "convene"
        elif state.get("frame_rounds", 0) < 2:
            phase = f"frame_{state.get('frame_rounds', 0)}"
        elif not state.get("glossary"):
            phase = "ground"
        elif state.get("clarify_terms"):
            phase = "clarify"
        else:
            phase = "verify"
        print(f"state: not locked, phase {phase}")
    replies = state.get("handle_replies") or {}
    issued = state.get("issued_checks") or {}
    every_check = bool(state.get("specialists"))
    for agent in state.get("specialists") or []:
        total = matched = 0
        for phase_key, by_agent in replies.items():
            if agent in by_agent:
                total += 1
                m = CHECK_RE.search(by_agent[agent] or "")
                if m and m.group(1) == issued.get(f"{phase_key}:{agent}"):
                    matched += 1
        every_check = every_check and total > 0 and matched == total
        print(f"specialist {agent}: {matched} of {total} recorded replies carry the issued delivery check")
    unverified = sum(1 for d in state.get("defects") or [] if isinstance(d, dict) and d.get("kind") == "unverified_reply")
    every_check = every_check and unverified == 0
    print(f"unverified replies recorded: {unverified}")

print(f"cost: {parent_cost + sub_cost:.4f} USD")
print(f"prompt tokens: {TOKENS['cacheRead'] + TOKENS['cacheWrite'] + TOKENS['input']}, of them "
      f"{TOKENS['cacheRead']} read from the cache, {TOKENS['cacheWrite']} written to it and "
      f"{TOKENS['input']} uncached; output tokens: {TOKENS['output']}")
print(f"omp exit status: {omp_status}")


def source_cause(text):
    # The gateway's answer, by what omp's message says: a status at the start of the message
    # or after HTTP, or the words of the refusal, never a number inside other text.
    t = (text or "").lower()
    if re.search(r"\b429\b|rate.?limit|too many requests|usage.?limit", t):
        return "HTTP 429"
    if re.search(r"^\W*(?:http\W*)?5\d\d\b|\bhttp\W*5\d\d\b|overloaded|bad gateway|service unavailable|internal server error|gateway time.?out", t):
        return "HTTP 5xx"
    if re.search(r"^\W*(?:http\W*)?40[13]\b|\bhttp\W*40[13]\b|unauthori[sz]ed|forbidden|invalid.{0,20}key|authentication", t):
        return "credential refused"
    if re.search(r"econnrefused|enotfound|econnreset|etimedout|fetch failed|socket|network|no answer", t):
        return "no answer"
    return None


try:
    with open(err_path, errors="replace") as fh:
        err_text = fh.read()
except OSError:
    err_text = ""

missing = [why for why, ok in (("omp did not discover both skills", SKILLS <= set(names)),
                               ("not every reply carries its delivery check", every_check),
                               ("no subagent read accord-receiver", receiver_readers & subagents)) if not ok]
if locked and not missing:
    print("result: locked")
    sys.exit(0)
causes = [c for c in (source_cause(last_parent_error), *map(source_cause, sub_failures)) if c]
if answered == 0:
    cause = source_cause(last_parent_error) or source_cause(err_text) or "no answer"
    print(f"result: model not reached ({cause})")
    sys.exit(3)
if causes and not locked:
    print(f"result: model not reached ({causes[0]}, after {answered} answers)")
    sys.exit(3)
if locked:
    print("result: locked, " + "; ".join(missing))
elif omp_status == "3":
    print("result: not locked, omp stopped at its --max-cost")
else:
    print("result: not locked")
sys.exit(1)
PY
STATUS=$?
cat -- "$RUN/report"

# The record check.sh's function of a program writes, as the header says, for a read that
# got no answer from the model gateway.
if [ "$STATUS" -eq 3 ] && [ -n "${__IOC_FAILED:-}" ]; then
    SAID=$(sed -n '${s/[[:cntrl:]]/ /g;p;}' "$RUN/report" | cut -c1-240)
    printf '%s\t-\t%s\t%s\n' 3 "omp -p --mode json --max-cost $MAX_COST" "${SAID:--}" >>"$__IOC_FAILED"
fi

if [ -n "${IOC_ACCORD_KEEP:-}" ]; then
    mkdir -p "$IOC_ACCORD_KEEP" &&
        cp -- "$RUN/events.jsonl" "$RUN/omp.err" "$IOC_ACCORD_KEEP/" &&
        { [ ! -f "$WORK/.accord/state.json" ] || cp -- "$WORK/.accord/state.json" "$IOC_ACCORD_KEEP/state.json"; }
fi
exit "$STATUS"
