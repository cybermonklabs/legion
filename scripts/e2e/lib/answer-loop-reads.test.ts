import { beforeAll, describe, expect, test } from "bun:test";
import { cpSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { expectNoMissingHelpers, nameMissingHelpers } from "./script-functions";

// Every read the answer loop's live proof judges a criterion by (lib/answer-loop-reads.sh, which
// scripts/e2e/answer-loop.sh sources), run here as the rig runs it — the same functions, under the
// rig's own `set -Eeuo pipefail` — over fixtures of the shapes a run writes
// (testdata/answer-loop, built from a real run's evidence with no token, host or private name in
// it). No model, no network, no Postgres and no NATS: the commands the reads call out to (pg_sql,
// legion_cmd, dispatch_get, smoke_gh) are stubbed with those fixtures.
//
// Each read is given three things: evidence that must pass, evidence that must fail, and an empty
// input — an empty log, an inbox with nothing in it, a list GitHub answered with an error object,
// a conversation of no bytes. The empty case is a judgement of its own: under `pipefail` a read
// that ends in a failing pipe takes the whole run down instead of recording a FAIL, which is how
// this proof has lost runs before. A read reverted to one of the bugs these cases name — the
// preflight counted as a model call, the agent's own words counted as the loop's append, an asks
// body read as an object, a bigint compared as a number, the duplicate under one dedupe key
// counted as one — fails here.
const lib = import.meta.dir;
const e2e = join(lib, "..");
const reads = join(lib, "answer-loop-reads.sh");
const data = join(lib, "testdata", "answer-loop");
// The rig and every library it sources, so a read that calls a helper this harness neither
// defines nor stubs is named rather than left as a bare exit 127.
const guard = nameMissingHelpers(join(e2e, "answer-loop.sh"));
const dir = temporaryDirectory("answer-loop-reads-test");

/** The daemon log of a run: every boot appended to one file, this one two boots long. */
const bootOne = readFileSync(join(data, "daemon-boot-one.log.txt"), "utf8");
const bootTwo = readFileSync(join(data, "daemon-boot-two.log.txt"), "utf8");
const daemonLog = join(dir, "daemon.log");
/** What the rig records when the second boot starts: boot_log reads past it, run_log reads all. */
const bootOffset = Buffer.byteLength(bootOne);

const emptyLog = join(dir, "empty.log");
const emptyConversation = join(dir, "empty.jsonl");
const goneConversation = join(dir, "no-such-conversation.jsonl");
/** The evidence of a run: the inbox, the proxy's records, the listener's log, A13's probes. */
const evidence = join(dir, "evidence");
/** The same evidence, every file of it empty: a run whose services have written nothing yet. */
const emptyEvidence = join(dir, "empty-evidence");
/** An evidence directory holding nothing at all: the files the reads look for are not there. */
const bareEvidence = join(dir, "bare-evidence");

beforeAll(() => {
  writeFileSync(daemonLog, bootOne + bootTwo);
  writeFileSync(emptyLog, "");
  writeFileSync(emptyConversation, "");
  mkdirSync(join(evidence, "logs"), { recursive: true });
  cpSync(join(data, "inbox.jsonl"), join(evidence, "inbox.jsonl"));
  cpSync(join(data, "model-calls.jsonl"), join(evidence, "model-calls.jsonl"));
  cpSync(join(data, "listener.log.txt"), join(evidence, "logs", "listener.log"));
  cpSync(join(data, "a13"), join(evidence, "a13"), { recursive: true });
  mkdirSync(join(emptyEvidence, "logs"), { recursive: true });
  writeFileSync(join(emptyEvidence, "inbox.jsonl"), "");
  writeFileSync(join(emptyEvidence, "model-calls.jsonl"), "");
  writeFileSync(join(emptyEvidence, "logs", "listener.log"), "");
  mkdirSync(join(bareEvidence, "logs"), { recursive: true });
});

const conversations = {
  architect: join(data, "omp-architect.jsonl"),
  twice: join(data, "omp-architect-twice.jsonl"),
  reviewer: join(data, "claude-reviewer.jsonl"),
  plain: join(data, "claude-reviewer-plain.jsonl"),
};
/** The same Oh My Pi conversation as the session table holds it: one row, its parts joined. */
const sessionRows = JSON.stringify([{ text: readFileSync(conversations.architect, "utf8") }]);

function shq(text: string): string {
  return `'${text.replaceAll("'", `'\\''`)}'`;
}

/**
 * Runs BODY with the reads sourced, the rig's globals set to the fixtures above and the commands
 * they call out to stubbed. A body sets a variable of its own (`evidence=$bare_evidence`,
 * `pg_rows=…`, `store=postgres`) to pick another fixture, and may redefine a stub.
 */
function run(body: string): { code: number; out: string; err: string } {
  const script = `set -Eeuo pipefail
${guard}
note() { printf 'NOTE %s\\n' "$*" >&2; }
# The rows pg_sql prints, the state legion_cmd prints: a body sets its own before it reads.
pg_rows='[]'
legion_state='{}'
pg_sql() { printf '%s\\n' "$pg_rows"; }
legion_cmd() { printf '%s\\n' "$legion_state"; }
dispatch_get() { jq -ce --arg p "$1" '.[$p]' ${shq(join(data, "dispatch.json"))}; }
smoke_gh() { jq -ce --arg p "$2" '.[$p]' ${shq(join(data, "github.json"))}; }
fixtures=${shq(data)}
evidence=${shq(evidence)}
empty_evidence=${shq(emptyEvidence)}
bare_evidence=${shq(bareEvidence)}
daemon_log=${shq(daemonLog)}
empty_log=${shq(emptyLog)}
quiet_log=${shq(join(data, "daemon-no-answers-quiet.log.txt"))}
loud_log=${shq(join(data, "daemon-no-answers-loud.log.txt"))}
boot_offset=${bootOffset}
whole_run=0
store=files
run_tag=4242
daemon_db=legion_files_4242
repo=sjawhar/legion-smoke
controller_session=controller-session
mode=workflow
questions_file=${shq(join(data, "questions.json"))}
declare -A agent=(
  [files:architect.file]=${shq(conversations.architect)}
  [files:twice.file]=${shq(conversations.twice)}
  [files:pre_change.file]=${shq(conversations.reviewer)}
  [files:plain.file]=${shq(conversations.plain)}
  [files:empty.file]=${shq(emptyConversation)}
  [files:gone.file]=${shq(goneConversation)}
  [postgres:architect.file]=${shq(conversations.architect)}
  [postgres:pre_change.file]=${shq(conversations.reviewer)}
)
. ${shq(reads)}
${body}`;
  const result = Bun.spawnSync(["bash", "-c", script]);
  const out = result.stdout.toString();
  const err = result.stderr.toString();
  expectNoMissingHelpers(out, err);
  return { code: result.exitCode ?? -1, out, err };
}

/** The read's output with its trailing newline dropped, and its exit status. */
function read(body: string): { code: number; text: string } {
  const result = run(body);
  return { code: result.code, text: result.out.replace(/\n$/, "") };
}

describe("the daemon's log, boot by boot", () => {
  test("run_log reads every boot and boot_log only the current one", () => {
    expect(read("run_log | jq -s length").text).toBe("17");
    expect(read("boot_log | jq -s length").text).toBe("10");
    // The line that is not JSON at all is in neither, and ends neither read.
    expect(read("run_log | grep -c 'not JSON' || true").text).toBe("0");
  });

  test("a boot that wrote nothing yet is an empty read, not a failure", () => {
    const empty = read('boot_offset=$(stat -c %s "$daemon_log"); boot_log | jq -s length');
    expect(empty).toEqual({ code: 0, text: "0" });
    expect(read("daemon_log=$empty_log; run_log | jq -s length")).toEqual({ code: 0, text: "0" });
  });

  test("answers_lines counts by message, session and state", () => {
    expect(read('answers_lines "answers: question" session-architect handed_off').text).toBe("1");
    expect(read('answers_lines "answers: question" session-planner posted').text).toBe("0");
    expect(
      read('whole_run=1; boot_offset=0; answers_lines "answers: question" session-planner posted')
        .text
    ).toBe("1");
    expect(read('answers_lines "answers: never logged"').text).toBe("0");
    expect(read('daemon_log=$empty_log; answers_lines "answers: question"')).toEqual({
      code: 0,
      text: "0",
    });
  });

  test("question_lines counts a question's lines however the boot names them", () => {
    expect(read("question_lines q-one").text).toBe("1");
    expect(read("question_lines q-never-asked").text).toBe("0");
    // Every line of the first boot that names it, a line with no msg at all among them (the
    // listener's route lines), is judged rather than ending the read.
    expect(read("boot_offset=0; question_lines q-one")).toEqual({ code: 0, text: "5" });
    expect(read("daemon_log=$empty_log; question_lines q-one")).toEqual({ code: 0, text: "0" });
  });

  test("newest_question names the last question of a session in a state", () => {
    // This boot logged the planner's question only as handed off again, which is no `answers:
    // question` line; the boot before it posted one.
    expect(read("newest_question session-planner posted").text).toBe("");
    expect(read("boot_offset=0; newest_question session-planner posted").text).toBe("q-one");
    expect(read("boot_offset=0; newest_question session-planner forwarded").text).toBe("");
    expect(read("daemon_log=$empty_log; newest_question session-planner posted")).toEqual({
      code: 0,
      text: "",
    });
  });

  test("answer_dropped names the session whose conversation had started", () => {
    expect(run("answer_dropped session-architect").code).toBe(0);
    expect(run("answer_dropped session-planner").code).toBe(1);
    expect(run("daemon_log=$empty_log; answer_dropped session-architect").code).toBe(1);
  });

  test("question_forwarded reads the role the question was handed to", () => {
    expect(run("question_forwarded implementer").code).toBe(0);
    expect(run("question_forwarded planner").code).toBe(1);
    expect(run("daemon_log=$empty_log; question_forwarded implementer").code).toBe(1);
  });

  test("fault_passed_on reads the fault the question was passed on for", () => {
    expect(run("fault_passed_on model_calls").code).toBe(0);
    expect(run("fault_passed_on over_limit").code).toBe(1);
    expect(run("daemon_log=$empty_log; fault_passed_on model_calls").code).toBe(1);
  });

  test("answered_fault_is reads the fault and the calls of an answered line", () => {
    const line = `line=$(boot_log | jq -c 'select(.msg == "answers: answered" and .fault == "model_calls")' | tail -1)`;
    expect(run(`${line}; answered_fault_is "$line" model_calls 1`).code).toBe(0);
    expect(run(`${line}; answered_fault_is "$line" model_calls 2`).code).toBe(1);
    expect(run('answered_fault_is "" model_calls 1').code).toBe(1);
  });

  test("decision_lines and decision_passed_once read where a decision went", () => {
    expect(read("decision_lines comment-77 | jq length").text).toBe("1");
    expect(run('decision_passed_once "$(decision_lines comment-77)" ALX-1-architect').code).toBe(0);
    expect(run('decision_passed_once "$(decision_lines comment-77)" "the controller"').code).toBe(
      1
    );
    expect(run('decision_passed_once "$(decision_lines comment-404)" ALX-1-architect').code).toBe(
      1
    );
    expect(run('decision_passed_once "" ALX-1-architect').code).toBe(1);
  });

  test("registry_404_routes groups the boot's 404s by route", () => {
    expect(read('registry_404_routes "$(boot_offset=0 boot_log)"').text).toBe(
      '[{"route":"PUT /v1/legion/projects/{project}","lines":1}]'
    );
    expect(read('registry_404_routes "$(boot_log)"').text).toBe("[]");
    expect(read('registry_404_routes ""')).toEqual({ code: 0, text: "[]" });
  });

  test("answering_blockers names what the answering criteria wait on", () => {
    expect(read("mode=claims-only; answering_blockers").text).toContain(
      "needs the review App key grant"
    );
    expect(read("boot_offset=0; answering_blockers").text).toContain("needs the listener hand-off");
    expect(read("answering_blockers")).toEqual({ code: 0, text: "" });
    expect(read("daemon_log=$empty_log; answering_blockers")).toEqual({ code: 0, text: "" });
  });

  test("check_boot_without_answers passes only a boot that logged no loop and no other 404", () => {
    expect(run("daemon_log=$quiet_log; boot_offset=0; check_boot_without_answers").code).toBe(0);
    expect(run("daemon_log=$loud_log; boot_offset=0; check_boot_without_answers").code).toBe(1);
    expect(run("daemon_log=$empty_log; boot_offset=0; check_boot_without_answers").code).toBe(0);
  });

  test("bad_cost_lines recomputes every cost, cache writes included", () => {
    const lines = `lines=$(run_log | jq -c 'select(.msg == "answers: answered" and .fault == "")')`;
    expect(read(`${lines}; bad_cost_lines "$lines"`).text).toBe("[]");
    const wrong = `lines=$(cat "$fixtures/cost-lines-wrong.jsonl")`;
    expect(read(`${wrong}; bad_cost_lines "$lines" | jq -r '.[].question'`).text).toBe("q-eight");
    expect(read('bad_cost_lines ""')).toEqual({ code: 0, text: "[]" });
  });

  test("cache_read_tokens reads the tokens an answer read from cache", () => {
    expect(read("cache_read_tokens '{\"cacheReadTokens\":1000}'").text).toBe("1000");
    expect(read("cache_read_tokens '{\"inputTokens\":3}'").text).toBe("0");
    expect(read('cache_read_tokens ""')).toEqual({ code: 0, text: "0" });
  });

  test("gh_line_is reads the App, the owner and how the call ended", () => {
    const ok = `line=$(boot_offset=0 boot_log | jq -c 'select(.msg == "answers: gh" and .role == "planner")')`;
    expect(run(`${ok}; gh_line_is "$line" review sjawhar 0`).code).toBe(0);
    expect(run(`${ok}; gh_line_is "$line" implement sjawhar 0`).code).toBe(1);
    expect(run(`${ok}; gh_line_is "$line" review sjawhar nonzero`).code).toBe(1);
    const refused = `line=$(boot_log | jq -c 'select(.msg == "answers: gh" and .role == "reviewer")')`;
    expect(run(`${refused}; gh_line_is "$line" review another-owner nonzero`).code).toBe(0);
    expect(run(`${refused}; gh_line_is "$line" review another-owner 0`).code).toBe(1);
    expect(run('gh_line_is "" review sjawhar any').code).toBe(1);
  });
});

