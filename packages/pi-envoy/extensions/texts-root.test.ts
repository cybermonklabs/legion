import { expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import {
  DISPATCH_FIRST_MARKER,
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { userTextMessage } from "../src/context-message";

// The launch of an experiment arm's folder of texts names its extension root in LEGION_TEXTS_ROOT
// (the daemon's `texts.RootVariable`), which the entry reads when its module loads. Each row loads
// the entry in a Bun process of its own, started with the variable as a pane is: the host package
// the entry imports is not installed here, and the role takeover smoke's preload stands it in
// without a `mock.module`, which would outlive this file (docs/testing.md). The variable is also in
// the C environment that process started with, which a child started in a PTY begins from.

/** The name the daemon sets, spelled here so a rename on either side fails this file. */
const TEXTS_ROOT = "LEGION_TEXTS_ROOT";
/** The plugin's own skills, as the entry finds them when it runs from the repository. */
const OWN_SKILLS = resolve(import.meta.dir, "..", "..", "..", "skills");
/** The plugin's own dispatch-first, as each request carries it with no root. */
const OWN_DISPATCH_FIRST = readDispatchFirstContext(dispatchFirstSkillFile(OWN_SKILLS));
/** The Dispatch token the child session is handed, so the entry adds dispatch-first. */
const DISPATCH_TOKEN = fixtureSecret("dsp");
const HOST_STUB = join(import.meta.dir, "..", "scripts", "role-takeover", "host-stub.ts");
const STAND_IN_HOST = join(import.meta.dir, "..", "test", "stand-in-host.ts");
const ENTRY = join(import.meta.dir, "envoy.ts");
const REPORT = "texts-root-report ";

const directories = temporaryDirectories("pi-envoy-texts-root-");

/** What one load of the entry gave a session. */
type Session = {
  /** The skill paths `resources_discover` gave. */
  readonly skillPaths: readonly string[];
  /** The messages of two requests the `context` handler built, in order. */
  readonly requests: readonly (readonly unknown[])[];
};

/** What two loads of the entry in one process showed. */
type LoadReport = Session & {
  /** `process.env.LEGION_TEXTS_ROOT` once the module has loaded; null when unset. */
  readonly processEnv: string | null;
  /** What a child started the PTY's way (`env K=V … env`) got; null when unset. */
  readonly pty: string | null;
  /** What a child handed `process.env` as its environment, as the host's shell is, got. */
  readonly shell: string | null;
  /** What the module's second evaluation in the same process gave its session, as Oh My Pi's
   *  loader evaluates it again for each session it creates without prepared extensions. */
  readonly again: Session;
  /** Every warning the host's logger was handed from the first load on. */
  readonly warnings: readonly string[];
};

// Runs in the child: the logger's sink goes on before the entry loads, since the load is what
// logs. Every request the session makes is refused, so Dispatch's answers read fails and the
// requests carry dispatch-first and the failure line. The second import names a query of its own,
// so it is a module of its own, as the loader's `?mtime=<tag>` makes each of its loads.
const SCRIPT = `
  const { logger } = await import("@oh-my-pi/pi-utils");
  const warnings = [];
  logger.registerLogSink((entry) => {
    if (entry.level === "warn") warnings.push(entry.message);
  });
  const { createStandInHost, processSessionContext } = await import(${JSON.stringify(STAND_IN_HOST)});
  globalThis.fetch = async () => new Response("unavailable", { status: 503 });
  const session = async (envoyExtension, id) => {
    const { handlers, pi } = createStandInHost();
    await envoyExtension(pi);
    const context = processSessionContext({ sessionID: id, cwd: process.cwd(), entries: [] });
    const discovered = await handlers.get("resources_discover")({ cwd: process.cwd(), reason: "startup" }, context);
    const turn = { role: "user", content: "Fix the bug.", timestamp: 1 };
    const reply = { role: "assistant", content: [{ type: "text", text: "Done." }], timestamp: 2 };
    const next = { role: "user", content: "Now the test.", timestamp: 3 };
    const requests = [];
    for (const messages of [[turn], [turn, reply, next]]) {
      requests.push((await handlers.get("context")({ messages }, context)).messages);
    }
    return { skillPaths: discovered.skillPaths, requests };
  };
  const { default: first } = await import(${JSON.stringify(ENTRY)});
  const processEnv = process.env.${TEXTS_ROOT} ?? null;
  const named = (text) => {
    const line = text.split("\\n").find((entry) => entry.startsWith("${TEXTS_ROOT}="));
    return line === undefined ? null : line.slice("${TEXTS_ROOT}=".length);
  };
  const overrides = Object.entries(process.env)
    .filter(([, value]) => value !== undefined)
    .map(([name, value]) => name + "=" + value);
  const pty = named(Bun.spawnSync(["env", ...overrides, "env"]).stdout.toString());
  const shell = named(Bun.spawnSync(["env"], { env: { ...process.env } }).stdout.toString());
  const firstSession = await session(first, "ses_texts_root");
  const { default: second } = await import(${JSON.stringify(`${ENTRY}?again`)});
  const again = await session(second, "ses_texts_root_again");
  console.log(${JSON.stringify(REPORT)} + JSON.stringify({
    ...firstSession,
    processEnv,
    pty,
    shell,
    again,
    warnings,
  }));
  process.exit(0);
`;

/**
 * Loads the entry in a process of its own with `root` in LEGION_TEXTS_ROOT (none when undefined),
 * under a HOME of its own whose envoy.json turns AskGate off, with Dispatch configured.
 */
async function load(root: string | undefined): Promise<LoadReport> {
  const home = directories("home");
  const config = join(home, "envoy.json");
  writeFileSync(config, `${JSON.stringify({ askgate: { enabled: false } })}\n`);
  const child = Bun.spawn([process.execPath, "--preload", HOST_STUB, "-e", SCRIPT], {
    cwd: import.meta.dir,
    env: {
      ...bunCacheOff,
      HOME: home,
      PATH: process.env.PATH ?? "/usr/bin:/bin",
      TMPDIR: tmpdir(),
      ENVOY_CONFIG_FILE: config,
      ENVOY_URL: "http://envoy.test",
      DISPATCH_URL: "http://dispatch.test",
      DISPATCH_TOKEN,
      DISPATCH_STATE_DIR: join(home, "dispatch-state"),
      ...(root === undefined ? {} : { [TEXTS_ROOT]: root }),
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, status] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  const line = stdout.split("\n").find((entry) => entry.startsWith(REPORT));
  if (status !== 0 || line === undefined) {
    throw new Error(`the process that loaded the entry exited ${status} with no report: ${stderr}`);
  }
  return JSON.parse(line.slice(REPORT.length)) as LoadReport;
}

/** A root holding `files`, each path under it with its text. */
function rootWith(files: Readonly<Record<string, string>>): string {
  const root = directories("root");
  for (const [path, text] of Object.entries(files)) {
    mkdirSync(join(root, path, ".."), { recursive: true });
    writeFileSync(join(root, path), text);
  }
  return root;
}

/** The warnings of a load that name the variable. */
function rootWarnings(report: LoadReport): string[] {
  return report.warnings.filter((warning) => warning.includes(TEXTS_ROOT));
}

const ROOT_DISPATCH_FIRST = [
  "---",
  "name: dispatch-first",
  'description: "The arm\'s dispatch-first."',
  "---",
  "",
  "# The arm's dispatch first",
  "",
  "ARM-DISPATCH-FIRST-MARKER: search before you act, as this arm words it.",
  "",
].join("\n");

const ROOT_SKILL = [
  "---",
  "name: envoy",
  'description: "The arm\'s envoy skill."',
  "---",
  "",
  "The arm's envoy.",
  "",
].join("\n");

test("a root's dispatch-first is the text each request carries", async () => {
  const root = rootWith({ "skills/dispatch-first/SKILL.md": ROOT_DISPATCH_FIRST });
  const report = await load(root);

  const carried = userTextMessage(
    readDispatchFirstContext(join(root, "skills", "dispatch-first", "SKILL.md"))
  );
  expect(JSON.stringify(carried)).toContain("ARM-DISPATCH-FIRST-MARKER");
  expect(report.requests).toHaveLength(2);
  for (const messages of report.requests) {
    expect(messages[0]).toEqual(carried);
    expect(JSON.stringify(messages).split(DISPATCH_FIRST_MARKER)).toHaveLength(2);
    expect(JSON.stringify(messages)).not.toContain("# Dispatch first");
  }
});

test("resources_discover gives the root's skills folder in place of its own", async () => {
  const root = rootWith({ "skills/envoy/SKILL.md": ROOT_SKILL });
  const report = await load(root);

  expect(report.skillPaths).toEqual([join(root, "skills")]);
  // The root holds no dispatch-first of its own, so the requests carry the plugin's.
  for (const messages of report.requests) {
    expect(messages[0]).toEqual(userTextMessage(OWN_DISPATCH_FIRST));
  }
});

test("a root with no dispatch-first and no skills folder keeps the plugin's own", async () => {
  const root = rootWith({ "agents/goal-red-team.md": "An agent and no skill.\n" });
  const report = await load(root);

  expect(report.skillPaths).toEqual([OWN_SKILLS]);
  for (const messages of report.requests) {
    expect(messages[0]).toEqual(userTextMessage(OWN_DISPATCH_FIRST));
  }
  expect(rootWarnings(report)).toEqual([]);
});

test("a root that does not exist is no root, and the load says so once", async () => {
  const missing = join(directories("parent"), "gone");
  const report = await load(missing);

  expect(report.skillPaths).toEqual([OWN_SKILLS]);
  for (const messages of report.requests) {
    expect(messages[0]).toEqual(userTextMessage(OWN_DISPATCH_FIRST));
  }
  expect(rootWarnings(report)).toEqual([
    `envoy: ${TEXTS_ROOT} names ${missing}, which is no directory: this session reads the plugin's own skills and dispatch-first`,
  ]);
  expect(report.processEnv).toBe("");
});

// An Oh My Pi the session starts reads Legion's own texts: the variable names no root in
// `process.env`, in a child handed `process.env` (the host's shell) and in a child started the
// PTY's way, which a delete would not reach.
test("the variable is gone from process.env once the module has loaded", async () => {
  const root = rootWith({ "skills/dispatch-first/SKILL.md": ROOT_DISPATCH_FIRST });
  const report = await load(root);

  expect(report.skillPaths).toEqual([join(root, "skills")]);
  expect(report.processEnv).toBe("");
  expect(report.shell).toBe("");
  expect(report.pty).toBe("");
});

test("with no root nothing changes", async () => {
  for (const root of [undefined, ""]) {
    const report = await load(root);

    expect(report.skillPaths).toEqual([OWN_SKILLS]);
    for (const messages of report.requests) {
      expect(messages[0]).toEqual(userTextMessage(OWN_DISPATCH_FIRST));
    }
    expect(report.processEnv).toBe(root ?? null);
    expect(report.pty).toBe(root ?? null);
    expect(rootWarnings(report)).toEqual([]);
  }
});

// Oh My Pi's extension loader imports `<path>?mtime=<tag>` at each call, so a second load in the
// same process evaluates the entry again after the first load has cleared LEGION_TEXTS_ROOT to "".
// The process keeps the root the first load read, so both loads serve the arm's skills and
// dispatch-first.
test("a second load of the entry in the same process keeps the root", async () => {
  const root = rootWith({ "skills/dispatch-first/SKILL.md": ROOT_DISPATCH_FIRST });
  const report = await load(root);

  const carried = userTextMessage(
    readDispatchFirstContext(join(root, "skills", "dispatch-first", "SKILL.md"))
  );
  expect(report.processEnv).toBe("");
  for (const loaded of [report, report.again]) {
    expect(loaded.skillPaths).toEqual([join(root, "skills")]);
    for (const messages of loaded.requests) {
      expect(messages[0]).toEqual(carried);
    }
  }
});
