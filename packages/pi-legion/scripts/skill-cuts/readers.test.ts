import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import { createGateway, type MessagesBody } from "./gateway";
import {
  ATTACK_PLANTED_LINE,
  AttackInput,
  MapInput,
  NAME_ONLY_REFUSAL,
  nameOnlyReason,
  PROMPTS_DIR,
  parsedAnswer,
  parsePrompt,
  plantControl,
  READER_NAMES,
  READER_TRIES,
  READERS,
  type Reader,
  ReaderFailed,
  type ReaderName,
  readerBody,
  runControl,
  runReader,
  SameJobInput,
  SentenceInput,
  skillFiles,
  TASKS_PER_LABEL,
} from "./readers";

/** The one user message of a reader's request body, its blocks' text. */
const UserMessage = z.tuple([
  z.object({
    role: z.literal("user"),
    content: z.array(z.object({ type: z.literal("text"), text: z.string() })),
  }),
]);

// The readers' schemas and their one call each, with a gateway whose fake fetch answers each body
// with a synthetic answer in the stream shape the gateway reads (testdata/F5): no test here
// reaches a network. Every skill, repository and message is made up.

const scratch = temporaryDirectory("skill-cuts-readers", { real: true });

const SKILL_MD = [
  "---",
  "name: alpha",
  "description: Use when a task asks for the alpha checklist of the made-up fixture project.",
  "---",
  "",
  "# Alpha",
  "",
  "1. Read `references/x.md` before the first step.",
  "2. Write each step's result on one line.",
  "3. Run the tests where needed.",
  "",
].join("\n");
const CUT_MD = SKILL_MD.replace("2. Write each step's result on one line.\n", "");

/** A streamed reply whose one text block is `text`, in the shape the gateway streams (F5). */
function sse(text: string): Response {
  const events = [
    {
      type: "message_start",
      message: {
        model: "claude-opus-5-5",
        id: "msg_test",
        type: "message",
        role: "assistant",
        content: [],
        stop_reason: null,
        usage: { input_tokens: 100, output_tokens: 1 },
      },
    },
    { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } },
    { type: "content_block_delta", index: 0, delta: { type: "text_delta", text } },
    { type: "content_block_stop", index: 0 },
    { type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 20 } },
    { type: "message_stop" },
  ];
  return new Response(
    events.map((event) => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join(""),
    {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    }
  );
}

/** A gateway whose fake fetch answers the n-th body (from 1) with the n-th of `answers` as JSON,
 *  the last again once they run out, and the bodies sent. */
function world(...answers: unknown[]) {
  const bodies: MessagesBody[] = [];
  const keyFile = path.join(scratch, `key-${Math.random()}`);
  writeFileSync(keyFile, fixtureSecret("gw"), { mode: 0o600 });
  const gateway = createGateway({
    baseUrl: "http://gateway.test",
    keyFile,
    concurrency: 1,
    timeoutMs: 5_000,
    fetch: async (_url, init) => {
      bodies.push(JSON.parse(String(init.body)) as MessagesBody);
      const answer = answers[Math.min(bodies.length, answers.length) - 1];
      return sse(typeof answer === "string" ? answer : JSON.stringify(answer));
    },
    sleep: async () => {},
  });
  return { gateway, bodies };
}

/** Why the schema of reader `name` refuses `answer` to `input`, as runReader would say it, or
 *  undefined when it parses. */
function refusal(name: ReaderName, input: unknown, answer: unknown): string | undefined {
  const reader: Reader<unknown, unknown> = READERS[name];
  try {
    parsedAnswer(reader.answer(reader.input.parse(input)), answer);
    return undefined;
  } catch (error) {
    return messageFor(error);
  }
}

/** One reader's input, a valid answer, and bad answers each with a piece of why it is refused. */
interface Row {
  readonly input: unknown;
  readonly valid: readonly unknown[];
  readonly bad: readonly { readonly answer: unknown; readonly why: string }[];
}