describe("the stopped agents' conversations", () => {
  test("conversation_text reads a session file and the session table's row alike", () => {
    const file = read("conversation_text architect | jq -s length");
    expect(file.text).toBe("8");
    const row = read(
      `store=postgres; pg_rows=${shq(sessionRows)}; conversation_text architect | jq -s length`
    );
    expect(row).toEqual({ code: 0, text: "8" });
    // A Claude Code transcript is a file under either store.
    expect(read("store=postgres; conversation_text pre_change | jq -s length").text).toBe("6");
  });

  test("a session the table holds no row for reads as nothing, not as jq's null", () => {
    expect(read("store=postgres; pg_rows='[]'; conversation_text architect")).toEqual({
      code: 0,
      text: "",
    });
    expect(read("store=postgres; pg_rows='[]'; appended_exchanges architect")).toEqual({
      code: 0,
      text: "0",
    });
    expect(read("conversation_text empty")).toEqual({ code: 0, text: "" });
  });

  test("conversation_holds reads what the conversation carries", () => {
    expect(run(`conversation_holds twice '"type":"compaction"'`).code).toBe(0);
    expect(run(`conversation_holds architect '"type":"compaction"'`).code).toBe(1);
    expect(run(`conversation_holds empty '"type":"compaction"'`).code).toBe(1);
  });

  test("notices_in_conversation counts what reached the agent, never its own words", () => {
    expect(read(`notices_in_conversation architect 'question in-order-1 of run 4242'`).text).toBe(
      "1"
    );
    // The agent said this itself, twice: no notice carried it.
    expect(read(`notices_in_conversation architect VIOLET-OTTER-88`).text).toBe("0");
    expect(read(`notices_in_conversation empty VIOLET-OTTER-88`)).toEqual({ code: 0, text: "0" });
  });

  test("appended_exchanges counts the loop's own marker entries, not the words of an answer", () => {
    expect(read("appended_exchanges architect").text).toBe("1");
    expect(read("appended_exchanges twice").text).toBe("2");
    expect(read("appended_exchanges architect a1a1a1a1b2b2b2b2").text).toBe("1");
    expect(read("appended_exchanges architect q-zzz").text).toBe("0");
    expect(read("appended_exchanges pre_change").text).toBe("1");
    expect(read("appended_exchanges plain").text).toBe("0");
    expect(read("appended_exchanges plain b2b2b2b2c3c3c3c3").text).toBe("0");
    expect(read("appended_exchanges empty")).toEqual({ code: 0, text: "0" });
  });

  test("last_assistant_text is the last thing the agent itself said", () => {
    expect(read("last_assistant_text architect").text).toBe(
      "Both answers are in my conversation now: VIOLET-OTTER-88 and the 20-run rerun."
    );
    expect(read("last_assistant_text twice").text).toBe(
      "The write-through cache, codename VIOLET-OTTER-88."
    );
    expect(read("last_assistant_text empty")).toEqual({ code: 0, text: "" });
  });

  test("transcript_effort and transcript_signatures read the replay's inputs", () => {
    expect(read(`transcript_effort "$(conversation_text pre_change)"`).text).toBe("high");
    expect(read(`transcript_signatures "$(conversation_text pre_change)"`).text).toBe(
      "c2lnbmF0dXJlLW9uZS1mb3ItdGhlLXJlcGxheQ=="
    );
    expect(read(`transcript_effort "$(conversation_text plain)"`).text).toBe("");
    expect(read(`transcript_signatures "$(conversation_text plain)"`).text).toBe("");
    expect(read('transcript_effort ""')).toEqual({ code: 0, text: "" });
    expect(read('transcript_signatures ""')).toEqual({ code: 0, text: "" });
  });

  test("answer_question_id reads the question out of the loop's marker", () => {
    expect(
      read(`answer_question_id 'the answer <!-- legion-answer:a1a1a1a1b2b2b2b2 -->'`).text
    ).toBe("a1a1a1a1b2b2b2b2");
    expect(read(`answer_question_id "$(inbox_reply event-one)"`).text).toBe("a1a1a1a1b2b2b2b2");
    expect(read(`answer_question_id 'an answer the agent wrote itself'`)).toEqual({
      code: 0,
      text: "",
    });
    expect(read(`answer_question_id ''`)).toEqual({ code: 0, text: "" });
  });

  test("occurrences counts, and counts none without failing", () => {
    expect(read(`occurrences 'a A9) b A9) c' 'A9)'`).text).toBe("2");
    expect(read(`occurrences 'a b c' 'A9)'`)).toEqual({ code: 0, text: "0" });
    expect(read(`occurrences '' 'A9)'`)).toEqual({ code: 0, text: "0" });
  });
});

