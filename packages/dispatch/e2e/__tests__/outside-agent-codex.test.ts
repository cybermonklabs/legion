/** The Codex leg of the outside agent's acceptance (`e2e/acceptance/outside-agent-codex.sh`, rows
 *  C0-C7) against stand-ins of what it drives; its stand-in model
 *  (`e2e/acceptance/outside-agent-codex-model.ts`) against the requests Codex sends it; and the
 *  reads of its provenance helper (`e2e/acceptance/outside-agent-codex-provenance.ts`).
 *
 *  The leg's stand-ins: an `npm` that installs a stand-in `codex` as the native binary of
 *  `@openai/codex`, and answers `npm audit signatures` as the scenario says; that `codex`, which
 *  installs the plugin release its marketplace names (a root holding a stand-in `dist/dispatch.js`,
 *  the terminal entry), files the session record the plugin's SessionStart hook would, and writes
 *  the stand-in model's log as the real model would for each run; a stand-in model that binds a
 *  port of its own and answers `/_ready` with the leg's nonce; a provenance helper and an
 *  `outside-agent-probe.ts` beside the leg that answer as the scenario says; and a git checkout
 *  whose commits the provenance names. The stand-in `codex` and the probe meet through files in a
 *  state directory: the probe's send to the session is what the session's next request holds, and
 *  the session's send is what the probe's receiver reads.
 *
 *  Each row reads PASS where the stand-ins behave as the edge, the registry and the plugin should,
 *  and, in the same file, not PASS where one crosses the rule the row reads. */

import { describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { findTool, nextTurn, type Plan, whoamiOf } from "../acceptance/outside-agent-codex-model";
import { compareBundles, provenanceOf } from "../acceptance/outside-agent-codex-provenance";
import { rowLimit, runBounded } from "./bounded-subprocess";
import {
  codexLeg,
  codexRows,
  copyLeg,
  type LegRun,
  rowsOf,
  script,
  textToolPath,
  verdictsOf,
} from "./outside-agent-fixture";
import { closedPort } from "./outside-path-reach-fixtures";

// On tmpfs, as the other two files of these legs keep theirs: a row here holds at most 4 runs of
// about 40 entries each.
const scratch = temporaryDirectories("outside-agent-codex", {
  root: memoryRoot({ need: 8 * 2 ** 20 }),
});

const person = "outside-e2e@example.com";
/** The release the stand-in runs install, and what its stand-in provenance names. */
const release = "5.0.1";

interface Whoami {
  readonly exit: number;
  readonly owner?: string;
  readonly project?: string;
  /** The session the answer names: the run's own (default), or another. */
  readonly session?: string;
  /** The words of a call that did not exit 0. */
  readonly detail?: string;
}

interface CodexScenario {
  /** What `codex --version` prints (default the pinned release). */
  readonly version?: string;
  /** Each run's whoami: main (C2), edge-refused (C6), inside (C2's control). */
  readonly whoami?: {
    readonly main?: Whoami;
    readonly "edge-refused"?: Whoami;
    readonly inside?: Whoami;
  };
  /** Whether the edge lists the session (C1; default yes). */
  readonly registers?: boolean;
  /** The probe's send to the session reaches it (C4; default yes). */
  readonly inboundDropped?: boolean;
  /** The control's send to `<peer>-foreign` reaches the session anyway (C4's control). */
  readonly controlDelivered?: boolean;
  /** The comment the session writes is not on the issue, or was written with scopes (C3). */
  readonly commentLost?: boolean;
  readonly commentScopes?: number;
  /** The session's envoy_send fails, or succeeds and never reaches the probe (C5). */
  readonly sendFails?: boolean;
  readonly outboundDropped?: boolean;
  /** OUTSIDE_INSIDE_DISPATCH_URL is not given (C2's control does not run). */
  readonly noInside?: boolean;
  /** OUTSIDE_CODEX_ENVOY_VERSION as given (default the release; null leaves it unset). */
  readonly envoyVersion?: string | null;
  /** OUTSIDE_CODEX_ENVOY_SOURCE=checkout, a development run. */
  readonly fromCheckout?: boolean;
  /** The registry's answer of the release's provenance, where it differs from one the release
   *  workflow wrote at the commit OUTSIDE_CODEX_ENVOY_COMMIT names. */
  readonly provenance?: Record<string, unknown>;
  /** OUTSIDE_DEVICE_LOGIN_COMMIT names a commit the release's commit does not hold. */
  readonly loginApart?: boolean;
  /** What `npm audit signatures` verifies: an attestation (default) or none. */
  readonly audit?: "attested" | "none";
  /** The installed plugin has no dist/dispatch.js, or its whoami answers otherwise (C7). */
  readonly noEntry?: boolean;
  readonly terminal?: "config" | "other";
  /** The stand-in model answers /_ready with another word than the leg's nonce, as another run's
   *  stand-in on the port would. */
  readonly modelSquat?: boolean;
}

/** The installed plugin's stand-in `dist/dispatch.js`, the terminal entry of C7: it keeps what it
 *  was run with and answers whoami as the scenario says. */
function standInEntry(state: string): string {
  return `import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
const state = ${JSON.stringify(state)};
const scenario = JSON.parse(readFileSync(join(state, "scenario.json"), "utf8"));
const seen = ["DISPATCH_HOST", "CODEX_THREAD_ID", "HOME", "AGENT_SECRETS_MACHINE_IDENTITY", "DISPATCH_URL"];
writeFileSync(join(state, "terminal.json"), JSON.stringify({ argv: process.argv.slice(2), env: Object.fromEntries(seen.map((name) => [name, process.env[name] ?? null])) }));
if (scenario.terminal === "config") {
  console.log("dispatch: dispatch config: AGENT_SECRETS_MACHINE_IDENTITY names no identity this machine can prove; it takes kubernetes, metadata or pool");
  process.exit(2);
}
const owner = scenario.terminal === "other" ? "someone@example.com" : ${JSON.stringify(person)};
console.log("Session " + process.env.CODEX_THREAD_ID + " acts for " + owner + ": issues you create without an assignee are assigned to " + owner + ". It may write project EXAMPLE and no other.");
`;
}

/** The stand-in `codex`: a Bun script with this run's state directory written into it. */
function standInCodex(state: string): string {
  return `#!/usr/bin/env bun
import { appendFileSync, copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
const state = ${JSON.stringify(state)};
const scenario = JSON.parse(readFileSync(join(state, "scenario.json"), "utf8"));
const argv = process.argv.slice(2);
const home = process.env.CODEX_HOME ?? "";
if (argv[0] === "--version") {
  console.log("codex-cli " + (scenario.version ?? "0.171.0"));
  process.exit(0);
}
// As Codex does, \`plugin marketplace add\` and \`plugin add\` add their tables to config.toml, and a
// run whose config.toml lacks the plugin's table runs the plugin not at all: no hook, no tool.
// \`plugin add\` installs the release the marketplace's npm source names (a local source as 1.0.0)
// under the plugin cache, and names that root, as Codex 0.171.0 does.
const config = join(home, "config.toml");
if (argv[0] === "plugin") {
  appendFileSync(join(state, "plugin-args"), argv.join(" ") + "\\n");
  if (argv[1] === "marketplace") {
    copyFileSync(join(argv[3], ".agents", "plugins", "marketplace.json"), join(state, "marketplace.json"));
    appendFileSync(config, '\\n[marketplaces.outside-e2e]\\nsource_type = "local"\\n');
  }
  if (argv[1] === "add") {
    const source = JSON.parse(readFileSync(join(state, "marketplace.json"), "utf8")).plugins[0].source;
    const version = source.source === "npm" ? source.version : "1.0.0";
    const root = join(home, "plugins", "cache", "outside-e2e", "codex-envoy", version);
    mkdirSync(join(root, "dist"), { recursive: true });
    writeFileSync(join(root, "package.json"), JSON.stringify({ version }));
    if (!scenario.noEntry) copyFileSync(join(state, "entry.js"), join(root, "dist", "dispatch.js"));
    appendFileSync(config, '\\n[plugins."codex-envoy@outside-e2e"]\\nenabled = true\\n');
    console.log("Added plugin \`codex-envoy\` from marketplace \`outside-e2e\`. Installed plugin root: " + root);
  }
  process.exit(0);
}
if (argv[0] !== "exec") process.exit(2);
const run = /codex-run (\\S+) /.exec(argv.at(-1) ?? "")?.[1] ?? "";
const session = "codex-thread-" + run;
const seen = ["ENVOY_URL", "DISPATCH_URL", "HOME", "AGENT_SECRETS_MACHINE_IDENTITY", "AGENT_SECRETS_PROJECTS", "DISPATCH_TOKEN"];
writeFileSync(join(state, "exec-" + run + ".json"), JSON.stringify({
  argv,
  cwd: process.cwd(),
  env: Object.fromEntries(seen.map((name) => [name, process.env[name] ?? null])),
}));
const [planFile, logFile] = readFileSync(join(state, "model-argv"), "utf8").trim().split("\\n");
const plan = JSON.parse(readFileSync(planFile, "utf8"));
const log = (line) => appendFileSync(logFile, JSON.stringify({ run, ...line }) + "\\n");
const configured = existsSync(config) ? readFileSync(config, "utf8") : "";
if (!configured.includes('[plugins."codex-envoy@outside-e2e"]') || !configured.includes("model_provider = ")) {
  log({ step: "tools", dispatch: "", send: "" });
  log({ step: "end" });
  process.exit(0);
}
const records = join(home, "plugins", "data", "codex-envoy-outside-e2e", "codex-sessions");
mkdirSync(records, { recursive: true });
writeFileSync(join(records, process.pid + ".json"), JSON.stringify({ session_id: session }));
if (scenario.registers !== false) appendFileSync(join(state, "sessions"), session + "\\n");
const until = async (file) => { while (!existsSync(join(state, file))) await Bun.sleep(25); };
log({ step: "tools", dispatch: "mcp__envoy.dispatch", send: "mcp__envoy.envoy_send" });
const wanted = (scenario.whoami ?? {})[run] ?? (run === "inside" ? { exit: 2 } : { exit: 0 });
log({
  step: "whoami",
  exit: wanted.exit,
  session: wanted.session ?? session,
  owner: wanted.exit === 0 ? (wanted.owner ?? ${JSON.stringify(person)}) : "",
  project: wanted.exit === 0 ? (wanted.project ?? "EXAMPLE") : "",
  elsewhere: "",
  ...(wanted.exit === 0 ? {} : { detail: wanted.detail ?? "dispatch: Dispatch did not answer [exit " + wanted.exit + "]" }),
});
if (run === "main") {
  if (scenario.registers === false) await new Promise(() => {});
  if (plan.issue !== "") {
    log({ step: "comment", exit: 0, comment_id: "3f1c2a9e-8b7d-4c11-9e2a-5d6f7a8b9c0d" });
    if (!scenario.commentLost) writeFileSync(join(state, "comment"), plan.comment);
  }
  await until("send-attempted");
  if (existsSync(join(state, "control"))) log({ step: "control", at: Date.now() });
  if (existsSync(join(state, "inbound"))) {
    log({ step: "inbound", at: Date.now(), waits: 1 });
    log({ step: "send", at: Date.now() });
    if (!scenario.sendFails && !scenario.outboundDropped) {
      writeFileSync(join(state, "outbound"), JSON.stringify({ text: plan.outbound, from: session, to: plan.outboundTo }));
    }
    log({ step: "sent", ok: !scenario.sendFails, detail: scenario.sendFails ? "tool call error: Envoy is not ready" : "delivered to " + plan.outboundTo });
  } else {
    log({ step: "wait-ended", waits: plan.waitCalls });
  }
  writeFileSync(join(state, "main-ended"), "");
}
log({ step: "end" });
`;
}

/** The stand-in probe: answers each command the Codex leg runs, from the state directory. */
function standInProbe(state: string): string {
  return `import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
const state = ${JSON.stringify(state)};
const scenario = JSON.parse(readFileSync(join(state, "scenario.json"), "utf8"));
const [command, ...args] = Bun.argv.slice(2);
const print = (value) => process.stdout.write(JSON.stringify(value) + "\\n");
const ok = { status: 200, code: null, detail: null };
switch (command) {
  case "issue":
    print({ ...ok, key: "EXAMPLE-7" });
    break;
  case "sessions": {
    const file = join(state, "sessions");
    const sessions = existsSync(file) ? readFileSync(file, "utf8").trim().split("\\n") : [];
    print({ ...ok, sessions, at_dir: null });
    break;
  }
  case "send": {
    const [target] = args;
    if (target.endsWith("-foreign")) {
      if (scenario.controlDelivered) writeFileSync(join(state, "control"), args.slice(1).join(" "));
      print({ status: 404, code: null, detail: "no such session" });
      break;
    }
    if (!scenario.inboundDropped) writeFileSync(join(state, "inbound"), args.slice(1).join(" "));
    writeFileSync(join(state, "send-attempted"), "");
    print(ok);
    break;
  }
  case "receive": {
    print({ ready: true });
    while (!existsSync(join(state, "outbound")) && !existsSync(join(state, "main-ended"))) await Bun.sleep(25);
    const file = join(state, "outbound");
    if (existsSync(file)) {
      const sent = JSON.parse(readFileSync(file, "utf8"));
      if (sent.to === process.env.OUTSIDE_PROBE_SESSION) print({ read: sent.text, from: sent.from, at: Date.now() });
      else print({ read: null, from: null, at: Date.now() });
    } else print({ read: null, from: null, at: Date.now() });
    break;
  }
  case "comment-find": {
    const found = existsSync(join(state, "comment")) && readFileSync(join(state, "comment"), "utf8") === args.slice(1).join(" ");
    print(found
      ? { ...ok, found: true, id: "3f1c2a9e-8b7d-4c11-9e2a-5d6f7a8b9c0d", author_kind: "agent", owner: ${JSON.stringify(person)}, project: "EXAMPLE", session: "codex-thread-main", scope_count: scenario.commentScopes ?? 0 }
      : { ...ok, found: false, id: null, author_kind: null, owner: null, project: null, session: null, scope_count: null });
    break;
  }
  default:
    print({ unknown: "the stand-in probe has no " + command });
    process.exit(2);
}
`;
}

interface CodexRun extends LegRun {
  readonly state: string;
  readonly inside: string;
  readonly listener: string;
  readonly loginHome: string;
}

/** A git command in `dir`, with an identity and no host configuration of its own. */
function git(dir: string, ...args: string[]): string {
  const ran = Bun.spawnSync(["git", "-C", dir, "-c", "init.defaultBranch=main", ...args], {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: dir,
      GIT_CONFIG_NOSYSTEM: "1",
      GIT_AUTHOR_NAME: "example",
      GIT_AUTHOR_EMAIL: "example@example.com",
      GIT_COMMITTER_NAME: "example",
      GIT_COMMITTER_EMAIL: "example@example.com",
    },
  });
  if (!ran.success) throw new Error(`git ${args.join(" ")}: ${ran.stderr.toString()}`);
  return ran.stdout.toString().trim();
}

