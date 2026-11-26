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
#   sh accord-skills-run.sh --omp-flags
#
# --max-cost is required and is omp's own flag of that name, the one `omp --help` prints:
# omp stops sending model requests once the session's spend, its subagents' included,
# reaches the value, and the script writes no cap of its own. The script ends cannot run
# when the help of the omp it finds starts no line with one of the flags the session passes,
# --max-cost and -p among them, so a renamed flag is never read as a model that did not answer.
# --omp-flags reads that help alone and runs no session: it prints "omp --help prints no
# <flag> flag" for the first flag the help lacks and exits 2, or prints nothing and exits 0.
# check.sh's prerequisites run it before --replay runs any row (the line flags|omp of its
# prerequisite_table), so that such an omp ends --replay cannot run, of the kind
# prerequisite, and never fails this row; the list of the flags and its reading stand once,
# here (OMP_FLAGS, omp_flag_missing), for both.
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
#      carries the delivery check issued to it, and each specialist has a subagent of its
#      own: in every phase the state recorded replies for, each specialist that replied can
#      be given a subagent of its own, none shared, that read skill://accord-receiver and
#      returned the delivery check the state issued to that specialist for that phase, so
#      that a subagent that answers for both specialists, or a reply the orchestrator wrote
#      itself with the issued check copied in, leaves a specialist without one.
#   1  result: not locked, or result: locked, with what is missing. The model answered and
#      the protocol did not complete as above; the state line names the phase it reached.
#      not locked is printed only for a session that ended on an answer of its own, omp's
#      status 0 after a last answer whose stopReason is stop, and for one omp stopped at its
#      --max-cost, omp's status 3.
#   2  result: cannot run. A required input is missing, and the first line, "cannot run:
#      <name> ...", names it: --max-cost, TMPDIR, IOC_OMP_MODELS unset or unreadable, omp,
#      python3, git, tar or timeout not on PATH, an omp whose --help starts no line with
#      one of the flags the session passes, or no clone holding the commit.
#   3  result: model not reached (<cause>). The gateway gave no answer, HTTP 429, HTTP 5xx
#      or a refusal of the credential, on the first request or on the last attempt of a
#      turn or of a subagent. Or omp ended the session before the answer that ends it, the
#      model having answered K times, K 0 or more: the script's own bound did, timeout's
#      status 124, or 137 once the bound has passed, the KILL timeout sends 30 s after its
#      TERM to an omp that did not end on it, as check.sh's bounded_run reads its own bound
#      ("no answer, omp ended at the script's 1500 s bound after K answers"); or a signal N
#      ended omp, any other status above 128, as the pod's memory guard ends one with
#      SIGKILL ("no answer, omp ended by signal N after K answers"); each unless the state
#      locked with nothing missing. And, where the model answered and the state did not
#      lock, omp's --max-time did, which omp 18.18.6, as measured on 2026-11-25, ends with
#      status 0, nothing on stderr and an event stream whose last answer still asks for a
#      tool, its stopReason toolUse, before agent_end ("no answer, omp ended its session at
#      --max-time after K answers"), or omp ended any other way than exit 1 names, with
#      status N and its last answer's stopReason R, as on an error no gateway cause above
#      names ("no answer, omp ended with status N (R) after K answers", R "no final stop"
#      where there is none). A session so cut is no verdict on the deliverable, since the
#      model never gave the answer that ends it. Run by check.sh's --replay, which sets
#      __IOC_FAILED, the script first writes into that record the line check.sh's function
#      of a program writes for it: the status 3, or omp's own status for a signal, as 137,
#      no declared status, the fixed command text "omp -p --mode json --max-cost <N>", and
#      the "result:" line, so that check.sh reads the row's failure as the model gateway
#      not answering (its source_answer), or a status above 128 as the signal that ended
#      omp, which ends --replay cannot run, of the kind environment (its run_recorded), and
#      never as a verdict on the deliverable. The cause is one of those words and never
#      omp's own message, which can name the gateway's address.
# IOC_ACCORD_KEEP, when set to a directory, receives the session's event stream, omp's
# stderr, which may name the gateway's address, and the state file before the run's
# directory is removed. IOC_ACCORD_BOUND, a whole number of seconds, is the script's own
# bound on the session in place of 1500, for tests: check.test.sh's case 249 ends a stub omp
# past a bound of 1 s; a replay leaves it unset.
IOC_CLONES=${IOC_CLONES:-${TMPDIR:-/tmp}/ioc-clones}
CP=$IOC_CLONES/cognition-protocols
COMMIT=e3c7bf10f676de37ac08d2fe97e7de982f74629a
MODEL=${IOC_OMP_MODEL:-anthropic/claude-sonnet-5-5}

cannot_run() {
    printf 'cannot run: %s\n' "$1"
    printf '%s\n' 'result: cannot run'
    exit 2
}