const CRITERION = {
  pass: "The step writes each alpha step's result on a line of its own.",
  fail: "The step puts two results on one line.",
};
const caseInput = {
  skill: "alpha",
  // The lines of SKILL.md the session's read held, each with its number in the file.
  files: [
    {
      path: "SKILL.md",
      lines: [
        { number: 6, text: "# Alpha" },
        { number: 8, text: "1. Read `references/x.md` before the first step." },
        { number: 9, text: "2. Write each step's result on one line." },
      ],
    },
  ],
  messages: [
    {
      index: 2,
      role: "user",
      text: '[user]\nWrite the alpha checklist, and put every result on "one line".',
    },
    {
      index: 3,
      role: "assistant",
      text: '[assistant]\n[tool call: read] {"path":"skill://alpha"}',
    },
    {
      index: 4,
      role: "toolResult",
      text: "[tool result: read]\n[the text of a file of the skill, left out]",
    },
    {
      index: 5,
      role: "assistant",
      text: '[assistant]\n[tool call: todo] {"plan":"Each result goes on\\n\\"one line\\"."}',
    },
    { index: 6, role: "assistant", text: "[assistant]\n1. ok\n2. ok" },
  ],
};
const caseStep = {
  step: { messageIndex: 6, what: "writes the results" },
  line: { file: "SKILL.md", text: "2. Write each step's result on one line." },
  criterion: CRITERION,
  violatingStep: { text: "1. ok, 2. ok", toolCalls: [] },
  restatements: [],
};
/** The same case, with each message before the step that states the line, its words quoted:
 *  white space, case and a JSON string's escapes aside. */
const restated = {
  ...caseStep,
  violatingStep: {
    text: "",
    toolCalls: [{ name: "write", arguments: { path: "checklist.md", content: "1. ok, 2. ok" } }],
  },
  restatements: [
    { messageIndex: 2, quote: 'Put every result on "one line".' },
    { messageIndex: 5, quote: 'each result goes on "one   line".' },
  ],
};

/** A window of the restatement check: message 5 comes in two parts. */
const restateInput = {
  line: "2. Write each step's result on one line.",
  messages: [
    {
      index: 2,
      role: "user",
      text: '[user]\nWrite the alpha checklist, and put every result on "one line".',
    },
    { index: 5, role: "toolResult", text: "[part 1 of 2 of this message]\nThe notes say:" },
    {
      index: 5,
      role: "toolResult",
      text: "[part 2 of 2 of this message]\nKeep each result to a single line.",
    },
  ],
};
const auditInput = {
  line: "2. Write each step's result on one line.",
  before: "[user]\nWrite the alpha checklist.",
  step: "[assistant]\n1. ok\n2. ok",
};

const mapInput = {
  skill: "alpha",
  base: [{ path: "SKILL.md", text: SKILL_MD }],
  cut: [{ path: "SKILL.md", text: CUT_MD }],
  audit: null,
};
const kept = {
  file: "SKILL.md",
  line: 8,
  rule: "read x first",
  status: "kept",
  cut: { file: "SKILL.md", line: 8 },
  reason: "same words",
  audit: null,
};
const lost = {
  file: "SKILL.md",
  line: 9,
  rule: "one line per result",
  status: "lost",
  cut: null,
  reason: "left out",
  audit: null,
};

const skillsInput = {
  skills: [
    { id: "s1", repo: "acme/skills", name: "alpha", description: "Use for the alpha checklist." },
    {
      id: "s2",
      repo: "acme/tools",
      name: "alpha-list",
      description: "Use to write the alpha checklist.",
    },
    { id: "s3", repo: "acme/tools", name: "beta", description: "Use to greet." },
  ],
};

const namesInput = {
  skills: [
    {
      id: "s1",
      repo: "acme/skills",
      name: "alpha",
      description: "Use for the alpha checklist.",
      descriptionFault: null,
    },
    {
      id: "s2",
      repo: "acme/tools",
      name: "beta",
      description: "Use to write the greeting card of the fixture project.",
      descriptionFault: "a need task loaded it on 0 of 3 runs",
    },
  ],
  namesTaken: ["alpha", "beta", "gamma"],
};
const notNominated = { id: "s1", nominated: false };
const nominated = {
  id: "s2",
  nominated: true,
  risk: "missed",
  task: "writing the greeting card of the fixture project",
  reason:
    "Beta names a letter, not greetings: an agent asked for a greeting card looks for a greeting skill.",
  proposed: ["greeting-card"],
};