/** A checkout with the device login's commit, the release's commit that holds it, and another
 *  commit that does not; and a plugin build that makes a directory, for a development run. */
function standInCheckout(): { dir: string; login: string; built: string; apart: string } {
  const dir = scratch("checkout");
  const build = join(dir, "packages", "claude-envoy", "scripts");
  mkdirSync(build, { recursive: true });
  writeFileSync(
    join(build, "build.ts"),
    `import { mkdirSync, writeFileSync } from "node:fs";
const out = process.argv[process.argv.indexOf("--codex-out") + 1];
mkdirSync(out + "/.codex-plugin", { recursive: true });
writeFileSync(out + "/.codex-plugin/plugin.json", "{}");
`
  );
  git(dir, "init", "-q", "--template=");
  git(dir, "add", "-A");
  git(dir, "commit", "-q", "-m", "the device login");
  const login = git(dir, "rev-parse", "HEAD");
  git(dir, "commit", "-q", "--allow-empty", "-m", "the release");
  const built = git(dir, "rev-parse", "HEAD");
  const apart = git(dir, "commit-tree", `${login}^{tree}`, "-m", "apart");
  return { dir, login, built, apart };
}

/** One run of a copy of the Codex leg against the stand-ins, as `scenario` says. */
async function codexStandInRun(scenario: CodexScenario): Promise<CodexRun> {
  const leg = copyLeg(scratch, codexLeg);
  const legDir = leg.slice(0, leg.lastIndexOf("/"));
  const state = scratch("state");
  const repo = standInCheckout();
  writeFileSync(join(state, "scenario.json"), JSON.stringify(scenario));
  writeFileSync(join(state, "entry.js"), standInEntry(state));
  await Bun.write(join(legDir, "outside-agent-probe.ts"), standInProbe(state));
  // The stand-in model binds a port of its own, prints it and answers /_ready with the plan's
  // nonce, as the real one does; it answers no model request, since the stand-in codex writes the
  // model's log itself, from the plan and log paths the leg gave the model. A squat answers
  // /_ready as another run's stand-in on that port would.
  await Bun.write(
    join(legDir, "outside-agent-codex-model.ts"),
    `import { readFileSync, writeFileSync } from "node:fs";
const [planFile, logFile] = Bun.argv.slice(2);
writeFileSync(${JSON.stringify(join(state, "model-argv"))}, planFile + "\\n" + logFile + "\\n");
const nonce = ${scenario.modelSquat === true ? '"ready"' : 'JSON.parse(readFileSync(planFile, "utf8")).nonce'};
const server = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch: (request) => new URL(request.url).pathname === "/_ready" ? new Response(nonce) : new Response("no route", { status: 404 }) });
console.log(JSON.stringify({ port: server.port }));
`
  );
  // The stand-in provenance helper: the registry's record of the release, with the provenance the
  // release workflow writes at the release's commit unless the scenario says otherwise.
  await Bun.write(
    join(legDir, "outside-agent-codex-provenance.ts"),
    `const [command] = Bun.argv.slice(2);
if (command === "compare") console.log(JSON.stringify({ equal: 7, differ: "", only_built: "", only_installed: "" }));
else console.log(JSON.stringify({ status: 200, integrity: "sha512-standin", provenance: true, repository: "https://github.com/cybermonklabs/legion", workflow: ".github/workflows/release.yaml", ref: "refs/heads/main", commit: ${JSON.stringify(repo.built)}, subject_matches: true, ...${JSON.stringify(scenario.provenance ?? {})} }));
`
  );
  const bin = textToolPath(scratch, ["bun", "git", "tar"]);
  const codex = script(join(state, "codex"), standInCodex(state));
  // npm: installs the stand-in codex for \`npm install --prefix <dir> @openai/codex@<v>\`, takes the
  // release for the audit project, and answers \`npm audit signatures\` as the scenario says.
  script(
    join(bin, "npm"),
    `#!/usr/bin/env bun
import { appendFileSync, copyFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
const argv = process.argv.slice(2);
appendFileSync(${JSON.stringify(join(state, "npm-args"))}, argv.join(" ") + "\\n");
if (argv[0] === "audit") {
  console.log("audited 1 package in 0s\\n\\n1 package has a verified registry signature\\n");
  if (${JSON.stringify(scenario.audit ?? "attested")} === "attested") console.log("1 package has a verified attestation\\n");
  process.exit(0);
}
if (!argv.includes("--prefix")) process.exit(0);
const prefix = argv[argv.indexOf("--prefix") + 1];
const dir = join(prefix, "node_modules", "@openai", "codex-linux-x64", "vendor", "x86_64-unknown-linux-musl", "bin");
mkdirSync(dir, { recursive: true });
copyFileSync(${JSON.stringify(codex)}, join(dir, "codex"));
`
  );
  const loginHome = scratch("login-home");
  await Bun.write(join(loginHome, ".config", "legion", "device-login.json"), "{}");
  const listener = `http://127.0.0.1:${await closedPort()}`;
  const inside = `http://127.0.0.1:${await closedPort()}`;
  const envoyVersion = scenario.envoyVersion === undefined ? release : scenario.envoyVersion;
  const { code, stdout, stderr } = await runBounded({
    argv: ["bash", leg],
    bound: "short",
    cwd: scratch("cwd"),
    env: {
      PATH: bin,
      HOME: scratch("home"),
      ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
      OUTSIDE_ALLOW_HTTP: "1",
      OUTSIDE_DISPATCH_URL: `http://127.0.0.1:${await closedPort()}`,
      OUTSIDE_BROKER_URL: `http://127.0.0.1:${await closedPort()}`,
      OUTSIDE_LISTENER_URL: listener,
      ...(scenario.noInside === true ? {} : { OUTSIDE_INSIDE_DISPATCH_URL: inside }),
      OUTSIDE_PROJECT: "EXAMPLE",
      OUTSIDE_PERSON: person,
      OUTSIDE_PEER_SESSION: "example-peer",
      OUTSIDE_DEVICE_LOGIN_HOME: loginHome,
      OUTSIDE_LEGION_CHECKOUT: repo.dir,
      OUTSIDE_CODEX_LIST_SECONDS: "3",
      ...(envoyVersion === null ? {} : { OUTSIDE_CODEX_ENVOY_VERSION: envoyVersion }),
      OUTSIDE_CODEX_ENVOY_COMMIT: repo.built,
      OUTSIDE_DEVICE_LOGIN_COMMIT: scenario.loginApart === true ? repo.apart : repo.login,
      ...(scenario.fromCheckout === true ? { OUTSIDE_CODEX_ENVOY_SOURCE: "checkout" } : {}),
    },
    from: import.meta.url,
  });
  return { code, stdout, stderr, state, inside, listener, loginHome };
}