describe("the asker's inbox", () => {
  test("inbox_reply prints the answer to an envelope, and nothing for no answer", () => {
    expect(read("inbox_reply event-one").text).toContain("VIOLET-OTTER-88");
    expect(read("inbox_reply event-one").text).toContain("In Reply To: event-one");
    const none = read("inbox_reply event-never");
    expect(none.text).toBe("");
    expect(none.code).not.toBe(0);
    expect(read("evidence=$empty_evidence; inbox_reply event-one").code).not.toBe(0);
    expect(read("evidence=$bare_evidence; inbox_reply event-one")).toEqual({ code: 1, text: "" });
  });

  test("inbox_message strips the reply header and the topic line", () => {
    expect(read("inbox_message event-one").text).toBe(
      "The write-through cache, codename VIOLET-OTTER-88. I decided it before I stopped.\n\n<!-- legion-answer:a1a1a1a1b2b2b2b2 -->"
    );
    expect(read("inbox_message event-never").text).toBe("");
  });

  test("inbox_replies counts a duplicate under one dedupe key as the two it is", () => {
    expect(read("inbox_replies event-one").text).toBe("1");
    expect(read("inbox_replies event-two").text).toBe("2");
    expect(read("inbox_replies event-never").text).toBe("0");
    expect(read("evidence=$empty_evidence; inbox_replies event-one")).toEqual({
      code: 0,
      text: "0",
    });
    expect(read("evidence=$bare_evidence; inbox_replies event-one")).toEqual({
      code: 0,
      text: "0",
    });
  });

  test("first_reply_of reads which answer reached the asker first, not which was asked first", () => {
    expect(read("first_reply_of event-two event-one").text).toContain(
      "I decided it before I stopped"
    );
    expect(read("first_reply_of event-one event-two").text).toContain(
      "I decided it before I stopped"
    );
    expect(read("first_reply_of event-never event-also-never")).toEqual({ code: 0, text: "" });
    expect(read("evidence=$empty_evidence; first_reply_of event-one event-two")).toEqual({
      code: 0,
      text: "",
    });
  });

  test("controller_notices counts the stand-in's own notices alone", () => {
    expect(read("controller_notices agent-decision comment-77").text).toBe("1");
    expect(read("controller_notices agent-decision comment-404").text).toBe("0");
    expect(read("controller_notices other-kind comment-77").text).toBe("0");
    expect(read("evidence=$empty_evidence; controller_notices agent-decision comment-77")).toEqual({
      code: 0,
      text: "0",
    });
    expect(read("evidence=$bare_evidence; controller_notices agent-decision comment-77")).toEqual({
      code: 0,
      text: "0",
    });
  });
});

