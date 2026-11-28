/**
 * The fresh agent of one fixture, in a sandbox of its own (bubblewrap). What the sandbox hides,
 * and what it does not:
 *
 * - It hides every path of the caller's machine but the system's directories (SYSTEM, read-only),
 *   omp and the compiled `dispatch`: so the checkout with expected.json, --out of this run and of
 *   earlier ones, the caller's TMPDIR and the caller's home. Before each agent, proveSandbox checks
 *   that in the agent's own sandbox, and refuses the run (exit 2) where it does not hold.
 * - It does not hide the system's directories. A checkout or an --out under one of them, such as
 *   /usr/local/src, is in the agent's reach, so the probe refuses that run and names the path.
 * - It does not cut the network, which reaches the stack and the model's API, so a copy of the
 *   repository from the network is in the agent's reach (EVALUATION_NAMES in answer.ts says what
 *   catches such a copy, and what does not).
 *
 * The agent's omp gets no models.yml of the caller's, which would put every provider's key in its
 * reach: it reaches the model only through MODEL_VARIABLES. So the command refuses to run where
 * MODEL_KEY is not set, and proveSandbox refuses a model that omp in the sandbox does not list.
 */
import { copyFile, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { CannotRunError, recordOf } from "../approval-eval/corpus-io";
import { answerOf, PROFILE } from "../approval-eval/fresh-agent";

/** The agent's own directory inside its sandbox: its home, its work, its state and the skill. */
const OWN = "/review";
const HOME = `${OWN}/home`;
const WORK = `${OWN}/work`;
const STATE = `${OWN}/state`;
const SKILL = `${OWN}/SKILL.md`;
/** The agent's `dispatch`, this checkout's CLI compiled to one file, and omp, inside its sandbox. */
const TOOLS = "/opt/bin";
const DISPATCH = `${TOOLS}/dispatch`;
const OMP = "/opt/omp/omp";
/** The system's directories, read-only in the sandbox: its commands, libraries and settings. */
const SYSTEM = [
  "/usr",
  "/bin",
  "/sbin",
  "/lib",
  "/lib32",
  "/lib64",
  "/etc",
  "/run/systemd/resolve",
];
/**
 * How long one command in an agent's sandbox may take before it is killed: a killed agent run
 * fails, and a killed probe or model check refuses the run.
 */
const RUN_TIMEOUT_MS = 600_000;

/** The caller's bwrap, and the omp and compiled `dispatch` that every sandbox binds read-only. */
export interface Sandbox {
  readonly bwrap: string;
  readonly omp: string;
  readonly dispatch: string;
}

/** What one agent run returned. */
export interface Run {
  readonly text: string;
  readonly model: string | null;
  readonly stdout: string;
  readonly stderr: string;
  readonly code: number;
}

/**
 * The key without which the agent's omp lists and calls no model. With it, omp lists Anthropic's
 * models and calls Anthropic's own API.
 */
export const MODEL_KEY = "ANTHROPIC_API_KEY";
/** MODEL_KEY, and the address of a gateway that omp calls in place of that API where it is set. */
export const MODEL_VARIABLES = [MODEL_KEY, "ANTHROPIC_BASE_URL"] as const;

export function promptOf(issue: string): string {
  return `You review a change request. Read ${SKILL} and follow it for issue ${issue}, document spec.md.`;
}

/**
 * The environment of an agent: its own home, state and TMPDIR, a PATH of its own `dispatch` and
 * the system's directories, the stack's address and token, and each of MODEL_VARIABLES that the
 * caller sets, which are the only values it takes from the caller.
 */
export function environmentOf(
  stack: { readonly url: string; readonly token: string },
  session: string,
  caller: Readonly<Record<string, string | undefined>>
): Record<string, string> {
  const environment: Record<string, string> = {
    HOME,
    PATH: `${TOOLS}:/usr/local/bin:/usr/bin:/bin`,
    TMPDIR: "/tmp",
    DISPATCH_URL: stack.url,
    DISPATCH_TOKEN: stack.token,
    DISPATCH_HOST: "omp",
    DISPATCH_SESSION_ID: session,
    DISPATCH_STATE_DIR: STATE,
  };
  for (const name of MODEL_VARIABLES) {
    const value = caller[name];
    if (value !== undefined && value !== "") environment[name] = value;
  }
  return environment;
}

/** bwrap's arguments for the agent whose own directory is `own` on the caller's machine. */
export function sandboxArgs(sandbox: Sandbox, own: string): string[] {
  return [
    "--die-with-parent",
    // A session of its own, so the agent holds no terminal of the caller's: through that terminal
    // a process in the sandbox could push keystrokes for the caller's shell to run (CVE-2017-5226).
    "--new-session",
    "--unshare-all",
    "--share-net",
    ...SYSTEM.flatMap((path) => ["--ro-bind-try", path, path]),
    "--proc",
    "/proc",
    "--dev",
    "/dev",
    "--tmpfs",
    "/tmp",
    "--bind",
    own,
    OWN,
    "--ro-bind",
    sandbox.dispatch,
    DISPATCH,
    "--ro-bind",
    sandbox.omp,
    OMP,
    "--chdir",
    WORK,
  ];
}

/** This checkout's `dispatch` command, compiled to one file at `into`, which a sandbox binds. */
export async function compileDispatch(repo: string, into: string): Promise<string> {
  const child = Bun.spawn({
    cmd: [
      process.execPath,
      "build",
      "--compile",
      join(repo, "packages", "envoy-client", "bin", "dispatch.ts"),
      "--outfile",
      into,
    ],
    stdout: "ignore",
    stderr: "pipe",
  });
  const [stderr, code] = await Promise.all([new Response(child.stderr).text(), child.exited]);
  if (code !== 0)
    throw new CannotRunError(`bun build --compile of dispatch exited ${code}\n${stderr}`);
  return into;
}

/** Makes the agent's own directory: its omp profile, its work and state, and a copy of the skill. */
export async function writeAgent(own: string, skill: string): Promise<void> {
  const profile = join(own, "home", ".omp", "profiles", PROFILE, "agent");
  for (const path of [profile, join(own, "work"), join(own, "state")]) {
    await mkdir(path, { recursive: true });
  }
  await writeFile(join(profile, "config.yml"), "skills:\n  customDirectories: []\n", "utf8");
  await copyFile(skill, join(own, "SKILL.md"));
}

/**
 * What a probe in the agent's sandbox found: each path of `own` it must see, each path of `hidden`
 * it must not, and whether omp runs there. Each path needs its own line, `seen` or `hidden`: a
 * path with no line is no proof that it is hidden.
 */
export function probeProblems(
  printed: string,
  own: readonly string[],
  hidden: readonly string[]
): string[] {
  const found = new Map<string, string>();
  for (const line of printed.split("\n")) {
    const [state, ...path] = line.split(" ");
    if (state === "seen" || state === "hidden") found.set(path.join(" "), state);
  }
  return [
    ...own
      .filter((path) => found.get(path) !== "seen")
      .map((path) => `the agent's sandbox does not show it ${path}`),
    ...hidden
      .filter((path) => found.get(path) !== "hidden")
      .map((path) =>
        found.has(path)
          ? `the agent's sandbox shows it ${path}, which holds the answers`
          : `the probe gave no line for ${path}, so nothing shows that it is hidden`
      ),
    ...(printed.includes("omp runs\n") ? [] : ["omp does not run in the agent's sandbox"]),
  ];
}

/**
 * What is wrong where `printed`, the output of `omp models find <model> --json` in the agent's
 * sandbox, lists no model `model` by its id or its selector; null where it lists it.
 */
export function modelProblem(printed: string, model: string): string | null {
  let models: unknown;
  try {
    models = recordOf(JSON.parse(printed))?.models;
  } catch {
    models = undefined;
  }
  if (!Array.isArray(models)) {
    return `omp in the agent's sandbox printed no list of models for ${model}: ${printed.trim().slice(0, 200) || "nothing"}`;
  }
  const listed = models.some((one) => {
    const record = recordOf(one);
    return record?.id === model || record?.selector === model;
  });
  return listed
    ? null
    : `omp in the agent's sandbox lists no model ${model}: the agent gets no models.yml, so omp there lists only Anthropic's models, and those only with ${MODEL_KEY}`;
}

/** What a command in the agent's sandbox printed, and its exit status. */
interface Sandboxed {
  readonly stdout: string;
  readonly stderr: string;
  readonly code: number;
}

/** Runs `command` in the sandbox of the agent whose own directory is `own`, with no stdin. */
async function sandboxed(
  sandbox: Sandbox,
  own: string,
  environment: Record<string, string>,
  command: readonly string[]
): Promise<Sandboxed> {
  const child = Bun.spawn({
    cmd: [sandbox.bwrap, ...sandboxArgs(sandbox, own), ...command],
    env: environment,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    timeout: RUN_TIMEOUT_MS,
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { stdout, stderr, code };
}

/**
 * Refuses the run unless the agent's sandbox shows it its own files and runs omp there, hides
 * each path of `hidden`, the caller's paths that hold the expected answers and earlier runs'
 * transcripts, and lists `model` to omp with the agent's own `environment`. The probe is the same
 * sandbox and the same environment as the agent's, so a sandbox that cannot hide the answers, or
 * an agent that cannot reach its model, stops the command before any agent runs.
 */
export async function proveSandbox(
  sandbox: Sandbox,
  own: string,
  hidden: readonly string[],
  model: string,
  environment: Record<string, string>
): Promise<void> {
  const mine = [SKILL, DISPATCH, HOME, WORK];
  const script = [
    'for path in "$@"; do',
    '  if [ -e "$path" ]; then echo "seen $path"; else echo "hidden $path"; fi',
    "done",
    `if ${OMP} --version >/dev/null 2>&1; then echo "omp runs"; fi`,
  ].join("\n");
  const probe = await sandboxed(sandbox, own, environment, [
    "/bin/sh",
    "-c",
    script,
    "probe",
    ...mine,
    ...hidden,
  ]);
  const problems =
    probe.code === 0
      ? probeProblems(probe.stdout, mine, hidden)
      : [`bwrap exited ${probe.code}: ${probe.stderr.trim()}`];
  if (problems.length > 0) {
    throw new CannotRunError(
      `the command refuses the run: the agent's sandbox cannot hide the answers\n${problems.join("\n")}`
    );
  }
  const listing = await sandboxed(sandbox, own, environment, [
    OMP,
    "--profile",
    PROFILE,
    "models",
    "find",
    model,
    "--json",
  ]);
  const problem =
    listing.code === 0
      ? modelProblem(listing.stdout, model)
      : `omp models find exited ${listing.code} in the agent's sandbox: ${listing.stderr.trim()}`;
  if (problem !== null) {
    throw new CannotRunError(
      `the command refuses the run: no agent can reach its model\n${problem}`
    );
  }
}

/** One fresh agent in its sandbox, with the tools read and bash, on the prompt for its issue. */
export async function review(
  sandbox: Sandbox,
  own: string,
  model: string,
  prompt: string,
  environment: Record<string, string>
): Promise<Run> {
  const { stdout, stderr, code } = await sandboxed(sandbox, own, environment, [
    OMP,
    "--profile",
    PROFILE,
    "--cwd",
    WORK,
    "--model",
    model,
    "--tools",
    "read,bash",
    "--no-skills",
    "--no-extensions",
    "--no-rules",
    "--no-context-files",
    "--no-session",
    "--no-title",
    "--mode",
    "json",
    "-p",
    prompt,
  ]);
  return { ...answerOf(stdout), stdout, stderr, code };
}