const tasksInput = {
  skill: { name: "alpha", description: "Use when a task asks for the alpha checklist." },
  messages: [
    { id: "m1", text: "Write the alpha checklist for the fixture project." },
    { id: "m2", text: "Write the beta greeting for the fixture project." },
  ],
};
/** A need task's statement that the skill governs its first step. */
const governs = { firstStep: { governed: true, reason: "reading the checklist comes first" } };
const written = (label: string, n: number) => ({
  label,
  from: "written",
  text: `task ${label} ${n}`,
  why: "the description",
  ...(label === "need" ? governs : {}),
});
const fourEach = ["need", "near-miss"].flatMap((label) =>
  [1, 2, 3, 4].map((n) => written(label, n))
);
const tasksAnswer = {
  terms: ["alpha checklist"],
  tasks: [
    {
      label: "need",
      from: "session",
      messageId: "m1",
      why: "it asks for the checklist",
      ...governs,
    },
    {
      label: "near-miss",
      from: "session",
      messageId: "m2",
      why: "a fixture task without the checklist",
    },
    ...fourEach,
  ],
};
/** The first need task, saying nothing of its first step. */
const unstated = Object.fromEntries(
  Object.entries(tasksAnswer.tasks[0] ?? {}).filter(([key]) => key !== "firstStep")
);