describe("the listener's log", () => {
  test("listener_handoffs counts the hand-offs of one envelope", () => {
    expect(read("listener_handoffs event-one").text).toBe("1");
    expect(read("listener_handoffs event-three").text).toBe("0");
    expect(read("listener_handoffs event-never").text).toBe("0");
    expect(read("evidence=$empty_evidence; listener_handoffs event-one")).toEqual({
      code: 0,
      text: "0",
    });
  });

  test("listener_handoffs_to counts every hand-off for a session's daemon", () => {
    expect(read("listener_handoffs_to session-tester-old").text).toBe("2");
    expect(read("listener_handoffs_to session-architect").text).toBe("1");
    expect(read("listener_handoffs_to session-never").text).toBe("0");
    expect(read("evidence=$empty_evidence; listener_handoffs_to session-tester-old")).toEqual({
      code: 0,
      text: "0",
    });
  });
});

describe("the recording proxy's records", () => {
  test("proxy_count counts model calls and proxy_ordinal every request", () => {
    // Three records for claude-files, the first a HEAD /api/hello preflight: two model calls.
    expect(read("proxy_count claude-files").text).toBe("2");
    expect(read("proxy_ordinal claude-files").text).toBe("3");
    // Two for agents, the first a GET /v1/models listing.
    expect(read("proxy_count agents").text).toBe("2");
    expect(read("proxy_ordinal agents").text).toBe("3");
    expect(read("proxy_count no-such-label").text).toBe("0");
    expect(read("evidence=$empty_evidence; proxy_count claude-files")).toEqual({
      code: 0,
      text: "0",
    });
    expect(read("evidence=$bare_evidence; proxy_ordinal claude-files")).toEqual({
      code: 0,
      text: "0",
    });
  });

  test("first_request_after skips the preflight the label's first records are", () => {
    expect(
      read("first_request_after claude-files 0 | jq -r '.messages[0].content[0].text'").text
    ).toContain("A10-1)");
    expect(
      read("first_request_after claude-files 1 | jq -r '.messages[0].content[0].text'").text
    ).toContain("A10-2)");
    expect(read("first_request_after claude-files 2").text).toBe("null");
    expect(read("first_request_after no-such-label 0").text).toBe("null");
    expect(read("evidence=$bare_evidence; first_request_after claude-files 0")).toEqual({
      code: 0,
      text: "null",
    });
  });

  test("daemon_request_for finds the loop's own call by what its body holds", () => {
    const marker = "legion-answer:d4d4d4d4e5e5e5e5";
    expect(
      read(`daemon_request_for ${marker} | jq -r '.messages[0].content[0].text'`).text
    ).toContain(marker);
    expect(read("daemon_request_for legion-answer:0000000000000000").text).toBe("null");
    expect(read(`evidence=$bare_evidence; daemon_request_for ${marker}`)).toEqual({
      code: 0,
      text: "null",
    });
  });

  test("request_effort reads the effort a request carries, wherever it stands", () => {
    expect(read('request_effort "$(first_request_after claude-files 0)"').text).toBe("medium");
    expect(read('request_effort "$(first_request_after agents 0)"').text).toBe("");
    expect(read('request_effort ""')).toEqual({ code: 0, text: "" });
  });
});