/** The leg's run of `run`, as the stand-in codex recorded it. */
function execOf(
  run: CodexRun,
  name: string
): { argv: string[]; env: Record<string, string | null> } {
  return JSON.parse(readFileSync(join(run.state, `exec-${name}.json`), "utf8"));
}

/** A copy of the Codex leg that defaults to PASS: every `UNKNOWN` reads `PASS` and every `exit 2`
 *  reads `exit 0`, in the leg and in what it sources. */
function passingCopy(): string {
  return copyLeg(scratch, codexLeg, (text) =>
    text.replaceAll("UNKNOWN", "PASS").replaceAll("exit 2", "exit 0")
  );
}

/** What is wrong with `run` as the leg's answer when its targets are absent: each problem in
 *  words, none when every C row reads UNKNOWN in order, names what it could not read, and the run
 *  exits 2. */
function unknownProblems(run: LegRun): string[] {
  const problems: string[] = [];
  if (run.code !== 2) problems.push(`exit ${run.code}, want 2`);
  const lines = run.stdout.split("\n").filter((line) => line !== "");
  if (lines[0] !== "row\twant\tgot\tverdict") problems.push("the first line is not the header");
  const body = lines.slice(1);
  codexRows.forEach((name, index) => {
    const fields = (body[index] ?? "").split("\t");
    if (fields[0] !== name) problems.push(`line ${index + 2} is not row ${name}`);
    else if ((fields[2] ?? "").trim() === "") problems.push(`row ${name} names nothing`);
    else if (fields[3] !== "UNKNOWN") problems.push(`row ${name} reads ${fields[3]}, want UNKNOWN`);
  });
  if (body.length !== codexRows.length)
    problems.push(`${body.length} rows, want ${codexRows.length}`);
  return problems;
}