# The flags the session passes. A flag counts only where a line of omp --help starts with
# it, alone or after its one-letter form, so a flag the help names only in its prose, as an
# example or another flag's text does, is no flag.
OMP_FLAGS='--max-cost --max-time --mode --no-session --no-title --no-lsp --no-context-files --approval-mode --thinking --model -p'

# omp_flag_missing <help text>; prints "omp --help prints no <flag> flag" for the first flag
# of OMP_FLAGS that no line of the help starts with and returns 0, or returns 1 when the help
# prints every one.
omp_flag_missing() {
    for flag in $OMP_FLAGS; do
        printf '%s\n' "$1" | grep -q -E -- "^[[:space:]]*(-[a-zA-Z], )?$flag([=,[:space:]]|\$)" || {
            printf 'omp --help prints no %s flag\n' "$flag"
            return 0
        }
    done
    return 1
}

if [ "${1:-}" = --omp-flags ]; then
    omp_flag_missing "$(omp --help </dev/null 2>&1)" && exit 2
    exit 0
fi

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

# Every omp call reads /dev/null as its input: omp -p puts what its input holds before its
# prompt, so a run whose caller's input held anything, a loop's rows or a terminal, would
# hand that to the session (the fresh-clone --replay of 8944c0af, whose first prompt, of
# 1551525 characters, held the 778 rows of claims.jsonl after the row's own).
MISSING=$(omp_flag_missing "$(cd "$WORK" && env -i "$@" omp --help </dev/null 2>&1)") && cannot_run "$MISSING"

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

printf '%s\n' "accord-skills-run: cognition-protocols $COMMIT, $(env -i "$@" omp --version </dev/null 2>/dev/null | head -n 1), model $MODEL, --max-cost $MAX_COST"
INSTALLED=
for d in accord-initiator accord-receiver; do
    INSTALLED="$INSTALLED $d ($(find "$AGENT/skills/$d" -type f | wc -l | tr -d ' ') files)"
done
printf 'installed in ~/.omp/agent/skills of the new HOME:%s\n' "$INSTALLED"
(cd "$WORK" && env -i "$@" omp skill list --json </dev/null 2>/dev/null) >"$RUN/skills.json"

# The prompt has the orchestrator write any file it needs inside the working directory, never
# under /tmp, so that whatever the session writes is under the run's own TMPDIR. BOUND is the
# script's own bound on the session, in seconds, 1500 unless IOC_ACCORD_BOUND, a bound for
# tests, names another: at it timeout sends omp TERM, and omp's status is then 124, or 137 if
# omp did not end on TERM and timeout's KILL ended it 30 s later; ELAPSED, the seconds the
# session ran, tells that 137 from one a signal sent before the bound.
BOUND=${IOC_ACCORD_BOUND:-1500}
START=$(date +%s)
(cd "$WORK" && env -i "$@" timeout -k 30 "$BOUND" omp -p --mode json --no-session --no-title --no-lsp \
    --no-context-files --approval-mode yolo --thinking low --model "$MODEL" \
    --max-cost "$MAX_COST" --max-time 20m "$PROMPT" </dev/null) >"$RUN/events.jsonl" 2>"$RUN/omp.err"
OMP_STATUS=$?
ELAPSED=$(($(date +%s) - START))

python3 - "$RUN/skills.json" "$RUN/events.jsonl" "$RUN/omp.err" "$WORK/.accord/state.json" "$OMP_STATUS" "$BOUND" "$ELAPSED" >"$RUN/report" <<'PY'
import json, re, sys

skills_path, events_path, err_path, state_path, omp_status, bound, elapsed = sys.argv[1:8]
SUBS = ("init", "roster", "frame", "glossary", "verify", "lock", "execution-message", "status", "handle")
CHECK_RE = re.compile(r'<accord-delivery\b[^>]*\bcheck="([0-9a-f]+)"')
SKILL_RE = re.compile(r"accord-(initiator|receiver)")
CMD_RE = re.compile(r"(?:accord_initiator\.py[\"']?|\$\{?ACCORD\}?[\"']?)\s+(?:--state\s+\S+\s+)?(" + "|".join(SUBS) + r")\b")
# The start of what each command of the script prints, one JSON object whose first two keys
# are "ok" and "phase", a refusal with "ok": false. The phase is the subcommand (argparse's
# dest), or "usage" for a call whose arguments the parser refused: _Parser.error reports the
# phase _accord_phase holds, which nothing sets, and "usage" by default. The objects nested
# in its "data" carry a "phase" too, but none starts with "ok", and a string in it escapes
# its quotes, so only the object a call printed matches.
RESULT_RE = re.compile(r'\{\s*"ok":\s*(true|false),\s*"phase":\s*"(' + "|".join(SUBS + ("usage",)) + r')"')
SKILLS = {"accord-initiator", "accord-receiver"}