describe("the questions file", () => {
  test("question and question_text read a question, and fail for one the file has not", () => {
    expect(read("question_text architect-decision").text).toContain("Which cache strategy");
    expect(run("question no-such-question").code).not.toBe(0);
  });

  test("answer_holds names the first key an answer lacks, and refuses an unknown question", () => {
    expect(
      run(`answer_holds architect-decision 'the write-through cache, codename violet-otter-88'`)
        .code
    ).toBe(0);
    const missing = read(`answer_holds architect-decision 'the write-through cache'`);
    expect(missing).toEqual({ code: 1, text: "VIOLET-OTTER-88" });
    // A question with no keys holds for any answer; one the file does not have holds for none.
    expect(run(`answer_holds unanswerable 'anything at all'`).code).toBe(0);
    const unknown = read(`answer_holds no-such-question 'anything at all'`);
    expect(unknown.code).toBe(1);
    expect(unknown.text).toContain("no question no-such-question");
    expect(read(`answer_holds architect-decision ''`)).toEqual({ code: 1, text: "write-through" });
  });

  test("recorded_hit reads the hits the file records as not about the loop", () => {
    expect(
      run(`recorded_hit skills/envoy/SKILL.md 'A peer with no tools of its own still answers.'`)
        .code
    ).toBe(0);
    expect(
      run(`recorded_hit docs/reference/tools.md 'The tool-less reader of the docs site'`).code
    ).toBe(0);
    expect(run(`recorded_hit skills/envoy/SKILL.md 'a line nobody recorded'`).code).toBe(1);
    expect(run(`recorded_hit '' ''`).code).toBe(1);
  });
});