describe.skipIf(skipWithout("bash", "bun"))("the Codex leg, against stand-ins", () => {
  test(
    "C0-C7 pass where the release's provenance holds, and the session registers, reads, writes, receives and sends as the outside person, its two controls failing as they should",
    async () => {
      const run = await codexStandInRun({});
      expect({
        verdicts: verdictsOf(run.stdout),
        stdout: run.stdout,
        stderr: run.stderr,
      }).toMatchObject({
        verdicts: Object.fromEntries(codexRows.map((name) => [name, "PASS"])),
      });
      expect(run.code).toBe(0);
      const rows = rowsOf(run.stdout);
      // What ran, in every row: the one pinned Codex release, the model name, and the plugin
      // release from npm with its integrity and the root Codex installed it at.
      for (const name of codexRows) {
        expect(rows.get(name)?.got).toContain(
          `codex-cli 0.171.0 (@openai/codex@0.171.0), -m gpt-5.5 on the stand-in model, @sjawhar/codex-envoy@${release} from npm (integrity sha512-standin), installed at `
        );
        expect(rows.get(name)?.got).toContain(`/plugins/cache/outside-e2e/codex-envoy/${release}`);
      }
      expect(readFileSync(join(run.state, "npm-args"), "utf8")).toContain(
        " @openai/codex@0.171.0\n"
      );
      // The marketplace names the exact release on npm, as the outside person's install text does.
      expect(JSON.parse(readFileSync(join(run.state, "marketplace.json"), "utf8"))).toMatchObject({
        name: "outside-e2e",
        plugins: [
          {
            name: "codex-envoy",
            source: { package: "@sjawhar/codex-envoy", source: "npm", version: release },
          },
        ],
      });
      expect(readFileSync(join(run.state, "plugin-args"), "utf8")).toContain(
        "plugin add codex-envoy@outside-e2e"
      );
      expect(rows.get("C0")?.got).toContain(
        "provenance: https://github.com/cybermonklabs/legion .github/workflows/release.yaml"
      );
      expect(rows.get("C0")?.got).toContain("npm audit signatures: the attestation is verified");
      expect(rows.get("C0")?.got).toContain("holds the device login");
      expect(rows.get("C2")?.got).toContain(`at ${run.inside} it exited 2`);
      expect(rows.get("C4")?.got).toContain("example-peer-foreign answered 404");
      expect(rows.get("C5")?.got).toContain("read it from codex-thread-main");
      expect(rows.get("C6")?.got).toContain("ENVOY_URL at http://127.0.0.1:1, which refused");
      // C7 ran the installed copy's entry as a terminal would: the host codex, a session of its
      // own, P2's HOME and the device login.
      const terminal = JSON.parse(readFileSync(join(run.state, "terminal.json"), "utf8"));
      expect(terminal).toMatchObject({
        argv: ["whoami"],
        env: {
          DISPATCH_HOST: "codex",
          HOME: run.loginHome,
          AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
        },
      });
      expect(terminal.env.CODEX_THREAD_ID).toMatch(/^outside-e2e-codex-terminal-/);
      // Each run under the outside person's settings and P2's HOME, with -m gpt-5.5; C6 with the
      // listener at a port that refuses, C2's control with Dispatch at the inside origin; no token.
      const main = execOf(run, "main");
      expect(main.argv).toEqual(
        expect.arrayContaining(["exec", "--dangerously-bypass-hook-trust", "-m", "gpt-5.5"])
      );
      expect(main.env).toMatchObject({
        ENVOY_URL: run.listener,
        HOME: run.loginHome,
        AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
        AGENT_SECRETS_PROJECTS: "EXAMPLE",
        DISPATCH_TOKEN: null,
      });
      expect(execOf(run, "edge-refused").env.ENVOY_URL).toBe("http://127.0.0.1:1");
      expect(execOf(run, "inside").env.DISPATCH_URL).toBe(run.inside);
    },
    // The run, a bounded child under the short bound.
    rowLimit("short")
  );

  /** Row `name` of `run`, beside the run's stdout for the failure message. */
  const rowOf = (run: CodexRun, name: string) => ({
    name,
    row: rowsOf(run.stdout).get(name),
    stdout: run.stdout,
  });

  test(
    "C2 reads UNKNOWN where its control did not fail, and C4 fails where a message to a session of another project reached the session",
    async () => {
      const [insideSame, noInside, crossed] = await Promise.all([
        codexStandInRun({ whoami: { inside: { exit: 0 } } }),
        codexStandInRun({ noInside: true }),
        codexStandInRun({ controlDelivered: true }),
      ]);
      // C2's control: at the inside origin the tool answers as at the edge, or it never ran.
      expect(rowOf(insideSame, "C2")).toMatchObject({ row: { verdict: "UNKNOWN" } });
      expect(rowOf(insideSame, "C2").row?.got).toContain(
        "does not tell the edge from the inside origin"
      );
      expect(rowOf(noInside, "C2")).toMatchObject({ row: { verdict: "UNKNOWN" } });
      expect(rowOf(noInside, "C2").row?.got).toContain("OUTSIDE_INSIDE_DISPATCH_URL is unset");
      expect(noInside.code).toBe(2);
      // C4's control: a message sent to a session of another project reached this one.
      expect(rowOf(crossed, "C4")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(crossed, "C4").row?.got).toContain("a session of another project");
      expect(crossed.code).toBe(1);
    },
    // The three runs at once, each a bounded child under the short bound.
    rowLimit({ atOnce: ["short", "short", "short"] })
  );

  test(
    "C4 fails where the session never holds the probe's message, C1 where the edge never lists the session, and C2 where whoami names another person",
    async () => {
      const [dropped, unregistered, wrongOwner] = await Promise.all([
        codexStandInRun({ inboundDropped: true }),
        codexStandInRun({ registers: false }),
        codexStandInRun({
          whoami: { main: { exit: 0, owner: "someone@example.com" }, "edge-refused": { exit: 2 } },
        }),
      ]);
      // C4: the edge took the send and the session never held it; then C5 has nothing to answer.
      expect(rowOf(dropped, "C4")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(dropped, "C5")).toMatchObject({ row: { verdict: "UNKNOWN" } });
      // C1: a session the edge never lists, while Codex runs; the run is stopped, not waited out.
      expect(rowOf(unregistered, "C1")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(unregistered, "C1").row?.got).toContain("while codex exec ran");
      // C2: whoami names another person; C6, whose whoami then compares with nothing, is unread.
      expect(rowOf(wrongOwner, "C2")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(wrongOwner, "C2").row?.got).toContain("owner someone@example.com");
      expect(rowOf(wrongOwner, "C6")).toMatchObject({ row: { verdict: "UNKNOWN" } });
      for (const run of [dropped, unregistered, wrongOwner]) expect(run.code).toBe(1);
    },
    // The three runs at once, each a bounded child under the short bound.
    rowLimit({ atOnce: ["short", "short", "short"] })
  );

  test(
    "C3 fails where the comment is not on the issue, and C5 where envoy_send errs or its message never reaches the probe",
    async () => {
      const [lostComment, sendFails, outboundDropped] = await Promise.all([
        codexStandInRun({ commentLost: true }),
        codexStandInRun({ sendFails: true }),
        codexStandInRun({ outboundDropped: true }),
      ]);
      expect(rowOf(lostComment, "C3")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(lostComment, "C3").row?.got).toContain("no comment of EXAMPLE-7 holds");
      expect(rowOf(sendFails, "C5")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(sendFails, "C5").row?.got).toContain("envoy_send answered an error");
      expect(rowOf(outboundDropped, "C5")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(outboundDropped, "C5").row?.got).toContain("got no message");
      for (const run of [lostComment, sendFails, outboundDropped]) expect(run.code).toBe(1);
    },
    // The three runs at once, each a bounded child under the short bound.
    rowLimit({ atOnce: ["short", "short", "short"] })
  );

  test(
    "C6 fails where the whoami answers with the listener reachable and not with it refused, and C3 where the comment carried a scope",
    async () => {
      const [refused, scoped] = await Promise.all([
        codexStandInRun({ whoami: { "edge-refused": { exit: 2 } } }),
        codexStandInRun({ commentScopes: 2 }),
      ]);
      expect({ row: rowsOf(refused.stdout).get("C6"), stdout: refused.stdout }).toMatchObject({
        row: { verdict: "FAIL" },
      });
      expect(rowsOf(refused.stdout).get("C6")?.got).toContain("exited 2");
      expect({ row: rowsOf(scoped.stdout).get("C3"), stdout: scoped.stdout }).toMatchObject({
        row: { verdict: "FAIL" },
      });
      expect(rowsOf(scoped.stdout).get("C3")?.got).toContain("2 scopes");
    },
    // The two runs at once, each a bounded child under the short bound.
    rowLimit({ atOnce: ["short", "short"] })
  );

  test(
    "C0 fails on a release with no provenance (as 5.0.0, published by hand), one whose provenance names another commit, and one whose commit lacks the device login",
    async () => {
      const [byHand, otherCommit, noLogin] = await Promise.all([
        codexStandInRun({ provenance: { provenance: false }, audit: "none" }),
        codexStandInRun({ provenance: { commit: "0".repeat(40) } }),
        codexStandInRun({ loginApart: true }),
      ]);
      expect(rowOf(byHand, "C0")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(byHand, "C0").row?.got).toContain("has no provenance attestation");
      expect(rowOf(byHand, "C0").row?.got).toContain(
        "npm audit signatures verified no attestation"
      );
      expect(rowOf(otherCommit, "C0")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(otherCommit, "C0").row?.got).toContain(
        `the provenance names commit ${"0".repeat(40)}, not OUTSIDE_CODEX_ENVOY_COMMIT`
      );
      expect(rowOf(noLogin, "C0")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(noLogin, "C0").row?.got).toContain("does not hold the device login");
      for (const run of [byHand, otherCommit, noLogin]) {
        // The build comparison is recorded beside the verdict and never decides it.
        expect(rowOf(run, "C0").row?.got).toContain("recorded: ");
        expect(run.code).toBe(1);
      }
    },
    // The three runs at once, each a bounded child under the short bound.
    rowLimit({ atOnce: ["short", "short", "short"] })
  );

  test(
    "a development run reads C0 UNKNOWN; C7 fails where the installed release has no entry or its entry refuses the device login, and C2 where the tool refuses it",
    async () => {
      const [development, noEntry, refusing] = await Promise.all([
        codexStandInRun({ fromCheckout: true }),
        codexStandInRun({ noEntry: true }),
        codexStandInRun({
          terminal: "config",
          whoami: {
            main: {
              exit: 2,
              detail:
                "dispatch: dispatch config: AGENT_SECRETS_MACHINE_IDENTITY names no identity this machine can prove; it takes kubernetes, metadata or pool [exit 2]",
            },
          },
        }),
      ]);
      expect(rowOf(development, "C0")).toMatchObject({ row: { verdict: "UNKNOWN" } });
      expect(rowOf(development, "C0").row?.got).toContain("a development run");
      expect(rowOf(development, "C1").row?.got).toContain("OUTSIDE_CODEX_ENVOY_SOURCE=checkout");
      expect(development.code).toBe(2);
      expect(rowOf(noEntry, "C7")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(noEntry, "C7").row?.got).toContain("has no dist/dispatch.js");
      expect(rowOf(refusing, "C7")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(refusing, "C7").row?.got).toContain("refused its configuration (exit 2)");
      expect(rowOf(refusing, "C2")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(refusing, "C2").row?.got).toContain("names no identity this machine can prove");
      for (const run of [noEntry, refusing]) expect(run.code).toBe(1);
    },
    // The three runs at once, each a bounded child under the short bound.
    rowLimit({ atOnce: ["short", "short", "short"] })
  );

  test(
    "nothing runs against a model that does not answer the leg's nonce, without an exact release, or on a Codex that is not the pinned release: every row reads UNKNOWN",
    async () => {
      const [squat, unset, range, otherCodex] = await Promise.all([
        codexStandInRun({ modelSquat: true }),
        codexStandInRun({ envoyVersion: null }),
        codexStandInRun({ envoyVersion: ">=5.0.0" }),
        codexStandInRun({ version: "0.170.0" }),
      ]);
      for (const run of [squat, unset, range, otherCodex]) {
        expect({ problems: unknownProblems(run), stdout: run.stdout }).toEqual({
          problems: [],
          stdout: run.stdout,
        });
      }
      expect(squat.stdout).toContain("did not answer this run's nonce");
      // The squat's port was never handed to Codex: no codex exec ran.
      expect(Bun.file(join(squat.state, "exec-main.json")).size).toBe(0);
      expect(unset.stdout).toContain("OUTSIDE_CODEX_ENVOY_VERSION is unset");
      expect(range.stdout).toContain("OUTSIDE_CODEX_ENVOY_VERSION is no exact version: >=5.0.0");
      expect(otherCodex.stdout).toContain("not codex-cli 0.171.0");
    },
    // The four runs at once, each a bounded child under the short bound.
    rowLimit({ atOnce: ["short", "short", "short", "short"] })
  );

  test(
    "with no input every row reads UNKNOWN and the leg exits 2, and the check fails on a copy that defaults to PASS",
    async () => {
      const env = {
        PATH: process.env.PATH ?? "",
        HOME: scratch("home"),
        ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
      };
      const run = async (leg: string) => {
        const ran = await runBounded({
          argv: ["bash", leg],
          bound: "short",
          cwd: scratch("cwd"),
          env,
          from: import.meta.url,
        });
        return { code: ran.code, stdout: ran.stdout, stderr: ran.stderr };
      };
      const plain = await run(copyLeg(scratch, codexLeg));
      expect(unknownProblems(plain)).toEqual([]);
      for (const name of ["OUTSIDE_DISPATCH_URL", "OUTSIDE_DEVICE_LOGIN_HOME"]) {
        expect(plain.stdout).toContain(name);
      }
      const passing = await run(passingCopy());
      const problems = unknownProblems(passing);
      expect(problems).toContain("exit 0, want 2");
      expect(problems.some((problem) => problem.endsWith("reads PASS, want UNKNOWN"))).toBe(true);
    },
    // The two runs, one after another, each a bounded child under the short bound.
    rowLimit("short", "short")
  );
});