const ROWS: Record<ReaderName, Row> = {
  restate: {
    input: restateInput,
    valid: [
      { restatements: [] },
      {
        restatements: [
          { messageIndex: 2, quote: 'put every result on "one line"' },
          { messageIndex: 5, quote: "Keep each result to a single line." },
        ],
      },
    ],
    bad: [
      {
        answer: { restatements: [{ messageIndex: 3, quote: "x" }] },
        why: "restatements.0.messageIndex: is no message index of the window",
      },
      {
        answer: { restatements: [{ messageIndex: 2, quote: "Keep each result" }] },
        why: "restatements.0.quote: is not words of message 2",
      },
      { answer: {}, why: "restatements" },
    ],
  },
  audit: {
    input: auditInput,
    valid: [
      { verdict: "follows", reason: "It writes each result on its own line." },
      { verdict: "breaks", reason: "It puts two results on one line." },
      { verdict: "not-governed", reason: "The step writes no result." },
    ],
    bad: [
      { answer: { verdict: "pass", reason: "x" }, why: "verdict" },
      { answer: { verdict: "follows" }, why: "reason" },
      { answer: { verdict: "follows", reason: "x", criterion: "y" }, why: "criterion" },
    ],
  },
  case: {
    input: caseInput,
    valid: [
      caseStep,
      restated,
      { step: null, reason: "No step after the read follows a line of the skill." },
    ],
    bad: [
      {
        answer: { ...caseStep, step: { messageIndex: 4, what: "x" } },
        why: "is a toolResult message, not an assistant message",
      },
      {
        answer: { ...caseStep, step: { messageIndex: 9, what: "x" } },
        why: "is no message index of the transcript",
      },
      {
        answer: { ...caseStep, line: { file: "README.md", text: "a line" } },
        why: "line.file: is no file of the skill",
      },
      {
        answer: { ...caseStep, line: { file: "SKILL.md", text: "two\nlines" } },
        why: "is not one non-blank line",
      },
      { answer: { ...caseStep, criterion: { pass: "x" } }, why: "criterion.fail" },
      { answer: { step: null }, why: "reason" },
      {
        answer: { ...caseStep, violatingStep: { text: " ", toolCalls: [] } },
        why: "violatingStep: holds no text and no tool call",
      },
      {
        answer: {
          ...caseStep,
          violatingStep: { text: "", toolCalls: [{ name: "", arguments: {} }] },
        },
        why: "violatingStep.toolCalls.0.name",
      },
      {
        answer: { ...caseStep, violatingStep: undefined },
        why: "violatingStep: Invalid input: expected object, received undefined",
      },
      {
        answer: { ...caseStep, restatements: undefined },
        why: "restatements: Invalid input: expected array, received undefined",
      },
      {
        answer: { ...caseStep, restatements: [{ messageIndex: 6, quote: "1. ok" }] },
        why: "restatements.0.messageIndex: is not before the step, message 6",
      },
      {
        answer: { ...caseStep, restatements: [{ messageIndex: 7, quote: "x" }] },
        why: "restatements.0.messageIndex: is no message index of the transcript",
      },
      {
        answer: {
          ...caseStep,
          restatements: [{ messageIndex: 2, quote: "Write each result on one line." }],
        },
        why: "restatements.0.quote: is not words of message 2",
      },
    ],
  },
  map: {
    input: mapInput,
    valid: [{ rules: [kept, lost] }],
    bad: [
      {
        answer: { rules: [{ ...lost, cut: { file: "SKILL.md", line: 9 } }] },
        why: "a lost rule has no place in the cut text",
      },
      {
        answer: { rules: [{ ...kept, cut: null }] },
        why: "a lost rule has no place in the cut text",
      },
      {
        answer: { rules: [{ ...kept, line: 12 }] },
        why: "is past the last line, 10, of SKILL.md in the base text",
      },
      {
        answer: { rules: [{ ...kept, cut: { file: "SKILL.md", line: 10 } }] },
        why: "is past the last line, 9, of SKILL.md in the cut text",
      },
      {
        answer: { rules: [{ ...kept, audit: "the audit's words" }] },
        why: "cites an audit, and none was given",
      },
      { answer: { rules: [{ ...kept, status: "moved" }] }, why: "status" },
      { answer: { rules: [] }, why: "rules" },
    ],
  },
  attack: {
    input: { skill: "alpha", files: [{ path: "SKILL.md", text: SKILL_MD }] },
    valid: [
      {
        findings: [
          {
            kind: "vague",
            file: "SKILL.md",
            line: 10,
            quote: "where needed",
            why: "which tests, and when?",
          },
        ],
      },
      { findings: [] },
    ],
    bad: [
      {
        answer: {
          findings: [{ kind: "unclear", file: "SKILL.md", line: 10, quote: "q", why: "w" }],
        },
        why: "kind",
      },
      {
        answer: { findings: [{ kind: "vague", file: "SKILL.md", line: 11, quote: "q", why: "w" }] },
        why: "is past the last line, 10",
      },
      {
        answer: { findings: [{ kind: "vague", file: "x.md", line: 1, quote: "q", why: "w" }] },
        why: "is no file of the skill",
      },
    ],
  },
  "same-job": {
    input: skillsInput,
    valid: [
      {
        pairs: [{ a: "s1", b: "s2", task: "the alpha checklist", why: "both say alpha checklist" }],
      },
      { pairs: [] },
    ],
    bad: [
      {
        answer: { pairs: [{ a: "s1", b: "s9", task: "t", why: "w" }] },
        why: "pairs.0.b: is no id of the skills",
      },
      {
        answer: { pairs: [{ a: "s1", b: "s1", task: "t", why: "w" }] },
        why: "pairs a skill with itself",
      },
    ],
  },
  sentence: {
    input: {
      skills: [
        {
          id: "s1",
          repo: "acme/skills",
          path: "skills/alpha/SKILL.md",
          text: "# Alpha\n\nWhere this text and the program differ, trust the program.\n",
        },
        { id: "s2", repo: "acme/skills", path: "skills/beta/SKILL.md", text: "# Beta\n" },
      ],
    },
    valid: [
      {
        found: [
          {
            id: "s1",
            line: 3,
            quote: "Where this text and the program differ, trust the program.",
          },
        ],
      },
      { found: [] },
    ],
    bad: [
      {
        answer: { found: [{ id: "s1", line: 4, quote: "q" }] },
        why: "is past the last line, 3, of skill s1",
      },
      { answer: { found: [{ id: "s7", line: 1, quote: "q" }] }, why: "is no id of the skills" },
    ],
  },
  names: {
    input: namesInput,
    valid: [{ skills: [notNominated, nominated] }],
    bad: [
      {
        answer: {
          skills: [
            notNominated,
            { ...nominated, reason: "The job is not clear from the name alone." },
          ],
        },
        why: `skills.1.reason: ${NAME_ONLY_REFUSAL}`,
      },
      {
        answer: {
          skills: [
            notNominated,
            { ...nominated, reason: "The name beta alone does not say what the skill does." },
          ],
        },
        why: `skills.1.reason: ${NAME_ONLY_REFUSAL}`,
      },
      {
        answer: { skills: [notNominated, { ...nominated, task: "unclear from the name" }] },
        why: `skills.1.task: ${NAME_ONLY_REFUSAL}`,
      },
      {
        answer: { skills: [notNominated, { ...nominated, proposed: ["gamma"] }] },
        why: "is a name in use",
      },
      {
        answer: { skills: [notNominated, { ...nominated, proposed: ["beta"] }] },
        why: "is the skill's own name",
      },
      {
        answer: { skills: [notNominated, { ...nominated, proposed: ["Greeting_Card"] }] },
        why: "is not lowercase words joined by -",
      },
      {
        answer: {
          skills: [notNominated, { ...nominated, proposed: ["a-1", "a-2", "a-3", "a-4"] }],
        },
        why: "proposed",
      },
      { answer: { skills: [notNominated, { ...nominated, proposed: [] }] }, why: "proposed" },
      {
        answer: { skills: [notNominated, { id: "s2", nominated: false }] },
        why: "is false for a skill whose description has a fault",
      },
      { answer: { skills: [nominated] }, why: "gives no entry for s1" },
      { answer: { skills: [notNominated, notNominated, nominated] }, why: "answers s1 twice" },
    ],
  },
  tasks: {
    input: tasksInput,
    valid: [
      tasksAnswer,
      { terms: ["alpha"], tasks: [...fourEach, written("need", 5), written("near-miss", 5)] },
    ],
    bad: [
      {
        answer: { ...tasksAnswer, tasks: tasksAnswer.tasks.slice(1) },
        why: `holds 4 need tasks whose first step the skill governs, fewer than ${TASKS_PER_LABEL}`,
      },
      {
        answer: { ...tasksAnswer, tasks: [unstated, ...tasksAnswer.tasks.slice(1)] },
        why: "tasks.0.firstStep: is missing: a need task says whether the skill governs its first step",
      },
      {
        // A need task whose first step the skill does not govern is not one of the five.
        answer: {
          ...tasksAnswer,
          tasks: [
            { ...unstated, firstStep: { governed: false, reason: "the file is read first" } },
            ...tasksAnswer.tasks.slice(1),
          ],
        },
        why: `holds 4 need tasks whose first step the skill governs, fewer than ${TASKS_PER_LABEL}`,
      },
      {
        answer: {
          ...tasksAnswer,
          tasks: [{ ...tasksAnswer.tasks[0], messageId: "m9" }, ...tasksAnswer.tasks.slice(1)],
        },
        why: "is no id of the messages",
      },
      {
        answer: {
          ...tasksAnswer,
          tasks: [
            ...tasksAnswer.tasks,
            { label: "need", from: "session", messageId: "m1", why: "again" },
          ],
        },
        why: "takes a message a task already took",
      },
      {
        answer: {
          ...tasksAnswer,
          tasks: [{ ...tasksAnswer.tasks[0], text: "changed" }, ...tasksAnswer.tasks.slice(1)],
        },
        why: "text",
      },
      { answer: { ...tasksAnswer, terms: [] }, why: "terms" },
    ],
  },
};