describe("Dispatch", () => {
  test("ask_of names the newest ask of a session, and reads an error body as no ask", () => {
    expect(read("ask_of session-architect ALX-1").text).toBe("ask-1");
    expect(read("ask_of session-tester ALX-1").text).toBe("");
    // The route answered an object, not the array of asks: no ask, and no error of its own.
    expect(read("ask_of session-architect ALX-9")).toEqual({ code: 0, text: "" });
    // And reads it as a body, not as the array it is not: no jq error in the proof's transcript.
    expect(run("ask_of session-architect ALX-9").err).toBe("");
    expect(read("ask_of session-architect ALX-404")).toEqual({ code: 0, text: "" });
  });

  test("ask_reply_by prints the agent's own reply on the thread", () => {
    expect(read("ask_reply_by ask-1 session-architect").text).toContain("Keep the 7-minute TTL");
    expect(read("ask_reply_by ask-1 session-planner").code).not.toBe(0);
    expect(read("ask_reply_by ask-2 session-architect").code).not.toBe(0);
    expect(read("ask_reply_by ask-404 session-architect").code).not.toBe(0);
  });

  test("comment_reply_by prints the agent's reply to a comment on the issue's thread", () => {
    expect(read("comment_reply_by ALX-3 comment-77 session-tester").text).toContain(
      "The 20-run rerun"
    );
    expect(read("comment_reply_by ALX-3 comment-77 session-architect").code).not.toBe(0);
    expect(read("comment_reply_by ALX-8 comment-77 session-tester").code).not.toBe(0);
  });

  test("record_reply_by reads the reply out of a conversation page's own record", () => {
    const record = `{"message":{"body":"q"},"replies":[{"author":{"id":"session-tester"},"body":"the 20-run rerun"}]}`;
    expect(read(`record_reply_by '${record}' session-tester`).text).toBe("the 20-run rerun");
    expect(read(`record_reply_by '${record}' session-planner`)).toEqual({ code: 0, text: "" });
    expect(read(`record_reply_by '{}' session-tester`)).toEqual({ code: 0, text: "" });
    expect(read(`record_reply_by '' session-tester`)).toEqual({ code: 0, text: "" });
  });

  test("collaborator_role_token names the current tester's role token alone", () => {
    const view = `view=$(dispatch_get "issues/ALX-3/collaborators")`;
    expect(read(`${view}; collaborator_role_token "$view" session-tester`).text).toBe(
      "ALX-3-tester"
    );
    expect(read(`${view}; collaborator_role_token "$view" session-tester-old`).text).toBe("");
    const replaced = `view=$(dispatch_get "issues/ALX-8/collaborators")`;
    expect(read(`${replaced}; collaborator_role_token "$view" session-tester`)).toEqual({
      code: 0,
      text: "",
    });
    expect(read(`collaborator_role_token '' session-tester`)).toEqual({ code: 0, text: "" });
  });

  test("comment_delivered_to and one_failed_delivery read Dispatch's delivery records", () => {
    const sent = `{"id":"c","deliveries":[{"state":"sent","session_id":"session-tester"}]}`;
    expect(run(`comment_delivered_to '${sent}' session-tester`).code).toBe(0);
    expect(run(`comment_delivered_to '${sent}' session-planner`).code).toBe(1);
    expect(run(`comment_delivered_to '' session-tester`).code).toBe(1);
    expect(run(`one_failed_delivery '[{"state":"failed","error":"no session"}]'`).code).toBe(0);
    expect(run(`one_failed_delivery '[{"state":"sent"}]'`).code).toBe(1);
    expect(run(`one_failed_delivery '[{"state":"failed"},{"state":"failed"}]'`).code).toBe(1);
    expect(run(`one_failed_delivery '[]'`).code).toBe(1);
    expect(run(`one_failed_delivery '{"state":"failed"}'`).code).toBe(1);
    expect(run(`one_failed_delivery ''`).code).toBe(1);
  });

  test("rows_hold_paths and scan_rows read the daemon's and Oh My Pi's own rows", () => {
    const rows = `[{"path":"/s/architect.jsonl"},{"path":"/s/tester.jsonl"}]`;
    expect(run(`rows_hold_paths '${rows}' /s/architect.jsonl /s/tester.jsonl`).code).toBe(0);
    expect(run(`rows_hold_paths '${rows}' /s/architect.jsonl /s/planner.jsonl`).code).toBe(1);
    expect(run(`rows_hold_paths '[]' /s/architect.jsonl`).code).toBe(1);
    expect(run(`rows_hold_paths '' /s/architect.jsonl`).code).toBe(1);
    const scanned = `[{"session":"session-pre-change","source":"scan","harness":"claude"},{"session":"session-architect","source":"claim","harness":"omp"}]`;
    expect(read(`scan_rows '${scanned}' session-pre-change`).text).toBe("1");
    expect(read(`scan_rows '${scanned}' session-architect`).text).toBe("0");
    expect(read(`scan_rows '[]' session-pre-change`)).toEqual({ code: 0, text: "0" });
    expect(read(`scan_rows '' session-pre-change`)).toEqual({ code: 0, text: "0" });
  });

  test("registry_entry_matches and registry_replaced_by read the listener's registry entry", () => {
    const entry = `{"session_id":"session-tester-old","issue":"ALX-3","live":false,"replaced_by":"session-tester"}`;
    expect(run(`registry_entry_matches '${entry}' ALX-3`).code).toBe(0);
    expect(run(`registry_entry_matches '${entry}' ALX-1`).code).toBe(1);
    expect(run(`registry_entry_matches '{"issue":"ALX-3","live":true}' ALX-3`).code).toBe(1);
    expect(run(`registry_entry_matches null ALX-3`).code).toBe(1);
    expect(run(`registry_entry_matches '' ALX-3`).code).toBe(1);
    expect(run(`registry_replaced_by '${entry}' session-tester`).code).toBe(0);
    expect(run(`registry_replaced_by '${entry}' session-other`).code).toBe(1);
    expect(run(`registry_replaced_by '' session-tester`).code).toBe(1);
  });
});