describe("the Codex leg's provenance reads", () => {
  /** An attestations answer as the registry serves it: a publish attestation and a SLSA
   *  provenance whose payload names `workflow`, `commit` and the tarball's sha512 `subject`. */
  const attestations = (workflow: Record<string, string>, commit: string, subject: string) => ({
    attestations: [
      { predicateType: "https://github.com/npm/attestation/tree/main/specs/publish/v0.1" },
      {
        predicateType: "https://slsa.dev/provenance/v1",
        bundle: {
          dsseEnvelope: {
            payload: Buffer.from(
              JSON.stringify({
                subject: [
                  { name: "pkg:npm/%40sjawhar/codex-envoy@5.0.1", digest: { sha512: subject } },
                ],
                predicate: {
                  buildDefinition: {
                    externalParameters: { workflow },
                    resolvedDependencies: [
                      {
                        uri: "git+https://github.com/cybermonklabs/legion@refs/heads/main",
                        digest: { gitCommit: commit },
                      },
                    ],
                  },
                },
              })
            ).toString("base64"),
          },
        },
      },
    ],
  });
  const tarball = Buffer.from("the tarball's bytes");
  const sha512 = new Bun.CryptoHasher("sha512").update(tarball);
  const hex = sha512.digest("hex");
  const integrity = `sha512-${Buffer.from(hex, "hex").toString("base64")}`;
  const release = {
    repository: "https://github.com/cybermonklabs/legion",
    path: ".github/workflows/release.yaml",
    ref: "refs/heads/main",
  };

  test("it reads the workflow, the source commit and whether the subject is the registry's tarball", () => {
    expect(provenanceOf(attestations(release, "a".repeat(40), hex), integrity)).toEqual({
      provenance: true,
      repository: "https://github.com/cybermonklabs/legion",
      workflow: ".github/workflows/release.yaml",
      ref: "refs/heads/main",
      commit: "a".repeat(40),
      subject_matches: true,
    });
    // Another tarball's digest, and an answer with no SLSA provenance, as a hand publish has.
    expect(
      provenanceOf(attestations(release, "a".repeat(40), "0".repeat(128)), integrity)
    ).toMatchObject({
      subject_matches: false,
    });
    expect(provenanceOf({ attestations: [] }, integrity)).toEqual({ provenance: false });
  });

  test("it compares two builds' bundles by name, naming those that differ or are on one side", () => {
    const built = scratch("built");
    const installed = scratch("installed");
    for (const [dir, files] of [
      [built, { "a.js": "same", "b.js": "one", "c.js": "only here" }],
      [installed, { "a.js": "same", "b.js": "two", "d.js": "only there" }],
    ] as const) {
      mkdirSync(join(dir, "dist"));
      for (const [name, text] of Object.entries(files))
        writeFileSync(join(dir, "dist", name), text);
      // A file that is no bundle, different on each side, which the comparison leaves out.
      writeFileSync(join(dir, "dist", "THIRD_PARTY_NOTICES"), dir);
    }
    expect(compareBundles(built, installed)).toEqual({
      equal: 1,
      differ: "b.js",
      only_built: "c.js",
      only_installed: "d.js",
    });
  });
});