test("each reader's schema refuses a bad answer and a valid one parses", () => {
  expect(Object.keys(ROWS).sort()).toEqual([...READER_NAMES].sort());
  for (const name of READER_NAMES) {
    const row = ROWS[name];
    for (const answer of row.valid)
      expect({ name, refused: refusal(name, row.input, answer) }).toEqual({
        name,
        refused: undefined,
      });
    for (const { answer, why } of row.bad) {
      const refused = refusal(name, row.input, answer);
      expect({ name, refused: refused?.includes(why) ? why : refused }).toEqual({
        name,
        refused: why,
      });
    }
  }
});

test("the names reader refuses a reason that is only that the job is not clear from the name alone", () => {
  for (const reason of [
    "The job is not clear from the name alone.",
    "The name alone doesn't say what the skill does.",
    "Without reading the description, an agent cannot know what alpha is for.",
    "The name is too generic.",
  ]) {
    expect({ reason, nameOnly: nameOnlyReason(reason, "alpha") }).toEqual({
      reason,
      nameOnly: true,
    });
  }
  for (const reason of [
    "The name suggests running tests, and the skill sets up experiments.",
    "An agent asked to write a greeting card would look for a greeting skill, not alpha.",
  ]) {
    expect({ reason, nameOnly: nameOnlyReason(reason, "alpha") }).toEqual({
      reason,
      nameOnly: false,
    });
  }
});