describe("GitHub", () => {
  test("pr_comments reads the comments holding the marker, and an error body as none", () => {
    expect(read("pr_comments 41 answer-loop-a17-4242-files | jq length").text).toBe("1");
    expect(read("pr_comments 43 answer-loop-a17-4242-files | jq length").text).toBe("2");
    // GitHub's error body is an object, not the array of comments.
    expect(read("pr_comments 42 answer-loop-a17-4242-files")).toEqual({ code: 0, text: "[]" });
    // A gh call that printed nothing, and one that failed, each leave one empty list.
    expect(read("smoke_gh() { :; }; pr_comments 41 marker")).toEqual({ code: 0, text: "[]" });
    expect(read("smoke_gh() { return 1; }; pr_comments 41 marker")).toEqual({
      code: 0,
      text: "[]",
    });
  });

  test("one_comment_by reads the one comment of the implement App with the session's footer", () => {
    const comments = `comments=$(pr_comments 41 answer-loop-a17-4242-files)`;
    expect(
      run(`${comments}; one_comment_by "$comments" 'example-implement[bot]' session-implementer`)
        .code
    ).toBe(0);
    expect(
      run(`${comments}; one_comment_by "$comments" 'example-review[bot]' session-implementer`).code
    ).toBe(1);
    expect(
      run(`${comments}; one_comment_by "$comments" 'example-implement[bot]' another-session`).code
    ).toBe(1);
    const twice = `comments=$(pr_comments 43 answer-loop-a17-4242-files)`;
    expect(
      run(`${twice}; one_comment_by "$comments" 'example-implement[bot]' session-implementer`).code
    ).toBe(1);
    expect(run(`one_comment_by '[]' 'example-implement[bot]' session-implementer`).code).toBe(1);
    expect(run(`one_comment_by '' 'example-implement[bot]' session-implementer`).code).toBe(1);
  });

  test("refusal_mentioned reads whether the agent said GitHub refused it", () => {
    expect(run(`refusal_mentioned 'GitHub answered 404 Not Found'`).code).toBe(0);
    expect(
      run(`refusal_mentioned 'I was denied: resource not accessible by integration'`).code
    ).toBe(0);
    expect(run(`refusal_mentioned 'I created the file on the default branch.'`).code).toBe(1);
    expect(run(`refusal_mentioned ''`).code).toBe(1);
  });

  test("reviewer_approvals reads the approvals footed with the reviewer's own session", () => {
    expect(read("reviewer_approvals 41 session-reviewer | jq length").text).toBe("1");
    expect(read("reviewer_approvals 41 session-reviewer | jq -r '.[0].footer'").text).toContain(
      "session-reviewer"
    );
    expect(read("reviewer_approvals 41 another-session | jq length").text).toBe("1");
    expect(read("reviewer_approvals 41 session-never | jq length").text).toBe("0");
    expect(read("reviewer_approvals 42 session-reviewer")).toEqual({ code: 0, text: "[]" });
    expect(read("reviewer_approvals 43 session-reviewer")).toEqual({ code: 0, text: "[]" });
    expect(read("smoke_gh() { return 1; }; reviewer_approvals 41 session-reviewer")).toEqual({
      code: 0,
      text: "[]",
    });
  });

  test("approval_by and approval_id read the one approval and its bigint id as a string", () => {
    const head = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c";
    const approvals = `approvals=$(reviewer_approvals 41 session-reviewer)`;
    expect(run(`${approvals}; approval_by "$approvals" 'example-review[bot]' ${head}`).code).toBe(
      0
    );
    expect(
      run(
        `${approvals}; approval_by "$approvals" 'example-review[bot]' 0000000000000000000000000000000000000000`
      ).code
    ).toBe(1);
    expect(
      run(`${approvals}; approval_by "$approvals" 'example-implement[bot]' ${head}`).code
    ).toBe(1);
    expect(run(`approval_by '[]' 'example-review[bot]' ${head}`).code).toBe(1);
    expect(run(`approval_by '' 'example-review[bot]' ${head}`).code).toBe(1);
    expect(read(`${approvals}; approval_id "$approvals"`).text).toBe("2147483648124");
    expect(read(`approval_id '[]'`)).toEqual({ code: 0, text: "0" });
    expect(read(`approval_id ''`)).toEqual({ code: 0, text: "0" });
  });
});

