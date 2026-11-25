import { beforeAll, describe, expect, test } from "bun:test";
import { cpSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  expectNoMissingHelpers,
  nameMissingHelpers,
  scriptAssignments,
  scriptFunctions,
} from "./script-functions";

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
/** The rig's own functions and variables the reads below call or judge: the rows' reads that live in
 * answer-loop.sh itself, run here as the rig runs them. */
const rig = scriptFunctions(join(e2e, "answer-loop.sh"));
const rigVariable = scriptAssignments(join(e2e, "answer-loop.sh"));

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
/** A13's probes of the branch alone: the base commit's captures are not there. */
const branchOnlyEvidence = join(dir, "branch-only-evidence");
/** A13's probes where a minted field is on one side and not the other: the branch's publish is
 * the base's with `kept_until` left out, and its Dispatch delivery names no envelope where the
 * base's names one. */
const presenceEvidence = join(dir, "presence-evidence");
/** A run's proxy records of A10's GPT-6.1 clause: the preflight, the daemon's two answers to the
 * merger (the first of two calls, the second of one), and the resumed merger's own calls. */
const gptEvidence = join(dir, "gpt-evidence");
/** A chain of three answers the daemon's own loop made to the GPT-6.1 merger through the model
 * gateway, as A10's GPT-6.1 leg asks them: the two cache questions, then the first question of the
 * questions file's a10.reasoning list. */
const gptReasoningEvidence = join(dir, "gpt-reasoning-evidence");
/** A run's proxy records of A23's web searches: the loop's own model call, a search that ran and
 * cited its sources, one whose result was an error, another store's, and the resumed agents'
 * requests holding a search's call and result, Anthropic's and OpenAI's. */
const webSearchEvidence = join(dir, "web-search-evidence");
/** A13's probes with the control's beside them: the branch with `answers` on, whose send was handed
 * to the daemon rather than stored for the stopped session, and whose other captures are the
 * branch's own. */
const controlEvidence = join(dir, "control-evidence");
/** The questions file the live proof reads. */
const liveQuestions = join(e2e, "answer-loop", "questions.json");
/** The questions file's shape, as the tests below read it: the agents the run stops, A10's list of
 * reasoning questions, and every question with its answer keys, and for A22's the comment the proof
 * person writes on the agent's issue once it stopped, which holds them. */
type QuestionsFile = {
  agents: Record<string, { role: string; task: string; replacement_task?: string }>;
  a10: { reasoning: string[] };
  questions: {
    id: string;
    agent: string;
    criteria: string[];
    text: string;
    to?: string;
    expect_kind?: string;
    must_contain?: string[];
    comment?: string;
    expected?: string;
    url?: string;
    published?: string;
  }[];
};

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
  for (const side of ["branch", "base"]) {
    mkdirSync(join(emptyEvidence, "a13", side, "files"), { recursive: true });
    writeFileSync(join(emptyEvidence, "a13", side, "files", "send.body"), "");
  }
  mkdirSync(join(bareEvidence, "logs"), { recursive: true });
  cpSync(join(data, "a13", "branch"), join(branchOnlyEvidence, "a13", "branch"), {
    recursive: true,
  });
  cpSync(join(data, "a13"), join(presenceEvidence, "a13"), { recursive: true });
  mkdirSync(gptEvidence, { recursive: true });
  cpSync(join(data, "model-calls-gpt.jsonl"), join(gptEvidence, "model-calls.jsonl"));
  mkdirSync(gptReasoningEvidence, { recursive: true });
  cpSync(
    join(data, "model-calls-gpt-reasoning.jsonl"),
    join(gptReasoningEvidence, "model-calls.jsonl")
  );
  const presence = join(presenceEvidence, "a13", "branch", "files");
  const basePublish = JSON.parse(
    readFileSync(join(data, "a13", "base", "files", "publish.body"), "utf8")
  ) as Record<string, unknown>;
  delete basePublish.kept_until;
  writeFileSync(join(presence, "publish.body"), JSON.stringify(basePublish));
  const baseSession = JSON.parse(
    readFileSync(join(data, "a13", "base", "files", "dispatch-session.json"), "utf8")
  ) as Record<string, unknown>[];
  writeFileSync(
    join(presence, "dispatch-session.json"),
    JSON.stringify(baseSession.map((delivery) => ({ ...delivery, envelope_id: null })))
  );
  mkdirSync(webSearchEvidence, { recursive: true });
  cpSync(join(data, "model-calls-web-search.jsonl"), join(webSearchEvidence, "model-calls.jsonl"));
  cpSync(join(data, "a13"), join(controlEvidence, "a13"), { recursive: true });
  cpSync(join(data, "a13", "branch"), join(controlEvidence, "a13", "control"), { recursive: true });
  const handedOff = JSON.parse(
    readFileSync(join(data, "a13", "branch", "files", "send.body"), "utf8")
  ) as Record<string, unknown>;
  delete handedOff.state;
  delete handedOff.recipient_state;
  delete handedOff.last_seen;
  writeFileSync(
    join(controlEvidence, "a13", "control", "files", "send.body"),
    JSON.stringify(handedOff)
  );
});

