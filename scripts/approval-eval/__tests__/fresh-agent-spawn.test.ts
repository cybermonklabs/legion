import { afterEach, describe, expect, test } from "bun:test";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { type AgentRequest, askAgent } from "../fresh-agent";

/**
 * `askAgent` against a stand-in for the agent: a shell script that keeps the message it was sent
 * and the arguments it was started with, and prints one answer in the agent's own stream. Unlike
 * `fresh-agent.test.ts` this starts a process; it reaches no model.
 */

/** The directories this file made, each removed by its own exact path. */
const made: string[] = [];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** One scratch directory under the scratch volume, never under the workspace. */
async function scratch(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "fresh-agent-spawn-"));
  made.push(directory);
  return directory;
}

/** The answer the stand-in prints, as the agent's JSON stream carries one. */
const ANSWER = JSON.stringify({
  type: "turn_end",
  message: {
    role: "assistant",
    model: "claude-opus-5-5",
    content: [{ type: "text", text: "Approve" }],
  },
});

/** A stand-in agent in `directory` that runs `lines` as its shell script. */
async function standIn(directory: string, lines: readonly string[]): Promise<string> {
  const path = join(directory, "omp");
  await writeFile(path, ["#!/bin/sh", ...lines].join("\n").concat("\n"), "utf8");
  await chmod(path, 0o755);
  return path;
}

/** An agent that keeps its standard input and its arguments in its directory, then answers. */
const ANSWERS = ["cat > message", 'printf "%s\\n" "$@" > arguments', `printf '%s\\n' '${ANSWER}'`];

/** An agent that exits before it reads its standard input, as one refused at start does. */
const QUITS = ["echo 'no credential for the model' >&2", "exit 3"];

/** One request of the run, started in `directory`. */
function request(directory: string, prompt: string): AgentRequest {
  return { arm: "with-skill", caseId: "case-0", home: directory, directory, prompt };
}

describe("the message the agent is sent", () => {
  test("a message longer than any one argument reaches the agent whole, on its standard input", async () => {
    const directory = await scratch();
    // Over 2 MiB, past the limit of the whole argument list as well as of one argument, with
    // characters of more than one byte and the line feeds a rendered case ends in.
    const prompt = `${"Évidence du cas, ligne après ligne.\n".repeat(60_000)}\n\n`;
    expect(Buffer.byteLength(prompt)).toBeGreaterThan(2 * 1024 * 1024);

    const answer = await askAgent(await standIn(directory, ANSWERS), request(directory, prompt));

    expect(answer.failure).toBeNull();
    expect(answer.text).toBe("Approve");
    expect(answer.model).toBe("claude-opus-5-5");
    expect(await readFile(join(directory, "message"), "utf8")).toBe(prompt);
    // `-p` is the last argument, with no message after it, which is what has the agent read its
    // standard input as the message.
    const started = (await readFile(join(directory, "arguments"), "utf8")).split("\n");
    expect(started.at(-2)).toBe("-p");
  });

  test("an agent that cannot be started is that case's failure, and the run is not ended", async () => {
    const directory = await scratch();

    const answer = await askAgent(join(directory, "no-such-agent"), request(directory, "Judge."));

    expect(answer.text).toBe("");
    expect(answer.model).toBeNull();
    expect(answer.failure).toStartWith("the agent could not be started: ");
  });

  test("an agent that exits before reading a long message is that case's failure", async () => {
    const directory = await scratch();
    const prompt = "Évidence.\n".repeat(300_000);

    const answer = await askAgent(await standIn(directory, QUITS), request(directory, prompt));

    expect(answer.text).toBe("");
    expect(answer.failure).toBe("the agent exited 3 with no answer: no credential for the model");
  });
});
