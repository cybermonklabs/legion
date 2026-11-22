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
import { mkdirSync, readFileSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { findTool, nextTurn, type Plan, whoamiOf } from "../acceptance/outside-agent-codex-model";
import {
  certificateProblem,
  compareTrees,
  fulcioExtensions,
  provenanceOf,
} from "../acceptance/outside-agent-codex-provenance";
import { commentAuthor } from "../acceptance/outside-agent-probe";
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
/** How long the leg may run on after its last row, in ms. Its cleanup stops its background
 *  children together and any Codex pane still open, each in 10 s at most, and deletes its scratch
 *  directory; a run of the leg whose cleanup took minutes (the deletion of a tree of 80 879
 *  entries) went unseen before this row. */
const exitAfterRows = 60_000;
/** The screens a run that passes answers, as each row's evidence names them: the folder trust of a
 *  first start in a git repository, the two first-run screens, and the envoy_send approval. */
const answeredScreens =
  'screens answered: main: Folder access -> Trust and continue; main: Hooks need review -> Trust all and continue; main: Meet GPT-6 Sol -> Use existing model; main: Allow the envoy MCP server to run tool "envoy_send"? -> Always allow';

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
  /** The comment the session writes is not on the issue (C3). */
  readonly commentLost?: boolean;
  /** The comment's author, where it differs from what Dispatch records for the Codex session's
   *  write with the outside person's token: a field set to null is absent. */
  readonly commentAuthor?: Record<string, string | boolean | null>;
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
  /** OUTSIDE_CODEX_FIRST_START: a control run's answers to the first-run screens. */
  readonly firstStart?: "no-trust" | "new-model";
  /** The interactive start shows a screen the leg does not expect (a folder trust question). */
  readonly unexpectedScreen?: boolean;
  /** The tree Codex installs differs from the release's tarball in one file, or the registry's
   *  tarball is not the bytes its integrity names (C0). */
  readonly installedDiffers?: boolean;
  readonly tarballMismatch?: boolean;
  /** The integrity npm records for @openai/codex is not the pinned one. */
  readonly codexIntegrity?: "other";
  /** OUTSIDE_CODEX_START_SECONDS (default 30): how long each start may hold before its turn. */
  readonly startSeconds?: number;
  /** The first-run menus draw each move late, 2.5 s after the keys, as a slow machine's Codex can:
   *  a key sent before the last move is drawn is one move more. */
  readonly slowScreens?: boolean;
  /** Each run's model log holds an `end` line right after its tools, and its turn goes on 3 s
   *  later, as the stand-in model logged one for the request Codex 0.171.0 sends 107 ms into an
   *  interactive turn (it offers apply_patch alone) before that request was ignored. */
  readonly endBeforeTurn?: boolean;
}

/** Where the real provenance helper is: the stand-in imports its tree comparison. */
const provenanceHelper = join(
  import.meta.dir,
  "..",
  "acceptance",
  "outside-agent-codex-provenance.ts"
);
/** Where the real probe is: the stand-in probe reads Dispatch's comments with its reader. */
const realProbe = join(import.meta.dir, "..", "acceptance", "outside-agent-probe.ts");

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

/** The stand-in `codex`: a Bun script with this run's state directory written into it. Its
 *  interactive start (`--no-daemon`) runs in the stand-in tmux's pane: it shows the two first-run
 *  screens Codex 0.171.0 shows, each a menu whose chosen entry carries `›`, takes Down and Enter
 *  from the pane's keys, and keeps each answer as Codex does in config.toml, so a later start shows
 *  neither. Untrusted hooks run none (no session record, no registration, no delivery into the
 *  turn); the new model lists no plugin tool. It answers no `exec`. */
