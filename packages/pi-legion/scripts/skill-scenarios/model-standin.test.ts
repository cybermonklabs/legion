import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// Every row runs model-standin.ts as rig.sh does, a process of its own, and posts Messages requests
// to it as Oh My Pi's Anthropic provider does.
const work = temporaryDirectory("model-standin");
let runs = 0;

/** The fixed message of the stand-in, as model-standin.ts exports it. */
const FIXED =
  "This is the model stand-in of the skill-scenario rig: it answers every turn with this one message and calls no tool.";

/** One block of a streamed reply: a text, or a tool_use with its id, its tool and its input. */
type Answered =
  | { type: "text"; text: string }
  | { type: "tool_use"; id: string; name: string; input: Record<string, unknown> };

/** The blocks of one Anthropic Messages event stream, each put together from its deltas. */
function blocksOf(stream: string): Answered[] {
  const blocks: { type: string; id?: string; name?: string; text: string; json: string }[] = [];
  for (const data of stream.split("\n").filter((line) => line.startsWith("data: "))) {
    const event = JSON.parse(data.slice("data: ".length));
    if (event.type === "content_block_start")
      blocks[event.index] = { ...event.content_block, text: "", json: "" };
    if (event.type === "content_block_delta") {
      const block = blocks[event.index];
      if (block === undefined) continue;
      if (event.delta.type === "text_delta") block.text += event.delta.text;
      if (event.delta.type === "input_json_delta") block.json += event.delta.partial_json;
    }
  }
  return blocks.map((block) =>
    block.type === "tool_use"
      ? {
          type: "tool_use",
          id: block.id ?? "",
          name: block.name ?? "",
          input: JSON.parse(block.json),
        }
      : { type: "text", text: block.text }
  );
}

/** A stand-in serving MODE (SKILL_SCENARIOS_MODEL) with SCRIPT beside its log, and a way to post one
 * Messages turn to it. */
async function standin(mode: string, script?: object) {
  const dir = path.join(work, `run-${++runs}`);
  mkdirSync(dir, { recursive: true });
  const log = path.join(dir, "model-standin.jsonl");
  if (script !== undefined)
    writeFileSync(path.join(dir, "standin-script.json"), JSON.stringify(script));
  const server = Bun.spawn(
    [process.execPath, path.join(import.meta.dir, "model-standin.ts"), log],
    {
      env: { PATH: process.env.PATH ?? "", SKILL_SCENARIOS_MODEL: mode },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  const reader = server.stdout.getReader();
  let printed = "";
  while (!printed.includes("\n")) {
    const { value, done } = await reader.read();
    if (done) break;
    printed += new TextDecoder().decode(value);
  }
  const base = /^listening on (\S+)/.exec(printed)?.[1] ?? "";
  const turn = async (messages: object[]) => {
    const response = await fetch(`${base}/anthropic/v1/messages`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ model: "standin-model", stream: true, messages }),
    });
    return { status: response.status, blocks: blocksOf(await response.text()) };
  };
  const lines = () =>
    readFileSync(log, "utf8")
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line));
  return { server, base, turn, lines };
}

const report = {
  op: "report_defect",
  issue: "ACME-1",
  defect: "path check: src/config.ts loadConfig returns DEFAULTS on a parse error",
  head: "a".repeat(40),
};
const completion = {
  op: "handoff_complete",
  summary: "path check",
  plan_rating: 3,
  plan_reason: "path check",
};
const script = {
  calls: [
    { tool: "legion", input: report },
    { tool: "legion", input: completion },
  ],
};
const task = { role: "user", content: [{ type: "text", text: "Your retro of ACME-1." }] };
/** The user message that carries one tool's result back. */
const resultOf = (id: string, text: string) => ({
  role: "user",
  content: [{ type: "tool_result", tool_use_id: id, content: text }],
});
/** The assistant message that made a tool call. */
const callOf = (block: Answered) => ({ role: "assistant", content: [block] });

// Catches: a scripted model that makes its calls in another order, makes one twice, answers its
// fixed message before both are made, or logs a tool result that is not the one the host sent back.
test("standin-report answers the report, then the completion, then its fixed message, and its log holds the two tool results", async () => {
  const { server, base, turn, lines } = await standin("standin-report", script);
  try {
    expect(base).toStartWith("http://");
    const first = await turn([task]);
    expect(first.status).toBe(200);
    const [reported] = first.blocks;
    expect(reported).toMatchObject({ type: "tool_use", name: "legion", input: report });
    if (reported?.type !== "tool_use") throw new Error("the first turn made no call");
    const answer = '{"defect":"d-7","state":"recorded","nextChange":1}';
    const second = await turn([task, callOf(reported), resultOf(reported.id, answer)]);
    const [completed] = second.blocks;
    expect(completed).toMatchObject({ type: "tool_use", name: "legion", input: completion });
    if (completed?.type !== "tool_use") throw new Error("the second turn made no call");
    const third = await turn([
      task,
      callOf(reported),
      resultOf(reported.id, answer),
      callOf(completed),
      resultOf(completed.id, "completed"),
    ]);
    expect(third.blocks).toEqual([{ type: "text", text: FIXED }]);
    expect(
      lines().map((line) => ({ answered: line.answered, toolResult: line.toolResult }))
    ).toEqual([
      { answered: 1, toolResult: null },
      { answered: 2, toolResult: answer },
      { answered: 3, toolResult: "completed" },
    ]);
  } finally {
    server.kill();
    await server.exited;
  }
  // Control: the plain stand-in answers the first turn with its fixed message, and calls no tool.
  const plain = await standin("standin", script);
  try {
    expect((await plain.turn([task])).blocks).toEqual([{ type: "text", text: FIXED }]);
  } finally {
    plain.server.kill();
    await plain.server.exited;
  }
});