test("a reader makes one call per batch, and asks again while the answer is refused", async () => {
  // A valid answer: one call, its answer, no refusal.
  const one = world(ROWS.map.valid[0]);
  const result = await runReader("map", mapInput, one.gateway);
  expect(one.bodies).toHaveLength(1);
  expect(result.answer).toEqual(ROWS.map.valid[0]);
  expect(result.refusals).toEqual([]);
  expect(result.spent).toHaveLength(1);
  // The request: the base and cut texts with their lines numbered, and the audit or None.
  const [message] = UserMessage.parse(one.bodies[0]?.messages);
  const text = message.content[0]?.text ?? "";
  expect(text).toContain('<base>\n<file path="SKILL.md">\n1: ---\n2: name: alpha');
  expect(text).toContain(
    "9: 2. Write each step's result on one line.\n10: 3. Run the tests where needed.\n</file>\n</base>"
  );
  expect(text).toContain("9: 3. Run the tests where needed.\n</file>\n</cut>");
  expect(one.bodies[0]).toEqual(readerBody("map", mapInput));
  expect(text.endsWith("<audit>\nNone.\n</audit>")).toBe(true);

  // A name-only reason, then text that is no JSON, then a valid fenced answer: three calls of the
  // same body, two refusals kept, the third answer taken.
  const nameOnly = {
    skills: [notNominated, { ...nominated, reason: "The job is not clear from the name alone." }],
  };
  const retried = world(
    nameOnly,
    "I nominate beta.",
    `\`\`\`json\n${JSON.stringify(ROWS.names.valid[0])}\n\`\`\``
  );
  const names = await runReader("names", namesInput, retried.gateway);
  expect(retried.bodies).toHaveLength(3);
  expect(retried.bodies[1]).toEqual(retried.bodies[0]);
  expect(names.answer).toEqual(ROWS.names.valid[0]);
  expect(names.refusals).toHaveLength(2);
  expect(names.refusals[0]).toContain(NAME_ONLY_REFUSAL);
  expect(names.refusals[1]).toContain("the answer is not one JSON value");

  // Refused on every try: ReaderFailed with each refusal, after READER_TRIES calls.
  const never = world(nameOnly);
  const failed = await runReader("names", namesInput, never.gateway).catch(
    (error: unknown) => error
  );
  expect(never.bodies).toHaveLength(READER_TRIES);
  if (!(failed instanceof ReaderFailed))
    throw new Error(`expected ReaderFailed, got ${messageFor(failed)}`);
  expect(failed.refusals).toHaveLength(READER_TRIES);
  expect(failed.refusals.every((each) => each.includes(NAME_ONLY_REFUSAL))).toBe(true);

  // An input the reader's schema refuses makes no call.
  const none = world(ROWS.map.valid[0]);
  await expect(runReader("map", { ...mapInput, base: [] }, none.gateway)).rejects.toThrow(
    "the map reader's input is refused: base"
  );
  await expect(
    runReader("same-job", { skills: [skillsInput.skills[0], skillsInput.skills[0]] }, none.gateway)
  ).rejects.toThrow("gives the id s1 twice");
  expect(none.bodies).toHaveLength(0);
});

test("the case reader's request shows only the offered lines, each with its number in the file, and says a line not shown cannot be a case's line; its input refuses a file with no line and a blank line", () => {
  const [user] = UserMessage.parse(readerBody("case", caseInput).messages);
  const text = user.content.map((part) => part.text).join("\n\n");
  expect(text).toStartWith('<skill name="alpha">\nOnly the lines a case may name are shown');
  expect(text).toContain("A line not shown cannot be a case's line");
  expect(text).toContain(
    '<file path="SKILL.md">\n6: # Alpha\n8: 1. Read `references/x.md` before the first step.\n9: 2. Write each step\'s result on one line.\n</file>\n</skill>'
  );
  expect(text).not.toContain("3. Run the tests where needed.");
  const file = caseInput.files[0];
  expect(READERS.case.input.safeParse({ ...caseInput, files: [] }).success).toBe(false);
  expect(
    READERS.case.input.safeParse({ ...caseInput, files: [{ ...file, lines: [] }] }).success
  ).toBe(false);
  expect(
    READERS.case.input.safeParse({
      ...caseInput,
      files: [{ ...file, lines: [{ number: 7, text: " " }] }],
    }).success
  ).toBe(false);
});

test("in control mode the tasks reader takes every message, once, as a need task saying whether the skill governs its first step, and its request says so", () => {
  const input = { ...tasksInput, mode: "control" };
  const take = (id: string, governed: boolean) => ({
    label: "need",
    from: "session",
    messageId: id,
    why: "it asks for the checklist",
    firstStep: { governed, reason: governed ? "the checklist comes first" : "a file comes first" },
  });
  expect(refusal("tasks", input, { terms: [], tasks: [take("m1", true), take("m2", false)] })).toBe(
    undefined
  );
  expect(refusal("tasks", input, { terms: [], tasks: [take("m1", true)] })).toContain(
    "leaves out the message m2"
  );
  expect(
    refusal("tasks", input, {
      terms: [],
      tasks: [take("m1", true), take("m2", true), written("near-miss", 1)],
    })
  ).toContain("is not a need task of a message");
  const bare = Object.fromEntries(
    Object.entries(take("m2", true)).filter(([key]) => key !== "firstStep")
  );
  expect(refusal("tasks", input, { terms: [], tasks: [take("m1", true), bare] })).toContain(
    "a need task says whether the skill governs its first step"
  );
  const [user] = UserMessage.parse(readerBody("tasks", input).messages);
  expect(user.content[0]?.text).toStartWith("<control>\nEach message is the first request");
  const [picking] = UserMessage.parse(readerBody("tasks", tasksInput).messages);
  expect(picking.content[0]?.text).toStartWith('<skill name="alpha">');
});