function standInCodex(state: string): string {
  return `#!/usr/bin/env bun
import { appendFileSync, cpSync, existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
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
// \`plugin add\` installs the release (the tree the release's tarball holds) under the plugin
// cache, at the version an npm source names (a local source as 1.0.0), and names that root.
const config = join(home, "config.toml");
if (argv[0] === "plugin") {
  appendFileSync(join(state, "plugin-args"), argv.join(" ") + "\\n");
  if (argv[1] === "marketplace") {
    cpSync(join(argv[3], ".agents", "plugins", "marketplace.json"), join(state, "marketplace.json"));
    appendFileSync(config, '\\n[marketplaces.outside-e2e]\\nsource_type = "local"\\n');
  }
  if (argv[1] === "add") {
    const source = JSON.parse(readFileSync(join(state, "marketplace.json"), "utf8")).plugins[0].source;
    const version = source.source === "npm" ? source.version : "1.0.0";
    const root = join(home, "plugins", "cache", "outside-e2e", "codex-envoy", version);
    cpSync(join(state, "release", "package"), root, { recursive: true });
    writeFileSync(join(root, "package.json"), JSON.stringify({ version }));
    if (scenario.installedDiffers) appendFileSync(join(root, "dist", "codex-hook.js"), "// changed\\n");
    appendFileSync(config, '\\n[plugins."codex-envoy@outside-e2e"]\\nenabled = true\\n');
    console.log("Added plugin \`codex-envoy\` from marketplace \`outside-e2e\`. Installed plugin root: " + root);
  }
  process.exit(0);
}
if (argv[0] !== "--no-daemon") process.exit(2);
const run = /codex-run (\\S+) /.exec(argv.at(-1) ?? "")?.[1] ?? "";
const session = "codex-thread-" + run;
const seen = ["ENVOY_URL", "DISPATCH_URL", "HOME", "AGENT_SECRETS_MACHINE_IDENTITY", "AGENT_SECRETS_PROJECTS", "DISPATCH_TOKEN"];
writeFileSync(join(state, "exec-" + run + ".json"), JSON.stringify({
  argv,
  cwd: process.cwd(),
  git: existsSync(join(process.cwd(), ".git")),
  config: readFileSync(config, "utf8"),
  env: Object.fromEntries(seen.map((name) => [name, process.env[name] ?? null])),
}));
// The pane: what the stand-in tmux shows of it, and the keys it was sent.
const pane = process.env.STANDIN_PANE ?? "";
const show = (lines) => {
  writeFileSync(join(pane, "screen.writing"), lines.join("\\n") + "\\n");
  renameSync(join(pane, "screen.writing"), join(pane, "screen"));
};
let consumed = 0;
const keys = () => {
  const all = readFileSync(join(pane, "keys"), "utf8").split("\\n").filter((key) => key !== "");
  const fresh = all.slice(consumed);
  consumed = all.length;
  return fresh;
};
// A slow screen reads the keys sent and draws the move only every 2.5 s.
const pause = scenario.slowScreens ? 2500 : 25;
const menu = async (title, options) => {
  let chosen = 0;
  for (;;) {
    show([title, "", ...options.map((option, index) => (index === chosen ? "› " : "  ") + (index + 1) + ". " + option), "", "enter to confirm"]);
    for (const key of keys()) {
      if (key === "Down") chosen = Math.min(chosen + 1, options.length - 1);
      if (key === "Up") chosen = Math.max(chosen - 1, 0);
      if (key === "Enter") return options[chosen];
    }
    await Bun.sleep(pause);
  }
};
const forever = () => new Promise(() => {});
if (scenario.unexpectedScreen) {
  show(["Do you trust the files in this folder?", "", "› 1. Yes, continue", "  2. No, quit"]);
  await forever();
}
// A first start in a git repository asks whether to trust the folder, unless config.toml already
// trusts it, and keeps the answer there.
const cwd = process.cwd();
if (existsSync(join(cwd, ".git")) && !readFileSync(config, "utf8").includes('[projects."' + cwd + '"]')) {
  const answer = await menu("  Folder access\\n  " + cwd + "\\n  Trust this folder? Codex can read, edit, and run files here, subject to your permission settings.", ["Trust and continue", "Back to Agent Command Center"]);
  if (answer !== "Trust and continue") {
    show(["Agent Command Center"]);
    await forever();
  }
  appendFileSync(config, '\\n[projects."' + cwd + '"]\\ntrust_level = "trusted"\\n');
}
let hooksRun = true;
let newModel = false;
if (!readFileSync(config, "utf8").includes("[hooks.state")) {
  const answer = await menu("Hooks need review", ["Review hooks", "Trust all and continue", "Continue without trusting (hooks won't run)"]);
  if (answer === "Review hooks") {
    show(["Review hooks", "", "hooks/hooks.json"]);
    await forever();
  }
  hooksRun = answer === "Trust all and continue";
  if (hooksRun) appendFileSync(config, '\\n[hooks.state."codex-envoy@outside-e2e:hooks/hooks.json"]\\ntrusted_hash = "standin"\\n');
}
if (!readFileSync(config, "utf8").includes("[notice.model_migrations]")) {
  newModel = (await menu("Meet GPT-6 Sol", ["Try new model", "Use existing model"])) === "Try new model";
  appendFileSync(config, '\\n[notice.model_migrations]\\n"gpt-5.5" = "gpt-6-sol"\\n');
}
show(["› " + argv.at(-1), "", "• Working"]);
const [planFile, logFile] = readFileSync(join(state, "model-argv"), "utf8").trim().split("\\n");
const plan = JSON.parse(readFileSync(planFile, "utf8"));
const log = (line) => appendFileSync(logFile, JSON.stringify({ run, ...line }) + "\\n");
const configured = existsSync(config) ? readFileSync(config, "utf8") : "";
const plugin = configured.includes('[plugins."codex-envoy@outside-e2e"]') && configured.includes("model_provider = ");
// The SessionStart hook records the session at the first prompt, whatever model then runs.
if (plugin && hooksRun) {
  const records = join(home, "plugins", "data", "codex-envoy-outside-e2e", "codex-sessions");
  mkdirSync(records, { recursive: true });
  writeFileSync(join(records, process.pid + ".json"), JSON.stringify({ session_id: session }));
  if (scenario.registers !== false) appendFileSync(join(state, "sessions"), session + "\\n");
}
if (newModel || !plugin) {
  log({ step: "tools", dispatch: "", send: "" });
  log({ step: "end" });
  await forever();
}
const until = async (file) => { while (!existsSync(join(state, file))) await Bun.sleep(25); };
log({ step: "tools", dispatch: "mcp__envoy.dispatch", send: "mcp__envoy.envoy_send" });
if (scenario.endBeforeTurn) {
  log({ step: "end" });
  await Bun.sleep(3000);
}
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
  if (scenario.registers === false) await forever();
  if (plan.issue !== "") {
    log({ step: "comment", exit: 0, comment_id: "3f1c2a9e-8b7d-4c11-9e2a-5d6f7a8b9c0d" });
    if (!scenario.commentLost) writeFileSync(join(state, "comment"), plan.comment);
  }
  // With the hooks untrusted no record names the session, so nothing is sent to it, and its turn
  // sleeps until the leg stops it.
  if (!hooksRun) await forever();
  await until("send-attempted");
  if (scenario.endBeforeTurn) await Bun.sleep(3000);
  // The PostToolUse hook puts what reached the session into the turn: only a trusted hook runs.
  if (existsSync(join(state, "control"))) log({ step: "control", at: Date.now() });
  if (existsSync(join(state, "inbound"))) {
    log({ step: "inbound", at: Date.now(), waits: 1 });
    log({ step: "send", at: Date.now() });
    // Codex asks before the plugin's envoy_send runs, unless the configuration lets every tool run
    // (approval_policy = "never") or keeps an earlier "Always allow".
    const settings = readFileSync(config, "utf8");
    let allowed = true;
    if (!settings.includes('approval_policy = "never"') && !settings.includes("[mcp_servers.envoy.tools.envoy_send]")) {
      const answer = await menu('  Allow the envoy MCP server to run tool "envoy_send"?', ["Allow", "Allow for this session", "Always allow", "Cancel"]);
      if (answer === "Always allow") appendFileSync(config, '\\n[mcp_servers.envoy.tools.envoy_send]\\napproval = "always"\\n');
      allowed = answer !== "Cancel";
      show(["› " + argv.at(-1), "", "• Called envoy.envoy_send"]);
    }
    const fails = scenario.sendFails || !allowed;
    if (!fails && !scenario.outboundDropped) {
      writeFileSync(join(state, "outbound"), JSON.stringify({ text: plan.outbound, from: session, to: plan.outboundTo }));
    }
    log({ step: "sent", ok: !fails, detail: scenario.sendFails ? "tool call error: Envoy is not ready" : allowed ? "delivered to " + plan.outboundTo : "tool call cancelled" });
  } else {
    log({ step: "wait-ended", waits: plan.waitCalls });
  }
  writeFileSync(join(state, "main-ended"), "");
}
log({ step: "end" });
// The interactive session stays open after its turn, until its tmux server is stopped.
await forever();
`;
}

/** The stand-in `tmux`: a pane is a directory beside the socket path, holding the pid of the
 *  command it runs, the screen that command last showed and the keys it was sent. As tmux does, the
 *  server makes its socket as it starts and leaves it when `kill-server` stops it; a stopped
 *  server's pane names no process, so no later command signals a pid the system has reused. */