describe("the Codex leg's stand-in model, on the requests Codex sends it", () => {
  const plan: Plan = {
    nonce: "n1",
    issue: "EXAMPLE-7",
    comment: "outside-e2e codex-comment n1",
    inbound: "outside-e2e codex-in n1",
    control: "outside-e2e codex-control n1",
    outbound: "outside-e2e codex-out n1",
    outboundTo: "outside-e2e-codex-n1",
    waitCalls: 2,
    waitSeconds: 2,
  };
  const prompt = (run: string) => ({
    content: [{ text: `outside-e2e codex-run ${run} n1`, type: "input_text" }],
    role: "user",
    type: "message",
  });
  /** The tools as Codex 0.171.0 lists them under `-m gpt-5.5`: the plugin's behind tool_search. */
  const deferred = [{ name: "exec_command", type: "function" }, { type: "tool_search" }];
  const found = {
    call_id: "outside-search-1",
    tools: [
      {
        name: "mcp__envoy",
        tools: [
          { name: "envoy_send", type: "function" },
          { name: "dispatch", type: "function" },
        ],
        type: "namespace",
      },
    ],
    type: "tool_search_output",
  };
  const output = (call: Record<string, unknown>, text: string) => ({
    call_id: call.call_id,
    output: [{ text, type: "input_text" }],
    type: "function_call_output",
  });

  test("on 0.171.0 with gpt-5.5 it searches for the tools, then calls them in the order the rows read", () => {
    const logged = new Set<string>();
    let input: Record<string, unknown>[] = [prompt("main")];
    const step = (n: number, now = 1000) =>
      nextTurn(plan, { input, tools: deferred }, n, logged, now);
    const search = step(1);
    expect(search.items).toMatchObject([{ execution: "client", type: "tool_search_call" }]);
    input = [...input, ...(search.items as Record<string, unknown>[]), found];
    const whoami = step(2);
    expect(whoami.items).toMatchObject([
      { name: "dispatch", namespace: "mcp__envoy", type: "function_call" },
    ]);
    expect(JSON.parse(String(whoami.items[0]?.arguments))).toEqual({ args: ["whoami"] });
    expect(whoami.log).toEqual([
      { step: "tools", dispatch: "mcp__envoy.dispatch", send: "mcp__envoy.envoy_send" },
    ]);
    const said = `Session th-1 acts for ${person}: issues you create without an assignee are assigned to ${person}. It may write project EXAMPLE and no other.\n[exit 0]`;
    input = [
      ...input,
      ...(whoami.items as Record<string, unknown>[]),
      output(whoami.items[0] as Record<string, unknown>, said),
    ];
    const comment = step(3);
    expect(JSON.parse(String(comment.items[0]?.arguments))).toEqual({
      args: ["comment", "--issue", "EXAMPLE-7", "--body-file", "-"],
      stdin: "outside-e2e codex-comment n1",
    });
    expect(comment.log).toEqual([
      {
        step: "whoami",
        exit: 0,
        session: "th-1",
        owner: person,
        project: "EXAMPLE",
        elsewhere: "",
      },
    ]);
    input = [
      ...input,
      ...(comment.items as Record<string, unknown>[]),
      output(
        comment.items[0] as Record<string, unknown>,
        "Commented 3f1c2a9e-8b7d-4c11-9e2a-5d6f7a8b9c0d\n[exit 0]"
      ),
    ];
    const wait = step(4);
    expect(wait.items).toMatchObject([{ name: "exec_command", type: "function_call" }]);
    expect(JSON.parse(String(wait.items[0]?.arguments))).toEqual({ cmd: "sleep 2" });
    expect(wait.log).toEqual([
      { step: "comment", exit: 0, comment_id: "3f1c2a9e-8b7d-4c11-9e2a-5d6f7a8b9c0d" },
    ]);
    // The hook's text, as Codex adds it after the tool's output.
    input = [
      ...input,
      ...(wait.items as Record<string, unknown>[]),
      output(wait.items[0] as Record<string, unknown>, "Process exited with code 0"),
      {
        content: [{ text: "Envoy: outside-e2e codex-in n1", type: "input_text" }],
        role: "developer",
        type: "message",
      },
    ];
    const send = step(5, 5000);
    expect(send.items).toMatchObject([{ name: "envoy_send", namespace: "mcp__envoy" }]);
    expect(JSON.parse(String(send.items[0]?.arguments))).toEqual({
      message: "outside-e2e codex-out n1",
      session_id: "outside-e2e-codex-n1",
    });
    expect(send.log).toEqual([
      { step: "inbound", at: 5000, waits: 1 },
      { step: "send", at: 5000 },
    ]);
    input = [
      ...input,
      ...(send.items as Record<string, unknown>[]),
      output(send.items[0] as Record<string, unknown>, "Sent to outside-e2e-codex-n1: delivered"),
    ];
    const end = step(6);
    expect(end.items).toMatchObject([{ role: "assistant", type: "message" }]);
    expect(end.log).toEqual([
      { step: "sent", ok: true, detail: "Sent to outside-e2e-codex-n1: delivered" },
      { step: "end" },
    ]);
    // A request Codex sends again adds no line to the log.
    expect(step(7).log).toEqual([]);
  });

  test("where Codex lists the tools in the request, it calls them at once, and a run but main ends after whoami", () => {
    const listed = [
      {
        name: "mcp__envoy",
        tools: [{ name: "dispatch" }, { name: "envoy_send" }],
        type: "namespace",
      },
    ];
    const logged = new Set<string>();
    const first = nextTurn(plan, { input: [prompt("edge-refused")], tools: listed }, 1, logged, 1);
    expect(first.items).toMatchObject([{ name: "dispatch", namespace: "mcp__envoy" }]);
    const answered = [
      prompt("edge-refused"),
      ...(first.items as Record<string, unknown>[]),
      output(
        first.items[0] as Record<string, unknown>,
        "dispatch: dispatch config: no answer\n[exit 2]"
      ),
    ];
    const last = nextTurn(plan, { input: answered, tools: listed }, 2, logged, 2);
    expect(last.items).toMatchObject([{ type: "message" }]);
    expect(last.log).toEqual([
      {
        step: "whoami",
        exit: 2,
        session: "",
        owner: "",
        project: "",
        elsewhere: "",
        detail: "dispatch: dispatch config: no answer [exit 2]",
      },
      { step: "end" },
    ]);
    expect(findTool([{ name: "mcp__envoy__dispatch", type: "function" }], "dispatch")).toEqual({
      name: "mcp__envoy__dispatch",
    });
  });

  test("it logs a control message that reached the session, and the end of the sleeps when no message came", () => {
    const listed = [
      {
        name: "mcp__envoy",
        tools: [{ name: "dispatch" }, { name: "envoy_send" }],
        type: "namespace",
      },
    ];
    const logged = new Set<string>();
    let input: Record<string, unknown>[] = [prompt("main")];
    const plain = { ...plan, issue: "" };
    for (let n = 1; n <= 4; n += 1) {
      const turn = nextTurn(plain, { input, tools: listed }, n, logged, n);
      const call = turn.items[0] as Record<string, unknown>;
      if (call.type !== "function_call") {
        expect(turn.log).toEqual([{ step: "wait-ended", waits: 2 }, { step: "end" }]);
        break;
      }
      const said =
        call.name === "dispatch"
          ? "[exit 0]"
          : "Process exited with code 0 outside-e2e codex-control n1";
      input = [...input, call, output(call, said)];
    }
    expect([...logged]).toContain("main:control");
    // The whoami text that names another session than the host's.
    expect(
      whoamiOf(
        `Session s-2 acts for ${person}: issues. Your credential names session s-9, which is not this host session (s-2): Dispatch records every write you make as s-9. It may write project EXAMPLE and no other.\n[exit 0]`
      )
    ).toMatchObject({ session: "s-2", elsewhere: "s-9" });
  });
});
