// The one reader of a claim boot refusal (boot-refusal.ts), which rig.sh's boot_agent asks inside
// every run's boot through `health.ts --boot-refusal`: a module of its own, so what the world
// loads to answer it holds neither health.ts nor score.ts, and health.ts's answer is the
// module's. What each refusal reads as is rig.test.ts's (boot_agent's relaunch rows, and "a claim
// boot sentence is read in the wording of the run's own label's plugin").
import { describe, expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, realpathSync, writeFileSync } from "node:fs";
import path from "node:path";
import { CLAIM_BOOT_FAILED_OPENING } from "@legion/pi-shared/legion-claim-core";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { importGraph } from "./run-programs";
import { spawnChecked } from "./test-programs";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default, so each gets a minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

/** This checkout, whose plugin's claim core the runs below name in their built-from. */
const CHECKOUT = path.resolve(import.meta.dir, "../../../..");

// Catches: the boot-refusal reader importing health.ts or score.ts, or a module that does, which
// puts every rule of a whole run's reading on the path of every run's boot; and the module's own
// graph growing past the leaf modules a boot's record needs unremarked.
test("boot-refusal.ts loads neither health.ts nor score.ts, only the leaf modules a boot's record needs", () => {
  const here = realpathSync(import.meta.dir);
  const graph = importGraph(path.join(here, "boot-refusal.ts"));
  for (const program of ["health.ts", "score.ts"])
    expect(graph.has(path.join(here, program))).toBe(false);
  expect(
    [...graph]
      .filter((file) => path.dirname(file) === here)
      .map((file) => path.basename(file))
      .sort()
  ).toEqual([
    "boot-refusal.ts",
    "completion-answers.ts",
    "daemon-pane.ts",
    "gh-selectors.ts",
    "run-names.ts",
    "run-records.ts",
    "step-records.ts",
    "transcript.ts",
  ]);
});

// Catches: `health.ts --boot-refusal`, the question rig.sh's boot_agent asked before the names left
// the world (it asks boot-refusal.ts itself now), answering otherwise than the module that holds
// the reader, on any of the answers boot_agent reads: a relaunch (exit 0 and the refusal), none
// (exit 1), and a record it cannot read (exit 2).
describe("health.ts --boot-refusal answers as boot-refusal.ts does", () => {
  const runs = temporaryDirectories("boot-refusal");
  const SINCE = "2026-11-16T00:00:00.000Z";
  const OPENED = '{"at":"2026-11-16T00:00:00.000Z","opened":"daemon.log"}';
  const register = (status: number, code?: string) =>
    JSON.stringify({
      at: "2026-11-16T00:00:01.000Z",
      method: "POST",
      path: "/legion/v1/claims/register",
      status,
      ...(code === undefined ? {} : { code }),
      role: "reviewer",
    });
  const OPENING = [
    { type: "session", id: "s1" },
    { type: "message", message: { role: "user", content: [{ type: "text", text: "Review it." }] } },
  ];
  const TURN = {
    type: "message",
    message: { role: "assistant", content: [{ type: "text", text: "Reading the pull request." }] },
  };
  const SENTENCE = `${CLAIM_BOOT_FAILED_OPENING}claimRole after claims/register registered legion-skills1-lweval-1-reviewer with the daemon at http://127.0.0.1:4321; exiting so the daemon launches it again: ECONNREFUSED`;
  /** A run whose reviewer's launch left `daemon` (the stand-in's lines after its opening one),
   * `out` and the transcript `entries`, built from this checkout; and the arguments boot_agent
   * asks about it with. */
  const booted = (daemon: string[], out: string, entries: object[]) => {
    const dir = runs("run");
    const sessions = path.join(dir, "sessions");
    mkdirSync(sessions);
    writeFileSync(path.join(dir, "built-from"), `checkout ${CHECKOUT}\n`);
    writeFileSync(path.join(dir, "daemon.log"), [OPENED, ...daemon, ""].join("\n"));
    writeFileSync(path.join(dir, "out.txt"), out);
    writeFileSync(
      path.join(sessions, "s1.jsonl"),
      entries.map((entry) => `${JSON.stringify(entry)}\n`).join("")
    );
    return [dir, "reviewer", SINCE, sessions];
  };
  /** Each program's answer to `args`: its exit, its stdout, and its stderr less the program's own
   * name, which opens a line it prints for a record it cannot read. */
  const answers = (args: string[]) =>
    (
      [
        ["health", [path.join(import.meta.dir, "health.ts"), "--boot-refusal"]],
        ["boot-refusal", [path.join(import.meta.dir, "boot-refusal.ts")]],
      ] as const
    ).map(([program, command]) => {
      const asked = spawnChecked(["bun", ...command, ...args], { stdout: "pipe", stderr: "pipe" });
      return {
        code: asked.exitCode,
        stdout: asked.stdout.toString(),
        stderr: asked.stderr.toString().replace(new RegExp(`^${program}: `, "gm"), ""),
      };
    });

  for (const [name, run, code, stdout] of [
    [
      "a boot the stand-in refused",
      () =>
        booted(
          [register(401, "INVALID_BOOT_TOKEN")],
          "[legion] claims/register registration failed (401): Invalid boot token\nexit=1\n",
          OPENING
        ),
      0,
      "the daemon stand-in answered the reviewer's POST /legion/v1/claims/register 401 INVALID_BOOT_TOKEN\n",
    ],
    [
      "a boot the plugin refused after its registration",
      () => booted([register(200)], `[legion] ${SENTENCE}\nexit=1\n`, OPENING),
      0,
      `${SENTENCE}\n`,
    ],
    [
      "an accepted boot",
      () => booted([register(200)], "reviewed\nexit=0\n", [...OPENING, TURN]),
      1,
      "",
    ],
    [
      "a refusal after a turn",
      () => booted([register(200)], `[legion] ${SENTENCE}\nexit=1\n`, [...OPENING, TURN]),
      1,
      "",
    ],
    [
      "a stand-in log that does not parse",
      () => booted(["not a record"], "exit=1\n", OPENING),
      2,
      "",
    ],
  ] as const)
    test(`${name}: exit ${code}`, () => {
      const [health, module] = answers(run());
      expect(health).toEqual(module);
      expect({ code: health?.code, stdout: health?.stdout }).toEqual({ code, stdout });
    }, 30_000);
});