def load_json(path, default):
    try:
        with open(path, errors="replace") as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return default


TOKENS = {"cacheRead": 0, "cacheWrite": 0, "input": 0, "output": 0}


# cost_of(usage): the cost omp billed for one model call or one subagent run, the figure
# --max-cost reads.
def cost_of(usage):
    if not isinstance(usage, dict):
        return 0.0
    cost = usage.get("cost")
    if isinstance(cost, dict):
        cost = cost.get("total")
    return cost if isinstance(cost, (int, float)) else 0.0


# tally(usage): adds the tokens of one model call or one subagent run, by kind, into TOKENS,
# so that the report shows whether the gateway read the session's cache, and returns its
# cost. Unread, as a call whose cache is not read is billed, the four newest sessions of the
# row's note would have cost about five times as much (3.38 to 3.61 USD against 0.69 to 0.72).
def tally(usage):
    if isinstance(usage, dict):
        for kind in TOKENS:
            n = usage.get(kind)
            TOKENS[kind] += n if isinstance(n, int) else 0
    return cost_of(usage)


# result_text(result): what one tool call printed, the text of its result's content.
def result_text(result):
    return "".join(c.get("text", "") for c in result.get("content") or [] if isinstance(c, dict))


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
# last_stop: the stopReason of the session's last assistant message, toolUse where its last
# answer still asked for a tool when the session ended, stop where it ended on an answer of
# its own. sub_results: each subagent's result, as its id and what it returned, its output,
# which the condition of a specialist's subagent of its own reads.
last_stop, sub_results = None, []
for ev in events:
    kind = ev.get("type")
    if kind == "tool_execution_start" and ev.get("toolName") == "read":
        m = SKILL_RE.search(str((ev.get("args") or {}).get("path", "")))
        if m:
            read_by_orchestrator.add(m.group(0))
    elif kind == "message_end" and (ev.get("message") or {}).get("role") == "assistant":
        msg = ev["message"]
        parent_cost += tally(msg.get("usage"))
        last_stop = msg.get("stopReason")
        if last_stop == "error":
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
                sub_results.append((r.get("id"), str(r.get("output") or "")))
                sub_cost += tally(r.get("usage"))
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
            refused += max(failed, len(re.findall(r'"ok":\s*false', result_text(result))))
    elif kind == "tool_execution_end" and ev.get("toolName") == "eval":
        # A session can run the script through its eval tool too, whose events carry the code
        # and what it printed, and no trace of what ran. The code is no record of the calls:
        # in the real run of the tester's report of round 52, the first cell defined a function
        # that ran the script and called it, and each later cell held only calls of that
        # function, which name the script nowhere, so a reading of the code missed about nine
        # calls of ten. What each call printed is read instead, one object per call
        # (RESULT_RE), "ok": false a refusal; its phase is the subcommand, and a call the
        # parser refused, whose phase is "usage", is recorded as (none), as the bash tool's
        # trace records a call whose argv names no subcommand.
        for ok, phase in RESULT_RE.findall(result_text(ev.get("result") or {})):
            calls.append(phase if phase in SUBS else "(none)")
            if ok == "false":
                refused += 1

print("orchestrator read skills: " + (" ".join(sorted(read_by_orchestrator)) or "(none)"))
subagents.discard(None)
readers = receiver_readers & subagents
print(f"subagents that read accord-receiver: {len(readers)} of {len(subagents)}")
print("initiator commands: " + (" ".join(sorted(set(calls))) or "(none)"))
print(f"initiator calls: {len(calls)}, refused: {refused}")

# distinct(eligible): eligible maps each specialist to the subagents it may be given; the
# most specialists that can each be given one of them, none given twice, found by one
# augmenting path per specialist (a maximum bipartite matching), so that a subagent two
# specialists may each be given counts for one alone.
def distinct(eligible):
    held = {}

    def give(agent, seen):
        for sub in eligible[agent]:
            if sub not in seen:
                seen.add(sub)
                if sub not in held or give(held[sub], seen):
                    held[sub] = agent
                    return True
        return False

    return sum(1 for agent in eligible if give(agent, set()))