function standInTmux(): string {
  return `#!/usr/bin/env bash
sock=''
if [ "$1" = -S ]; then sock=$2; shift 2; fi
if [ "$1" = -f ]; then shift 2; fi
pane="$sock.d"
command=$1
shift
case $command in
  new-session)
    cwd=.
    while [ "$#" -gt 0 ]; do
      case $1 in -d) shift ;; -s | -x | -y) shift 2 ;; -c) cwd=$2; shift 2 ;; *) break ;; esac
    done
    mkdir -p "$pane"
    : >"$pane/keys"
    : >"$pane/screen"
    bun -e 'require("node:net").createServer().listen(process.argv[1], () => process.exit(0))' "$sock" </dev/null >/dev/null 2>&1
    (cd "$cwd" && STANDIN_PANE="$pane" exec "$@") </dev/null >"$pane/out" 2>&1 &
    echo "$!" >"$pane/pid"
    ;;
  capture-pane) cat "$pane/screen" 2>/dev/null ;;
  send-keys) shift 2; for key in "$@"; do printf '%s\\n' "$key" >>"$pane/keys"; done ;;
  has-session) kill -0 "$(cat "$pane/pid" 2>/dev/null)" 2>/dev/null ;;
  display-message) cat "$pane/pid" 2>/dev/null ;;
  kill-server) pid=$(cat "$pane/pid" 2>/dev/null); rm -f "$pane/pid"; [ -z "$pid" ] || kill "$pid" 2>/dev/null; true ;;
  *) exit 1 ;;
esac
`;
}

/** The stand-in probe: answers each command the Codex leg runs, from the state directory. Its
 *  `comment-find` serves the issue's comments as Dispatch answers them, the author as Dispatch
 *  records a write made with a broker-issued token named with one project (kind `session`, the
 *  session as id, proven, owner and project, no act: api/broker_token.go), and reads them with the
 *  real probe's own reader. */
function standInProbe(state: string): string {
  return `import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { commentAuthor } from ${JSON.stringify(realProbe)};
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
    const comments = [{ id: "5d0c7a1e-0000-4000-8000-000000000001", body: "an earlier comment", author: { kind: "person", id: ${JSON.stringify(person)}, proven: true } }];
    if (existsSync(join(state, "comment"))) {
      const author = { kind: "session", id: "codex-thread-main", owner: ${JSON.stringify(person)}, project: "EXAMPLE", proven: true, ...(scenario.commentAuthor ?? {}) };
      for (const [name, value] of Object.entries(author)) if (value === null) delete author[name];
      comments.push({ id: "3f1c2a9e-8b7d-4c11-9e2a-5d6f7a8b9c0d", body: readFileSync(join(state, "comment"), "utf8"), author });
    }
    print({ ...ok, ...commentAuthor(comments, args.slice(1).join(" ")) });
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
  /** How long the leg ran on after it wrote its last row (its TSV's last write), in ms. */
  readonly outlived: number;
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
  // The release's tree, which its tarball holds and Codex installs: a package.json, the bundles
  // and, unless the scenario drops it, the terminal entry.
  const releaseTree = join(state, "release", "package");
  mkdirSync(join(releaseTree, "dist"), { recursive: true });
  writeFileSync(join(releaseTree, "package.json"), JSON.stringify({ version: release }));
  writeFileSync(join(releaseTree, "dist", "codex-hook.js"), "// the session-start hook\n");
  if (scenario.noEntry !== true)
    writeFileSync(join(releaseTree, "dist", "dispatch.js"), standInEntry(state));
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
  // The stand-in provenance helper: the registry's record of the release, its tarball (the
  // release's tree, packed), and the provenance the release workflow writes at the release's
  // commit, unless the scenario says otherwise; the tree comparison is the real helper's.
  await Bun.write(
    join(legDir, "outside-agent-codex-provenance.ts"),
    `import { compareTrees } from ${JSON.stringify(provenanceHelper)};
const [command, first, second, third] = Bun.argv.slice(2);
const print = (value) => console.log(JSON.stringify(value));
if (command === "tree") print(compareTrees(first, second));
else {
  // What registry the leg named to this helper: none, whatever the leg was given.
  require("node:fs").writeFileSync(${JSON.stringify(join(state, "helper-registry"))}, process.env.OUTSIDE_NPM_REGISTRY ?? "none");
  Bun.spawnSync(["tar", "-czf", third, "-C", ${JSON.stringify(join(state, "release"))}, "package"]);
  print({ status: 200, integrity: "sha512-standin", tarball_matches: ${scenario.tarballMismatch !== true}, provenance: true, repository: "https://github.com/cybermonklabs/legion", workflow: ".github/workflows/release.yaml", ref: "refs/heads/main", commit: ${JSON.stringify(repo.built)}, subject_matches: true, certificate_matches: true, certificate_problem: "", ...${JSON.stringify(scenario.provenance ?? {})} });
}
`
  );
  const bin = textToolPath(scratch, ["bun", "git", "tar", "gzip"]);
  script(join(bin, "tmux"), standInTmux());
  const codex = script(join(state, "codex"), standInCodex(state));
  // npm: installs the stand-in codex for \`npm install --prefix <dir> @openai/codex@<v>\`, recording
  // the integrity of the two packages as npm does in its hidden lockfile, takes the release for the
  // audit project, and answers \`npm audit signatures\` as the scenario says.
  const lock = {
    packages: {
      "node_modules/@openai/codex": {
        integrity:
          scenario.codexIntegrity === "other"
            ? "sha512-another"
            : "sha512-1IOFzL2Xg86tLdw6kDo3GoKbkQENgQi7TI6rlEvN132R7ngSw7SNOIQoTcnKzSZZ8kf9N9GynCweSoaFbqk2Eg==",
      },
      "node_modules/@openai/codex-linux-x64": {
        integrity:
          "sha512-l66NZkQgdk2C3pbcfULDz42yEUB581MVA6I+Y43Wp4OIqEy73HCdmZ0GTjeAHY2ycuH4N00d9VsLrwT1jxshTw==",
      },
    },
  };
  script(
    join(bin, "npm"),
    `#!/usr/bin/env bun
import { appendFileSync, copyFileSync, mkdirSync, writeFileSync } from "node:fs";
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
writeFileSync(join(prefix, "node_modules", ".package-lock.json"), ${JSON.stringify(JSON.stringify(lock))});
`
  );
  const loginHome = scratch("login-home");
  await Bun.write(join(loginHome, ".config", "legion", "device-login.json"), "{}");
  const listener = `http://127.0.0.1:${await closedPort()}`;
  const inside = `http://127.0.0.1:${await closedPort()}`;
  const envoyVersion = scenario.envoyVersion === undefined ? release : scenario.envoyVersion;
  const rows = join(state, "rows.tsv");
  const { code, stdout, stderr } = await runBounded({
    argv: ["bash", leg],
    bound: "codexRun",
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
      OUTSIDE_CODEX_START_SECONDS: String(scenario.startSeconds ?? 30),
      OUTSIDE_CODEX_TSV: rows,
      // An input the leg no longer takes: the pass row reads that its provenance helper got none.
      OUTSIDE_NPM_REGISTRY: "http://registry.invalid",
      ...(scenario.firstStart === undefined
        ? {}
        : { OUTSIDE_CODEX_FIRST_START: scenario.firstStart }),
      ...(envoyVersion === null ? {} : { OUTSIDE_CODEX_ENVOY_VERSION: envoyVersion }),
      OUTSIDE_CODEX_ENVOY_COMMIT: repo.built,
      OUTSIDE_DEVICE_LOGIN_COMMIT: scenario.loginApart === true ? repo.apart : repo.login,
      ...(scenario.fromCheckout === true ? { OUTSIDE_CODEX_ENVOY_SOURCE: "checkout" } : {}),
    },
    from: import.meta.url,
  });
  // The leg writes each row to its TSV as it prints it, so the file's last write is its last row.
  const outlived = Date.now() - statSync(rows).mtimeMs;
  return { code, stdout, stderr, state, inside, listener, loginHome, outlived };
}