test("the audit's request holds the line, the messages before the step and the step, and its input refuses a criterion; the restatement check's holds the line and its window", () => {
  const [audit] = UserMessage.parse(readerBody("audit", auditInput).messages);
  expect(audit.content.map((part) => part.text)).toEqual([
    `<line>\n${auditInput.line}\n</line>\n\n<before>\n${auditInput.before}\n</before>\n\n<step>\n${auditInput.step}\n</step>`,
  ]);
  expect(READERS.audit.input.safeParse({ ...auditInput, criterion: CRITERION }).success).toBe(
    false
  );
  const [restate] = UserMessage.parse(readerBody("restate", restateInput).messages);
  const text = restate.content[0]?.text ?? "";
  expect(text.startsWith(`<line>\n${restateInput.line}\n</line>\n\n<messages>\n`)).toBe(true);
  expect(text).toContain(
    '<message index="5" role="toolResult">\n[part 2 of 2 of this message]\nKeep each result to a single line.\n</message>'
  );
});

test("each prompt file pins its settings, and they reach every request made with it", async () => {
  for (const name of ["judge", ...READER_NAMES]) {
    const settings = parsePrompt(
      name,
      readFileSync(path.join(PROMPTS_DIR, `${name}.md`), "utf8")
    ).settings;
    expect({
      name,
      model: settings.model,
      temperature: settings.temperature,
      effort: settings.effort,
    }).toEqual({
      name,
      model: "claude-opus-5-5",
      temperature: 1,
      effort: "high",
    });
  }
  for (const name of READER_NAMES) {
    const settings = parsePrompt(
      name,
      readFileSync(path.join(PROMPTS_DIR, `${name}.md`), "utf8")
    ).settings;
    const row = ROWS[name];
    // A refused answer, then a valid one: both requests carry the settings.
    const { gateway, bodies } = world({ refused: true }, row.valid[0]);
    await runReader(name, row.input, gateway);
    expect(bodies).toHaveLength(2);
    for (const body of bodies) {
      expect({
        name,
        model: body.model,
        max_tokens: body.max_tokens,
        temperature: body.temperature,
        thinking: body.thinking,
        output_config: body.output_config,
        stream: body.stream,
        system: body.system,
      }).toEqual({
        name,
        model: settings.model,
        max_tokens: settings.max_tokens,
        temperature: settings.temperature,
        thinking: { type: "enabled", budget_tokens: settings.thinking_budget },
        output_config: { effort: settings.effort },
        stream: true,
        system: [
          {
            type: "text",
            text: readFileSync(path.join(PROMPTS_DIR, `${name}.md`), "utf8")
              .replace(/^---\n[\s\S]*?\n---\n/, "")
              .trim(),
          },
        ],
      });
    }
  }
});

test("a prompt header that does not pin every setting, and only those, is refused", () => {
  const good =
    "---\nmodel: claude-opus-5-5\nthinking_budget: 4000\nmax_tokens: 8000\ntemperature: 1\neffort: high\n---\nJudge.\n";
  expect(parsePrompt("p", good)).toEqual({
    name: "p",
    settings: {
      model: "claude-opus-5-5",
      thinking_budget: 4000,
      max_tokens: 8000,
      temperature: 1,
      effort: "high",
    },
    text: "Judge.",
  });
  const refusals: [string, string][] = [
    ["Judge.\n", "prompts/p.md has no header"],
    [good.replace("effort: high\n", ""), "effort"],
    [good.replace("effort: high\n", "effort: high\ntop_k: 5\n"), "top_k"],
    [good.replace("temperature: 1", "temperature: 0.5"), "temperature"],
    [
      good.replace("thinking_budget: 4000", "thinking_budget: 8000"),
      "thinking_budget: is not below max_tokens",
    ],
    [good.replace("thinking_budget: 4000", "thinking_budget: 512"), "thinking_budget"],
    [
      good.replace("max_tokens: 8000", "max_tokens: 8000\nmax_tokens: 9000"),
      "the header gives max_tokens twice",
    ],
    [
      good.replace("model: claude-opus-5-5", "model claude-opus-5-5"),
      'a header line is not "<key>: <value>"',
    ],
    [good.replace("Judge.\n", "\n"), "holds no prompt text"],
  ];
  for (const [file, why] of refusals) expect(() => parsePrompt("p", file)).toThrow(why);
});