state = load_json(state_path, None)
locked, every_check, specialists, checked_phases, short_phases = False, False, [], [], []
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
    specialists = state.get("specialists") or []
    every_check = bool(specialists)
    for agent in specialists:
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
    # Each specialist's subagent of its own (the header's exit 0), phase by phase: in each
    # phase the state recorded replies for, a specialist that replied may be given a subagent
    # that read accord-receiver and whose output carries the check the state issued to it for
    # that phase (issued_checks, keyed <phase>:<agent>), and the phase holds when each such
    # specialist can be given one of its own, none shared (distinct). The initiator at the
    # pinned commit sends every specialist the same message, so the same check, in every
    # phase but frame_1, so a subagent that answered for both specialists, or one specialist's
    # subagent whose check the orchestrator copied into the other's reply, carries the checks
    # of both and serves one of them alone. The output is read and not the whole result
    # entry, which holds the task the subagent was given, with the check in it.
    for phase_key, by_agent in replies.items():
        replied = [a for a in specialists if isinstance(by_agent, dict) and a in by_agent]
        if not replied:
            continue
        eligible = {}
        for a in replied:
            c = issued.get(f"{phase_key}:{a}")
            eligible[a] = {sub for sub, out in sub_results if sub in readers and c in out} if isinstance(c, str) and c else set()
        checked_phases.append(phase_key)
        m = distinct(eligible)
        if m < len(replied):
            short_phases.append(f"{phase_key} ({m} of {len(replied)})")
    print("phases with a distinct accord-receiver subagent per specialist: "
          f"{len(checked_phases) - len(short_phases)} of {len(checked_phases)}")

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

if not specialists:
    no_own = "the state names no specialist"
elif not checked_phases:
    no_own = "no phase recorded a specialist reply"
else:
    no_own = f"fewer distinct accord-receiver subagents than specialists in {', '.join(short_phases)}"
missing = [why for why, ok in (("omp did not discover both skills", SKILLS <= set(names)),
                               ("not every reply carries its delivery check", every_check),
                               (no_own, specialists and checked_phases and not short_phases)) if not ok]
if locked and not missing:
    print("result: locked")
    sys.exit(0)
causes = [c for c in (source_cause(last_parent_error), *map(source_cause, sub_failures)) if c]
# The ends of the session other than the answer that ends it (the header's exit 3), read in
# this order. First the machine's, whatever the model answered: timeout's status 124 at the
# script's bound, or 137 once the bound has passed, the KILL timeout sends 30 s after its TERM
# to an omp that did not end on it, as check.sh's bounded_run reads its own bound, and any
# other status above 128, the signal that ended omp. Then a model that never answered, and a
# gateway's cause. Then, where the state did not lock, omp's status 0 after a last answer that
# still asked for a tool is its --max-time, as omp 18.18.6 ends one there, and any other end
# but the two exit 1 names, omp's status 3 at its --max-cost and its status 0 after an answer
# of its own, stopReason stop, is named by omp's status and that stopReason.
status = int(omp_status)
stop = None
if status == 124 or (status == 137 and int(elapsed) >= int(bound)):
    stop = f"omp ended at the script's {bound} s bound"
elif status > 128:
    stop = f"omp ended by signal {status - 128}"
elif answered == 0:
    cause = source_cause(last_parent_error) or source_cause(err_text) or "no answer"
    print(f"result: model not reached ({cause})")
    sys.exit(3)
elif causes and not locked:
    print(f"result: model not reached ({causes[0]}, after {answered} answers)")
    sys.exit(3)
elif not locked and status != 3 and (status != 0 or last_stop != "stop"):
    if status == 0 and last_stop == "toolUse":
        stop = "omp ended its session at --max-time"
    else:
        stop = f"omp ended with status {status} ({last_stop or 'no final stop'})"
if stop:
    print(f"result: model not reached (no answer, {stop} after {answered} answers)")
    sys.exit(3)
if locked:
    print("result: locked, " + "; ".join(missing))
elif status == 3:
    print("result: not locked, omp stopped at its --max-cost")
else:
    print("result: not locked")
sys.exit(1)
PY
STATUS=$?
cat -- "$RUN/report"

# The record check.sh's function of a program writes, as the header says, for a read that
# got no answer from the model gateway, with the status 3, and for a session a signal ended
# before the bound, with omp's own status, which check.sh reads as that signal.
if [ "$STATUS" -eq 3 ] && [ -n "${__IOC_FAILED:-}" ]; then
    SAID=$(sed -n '${s/[[:cntrl:]]/ /g;p;}' "$RUN/report" | cut -c1-240)
    RECORDED=3
    case $SAID in *'omp ended by signal '*) RECORDED=$OMP_STATUS ;; esac
    printf '%s\t-\t%s\t%s\n' "$RECORDED" "omp -p --mode json --max-cost $MAX_COST" "${SAID:--}" >>"$__IOC_FAILED"
fi

if [ -n "${IOC_ACCORD_KEEP:-}" ]; then
    mkdir -p "$IOC_ACCORD_KEEP" &&
        cp -- "$RUN/events.jsonl" "$RUN/omp.err" "$IOC_ACCORD_KEEP/" &&
        { [ ! -f "$WORK/.accord/state.json" ] || cp -- "$WORK/.accord/state.json" "$IOC_ACCORD_KEEP/state.json"; }
fi
exit "$STATUS"