/** The leg's run of `run`, as the stand-in codex recorded it as it started: its arguments, its
 *  folder and whether that is a git repository, config.toml then, and its environment. */
function execOf(
  run: CodexRun,
  name: string
): {
  argv: string[];
  cwd: string;
  git: boolean;
  config: string;
  env: Record<string, string | null>;
} {
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
        "provenance: https://github.com/cybermonklabs/legion .github/workflows/release.yaml at refs/heads/main"
      );
      expect(rows.get("C0")?.got).toContain(
        "the tree Codex installed is the tarball, file by file (3 files)"
      );
      expect(rows.get("C0")?.got).toContain("npm audit signatures: the attestation is verified");
      expect(rows.get("C0")?.got).toContain("holds the device login");
      expect(rows.get("C0")?.got).toContain(
        "its signing certificate names the same repository, workflow, ref and commit"
      );
      // The leg names no registry to its provenance helper, whatever it is given.
      expect(readFileSync(join(run.state, "helper-registry"), "utf8")).toBe("none");
      // The screens answered as the outside person is told to: the folder trust, the two first-run
      // screens and the envoy_send approval, each once; no later start asked again.
      expect(rows.get("C1")?.got).toContain(answeredScreens);
      expect(rows.get("C2")?.got).toContain(
        `at ${run.inside} the tool's dispatch whoami refused (exit 2)`
      );
      // C3 read the comment's author as Dispatch records the Codex session's write.
      expect(rows.get("C3")?.got).toContain(
        `by session codex-thread-main, proven, owner ${person}, project EXAMPLE`
      );
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
      // Each run is the interactive start in a pane, `codex --no-daemon -m gpt-5.5 <prompt>`, under
      // the outside person's settings and P2's HOME; C6 with the listener at a port that refuses,
      // C2's control with Dispatch at the inside origin; no token; never exec, never a bypass.
      const main = execOf(run, "main");
      expect(main.argv).toEqual([
        "--no-daemon",
        "-m",
        "gpt-5.5",
        expect.stringMatching(/^outside-e2e codex-run main /),
      ]);
      expect(main.env).toMatchObject({
        ENVOY_URL: run.listener,
        HOME: run.loginHome,
        AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
        AGENT_SECRETS_PROJECTS: "EXAMPLE",
        DISPATCH_TOKEN: null,
      });
      // Codex ran with the outside person's configuration: the leg set no approval policy,
      // sandbox mode or folder trust of its own, and the folder it started in is a git repository.
      expect(main.config).not.toMatch(/approval_policy|sandbox_mode|trust_level/);
      expect(main.git).toBe(true);
      expect(execOf(run, "edge-refused").env.ENVOY_URL).toBe("http://127.0.0.1:1");
      expect(execOf(run, "inside").env.DISPATCH_URL).toBe(run.inside);
    },
    // The run, a bounded child under the Codex run's bound.
    rowLimit("codexRun")
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
    // The three runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun", "codexRun"] })
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
      expect(rowOf(unregistered, "C1").row?.got).toContain("while Codex ran");
      // C2: whoami names another person; C6, whose whoami then compares with nothing, is unread.
      expect(rowOf(wrongOwner, "C2")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(wrongOwner, "C2").row?.got).toContain("owner someone@example.com");
      expect(rowOf(wrongOwner, "C6")).toMatchObject({ row: { verdict: "UNKNOWN" } });
      for (const run of [dropped, unregistered, wrongOwner]) expect(run.code).toBe(1);
    },
    // The three runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun", "codexRun"] })
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
    // The three runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun", "codexRun"] })
  );

  test(
    "C6 fails where the whoami answers with the listener reachable and not with it refused, and C2 reads UNKNOWN where its control's call ended with no exit of the command",
    async () => {
      const [refused, controlKilled] = await Promise.all([
        codexStandInRun({ whoami: { "edge-refused": { exit: 2 } } }),
        codexStandInRun({
          whoami: { inside: { exit: -1, detail: "tool call error: the call timed out" } },
        }),
      ]);
      expect({ row: rowsOf(refused.stdout).get("C6"), stdout: refused.stdout }).toMatchObject({
        row: { verdict: "FAIL" },
      });
      expect(rowsOf(refused.stdout).get("C6")?.got).toContain("exited 2");
      // A timeout, a cancel or a kill of the control's tool call is no refusal.
      expect(rowOf(controlKilled, "C2")).toMatchObject({ row: { verdict: "UNKNOWN" } });
      expect(rowOf(controlKilled, "C2").row?.got).toContain("which is no refusal");
    },
    // The two runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun"] })
  );

  test(
    "C3 fails where the comment's author is of another kind, another session, not proven, another person, or names no project (a broker that does not name a restricted member's trade)",
    async () => {
      const [asUser, asAgent, otherSession, unproven, otherOwner, noProject] = await Promise.all([
        codexStandInRun({ commentAuthor: { kind: "user" } }),
        codexStandInRun({ commentAuthor: { kind: "agent" } }),
        codexStandInRun({ commentAuthor: { id: "codex-thread-other" } }),
        codexStandInRun({ commentAuthor: { proven: null } }),
        codexStandInRun({ commentAuthor: { owner: "someone@example.com" } }),
        codexStandInRun({ commentAuthor: { project: null } }),
      ]);
      const failed = (run: CodexRun, words: string) => {
        expect(rowOf(run, "C3")).toMatchObject({ row: { verdict: "FAIL" } });
        expect(rowOf(run, "C3").row?.got).toContain(words);
        expect(run.code).toBe(1);
      };
      // Dispatch records a broker token's write as kind `session`; any other kind is not the
      // Codex session's own write, whatever its id, owner and project.
      failed(asUser, "author kind user");
      failed(asAgent, "author kind agent");
      failed(otherSession, "session codex-thread-other, not codex-thread-main");
      failed(unproven, "not proven by its credential");
      failed(otherOwner, "owner someone@example.com");
      failed(noProject, "project null (a token naming no project");
    },
    // The six runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun", "codexRun", "codexRun", "codexRun", "codexRun"] })
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
      for (const run of [byHand, otherCommit, noLogin]) expect(run.code).toBe(1);
    },
    // The three runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun", "codexRun"] })
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
    // The three runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun", "codexRun"] })
  );

  test(
    "nothing runs against a model that does not answer the leg's nonce, without an exact release, on a Codex that is not the pinned release, or one whose recorded integrity is not the pinned one: every row reads UNKNOWN",
    async () => {
      const [squat, unset, range, otherCodex, otherBytes] = await Promise.all([
        codexStandInRun({ modelSquat: true }),
        codexStandInRun({ envoyVersion: null }),
        codexStandInRun({ envoyVersion: ">=5.0.0" }),
        codexStandInRun({ version: "0.170.0" }),
        codexStandInRun({ codexIntegrity: "other" }),
      ]);
      for (const run of [squat, unset, range, otherCodex, otherBytes]) {
        expect({ problems: unknownProblems(run), stdout: run.stdout }).toEqual({
          problems: [],
          stdout: run.stdout,
        });
      }
      expect(squat.stdout).toContain("did not answer this run's nonce");
      // The squat's port was never handed to Codex: no Codex started.
      expect(Bun.file(join(squat.state, "exec-main.json")).size).toBe(0);
      expect(unset.stdout).toContain("OUTSIDE_CODEX_ENVOY_VERSION is unset");
      expect(range.stdout).toContain("OUTSIDE_CODEX_ENVOY_VERSION is no exact version: >=5.0.0");
      expect(otherCodex.stdout).toContain("not codex-cli 0.171.0");
      expect(otherBytes.stdout).toContain("the integrity it recorded is not the pinned one");
    },
    // The five runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun", "codexRun", "codexRun", "codexRun"] })
  );

  test(
    "answering the first start otherwise than the outside person is told to fails the rows it breaks: hooks left untrusted, C1, C4 and C5; the new model, C2 and C3",
    async () => {
      const [noTrust, newModel] = await Promise.all([
        codexStandInRun({ firstStart: "no-trust" }),
        codexStandInRun({ firstStart: "new-model" }),
      ]);
      const notPass = (run: CodexRun, names: readonly string[]) =>
        Object.fromEntries(names.map((name) => [name, rowOf(run, name).row?.verdict === "PASS"]));
      expect({ passed: notPass(noTrust, ["C1", "C4", "C5"]), stdout: noTrust.stdout }).toEqual({
        passed: { C1: false, C4: false, C5: false },
        stdout: noTrust.stdout,
      });
      expect(rowOf(noTrust, "C1").row?.got).toContain("Codex filed no session record");
      expect(rowOf(noTrust, "C1").row?.got).toContain(
        "main: Hooks need review -> Continue without trusting"
      );
      expect(rowOf(noTrust, "C1").row?.got).toContain(
        "a control run (OUTSIDE_CODEX_FIRST_START=no-trust)"
      );
      expect({ passed: notPass(newModel, ["C2", "C3"]), stdout: newModel.stdout }).toEqual({
        passed: { C2: false, C3: false },
        stdout: newModel.stdout,
      });
      expect(rowOf(newModel, "C2").row?.got).toContain(
        "Codex listed no dispatch tool to the model"
      );
      expect(rowOf(newModel, "C2").row?.got).toContain("main: Meet GPT-6 Sol -> Try new model");
      for (const run of [noTrust, newModel]) expect(run.code).not.toBe(0);
    },
    // The two runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun"] })
  );

  test(
    "a first-run menu that draws each move late is answered with the entry it names, never one past it",
    async () => {
      // The leg's own default start window, 90 s: four screens drawn 2.5 s late are no start held.
      const run = await codexStandInRun({ slowScreens: true, startSeconds: 90 });
      expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
        verdicts: Object.fromEntries(codexRows.map((name) => [name, "PASS"])),
      });
      expect(rowOf(run, "C1").row?.got).toContain(answeredScreens);
      expect(run.code).toBe(0);
    },
    // The run, a bounded child under the Codex run's bound.
    rowLimit("codexRun")
  );

  test(
    "a line the rows do not read ends no run: with an end logged before each turn's calls, the turn's own lines still come and C2-C6 pass",
    async () => {
      const run = await codexStandInRun({ endBeforeTurn: true });
      expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
        verdicts: Object.fromEntries(codexRows.map((name) => [name, "PASS"])),
      });
      expect(run.code).toBe(0);
    },
    // The run, a bounded child under the Codex run's bound.
    rowLimit("codexRun")
  );

  test(
    `the leg exits within ${exitAfterRows / 1000} s of its last row, the new-model control run as every other`,
    async () => {
      // A leg still running at the Codex run's bound fails here as well: runBounded throws.
      const run = await codexStandInRun({ firstStart: "new-model" });
      expect(
        run.outlived,
        `the leg ran on ${run.outlived} ms after its last row: ${run.stderr}`
      ).toBeLessThan(exitAfterRows);
    },
    // The run, a bounded child under the Codex run's bound.
    rowLimit("codexRun")
  );

  test(
    "a screen the leg does not expect holds the runs, and their rows read UNKNOWN naming it",
    async () => {
      // Each of the three starts waits out its window on the screen, so the window is short.
      const run = await codexStandInRun({ unexpectedScreen: true, startSeconds: 6 });
      for (const name of ["C1", "C2", "C3", "C4", "C5", "C6"]) {
        expect(rowOf(run, name)).toMatchObject({ row: { verdict: "UNKNOWN" } });
      }
      expect(rowOf(run, "C1").row?.got).toContain(
        "held by a screen this leg does not expect: Do you trust the files in this folder?"
      );
      expect(run.code).toBe(2);
    },
    // The run, a bounded child under the Codex run's bound.
    rowLimit("codexRun")
  );

  test(
    "C0 fails where the tree Codex installed is not the attested tarball, where the registry's tarball is not its integrity, and where the provenance names another ref than main",
    async () => {
      const [differs, mismatch, branch] = await Promise.all([
        codexStandInRun({ installedDiffers: true }),
        codexStandInRun({ tarballMismatch: true }),
        codexStandInRun({ provenance: { ref: "refs/heads/feat/edited-release" } }),
      ]);
      expect(rowOf(differs, "C0")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(differs, "C0").row?.got).toContain(
        "the tree Codex installed is not the tarball: differ: dist/codex-hook.js"
      );
      expect(rowOf(mismatch, "C0")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(mismatch, "C0").row?.got).toContain("is not the bytes its integrity");
      expect(rowOf(branch, "C0")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(branch, "C0").row?.got).toContain(
        "names the ref refs/heads/feat/edited-release, not refs/heads/main"
      );
      for (const run of [differs, mismatch, branch]) expect(run.code).toBe(1);
    },
    // The three runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun", "codexRun"] })
  );

  test(
    "C0 fails where the attestation's signing certificate names another commit than its payload, and reads UNKNOWN where the certificate was not read",
    async () => {
      const [differs, unread] = await Promise.all([
        codexStandInRun({
          provenance: {
            certificate_matches: false,
            certificate_problem: `the certificate names the source commit ${"0".repeat(40)}, the payload another`,
          },
        }),
        codexStandInRun({
          provenance: {
            certificate_matches: false,
            certificate_unread: "a DER element runs past its parent",
          },
        }),
      ]);
      expect(rowOf(differs, "C0")).toMatchObject({ row: { verdict: "FAIL" } });
      expect(rowOf(differs, "C0").row?.got).toContain(
        `the certificate names the source commit ${"0".repeat(40)}`
      );
      expect(differs.code).toBe(1);
      expect(rowOf(unread, "C0")).toMatchObject({ row: { verdict: "UNKNOWN" } });
      expect(rowOf(unread, "C0").row?.got).toContain(
        "the attestation's signing certificate was not read: a DER element runs past its parent"
      );
    },
    // The two runs at once, each a bounded child under the Codex run's bound.
    rowLimit({ atOnce: ["codexRun", "codexRun"] })
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
  /** The signing certificate of @sjawhar/codex-envoy 5.1.0's SLSA provenance, as the registry
   *  serves it (`verificationMaterial.certificate.rawBytes`): Fulcio's, for release.yaml at
   *  refs/heads/main of cybermonklabs/legion, commit 61ca77bb5616b96605ecab86215240f417a88fe9. */
  const releaseCertificate =
    "MIIG/jCCBoSgAwIBAgIUcp3pTHiXhjWdqn9ZVSOZHTYc54cwCgYIKoZIzj0EAwMwNzEVMBMGA1UEChMMc2lnc3RvcmUuZGV2MR4wHAYDVQQDExVzaWdzdG9yZS1pbnRlcm1lZGlhdGUwHhcNMjYxMTIxMjMwNTQ4WhcNMjYxMTIxMjMxNTQ4WjAAMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE5RX7I7BthNwb19VrPn9TkXgJ1elq+fJ9RNdNm7pgQBIYfMwvRwxQiCT4BTW7D91t1NmrgjP4BbgdWI2OiAzI6KOCBaMwggWfMA4GA1UdDwEB/wQEAwIHgDATBgNVHSUEDDAKBggrBgEFBQcDAzAfBgNVHSMEGDAWgBTf0+nPViQRlvmo2OkoVaLGLhhkPzBkBgNVHREBAf8EWjBYhlZodHRwczovL2dpdGh1Yi5jb20vY3liZXJtb25rbGFicy9sZWdpb24vLmdpdGh1Yi93b3JrZmxvd3MvcmVsZWFzZS55YW1sQHJlZnMvaGVhZHMvbWFpbjA5BgorBgEEAYO/MAEBBCtodHRwczovL3Rva2VuLmFjdGlvbnMuZ2l0aHVidXNlcmNvbnRlbnQuY29tMBIGCisGAQQBg78wAQIEBHB1c2gwNgYKKwYBBAGDvzABAwQoNjFjYTc3YmI1NjE2Yjk2NjA1ZWNhYjg2MjE1MjQwZjQxN2E4OGZlOTAeBgorBgEEAYO/MAEEBBBSZWxlYXNlIFBhY2thZ2VzMCIGCisGAQQBg78wAQUEFGN5YmVybW9ua2xhYnMvbGVnaW9uMB0GCisGAQQBg78wAQYED3JlZnMvaGVhZHMvbWFpbjA7BgorBgEEAYO/MAEIBC0MK2h0dHBzOi8vdG9rZW4uYWN0aW9ucy5naXRodWJ1c2VyY29udGVudC5jb20wZgYKKwYBBAGDvzABCQRYDFZodHRwczovL2dpdGh1Yi5jb20vY3liZXJtb25rbGFicy9sZWdpb24vLmdpdGh1Yi93b3JrZmxvd3MvcmVsZWFzZS55YW1sQHJlZnMvaGVhZHMvbWFpbjA4BgorBgEEAYO/MAEKBCoMKDYxY2E3N2JiNTYxNmI5NjYwNWVjYWI4NjIxNTI0MGY0MTdhODhmZTkwHQYKKwYBBAGDvzABCwQPDA1naXRodWItaG9zdGVkMDcGCisGAQQBg78wAQwEKQwnaHR0cHM6Ly9naXRodWIuY29tL2N5YmVybW9ua2xhYnMvbGVnaW9uMDgGCisGAQQBg78wAQ0EKgwoNjFjYTc3YmI1NjE2Yjk2NjA1ZWNhYjg2MjE1MjQwZjQxN2E4OGZlOTAfBgorBgEEAYO/MAEOBBEMD3JlZnMvaGVhZHMvbWFpbjAaBgorBgEEAYO/MAEPBAwMCjExNDY5Mjc5OTAwMAYKKwYBBAGDvzABEAQiDCBodHRwczovL2dpdGh1Yi5jb20vY3liZXJtb25rbGFiczAZBgorBgEEAYO/MAERBAsMCTM1MTE3MDEyNzBmBgorBgEEAYO/MAESBFgMVmh0dHBzOi8vZ2l0aHViLmNvbS9jeWJlcm1vbmtsYWJzL2xlZ2lvbi8uZ2l0aHViL3dvcmtmbG93cy9yZWxlYXNlLnlhbWxAcmVmcy9oZWFkcy9tYWluMDgGCisGAQQBg78wARMEKgwoNjFjYTc3YmI1NjE2Yjk2NjA1ZWNhYjg2MjE1MjQwZjQxN2E4OGZlOTAUBgorBgEEAYO/MAEUBAYMBHB1c2gwWwYKKwYBBAGDvzABFQRNDEtodHRwczovL2dpdGh1Yi5jb20vY3liZXJtb25rbGFicy9sZWdpb24vYWN0aW9ucy9ydW5zLzQ0OTU2ODk3NDA0L2F0dGVtcHRzLzEwFgYKKwYBBAGDvzABFgQIDAZwdWJsaWMwUgYKKwYBBAGDvzABGAREDEJyZXBvOmN5YmVybW9ua2xhYnNAMzUxMTcwMTI3L2xlZ2lvbkAxMTQ2OTI3OTkwOnJlZjpyZWZzL2hlYWRzL21haW4wgYoGCisGAQQB1nkCBAIEfAR6AHgAdgDdPTBqxscRMmMZHhyZZzcCokpeuN48rf+HinKALynujgAAAaIAW9UEAAAEAwBHMEUCIQDu7qQjDAMUStzoH1hI3qjZmC57lJyblAP3WWk/jeK1PwIgGKDrMYqFfewLWvNNgu6QMDUMmw4p6VdteIsWeDsIboAwCgYIKoZIzj0EAwMDaAAwZQIxAL/CJXUVlaW9gPsXVQuSCYgzmhCUGYgN1M9hkFOtOkw3/VOqkr8q4YTKHVzI0BuCGwIwa+9IUN5oQlhpjOcNh0DeEaIWZVAbFyUWWbK9mTgFSAcWREBcqhIWU0xn5Aq9kFHj";
  const releaseCommit = "61ca77bb5616b96605ecab86215240f417a88fe9";
  /** An attestations answer as the registry serves it: a publish attestation and a SLSA
   *  provenance whose payload names `workflow`, `commit` and the tarball's sha512 `subject`,
   *  signed with `certificate` (base64 DER), the release's own by default. */
  const attestations = (
    workflow: Record<string, string>,
    commit: string,
    subject: string,
    certificate = releaseCertificate
  ) => ({
    attestations: [
      { predicateType: "https://github.com/npm/attestation/tree/main/specs/publish/v0.1" },
      {
        predicateType: "https://slsa.dev/provenance/v1",
        bundle: {
          verificationMaterial: { certificate: { rawBytes: certificate } },
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
    expect(provenanceOf(attestations(release, releaseCommit, hex), integrity)).toEqual({
      provenance: true,
      repository: "https://github.com/cybermonklabs/legion",
      workflow: ".github/workflows/release.yaml",
      ref: "refs/heads/main",
      commit: releaseCommit,
      subject_matches: true,
      certificate_matches: true,
      certificate_problem: "",
    });
    // Another tarball's digest, and an answer with no SLSA provenance, as a hand publish has.
    expect(
      provenanceOf(attestations(release, releaseCommit, "0".repeat(128)), integrity)
    ).toMatchObject({
      subject_matches: false,
    });
    expect(provenanceOf({ attestations: [] }, integrity)).toEqual({ provenance: false });
  });

  test("it holds the payload to the signing certificate's Fulcio extensions: another commit, ref or repository there differs, a missing extension is named, and a certificate it cannot walk is unread", () => {
    const extensions = fulcioExtensions(new Uint8Array(Buffer.from(releaseCertificate, "base64")));
    expect(Object.fromEntries([12, 13, 14, 18].map((arc) => [arc, extensions.get(arc)]))).toEqual({
      12: "https://github.com/cybermonklabs/legion",
      13: releaseCommit,
      14: "refs/heads/main",
      18: "https://github.com/cybermonklabs/legion/.github/workflows/release.yaml@refs/heads/main",
    });
    // A payload that names another commit, ref or repository than the certificate.
    expect(provenanceOf(attestations(release, "a".repeat(40), hex), integrity)).toMatchObject({
      certificate_matches: false,
      certificate_problem: `the certificate names the source commit ${releaseCommit}, the payload ${"a".repeat(40)}`,
    });
    expect(
      provenanceOf(
        attestations({ ...release, ref: "refs/heads/feat/edited" }, releaseCommit, hex),
        integrity
      ).certificate_problem
    ).toContain(
      "the certificate names the source ref refs/heads/main, the payload refs/heads/feat/edited"
    );
    // A certificate that lacks an extension the payload is held to.
    const lacking = new Map(extensions);
    lacking.delete(13);
    expect(
      certificateProblem(lacking, {
        repository: release.repository,
        workflow: release.path,
        ref: release.ref,
        commit: releaseCommit,
      })
    ).toBe("the certificate names no source commit (1.3.6.1.4.1.57264.1.13)");
    // A certificate cut short, and none at all: read as no reading, never as a match.
    for (const certificate of [releaseCertificate.slice(0, 200), ""]) {
      expect(
        provenanceOf(attestations(release, releaseCommit, hex, certificate), integrity)
      ).toMatchObject({
        certificate_matches: false,
        certificate_unread: expect.any(String),
      });
    }
  });

  test("it compares a tarball's tree with an installed one, every file by its path", () => {
    const tarball = scratch("tarball");
    const installed = scratch("installed");
    for (const [dir, files] of [
      [
        tarball,
        { "package.json": "{}", "dist/a.js": "same", "skills/x/SKILL.md": "one", "only.txt": "t" },
      ],
      [
        installed,
        { "package.json": "{}", "dist/a.js": "same", "skills/x/SKILL.md": "two", "extra.txt": "i" },
      ],
    ] as const) {
      for (const [name, text] of Object.entries(files)) {
        mkdirSync(join(dir, name, ".."), { recursive: true });
        writeFileSync(join(dir, name), text);
      }
    }
    expect(compareTrees(tarball, installed)).toEqual({
      equal: 2,
      differ: "skills/x/SKILL.md",
      only_tarball: "only.txt",
      only_installed: "extra.txt",
    });
  });

  test("it compares links by their targets, and reads an entry of another type as one that differs", () => {
    const tarball = scratch("tarball");
    const installed = scratch("installed");
    for (const dir of [tarball, installed]) writeFileSync(join(dir, "real.js"), "same");
    // The same target on both sides; another target; a link in one tree and a file in the other.
    symlinkSync("real.js", join(tarball, "same.js"));
    symlinkSync("real.js", join(installed, "same.js"));
    symlinkSync("real.js", join(tarball, "moved.js"));
    symlinkSync("elsewhere.js", join(installed, "moved.js"));
    symlinkSync("real.js", join(tarball, "kind.js"));
    writeFileSync(join(installed, "kind.js"), "same");
    expect(compareTrees(tarball, installed)).toEqual({
      equal: 2,
      differ: "kind.js moved.js",
      only_tarball: "",
      only_installed: "",
    });
  });

  test(
    "attestation downloads the release's tarball and reads whether it is the bytes the registry's integrity names, and reads no registry or tarball that is not https",
    async () => {
      const served = Buffer.from("the release's tarball, as the registry serves it");
      const good = `sha512-${new Bun.CryptoHasher("sha512").update(served).digest("base64")}`;
      // The same integrity with one character changed: the tarball is not its bytes.
      const bad = `${good.slice(0, 10)}${good[10] === "A" ? "B" : "A"}${good.slice(11)}`;
      let named = good;
      let tarballAt = "";
      const registry = Bun.serve({
        hostname: "127.0.0.1",
        port: 0,
        fetch: (request) =>
          new URL(request.url).pathname === "/release.tgz"
            ? new Response(served)
            : Response.json({
                dist: {
                  integrity: named,
                  tarball: tarballAt || new URL("/release.tgz", request.url).href,
                },
              }),
      });
      const read = async (origin = `http://127.0.0.1:${registry.port}`) => {
        const file = join(scratch("tarball-file"), "release.tgz");
        const ran = await runBounded({
          argv: ["bun", provenanceHelper, "attestation", "@sjawhar/codex-envoy", "5.0.1", file],
          bound: "short",
          env: {
            PATH: process.env.PATH ?? "",
            HOME: scratch("home"),
            OUTSIDE_NPM_REGISTRY: origin,
          },
          from: import.meta.url,
        });
        const written = Bun.file(file).size > 0 ? readFileSync(file, "utf8") : "";
        return { answer: JSON.parse(ran.stdout), written };
      };
      try {
        expect(await read()).toEqual({
          answer: { status: 200, integrity: good, tarball_matches: true, provenance: false },
          written: served.toString(),
        });
        named = bad;
        expect((await read()).answer).toEqual({
          status: 200,
          integrity: bad,
          tarball_matches: false,
          provenance: false,
        });
        // A plain-http registry off this machine is never read, nor a tarball the record names
        // at one.
        expect((await read("http://registry.example")).answer).toEqual({
          unknown: "OUTSIDE_NPM_REGISTRY is no https:// origin: http://registry.example",
        });
        tarballAt = "http://registry.example/release.tgz";
        expect((await read()).answer).toEqual({
          unknown:
            "the registry's record names no https:// tarball: http://registry.example/release.tgz",
        });
      } finally {
        registry.stop(true);
      }
    },
    // The four reads, one after another, each a bounded child under the short bound.
    rowLimit("short", "short", "short", "short")
  );
});

describe.skipIf(skipWithout("bash"))("the leg's waits, on a clock only `sleep` moves", () => {
  /** The leg's own `screen_text`, `answer_screen` and `drive`, cut from the leg as they stand, run
   *  in bash on a clock that only `sleep` moves: `pane` and `tools` are bash commands that read
   *  `$clock`, the pane's text and the stand-in model's tools line at that moment. Answers the
   *  call's status and its `drive_problem`. */
  const onFakeClock = async (call: string, pane: string, tools = ":") => {
    const file = join(scratch("fake-clock"), "harness.sh");
    writeFileSync(
      file,
      `set -u
here=${JSON.stringify(join(import.meta.dir, "..", "acceptance"))}
. "$here/outside-agent-lib.sh"
for name in screen_text answer_screen drive; do
  body=$(awk -v name="$name" 'index($0, name "() {") == 1 {found = 1} found {print} found && $0 == "}" {exit}' "$here/outside-agent-codex.sh")
  [ -n "$body" ] || { echo "the leg holds no $name"; exit 3; }
  eval "$body"
done
clock=0
now() { printf '%s\\n' "$clock"; }
sleep() { clock=$((clock + $1)); }
codex_pane() { ${pane}; }
logged() { ${tools}; }
codex_alive() { return 0; }
choose() { return 0; }
screens='' hooks_choice='Trust all and continue' model_choice='Use existing model'
drive_problem=''
${call}
printf 'status %s; %s\\n' "$?" "\${drive_problem:-no problem}"
`
    );
    const ran = await runBounded({
      argv: ["bash", file],
      bound: "short",
      env: { PATH: process.env.PATH ?? "" },
      from: import.meta.url,
    });
    return ran.stdout.trim();
  };
  const menu = "printf '%s\\n' '  Meet GPT-6 Sol' '› 1. Try new model' '  2. Use existing model'";
  const working = "printf '%s\\n' '› outside-e2e codex-run main n1' '' '• Working'";

  test(
    "a screen that goes, or a turn that begins, in a window's last second is read as gone or begun, never as held",
    async () => {
      // The menu goes as the 2 s window closes: answered, not a screen that stayed.
      const went = await onFakeClock(
        "answer_screen main 2",
        `if [ "$clock" -lt 2 ]; then ${menu}; else ${working}; fi`
      );
      // The control: a menu that never goes stays, and says so.
      const stayed = await onFakeClock("answer_screen main 2", menu);
      // The turn's tools line comes as the 2 s start window closes: the turn began.
      const began = await onFakeClock(
        "drive main 2",
        working,
        `[ "$clock" -lt 2 ] || printf '%s' '{"run":"main","step":"tools"}'`
      );
      expect({ went, stayed, began }).toEqual({
        went: "status 0; no problem",
        stayed:
          "status 1; the screen 'Meet GPT-6 Sol' stayed after 'Use existing model' was taken:   Meet GPT-6 Sol / › 1. Try new model /   2. Use existing model",
        began: "status 0; no problem",
      });
    },
    // The three runs, one after another, each a bounded child under the short bound.
    rowLimit("short", "short", "short")
  );
});

describe("the probe's reading of a comment's author", () => {
  test("it reads the author as Dispatch records a broker token's write: kind session, its id, proven, owner and project, and no act", () => {
    // api/broker_token.go's actor for a token named with one project, as an issue's comments
    // answer it; broker_token_test.go's requireSessionProven pins that it carries no act.
    const comments = [
      { id: "c-1", body: "another", author: { kind: "person", id: person, proven: true } },
      {
        id: "c-2",
        body: "outside-e2e codex-comment n1",
        author: { kind: "session", id: "th-1", owner: person, project: "EXAMPLE", proven: true },
      },
    ];
    expect(commentAuthor(comments, "codex-comment n1")).toEqual({
      found: true,
      id: "c-2",
      author_kind: "session",
      author_id: "th-1",
      proven: true,
      owner: person,
      project: "EXAMPLE",
    });
    // A field Dispatch leaves out reads null, and proven false: none of them reads as a match.
    expect(
      commentAuthor([{ id: "c-3", body: "x n1", author: { kind: "session", id: "th-1" } }], "n1")
    ).toEqual({
      found: true,
      id: "c-3",
      author_kind: "session",
      author_id: "th-1",
      proven: false,
      owner: null,
      project: null,
    });
    expect(commentAuthor(comments, "no such text")).toMatchObject({
      found: false,
      author_id: null,
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
    // 107 ms into an interactive turn, 0.171.0 sends a request that offers apply_patch alone and
    // holds the prompt twice. It is not the turn's: it is answered with a message and logs
    // nothing, no tools and no end, so the leg waits on for the turn's own lines.
    const side = nextTurn(
      plan,
      { input: [prompt("main"), prompt("main")], tools: [{ name: "apply_patch", type: "custom" }] },
      3,
      logged,
      1000
    );
    expect(side).toMatchObject({ items: [{ type: "message" }], log: [] });
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
