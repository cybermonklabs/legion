/**
 * Runs a fresh agent with skills/spec-statement-review on each fixed change request of
 * expected.json, against a Dispatch server of this checkout, and judges each answer.
 *
 *   ANTHROPIC_API_KEY=<key> [ANTHROPIC_BASE_URL=<gateway>] DISPATCH_TEST_DATABASE_URL=<postgres> \
 *     bun scripts/spec-review-eval/run.ts [--runs N] [--omp <omp executable>] [--model <model>]
 *     [--fixture <id>]... [--out <dir>]
 *
 * Each agent runs in a bubblewrap sandbox (agent.ts), so `bwrap` must be installed. The exit
 * status is 0 when every run passes, 1 when one fails, and 2 when the command cannot run: a flag
 * it refuses, ANTHROPIC_API_KEY not set, an --out that is no directory or is not empty, a stack
 * that does not start, a sandbox that cannot hide the answers, a model that omp in the sandbox
 * does not list, or any other error before every run has its verdict.
 */
import { mkdir, mkdtemp, readdir, realpath, rm, writeFile } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";
import { textsHeading } from "../../packages/envoy-client/src/dispatch-commands/spec-form-texts";
import {
  type DispatchStack,
  startDispatchStack,
} from "../../packages/envoy-client/test/dispatch-stack";
import { CannotRunError } from "../approval-eval/corpus-io";
import { MODEL } from "../approval-eval/fresh-agent";
import {
  compileDispatch,
  environmentOf,
  MODEL_KEY,
  promptOf,
  proveSandbox,
  review,
  type Sandbox,
  writeAgent,
} from "./agent";
import { compareFindings, evaluationNamed, parseFindings, specFormAnswered } from "./answer";
import {
  type Fixture,
  type FormRead,
  formProblems,
  type Plan,
  readingProblems,
  readPlan,
} from "./plan";

const REPO = resolve(import.meta.dir, "..", "..");
const SKILL = join(REPO, "skills", "spec-statement-review", "SKILL.md");
const PROJECT = "ACME";
const PERSON = "person@example.com";
const ROOT_SPEC =
  "# A root of The Spec\n\nIts children hold the change requests that the reviewer reads.\n";

const EXIT_PASSES = 0;
const EXIT_FAILS = 1;
const EXIT_CANNOT_RUN = 2;

interface Flags {
  readonly runs: number;
  readonly omp: string;
  readonly model: string;
  readonly fixtures: readonly string[];
  readonly out: string | undefined;
}

const USAGE =
  "usage: ANTHROPIC_API_KEY=<key> [ANTHROPIC_BASE_URL=<gateway>] DISPATCH_TEST_DATABASE_URL=<postgres> bun scripts/spec-review-eval/run.ts [--runs N] [--omp <omp executable>] [--model <model>] [--fixture <id>]... [--out <dir>]";