describe("the daemon's own record and state", () => {
  const open = `[{"phase":"reviewing","decision":null,"review_seen":"0"}]`;
  const ended = `[{"phase":"testing","decision":"approved","review_seen":2147483648124}]`;

  test("review_record reads the round, and no round at all as null", () => {
    expect(read(`pg_rows='${open}'; review_record ALX-5`).text).toBe(
      `{"phase":"reviewing","decision":null,"review_seen":"0"}`
    );
    expect(read(`pg_rows='[]'; review_record ALX-5`)).toEqual({ code: 0, text: "null" });
  });

  test("review_round_undecided holds only while nothing decided it", () => {
    expect(run(`pg_rows='${open}'; review_round_undecided "$(review_record ALX-5)"`).code).toBe(0);
    expect(run(`pg_rows='${ended}'; review_round_undecided "$(review_record ALX-5)"`).code).toBe(1);
    expect(run(`pg_rows='[]'; review_round_undecided "$(review_record ALX-5)"`).code).toBe(1);
    expect(run(`review_round_undecided ''`).code).toBe(1);
  });

  test("review_round_ended compares the review it ended on as a string, bigint or not", () => {
    // The record carries review_seen as a number here and as a string above; the id GitHub
    // answered with is a string either way.
    expect(
      run(`pg_rows='${ended}'; review_round_ended "$(review_record ALX-5)" 2147483648124`).code
    ).toBe(0);
    expect(
      run(`pg_rows='${ended}'; review_round_ended "$(review_record ALX-5)" 2147483648125`).code
    ).toBe(1);
    const string = `[{"phase":"testing","decision":"approved","review_seen":"2147483648124"}]`;
    expect(
      run(`pg_rows='${string}'; review_round_ended "$(review_record ALX-5)" 2147483648124`).code
    ).toBe(0);
    expect(run(`pg_rows='${open}'; review_round_ended "$(review_record ALX-5)" 0`).code).toBe(1);
    expect(run(`review_round_ended '' 0`).code).toBe(1);
  });

  test("review_round_open reads a record with no round at all as still open", () => {
    expect(run(`pg_rows='${open}'; review_round_open "$(review_record ALX-5)"`).code).toBe(0);
    expect(run(`pg_rows='${ended}'; review_round_open "$(review_record ALX-5)"`).code).toBe(1);
    expect(run(`pg_rows='[]'; review_round_open "$(review_record ALX-5)"`).code).toBe(0);
    expect(run(`review_round_open ''`).code).toBe(0);
  });

  const live = `{"admission":{"trees":["ALX-1"]},"issues":{"ALX-1":{"phase":"implementing"}},"questionsForController":[{"id":"q-four","ref":"envoy://event-one","reason":"cannot_answer"}]}`;

  test("tree_is_live reads the daemon's own live roots and the root's phase", () => {
    expect(run(`legion_state='${live}'; tree_is_live ALX-1 implementing`).code).toBe(0);
    expect(run(`legion_state='${live}'; tree_is_live ALX-1 done`).code).toBe(1);
    expect(run(`legion_state='${live}'; tree_is_live ALX-9 implementing`).code).toBe(1);
    expect(
      run(`legion_state='{"admission":{"trees":[]},"issues":{}}'; tree_is_live ALX-1 implementing`)
        .code
    ).toBe(1);
    expect(run(`legion_state='{}'; tree_is_live ALX-1 implementing`).code).toBe(1);
  });

  test("passed_to_controller and controller_questions read the questions the loop passed on", () => {
    expect(run(`legion_state='${live}'; passed_to_controller event-one cannot_answer`).code).toBe(
      0
    );
    expect(run(`legion_state='${live}'; passed_to_controller event-one needs_work`).code).toBe(1);
    expect(run(`legion_state='${live}'; passed_to_controller event-never cannot_answer`).code).toBe(
      1
    );
    expect(run(`legion_state='{}'; passed_to_controller event-one cannot_answer`).code).toBe(1);
    expect(read(`legion_state='${live}'; controller_questions cannot_answer`).text).toBe("1");
    expect(read(`legion_state='${live}'; controller_questions post_refused`).text).toBe("0");
    expect(read(`legion_state='{}'; controller_questions post_refused`)).toEqual({
      code: 0,
      text: "0",
    });
  });

  test("tree_claims_parked holds only while every claim of the tree is stopped", () => {
    const parked = `{"claims":[{"tree":"ALX-1","state":"suspended"},{"tree":"ALX-1","state":"retired"},{"tree":"ALX-9","state":"idle"}]}`;
    const started = `{"claims":[{"tree":"ALX-1","state":"suspended"},{"tree":"ALX-1","state":"idle"}]}`;
    expect(run(`tree_claims_parked '${parked}' ALX-1`).code).toBe(0);
    expect(run(`tree_claims_parked '${started}' ALX-1`).code).toBe(1);
    expect(run(`tree_claims_parked '{"claims":[]}' ALX-1`).code).toBe(0);
    expect(run(`tree_claims_parked '' ALX-1`).code).toBe(1);
  });
});

describe("A13: the branch's answer against the base commit's", () => {
  test("a13_same reads two bodies as the same but for the ids a listener mints", () => {
    expect(run("a13_same send.body").code).toBe(0);
    expect(run("a13_same publish.body").code).toBe(0);
    expect(run("a13_same send.status").code).toBe(1);
    expect(run("evidence=$bare_evidence; a13_same send.body").code).toBe(1);
  });
});