const conversations = {
  architect: join(data, "omp-architect.jsonl"),
  twice: join(data, "omp-architect-twice.jsonl"),
  snapcompact: join(data, "omp-architect-snapcompact.jsonl"),
  reviewer: join(data, "claude-reviewer.jsonl"),
  plain: join(data, "claude-reviewer-plain.jsonl"),
  gpt: join(data, "omp-gpt.jsonl"),
  gptPlain: join(data, "omp-gpt-plain.jsonl"),
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
branch_only_evidence=${shq(branchOnlyEvidence)}
presence_evidence=${shq(presenceEvidence)}
gpt_evidence=${shq(gptEvidence)}
gpt_reasoning_evidence=${shq(gptReasoningEvidence)}
web_search_evidence=${shq(webSearchEvidence)}
control_evidence=${shq(controlEvidence)}
daemon_log=${shq(daemonLog)}
empty_log=${shq(emptyLog)}
quiet_log=${shq(join(data, "daemon-no-answers-quiet.log.txt"))}
loud_log=${shq(join(data, "daemon-no-answers-loud.log.txt"))}
review_log=${shq(join(data, "daemon-review-round.log.txt"))}
gh_lines_log=${shq(join(data, "daemon-gh-lines.log.txt"))}
thread_replies_log=${shq(join(data, "daemon-thread-replies.log.txt"))}
tools_log=${shq(join(data, "daemon-tools.log.txt"))}
boot_offset=${bootOffset}
whole_run=0
store=files
run_tag=4242
daemon_db=legion_files_4242
repo=sjawhar/legion-smoke
controller_session=controller-session
mode=workflow
app_keys_missing=()
questions_file=${shq(join(data, "questions.json"))}
declare -A agent=(
  [files:architect.file]=${shq(conversations.architect)}
  [files:twice.file]=${shq(conversations.twice)}
  [files:snapcompact.file]=${shq(conversations.snapcompact)}
  [files:pre_change.file]=${shq(conversations.reviewer)}
  [files:plain.file]=${shq(conversations.plain)}
  [files:gpt.file]=${shq(conversations.gpt)}
  [files:gpt_plain.file]=${shq(conversations.gptPlain)}
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

/** An `answers: answered` line of an answer on GPT-6.1, with the attributes the loop logs and the
 * cache counts it logged for the second of two answers through the model gateway on 2026-11-18,
 * its cache reads summed over its calls. Its costUSD is its tokens at GPT-6.1's prices. */
const gptLine = {
  time: "2026-11-18T16:00:12.000Z",
  level: "INFO",
  msg: "answers: answered",
  question: "q-gpt",
  model: "openai/gpt-6.1-sol",
  kind: "answer",
  fault: "",
  calls: 2,
  inputTokens: 2480,
  outputTokens: 88,
  cacheWriteTokens: 369,
  cacheReadTokens: 2401,
  reasoningTokens: 40,
  costUSD: 0.0070026,
};

describe("the daemon's log, boot by boot", () => {
  test("run_log reads every boot and boot_log only the current one", () => {
    expect(read("run_log | jq -s length").text).toBe("19");
    expect(read("boot_log | jq -s length").text).toBe("12");
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
    // Claims-only names the key command the run was not given, one input or both.
    expect(
      read(
        "mode=claims-only; app_keys_missing=(LEGION_E2E_REVIEW_APP_KEY_COMMAND); answering_blockers"
      ).text
    ).toContain("WAITING (needs LEGION_E2E_REVIEW_APP_KEY_COMMAND):");
    expect(
      read(
        "mode=claims-only; app_keys_missing=(LEGION_E2E_IMPLEMENT_APP_KEY_COMMAND LEGION_E2E_REVIEW_APP_KEY_COMMAND); answering_blockers"
      ).text
    ).toContain(
      "WAITING (needs LEGION_E2E_IMPLEMENT_APP_KEY_COMMAND and LEGION_E2E_REVIEW_APP_KEY_COMMAND):"
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

  // The loop logs one `answers: answered` line whatever the provider (answer.Answer), so an answer
  // on GPT-6.1 is read as one on Claude is.
  test("cache_read_tokens reads an OpenAI answer's cost line as it reads a Claude one", () => {
    expect(read(`cache_read_tokens ${shq(JSON.stringify(gptLine))}`).text).toBe("2401");
    const unread = JSON.stringify({ ...gptLine, cacheReadTokens: 0 });
    expect(read(`cache_read_tokens ${shq(unread)}`)).toEqual({ code: 0, text: "0" });
  });

  // A GPT-6.1 line is recomputed at GPT-6.1's prices, 2/10/2.5/0.1 per million tokens: the line's
  // tokens cost 0.0070026 USD, and a line that says another cost is no line that recomputes.
  test("bad_cost_lines recomputes a GPT-6.1 cost line at GPT-6.1's prices", () => {
    expect(read(`bad_cost_lines ${shq(JSON.stringify(gptLine))}`).text).toBe("[]");
    const wrong = JSON.stringify({ ...gptLine, question: "q-gpt-wrong", costUSD: 0.0021 });
    expect(read(`bad_cost_lines ${shq(wrong)} | jq -r '.[].question'`).text).toBe("q-gpt-wrong");
    // At Sonnet's prices the same tokens would cost another sum: the model decides the price.
    const asSonnet = JSON.stringify({ ...gptLine, model: "anthropic/claude-sonnet-5-5" });
    expect(read(`bad_cost_lines ${shq(asSonnet)} | jq length`).text).toBe("1");
  });

  // An answer that searched the web costs its tokens and its searches: webSearchUSD, its searches'
  // tokens at the search model's price and the per-search fee, is in costUSD and in no token count,
  // so a line is recomputed with it (A14), and one that leaves it out, or counts it twice, fails.
  test("bad_cost_lines recomputes an answer's web searches into its cost", () => {
    const lines = `daemon_log=$tools_log; lines=$(run_log | jq -c 'select(.msg == "answers: answered")')`;
    expect(read(`${lines}; jq -s length <<<"$lines"`).text).toBe("2");
    expect(read(`${lines}; bad_cost_lines "$lines"`)).toEqual({ code: 0, text: "[]" });
    const searched = {
      msg: "answers: answered",
      question: "q-searched",
      model: "anthropic/claude-sonnet-5-5",
      fault: "",
      inputTokens: 1000,
      outputTokens: 200,
      cacheWriteTokens: 0,
      cacheReadTokens: 0,
      webSearches: 1,
      webSearchUSD: 0.019,
      costUSD: 0.025,
    };
    expect(read(`bad_cost_lines ${shq(JSON.stringify(searched))}`).text).toBe("[]");
    const unfeed = JSON.stringify({ ...searched, question: "q-no-fee", costUSD: 0.006 });
    expect(read(`bad_cost_lines ${shq(unfeed)} | jq -r '.[].question'`).text).toBe("q-no-fee");
    const twice = JSON.stringify({ ...searched, question: "q-fee-twice", costUSD: 0.044 });
    expect(read(`bad_cost_lines ${shq(twice)} | jq -r '.[].question'`).text).toBe("q-fee-twice");
    expect(read('daemon_log=$empty_log; bad_cost_lines "$(run_log)"')).toEqual({
      code: 0,
      text: "[]",
    });
  });

  // The boot's one line naming the loop's tools (A22, A23): the tools it offers, and under each
  // tool it does not offer, why. A daemon with `answers` removed logs no such line at all.
  test("boot_tools_line, tools_line_offers and tools_line_absent read the boot's tools line", () => {
    const tools = "daemon_log=$tools_log; boot_offset=0";
    expect(read(`${tools}; boot_tools_line | jq -c .tools`).text).toBe(
      '["gh","dispatch","web_search"]'
    );
    expect(run(`${tools}; tools_line_offers "$(boot_tools_line)" dispatch`).code).toBe(0);
    expect(run(`${tools}; tools_line_offers "$(boot_tools_line)" web_search`).code).toBe(0);
    expect(run(`${tools}; tools_line_offers "$(boot_tools_line)" bash`).code).toBe(1);
    expect(run(`${tools}; tools_line_absent "$(boot_tools_line)" dispatch no`).code).toBe(1);
    const noDispatch = shq(
      JSON.stringify({
        msg: "answers: tools",
        tools: ["gh", "web_search"],
        dispatch:
          "no dispatch command on PATH or at /opt/legion/pi-envoy/bin/dispatch (set LEGION_DISPATCH_PATH)",
      })
    );
    expect(run(`tools_line_absent ${noDispatch} dispatch 'no dispatch command'`).code).toBe(0);
    expect(run(`tools_line_offers ${noDispatch} dispatch`).code).toBe(1);
    // Another reason, and a tool the line lists, are no absence of the kind the row reads.
    expect(run(`tools_line_absent ${noDispatch} dispatch 'no dispatch_url'`).code).toBe(1);
    expect(run(`tools_line_absent ${noDispatch} web_search ''`).code).toBe(1);
    // A tool the line leaves out with no reason under its name says nothing of why.
    const noReason = shq(JSON.stringify({ msg: "answers: tools", tools: ["gh"] }));
    expect(run(`tools_line_absent ${noReason} web_search ''`).code).toBe(1);
    // The boot before it, and a boot that logged nothing, hold no tools line.
    expect(read("boot_tools_line")).toEqual({ code: 0, text: "" });
    expect(read("daemon_log=$empty_log; boot_offset=0; boot_tools_line")).toEqual({
      code: 0,
      text: "",
    });
    expect(run(`tools_line_offers '' gh`).code).toBe(1);
    expect(run(`tools_line_absent '' dispatch ''`).code).toBe(1);
  });

  // `legion config check` says what the boot line says, in one line of its own words.
  test("check_tools_line, check_tools_lists and check_tools_absent read config check's tools line", () => {
    const offered = `out=$'Config OK: project=ALX design-gate-steps=none\\nanswers: tools [gh dispatch web_search]\\npause unavailable: runtime: tmux'`;
    expect(read(`${offered}; check_tools_line "$out"`).text).toBe(
      "answers: tools [gh dispatch web_search]"
    );
    expect(run(`${offered}; check_tools_lists "$(check_tools_line "$out")" dispatch`).code).toBe(0);
    expect(run(`${offered}; check_tools_lists "$(check_tools_line "$out")" web_search`).code).toBe(
      0
    );
    expect(run(`${offered}; check_tools_lists "$(check_tools_line "$out")" dis`).code).toBe(1);
    expect(
      run(`${offered}; check_tools_absent "$(check_tools_line "$out")" dispatch ''`).code
    ).toBe(1);
    const absent = `line='answers: tools [gh web_search]; dispatch: no dispatch command on PATH or at /opt/legion/pi-envoy/bin/dispatch (set LEGION_DISPATCH_PATH)'`;
    expect(run(`${absent}; check_tools_absent "$line" dispatch 'no dispatch command'`).code).toBe(
      0
    );
    expect(run(`${absent}; check_tools_lists "$line" dispatch`).code).toBe(1);
    expect(run(`${absent}; check_tools_absent "$line" dispatch 'no bun'`).code).toBe(1);
    expect(run(`${absent}; check_tools_absent "$line" web_search ''`).code).toBe(1);
    // A refused check prints no tools line, and no line lists or leaves out anything.
    expect(read(`check_tools_line 'legion config check: refused: answers needs gh'`)).toEqual({
      code: 0,
      text: "",
    });
    expect(read(`check_tools_line ''`)).toEqual({ code: 0, text: "" });
    expect(run(`check_tools_lists '' gh`).code).toBe(1);
    expect(run(`check_tools_absent '' dispatch ''`).code).toBe(1);
  });

  // A22: the dispatch tool's one line a call names the session it ran as, its command, its exit
  // and whether it ran. A read of the comment written after the agent stopped is one that ran as
  // the agent's session and exited 0.
  test("question_dispatch_lines, dispatch_read_line_is and lines_all_as read the dispatch tool's lines", () => {
    const tools = "daemon_log=$tools_log; boot_offset=0";
    const nth = (question: string, n: number) =>
      `line=$(question_dispatch_lines ${question} | jq -c '.[${n}]')`;
    expect(read(`${tools}; question_dispatch_lines e1e1e1e1 | jq length`).text).toBe("2");
    expect(
      run(`${tools}; ${nth("e1e1e1e1", 0)}; dispatch_read_line_is "$line" session-planner`).code
    ).toBe(0);
    expect(
      run(
        `${tools}; first_line_where "$(question_dispatch_lines e1e1e1e1)" dispatch_read_line_is session-planner`
      ).code
    ).toBe(0);
    // A read that exited 1, a read as another session, and a write are no such read.
    expect(
      run(`${tools}; ${nth("e1e1e1e1", 1)}; dispatch_read_line_is "$line" session-planner`).code
    ).toBe(1);
    expect(
      run(`${tools}; ${nth("e1e1e1e1", 0)}; dispatch_read_line_is "$line" session-other`).code
    ).toBe(1);
    expect(
      run(`${tools}; ${nth("e2e2e2e2", 0)}; dispatch_read_line_is "$line" session-planner`).code
    ).toBe(1);
    expect(
      run(`${tools}; lines_all_as "$(question_dispatch_lines e3e3e3e3)" session-planner`).code
    ).toBe(0);
    expect(
      run(`${tools}; lines_all_as "$(question_dispatch_lines e4e4e4e4)" session-planner`).code
    ).toBe(1);
    expect(read(`${tools}; question_dispatch_lines e0e0e0e0`).text).toBe("[]");
    expect(read(`${tools}; question_dispatch_lines ''`).text).toBe("[]");
    expect(run(`dispatch_read_line_is '' session-planner`).code).toBe(1);
    expect(run(`lines_all_as '[]' session-planner`).code).toBe(1);
    expect(run(`lines_all_as '' session-planner`).code).toBe(1);
  });

  // A22: asked to set an issue's status, to claim it or to edit its document, the tool refuses each
  // before dispatch starts, as none of the commands it runs. A call it refused for naming a file of
  // the host, and a call dispatch ran, are no such refusal.
  test("dispatch_refusal_is reads a status write, a claim and a document edit the tool refused", () => {
    const lines = "daemon_log=$tools_log; boot_offset=0; lines=$(question_dispatch_lines e3e3e3e3)";
    for (const command of ["issue-update", "claim", "doc-edit"]) {
      expect(
        run(`${lines}; first_line_where "$lines" dispatch_refusal_is session-planner ${command}`)
          .code
      ).toBe(0);
      expect(
        run(`${lines}; first_line_where "$lines" dispatch_refusal_is session-other ${command}`).code
      ).toBe(1);
    }
    expect(
      run(`${lines}; first_line_where "$lines" dispatch_refusal_is session-planner comment`).code
    ).toBe(1);
    const ranRead =
      "daemon_log=$tools_log; boot_offset=0; lines=$(question_dispatch_lines e1e1e1e1)";
    expect(
      run(`${ranRead}; first_line_where "$lines" dispatch_refusal_is session-planner read`).code
    ).toBe(1);
    expect(run(`dispatch_refusal_is '' session-planner claim`).code).toBe(1);
    expect(run(`first_line_where '[]' dispatch_refusal_is session-planner claim`).code).toBe(1);
  });

  // A23: the web_search tool's one line a call carries the searches it made and what it cost, the
  // per-search fee included, and the answer's cost line counts them as its webSearches and
  // webSearchUSD.
  test("question_search_lines and search_fee_carried read a search's fee into the answer's cost line", () => {
    const tools = "daemon_log=$tools_log; boot_offset=0";
    const fee = `${tools}; answered=$(answered_line f1f1f1f1); lines=$(question_search_lines f1f1f1f1)`;
    expect(read(`${tools}; question_search_lines f1f1f1f1 | jq -c 'map(.seq)'`).text).toBe("[1,2]");
    expect(run(`${fee}; search_fee_carried "$answered" "$lines" 0.01`).code).toBe(0);
    // A fee above what the search cost: the line does not carry it.
    expect(run(`${fee}; search_fee_carried "$answered" "$lines" 0.02`).code).toBe(1);
    // A cost line that left the searches' cost out, or counted no search.
    const short = shq(
      JSON.stringify({
        msg: "answers: answered",
        webSearches: 1,
        webSearchUSD: 0.006,
        costUSD: 0.012,
      })
    );
    expect(run(`${fee}; search_fee_carried ${short} "$lines" 0.01`).code).toBe(1);
    const uncounted = shq(
      JSON.stringify({
        msg: "answers: answered",
        webSearches: 0,
        webSearchUSD: 0.019,
        costUSD: 0.025,
      })
    );
    expect(run(`${fee}; search_fee_carried ${uncounted} "$lines" 0.01`).code).toBe(1);
    // An answer whose searches all failed made none, and carries no fee.
    expect(
      run(
        `${tools}; search_fee_carried "$(answered_line f2f2f2f2)" "$(question_search_lines f2f2f2f2)" 0.01`
      ).code
    ).toBe(1);
    expect(read(`${tools}; question_search_lines f0f0f0f0`).text).toBe("[]");
    expect(run(`search_fee_carried '' '[]' 0.01`).code).toBe(1);
    expect(run(`search_fee_carried '' '' ''`).code).toBe(1);
  });

  test("search_refused reads the fourth search of an answer, refused", () => {
    const tools = "daemon_log=$tools_log; boot_offset=0";
    expect(
      read(`${tools}; search_refused "$(question_search_lines f2f2f2f2)" | jq -r .seq`)
    ).toEqual({ code: 0, text: "4" });
    // Searches that failed, and an answer with no refused search, are no such refusal.
    expect(read(`${tools}; search_refused "$(question_search_lines f1f1f1f1)"`)).toEqual({
      code: 1,
      text: "",
    });
    expect(read(`search_refused '[]'`)).toEqual({ code: 1, text: "" });
    expect(read(`search_refused ''`)).toEqual({ code: 1, text: "" });
  });

  test("unpriced_cost_lines names the lines of a model no price recomputes", () => {
    const lines = `lines=$(run_log | jq -c 'select(.msg == "answers: answered" and .fault == "")')`;
    expect(read(`${lines}; unpriced_cost_lines "$lines"`).text).toBe(
      '[{"question":"q-six","model":"anthropic/claude-opus-5-5"}]'
    );
    const priced = `lines=$(printf '%s\\n' ${shq(JSON.stringify(gptLine))} "$(cat "$fixtures/cost-lines-wrong.jsonl")")`;
    expect(read(`${priced}; unpriced_cost_lines "$lines"`).text).toBe("[]");
    expect(read('unpriced_cost_lines ""')).toEqual({ code: 0, text: "[]" });
  });

  test("answered_line reads one question's cost line, of this boot alone", () => {
    expect(read("answered_line q-six | jq -r .cacheReadTokens").text).toBe("19");
    expect(read('cache_read_tokens "$(answered_line q-four)"').text).toBe("2000");
    // The first boot answered q-one; this one did not.
    expect(read("answered_line q-one")).toEqual({ code: 0, text: "" });
    expect(read("boot_offset=0; answered_line q-one | jq -r .session").text).toBe(
      "session-planner"
    );
    expect(read("answered_line q-never-asked")).toEqual({ code: 0, text: "" });
    expect(read("daemon_log=$empty_log; answered_line q-six")).toEqual({ code: 0, text: "" });
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

  // Proof run 44163479734: the Claude Code reviewer read go.mod with two good gh calls, made a
  // third with no arguments, which the tool refused, and answered. The fixture holds that answer
  // (q-read), an implementer's comment followed by a read that failed (q-write), a later answer
  // of the same reviewer whose one call another owner's repository refused (q-later), an answer
  // the leak guard refused twice (q-leak), and one line of each kind logged with an empty id.
  const ghLinesLog = 'daemon_log="$gh_lines_log"; boot_offset=0';
  const ghLine = (question: string, call: string) =>
    `first_line_where "$(question_gh_lines ${question})" gh_line_is ${call}`;
  const leakRefusal = (question: string, refusal: string) =>
    `first_line_where "$(question_leak_refusals ${question})" leak_refusal_is ${refusal}`;

  test("an answer's own gh calls are judged, whichever it made last", () => {
    const log = ghLinesLog;
    const lastOfRole = (role: string) =>
      `line=$(boot_log | jq -c 'select(.msg == "answers: gh" and .role == "${role}")' | tail -1)`;
    expect(read(`${log}; question_gh_lines q-read | jq -c 'map(.seq)'`).text).toBe("[1,2,3]");
    expect(read(`${log}; question_gh_lines q-none`)).toEqual({ code: 0, text: "[]" });

    // The read A15 needs is the first of the answer's three calls.
    expect(read(`${log}; ${ghLine("q-read", "review sjawhar 0")} | jq -r .seq`).text).toBe("1");
    // Control: the answer's own last call is the refusal, and the role's last call is a later
    // answer's, so a read of either last line finds no read of the repository.
    expect(
      run(`${log}; gh_line_is "$(question_gh_lines q-read | jq -c last)" review sjawhar 0`).code
    ).toBe(1);
    expect(run(`${log}; ${lastOfRole("reviewer")}; gh_line_is "$line" review sjawhar 0`).code).toBe(
      1
    );

    // A17's two reads: the implementer's comment, a later failed read after it, and the refusal
    // on another owner, which the role's last line also happens to be here.
    expect(read(`${log}; ${ghLine("q-write", "implement sjawhar 0")} | jq -r .seq`).text).toBe("1");
    expect(
      run(`${log}; ${lastOfRole("implementer")}; gh_line_is "$line" implement sjawhar 0`).code
    ).toBe(1);
    expect(run(`${log}; ${ghLine("q-later", "review another-owner nonzero")}`).code).toBe(0);

    // An answer is never judged by another answer's call, nor by a call that ended another way.
    expect(read(`${log}; ${ghLine("q-later", "review sjawhar 0")}`)).toEqual({ code: 1, text: "" });
    expect(run(`${log}; ${ghLine("q-read", "review another-owner nonzero")}`).code).toBe(1);
    expect(run(`${log}; ${ghLine("q-read", "implement sjawhar 0")}`).code).toBe(1);
    expect(run(`${log}; ${ghLine("q-none", "review sjawhar any")}`).code).toBe(1);

    // A4, A5 and A12's model_calls leg read the answered line the same way: the reply's own
    // question's, where the log's last answered line is a later answer's.
    expect(read(`${log}; answered_line q-read | jq -r .kind`).text).toBe("answered");
    expect(
      read(`${log}; boot_log | jq -c 'select(.msg == "answers: answered")' | tail -1 | jq -r .kind`)
        .text
    ).toBe("not_answered");
  });

  // A reply that never came names no question. The fixture holds a gh call, a refusal and an
  // answered line logged with an empty id, each one a check would pass on, so a read that took
  // the empty id for a question would find them.
  test("no question is no line, whatever the log holds under an empty id", () => {
    const log = ghLinesLog;
    const smoke = "ALX-4 implementer sjawhar/legion-smoke 'the answer loop proof stand-in'";
    expect(
      read(`${log}; boot_log | jq -s -c '[.[] | select(.question == "") | .msg] | length'`).text
    ).toBe("3");
    expect(read(`${log}; question_gh_lines ""`)).toEqual({ code: 0, text: "[]" });
    expect(read(`${log}; ${ghLine('""', "review sjawhar 0")}`)).toEqual({ code: 1, text: "" });
    expect(read(`${log}; question_leak_refusals ""`)).toEqual({ code: 0, text: "[]" });
    expect(read(`${log}; ${leakRefusal('""', smoke)}`)).toEqual({ code: 1, text: "" });
    expect(read(`${log}; answered_line ""`)).toEqual({ code: 0, text: "" });
  });

  test("the guard's refusal of an answer's gh call is read by its question", () => {
    const reason = "the answer loop proof stand-in refuses this call";
    const smoke = `ALX-4 implementer sjawhar/legion-smoke '${reason}'`;
    expect(read("question_leak_refusals q-ten | jq -c 'map(.seq)'").text).toBe("[2]");
    expect(read(`${leakRefusal("q-ten", smoke)} | jq -r .question`).text).toBe("q-ten");
    // The question is read: another answer's refusal is not this one's, and an answer the guard
    // refused nothing of has none rather than ending the read.
    expect(run(leakRefusal("q-nine", smoke)).code).toBe(1);
    expect(read("question_leak_refusals q-six")).toEqual({ code: 0, text: "[]" });
    expect(read("daemon_log=$empty_log; question_leak_refusals q-ten")).toEqual({
      code: 0,
      text: "[]",
    });
    const line = "line=$(question_leak_refusals q-ten | jq -c first)";
    expect(run(`${line}; leak_refusal_is "$line" ${smoke}`).code).toBe(0);
    expect(
      run(`${line}; leak_refusal_is "$line" ALX-2 implementer sjawhar/legion-smoke '${reason}'`)
        .code
    ).toBe(1);
    expect(
      run(`${line}; leak_refusal_is "$line" ALX-4 reviewer sjawhar/legion-smoke '${reason}'`).code
    ).toBe(1);
    expect(
      run(`${line}; leak_refusal_is "$line" ALX-4 implementer sjawhar/legion-other '${reason}'`)
        .code
    ).toBe(1);
    // Another check's report is not this check's, and the setup failure of a guard wired to no
    // check at all — which refuses every call and writes this same line — is no refusal by one.
    expect(run(leakRefusal("q-nine", `ALX-2 planner sjawhar/legion-other '${reason}'`)).code).toBe(
      1
    );
    const unwired = `{"issue":"ALX-4","role":"implementer","repository":"sjawhar/legion-smoke","why":"LEAK CHECK SETUP ERROR: the gh call: the shared public leak check could not be found"}`;
    expect(run(`leak_refusal_is '${unwired}' ${smoke}`).code).toBe(1);
    expect(run(`leak_refusal_is "" ${smoke}`).code).toBe(1);

    // An answer the guard refused twice, the second time on another repository the model tried
    // next: the refusal A17 needs is the first, and the last line alone (the control) is not it.
    const log = ghLinesLog;
    expect(read(`${log}; ${leakRefusal("q-leak", smoke)} | jq -r .seq`).text).toBe("1");
    expect(
      run(`${log}; leak_refusal_is "$(question_leak_refusals q-leak | jq -c last)" ${smoke}`).code
    ).toBe(1);
  });

  test("review_refused reads the reason the workflow set a review of an issue aside for", () => {
    const session = `"the review App submitted it from a session that is not the reviewer's"`;
    const review = "daemon_log=$review_log; boot_offset=0";
    expect(run(`${review}; review_refused ALX-5 ${session}`).code).toBe(0);
    // Another issue's refusal, and another reason, are not this one.
    expect(run(`${review}; review_refused ALX-6 ${session}`).code).toBe(1);
    expect(run(`${review}; review_refused ALX-5 "footer names no head"`).code).toBe(1);
    expect(run(`${review}; review_refused ALX-6 "footer names no head"`).code).toBe(0);
    // The reason is the whole of it: a prefix of it names no refusal.
    expect(run(`${review}; review_refused ALX-5 "the review App submitted it"`).code).toBe(1);
    expect(run(`daemon_log=$empty_log; review_refused ALX-5 ${session}`).code).toBe(1);
  });

  test("phase_changed reads the issue's move, from one phase to the other", () => {
    const review = "daemon_log=$review_log; boot_offset=0";
    expect(run(`${review}; phase_changed ALX-5 reviewing accepting`).code).toBe(0);
    expect(run(`${review}; phase_changed ALX-5 accepting reviewing`).code).toBe(1);
    expect(run(`${review}; phase_changed ALX-6 reviewing accepting`).code).toBe(1);
    expect(run(`${review}; phase_changed ALX-6 testing reviewing`).code).toBe(0);
    // The boot after the one that logged it moved nothing.
    expect(
      run(
        `${review}; boot_offset=$(stat -c %s "$daemon_log"); phase_changed ALX-5 reviewing accepting`
      ).code
    ).toBe(1);
    expect(run("daemon_log=$empty_log; phase_changed ALX-5 reviewing accepting").code).toBe(1);
  });

  // A20's control: the person's second reply follows their own, in a thread whose root a person
  // wrote, so the tester's daemon takes it for no question for that reason and logs so. Only the
  // line of that session and that whole reason counts: the `answers: question` line of the same
  // session and reason is another message's, and the first count leaves it out.
  test("answers_lines counts a session's hand-offs taken for no question for one reason", () => {
    const thread = "daemon_log=$thread_replies_log; boot_offset=0";
    const skipped = `answers_lines "answers: hand-off is no question; not answered"`;
    const nobody = `"a reply in a comment thread whose root and newest comment before the reply no agent wrote"`;
    expect(read(`${thread}; ${skipped} session-tester "" ${nobody}`).text).toBe("1");
    expect(read(`${thread}; ${skipped} session-tester-old "" ${nobody}`).text).toBe("1");
    expect(
      read(
        `${thread}; ${skipped} session-tester "" "a root comment that does not mention the agent"`
      ).text
    ).toBe("1");
    // With no reason given, every hand-off of the session the loop took for no question counts.
    expect(read(`${thread}; ${skipped} session-tester`).text).toBe("2");
    // Another session and a prefix of the reason are not it.
    expect(read(`${thread}; ${skipped} session-planner "" ${nobody}`).text).toBe("0");
    expect(read(`${thread}; ${skipped} session-tester "" "a reply in a comment thread"`).text).toBe(
      "0"
    );
    // The boot after the one that logged it, and an empty log, logged none.
    expect(
      read(
        `${thread}; boot_offset=$(stat -c %s "$daemon_log"); ${skipped} session-tester "" ${nobody}`
      ).text
    ).toBe("0");
    expect(
      read(`daemon_log=$empty_log; boot_offset=0; ${skipped} session-tester "" ${nobody}`)
    ).toEqual({ code: 0, text: "0" });
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

  // Proof run 43996451813 (the deployment repository, at cb8eb857): A15 and A7 under postgres read
  // "the conversation holds" as no, each beside `jq: error: writing output failed: Broken pipe`. A
  // conversation read from the session table is one jq string; grep -q stops at its first match,
  // and under the rig's pipefail jq's write into the closed pipe fails the whole read. A read that
  // found its needle must not answer that it did not, however long the conversation is.
  test("conversation_holds finds a needle early in a long conversation read from the session table", () => {
    const rows = join(dir, "long-conversation-rows.json");
    writeFileSync(rows, JSON.stringify([{ text: `stage4b-fixture\n${"x".repeat(1 << 20)}\n` }]));
    const body = `store=postgres; pg_rows=$(cat ${shq(rows)})`;
    expect(run(`${body}; conversation_holds architect stage4b-fixture`).code).toBe(0);
    expect(run(`${body}; conversation_holds architect never-written`).code).toBe(1);
  });

  // The same read under the files store: a session file longer than the pipe's buffer, whose
  // `cat` a search that stops early would leave writing into a closed pipe.
  test("conversation_holds finds a needle early in a long session file", () => {
    const file = join(dir, "long-conversation.jsonl");
    writeFileSync(file, `stage4b-fixture\n${"x".repeat(1 << 20)}\n`);
    const body = `agent[files:long.file]=${shq(file)}`;
    expect(run(`${body}; conversation_holds long stage4b-fixture`).code).toBe(0);
    expect(run(`${body}; conversation_holds long never-written`).code).toBe(1);
    expect(run("conversation_holds gone stage4b-fixture").code).toBe(1);
  });

  // A15 reads the file's words after its own question, and its control after the control's own
  // question, which a conversation still holding A15's answer never received: the read must not
  // find the earlier answer for a question the conversation does not hold.
  test("conversation_holds_after reads what followed one question alone", () => {
    const asked = "'question in-order-1 of run 4242, A9'";
    expect(run(`conversation_holds_after architect VIOLET-OTTER-88 ${asked}`).code).toBe(0);
    // Before the question, and only in the question's own entry, is not after it.
    expect(
      run(`conversation_holds_after architect 'Decide the cache strategy' ${asked}`).code
    ).toBe(1);
    expect(run(`conversation_holds_after architect 'Which cache strategy' ${asked}`).code).toBe(1);
    // A control's question the conversation never received.
    const control = "'question in-order-1 of run 4242, control of A9'";
    expect(run(`conversation_holds_after architect VIOLET-OTTER-88 ${control}`).code).toBe(1);
    const rows = `store=postgres; pg_rows=${shq(sessionRows)}`;
    expect(run(`${rows}; conversation_holds_after architect VIOLET-OTTER-88 ${asked}`).code).toBe(
      0
    );
    expect(run(`conversation_holds_after architect VIOLET-OTTER-88 ''`).code).toBe(1);
    expect(run(`conversation_holds_after empty VIOLET-OTTER-88 ${asked}`).code).toBe(1);
    expect(run(`conversation_holds_after gone VIOLET-OTTER-88 ${asked}`).code).toBe(1);
  });

  test("text_compactions counts the compactions the loop's reader reads, never a snapcompact", () => {
    expect(read("text_compactions twice")).toEqual({ code: 0, text: "1" });
    // A person's bare /compact is the image-based snapcompact, and a rollover is no summary the
    // reader reads either: both are compactions, and neither counts.
    expect(run(`conversation_holds snapcompact '"type":"compaction"'`).code).toBe(0);
    expect(read("text_compactions snapcompact")).toEqual({ code: 0, text: "0" });
    expect(read("text_compactions architect")).toEqual({ code: 0, text: "0" });
    expect(read("store=postgres; pg_rows='[]'; text_compactions architect")).toEqual({
      code: 0,
      text: "0",
    });
    expect(read("text_compactions empty")).toEqual({ code: 0, text: "0" });
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

  // A9 judged the resumed architect by its last reply, and in proof run 43996451813 that reply
  // answered a person's earlier reply the architect took in the same turn, not the task. The read
  // takes every reply after the task, the last entry not the agent's own that holds the needle.
  test("assistant_texts_after reads every reply of the turn the needle's entry started", () => {
    const inOrder = "'question in-order-1 of run 4242'";
    // Both replies after it are read, the key in the first, and the reply before it is not.
    expect(read(`assistant_texts_after architect ${inOrder}`)).toEqual({
      code: 0,
      text: "The write-through cache, codename VIOLET-OTTER-88.\nBoth answers are in my conversation now: VIOLET-OTTER-88 and the 20-run rerun.",
    });
    expect(
      run(`answer_holds architect-decision "$(assistant_texts_after architect ${inOrder})"`).code
    ).toBe(0);
    // The last reply alone lacks the key: a read of it alone fails a turn that answered.
    expect(
      run(
        `answer_holds architect-decision 'Both answers are in my conversation now: VIOLET-OTTER-88 and the 20-run rerun.'`
      ).code
    ).toBe(1);
    // Asked twice, the turn read is the one the last of the two started.
    expect(read(`assistant_texts_after twice ${inOrder}`).text).toBe(
      "The write-through cache, codename VIOLET-OTTER-88."
    );
    // No entry holds the needle, or only the agent's own words do: nothing is read.
    expect(read("assistant_texts_after architect 'question after-resume of run 4242'")).toEqual({
      code: 0,
      text: "",
    });
    expect(read("assistant_texts_after architect 'opened the ask'")).toEqual({ code: 0, text: "" });
    expect(read(`assistant_texts_after empty ${inOrder}`)).toEqual({ code: 0, text: "" });
    expect(
      read(`store=postgres; pg_rows='[]'; assistant_texts_after architect ${inOrder}`)
    ).toEqual({ code: 0, text: "" });
  });

  test("transcript_effort, transcript_signatures and transcript_thinking read the replay's inputs", () => {
    expect(read(`transcript_effort "$(conversation_text pre_change)"`).text).toBe("high");
    expect(read(`transcript_signatures "$(conversation_text pre_change)"`).text).toBe(
      "c2lnbmF0dXJlLW9uZS1mb3ItdGhlLXJlcGxheQ=="
    );
    expect(read(`transcript_thinking "$(conversation_text pre_change)"`).text).toBe("1");
    // A transcript that holds no thinking leaves A10's byte-for-byte clause nothing to replay.
    expect(read(`transcript_effort "$(conversation_text plain)"`).text).toBe("");
    expect(read(`transcript_signatures "$(conversation_text plain)"`).text).toBe("");
    expect(read(`transcript_thinking "$(conversation_text plain)"`)).toEqual({
      code: 0,
      text: "0",
    });
    expect(read('transcript_effort ""')).toEqual({ code: 0, text: "" });
    expect(read('transcript_signatures ""')).toEqual({ code: 0, text: "" });
    expect(read('transcript_thinking ""')).toEqual({ code: 0, text: "0" });
  });

  test("omp_thinking_level reads the level a conversation last ran at, off where none is named", () => {
    expect(read('omp_thinking_level "$(conversation_text gpt)"')).toEqual({
      code: 0,
      text: "high",
    });
    // Its last change names no level, which the loop's reader takes as off, not as the high
    // before it.
    expect(read('omp_thinking_level "$(conversation_text gpt_plain)"').text).toBe("off");
    expect(read('omp_thinking_level "$(conversation_text architect)"').text).toBe("off");
    expect(read('omp_thinking_level ""')).toEqual({ code: 0, text: "off" });
  });

  test("omp_reasoning_items and omp_openai_summary read the reasoning a GPT-6.1 conversation stores", () => {
    const items = 'omp_reasoning_items "$(conversation_text gpt)"';
    // The turn that ended in error, rs_failed's, is one the loop's reader drops with what it holds.
    expect(read(`${items} | jq -c 'map(.id)'`).text).toBe('["rs_one","rs_two"]');
    expect(read(`${items} | jq -r '.[1].encrypted_content'`).text).toBe("gAAAAAB-rs-two-stored");
    // A reasoning item with no encrypted content is none a stateless replay could carry.
    expect(read('omp_reasoning_items "$(conversation_text gpt_plain)"').text).toBe("[]");
    expect(read('omp_reasoning_items "$(conversation_text architect)"').text).toBe("[]");
    expect(read('omp_reasoning_items ""')).toEqual({ code: 0, text: "[]" });
    // Nor is the failed turn's tool call one the conversation holds.
    expect(read('omp_openai_summary "$(conversation_text gpt)"').text).toBe(
      '{"turns":3,"failed":1,"models":["openai/openai-responses/gpt-6.1-sol"],"tool_calls":1,"reasoning":2,"encrypted":2}'
    );
    expect(read('omp_openai_summary "$(conversation_text gpt_plain)"').text).toBe(
      '{"turns":2,"failed":0,"models":["openai/openai-responses/gpt-6.1-sol"],"tool_calls":1,"reasoning":1,"encrypted":0}'
    );
    // A conversation with no tool call: A10's GPT-6.1 clause fails it.
    expect(
      read(
        `omp_openai_summary "$(conversation_text architect)" | jq -c '[.tool_calls, .encrypted]'`
      ).text
    ).toBe("[0,0]");
    expect(read('omp_openai_summary ""')).toEqual({
      code: 0,
      text: '{"turns":0,"failed":0,"models":[],"tool_calls":0,"reasoning":0,"encrypted":0}',
    });
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

  test("question_occurrences counts a question by the whole suffix it was asked with, not by its marker", () => {
    // A resumed agent's request as proof run 44858316560 recorded it: the question asked once,
    // then the agent's own reply naming the marker again.
    const request = JSON.stringify({
      messages: [
        {
          role: "user",
          content: "What blocked the release? (question pre-change-scan of run 12688, A10-1)",
        },
        {
          role: "assistant",
          content: "The same as for the earlier pre-change-scan questions (including A10-1).",
        },
        { role: "user", content: "And again? (question pre-change-scan of run 12688, A10-2)" },
      ],
    });
    const q = `'${request}'`;
    expect(read(`question_occurrences ${q} pre-change-scan 12688 A10-1`).text).toBe("1");
    expect(read(`question_occurrences ${q} pre-change-scan 12688 A10-2`).text).toBe("1");
    // The marker alone is what counted the question twice.
    expect(read(`occurrences ${q} 'A10-1)'`).text).toBe("2");
    expect(read(`question_occurrences ${q} pre-change-scan 12689 A10-1`)).toEqual({
      code: 0,
      text: "0",
    });
    expect(read(`question_occurrences '' pre-change-scan 12688 A10-1`)).toEqual({
      code: 0,
      text: "0",
    });
    // The ask ends with the same suffix the count looks for.
    expect(read("question_suffix pre-change-scan 12688 'A10 after compaction'").text).toBe(
      "(question pre-change-scan of run 12688, A10 after compaction)"
    );
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

  test("replies_to counts each answered event's replies once, however many sends name it", () => {
    // The listener answers a sender's own repeat under one key with its first copy's event, so
    // the sends of one envelope can all name one event: its replies count once, not once a send.
    expect(read("replies_to event-two event-two event-two").text).toBe("2");
    expect(read("replies_to event-one event-two").text).toBe("3");
    expect(read("replies_to event-one event-never event-one").text).toBe("1");
    expect(read("replies_to event-never").text).toBe("0");
    expect(read("evidence=$empty_evidence; replies_to event-one event-one")).toEqual({
      code: 0,
      text: "0",
    });
    expect(read("evidence=$bare_evidence; replies_to event-one")).toEqual({ code: 0, text: "0" });
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

  // A send the listener named no event for is answered by nothing: `In Reply To: ` alone is in
  // every reply, and read as an event it took another question's answer for its own. A control
  // whose message was not taken must read no answer, never every answer.
  test("the inbox reads an empty event as no send, never as every reply", () => {
    expect(read('inbox_reply ""')).toEqual({ code: 1, text: "" });
    expect(read('inbox_message ""').text).toBe("");
    expect(read('inbox_replies ""')).toEqual({ code: 0, text: "0" });
    expect(read('replies_to ""')).toEqual({ code: 0, text: "0" });
    expect(read('first_reply_of "" event-one')).toEqual({ code: 1, text: "" });
    expect(read('first_reply_of event-one ""')).toEqual({ code: 1, text: "" });
    expect(read('controller_notices agent-decision ""')).toEqual({ code: 0, text: "0" });
  });

  test("replied_with_keys reads one event's reply for its question's keys", () => {
    expect(read("replied_with_keys event-one architect-decision")).toEqual({ code: 0, text: "" });
    expect(read("replied_with_keys event-two architect-decision")).toEqual({
      code: 1,
      text: "write-through",
    });
    expect(read("replied_with_keys event-never architect-decision")).toEqual({
      code: 1,
      text: "no reply to event-never",
    });
    expect(read('replied_with_keys "" architect-decision')).toEqual({
      code: 1,
      text: "no reply to a send the listener named no event for",
    });
    expect(
      read("evidence=$empty_evidence; replied_with_keys event-one architect-decision").code
    ).toBe(1);
  });

  // A7 reads that nothing was posted in answer to the question the loop forwarded to the running
  // agent; its control places a reply carrying the loop's mark there, and the read must see it.
  test("no_reply_to holds only for a send nothing answered", () => {
    expect(run("no_reply_to event-never").code).toBe(0);
    expect(run("no_reply_to event-one").code).toBe(1);
    expect(run("no_reply_to event-two").code).toBe(1);
    expect(run('no_reply_to ""').code).toBe(1);
    expect(run("evidence=$bare_evidence; no_reply_to event-one").code).toBe(0);
  });
});

describe("the listener's log", () => {
  test("listener_handoffs_to counts every hand-off for a session's daemon", () => {
    expect(read("listener_handoffs_to session-tester-old").text).toBe("2");
    expect(read("listener_handoffs_to session-architect").text).toBe("1");
    expect(read("listener_handoffs_to session-never").text).toBe("0");
    expect(read("evidence=$empty_evidence; listener_handoffs_to session-tester-old")).toEqual({
      code: 0,
      text: "0",
    });
  });

  test("listener_stored_handoffs_to counts the hand-offs the stream stored, not its repeats", () => {
    // The planner's three sends under one key: one hand-off stored, two the stream recognised as
    // that copy (duplicate: true). A line from before the field was logged counts as stored.
    expect(read("listener_handoffs_to session-planner").text).toBe("3");
    expect(read("listener_stored_handoffs_to session-planner").text).toBe("1");
    expect(read("listener_stored_handoffs_to session-tester-old").text).toBe("2");
    expect(read("listener_stored_handoffs_to session-never").text).toBe("0");
    expect(read("evidence=$empty_evidence; listener_stored_handoffs_to session-planner")).toEqual({
      code: 0,
      text: "0",
    });
  });
});

describe("the listener's answers", () => {
  const controller = "legion-e2eal-controller-4242";
  const lanes = "notifications.legion.alf4242.controller notifications.legion.alp4242.controller";
  /** The answer to the controller stand-in's registration once it holds both controller roles. */
  const registered = JSON.stringify({
    session_id: controller,
    machine_id: "legion-e2eal-1",
    dir: "/work",
    topics: [
      `notifications.agent.${controller}`,
      "notifications.legion.alf4242.controller",
      "notifications.legion.alp4242.controller",
      "notifications.role.legion-alf4242-controller",
      "notifications.role.legion-alp4242-controller",
    ],
    updated_at: 1794867234870,
    delivery: {
      mode: "consumer",
      subject: `notifications.envoy.deliver.${controller}`,
      stream_id: "stream-1",
      consumer_start: 12,
    },
  });

  test("consumer_registration reads the consumer a registration was given and the topics it stored", () => {
    expect(run(`consumer_registration ${shq(registered)} ${controller} ${lanes}`).code).toBe(0);
    expect(run(`consumer_registration ${shq(registered)} ${controller}`).code).toBe(0);
    // The registration of a session holding no controller role: the listener left both lanes out
    // of what it stored, with a warning, and still gave the session its consumer.
    const laneless = JSON.stringify({
      ...JSON.parse(registered),
      topics: [`notifications.agent.${controller}`],
      warnings: ["notifications.legion.alf4242.controller was not registered: …"],
    });
    expect(run(`consumer_registration ${shq(laneless)} ${controller}`).code).toBe(0);
    expect(run(`consumer_registration ${shq(laneless)} ${controller} ${lanes}`).code).toBe(1);
    // The listener's refusal of a registration that asks for no consumer (main's #2603) names no
    // delivery at all, which is what the rig's self-subscribed stand-ins were answered.
    const refused = JSON.stringify({
      error: "this listener delivers to a session only from its own consumer",
      reason: "not_requested",
    });
    expect(run(`consumer_registration ${shq(refused)} ${controller}`).code).toBe(1);
    // A consumer relayed on another session's subject is no delivery the inbox reads.
    expect(run(`consumer_registration ${shq(registered)} legion-e2eal-asker-4242`).code).toBe(1);
    const elsewhere = JSON.stringify({
      ...JSON.parse(registered),
      delivery: { mode: "consumer", subject: "notifications.envoy.deliver.another-session" },
    });
    expect(run(`consumer_registration ${shq(elsewhere)} ${controller}`).code).toBe(1);
    expect(run(`consumer_registration '' ${controller}`).code).toBe(1);
    // A body that is no JSON at all (curl's own error text) is no registration either.
    expect(run(`consumer_registration 'not json' ${controller}`).code).not.toBe(0);
  });

  test("stored_for_stopped reads a send stored for a stopped session, never a hand-off", () => {
    const stored = readFileSync(join(data, "a13", "branch", "files", "send.body"), "utf8").trim();
    expect(run(`stored_for_stopped ${shq(stored)} session-architect`).code).toBe(0);
    expect(run(`stored_for_stopped ${shq(stored)} session-planner`).code).toBe(1);
    // A send handed to the stopped agent's daemon names its recipient and no state.
    const handedOff = JSON.stringify({ ...JSON.parse(stored), state: "" });
    expect(run(`stored_for_stopped ${shq(handedOff)} session-architect`).code).toBe(1);
    const refused = `{"error":"no live session session-architect","state":"gone"}`;
    expect(run(`stored_for_stopped '${refused}' session-architect`).code).toBe(1);
    expect(run(`stored_for_stopped '' session-architect`).code).toBe(1);
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

  test("daemon_call_for reads the first call that holds a text, and daemon_request_for its body", () => {
    // The second answer's request carries the first exchange too; the first call to hold its
    // question is still the first answer's.
    expect(read(`daemon_call_for "A10-1)" | jq .ordinal`).text).toBe("2");
    expect(read(`daemon_request_for "A10-1)" | jq '.messages | length'`).text).toBe("1");
    expect(read(`daemon_call_for "A10-2)" | jq .ordinal`).text).toBe("3");
    // A question asked again under a marker of its own is not the second question.
    expect(read(`daemon_call_for "A10-2r1)" | jq -c '[.ordinal, .mode]'`).text).toBe(
      '[4,"stand-in"]'
    );
    expect(read(`daemon_call_for "A10-9)"`).text).toBe("null");
    expect(read(`store=postgres; daemon_call_for "A10-1)"`).text).toBe("null");
    expect(read(`evidence=$empty_evidence; daemon_call_for "A10-1)"`)).toEqual({
      code: 0,
      text: "null",
    });
    expect(read(`evidence=$bare_evidence; daemon_request_for "A10-1)"`)).toEqual({
      code: 0,
      text: "null",
    });
  });

  test("call_organization reads the organization that served a forwarded call, and none of a stand-in", () => {
    expect(read(`call_organization "$(daemon_call_for "A10-1)")"`).text).toBe(
      "00000000-0000-4000-8000-00000000000a"
    );
    expect(read(`call_organization "$(daemon_call_for "A10-2)")"`).text).toBe(
      "00000000-0000-4000-8000-00000000000b"
    );
    // A stand-in's answer names no organization, and neither does a record with no answer's
    // headers at all (every earlier record of the fixture).
    expect(read(`call_organization "$(daemon_call_for "A10-2r1)")"`)).toEqual({
      code: 0,
      text: "",
    });
    expect(read(`call_organization "$(daemon_call_for legion-answer:d4d4d4d4e5e5e5e5)"`)).toEqual({
      code: 0,
      text: "",
    });
    expect(read(`call_organization "$(daemon_call_for "A10-9)")"`)).toEqual({ code: 0, text: "" });
    expect(read('call_organization ""')).toEqual({ code: 0, text: "" });
    expect(read("call_organization 'not a record'")).toEqual({ code: 0, text: "" });
  });

  // Proof run 44095624771 (the deployment repository, at 73cbecf0): under the files store six
  // answers strictly alternated two organizations, A10-1 5fd73c3e, A10-2 d0529c17, A10-2r1
  // 5fd73c3e and so on, and each read the cache its own organization's answer before it wrote:
  // A10-2r1 read the 3184 tokens A10-1 wrote. A pairing of neighbours found none and failed the
  // leg on a cache that was read.
  test("same_organization_pair pairs an answer with the latest earlier one its organization served", () => {
    const [a, b] = ["5fd73c3e", "d0529c17"];
    expect(read(`same_organization_pair ${a} ${b} ${a} ${b} ${a} ${b}`)).toEqual({
      code: 0,
      text: "0 2",
    });
    expect(read(`same_organization_pair ${a} ${a}`)).toEqual({ code: 0, text: "0 1" });
    // The latest earlier answer of the organization, not the first.
    expect(read(`same_organization_pair ${a} ${b} c ${b}`)).toEqual({ code: 0, text: "1 3" });
    // A route that names no organization: its answers share one.
    expect(read('same_organization_pair "" ""')).toEqual({ code: 0, text: "0 1" });
    expect(read(`same_organization_pair ${a} ${b}`).code).toBe(1);
    expect(read(`same_organization_pair ${a} ""`).code).toBe(1);
    expect(read(`same_organization_pair ${a}`).code).toBe(1);
    expect(read("same_organization_pair").code).toBe(1);
  });

  test("request_effort reads the effort a request carries, wherever it stands", () => {
    expect(read('request_effort "$(first_request_after claude-files 0)"').text).toBe("medium");
    expect(read('request_effort "$(first_request_after agents 0)"').text).toBe("");
    expect(read('request_effort ""')).toEqual({ code: 0, text: "" });
  });

  test("an OpenAI Responses call is a model call, and the preflight's one-line input is none", () => {
    expect(read("evidence=$gpt_evidence; proxy_count agents-gpt").text).toBe("1");
    expect(read("evidence=$gpt_evidence; proxy_ordinal agents-gpt").text).toBe("2");
    expect(read("evidence=$gpt_evidence; proxy_count daemon-files").text).toBe("3");
    expect(read("evidence=$gpt_evidence; proxy_count gpt-preflight").text).toBe("0");
    // The resumed merger's first model call, past its GET /v1/models listing.
    expect(
      read("evidence=$gpt_evidence; first_request_after agents-gpt 0 | jq -r .model").text
    ).toBe("gpt-6.1-sol");
    expect(read("evidence=$gpt_evidence; first_request_after agents-gpt 1").text).toBe("null");
  });

  test("answer_calls reads one answer's calls, never a later answer's that carry its question", () => {
    const ordinals = (args: string) =>
      `evidence=$gpt_evidence; answer_calls daemon-files ${args} | jq -c 'map(.ordinal)'`;
    expect(read(ordinals("'A10-gpt-1)' 'A10-gpt-2)'"))).toEqual({ code: 0, text: "[1,2]" });
    // Without the next question's marker, the second answer's call, which carries the first
    // exchange and its question, reads as the first answer's too.
    expect(read(ordinals("'A10-gpt-1)'")).text).toBe("[1,2,3]");
    expect(read(ordinals("'A10-gpt-2)'")).text).toBe("[3]");
    expect(read(ordinals("'A10-gpt-9)'")).text).toBe("[]");
    expect(read("evidence=$bare_evidence; answer_calls daemon-files 'A10-gpt-1)'")).toEqual({
      code: 0,
      text: "[]",
    });
  });

  test("calls_on_model, calls_efforts and efforts_all read every call's API, model and effort", () => {
    const first = `evidence=$gpt_evidence; calls=$(answer_calls daemon-files 'A10-gpt-1)' 'A10-gpt-2)')`;
    expect(run(`${first}; calls_on_model "$calls" /v1/responses gpt-6.1-sol`).code).toBe(0);
    expect(run(`${first}; calls_on_model "$calls" /v1/responses gpt-5`).code).toBe(1);
    expect(run(`${first}; calls_on_model "$calls" /v1/messages gpt-6.1-sol`).code).toBe(1);
    expect(run("calls_on_model '[]' /v1/responses gpt-6.1-sol").code).toBe(1);
    expect(run("calls_on_model '' /v1/responses gpt-6.1-sol").code).toBe(1);
    expect(read(`${first}; calls_efforts "$calls"`).text).toBe('["high","high"]');
    expect(run(`${first}; efforts_all "$calls" high`).code).toBe(0);
    expect(run(`${first}; efforts_all "$calls" medium`).code).toBe(1);
    // One call of the answer at no effort is an answer not made at the conversation's.
    const bare = `${first}; calls=$(jq -c '.[1].request_body |= del(.reasoning)' <<<"$calls")`;
    expect(read(`${bare}; calls_efforts "$calls"`).text).toBe('["high",null]');
    expect(run(`${bare}; efforts_all "$calls" high`).code).toBe(1);
    expect(read("calls_efforts ''")).toEqual({ code: 0, text: "[]" });
    expect(run("efforts_all '[]' high").code).toBe(1);
  });

  test("reasoning_replayed reads whether a request carries each reasoning item the transcript stores", () => {
    const request = (ordinal: number) =>
      `request=$(jq -c 'select(.label == "daemon-files" and .ordinal == ${ordinal}) | .request_body' "$gpt_evidence/model-calls.jsonl")`;
    const replayed = 'reasoning_replayed "$(conversation_text gpt)" "$request"';
    expect(read(`${request(1)}; ${replayed}`)).toEqual({ code: 0, text: "2 2" });
    // The second answer's first request carries them too, after the first answer's own.
    expect(read(`${request(3)}; ${replayed}`).text).toBe("2 2");
    // An item replayed with other encrypted content than the transcript stored, or not at all, is
    // named by its id.
    const other = `${request(1)}; request=$(jq -c '.input |= map(if .id? == "rs_two" then .encrypted_content = "gAAAAAB-rs-two-other" else . end)' <<<"$request")`;
    expect(read(`${other}; ${replayed}`).text).toBe("1 2 rs_two");
    const dropped = `${request(1)}; request=$(jq -c '.input |= map(select(.id? != "rs_one"))' <<<"$request")`;
    expect(read(`${dropped}; ${replayed}`).text).toBe("1 2 rs_one");
    // A transcript that stores none leaves nothing to replay.
    expect(
      read(`${request(1)}; reasoning_replayed "$(conversation_text gpt_plain)" "$request"`).text
    ).toBe("0 0");
    expect(read(`reasoning_replayed "$(conversation_text gpt)" ''`)).toEqual({
      code: 0,
      text: "0 2 rs_one,rs_two",
    });
    expect(read(`reasoning_replayed "$(conversation_text gpt)" 'not json'`)).toEqual({
      code: 0,
      text: "0 2 rs_one,rs_two",
    });
    expect(read("reasoning_replayed '' ''")).toEqual({ code: 0, text: "0 0" });
  });

  // Measured through the model gateway on 2026-11-18: a streamed Responses answer names one
  // reasoning item's encrypted content three times, a shorter one in `response.output_item.added`
  // and two of one length that differ in `response.output_item.done` and `response.completed`, and
  // the next step carried the done event's byte for byte. The fixture's three differ the same way,
  // so a read of `response.completed` fails the passing case here.
  test("reasoning_carried reads what an earlier step returned off its output_item.done event", () => {
    const first = `evidence=$gpt_evidence; calls=$(answer_calls daemon-files 'A10-gpt-1)' 'A10-gpt-2)')`;
    expect(read(`${first}; reasoning_carried "$calls"`)).toEqual({ code: 0, text: "1 1" });
    // A later request carrying the item as `response.completed` or `output_item.added` holds it
    // carries nothing of what the step returned.
    for (const content of ["gAAAAAB-rs-a1-completed", "gAAAAAB-rs-a1-added"]) {
      const carrying = `${first}; calls=$(jq -c '.[1].request_body.input |= map(if .id? == "rs_a1" then .encrypted_content = "${content}" else . end)' <<<"$calls")`;
      expect(read(`${carrying}; reasoning_carried "$calls"`).text).toBe("0 1");
    }
    const dropped = `${first}; calls=$(jq -c '.[1].request_body.input |= map(select(.id? != "rs_a1"))' <<<"$calls")`;
    expect(read(`${dropped}; reasoning_carried "$calls"`).text).toBe("0 1");
    // An answer of one call owes nothing: what its last call returned no later call carries.
    expect(
      read(`evidence=$gpt_evidence; reasoning_carried "$(answer_calls daemon-files 'A10-gpt-2)')"`)
        .text
    ).toBe("0 0");
    expect(read("reasoning_carried '[]'")).toEqual({ code: 0, text: "0 0" });
    expect(read("reasoning_carried ''")).toEqual({ code: 0, text: "0 0" });
    expect(read("reasoning_carried 'not json'")).toEqual({ code: 0, text: "0 0" });
  });

  // An answer of three calls whose first returned reasoning: the loop carries it in both later
  // calls, so a read that judged only the call after it would pass a third call that dropped it.
  test("reasoning_carried holds every later call of an answer to what an earlier one returned", () => {
    const three = `evidence=$gpt_evidence; calls=$(answer_calls daemon-postgres 'A10-gpt-3)')`;
    expect(read(`${three}; jq length <<<"$calls"`).text).toBe("3");
    expect(read(`${three}; reasoning_carried "$calls"`)).toEqual({ code: 0, text: "2 2" });
    const third = `${three}; calls=$(jq -c '.[2].request_body.input |= map(select(.id? != "rs_p1"))' <<<"$calls")`;
    expect(read(`${third}; reasoning_carried "$calls"`).text).toBe("1 2");
  });

  // A chain the daemon's own loop answered through the model gateway on 2026-11-19 as A10's
  // GPT-6.1 leg asks it (testdata/answer-loop/model-calls-gpt-reasoning.jsonl), recorded by a
  // throwaway harness over answer.Answer on proof run 44461333495's merger conversation, its gh
  // calls answered with the bytes go.mod holds. Each id and encrypted content is a stand-in that
  // keeps which of them are equal, and each request keeps the input items the reads look at. The
  // two cache answers returned no reasoning in any step, as all four of that proof run's GPT-6.1
  // answers did; the third, asked gpt-reason-reversed, returned rs_r2 beside its gh call, and its
  // later request carried the done event's content.
  const chain = (n: number) =>
    `evidence=$gpt_reasoning_evidence; calls=$(answer_calls daemon-files 'A10-gpt-${n})' 'A10-gpt-${n + 1})')`;
  const carriedOf = (n: number) =>
    `"$(reasoning_carried "$(answer_calls daemon-files 'A10-gpt-${n})' 'A10-gpt-${n + 1})')")"`;
  const reasoningQuestions = (JSON.parse(readFileSync(liveQuestions, "utf8")) as QuestionsFile).a10
    .reasoning;

  test("reasoning_carried passes a recorded step that reasoned before its gh call, and no change of it", () => {
    const ordinals = [1, 2, 3].map(
      (n) => read(`${chain(n)}; jq -c 'map(.ordinal)' <<<"$calls"`).text
    );
    expect(ordinals).toEqual(["[1,2]", "[3,4]", "[5,6]"]);
    expect(read(`${chain(3)}; reasoning_carried "$calls"`)).toEqual({ code: 0, text: "1 1" });
    // The later request carrying the item as the stream's `response.completed` or
    // `output_item.added` holds it, or not at all, carries nothing of what the step returned.
    for (const content of ["gAAAAAB-rs-r2-completed", "gAAAAAB-rs-r2-added"]) {
      const changed = `${chain(3)}; calls=$(jq -c '.[1].request_body.input |= map(if .id? == "rs_r2" then .encrypted_content = "${content}" else . end)' <<<"$calls")`;
      expect(read(`${changed}; reasoning_carried "$calls"`).text).toBe("0 1");
    }
    const dropped = `${chain(3)}; calls=$(jq -c '.[1].request_body.input |= map(select(.id? != "rs_r2"))' <<<"$calls")`;
    expect(read(`${dropped}; reasoning_carried "$calls"`).text).toBe("0 1");
    // The reply that answer posted holds its question's keys.
    const reply = `reply=$(jq -r 'select(.ordinal == 6) | .response_body' "$gpt_reasoning_evidence/model-calls.jsonl" | sed -n 's/^data: //p' | jq -R -r 'fromjson? | select(.type == "response.output_item.done") | .item.content[0].text | fromjson | .text')`;
    expect(
      run(
        `${reply}; questions_file=${shq(liveQuestions)}; answer_holds gpt-reason-reversed "$reply"`
      ).code
    ).toBe(0);
  });

  test("reasoning_carried owes nothing for recorded answers whose steps returned no reasoning", () => {
    for (const n of [1, 2]) {
      expect(read(`${chain(n)}; reasoning_carried "$calls"`)).toEqual({ code: 0, text: "0 0" });
    }
  });

  // A call the provider cut for length, which the daemon makes once more (agentloop's
  // Request.CallAgainWhenCut): the call made again is sent the cut call's request byte for byte,
  // so nothing the cut call returned reaches a later request, and it owes nothing. The cut call
  // here returns a reasoning item and ends `response.incomplete`, as a Responses stream cut at
  // max_output_tokens ends. Inserted before the recorded answer's later call, with that call's
  // request, it is one more later request that carries rs_r2: 2 of 2, where reading the cut call's
  // rs_cut as owed would make it 2 of 3. Inserted before the recorded step that returned rs_r2,
  // with that step's request, it carries nothing and owes nothing: 1 of 1, not 1 of 3.
  test("reasoning_carried owes nothing for a call the provider cut, which the daemon made again", () => {
    const cutStream = [
      {
        type: "response.output_item.added",
        output_index: 0,
        item: {
          type: "reasoning",
          id: "rs_cut",
          summary: [],
          encrypted_content: "gAAAAAB-rs-cut-added",
        },
      },
      {
        type: "response.output_item.done",
        output_index: 0,
        item: {
          type: "reasoning",
          id: "rs_cut",
          summary: [],
          encrypted_content: "gAAAAAB-rs-cut-done",
        },
      },
      {
        type: "response.incomplete",
        response: {
          id: "resp_cut",
          status: "incomplete",
          incomplete_details: { reason: "max_output_tokens" },
          output: [
            {
              type: "reasoning",
              id: "rs_cut",
              summary: [],
              encrypted_content: "gAAAAAB-rs-cut-completed",
            },
          ],
        },
      },
    ]
      .map((event) => `data: ${JSON.stringify(event)}\n\n`)
      .join("");
    const cutBody = `${cutStream}data: [DONE]\n\n`;
    // The cut call is made with the request of the call at index AT, and goes before it.
    const withCut = (at: number) =>
      `${chain(3)}; calls=$(jq -c --argjson at ${at} --arg body ${shq(cutBody)} '.[:$at] + [.[$at] | .response_body = $body] + .[$at:]' <<<"$calls")`;
    expect(read(`${withCut(1)}; jq length <<<"$calls"`).text).toBe("3");
    expect(read(`${withCut(1)}; reasoning_carried "$calls"`)).toEqual({ code: 0, text: "2 2" });
    expect(read(`${withCut(0)}; reasoning_carried "$calls"`)).toEqual({ code: 0, text: "1 1" });
    // The skip is the cut call's alone: the step that returned rs_r2 still owes it to the call
    // made again, which fails when it drops it.
    const dropped = `${withCut(1)}; calls=$(jq -c '.[2].request_body.input |= map(select(.id? != "rs_r2"))' <<<"$calls")`;
    expect(read(`${dropped}; reasoning_carried "$calls"`).text).toBe("1 2");
  });

  test("carried_total sums every answer's carried and owed, adding nothing for a line of no two counts", () => {
    expect(read("carried_total '0 0' '1 1' '2 3'")).toEqual({ code: 0, text: "3 4" });
    expect(read("carried_total '' 'not counts' '1' '1 x' '2 2'")).toEqual({ code: 0, text: "2 2" });
    expect(read("carried_total")).toEqual({ code: 0, text: "0 0" });
  });

  // Whether A10's GPT-6.1 leg asks again. After the two recorded cache answers, which owe nothing,
  // it asks the list's first question; after the recorded third, which owed its first step's
  // reasoning, it asks nothing more, and nothing more when that answer's later request dropped it
  // either, since the clause then has a carry to judge, and fails it. Past the list it asks
  // nothing: that is the bound.
  test("a10_next_question asks the next reasoning question while no answer owed a carry", () => {
    const recorded = `evidence=$gpt_reasoning_evidence; questions_file=${shq(liveQuestions)}`;
    expect(read(`${recorded}; a10_next_question 0 ${carriedOf(1)} ${carriedOf(2)}`)).toEqual({
      code: 0,
      text: reasoningQuestions[0],
    });
    expect(
      read(`${recorded}; a10_next_question 1 ${carriedOf(1)} ${carriedOf(2)} ${carriedOf(3)}`)
    ).toEqual({ code: 0, text: "" });
    const next = (args: string) =>
      read(`questions_file=${shq(liveQuestions)}; a10_next_question ${args}`).text;
    expect(next("1 '0 0' '0 0' '0 0'")).toBe(reasoningQuestions[1]);
    expect(next("2 '0 0' '0 0' '0 0' '0 0'")).toBe(reasoningQuestions[2]);
    expect(
      next(`${reasoningQuestions.length} ${"'0 0' ".repeat(reasoningQuestions.length + 2)}`)
    ).toBe("");
    expect(next("1 '0 0' '0 0' '0 1'")).toBe("");
    expect(next("0")).toBe(reasoningQuestions[0]);
    // An ASKED that counts no questions asked asks nothing, and so does a questions file with no
    // a10 list, the fixture's.
    expect(next("'' '0 0' '0 0'")).toBe("");
    expect(next("-1 '0 0' '0 0'")).toBe("");
    expect(read("a10_next_question 0 '0 0' '0 0'")).toEqual({ code: 0, text: "" });
  });

  // The cost line sums an answer's calls, and a later call reads what an earlier call of the same
  // answer wrote: the cache read the second answer is judged by is its first call's own.
  test("call_cached_tokens reads one call's cached input tokens off its response", () => {
    const second = `evidence=$gpt_evidence; call=$(answer_calls daemon-files 'A10-gpt-2)' | jq -c '.[0]')`;
    expect(read(`${second}; call_cached_tokens "$call"`)).toEqual({ code: 0, text: "1164" });
    // A first call that read nothing reads 0, whatever later calls of its answer read: the
    // three-call answer's first call read nothing and its second 900, which its cost line sums.
    const first = `evidence=$gpt_evidence; call=$(answer_calls daemon-files 'A10-gpt-1)' 'A10-gpt-2)' | jq -c '.[0]')`;
    expect(read(`${first}; call_cached_tokens "$call"`).text).toBe("0");
    const three = (index: number) =>
      `evidence=$gpt_evidence; call=$(answer_calls daemon-postgres 'A10-gpt-3)' | jq -c '.[${index}]')`;
    expect(read(`${three(0)}; call_cached_tokens "$call"`).text).toBe("0");
    expect(read(`${three(1)}; call_cached_tokens "$call"`).text).toBe("900");
    // A response that is no stream: its own usage.
    const plain = '{"response_body":{"usage":{"input_tokens_details":{"cached_tokens":7}}}}';
    expect(read(`call_cached_tokens ${shq(plain)}`).text).toBe("7");
    expect(read("call_cached_tokens ''")).toEqual({ code: 0, text: "0" });
    expect(read("call_cached_tokens null")).toEqual({ code: 0, text: "0" });
    expect(read("call_cached_tokens 'not json'")).toEqual({ code: 0, text: "0" });
  });

  test("openai_effort words a thinking level as agentloop asks OpenAI for it", () => {
    expect(read("openai_effort high").text).toBe("high");
    expect(read("openai_effort xhigh").text).toBe("xhigh");
    expect(read("openai_effort off").text).toBe("none");
    expect(read("openai_effort ''")).toEqual({ code: 0, text: "none" });
  });

  // A23: each web_search call is one Messages call with Anthropic's web search server tool, its
  // reply's result blocks the searches that ran and the addresses they returned. The loop's own
  // model call offers the tool by name and is no search.
  test("search_calls reads each web search the daemon made, its query, its searches and its addresses", () => {
    const ws = "evidence=$web_search_evidence";
    expect(
      read(`${ws}; search_calls daemon-files | jq -c 'map({ordinal, query, searches, urls})'`).text
    ).toBe(
      '[{"ordinal":2,"query":"newest jq release","searches":1,"urls":["https://github.com/jqlang/jq/releases","https://jqlang.org/download/","https://jqlang.org/news/"]},{"ordinal":3,"query":"jq changelog","searches":0,"urls":[]}]'
    );
    expect(read(`${ws}; search_calls daemon-files | jq -c '.[0].usage'`).text).toBe(
      '{"input_tokens":1200,"output_tokens":160,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"server_tool_use":{"web_search_requests":1}}'
    );
    expect(read(`${ws}; search_calls daemon-postgres | jq -r '.[].query'`).text).toBe(
      "newest ripgrep release"
    );
    expect(read(`${ws}; search_calls agents`)).toEqual({ code: 0, text: "[]" });
    expect(read(`search_calls daemon-files`)).toEqual({ code: 0, text: "[]" });
    expect(read(`evidence=$empty_evidence; search_calls daemon-files`)).toEqual({
      code: 0,
      text: "[]",
    });
    expect(read(`evidence=$bare_evidence; search_calls daemon-files`)).toEqual({
      code: 0,
      text: "[]",
    });
  });

  test("search_call_for reads the call of one search by its query", () => {
    const calls = "evidence=$web_search_evidence; calls=$(search_calls daemon-files)";
    expect(read(`${calls}; search_call_for "$calls" 'newest jq release' | jq .ordinal`).text).toBe(
      "2"
    );
    expect(read(`${calls}; search_call_for "$calls" 'never searched'`)).toEqual({
      code: 0,
      text: "null",
    });
    expect(read(`search_call_for '[]' 'newest jq release'`)).toEqual({ code: 0, text: "null" });
    expect(read(`search_call_for '' 'newest jq release'`)).toEqual({ code: 0, text: "null" });
  });

  test("reply_urls reads every address a reply names, without what closes the sentence or the link", () => {
    const reply = shq(
      "jq 1.8.1, per https://github.com/jqlang/jq/releases. See [its news](https://jqlang.org/news/) and 'https://jqlang.org/download/'."
    );
    expect(read(`reply_urls ${reply}`).text).toBe(
      "https://github.com/jqlang/jq/releases\nhttps://jqlang.org/download/\nhttps://jqlang.org/news/"
    );
    expect(read(`reply_urls 'no address in it'`)).toEqual({ code: 0, text: "" });
    expect(read(`reply_urls ''`)).toEqual({ code: 0, text: "" });
  });

  // A23 judges that every address the answer cites came out of its searches, none reached with a
  // tool, not that its fact is right: an address the searches never returned fails, whatever the
  // fact, and so does one the answer's own gh or dispatch call names.
  test("url_among reads an address among a list of addresses, a trailing slash aside", () => {
    const urls = `'["https://jqlang.org/download/","https://jqlang.org/news"]'`;
    expect(run(`url_among https://jqlang.org/download ${urls}`).code).toBe(0);
    expect(run(`url_among https://jqlang.org/news/ ${urls}`).code).toBe(0);
    expect(run(`url_among https://example.com/jq ${urls}`).code).toBe(1);
    expect(run(`url_among https://jqlang.org/download '[]'`).code).toBe(1);
    expect(run(`url_among https://jqlang.org/download null`).code).toBe(1);
    expect(run(`url_among https://jqlang.org/download ''`).code).toBe(1);
  });

  test("tool_names_url reads an address named in an argument of a gh or dispatch call", () => {
    const lines = `'[{"msg":"answers: gh","args":["api","https://jqlang.org/news/"]},{"msg":"answers: gh","args":["release","view","1.8.1"]}]'`;
    expect(run(`tool_names_url https://jqlang.org/news ${lines}`).code).toBe(0);
    expect(run(`tool_names_url https://jqlang.org/news/ ${lines}`).code).toBe(0);
    expect(run(`tool_names_url https://jqlang.org/download ${lines}`).code).toBe(1);
    expect(run(`tool_names_url https://jqlang.org/news '[]'`).code).toBe(1);
    expect(run(`tool_names_url https://jqlang.org/news '[{"msg":"answers: gh"}]'`).code).toBe(1);
    expect(run(`tool_names_url https://jqlang.org/news ''`).code).toBe(1);
  });

  test("citation_faults names each address an answer cites that no search returned, or that a tool reached", () => {
    const results = `'["https://github.com/jqlang/jq/releases","https://jqlang.org/news/"]'`;
    const none = "'[]'";
    const faults = (reply: string, gh = none, dispatch = none) =>
      read(`citation_faults ${shq(reply)} ${results} ${gh} ${dispatch}`);
    // Every address cited is among the results of the searches, taken together, a slash aside.
    expect(faults("jq 1.8.1, per https://jqlang.org/news.")).toEqual({ code: 0, text: "" });
    expect(
      faults("See https://github.com/jqlang/jq/releases and https://jqlang.org/news/")
    ).toEqual({ code: 0, text: "" });
    // A second address outside the results fails, naming it.
    expect(faults("See https://jqlang.org/news/ and https://example.com/jq").text).toBe(
      "https://example.com/jq is not among the results of the answer's searches"
    );
    // An address among the results that a gh call of the answer, or a dispatch call, names in an
    // argument: the agent reached it with a tool.
    const gh = `'[{"args":["api","https://jqlang.org/news/"]}]'`;
    expect(faults("per https://jqlang.org/news/", gh).text).toBe(
      "https://jqlang.org/news/ is named by an answers: gh call of the answer"
    );
    const dispatch = `'[{"args":["read","--issue","ALX-1","https://jqlang.org/news/"]}]'`;
    expect(faults("per https://jqlang.org/news/", none, dispatch).text).toBe(
      "https://jqlang.org/news/ is named by an answers: dispatch call of the answer"
    );
    // An address outside the results that a tool also names breaks both, one a line.
    expect(
      read(
        `citation_faults 'per https://example.com/jq' ${results} '[{"args":["api","https://example.com/jq"]}]' ${none}`
      ).text
    ).toBe(
      "https://example.com/jq is not among the results of the answer's searches\nhttps://example.com/jq is named by an answers: gh call of the answer"
    );
    // A reply that cites no address, and searches that returned none, are at fault too.
    expect(faults("jq 1.8.1, no address").text).toBe("the reply cites no address");
    expect(faults("").text).toBe("the reply cites no address");
    expect(read(`citation_faults 'per https://jqlang.org/news/' '[]' ${none} ${none}`).text).toBe(
      "https://jqlang.org/news/ is not among the results of the answer's searches"
    );
  });

  // A23: the resumed agent's transcript holds each search's call. Any object holding the query is no
  // call: it is the web_search tool's, as Anthropic's tool_use and OpenAI's function_call carry it.
  test("request_holds_search needs a call of web_search, and not any object that holds the query", () => {
    const call = { type: "tool_use", id: "t1", name: "web_search", input: { query: "q" } };
    const request = (blocks: unknown[]) =>
      shq(
        JSON.stringify({
          messages: [
            { role: "assistant", content: blocks },
            {
              role: "user",
              content: [{ type: "tool_result", content: "Sources: https://x.example/a" }],
            },
          ],
        })
      );
    const holds = (blocks: unknown[]) =>
      run(`request_holds_search ${request(blocks)} q https://x.example/a`).code;
    expect(holds([call])).toBe(0);
    // The same query as another tool's input, a bare object and a text, are no call of web_search,
    // however well the address is held.
    expect(holds([{ ...call, name: "gh" }])).toBe(1);
    expect(holds([{ query: "q" }])).toBe(1);
    expect(holds([{ type: "text", text: "I searched for q" }])).toBe(1);
    expect(holds([])).toBe(1);
    // OpenAI's call carries its arguments as a string of JSON.
    const function_call = {
      type: "function_call",
      name: "web_search",
      arguments: JSON.stringify({ query: "q" }),
    };
    expect(holds([function_call])).toBe(0);
    expect(holds([{ ...function_call, name: "gh" }])).toBe(1);
    expect(holds([{ ...function_call, arguments: JSON.stringify({ query: "another" }) }])).toBe(1);
    expect(holds([{ ...function_call, arguments: "not json" }])).toBe(1);
  });

  // A23: a search's cost line is its call's tokens at the search model's price plus the fee for
  // each search it ran; a failed search's line is its tokens alone.
  test("search_line_recomputes reads a search's fee into its own line", () => {
    const pair = (seq: number) =>
      `daemon_log=$tools_log; boot_offset=0; evidence=$web_search_evidence; line=$(question_search_lines f1f1f1f1 | jq -c '.[${seq - 1}]'); call=$(search_call_for "$(search_calls daemon-files)" "$(jq -r .query <<<"$line")")`;
    expect(run(`${pair(1)}; search_line_recomputes "$line" "$call" 0.01`).code).toBe(0);
    expect(run(`${pair(2)}; search_line_recomputes "$line" "$call" 0.01`).code).toBe(0);
    // Another fee, and the line read against another search's call.
    expect(run(`${pair(1)}; search_line_recomputes "$line" "$call" 0.02`).code).toBe(1);
    expect(
      run(
        `${pair(1)}; other=$(search_call_for "$(search_calls daemon-files)" 'jq changelog'); search_line_recomputes "$line" "$other" 0.01`
      ).code
    ).toBe(1);
    expect(run(`${pair(1)}; search_line_recomputes "$line" null 0.01`).code).toBe(1);
    expect(run(`search_line_recomputes '' '' ''`).code).toBe(1);
  });

  // A23: the resumed agent's first request holds each search's call, its query as the call's
  // input, and its result, the address it returned; Anthropic's tool_use and OpenAI's
  // function_call alike. The question's own words naming the same thing are no call.
  test("request_holds_search reads a search's call and result in a resumed agent's request", () => {
    const claude = "evidence=$web_search_evidence; request=$(first_request_after agents 0)";
    expect(
      run(
        `${claude}; request_holds_search "$request" 'newest jq release' https://github.com/jqlang/jq/releases`
      ).code
    ).toBe(0);
    const gpt = "evidence=$web_search_evidence; request=$(first_request_after agents-gpt 0)";
    expect(
      run(
        `${gpt}; request_holds_search "$request" 'newest ripgrep release' https://github.com/BurntSushi/ripgrep/releases`
      ).code
    ).toBe(0);
    expect(
      run(
        `${claude}; request_holds_search "$request" 'What is the newest jq release' https://github.com/jqlang/jq/releases`
      ).code
    ).toBe(1);
    expect(
      run(`${claude}; request_holds_search "$request" 'newest jq release' https://example.com/jq`)
        .code
    ).toBe(1);
    expect(
      run(
        `${gpt}; request_holds_search "$request" 'newest jq release' https://github.com/jqlang/jq/releases`
      ).code
    ).toBe(1);
    expect(run(`${claude}; request_holds_search "$request" '' ''`).code).toBe(1);
    expect(
      run(`request_holds_search null 'newest jq release' https://github.com/jqlang/jq/releases`)
        .code
    ).toBe(1);
    expect(
      run(`request_holds_search '' 'newest jq release' https://github.com/jqlang/jq/releases`).code
    ).toBe(1);
  });
});

describe("the model route's answer", () => {
  test("preflight_refusal passes a completed response and names what any other answer said", () => {
    const body = (name: string) => `"$fixtures/${name}"`;
    expect(read(`preflight_refusal 200 ${body("gpt-preflight-completed.json")}`)).toEqual({
      code: 0,
      text: "",
    });
    // LiteLLM's answer to a key it does not know names the key's masked tail and its hash.
    const refused = read(`preflight_refusal 401 ${body("gpt-preflight-refused.json")}`);
    expect(refused.text).toBe(
      "answered 401: Authentication Error, Invalid proxy server token passed. Received API key = <left out>, key hash <left out>. Unable to find token in cache or `LiteLLM_VerificationTokenTable`"
    );
    expect(refused.text).not.toContain("wxyz");
    expect(refused.text).not.toContain("0123456789abcdef");
    expect(read(`preflight_refusal 200 ${body("gpt-preflight-incomplete.json")}`).text).toBe(
      "answered 200, response status incomplete"
    );
    // A completed response under another status is no model served either.
    expect(read(`preflight_refusal 500 ${body("gpt-preflight-completed.json")}`).text).toBe(
      "answered 500, response status completed"
    );
    expect(
      read(`preflight_refusal 'unreachable (curl exited 7)' "$bare_evidence/no-body.json"`)
    ).toEqual({
      code: 0,
      text: "unreachable (curl exited 7)",
    });
    expect(read('preflight_refusal 404 "$empty_log"')).toEqual({ code: 0, text: "answered 404" });
  });

  // A route that echoes the key it was sent: the key is taken out before the message is cut to 300
  // characters, so a key longer than the cut leaves no head of it behind (a gateway's JWT-length
  // bearer). Without the key the read cannot know it, and the cut keeps its head.
  test("preflight_refusal takes the key out of an echoing refusal before it cuts the message", () => {
    const key = `eyJ${"k".repeat(420)}`;
    const echoed = join(dir, "gpt-preflight-echo.json");
    writeFileSync(echoed, JSON.stringify({ error: { message: `Received=${key} is not a key` } }));
    const refusal = read(`preflight_refusal 401 ${shq(echoed)} ${shq(key)}`);
    expect(refusal).toEqual({ code: 0, text: "answered 401: Received=<the key> is not a key" });
    expect(read(`preflight_refusal 401 ${shq(echoed)}`).text).toContain(key.slice(0, 40));
    // A key the message does not hold changes nothing.
    expect(
      read(`preflight_refusal 401 "$fixtures/gpt-preflight-refused.json" ${shq(key)}`).text
    ).toBe(
      "answered 401: Authentication Error, Invalid proxy server token passed. Received API key = <left out>, key hash <left out>. Unable to find token in cache or `LiteLLM_VerificationTokenTable`"
    );
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

  // The list A10's GPT-6.1 leg asks from after its two cache questions: three questions, the
  // leg's bound, each a question of its own to the GPT-6.1 merger with keys the rig checks.
  test("the live file's a10.reasoning names three questions to the GPT-6.1 merger, each with keys", () => {
    const file = JSON.parse(readFileSync(liveQuestions, "utf8")) as QuestionsFile;
    const reasoning = file.a10.reasoning;
    expect(reasoning).toHaveLength(3);
    expect(new Set(reasoning).size).toBe(3);
    for (const id of reasoning) {
      expect(["gpt-repository-module", "gpt-repository-go"]).not.toContain(id);
      const question = file.questions.find((q) => q.id === id);
      expect(question?.agent).toBe("gpt");
      expect(question?.criteria).toContain("A10");
      expect(question?.must_contain?.length ?? 0).toBeGreaterThan(0);
    }
  });

  // A20 asks the tester a second question in A3's thread, and A21 the implementer it fails on an
  // issue in implementing. Each question is its row's alone and carries keys, and each key is a
  // fact of the task its agent was given and none of the question's own words, so an answer
  // holding them is one read from the agent's conversation.
  test("the live file asks A20's and A21's questions of agents whose tasks hold their keys", () => {
    const file = JSON.parse(readFileSync(liveQuestions, "utf8")) as QuestionsFile;
    const rows = [
      { id: "tester-thread-reply", criterion: "A20", agent: "tester", role: "tester" },
      {
        id: "failed-implementer-role",
        criterion: "A21",
        agent: "failed_implementer",
        role: "implementer",
      },
    ];
    for (const row of rows) {
      const question = file.questions.find((q) => q.id === row.id);
      expect(question?.criteria).toEqual([row.criterion]);
      expect(question?.agent).toBe(row.agent);
      const agent = file.agents[row.agent];
      expect(agent?.role).toBe(row.role);
      const tasks = `${agent?.task ?? ""} ${agent?.replacement_task ?? ""}`.toLowerCase();
      const keys = question?.must_contain ?? [];
      expect(keys.length).toBeGreaterThan(0);
      for (const key of keys) {
        expect(tasks).toContain(key.toLowerCase());
        expect(question?.text.toLowerCase()).not.toContain(key.toLowerCase());
      }
    }
  });

  // A22's answer is in a comment the proof person writes on the agent's issue after the agent
  // stopped, and in nothing the agent was ever told: the comment holds every key, and neither the
  // question nor any task of the agent holds one. A23's questions ask for a web search and name no
  // key, since the row judges that a search ran, that every address cited is among its searches'
  // results and that none was reached with gh or dispatch, not the fact. Each is about one public
  // news page that is not on GitHub, and its fact, address and published time are written down in
  // the file before the run.
  test("the live file holds A22's keys in its comment alone, and A23's questions ask for a search", () => {
    const file = JSON.parse(readFileSync(liveQuestions, "utf8")) as QuestionsFile;
    const late = file.questions.find((q) => q.id === "a22-late-comment");
    expect(late?.criteria).toEqual(["A22"]);
    const agent = file.agents[late?.agent ?? ""];
    expect(agent?.role).toBe("planner");
    const tasks = `${agent?.task ?? ""} ${agent?.replacement_task ?? ""}`.toLowerCase();
    const keys = late?.must_contain ?? [];
    expect(keys.length).toBeGreaterThan(0);
    for (const key of keys) {
      expect(late?.comment ?? "").toContain(key);
      expect(late?.text.toLowerCase()).not.toContain(key.toLowerCase());
      expect(tasks).not.toContain(key.toLowerCase());
    }
    for (const id of ["a23-web-search", "a23-web-search-gpt", "a23-fourth-search"]) {
      const question = file.questions.find((q) => q.id === id);
      expect(question?.criteria).toEqual(["A23"]);
      expect(question?.must_contain ?? []).toEqual([]);
    }
    // A23's two legs ask about two different news pages, neither on GitHub, published after the
    // models' training cutoffs, and no question names its page's address or its answer.
    const pages = ["a23-web-search", "a23-web-search-gpt"].map((id) => {
      const question = file.questions.find((q) => q.id === id);
      expect(question?.text).toContain("web_search");
      expect(question?.text).toContain("cite the address of the search result you used");
      expect(question?.url).toMatch(/^https:\/\/techcrunch\.com\/2026\/11\/\d\d\//);
      expect(new URL(question?.url ?? "").hostname).not.toContain("github");
      expect(question?.published).toMatch(/^2026-11-\d\dT\d\d:\d\d:\d\dZ$/);
      expect(question?.expected?.length).toBeGreaterThan(0);
      expect(question?.text).not.toContain(question?.url ?? "");
      return question?.url;
    });
    expect(new Set(pages).size).toBe(2);
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

  // A16's search is for the meaning, not a word: a line saying the loop or an agent has no
  // tools, which would contradict the loop's `gh` tool. Each line is read with both questions
  // files' phrases, the fixture's and the one the live proof reads.
  const questionFiles = [join(data, "questions.json"), join(e2e, "answer-loop", "questions.json")];
  const aboutTheLoop = [
    "the loop will never give the agent a tool",
    "The answer loop has no tools.",
    "a tool-less model loop",
    "an agent without tools",
  ];
  // Lines the old phrase list (`no tools|tool-less|toolless|without tools|never give\b`) hit
  // that say nothing of the loop's tools: an architect's rule about a child's label
  // (skills/legion-architect/SKILL.md), the outbox retry, an eval's arms
  // (skills/approval-review/SKILL.md) and Envoy addressing (skills/envoy/SKILL.md).
  const notAboutTheLoop = [
    "Never give such a child the `legion` label, and never clear its parent while the tree is live",
    "a row whose decision or publish fails counts the attempt and backs off by `retry.Delay`, as an event does, and is never given up, its `retry.DeadLetterAttempts`th failure logged at Error.",
    "one model call with no tools, so what the number supports is the",
    "- Never give a peer the `subagent` field from `envoy_whoami`: it names your host session and is not an address.",
  ];
  /** The texts a16_hit, with FILE's phrases, does not answer CODE for (0 a hit, 1 none). */
  function misread(file: string, texts: string[], code: number): string[] {
    return texts.filter(
      (text) => run(`questions_file=${shq(file)}; a16_hit ${shq(text)}`).code !== code
    );
  }

  test("a16_pattern joins the phrases with `|`", () => {
    for (const file of questionFiles) {
      const phrases: string[] = JSON.parse(readFileSync(file, "utf8")).a16.phrases;
      expect(read(`questions_file=${shq(file)}; a16_pattern`).text).toBe(phrases.join("|"));
    }
  });

  test("a16_hit finds a line saying the loop or an agent has no tools", () => {
    for (const file of questionFiles) expect(misread(file, aboutTheLoop, 0)).toEqual([]);
  });

  test("a16_hit finds no line that only shares a word with one", () => {
    for (const file of questionFiles) expect(misread(file, notAboutTheLoop, 1)).toEqual([]);
    expect(run(`a16_hit ''`).code).toBe(1);
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

  // A1 and A6 read the answer to a person's reply on an ask, and their controls the answer to the
  // control's own reply on the same thread, which still holds the answer the loop gave before: the
  // read takes only what was written after the reply it names.
  test("ask_reply_after prints the agent's reply on the thread after a person's reply", () => {
    expect(read("ask_reply_after ask-1 reply-person session-architect").text).toContain(
      "Keep the 7-minute TTL"
    );
    expect(read("ask_reply_after ask-3 reply-a1 session-architect").text).toContain(
      "COBALT-HERON-41"
    );
    // The control's reply, after which nothing was written: the earlier answer is not its answer.
    const control = read("ask_reply_after ask-3 reply-control session-architect");
    expect(control.text).toBe("");
    expect(control.code).not.toBe(0);
    expect(read("ask_reply_after ask-1 reply-agent session-architect").code).not.toBe(0);
    expect(read("ask_reply_after ask-1 reply-person session-planner").code).not.toBe(0);
    expect(read("ask_reply_after ask-1 reply-never session-architect").code).not.toBe(0);
    expect(read("ask_reply_after ask-2 reply-person session-architect").code).not.toBe(0);
    expect(read("ask_reply_after ask-404 reply-person session-architect").code).not.toBe(0);
    expect(read("ask_reply_after ask-1 '' session-architect").code).not.toBe(0);
  });

  // A7's control reads row A1's thread at the end of the run, after the controls resumed the
  // architect, which may post on its own ask thread. The reply to the person's reply is the loop's
  // answer, the one that carries its mark, not a later post of the same session: proof run
  // 45542053007 at 423b8bf3 read the resumed architect's own post of 05:37:05Z there, found no
  // mark, and failed A7's control under files.
  test("ask_reply_after prints the agent's answer to a person's reply, not a later post of the same agent", () => {
    const answer = read("ask_reply_after ask-5 reply-a1 session-architect");
    expect(answer.code).toBe(0);
    expect(answer.text).toContain("<!-- legion-answer:a5a5a5a5b6b6b6b6 -->");
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

  // A22: the comment the stopped agent's dispatch tool posted where it was asked is the one
  // comment carrying the run's marker, in the agent's own name, its body carrying the tool's mark,
  // `<!-- legion-answer:<question>.<seq> -->`; the loop's own reply carries no seq.
  test("issue_comments_in and one_tool_marked_comment_by read the comment the dispatch tool posted", () => {
    const held = (marker: string) =>
      `comments=$(issue_comments_in "$(dispatch_get issues/ALX-22/comments)" ${marker})`;
    expect(
      read(`${held("answer-loop-a22-4242-files")}; jq -c 'map(.id)' <<<"$comments"`).text
    ).toBe('["comment-220"]');
    expect(
      run(
        `${held("answer-loop-a22-4242-files")}; one_tool_marked_comment_by "$comments" session-planner`
      ).code
    ).toBe(0);
    // The loop's own mark with no seq, two comments carrying the marker, another session's.
    for (const marker of [
      "answer-loop-a22-4242-postgres",
      "answer-loop-a22-4242-twice",
      "answer-loop-a22-4242-other",
    ]) {
      expect(
        run(`${held(marker)}; one_tool_marked_comment_by "$comments" session-planner`).code
      ).toBe(1);
    }
    expect(
      run(
        `${held("answer-loop-a22-4242-other")}; one_tool_marked_comment_by "$comments" session-tester`
      ).code
    ).toBe(0);
    expect(
      read('issue_comments_in "$(dispatch_get issues/ALX-22/comments)" answer-loop-a22-never')
    ).toEqual({
      code: 0,
      text: "[]",
    });
    expect(
      read('issue_comments_in "$(dispatch_get issues/ALX-404/comments)" answer-loop-a22-4242-files')
    ).toEqual({
      code: 0,
      text: "[]",
    });
    expect(read("issue_comments_in '' answer-loop-a22-4242-files")).toEqual({
      code: 0,
      text: "[]",
    });
    expect(run("one_tool_marked_comment_by '[]' session-planner").code).toBe(1);
    expect(run("one_tool_marked_comment_by '' session-planner").code).toBe(1);
  });

  // A22: the tool refused the status write and the claim, so the issue's status and claim read the
  // same after the ask as before it.
  test("issue_standing and standing_unchanged read an issue's status and claim", () => {
    expect(read("issue_standing ALX-22")).toEqual({
      code: 0,
      text: '{"status":"backlog","claim":null}',
    });
    expect(
      run(`standing_unchanged "$(issue_standing ALX-22)" "$(issue_standing ALX-22)"`).code
    ).toBe(0);
    expect(
      run(`standing_unchanged "$(issue_standing ALX-22)" "$(issue_standing ALX-23)"`).code
    ).toBe(1);
    expect(read("issue_standing ALX-404")).toEqual({ code: 0, text: "null" });
    expect(read("issue_standing ALX-405")).toEqual({ code: 0, text: "null" });
    expect(run(`standing_unchanged null null`).code).toBe(1);
    expect(run(`standing_unchanged '' ''`).code).toBe(1);
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

  // Every read of a Dispatch comment is one read: GET comments/<id>, the comment with its delivery
  // records and every reply in its thread, oldest first. The rig's dispatch_get fails on an error
  // status and prints the error body; a read that failed prints nothing.
  test("comment_record prints Dispatch's record of a comment, and nothing for a read that failed", () => {
    expect(read("comment_record comment-100 | jq -r '.comment.id, (.replies | length)'").text).toBe(
      "comment-100\n5"
    );
    const refused = `dispatch_get() { printf '{"error":{"code":"NOT_FOUND"}}\\n'; return 22; }`;
    expect(read(`${refused}; comment_record comment-100`)).toEqual({ code: 0, text: "" });
    expect(read("comment_record comment-never")).toEqual({ code: 0, text: "" });
    expect(read("comment_record ''")).toEqual({ code: 0, text: "" });
  });

  // A3 read the mention's delivery off the comment's own POST answer, which Dispatch gives before
  // it settles the mention, so the delivery read pending in proof run 43996451813. The read is of
  // the comment as Dispatch holds it now.
  test("comment_delivered_to and one_delivery_in read Dispatch's delivery records", () => {
    expect(run("comment_delivered_to comment-77 session-tester").code).toBe(0);
    expect(run("comment_delivered_to comment-110 session-implementer").code).toBe(0);
    // Not settled yet, kept for a role with no session, sent to another session, or no record at
    // all: not delivered to the session asked of.
    expect(run("comment_delivered_to comment-80 session-tester").code).toBe(1);
    expect(run("comment_delivered_to comment-111 session-implementer").code).toBe(1);
    expect(run("comment_delivered_to comment-81 session-tester").code).toBe(1);
    expect(run("comment_delivered_to comment-77 session-planner").code).toBe(1);
    expect(run("comment_delivered_to comment-404 session-tester").code).toBe(1);
    expect(run("comment_delivered_to comment-never session-tester").code).toBe(1);
    expect(run("comment_delivered_to '' session-tester").code).toBe(1);
    const refused = `[{"state":"failed","error":"no live session legion-e2eal-nobody-4242"}]`;
    expect(run(`one_delivery_in '${refused}' failed`).code).toBe(0);
    expect(
      run(`one_delivery_in '${refused}' failed 'no live session legion-e2eal-nobody-4242'`).code
    ).toBe(0);
    // A failure of another kind is no refusal of the session the control names.
    const noHolder = `[{"state":"failed","error":"no holder for role legion-e2eal-nobody-4242-tester"}]`;
    expect(
      run(`one_delivery_in '${noHolder}' failed 'no live session legion-e2eal-nobody-4242'`).code
    ).toBe(1);
    // A delivery still pending, and one Dispatch keeps for a role's first holder (queued, no
    // session), are no failure.
    expect(run(`one_delivery_in '[{"state":"pending","error":null}]' failed`).code).toBe(1);
    expect(run(`one_delivery_in '[{"state":"queued","error":null}]' queued`).code).toBe(0);
    expect(run(`one_delivery_in '[{"state":"sent"}]' failed`).code).toBe(1);
    expect(run(`one_delivery_in '[{"state":"failed"},{"state":"failed"}]' failed`).code).toBe(1);
    expect(run(`one_delivery_in '[]' failed`).code).toBe(1);
    expect(run(`one_delivery_in '{"state":"failed"}' failed`).code).toBe(1);
    expect(run(`one_delivery_in '' failed`).code).toBe(1);
  });

  test("comment_deliveries reads a comment's deliveries as Dispatch holds them now", () => {
    expect(read("comment_deliveries comment-90").text).toBe(
      `[{"state":"failed","error":"no live session legion-e2eal-nobody-4242"}]`
    );
    expect(
      run(
        `one_delivery_in "$(comment_deliveries comment-90)" failed 'no live session legion-e2eal-nobody-4242'`
      ).code
    ).toBe(0);
    expect(read("comment_deliveries comment-91").text).toBe(`[{"state":"pending","error":null}]`);
    // Dispatch's error body, a comment it answers nothing for, and no comment id at all: no records.
    expect(read("comment_deliveries comment-404")).toEqual({ code: 0, text: "[]" });
    expect(read("comment_deliveries comment-never")).toEqual({ code: 0, text: "[]" });
    expect(read("comment_deliveries ''")).toEqual({ code: 0, text: "[]" });
  });

  // A3's note and A21 read a comment's delivery records whole: the target each was for, its state,
  // the session it pinned and its error.
  test("comment_delivery_records reads a comment's delivery records whole", () => {
    expect(read("comment_delivery_records comment-110").text).toBe(
      `[{"target":"role:ALX-11-implementer","state":"sent","session_id":"session-implementer","error":null}]`
    );
    expect(read("comment_delivery_records comment-111").text).toBe(
      `[{"target":"role:ALX-11-reviewer","state":"queued","session_id":null,"error":null}]`
    );
    // A record that pins no session yet names none.
    expect(read("comment_delivery_records comment-91").text).toBe(
      `[{"target":"role:legion-e2eal-nobody-4242-tester","state":"pending","session_id":null,"error":null}]`
    );
    // Dispatch's error body, a read that failed, a comment it answers nothing for, and no comment
    // id at all: no records.
    const refused = `dispatch_get() { printf '{"error":{"code":"NOT_FOUND"}}\\n'; return 22; }`;
    expect(read("comment_delivery_records comment-404")).toEqual({ code: 0, text: "[]" });
    expect(read(`${refused}; comment_delivery_records comment-110`)).toEqual({
      code: 0,
      text: "[]",
    });
    expect(read("comment_delivery_records comment-never")).toEqual({ code: 0, text: "[]" });
    expect(read("comment_delivery_records ''")).toEqual({ code: 0, text: "[]" });
  });

  // A21 judges the records it read: its mention's one delivery `sent` to the implementer's
  // session, and its control's one delivery `queued` naming no session, which is how Dispatch
  // keeps a mention of a role the registry holds no entry for, for the role's first holder.
  test("one_delivery_for reads one delivery in a state, naming a session or none", () => {
    const records = (id: string) => `"$(comment_delivery_records ${id})"`;
    expect(run(`one_delivery_for ${records("comment-110")} sent session-implementer`).code).toBe(0);
    expect(run(`one_delivery_for ${records("comment-111")} queued ''`).code).toBe(0);
    expect(run(`one_delivery_for '[{"state":"queued"}]' queued ''`).code).toBe(0);
    // Another session, a session where none was wanted, none where one was, and another state.
    expect(run(`one_delivery_for ${records("comment-110")} sent session-tester`).code).toBe(1);
    expect(run(`one_delivery_for ${records("comment-110")} sent ''`).code).toBe(1);
    expect(run(`one_delivery_for ${records("comment-111")} queued session-implementer`).code).toBe(
      1
    );
    expect(run(`one_delivery_for ${records("comment-110")} queued session-implementer`).code).toBe(
      1
    );
    expect(run(`one_delivery_for ${records("comment-91")} queued ''`).code).toBe(1);
    const twice = `'[{"state":"sent","session_id":"s"},{"state":"sent","session_id":"s"}]'`;
    expect(run(`one_delivery_for ${twice} sent s`).code).toBe(1);
    // No records, a list that is none, and no list at all.
    expect(run(`one_delivery_for '[]' queued ''`).code).toBe(1);
    expect(run(`one_delivery_for '{"state":"queued"}' queued ''`).code).toBe(1);
    expect(run(`one_delivery_for '' queued ''`).code).toBe(1);
  });

  // A20 replies under the loop's own answer in A3's thread and reads the second answer there off
  // the thread as Dispatch holds it now (GET comments/<head>, every reply under the head, oldest
  // first): one session's replies in it, the first of them (the answer the person replies under),
  // how many there are, and the one written under a given comment, which Dispatch keeps as the
  // reply's `answers` beside the thread's head (`reply_to`).
  test("thread_replies_by reads one session's replies in a comment's thread, oldest first", () => {
    const tester = read("thread_replies_by comment-100 session-tester");
    expect(tester.code).toBe(0);
    expect(JSON.parse(tester.text)).toEqual([
      {
        id: "comment-101",
        answers: "comment-100",
        body: "VIOLET-OTTER-88, and 20 of 20 passed. <!-- legion-answer:c3c3c3c3d4d4d4d4 -->",
      },
      {
        id: "comment-105",
        answers: "comment-102",
        body: "TestQuartzLantern, under VIOLET-OTTER-88. <!-- legion-answer:e5e5e5e5f6f6f6f6 -->",
      },
    ]);
    expect(read("thread_replies_by comment-100 session-planner | jq -r '.[].id'").text).toBe(
      "comment-104"
    );
    expect(read("thread_replies_by comment-100 session-architect")).toEqual({
      code: 0,
      text: "[]",
    });
    // A thread with no replies, Dispatch's error body, a comment it answers nothing for, and no
    // comment id at all: no replies.
    expect(read("thread_replies_by comment-90 session-tester")).toEqual({ code: 0, text: "[]" });
    expect(read("thread_replies_by comment-404 session-tester")).toEqual({ code: 0, text: "[]" });
    expect(read("thread_replies_by comment-never session-tester")).toEqual({ code: 0, text: "[]" });
    expect(read("thread_replies_by '' session-tester")).toEqual({ code: 0, text: "[]" });
  });

  test("first_reply_id, reply_count and reply_answering read those replies", () => {
    const replies = "replies=$(thread_replies_by comment-100 session-tester)";
    expect(read(`${replies}; first_reply_id "$replies"`).text).toBe("comment-101");
    expect(read(`${replies}; reply_count "$replies"`).text).toBe("2");
    expect(read(`${replies}; reply_answering "$replies" comment-102`).text).toContain(
      "TestQuartzLantern"
    );
    // The first reply is the one written under the head; none of the tester's is written under
    // the control, nor under a comment named by no id.
    expect(read(`${replies}; reply_answering "$replies" comment-100`).text).toContain("20 of 20");
    expect(read(`${replies}; reply_answering "$replies" comment-103`).code).not.toBe(0);
    expect(read(`${replies}; reply_answering "$replies" ''`).code).not.toBe(0);
    // No replies, and a list that is none, hold no id, no count and no answer.
    for (const none of ["'[]'", "''", `'{"error":"comment not found"}'`]) {
      expect(read(`first_reply_id ${none}`)).toEqual({ code: 0, text: "" });
      expect(read(`reply_count ${none}`)).toEqual({ code: 0, text: "0" });
      expect(read(`reply_answering ${none} comment-102`).code).not.toBe(0);
    }
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

  // A21 asks the role mention of a failed implementer only once the registry's entry for its
  // session reads under the role token the mention names. The route answers the entry inside
  // `agent`, or bare. An entry the daemon of 043f9c67 wrote `released` reads the same, since no
  // reader takes that field any more.
  test("registry_entry_of reads an entry under its role token", () => {
    const entry = (fields: string) =>
      `'{"agent":{"session_id":"session-implementer","role_token":"ALX-11-implementer","issue":"ALX-11","live":false${fields}}}'`;
    const holds = (answer: string, session = "session-implementer", token = "ALX-11-implementer") =>
      run(`registry_entry_of ${answer} ${session} ${token}`).code;
    expect(holds(entry(""))).toBe(0);
    expect(holds(entry(`,"released":true`))).toBe(0);
    expect(
      holds(
        `'{"session_id":"session-implementer","role_token":"ALX-11-implementer","answering":true}'`
      )
    ).toBe(0);
    // Another session's entry, another role's, and no entry at all.
    expect(holds(entry(""), "session-other")).toBe(1);
    expect(holds(entry(""), "session-implementer", "ALX-11-tester")).toBe(1);
    expect(holds(`'{"error":"no such agent"}'`)).toBe(1);
    expect(holds("null")).toBe(1);
    expect(holds("''")).toBe(1);
  });
});

describe("GitHub", () => {
  test("pr_comment_listing reads GitHub's own listing, and anything that is none as null", () => {
    expect(read("pr_comment_listing 41 | jq length").text).toBe("2");
    // GitHub's error body is an object and no listing; so is a gh call that printed nothing, and
    // one that failed. None of the three is an empty list of comments.
    expect(read("pr_comment_listing 42")).toEqual({ code: 0, text: "null" });
    expect(read("smoke_gh() { :; }; pr_comment_listing 41")).toEqual({ code: 0, text: "null" });
    expect(read("smoke_gh() { return 1; }; pr_comment_listing 41")).toEqual({
      code: 0,
      text: "null",
    });
  });

  test("refused_write_absent reads the refused write's absence off a listing it read", () => {
    const kept = "answer-loop-a17-4242-files";
    const refused = "answer-loop-leak-4242";
    expect(run(`refused_write_absent "$(pr_comment_listing 41)" ${kept} ${refused}`).code).toBe(0);
    // The write was made after all: the comment carrying the refusal marker is on the pull request.
    expect(run(`refused_write_absent "$(pr_comment_listing 44)" ${kept} ${refused}`).code).toBe(1);
    // A listing that examined nothing, and one of some other pull request: neither holds the
    // comment the leg posted minutes before, so neither shows the refused write's absence. jq's
    // `all` over an empty array is true, which is what this half of the read is here to catch.
    expect(run(`refused_write_absent "$(pr_comment_listing 46)" ${kept} ${refused}`).code).toBe(1);
    expect(run(`refused_write_absent "$(pr_comment_listing 45)" ${kept} ${refused}`).code).toBe(1);
    // A listing nobody could read is no absence: an error body, a failed call, no listing at all.
    expect(run(`refused_write_absent "$(pr_comment_listing 42)" ${kept} ${refused}`).code).toBe(1);
    expect(run(`refused_write_absent null ${kept} ${refused}`).code).toBe(1);
    expect(run(`refused_write_absent '' ${kept} ${refused}`).code).toBe(1);
  });

  test("leak_reason_quoted reads the check's own reason in the agent's reply", () => {
    const reason = "the answer loop proof stand-in refuses this call";
    expect(
      run(
        `leak_reason_quoted 'The tool answered: refused: LEAK CHECK: refusing the gh call: the shared public leak check refused it; LEAK CHECK: refusing: ${reason}' '${reason}'`
      ).code
    ).toBe(0);
    // A reply that says only that something refused it carries no reason of the check's.
    expect(
      run(`leak_reason_quoted 'My gh call was refused, so I posted nothing.' '${reason}'`).code
    ).toBe(1);
    expect(run(`leak_reason_quoted '' '${reason}'`).code).toBe(1);
  });

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

  // A19's control asks the reviewer again after A19's own approval stands on the pull request: only
  // an approval submitted since the control's ask is its answer.
  test("reviewer_approvals with a time reads only the approvals submitted since then", () => {
    const since = (time: string, session = "session-reviewer") =>
      `reviewer_approvals 41 ${session} ${time} | jq -c 'map(.submitted_at)'`;
    expect(read(since("2026-11-24T12:04:00Z")).text).toBe('["2026-11-24T12:05:00Z"]');
    expect(read(since("2026-11-24T12:05:00Z")).text).toBe('["2026-11-24T12:05:00Z"]');
    expect(read(since("2026-11-24T12:06:00Z")).text).toBe("[]");
    expect(read(since("2026-11-24T12:04:00Z", "another-session")).text).toBe("[]");
    expect(read(since("2026-11-24T12:00:00Z", "another-session")).text).toBe(
      '["2026-11-24T12:01:00Z"]'
    );
    expect(read("reviewer_approvals 42 session-reviewer 2026-11-24T12:00:00Z")).toEqual({
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

  test("tree_claims_parked holds only while the tree has a claim and every one of them is stopped", () => {
    const parked = `{"claims":[{"tree":"ALX-1","state":"suspended"},{"tree":"ALX-1","state":"retired"},{"tree":"ALX-9","state":"idle"}]}`;
    const started = `{"claims":[{"tree":"ALX-1","state":"suspended"},{"tree":"ALX-1","state":"idle"}]}`;
    expect(run(`tree_claims_parked '${parked}' ALX-1`).code).toBe(0);
    expect(run(`tree_claims_parked '${started}' ALX-1`).code).toBe(1);
    // A failed claim is no parked one: A1's tree reads suspended or retired alone.
    const failed = `{"claims":[{"tree":"ALX-1","state":"suspended"},{"tree":"ALX-1","state":"failed"}]}`;
    expect(run(`tree_claims_parked '${failed}' ALX-1`).code).toBe(1);
    // A listing that holds no claim of the tree examined nothing of it: jq's `all` over no claims
    // is true, which is what the read's first clause is there to catch.
    expect(run(`tree_claims_parked '{"claims":[]}' ALX-1`).code).toBe(1);
    expect(
      run(`tree_claims_parked '{"claims":[{"tree":"ALX-9","state":"suspended"}]}' ALX-1`).code
    ).toBe(1);
    expect(run(`tree_claims_parked '' ALX-1`).code).toBe(1);
  });

  // A21's tree holds a failed implementer and a suspended architect, and a role mention of the
  // implementer must start neither: every claim of the tree stands suspended, failed or retired.
  test("tree_claims_in holds only while the tree has a claim and each stands in a state given", () => {
    const states = "suspended failed retired";
    const stopped = `{"claims":[{"tree":"ALX-11","state":"failed"},{"tree":"ALX-11","state":"suspended"},{"tree":"ALX-12","state":"idle"}]}`;
    expect(run(`tree_claims_in '${stopped}' ALX-11 ${states}`).code).toBe(0);
    expect(
      run(`tree_claims_in '{"claims":[{"tree":"ALX-11","state":"retired"}]}' ALX-11 ${states}`).code
    ).toBe(0);
    for (const state of ["queued", "launching", "ready", "working", "idle"]) {
      const started = `{"claims":[{"tree":"ALX-11","state":"failed"},{"tree":"ALX-11","state":"${state}"}]}`;
      expect(run(`tree_claims_in '${started}' ALX-11 ${states}`).code).toBe(1);
    }
    // A claim in a state not given, and no state given at all.
    expect(run(`tree_claims_in '${stopped}' ALX-11 suspended retired`).code).toBe(1);
    expect(run(`tree_claims_in '${stopped}' ALX-11`).code).toBe(1);
    // A listing that holds no claim of the tree examined nothing of it: jq's `all` over no claims
    // is true, which is what the read's first clause is there to catch.
    expect(run(`tree_claims_in '{"claims":[]}' ALX-11 ${states}`).code).toBe(1);
    expect(
      run(`tree_claims_in '{"claims":[{"tree":"ALX-12","state":"failed"}]}' ALX-11 ${states}`).code
    ).toBe(1);
    expect(run(`tree_claims_in '' ALX-11 ${states}`).code).toBe(1);
  });

  // A21 judges the claims listing it read once: the implementer's claim stands failed, and the
  // role and state of each claim of the issue is what its note and its FAIL print.
  const a21Claims = `'{"claims":[{"token":"ALX-11-architect","tree":"ALX-11","issue":"ALX-11","role":"architect","state":"suspended"},{"token":"ALX-11-implementer","tree":"ALX-11","issue":"ALX-11","role":"implementer","state":"failed"},{"token":"ALX-12-tester","tree":"ALX-12","issue":"ALX-12","role":"tester","state":"idle"}]}'`;

  test("listed_claim_is reads one claim's state off the listing", () => {
    expect(run(`listed_claim_is ${a21Claims} ALX-11-implementer failed`).code).toBe(0);
    // Another state, another claim's, and a claim the listing does not hold.
    expect(run(`listed_claim_is ${a21Claims} ALX-11-implementer suspended`).code).toBe(1);
    expect(run(`listed_claim_is ${a21Claims} ALX-11-architect failed`).code).toBe(1);
    expect(run(`listed_claim_is ${a21Claims} ALX-11-reviewer failed`).code).toBe(1);
    // No claims, a listing that is none, and no listing at all.
    expect(run(`listed_claim_is '{"claims":[]}' ALX-11-implementer failed`).code).toBe(1);
    expect(run(`listed_claim_is '{}' ALX-11-implementer failed`).code).toBe(1);
    expect(run(`listed_claim_is '' ALX-11-implementer failed`).code).toBe(1);
  });

  test("issue_claim_roles prints the role and state of each claim of the issue", () => {
    expect(read(`issue_claim_roles ${a21Claims} ALX-11`).text).toBe(
      `[{"role":"architect","state":"suspended"},{"role":"implementer","state":"failed"}]`
    );
    expect(read(`issue_claim_roles ${a21Claims} ALX-13`)).toEqual({ code: 0, text: "[]" });
    // A listing that is none, and no listing at all: no claims, one line.
    expect(read(`issue_claim_roles '{}' ALX-11`)).toEqual({ code: 0, text: "[]" });
    expect(read(`issue_claim_roles 'not a listing' ALX-11`)).toEqual({ code: 0, text: "[]" });
    expect(read(`issue_claim_roles '' ALX-11`)).toEqual({ code: 0, text: "[]" });
  });
});

describe("A13: the branch's answer against the base commit's", () => {
  // Proof run 44095624771 built A13's base from main's tip at dispatch, a2b379d1, which carries
  // #4281's `requested_by.proven` and the branch did not, so the criterion read main's change as
  // the branch's. The base is the merge base of the revision and the checkout's head.
  test("merge_base_commit builds a base ahead of the branch at their merge base, and a base behind it as itself", () => {
    const repo = join(dir, "merge-base-repo");
    mkdirSync(repo, { recursive: true });
    const git = (...args: string[]) => {
      const result = Bun.spawnSync(
        ["git", "-C", repo, "-c", "user.name=rig", "-c", "user.email=rig@example.invalid", ...args],
        { env: { PATH: process.env.PATH ?? "", HOME: dir, GIT_CONFIG_NOSYSTEM: "1" } }
      );
      if (result.exitCode !== 0)
        throw new Error(`git ${args.join(" ")}: ${result.stderr.toString()}`);
      return result.stdout.toString().trim();
    };
    git("init", "-q", "-b", "main");
    git("commit", "-q", "--allow-empty", "-m", "the fork point");
    const fork = git("rev-parse", "HEAD");
    git("commit", "-q", "--allow-empty", "-m", "main moves on");
    const ahead = git("rev-parse", "HEAD");
    git("checkout", "-q", "-b", "branch", fork);
    git("commit", "-q", "--allow-empty", "-m", "the branch's change");
    const head = git("rev-parse", "HEAD");
    expect(read(`merge_base_commit ${repo} ${ahead}`)).toEqual({
      code: 0,
      text: `${fork} ${ahead} ${head}`,
    });
    expect(read(`merge_base_commit ${repo} main`)).toEqual({
      code: 0,
      text: `${fork} ${ahead} ${head}`,
    });
    expect(read(`merge_base_commit ${repo} ${fork}`)).toEqual({
      code: 0,
      text: `${fork} ${fork} ${head}`,
    });
    expect(read(`merge_base_commit ${repo} no-such-revision`).code).toBe(1);
    git("checkout", "-q", "--orphan", "unrelated");
    git("commit", "-q", "--allow-empty", "-m", "a history of its own");
    expect(read(`merge_base_commit ${repo} ${ahead}`).code).toBe(1);
  });

  test("a13_same reads the listener's two answers as the same but for what each call minted", () => {
    // Both sends were stored for the stopped session; they differ in their envelope's ids, its
    // dedupe key, its trace id and its issue time alone, as the proof run's captures did.
    expect(run("a13_same send.body").code).toBe(0);
    // Both role publishes were kept for the role's next holder, each until its own copy's hour
    // ends (`kept_until`), which is the field the ids alone did not cover.
    expect(run("a13_same publish.body").code).toBe(0);
    expect(run("evidence=$bare_evidence; a13_same send.body").code).toBe(1);
  });

  test("a13_same reads Dispatch's deliveries as the same but for the envelope each one minted", () => {
    expect(run("a13_same dispatch-session.json").code).toBe(0);
    // The branch's role delivery was queued for the stopped architect where the base's failed
    // `no holder`: a real difference, which no normalization hides.
    expect(run("a13_same dispatch-role.json").code).toBe(1);
  });

  test("a13_same compares a minted field by whether it is there at all, and a status byte for byte", () => {
    // The branch's publish carries no kept_until where the base's carries one.
    expect(run("evidence=$presence_evidence; a13_same publish.body").code).toBe(1);
    // The branch's delivery names no envelope where the base's names one.
    expect(run("evidence=$presence_evidence; a13_same dispatch-session.json").code).toBe(1);
    expect(run("a13_same send.status").code).toBe(0);
    expect(run("a13_same publish.status").code).toBe(1);
  });

  test("a13_same reads a probe that captured nothing as no answer, never as the same", () => {
    // `cmp` reads two empty files as equal: a probe that wrote no bytes on either stack.
    expect(run("evidence=$empty_evidence; a13_same send.body").code).toBe(1);
    // The branch captured its body and the base captured none.
    expect(run("evidence=$branch_only_evidence; a13_same send.body").code).toBe(1);
  });

  // A13's control: the same probe against the branch with `answers` on, compared with the base
  // commit's. A send handed to the daemon names no `state: stopped`, and the comparison fails.
  test("a13_same compares the control's capture with the base's as it compares the branch's", () => {
    const control = "evidence=$control_evidence";
    expect(run(`${control}; a13_same send.body control`).code).toBe(1);
    expect(run(`${control}; a13_same send.body`).code).toBe(0);
    expect(run(`${control}; a13_same send.status control`).code).toBe(0);
    expect(run(`${control}; a13_same dispatch-session.json control`).code).toBe(0);
    // No control probe at all is no capture to compare.
    expect(run("a13_same send.body control").code).toBe(1);
  });

  // The control passes on a difference, and a13_same reads a missing capture as one: so the control
  // first reads that its probe and the base's captured all six files.
  test("a13_captured reads every capture of a probe and of the base's, each holding bytes", () => {
    expect(run("evidence=$control_evidence; a13_captured control").code).toBe(0);
    expect(run("a13_captured branch").code).toBe(0);
    expect(run("a13_captured control").code).toBe(1);
    expect(run("evidence=$branch_only_evidence; a13_captured branch").code).toBe(1);
    expect(run("evidence=$empty_evidence; a13_captured branch").code).toBe(1);
    expect(run("evidence=$bare_evidence; a13_captured branch").code).toBe(1);
  });
});

// A23's search read judges one answer: the proxy's records of the architect's search and of its
// resumed request (the web-search fixture), the daemon's log of that answer (daemon-tools.log.txt:
// two search lines and the cost line of question f1f1f1f1) and an inbox holding the reply to
// event-ws. A case may add call lines of the answer's own gh or dispatch tool to the log, and may
// take the web_search call out of the resumed request.
type ProxyRecord = { label: string; request_body: { messages?: { content: unknown }[] } };

function answerOf(
  name: string,
  reply: string,
  options: { toolLines?: object[]; withoutSearchCall?: boolean } = {}
): { evidence: string; log: string } {
  const evidenceDir = join(dir, `answer-${name}`);
  mkdirSync(evidenceDir, { recursive: true });
  const records = readFileSync(join(data, "model-calls-web-search.jsonl"), "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line) as ProxyRecord);
  for (const record of records) {
    if (!options.withoutSearchCall || record.label !== "agents") continue;
    for (const message of record.request_body.messages ?? []) {
      if (!Array.isArray(message.content)) continue;
      message.content = message.content.filter(
        (block: { type?: string }) => block.type !== "tool_use"
      );
    }
  }
  writeFileSync(
    join(evidenceDir, "model-calls.jsonl"),
    `${records.map((record) => JSON.stringify(record)).join("\n")}\n`
  );
  writeFileSync(
    join(evidenceDir, "inbox.jsonl"),
    `${JSON.stringify({
      at: "2026-11-24T12:04:20.000Z",
      session: "asker-session",
      text: `In Reply To: event-ws\nMessage:\n${reply}\n\n<!-- legion-answer:f1f1f1f1 -->\n\nTopic: notifications.agent.asker-session`,
    })}\n`
  );
  const log = join(dir, `answer-${name}.log`);
  writeFileSync(
    log,
    `${readFileSync(join(data, "daemon-tools.log.txt"), "utf8")}\n${(options.toolLines ?? []).map((line) => JSON.stringify(line)).join("\n")}\n`
  );
  return { evidence: evidenceDir, log };
}

function searchRead(answer: { evidence: string; log: string }): { held: boolean; said: string } {
  const result = run(`${rig("said_text")}
${rig("a23_search_read")}
evidence=${shq(answer.evidence)}
daemon_log=${shq(answer.log)}
boot_offset=0
web_search_fee=0.01
verdict=failed
a23_search_read event-ws 0 agents architect && verdict=held
printf '%s\\n%s\\n' "$verdict" "$read_said"`);
  const [verdict, ...said] = result.out.trimEnd().split("\n");
  return { held: verdict === "held", said: said.join("\n") };
}

const toolLine = (msg: string, args: string[]) => ({
  time: "2026-11-24T12:04:06.000Z",
  level: "INFO",
  msg,
  question: "f1f1f1f1",
  seq: 1,
  session: "session-architect",
  args,
  exit: 0,
  ran: true,
});

describe("A23's search read, over one answer", () => {
  test("an answer that cites only what its search returned, with a web_search call in the resumed transcript, holds", () => {
    const held = searchRead(
      answerOf("held", "jq 1.8.1, per https://github.com/jqlang/jq/releases.")
    );
    expect(held.held).toBe(true);
    expect(held.said).toContain("what is wrong with the addresses the reply cites: nothing");
    // Every address the reply names, not one: two cited, both among the results.
    expect(
      searchRead(
        answerOf("both", "See https://github.com/jqlang/jq/releases and https://jqlang.org/news/")
      ).held
    ).toBe(true);
  });

  test("a reply that cites a second address outside the results fails, naming the address", () => {
    const outside = searchRead(
      answerOf("outside", "per https://github.com/jqlang/jq/releases and https://example.com/jq")
    );
    expect(outside.held).toBe(false);
    expect(outside.said).toContain(
      "https://example.com/jq is not among the results of the answer's searches"
    );
    const none = searchRead(answerOf("none", "jq 1.8.1, from memory"));
    expect(none.held).toBe(false);
    expect(none.said).toContain("the reply cites no address");
  });

  test("a reply that cites an address a gh or dispatch call line of the answer names fails, naming the tool", () => {
    const url = "https://jqlang.org/news/";
    const gh = searchRead(
      answerOf("gh", `per ${url}`, { toolLines: [toolLine("answers: gh", ["api", url])] })
    );
    expect(gh.held).toBe(false);
    expect(gh.said).toContain(`${url} is named by an answers: gh call of the answer`);
    const dispatch = searchRead(
      answerOf("dispatch", `per ${url}`, {
        toolLines: [toolLine("answers: dispatch", ["read", "--issue", "ALX-1", url])],
      })
    );
    expect(dispatch.held).toBe(false);
    expect(dispatch.said).toContain(`${url} is named by an answers: dispatch call of the answer`);
    // A gh call of the answer that names no address it cites leaves the answer as it was.
    expect(
      searchRead(
        answerOf("gh-other", `per ${url}`, {
          toolLines: [toolLine("answers: gh", ["api", "repos/jqlang/jq"])],
        })
      ).held
    ).toBe(true);
  });

  test("an answer whose resumed transcript holds no web_search call fails", () => {
    const without = searchRead(
      answerOf("without", "per https://github.com/jqlang/jq/releases.", {
        withoutSearchCall: true,
      })
    );
    expect(without.held).toBe(false);
    expect(without.said).toContain("holds a web_search call and its result for each search: no");
  });

  test("an answer with no search at all fails", () => {
    // The log of a question with no web_search line: nothing ran.
    const answer = answerOf("no-search", "per https://github.com/jqlang/jq/releases.");
    writeFileSync(
      answer.log,
      '{"time":"2026-11-24T12:04:10.000Z","level":"INFO","msg":"answers: answered","question":"f1f1f1f1","session":"session-architect","fault":"","calls":1}\n'
    );
    expect(searchRead(answer).held).toBe(false);
  });
});

describe("the control rows' reads", () => {
  // A control's parts are read in different phases: each says what it read under its own words, and
  // the control is held (it reads FAIL) only when a part's read held.
  test("read_failed keeps what each part said and marks a control held only when a read held", () => {
    const control = (body: string) => `declare -A control=()
${rig("read_failed")}
r_holds() { read_said='it holds'; return 0; }
r_fails() { read_said='it fails'; return 1; }
${body}`;
    const failed = run(
      control(`read_failed A9 'the first part' r_fails
read_failed A9 'the second part' r_fails
echo "said: \${control[files:A9.said]}"
echo "held: \${control[files:A9.held]:-no}"`)
    );
    expect(failed.code).toBe(0);
    expect(failed.out).toContain(
      "said: the first part failed: it fails; the second part failed: it fails"
    );
    expect(failed.out).toContain("held: no");
    const held = run(
      control(`read_failed A9 'the first part' r_fails
read_failed A9 'the second part' r_holds
read_failed A10 'another row' r_fails
echo "said: \${control[files:A9.said]}"
echo "held: \${control[files:A9.held]:-no} \${control[files:A10.held]:-no}"`)
    );
    expect(held.code).toBe(0);
    expect(held.out).toContain(
      "said: the first part failed: it fails; the second part held: it holds"
    );
    expect(held.out).toContain("held: 1 no");
  });

  // A7's control runs the running-agent leg's read that the loop posted nothing over row A1's
  // thread, where the loop answered a stopped agent: it finds the loop's mark there.
  test("a7_loop_posted_nothing finds the loop's mark in row A1's thread and none where it posted nothing", () => {
    const posted = (reply: string) =>
      run(
        `${rig("said_text")}\n${rig("a7_loop_posted_nothing")}\na7_loop_posted_nothing ${shq(reply)}`
      );
    expect(
      posted("The architect decided on 7 minutes. <!-- legion-answer:a1a1a1a1b2b2b2b2 -->").code
    ).toBe(1);
    expect(posted("No mark in what the thread holds").code).toBe(0);
    expect(posted("").code).toBe(0);
    const thread = `${rig("said_text")}\n${rig("a7_loop_posted_nothing")}
reply=$(ask_reply_after ask-1 reply-person session-architect) || reply=
a7_loop_posted_nothing "$reply" && echo quiet || echo "loop answered: $read_said"`;
    expect(run(thread).out).toContain(
      "loop answered: the loop's mark in what the thread holds: a1a1a1a1b2b2b2b2"
    );
  });

  test("a7_alone_read reads the asker's reply to the question beside the log and the conversation", () => {
    const alone = (sent: string) =>
      run(`${rig("said_text")}\n${rig("answers_line_count")}\n${rig("a7_loop_posted_nothing")}\n${rig("a7_alone_read")}
daemon_log=$empty_log; boot_offset=0
a7_alone_read ${shq(sent)} VIOLET-OTTER-88 0`).code;
    // The question reached the architect, which holds it; the loop logged and posted nothing.
    expect(alone("200 event-never")).toBe(0);
    // A reply of the loop's on the asker's topic, a refused send and a conversation without the
    // question each fail the leg.
    expect(alone("200 event-one")).toBe(1);
    expect(alone("404 event-never")).toBe(1);
    expect(
      run(`${rig("said_text")}\n${rig("answers_line_count")}\n${rig("a7_loop_posted_nothing")}\n${rig("a7_alone_read")}
daemon_log=$empty_log; boot_offset=0
a7_alone_read '200 event-never' never-said-by-the-architect 0`).code
    ).toBe(1);
  });

  // A10's Claude Code leg judges the signatures of the conversation as it stood before its first
  // question. The answers the loop gave since, and the resumed reviewer's turn, write signatures
  // into the live conversation that the first request could not hold.
  describe("a10_claude_read", () => {
    const assistant = (signature: string) =>
      JSON.stringify({
        type: "assistant",
        effort: "xhigh",
        message: { content: [{ type: "thinking", thinking: "t", signature }] },
      });
    const evidenceDir = join(dir, "a10-claude-evidence");
    const before = join(dir, "pre_change-before-A10-1.jsonl");
    const after = join(dir, "pre_change-after-A10.jsonl");
    const suffix = (n: number) => `(question pre-change-scan of run 4242, A10-${n})`;
    const reply = (event: string, marker: string) =>
      JSON.stringify({
        at: "2026-11-24T12:00:00.000Z",
        session: "asker-session",
        text: `In Reply To: ${event}\nMessage:\nThe SBOM is missing, SBOM-ZINC-512. <!-- legion-answer:${marker} -->\n\nTopic: notifications.agent.asker-session`,
      });

    beforeAll(() => {
      mkdirSync(evidenceDir, { recursive: true });
      writeFileSync(before, `${assistant("c2lnQQ==")}\n`);
      // The conversation as it stands after the answers: the loop's appended turn carries a
      // signature of its own.
      writeFileSync(after, `${assistant("c2lnQQ==")}\n${assistant("c2lnQg==")}\n`);
      writeFileSync(
        join(evidenceDir, "inbox.jsonl"),
        `${reply("e1", "a1a1a1a1")}\n${reply("e2", "b2b2b2b2")}\n`
      );
      writeFileSync(
        join(evidenceDir, "model-calls.jsonl"),
        `${JSON.stringify({
          label: "daemon-files",
          ordinal: 1,
          request_body: {
            output_config: { effort: "xhigh" },
            messages: [
              {
                role: "assistant",
                content: [{ type: "thinking", thinking: "t", signature: "c2lnQQ==" }],
              },
              { role: "user", content: `What blocked the release? ${suffix(1)}` },
            ],
          },
        })}\n${JSON.stringify({
          label: "claude-files",
          ordinal: 2,
          request_body: {
            messages: [{ role: "user", content: `${suffix(1)} and ${suffix(2)}` }],
          },
        })}\n`
      );
    });

    const claude = (transcript: string, line = '{"cacheReadTokens":4961}') =>
      run(`${rig("a10_claude_read")}
questions_file=${shq(liveQuestions)}
evidence=${shq(evidenceDir)}
verdict=failed
a10_claude_read e1 e2 A10-1 A10-2 ${shq(line)} 0 ${shq(transcript)} && verdict=held
printf '%s\\n%s\\n%s\\n' "$verdict" "$read_said" "$read_pass_said"`);

    test("holds over the transcript kept before the first question, and says what it judged", () => {
      const result = claude(before);
      const [verdict, said, passSaid] = result.out.trimEnd().split("\n");
      expect(verdict).toBe("held");
      expect(said).toContain("its thinking signatures 1, missing from that request 0");
      expect(passSaid).toBe(
        'Claude Code: effort "xhigh" and each of the transcript\'s 1 thinking signatures replayed'
      );
    });

    test("fails over the conversation as it stands after the answers, which hold a signature no first request could", () => {
      const [verdict, said] = claude(after).out.trimEnd().split("\n");
      expect(verdict).toBe("failed");
      expect(said).toContain("its thinking signatures 2, missing from that request 1");
    });

    test("fails with no cache read, no transcript and no thinking signature", () => {
      expect(claude(before, '{"cacheReadTokens":0}').out).toContain("failed");
      expect(claude(join(dir, "no-such-transcript.jsonl")).out).toContain("failed");
      const plain = join(dir, "pre_change-plain.jsonl");
      writeFileSync(
        plain,
        `${JSON.stringify({ type: "assistant", effort: "xhigh", message: { content: [] } })}\n`
      );
      expect(claude(plain).out).toContain("failed");
    });
  });

  // A20's read takes the replies the thread must hold, the row's two and, for the control, the ones
  // it found and the one its answer would add; the tester's reply under the control's own reply is
  // read from the replies a20_read holds.
  describe("a20_read", () => {
    const a20 = (args: string, body = "") =>
      run(`${rig("said_text")}
${rig("a20_skipped")}
${rig("a20_nothing_posted")}
${rig("a20_read")}
${rigVariable("a20_nobody")}
a20_answer=
daemon_log=$thread_replies_log; boot_offset=0
questions_file=${shq(liveQuestions)}
agent[files:tester.session]=session-tester
agent[files:tester.token]=legion-alx-alx-3-tester
claim_json() { printf '{"state":"suspended"}\\n'; }
pane_count() { printf '0\\n'; }
${body}
a20_read ${args} && echo held || echo failed
echo "$read_said"`);
    // The tester's two replies in comment-100's thread: A3's answer and the one under comment-102.
    test("holds with the two replies the row expects, and fails with another count", () => {
      expect(a20("comment-100 comment-102 comment-103 0 0 2").out).toMatch(/^held/);
      const three = a20("comment-100 comment-102 comment-103 0 0 3").out;
      expect(three).toMatch(/^failed/);
      expect(three).toContain("replies in the thread of comment-100: 2 (want 3)");
      expect(a20("comment-100 comment-102 comment-103 0 0 1").out).toMatch(/^failed/);
    });

    test("fails on the part that the tester posted nothing under the control once a reply stands there", () => {
      // comment-100 is a comment the tester has a reply under, as a placed reply would stand.
      expect(a20("comment-100 comment-102 comment-100 0 0 2").out).toMatch(/^failed/);
      const nothing = (control: string, replies = "") =>
        run(`${rig("said_text")}\n${rig("a20_nothing_posted")}
agent[files:tester.session]=session-tester
a20_nothing_posted comment-100 ${control} ${replies} && echo quiet || echo posted`).out.trim();
      expect(nothing("comment-103")).toBe("quiet");
      expect(nothing("comment-100")).toBe("posted");
      // The replies given are the ones read, and no thread is fetched for them.
      expect(nothing("comment-103", `'[{"id":"x","answers":"comment-103","body":"b"}]'`)).toBe(
        "posted"
      );
      expect(nothing("comment-103", "'[]'")).toBe("quiet");
      expect(nothing("''")).toBe("posted");
    });
  });

  // A21's read: the picker mention and the message to the failed role's topic, each answered by the
  // failed implementer, and nothing of the issue started across both legs.
  describe("a21_read", () => {
    const evidenceDir = join(dir, "a21-evidence");
    const logFile = join(dir, "a21.log");
    const inbox = (events: string[]) =>
      `${events.map((text) => JSON.stringify({ at: "2026-11-24T12:00:00.000Z", session: "asker-session", text: `${text}\n\nTopic: notifications.agent.asker-session` })).join("\n")}\n`;
    const answerText = (event: string, body: string) =>
      `In Reply To: ${event}\nMessage:\n${body} <!-- legion-answer:ab12ab12 -->`;
    const keyed = "The backoff starts at 250 ms, tracked as AMBER-KESTREL-63.";
    const claims = (implementer: string) =>
      JSON.stringify({
        claims: [
          {
            token: "legion-alx-alx-11-implementer",
            tree: "ALX-11",
            issue: "ALX-11",
            role: "implementer",
            state: implementer,
          },
          {
            token: "legion-alx-alx-11-architect",
            tree: "ALX-11",
            issue: "ALX-11",
            role: "architect",
            state: "suspended",
          },
        ],
      });

    beforeAll(() => {
      mkdirSync(evidenceDir, { recursive: true });
      writeFileSync(
        logFile,
        '{"time":"2026-11-24T12:00:01.000Z","level":"INFO","msg":"answers: answered","question":"ab12ab12","issue":"ALX-11","role":"implementer","session":"session-implementer","fault":"","calls":1}\n'
      );
    });

    const a21 = (
      inboxText: string[],
      options: { claimState?: string; panes?: number; replied?: boolean; roleEvent?: string } = {}
    ) => {
      writeFileSync(join(evidenceDir, "inbox.jsonl"), inbox(inboxText));
      const result = run(`${rig("said_text")}
${rig("a21_read")}
questions_file=${shq(liveQuestions)}
evidence=${shq(evidenceDir)}
daemon_log=${shq(logFile)}; boot_offset=0
agent[files:failed_implementer.session]=session-implementer
agent[files:failed_implementer.token]=legion-alx-alx-11-implementer
comment_reply_by() { case "$2" in comment-110) printf '%s\\n' ${shq(keyed)} ;; comment-111) ${options.replied ? `printf 'a reply\\n'` : "return 1"} ;; *) return 1 ;; esac; }
claims_cmd() { printf '%s\\n' ${shq(claims(options.claimState ?? "failed"))}; }
pane_count() { printf '${options.panes ?? 1}\\n'; }
a21_read ALX-11 comment-110 comment-111 1 ${options.roleEvent ?? "event-role"} && echo held || echo failed
echo "$read_said"`);
      return result.out;
    };

    test("holds when both legs were answered once by the failed implementer, its claim still failed and no pane started", () => {
      const out = a21([answerText("event-role", keyed)]);
      expect(out).toMatch(/^held/);
      expect(out).toContain(
        "the message to the role's topic event-role: 1 reply (want one), holding the keys: yes, answered as session-implementer: yes"
      );
    });

    test("fails when the role topic's message was answered twice, without its keys, by another session, or not at all", () => {
      const once = answerText("event-role", keyed);
      expect(a21([once, once])).toMatch(/^failed/);
      expect(a21([answerText("event-role", "It starts at 250 ms.")])).toMatch(/^failed/);
      expect(a21([])).toMatch(/^failed/);
      expect(a21([once], { roleEvent: "event-other" })).toMatch(/^failed/);
      // A reply whose answer the loop logged for another session.
      writeFileSync(
        logFile,
        '{"time":"2026-11-24T12:00:01.000Z","level":"INFO","msg":"answers: answered","question":"ab12ab12","session":"session-planner","fault":"","calls":1}\n'
      );
      expect(a21([once])).toMatch(/^failed/);
      writeFileSync(
        logFile,
        '{"time":"2026-11-24T12:00:01.000Z","level":"INFO","msg":"answers: answered","question":"ab12ab12","session":"session-implementer","fault":"","calls":1}\n'
      );
      expect(a21([once])).toMatch(/^held/);
    });

    test("fails when a pane started, the implementer's claim no longer stands failed, or the control was answered", () => {
      const once = answerText("event-role", keyed);
      expect(a21([once], { panes: 2 })).toMatch(/^failed/);
      expect(a21([once], { claimState: "suspended" })).toMatch(/^failed/);
      expect(a21([once], { replied: true })).toMatch(/^failed/);
    });
  });

  test("tree_is_live accepts an issue held from the phase when asked to, and A6's use of it as before", () => {
    const held = `'{"admission":{"trees":["ALX-1"]},"issues":{"ALX-1":{"phase":"held","heldFrom":"implementing"}}}'`;
    expect(run(`legion_state=${held}; tree_is_live ALX-1 implementing`).code).toBe(1);
    expect(run(`legion_state=${held}; tree_is_live ALX-1 implementing held`).code).toBe(0);
    expect(run(`legion_state=${held}; tree_is_live ALX-1 testing held`).code).toBe(1);
    expect(run(`legion_state=${held}; tree_is_live ALX-1 done held`).code).toBe(1);
    // A held issue that is no live root of the daemon's, and a live root still at the phase.
    const lingering = `'{"admission":{"trees":[]},"issues":{"ALX-1":{"phase":"held","heldFrom":"implementing"}}}'`;
    expect(run(`legion_state=${lingering}; tree_is_live ALX-1 implementing held`).code).toBe(1);
    const running = `'{"admission":{"trees":["ALX-1"]},"issues":{"ALX-1":{"phase":"implementing"}}}'`;
    expect(run(`legion_state=${running}; tree_is_live ALX-1 implementing held`).code).toBe(0);
    expect(run(`legion_state=${running}; tree_is_live ALX-1 implementing`).code).toBe(0);
  });

  test("reply_message prints the message of a reply, and nothing for none", () => {
    const reply = shq(
      "In Reply To: e1\nMessage:\nThe answer.\n\n<!-- legion-answer:ab12 -->\n\nTopic: notifications.agent.s"
    );
    expect(read(`reply_message ${reply}`).text).toBe("The answer.\n\n<!-- legion-answer:ab12 -->");
    expect(read("reply_message ''")).toEqual({ code: 0, text: "" });
    expect(read("inbox_message event-never")).toEqual({ code: 0, text: "" });
  });

  test("inbox_holds reads a marker in what the inbox relayed to one session", () => {
    expect(run("inbox_holds asker-session VIOLET-OTTER-88").code).toBe(0);
    // Another session's relay, a marker nobody relayed, no marker and no inbox hold nothing.
    expect(run("inbox_holds controller-session VIOLET-OTTER-88").code).toBe(1);
    expect(run("inbox_holds asker-session never-relayed").code).toBe(1);
    expect(run("inbox_holds asker-session ''").code).toBe(1);
    expect(run("evidence=$empty_evidence; inbox_holds asker-session VIOLET-OTTER-88").code).toBe(1);
    expect(run("evidence=$bare_evidence; inbox_holds asker-session VIOLET-OTTER-88").code).toBe(1);
  });

  test("answered_by reads the session an answer was made in the name of", () => {
    const line = "daemon_log=$tools_log; boot_offset=0; line=$(answered_line f1f1f1f1)";
    expect(run(`${line}; answered_by "$line" session-architect`).code).toBe(0);
    expect(run(`${line}; answered_by "$line" session-planner`).code).toBe(1);
    for (const none of ["''", "null", "'{}'", "'[]'"]) {
      expect(run(`answered_by ${none} session-architect`).code).toBe(1);
    }
  });

  test("comment_reply_in, issue_comments_in and pr_comments_holding read a list already fetched", () => {
    const comments = '"$(dispatch_get issues/ALX-3/comments)"';
    expect(read(`comment_reply_in ${comments} comment-77 session-tester`).text).toBe(
      read("comment_reply_by ALX-3 comment-77 session-tester").text
    );
    expect(read(`comment_reply_in ${comments} comment-77 session-architect`).code).not.toBe(0);
    expect(read("comment_reply_in '' comment-77 session-tester").code).not.toBe(0);
    expect(read("comment_reply_in null comment-77 session-tester").code).not.toBe(0);
    const listing = "listing=$(pr_comment_listing 43)";
    expect(
      read(`${listing}; pr_comments_holding "$listing" answer-loop-a17-4242-files | jq length`).text
    ).toBe("2");
    expect(read(`${listing}; pr_comments_holding "$listing" never-marked`).text).toBe("[]");
    expect(read("pr_comments_holding null marker")).toEqual({ code: 0, text: "[]" });
    expect(read("pr_comments_holding '' marker")).toEqual({ code: 0, text: "[]" });
  });

  test("a13_differs names the captures that differ from the base commit's, and a13_files lists the six", () => {
    expect(read(`echo \${#a13_files[@]}`).text).toBe("6");
    // The files a13_same rejects, in the order of a13_files: what a13_differs prints, each followed
    // by a space.
    const files = [
      "send.status",
      "send.body",
      "publish.status",
      "publish.body",
      "dispatch-session.json",
      "dispatch-role.json",
    ];
    const rejected = (setup: string, side = "") =>
      files.filter((file) => run(`${setup}; a13_same ${file} ${side}`).code !== 0);
    const differs = (setup: string, side = "") => {
      const printed = read(`${setup}; a13_differs ${side}`);
      expect(printed.code).toBe(0);
      return printed.text.split(" ").filter((file) => file !== "");
    };
    for (const setup of [
      "true",
      "evidence=$presence_evidence",
      "evidence=$empty_evidence",
      "evidence=$bare_evidence",
    ]) {
      expect(differs(setup)).toEqual(rejected(setup));
    }
    expect(differs("evidence=$control_evidence", "control")).toEqual(
      rejected("evidence=$control_evidence", "control")
    );
    for (const file of ["publish.body", "dispatch-session.json"]) {
      expect(differs("evidence=$presence_evidence")).toContain(file);
    }
    expect(differs("evidence=$control_evidence", "control")).toContain("send.body");
    // A capture that is missing differs: all six of a probe that wrote none.
    expect(differs("evidence=$bare_evidence")).toEqual(files);
  });
});