function readFlags(
  argv: readonly string[],
  caller: Readonly<Record<string, string | undefined>>
): Flags {
  let values: { runs?: string; omp?: string; model?: string; fixture?: string[]; out?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        runs: { type: "string" },
        omp: { type: "string" },
        model: { type: "string" },
        fixture: { type: "string", multiple: true },
        out: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch (error) {
    throw new CannotRunError(`${error instanceof Error ? error.message : String(error)}\n${USAGE}`);
  }
  if ((caller[MODEL_KEY] ?? "") === "") {
    throw new CannotRunError(
      `${MODEL_KEY} is not set, and without it the agent's omp, which gets no models.yml, lists and calls no model\n${USAGE}`
    );
  }
  const runs = Number(values.runs ?? "1");
  if (!Number.isInteger(runs) || runs < 1)
    throw new CannotRunError(`--runs takes a whole number of 1 or more\n${USAGE}`);
  const omp = Bun.which(values.omp ?? "omp");
  if (omp === null) throw new CannotRunError(`no omp at ${values.omp ?? "omp"} on PATH`);
  return {
    runs,
    omp,
    model: values.model ?? MODEL,
    fixtures: values.fixture ?? [],
    out: values.out,
  };
}

/** The server's answer to one request, or a CannotRunError that names the request and the answer. */
async function call<T>(
  stack: DispatchStack,
  method: string,
  route: string,
  body: unknown,
  as: "person" | "agent"
): Promise<T> {
  const response = await fetch(`${stack.url}${route}`, {
    method,
    headers: {
      "Content-Type": "application/json",
      ...(as === "person"
        ? { "X-Dispatch-User": PERSON }
        : { Authorization: `Bearer ${stack.token}` }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  const text = await response.text();
  if (!response.ok) throw new CannotRunError(`${method} ${route}: ${response.status} ${text}`);
  return JSON.parse(text) as T;
}

/** One seeded fixture: its issue and its spec. */
interface Seeded {
  readonly fixture: Fixture;
  readonly issue: string;
  readonly artifact: string;
}

/** A seeded fixture with the line that `dispatch spec-form` prints before the texts of its spec. */
interface Checked extends Seeded {
  readonly heading: string;
}

/**
 * The project, each root with its terms.md, which the person approves, and one child issue for
 * each fixture with the fixture as its spec. A root's title names only its place, and a fixture's
 * issue only its place among the fixtures, so no title names an answer. Each fixture's spec gets
 * the key of its own issue in each `serves`.
 */
async function seed(
  stack: DispatchStack,
  plan: Plan,
  fixtures: readonly Fixture[]
): Promise<Seeded[]> {
  await call(stack, "POST", "/api/v1/projects", { key: PROJECT, name: PROJECT }, "person");
  const rootKeys = new Map<string, string>();
  for (const [index, root] of plan.roots.entries()) {
    const issue = await call<{ key: string }>(
      stack,
      "POST",
      "/api/v1/issues",
      { project: PROJECT, title: `Part ${index + 1} of The Spec`, force: true, spec: ROOT_SPEC },
      "person"
    );
    if (issue.key !== root.key) {
      throw new CannotRunError(
        `the root ${root.name} got the key ${issue.key}, and DISPATCH_SPEC_ROOTS names ${root.key}`
      );
    }
    const terms = await call<{ artifact: { id: string } }>(
      stack,
      "POST",
      `/api/v1/issues/${issue.key}/artifacts`,
      { name: "terms.md", content: root.termsText },
      "person"
    );
    await call(
      stack,
      "POST",
      `/api/v1/artifacts/${terms.artifact.id}/reviews`,
      { state: "approved" },
      "person"
    );
    rootKeys.set(root.name, issue.key);
  }
  const seeded: Seeded[] = [];
  for (const [index, fixture] of fixtures.entries()) {
    const key = `${PROJECT}-${plan.roots.length + index + 1}`;
    const issue = await call<{ key: string; primary_artifact_id: string }>(
      stack,
      "POST",
      "/api/v1/issues",
      {
        project: PROJECT,
        title: `Request ${index + 1} proposes statements for The Spec`,
        parent: rootKeys.get(fixture.root),
        force: true,
        spec: fixture.specText.replaceAll("@ISSUE@", key),
      },
      "person"
    );
    if (issue.key !== key) {
      throw new CannotRunError(
        `the fixture ${fixture.id} got the key ${issue.key}, and its spec names ${key}`
      );
    }
    seeded.push({ fixture, issue: key, artifact: issue.primary_artifact_id });
  }
  return seeded;
}

/**
 * Refuses the run unless the form check of Dispatch reads each fixture in scope with no finding,
 * and what the application's own reader read holds each node and term of its expectation. Each
 * fixture gets the line that an answer of `dispatch spec-form` prints for its spec's version.
 */
async function checkForm(stack: DispatchStack, seeded: readonly Seeded[]): Promise<Checked[]> {
  const problems: string[] = [];
  const checked: Checked[] = [];
  for (const one of seeded) {
    const read = await call<FormRead>(
      stack,
      "GET",
      `/api/v1/artifacts/${one.artifact}/spec-form`,
      undefined,
      "agent"
    );
    problems.push(...formProblems(one.fixture, read), ...readingProblems(one.fixture, read));
    checked.push({ ...one, heading: textsHeading(read.name, read.version) });
  }
  if (problems.length > 0) throw new CannotRunError(problems.join("\n"));
  return checked;
}

/** Refuses an --out that is a file or holds anything, as an earlier run's --out does. */
async function refuseUsedOut(out: string): Promise<void> {
  const earlier = await readdir(out).catch((error: NodeJS.ErrnoException) => {
    if (error.code === "ENOENT") return [];
    if (error.code === "ENOTDIR") throw new CannotRunError(`--out ${out} is not a directory`);
    throw error;
  });
  if (earlier.length > 0) {
    const shown = earlier.sort().slice(0, 3).join(", ");
    const more = earlier.length > 3 ? ` and ${earlier.length - 3} more` : "";
    throw new CannotRunError(
      `--out ${out} is not empty: it holds ${shown}${more}; give a directory that is new or empty`
    );
  }
}

async function evaluate(flags: Flags): Promise<number> {
  const plan = await readPlan();
  const unknown = flags.fixtures.filter(
    (id) => !plan.fixtures.some((fixture) => fixture.id === id)
  );
  if (unknown.length > 0)
    throw new CannotRunError(`expected.json holds no fixture ${unknown.join(", ")}`);
  const fixtures = plan.fixtures.filter(
    (fixture) => flags.fixtures.length === 0 || flags.fixtures.includes(fixture.id)
  );
  if (flags.out !== undefined) await refuseUsedOut(flags.out);
  const out = resolve(flags.out ?? (await mkdtemp(join(tmpdir(), "spec-review-eval-"))));
  await mkdir(out, { recursive: true });
  // The agents' own directories and the compiled dispatch, outside --out and the checkout.
  const agents = await mkdtemp(join(tmpdir(), "spec-review-agents-"));
  // What no agent may see: the checkout with expected.json, this run's --out, the directory of
  // the agents' own directories, which is in the caller's TMPDIR beside earlier runs' --out, and
  // the caller's home, which can hold other checkouts.
  const hidden = [REPO, out, agents, homedir()];
  let sandbox: Sandbox | null = null;
  const results: string[] = [];
  let failed = 0;
  try {
    for (let run = 1; run <= flags.runs; run += 1) {
      const stackDirectory = join(out, `run-${run}`, "stack");
      await mkdir(stackDirectory, { recursive: true });
      const stack = await startDispatchStack({
        work: stackDirectory,
        env: { DISPATCH_SPEC_ROOTS: plan.roots.map((root) => root.key).join(",") },
      });
      try {
        const checked = await checkForm(stack, await seed(stack, plan, fixtures));
        if (sandbox === null) {
          const bwrap = Bun.which("bwrap");
          if (bwrap === null) {
            throw new CannotRunError("no bwrap on PATH: each agent runs in a bubblewrap sandbox");
          }
          sandbox = {
            bwrap,
            omp: await realpath(flags.omp),
            dispatch: await compileDispatch(REPO, join(agents, "dispatch")),
          };
        }
        for (const [index, { fixture, issue, heading }] of checked.entries()) {
          const own = join(agents, `${run}-${index + 1}`);
          const environment = environmentOf(stack, crypto.randomUUID(), process.env);
          await writeAgent(own, SKILL);
          await proveSandbox(sandbox, own, hidden, flags.model, environment);
          const answer = await review(sandbox, own, flags.model, promptOf(issue), environment);
          const records = join(out, `run-${run}`, fixture.id);
          await mkdir(records, { recursive: true });
          await writeFile(join(records, "events.jsonl"), answer.stdout, "utf8");
          await writeFile(join(records, "stderr.txt"), answer.stderr, "utf8");
          const problems = [
            ...(answer.code === 0 ? [] : [`the agent exited ${answer.code}`]),
            ...compareFindings(fixture, parseFindings(answer.text)).problems,
            ...(specFormAnswered(answer.stdout, heading)
              ? []
              : ["the transcript holds no dispatch spec-form call that Dispatch answered"]),
            ...evaluationNamed(answer.stdout).map((named) => `the transcript's ${named}`),
          ];
          const pass = problems.length === 0;
          if (!pass) failed += 1;
          console.log(
            `${pass ? "PASS" : "FAIL"} run ${run} ${fixture.id} (${issue}, ${answer.model ?? "no model"})${pass ? "" : `: ${problems.join("; ")}`}`
          );
          console.log(answer.text.replace(/^/gm, "    "));
          results.push(
            JSON.stringify({
              run,
              fixture: fixture.id,
              issue,
              model: answer.model,
              pass,
              problems,
              answer: answer.text,
            })
          );
        }
      } finally {
        await stack.stop();
      }
    }
  } finally {
    await rm(agents, { recursive: true, force: true });
  }
  await writeFile(join(out, "results.jsonl"), `${results.join("\n")}\n`, "utf8");
  const total = flags.runs * fixtures.length;
  console.log(
    `${total - failed} of ${total} runs passed; transcripts and results.jsonl are in ${out}`
  );
  return failed === 0 ? EXIT_PASSES : EXIT_FAILS;
}

/**
 * Each error that ends the command, of the command or of the stack, the database and the server
 * under it, is the exit status 2: the evaluation is incomplete, whatever verdicts it printed
 * before the error, and 1 would tell the reader that a reviewer failed.
 */
async function main(argv: readonly string[]): Promise<number> {
  try {
    return await evaluate(readFlags(argv, process.env));
  } catch (error) {
    console.error(
      error instanceof CannotRunError
        ? error.message
        : error instanceof Error
          ? (error.stack ?? error.message)
          : String(error)
    );
    return EXIT_CANNOT_RUN;
  }
}

process.exit(await main(Bun.argv.slice(2)));