test("each control plants its finding and holds only when the answer names it", async () => {
  // map: a rule the cut kept word for word (line 8 of both texts) deleted from the cut text.
  const keptLine = "1. Read `references/x.md` before the first step.";
  const map = plantControl({ reader: "map", input: mapInput, deleteLine: keptLine });
  expect(MapInput.parse(map.input).cut).toEqual([
    { path: "SKILL.md", text: CUT_MD.replace(`${keptLine}\n`, "") },
  ]);
  expect(
    map.held({ rules: [{ ...kept, status: "lost", cut: null, reason: "deleted" }] }).held
  ).toBe(true);
  expect(map.held({ rules: [kept] })).toEqual({
    held: false,
    why: "the rule at SKILL.md:8 of the base text is kept: same words",
  });
  expect(map.held({ rules: [lost] })).toEqual({
    held: false,
    why: "the map gives no rule at SKILL.md:8",
  });
  expect(() =>
    plantControl({
      reader: "map",
      input: mapInput,
      deleteLine: "2. Write each step's result on one line.",
    })
  ).toThrow("the map control's line is no line of the cut text");
  const ran = await runControl(
    { reader: "map", input: mapInput, deleteLine: keptLine },
    world({ rules: [{ ...kept, status: "lost", cut: null, reason: "deleted" }] }).gateway
  );
  expect([ran.held, ran.result.refusals]).toEqual([true, []]);

  // attack: the planted line added at the end of SKILL.md, as a paragraph of its own (line 11).
  const attack = plantControl({
    reader: "attack",
    input: { skill: "alpha", files: [{ path: "SKILL.md", text: CUT_MD }] },
    plantLine: ATTACK_PLANTED_LINE,
  });
  expect(AttackInput.parse(attack.input).files[0]?.text).toBe(
    `${CUT_MD}\n${ATTACK_PLANTED_LINE}\n`
  );
  const finding = (line: number) => ({
    findings: [{ kind: "vague", file: "SKILL.md", line, quote: "where needed", why: "which?" }],
  });
  expect(attack.held(finding(11)).held).toBe(true);
  expect(attack.held(finding(9))).toEqual({
    held: false,
    why: "no finding at SKILL.md:11, the added line",
  });

  // same-job: a rewording of s1's description under a made-up name, paired with s1 only.
  const planted = {
    id: "s4",
    repo: "acme/more",
    name: "checklist-writer",
    description: "Use to produce the list of alpha steps.",
  };
  const sameJob = plantControl({ reader: "same-job", input: skillsInput, planted, copyOf: "s1" });
  expect(SameJobInput.parse(sameJob.input).skills).toEqual([...skillsInput.skills, planted]);
  const pair = (a: string, b: string) => ({
    pairs: [{ a, b, task: "the alpha checklist", why: "w" }],
  });
  expect(sameJob.held(pair("s1", "s4")).held).toBe(true);
  expect(sameJob.held(pair("s1", "s2"))).toEqual({ held: false, why: "no pair of s4 and s1" });
  expect(() =>
    plantControl({
      reader: "same-job",
      input: skillsInput,
      planted: { ...planted, id: "s1" },
      copyOf: "s2",
    })
  ).toThrow("is an id of the skills already");

  // sentence: a rewording added at the end of s2 (line 3).
  const reworded = "Should the program and these steps differ, the program is right.";
  const sentence = plantControl({
    reader: "sentence",
    input: SentenceInput.parse(ROWS.sentence.input),
    into: "s2",
    sentence: reworded,
  });
  expect(SentenceInput.parse(sentence.input).skills[1]?.text).toBe(`# Beta\n\n${reworded}\n`);
  expect(sentence.held({ found: [{ id: "s2", line: 3, quote: reworded }] }).held).toBe(true);
  expect(sentence.held({ found: [{ id: "s1", line: 3, quote: "q" }] }).held).toBe(false);
});

test("a skill's files: its .md files, SKILL.md first and the rest by path", () => {
  const dir = path.join(scratch, "skill-files");
  for (const [file, text] of [
    ["references/b.md", "b\n"],
    ["SKILL.md", "s\n"],
    ["LICENSE", "l\n"],
    ["references/a.md", "a\n"],
  ] as const) {
    mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
    writeFileSync(path.join(dir, file), text);
  }
  expect(skillFiles(dir)).toEqual([
    { path: "SKILL.md", text: "s\n" },
    { path: "references/a.md", text: "a\n" },
    { path: "references/b.md", text: "b\n" },
  ]);
});
