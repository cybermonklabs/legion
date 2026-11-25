import { beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import {
  cpSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  realpathSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptAssignments } from "../../../../scripts/e2e/lib/script-functions";
import {
  claimRefusal,
  composeRolePrompt,
  countDefault,
  DAEMON_MODULE,
  labelDaemon,
  labelRoutes,
  labelShapes,
  postRefusal,
  RIG_GH_TOKEN,
  repositoriesLive,
  reviewRoundSentences,
  runDaemonPane,
  serves,
  workerPane,
} from "./daemon-pane";
import { nameWords, systemPromptIn, systemPromptOf, vocabulary } from "./run-names";
import { bundled, spawnChecked } from "./test-programs";
import { WORLD_REPO } from "./world-issue";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default, so each gets a minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

// Every row below asks the daemon's own functions for a pane environment or a role's prompt, which
// `go run` compiles and runs, so a host with no go runs none of them.
const skip = skipWithout("go");

// The run directory: the daemon's pane environment makes the pane's home and the claim's gh
// directory under its state directory.
const rig = temporaryDirectory("skill-scenarios-pane");

/** The pane composer the rows run, a bundle built from worker-pane.ts for this suite, so no run
 * of it transpiles it again (test-programs.ts). */
const WORKER_PANE = bundled(path.join(import.meta.dir, "worker-pane.ts"));

// The caller's home, where the profile's agent directory and its envoy.json live: a scratch one, so
// no envoy.json of the person running the test is read.
const home = path.join(rig, "caller-home");

// A pane's jj is the first jj on its PATH, and its git the launcher directory's git wrapper where
// the daemon's boot writes one. Its gh is the launcher directory's wrapper over the real gh the
// daemon resolves on its PATH at boot: the inherited PATH holds a stand-in gh, the one tool the
// daemon needs on it for the pane to be built. It reads as a compiled program (no `#!` line),
// since the daemon refuses a script as the first gh on PATH.
// Both bins are staged in a hook, below the one the reservation above registered: a run that
// selects no row of this file makes neither, and the run directory itself is never made.
const outerBin = path.join(rig, "outer-bin");
const scriptBin = path.join(rig, "script-bin");
beforeAll(() => {
  mkdirSync(outerBin);
  writeFileSync(path.join(outerBin, "gh"), "\x7fELF\x02\x01\x01\x00", { mode: 0o700 });
  mkdirSync(scriptBin);
  writeFileSync(path.join(scriptBin, "gh"), "#!/bin/sh\nexit 0\n", { mode: 0o700 });
});
/** The caller's model keys: random, as real ones are. Two reach the worker, one must not. */
const ANTHROPIC_KEY = fixtureSecret("anthro");
const GEMINI_KEY = fixtureSecret("gemini");
const OPENAI_KEY = fixtureSecret("openai");
const inherited = {
  HOME: home,
  ANTHROPIC_API_KEY: ANTHROPIC_KEY,
  GEMINI_API_KEY: GEMINI_KEY,
  OPENAI_API_KEY: OPENAI_KEY,
  LEGION_GRANT: "outer-grant",
  DISPATCH_TOKEN: fixtureSecret("dsp"),
  GH_TOKEN: fixtureSecret("gh"),
  GH_CONFIG_DIR: "/outer/gh",
  LEGION_GH_PATH: "/outer/gh-binary",
  PATH: `${outerBin}:/usr/bin`,
  OMP_SESSION_EXCLUSIVE: "1",
};

/** The claim every row asks for a pane of: the rig's project and issue, and the repository and
 * default branch of the world rig.sh builds ($worker_repo, world-issue.ts's WORLD_REPO, and
 * $worker_base). They are the world's own because the pane's addressing names them, and the
 * claim's gh bundle is the directory of that repository's owner. */
const claim = {
  project: "skillsrig",
  issue: "LWEVAL-1",
  role: "tester",
  repository: WORLD_REPO,
  defaultBranch: "main",
} as const;
/** The owner of the world's repository, whose directory the claim's gh bundle is. */
const WORLD_OWNER = WORLD_REPO.split("/")[0];

// The pane environment comes from the daemon's own functions, compiled and run by `go run`, which
// can take minutes on a cold build cache.
test.skipIf(skip)(
  "strips an inherited Anthropic key and GitHub token, and gives the pane the claim's gh files and no grant file",
  async () => {
    const workerEnv = (
      await workerPane({ rig, port: 13399, profile: "rig-profile" }, inherited, claim, {
        daemonModule: DAEMON_MODULE,
      })
    ).env;

    expect(workerEnv).not.toHaveProperty("ANTHROPIC_API_KEY");
    expect(workerEnv).not.toHaveProperty("LEGION_GRANT");
    expect(workerEnv).not.toHaveProperty("DISPATCH_TOKEN");
    // The daemon names no grant file on a pane: the `legion` tool mints its grants in-process, and
    // no agent runs `legion` from bash (LEGION-631).
    expect(workerEnv).not.toHaveProperty("LEGION_GRANT_FILE");
    expect(workerEnv).not.toHaveProperty("LEGION_GIT_PATH");
    expect(workerEnv).not.toHaveProperty("LEGION_JJ_PATH");
    expect(workerEnv).not.toHaveProperty("OMP_SESSION_EXCLUSIVE");
    const ghRoot = `${rig}/state/secrets/legion-skillsrig-lweval-1-tester-gh`;
    // The bundle's one directory is that of the owner of the claim's repository, which the pane's
    // addressing names.
    const ghConfigDir = `${ghRoot}/${WORLD_OWNER}`;
    expect(workerEnv).toMatchObject({
      GEMINI_API_KEY: GEMINI_KEY,
      OPENAI_API_KEY: OPENAI_KEY,
      PATH: `${rig}/state/bin:${outerBin}:/usr/bin`,
      // The caller's own token and directory never reach the pane: the daemon's pane sets the
      // three token variables empty beside the claim's gh bundle, so gh reads the files and
      // nothing else, and its wrapper runs the real gh the daemon found on its PATH, not the
      // caller's.
      GH_CONFIG_DIR: ghConfigDir,
      LEGION_GH_OWNERS_DIR: ghRoot,
      LEGION_GH_PATH: path.join(outerBin, "gh"),
      GH_TOKEN: "",
      GITHUB_TOKEN: "",
      GH_HOST: "",
      LEGION_ISSUE: "LWEVAL-1",
      LEGION_ROLE: "tester",
      XDG_CONFIG_HOME: `${rig}/state/home/.config`,
    });
    // The claim's gh bundle holds the rig's token in gh's own migrated shape, readable by the pane
    // alone, as the daemon's tmux runtime writes it.
    expect(statSync(ghRoot).mode & 0o777).toBe(0o700);
    expect(statSync(ghConfigDir).mode & 0o777).toBe(0o700);
    expect(statSync(path.join(ghConfigDir, "hosts.yml")).mode & 0o777).toBe(0o600);
    expect(readFileSync(path.join(ghConfigDir, "hosts.yml"), "utf8")).toContain(
      `oauth_token: ${RIG_GH_TOKEN}`
    );
    expect(readFileSync(path.join(ghConfigDir, "config.yml"), "utf8")).toBe('version: "1"\n');
  },
  600_000
);

// The pane's own Envoy extension reads the profile's envoy.json, so the daemon is asked to name the
// listener and NATS servers written there.
test.skipIf(skip)(
  "names the listener and NATS servers the profile's envoy.json writes",
  async () => {
    const agent = path.join(home, ".omp", "profiles", "rig-envoy-profile", "agent");
    mkdirSync(agent, { recursive: true });
    writeFileSync(
      path.join(agent, "envoy.json"),
      JSON.stringify({
        envoy: {
          enabled: true,
          url: "http://rig-listener.internal.example:9020",
          natsUrls: ["nats://rig-nats.internal.example:4222"],
        },
      })
    );
    const workerEnv = (
      await workerPane({ rig, port: 13399, profile: "rig-envoy-profile" }, inherited, claim, {
        daemonModule: DAEMON_MODULE,
      })
    ).env;

    expect(workerEnv).toMatchObject({
      OMP_PROFILE: "rig-envoy-profile",
      PI_CODING_AGENT_DIR: agent,
      ENVOY_URL: "http://rig-listener.internal.example:9020",
      ENVOY_NATS_URL: "nats://rig-nats.internal.example:4222",
    });
  },
  600_000
);

// A seed the profile's envoy.json pairs with other servers than ENVOY_NATS_URL leaves Envoy on with
// inbound messages off, which the pane's own extension would meet; the rig refuses it, as it refuses
// an Envoy that is off with an error, rather than ask the daemon for a pane with no NATS server.
test.skipIf(skip)(
  "refuses a profile envoy.json whose NATS seed Envoy refuses",
  async () => {
    const agent = path.join(home, ".omp", "profiles", "rig-seed-profile", "agent");
    mkdirSync(agent, { recursive: true });
    const file = path.join(agent, "envoy.json");
    writeFileSync(
      file,
      JSON.stringify({
        envoy: {
          url: "http://rig-listener.internal.example:9020",
          natsUrls: ["nats://rig-nats.internal.example:4222"],
          natsSeed: "rig-seed-placeholder",
        },
      })
    );

    await expect(
      workerPane(
        { rig, port: 13399, profile: "rig-seed-profile" },
        { ...inherited, ENVOY_NATS_URL: "nats://other-nats.internal.example:4222" },
        claim,
        { daemonModule: DAEMON_MODULE }
      )
    ).rejects.toThrow(
      `envoy: ENVOY_NATS_URL names other servers than ${file}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`
    );
  },
  600_000
);

// An Envoy that is on with no NATS server takes no inbound messages either, and the pane's Legion
// claim refuses then (pi-envoy's `requireInbound`), so the rig refuses it the same way.
test.skipIf(skip)(
  "refuses a profile envoy.json whose Envoy has no NATS server",
  async () => {
    const agent = path.join(home, ".omp", "profiles", "rig-no-nats-profile", "agent");
    mkdirSync(agent, { recursive: true });
    writeFileSync(
      path.join(agent, "envoy.json"),
      JSON.stringify({ envoy: { url: "http://rig-listener.internal.example:9020" } })
    );

    await expect(
      workerPane({ rig, port: 13399, profile: "rig-no-nats-profile" }, inherited, claim, {
        daemonModule: DAEMON_MODULE,
      })
    ).rejects.toThrow("envoy: Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)");
  },
  600_000
);

// The daemon refuses a script as the first gh on its PATH (a gh wrapper would answer in place of
// the claim's App token), and so does the rig's reading of it; a gh the rig names, as an operator
// names one with LEGION_GH_PATH, is the pane's real gh whatever it is, which is how the rig hands
// panes its recording stand-in.
test.skipIf(skip)(
  "refuses a script as the first gh on PATH and takes the gh the rig names",
  async () => {
    const launch = { rig, port: 13399, profile: "rig-profile" };
    const shimmed = { ...inherited, PATH: `${scriptBin}:${outerBin}:/usr/bin` };
    await expect(
      workerPane(launch, shimmed, claim, { daemonModule: DAEMON_MODULE })
    ).rejects.toThrow(
      `the first gh on the daemon's PATH, ${path.join(scriptBin, "gh")}, is a script`
    );
    const named = (
      await workerPane(launch, shimmed, claim, {
        daemonModule: DAEMON_MODULE,
        ghPath: path.join(scriptBin, "gh"),
      })
    ).env;
    expect(named.LEGION_GH_PATH).toBe(path.join(scriptBin, "gh"));
  },
  600_000
);

// Catches: a pane whose addressing names a repository and a branch of the rig's own rather than
// the world's. It named `skills-rig/project` on `trunk` while every world rig.sh builds is
// `fernbrook/widgets` on `main`, and a worker follows its addressing: recorded runs posted their
// review to the repository the addressing named, which the scorer's repository rule read as no
// review at all (rig.sh's worker world comment holds that story). The gh bundle is the same
// fact: its one directory is the owner of that repository, and the addressing names that owner
// as the one its App reaches, so no part of the pane names a second one.
test.skipIf(skip)(
  "the addressing names the world's own repository and default branch",
  async () => {
    const pane = await workerPane({ rig, port: 13399, profile: "rig-profile" }, inherited, claim, {
      daemonModule: DAEMON_MODULE,
      systemPrompt: true,
    });
    // The pane is handed its prompt as a file, as a pod's launch hands it (#4190), so the
    // addressing is read from the file the argument names (run-names.ts's `systemPromptIn`).
    const prompt = systemPromptIn(pane.systemPromptArgument ?? "", rig);
    expect(prompt.place).toBe(
      "<run>/state/secrets/legion-skillsrig-lweval-1-tester-system-prompt.md"
    );
    expect(prompt.text).toContain(
      `Your issue's repository is ${WORLD_REPO} (default branch main); your role's GitHub App is installed on the owner ${WORLD_OWNER}, and its token reaches every repository of that owner.`
    );
    expect(prompt.text).not.toContain("skills-rig");
    expect(pane.env.GH_CONFIG_DIR).toBe(
      `${rig}/state/secrets/legion-skillsrig-lweval-1-tester-gh/${WORLD_OWNER}`
    );
  },
  600_000
);

// And a world that names no default branch is refused rather than panes built on an empty one.
// Catches: the pane composed with `defaultBranch: ""`, whose addressing then reads "default
// branch " with nothing after it — a worker follows its addressing, and a branch it cannot read
// is one it would guess. The repository is read the same way, by the parser that refuses an
// empty name, so this is the branch's own refusal beside it.
test.skipIf(skip)(
  "a world that names no default branch is refused",
  async () => {
    await expect(
      workerPane(
        { rig, port: 13399, profile: "rig-profile" },
        inherited,
        {
          ...claim,
          defaultBranch: "",
        },
        {
          daemonModule: DAEMON_MODULE,
          systemPrompt: true,
        }
      )
    ).rejects.toThrow("the rig's world names no default branch, which the pane's addressing names");
  },
  600_000
);

// The phase a worker runs in is the daemon's own: the first phase of the transition table
// workflow.RoleFor gives the role, with the word of the handoff record that phase ends with, and
// a role the table gives no phase is an error rather than an empty answer. daemon-standin.ts
// reads both from this command, so a wrong answer there would seed every run's phase wrongly.
test.skipIf(skip)(
  "phase names the phase a role works and the handoff word it ends with",
  () => {
    expect(runDaemonPane(DAEMON_MODULE, ["phase", "implementer"])).toBe(
      "implementing\nimplement\n"
    );
    expect(runDaemonPane(DAEMON_MODULE, ["phase", "tester"])).toBe("testing\ntest\n");
    expect(() => runDaemonPane(DAEMON_MODULE, ["phase", "nobody"])).toThrow(
      '"nobody" works no phase'
    );
  },
  600_000
);

// Dispatch's refusal of a message the daemon posts, as the daemon decides it before it records a
// move whose sentence it posts (workflow's `backwardReasonRefusal`): the longest post its outbox
// could make of the body, the body under the marker of the largest row id, read by Dispatch's
// own reader (daemon-pane.go's `post-check`). daemon-standin.ts refuses a backward move by it.
// Catches: a reading of the body alone, which takes a body of as many blocks as Dispatch holds
// one to, where the marker's block puts the post past it (the daemon's own
// TestABackwardReasonIsMeasuredAsItsPost), and a body ending in an open fence, whose code block
// the marker lengthens; a decision block read as a bound, or as no refusal; and a body Dispatch
// takes read as refused.
test.skipIf(skip)(
  "post-check reads the longest post the daemon could make of a body as Dispatch reads it",
  () => {
    const checkout = path.resolve(DAEMON_MODULE, "../..");
    const blocks = (count: number) => Array.from({ length: count }, () => "y").join("\n\n");
    expect(postRefusal(checkout, "the fix needs code.")).toBeUndefined();
    expect(postRefusal(checkout, blocks(32_767))).toBeUndefined();
    expect(postRefusal(checkout, blocks(32_768))).toEqual({
      decision: false,
      sentence: "body is 1 blocks over the 32768-block limit (32769/32768)",
    });
    expect(postRefusal(checkout, `\`\`\`\n${"y".repeat(31_970)}`)).toEqual({
      decision: false,
      sentence:
        "body block 1 (code_block) is 18 characters over the 32000-character block limit (32018/32000)",
    });
    expect(
      postRefusal(checkout, 'The plan is wrong.\n\n:::ask{urgency="med"}\nWhich table?\n:::')
    ).toEqual({
      decision: true,
      sentence:
        "body block 2 (ask) holds a decision block, which nobody can answer where it stands: ask the question in the issue's own document, as a decision block there, or open an ask with `dispatch ask`; to show a decision block as an example, put it in a fenced code block",
    });
  },
  600_000
);

// The refusal a worker's completion of a phase its role does not run gets is the daemon's own
// sentence, which the stand-in answers with and the skill has the worker quote to its
// architect. This row holds the reading to the daemon's source: it takes the format out of the
// source itself and fills it, so a reading that drifted from the daemon — a different sentence,
// the arguments in another order — fails here. This checkout's engine builds that refusal with
// `intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key)`, whose sentence intake's
// `func NotCurrentPhase` writes with its parameters in the same order.
const ENGINE = path.join(DAEMON_MODULE, "internal", "workflow", "engine.go");
const INTAKE = path.join(DAEMON_MODULE, "internal", "intake", "facts.go");
/** The format the daemon builds its HANDOFF_NOT_CURRENT_PHASE message with, read out of its
 * source, and the pieces of text around its verbs. */
function refusalFormat(): { format: string; pieces: string[] } {
  expect(readFileSync(ENGINE, "utf8")).toContain(
    "intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key)"
  );
  const [, format = ""] =
    /func NotCurrentPhase\(role claim\.Role, p phase\.Phase, issue string\)[^{]*\{[^}]*Code: "HANDOFF_NOT_CURRENT_PHASE", Message: fmt\.Sprintf\("((?:[^"\\]|\\.)*)", role, p, issue\)/.exec(
      readFileSync(INTAKE, "utf8")
    ) ?? [];
  return { format, pieces: format.split(/%[a-z]/) };
}
test.skipIf(skip)(
  "refusal not-current-phase prints the daemon's own refusal, read from the daemon's code",
  () => {
    const { format, pieces } = refusalFormat();
    expect(format).not.toBe("");
    // Filled with the role, the issue's phase and the issue's key, in that order (refusalFormat).
    const filled = ["implementer", "accepting", "LWEVAL-1"].reduce(
      (text, value) => text.replace(/%[a-z]/, value),
      format
    );
    expect(
      runDaemonPane(DAEMON_MODULE, [
        "refusal",
        "not-current-phase",
        "implementer",
        "accepting",
        "LWEVAL-1",
      ])
    ).toBe(`HANDOFF_NOT_CURRENT_PHASE\n${filled}\n`);
    expect(pieces[1]).toBeDefined();
  },
  600_000
);

// And no file of the rig writes that sentence itself: a copy of it passes the row above on the
// day it is made and goes stale the day the daemon's wording moves, with nothing to say so. What
// a copy would carry is the words the engine writes between the role and the phase, which are
// this refusal's own. The tail every workflow refusal ends with — `; this completion changed
// nothing` — is no test of it: the stand-in restates two other refusals of the daemon's, the
// record-required and record-mismatch ones, and carries that tail in each.
test("no file of the rig restates the daemon's refusal", () => {
  const own = refusalFormat().pieces[1] ?? "";
  expect(own.trim().length).toBeGreaterThan(10);
  for (const file of [
    "daemon-standin.ts",
    "daemon-standin.test.ts",
    "daemon-pane.go",
    "daemon-pane.ts",
    "rig.sh",
    "score.ts",
  ]) {
    expect(readFileSync(path.join(import.meta.dir, file), "utf8")).not.toContain(own);
  }
});

// The two shapes the daemon has built that refusal in, one row each, and a source in neither,
// each read through sources of the row's own: the files the `refusal` command takes after its
// three arguments, an engine source and an intake package directory (daemon-pane.go's main),
// which are the daemon's own for every other caller. Parsed, never compiled or linked: the
// reading is `parser.ParseFile` of each, so a substitute needs the shape of the daemon's code
// and nothing it declares. Catches: either shape's branch dropped (this checkout's engine no
// longer writes an older daemon's, so no other row reaches it), parameters filled in the
// format's order rather than mapped to the engine's arguments, and a reading that answers a
// daemon in neither shape with a sentence rather than a refusal to boot.
/** Runs the `refusal` command over `engine`, the engine's body, and over an intake package
 * holding `intake` when given, and answers what it printed. */
function refusalOver(name: string, engine: string, intake?: string): string {
  const engineFile = path.join(rig, `engine-${name}.go`);
  writeFileSync(engineFile, `package workflow\n\nfunc complete() error {\n\treturn ${engine}\n}\n`);
  const intakeDir = path.join(rig, `intake-${name}`);
  mkdirSync(intakeDir, { recursive: true });
  if (intake !== undefined)
    writeFileSync(path.join(intakeDir, "facts.go"), `package intake\n\n${intake}\n`);
  return runDaemonPane(DAEMON_MODULE, [
    "refusal",
    "not-current-phase",
    "implementer",
    "accepting",
    "LWEVAL-1",
    engineFile,
    intakeDir,
  ]);
}
test.skipIf(skip)(
  "the refusal reading fills the intake package's sentence with the engine's arguments",
  () => {
    // The format takes the issue first; the engine's call names it last.
    expect(
      refusalOver(
        "intake-shape",
        "intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key)",
        'func NotCurrentPhase(role claim.Role, p phase.Phase, issue string) *Refusal {\n\treturn &Refusal{Status: 409, Code: "HANDOFF_NOT_CURRENT_PHASE", Message: fmt.Sprintf("%s: the %s may not finish %s", issue, role, p)}\n}'
      )
    ).toBe("HANDOFF_NOT_CURRENT_PHASE\nLWEVAL-1: the implementer may not finish accepting\n");
  },
  600_000
);
test.skipIf(skip)(
  "the refusal reading fills an older daemon's refused call",
  () => {
    expect(
      refusalOver(
        "refused-shape",
        'refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf("the %s may not finish %s of %s", fact.Role, issue.Phase, issue.Key))'
      )
    ).toBe("HANDOFF_NOT_CURRENT_PHASE\nthe implementer may not finish accepting of LWEVAL-1\n");
  },
  600_000
);
test.skipIf(skip)(
  "the refusal reading refuses a daemon in neither shape, naming both",
  () => {
    expect(() => refusalOver("neither-shape", "nil")).toThrow(
      'holds no refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf(…)) call and no intake.NotCurrentPhase(…) call'
    );
  },
  600_000
);

// The guards that reading stands on, one row each. Every one of them answers a daemon that
// writes its refusal in a shape this program cannot fill, which today's daemon does not, so a
// row reaches them only through sources of its own (above). Catches: any of them dropped, each
// of the first four of which survived the mutation pass of 3d88a1ed for want of this path. A
// dropped guard answers a drifted daemon with a sentence built out of nothing — an empty
// argument, a `%!s(MISSING)` verb, a format some other printer wrote — or with a panic, and the
// stand-in would teach every agent that sentence.
const SHARED =
  'func NotCurrentPhase(role claim.Role, p phase.Phase, issue string) *Refusal {\n\treturn &Refusal{Status: 409, Code: "HANDOFF_NOT_CURRENT_PHASE", Message: fmt.Sprintf("%s may not finish %s of %s", role, p, issue)}\n}';
const SUBSTITUTE_ENGINES: readonly {
  readonly file: string;
  readonly shape: string;
  readonly call: string;
  readonly intake?: string;
  readonly says: string;
}[] = [
  {
    file: "unknown-argument",
    shape: "an argument this program has no value for",
    call: 'refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf("%s may not complete %s", fact.Role, issue.Tree))',
    says: "fills the HANDOFF_NOT_CURRENT_PHASE message with issue.Tree, which this program has no value for",
  },
  {
    file: "more-verbs",
    shape: "a format with more verbs than the arguments it names",
    call: 'refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf("%s may not complete %s of %s at %s", fact.Role, issue.Phase, issue.Key))',
    says: "takes other arguments than the 3 it names",
  },
  {
    file: "one-argument",
    shape: "a refused call of another arity",
    call: 'refused("HANDOFF_NOT_CURRENT_PHASE")',
    says: 'holds no refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf(…)) call',
  },
  {
    // Three arguments, the engine's own message among them: a reading that takes any call of
    // two or more reads the sentence out of a call whose third argument it never looked at.
    file: "three-arguments",
    shape: "a refused call of three arguments",
    call: 'refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf("the %s does not run phase %s of %s", fact.Role, issue.Phase, issue.Key), cause)',
    says: 'holds no refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf(…)) call',
  },
  {
    file: "other-printer",
    shape: "a message another printer built",
    call: 'refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Errorf("%s may not complete %s", fact.Role, issue.Phase))',
    says: 'holds no refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf(…)) call',
  },
  {
    file: "no-intake-function",
    shape: "an intake call whose package holds no NotCurrentPhase",
    call: "intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key)",
    intake: "func OtherRefusal() *Refusal { return nil }",
    says: "holds no func NotCurrentPhase returning a HANDOFF_NOT_CURRENT_PHASE refusal",
  },
  {
    file: "intake-arity",
    shape: "an intake call of another arity than the function it calls",
    call: "intake.NotCurrentPhase(fact.Role, issue.Phase)",
    intake: SHARED,
    says: "calls intake.NotCurrentPhase with 2 arguments",
  },
  {
    file: "intake-no-parameter",
    shape: "an intake sentence filled with no parameter of its function",
    call: "intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key)",
    intake:
      'func NotCurrentPhase(role claim.Role, p phase.Phase, issue string) *Refusal {\n\treturn &Refusal{Status: 409, Code: "HANDOFF_NOT_CURRENT_PHASE", Message: fmt.Sprintf("%s may not finish %s", role, tree)}\n}',
    says: "fills the HANDOFF_NOT_CURRENT_PHASE message with tree, which is none of NotCurrentPhase's parameters",
  },
];
for (const { file, shape, call, intake, says } of SUBSTITUTE_ENGINES) {
  test.skipIf(skip)(
    `the refusal reading refuses ${shape} and invents no sentence`,
    () => {
      expect(() => refusalOver(file, call, intake)).toThrow(says);
    },
    600_000
  );
}

/** A substitute checkout whose daemon's API package holds `body` in one file, read through
 * daemon-pane.ts's `labelRoutes`. */
function routesOver(name: string, body: string): readonly string[] {
  const checkout = path.join(rig, `routes-${name}`);
  const api = path.join(checkout, "packages", "daemon", "internal", "api");
  mkdirSync(api, { recursive: true });
  writeFileSync(path.join(api, "server.go"), `package api\n\nfunc routes() {\n${body}\n}\n`);
  // A test file registers nothing the daemon serves.
  writeFileSync(
    path.join(api, "server_test.go"),
    'package api\n\nfunc testRoutes() {\n\tmux.HandleFunc("POST /only/in/a/test", nil)\n}\n'
  );
  return labelRoutes(checkout);
}

// The routes the rig reads a label's daemon as serving, which the stand-in answers by and health
// holds every 404 to, read out of that daemon's own source. Catches: a registration read in one
// of its two call shapes alone, a pattern read twice, a test file's registration read as the
// daemon's, the mux's catch-all `/` read as a route, another pattern with no method read as one
// method's, and a package registering none read as a daemon that serves nothing, under which the
// stand-in would answer every request 404 and health would call each the daemon's own answer.
test.skipIf(skip)(
  "the routes reading takes each pattern a HandleFunc or Handle call registers, and refuses what it cannot read",
  () => {
    expect(
      routesOver(
        "both-shapes",
        [
          '\tmux.HandleFunc("POST /legion/v1/claims/turn-outcome", s.turnOutcome)',
          '\tmux.Handle("GET /legion/v1/state", handler)',
          '\tmux.HandleFunc("POST /legion/v1/claims/turn-outcome", s.turnOutcome)',
          '\tmux.HandleFunc("/", notFound)',
          "\tm.Handle(ctx, event)",
        ].join("\n")
      )
    ).toEqual(["GET /legion/v1/state", "POST /legion/v1/claims/turn-outcome"]);
    expect(() => routesOver("methodless", '\tmux.HandleFunc("/legion/v1/state", s.state)')).toThrow(
      'registers ["/legion/v1/state"] with no method'
    );
    expect(() => routesOver("none", "\tm.Handle(ctx, event)")).toThrow(
      "registers no route with a HandleFunc or Handle call or a []route table naming its pattern"
    );
  },
  600_000
);

// Main's daemon since #4400 (53980e0e32) registers its routes from one table, server.go's
// `routes`, a `[]route` whose each element names its pattern first and its handler second, with
// the catch-all `/` its last row, through one `mux.HandleFunc(route.pattern, route.handler)`.
// Catches: a reading of the calls alone, which finds no pattern in that daemon (its one call names
// a field, not a literal) and refused every batch of a label at main 9aff682b; a row's handler, or
// a table of another type, read as a pattern; a keyed row's pattern missed; and the table's
// catch-all read as a route, or a methodless row of it read as one method's.
test.skipIf(skip)(
  "the routes reading takes each pattern a []route table names, as main's daemon since #4400 registers them",
  () => {
    expect(
      routesOver(
        "table",
        [
          "\treturn []route{",
          '\t\t{"GET /legion/v1/state", s.stateRoute},',
          '\t\t{"POST /legion/v1/handoff/complete", s.handoffComplete},',
          '\t\t{handler: s.signOff, pattern: "POST /legion/v1/signoff"},',
          '\t\t{"/", noRoute},',
          "\t}",
          '\tother := []string{"POST /not/a/route"}',
          "\tfor _, route := range s.routes() {",
          "\t\tmux.HandleFunc(route.pattern, route.handler)",
          "\t}",
        ].join("\n")
      )
    ).toEqual([
      "GET /legion/v1/state",
      "POST /legion/v1/handoff/complete",
      "POST /legion/v1/signoff",
    ]);
    expect(() =>
      routesOver("table-methodless", '\treturn []route{\n\t\t{"/legion/v1/state", s.state},\n\t}')
    ).toThrow('registers ["/legion/v1/state"] with no method');
  },
  600_000
);

// Catches: a request matched against a route by its path alone where the log names the method,
// and a `{name}` segment matched literally, which would read an operator route's 404 as no
// daemon's route; and a request whose log names no method (an older stand-in's) refused any route
// of its path.
test("a request is served by a route of its method and of its path, segment by segment", () => {
  const routes = [
    "POST /legion/v1/claims/turn-outcome",
    "POST /legion/v1/operator/claims/{token}/stop",
  ];
  expect(serves(routes, "POST", "/legion/v1/claims/turn-outcome")).toBe(true);
  expect(serves(routes, "GET", "/legion/v1/claims/turn-outcome")).toBe(false);
  expect(serves(routes, undefined, "/legion/v1/claims/turn-outcome")).toBe(true);
  expect(serves(routes, "POST", "/legion/v1/operator/claims/legion-a-b-c/stop")).toBe(true);
  expect(serves(routes, "POST", "/legion/v1/operator/claims//stop")).toBe(false);
  expect(serves(routes, "POST", "/legion/v1/claims/turn-outcome/more")).toBe(false);
  expect(serves(routes, "POST", "/legion/v1/claims")).toBe(false);
});

// The facts cache the stand-in's boot reads through (daemon-pane.ts's `cachedRead`), kept under
// the rig's work directory. Catches: a key that leaves out a source's bytes, under which a
// changed daemon is answered with what its older source said; a hit that runs the program again
// rather than reading the file, which is the boot cost the cache is for; and a key holding the
// checkout's absolute path, under which a second checkout of the same sources reads nothing.
test.skipIf(skip)(
  "a read is kept under a key of its sources, and a changed source is read again",
  () => {
    const kept = process.env.SKILL_SCENARIOS_WORK;
    const cacheWork = path.join(rig, "facts-work");
    process.env.SKILL_SCENARIOS_WORK = cacheWork;
    try {
      const cached = () =>
        readdirSync(path.join(cacheWork, "daemon-facts")).filter((name) => name.endsWith(".out"));
      expect(routesOver("cached", '\tmux.HandleFunc("POST /one", s.one)')).toEqual(["POST /one"]);
      expect(cached()).toHaveLength(1);
      expect(
        routesOver(
          "cached",
          '\tmux.HandleFunc("POST /one", s.one)\n\tmux.HandleFunc("POST /two", s.two)'
        )
      ).toEqual(["POST /one", "POST /two"]);
      const [first, second] = cached();
      expect(second).toBeDefined();
      // The newest answer is the one a hit now reads: overwritten, it is what the read answers.
      const newest = [first, second].find((name) =>
        readFileSync(path.join(cacheWork, "daemon-facts", name ?? ""), "utf8").includes("/two")
      );
      writeFileSync(path.join(cacheWork, "daemon-facts", newest ?? ""), "POST /from/the/file\n");
      const again = path.join(rig, "routes-cached");
      expect(labelRoutes(again)).toEqual(["POST /from/the/file"]);
      const copy = path.join(rig, "routes-cached-copy");
      cpSync(again, copy, { recursive: true });
      expect(labelRoutes(copy)).toEqual(["POST /from/the/file"]);
      expect(cached()).toHaveLength(2);
    } finally {
      if (kept === undefined) delete process.env.SKILL_SCENARIOS_WORK;
      else process.env.SKILL_SCENARIOS_WORK = kept;
    }
  },
  600_000
);

// Catches: the facts cache kept under TMPDIR, where no work directory is named, in a directory
// whose name is the rig's (`skill-scenarios-daemon-facts`): on a Legion pod TMPDIR is the scratch
// every role of the issue shares, and any of them lists it.
test.skipIf(skip)(
  "with no work directory named, a read is kept under TMPDIR in a directory of the world's own name",
  () => {
    const keptWork = process.env.SKILL_SCENARIOS_WORK;
    const keptTmp = process.env.TMPDIR;
    const tmp = path.join(rig, "facts-tmp");
    mkdirSync(tmp, { recursive: true });
    delete process.env.SKILL_SCENARIOS_WORK;
    process.env.TMPDIR = tmp;
    try {
      expect(routesOver("tmp-cached", '\tmux.HandleFunc("POST /one", s.one)')).toEqual([
        "POST /one",
      ]);
      const made = readdirSync(tmp);
      expect(made).toContain("legion-daemon-cache");
      expect(made.filter((name) => nameWords(name, vocabulary([])) !== undefined)).toEqual([]);
    } finally {
      if (keptWork === undefined) delete process.env.SKILL_SCENARIOS_WORK;
      else process.env.SKILL_SCENARIOS_WORK = keptWork;
      if (keptTmp === undefined) delete process.env.TMPDIR;
      else process.env.TMPDIR = keptTmp;
    }
  },
  600_000
);

// How a label's daemon takes a completion (daemon-pane.ts's `labelDaemon`, daemon-completion.go),
// read out of the checkout the label is built from and never by the label's name: the base label
// was main ff2a10ef and is now main at the commit the head's newest forward merge brought in, so a
// table keyed by the name answered every base run as a daemon its checkout no longer has. Each
// file is read in two answers, this checkout's and an older daemon's (main ff2a10ef's among them),
// each written here as that daemon writes it, so no row needs a checkout of the older daemon on
// the host.
/** An engine of this checkout's shape (`recorded`, main's since #4611): `handoff` asks
 * `pastCompletion`, which asks `pastReportOf`, declared in plan.go (`PAST_REPORT`), which asks
 * `leftItsPhase`, which asks `ranUnreported`, which reads `row.Completed()`. */
const ASKING_ENGINE = [
  "func (e *Engine) leftItsPhase(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, token claim.Token) (bool, error) {",
  "\treturn e.ranUnreported(ctx, tx, issue, role, token)",
  "}",
  "",
  "func (e *Engine) ranUnreported(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, token claim.Token) (bool, error) {",
  "\trow, err := e.phaseRow(ctx, tx, issue.Key, role)",
  "\tif err != nil || row.Completed() {",
  "\t\treturn false, err",
  "\t}",
  "\treturn true, nil",
  "}",
  "",
  "func (e *Engine) pastCompletion(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.HandoffComplete, reported phase.Phase) (now, late *intake.Refusal, err error) {",
  "\tpast, err := e.pastReportOf(ctx, tx, issue, fact, reported)",
  "\tif past != leftPhaseReported {",
  "\t\treturn intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key), nil, nil",
  "\t}",
  "\treturn nil, nil, err",
  "}",
  "",
  "func (e *Engine) handoff(ctx context.Context, tx pgx.Tx, fact intake.HandoffComplete) (intake.Result, error) {",
  "\treported := phase.Of(fact.Role, issue.Phase)",
  "\tif reported != issue.Phase {",
  "\t\tif now, _, err := e.pastCompletion(ctx, tx, *issue, fact, reported); err != nil || now != nil {",
  "\t\t\treturn intake.Result{Refusal: now}, err",
  "\t\t}",
  "\t}",
  "\treturn intake.Result{}, nil",
  "}",
].join("\n");
/** The method of main's engine since #4611 that asks `leftItsPhase`, as its workflow/plan.go
 * declares it, beside ASKING_ENGINE. */
const PAST_REPORT = [
  "func (e *Engine) pastReportOf(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.HandoffComplete, reported phase.Phase) (pastReport, error) {",
  "\tleft, err := e.leftItsPhase(ctx, tx, issue, fact.Role, fact.Claim)",
  "\tif err != nil || !left {",
  "\t\treturn noRunReported, err",
  "\t}",
  "\treturn leftPhaseReported, nil",
  "}",
].join("\n");
/** An engine of main's shape before #4611 (`recorded`): `handoff` asks `leftItsPhase`, which reads
 * `row.Completed()`. */
const RECORDED_ENGINE = [
  "func (e *Engine) leftItsPhase(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role) (bool, error) {",
  "\trow, err := e.phaseRow(ctx, tx, issue.Key, role)",
  "\tif err != nil || row.Completed() {",
  "\t\treturn false, err",
  "\t}",
  "\treturn true, nil",
  "}",
  "",
  "func (e *Engine) handoff(ctx context.Context, tx pgx.Tx, fact intake.HandoffComplete) (intake.Result, error) {",
  "\treported := phase.Of(fact.Role, issue.Phase)",
  "\tif reported != issue.Phase {",
  "\t\tleft, err := e.leftItsPhase(ctx, tx, *issue, fact.Role)",
  "\t\tif err != nil {",
  "\t\t\treturn intake.Result{}, err",
  "\t\t}",
  "\t\tif !left {",
  "\t\t\treturn intake.Result{Refusal: intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key)}, nil",
  "\t\t}",
  "\t}",
  "\treturn intake.Result{}, nil",
  "}",
].join("\n");
/** The older daemon's gate (`refused`), as main ff2a10ef's `handoff` writes it. */
const REFUSING_GATE = [
  "\tif RoleFor(issue.Phase) != fact.Role {",
  '\t\treturn refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf("an older daemon refuses the %s in %s of %s", fact.Role, issue.Phase, issue.Key)), nil',
  "\t}",
].join("\n");
/** An engine of the older shape: `handoff` holds the refusing gate and asks no `leftItsPhase`. */
const REFUSED_ENGINE = `func (e *Engine) handoff(ctx context.Context, tx pgx.Tx, fact intake.HandoffComplete) (intake.Result, error) {\n${REFUSING_GATE}\n\treturn intake.Result{}, nil\n}`;
/** A route of this checkout's shape (`left`), in `answerCompletion` as this checkout writes it,
 * beside the entry fence's own `recordBacked`, which is no part of either shape. */
const LEFT_ROUTE = [
  "func (s *server) answerCompletion(w http.ResponseWriter, r *http.Request) {",
  "\tof := phase.Of(grant.Role, issue.Phase)",
  '\tif of == "" {',
  "\t\tof = issue.Phase",
  "\t}",
  "\tword, writer, handoffPhase := phase.Handoff(of)",
  '\tnamesRecord := req.HandoffID != "" || req.HandoffVersion != 0',
  "\trecordBacked := handoffPhase && writer == grant.Role && (of == issue.Phase || namesRecord)",
  "}",
  "",
  "func entryFence(p phase.Phase) bool {",
  "\t_, _, recordBacked := phase.Handoff(p)",
  "\treturn recordBacked",
  "}",
].join("\n");
/** A route of the older shape (`issue`), in `handoffComplete` as main ff2a10ef writes it. */
const ISSUE_ROUTE = [
  "func (s *server) handoffComplete(w http.ResponseWriter, r *http.Request) {",
  "\tword, writer, handoffPhase := phase.Handoff(issue.Phase)",
  "\trecordBacked := handoffPhase && writer == grant.Role",
  "}",
].join("\n");
/** A substitute checkout whose daemon's engine.go holds `engine`, whose api/handoff.go holds
 * `route` and whose workflow/plan.go holds `plan`, each after its package clause; undefined writes
 * no such file. */
function completionOver(name: string, engine?: string, route?: string, plan?: string): string {
  const checkout = path.join(rig, `completion-${name}`);
  const daemon = path.join(checkout, "packages", "daemon", "internal");
  for (const [dir, file, body] of [
    ["workflow", "engine.go", engine],
    ["api", "handoff.go", route],
    ["workflow", "plan.go", plan],
  ] as const) {
    mkdirSync(path.join(daemon, dir), { recursive: true });
    if (body !== undefined)
      writeFileSync(path.join(daemon, dir, file), `package ${dir}\n\n${body}\n`);
  }
  return checkout;
}

// This checkout's own daemon, read out of its own workflow package and handoff.go. Catches: a
// reading that knows the older shape alone, which reads the head's daemon as no daemon, and a
// reading that finds the route in one function by name (the route's statements moved out of
// `handoffComplete` into `answerCompletion`).
test.skipIf(skip)(
  "the completion reading takes this checkout's daemon as recorded and left",
  () => {
    expect(labelDaemon(path.resolve(DAEMON_MODULE, "../.."))).toEqual({
      lateRole: "recorded",
      recordRoute: "left",
    });
  },
  600_000
);

// Each shape of each file, in a checkout holding it and the other file in either shape, so each
// file is read for itself, and main's engine since #4611, whose `leftItsPhase` its plan.go asks.
// Catches: either shape of either file dropped, which reads a checkout holding it as no daemon, a
// reading that takes one file's shape as the other's, and one that reads engine.go's methods alone.
test.skipIf(skip)(
  "the completion reading takes each file in either shape, in either pairing",
  () => {
    for (const [name, engine, route, read] of [
      ["recorded-left", RECORDED_ENGINE, LEFT_ROUTE, { lateRole: "recorded", recordRoute: "left" }],
      ["refused-issue", REFUSED_ENGINE, ISSUE_ROUTE, { lateRole: "refused", recordRoute: "issue" }],
      [
        "recorded-issue",
        RECORDED_ENGINE,
        ISSUE_ROUTE,
        { lateRole: "recorded", recordRoute: "issue" },
      ],
      ["refused-left", REFUSED_ENGINE, LEFT_ROUTE, { lateRole: "refused", recordRoute: "left" }],
    ] as const)
      expect([name, labelDaemon(completionOver(name, engine, route))]).toEqual([name, read]);
    expect(
      labelDaemon(completionOver("asking-left", ASKING_ENGINE, LEFT_ROUTE, PAST_REPORT))
    ).toEqual({ lateRole: "recorded", recordRoute: "left" });
  },
  600_000
);

// A source in neither shape, or in both, is refused, naming the file and what was looked for,
// never read as a shape. Catches: a reading that defaults to one shape where it finds neither or
// both, one that takes a leftItsPhase reading no row.Completed() as `recorded`, one that takes
// main's `pastCompletion` as `recorded` where no method of the package asks `leftItsPhase`, a gate
// refusing with another code as `refused`, and a route whose record-backed rule is neither shape's.
test.skipIf(skip)(
  "the completion reading refuses a file in neither shape or in both, naming it",
  () => {
    const engineShapes =
      'a call of pastCompletion, which calls pastReportOf, which calls leftItsPhase, which calls ranUnreported, which reads row.Completed(), or of leftItsPhase, which reads row.Completed(), each a method of the package (recorded), or `if RoleFor(issue.Phase) != fact.Role { return refused("HANDOFF_NOT_CURRENT_PHASE", …) }` (refused)';
    const routeShapes =
      "`of := phase.Of(grant.Role, issue.Phase); word, writer, handoffPhase := phase.Handoff(of); recordBacked := handoffPhase && writer == grant.Role && (of == issue.Phase || namesRecord)` (left) nor `word, writer, handoffPhase := phase.Handoff(issue.Phase); recordBacked := handoffPhase && writer == grant.Role` (issue)";
    const engine = path.join("workflow", "engine.go");
    const route = path.join("api", "handoff.go");
    /** Each substitute: its name, its engine and route sources, the file the reading refuses and
     * what it says of it. */
    const refusals: readonly (readonly [string, string | undefined, string, string, string])[] = [
      [
        "no-handoff",
        "func (e *Engine) other() {}",
        ISSUE_ROUTE,
        engine,
        "declares no handoff method",
      ],
      [
        "no-gate",
        "func (e *Engine) handoff() (intake.Result, error) {\n\treturn intake.Result{}, nil\n}",
        ISSUE_ROUTE,
        engine,
        `holds neither ${engineShapes}`,
      ],
      [
        "unread-row",
        RECORDED_ENGINE.replace("row.Completed()", "row.Taken()"),
        LEFT_ROUTE,
        engine,
        `holds neither ${engineShapes}`,
      ],
      ["unasked-left", ASKING_ENGINE, LEFT_ROUTE, engine, `holds neither ${engineShapes}`],
      [
        "other-code",
        REFUSED_ENGINE.replace('"HANDOFF_NOT_CURRENT_PHASE"', '"HANDOFF_STALE_ENTRY"'),
        ISSUE_ROUTE,
        engine,
        `holds neither ${engineShapes}`,
      ],
      [
        "both-engines",
        RECORDED_ENGINE.replace("\treported := ", `${REFUSING_GATE}\n\treported := `),
        LEFT_ROUTE,
        engine,
        `holds both ${engineShapes}`,
      ],
      ["no-engine", undefined, LEFT_ROUTE, engine, "is not there, so how the daemon of"],
      [
        "no-route",
        RECORDED_ENGINE,
        LEFT_ROUTE.replace("(of == issue.Phase || namesRecord)", "of == issue.Phase"),
        route,
        `holds neither ${routeShapes} in any function`,
      ],
      [
        "both-routes",
        REFUSED_ENGINE,
        `${LEFT_ROUTE}\n\n${ISSUE_ROUTE}`,
        route,
        "holds more than one shape of the route (left in answerCompletion, issue in handoffComplete)",
      ],
    ];
    for (const [name, engineSource, routeSource, file, says] of refusals) {
      let thrown = "";
      try {
        labelDaemon(completionOver(name, engineSource, routeSource));
      } catch (error) {
        thrown = error instanceof Error ? error.message : String(error);
      }
      expect(thrown).toContain(
        path.join(rig, `completion-${name}`, "packages", "daemon", "internal", file)
      );
      expect(thrown).toContain(says);
    }
  },
  600_000
);

// The facts cache keeps no failed read, and health asks for a run's daemon once a rule a run: the
// tester's fixture of 2026-11-14 (a rig.test.ts row until the architect retired it on
// 2026-11-18), whose built-from named checkouts that were not there, paid 15 `go run`s of
// daemon-completion.go and timed out at 60 s. Each `go` this process starts is a wrapper that
// records the call and runs the real go. Catches: an absent source handed to the program, which
// says nothing a missing file needs a compile to say; a refused source run again in the same
// process; and a refusal kept past a rewrite of its source.
test.skipIf(skip)(
  "the completion reading runs no program for an absent source, and one for a refused source",
  () => {
    const bin = path.join(rig, "completion-go-wrapper");
    mkdirSync(bin, { recursive: true });
    const ran = path.join(bin, "ran");
    writeFileSync(
      path.join(bin, "go"),
      `#!/bin/sh\necho go >>'${ran}'\nexec '${Bun.which("go")}' "$@"\n`,
      { mode: 0o755 }
    );
    const runs = () => (existsSync(ran) ? readFileSync(ran, "utf8").split("\n").length - 1 : 0);
    const keptPath = process.env.PATH;
    const keptWork = process.env.SKILL_SCENARIOS_WORK;
    process.env.PATH = `${bin}:${keptPath ?? ""}`;
    // A cache of the row's own, so the one read that succeeds is no hit of an earlier row's.
    process.env.SKILL_SCENARIOS_WORK = path.join(rig, "completion-go-work");
    try {
      const absent = completionOver("absent-sources");
      for (let read = 0; read < 3; read += 1)
        expect(() => labelDaemon(absent)).toThrow(
          `${path.join(absent, "packages", "daemon", "internal", "workflow", "engine.go")} is not there`
        );
      expect(runs()).toBe(0);
      const gate =
        "func (e *Engine) handoff() (intake.Result, error) {\n\treturn intake.Result{}, nil\n}";
      const refused = completionOver("refused-once", gate, ISSUE_ROUTE);
      for (let read = 0; read < 3; read += 1)
        expect(() => labelDaemon(refused)).toThrow("'s handoff holds neither");
      expect(runs()).toBe(1);
      completionOver("refused-once", REFUSED_ENGINE, ISSUE_ROUTE);
      expect(labelDaemon(refused)).toEqual({ lateRole: "refused", recordRoute: "issue" });
      expect(runs()).toBe(2);
    } finally {
      process.env.PATH = keptPath;
      if (keptWork === undefined) delete process.env.SKILL_SCENARIOS_WORK;
      else process.env.SKILL_SCENARIOS_WORK = keptWork;
    }
  },
  600_000
);

// The reading is kept in the facts cache under a key of the two sources' bytes, as the stand-in's
// boot reads every fact (`cachedRead`), so a label is read once per source and a checkout that
// moves is read anew. Catches: a key that leaves a source out, under which a label whose engine
// changed is answered with the shape it had; a hit that runs the program again; a key holding the
// checkout's path; and a printout that is no late role and record route read as one.
test.skipIf(skip)(
  "the completion reading is kept under both sources, and a changed source is read again",
  () => {
    const kept = process.env.SKILL_SCENARIOS_WORK;
    const cacheWork = path.join(rig, "completion-facts-work");
    process.env.SKILL_SCENARIOS_WORK = cacheWork;
    try {
      const cached = () =>
        readdirSync(path.join(cacheWork, "daemon-facts")).filter((name) => name.endsWith(".out"));
      const checkout = completionOver("cached", RECORDED_ENGINE, LEFT_ROUTE);
      expect(labelDaemon(checkout)).toEqual({ lateRole: "recorded", recordRoute: "left" });
      expect(cached()).toHaveLength(1);
      completionOver("cached", RECORDED_ENGINE, ISSUE_ROUTE);
      expect(labelDaemon(checkout)).toEqual({ lateRole: "recorded", recordRoute: "issue" });
      completionOver("cached", REFUSED_ENGINE, ISSUE_ROUTE);
      expect(labelDaemon(checkout)).toEqual({ lateRole: "refused", recordRoute: "issue" });
      const read = cached();
      expect(read).toHaveLength(3);
      // The newest answer is the one a hit reads, out of the file and in no other place.
      const newest = read.find((name) =>
        readFileSync(path.join(cacheWork, "daemon-facts", name), "utf8").startsWith("refused\n")
      );
      writeFileSync(path.join(cacheWork, "daemon-facts", newest ?? ""), "refused\nleft\n");
      const copy = path.join(rig, "completion-cached-copy");
      cpSync(checkout, copy, { recursive: true });
      expect(labelDaemon(copy)).toEqual({ lateRole: "refused", recordRoute: "left" });
      writeFileSync(path.join(cacheWork, "daemon-facts", newest ?? ""), "refused\nsome route\n");
      expect(() => labelDaemon(checkout)).toThrow(
        'printed "refused\\nsome route\\n", which is no late role (recorded or refused) and record route (left or issue)'
      );
    } finally {
      if (kept === undefined) delete process.env.SKILL_SCENARIOS_WORK;
      else process.env.SKILL_SCENARIOS_WORK = kept;
    }
  },
  600_000
);

// The claim refusals the stand-in answers a refused caller with, read out of the daemon's claim
// source. Catches: a sentence this program writes for itself, and a declaration of another shape
// read as a refusal.
test.skipIf(skip)(
  "the claim refusal reading takes the daemon's own refusal and refuses what it cannot read",
  () => {
    const checkout = path.join(rig, "claim-refusals");
    const claim = path.join(checkout, "packages", "daemon", "internal", "claim");
    mkdirSync(claim, { recursive: true });
    writeFileSync(
      path.join(claim, "wire.go"),
      'package claim\n\nvar (\n\tInvalidSecret = Refusal{Status: http.StatusForbidden, Message: "no such secret here"}\n\tStaleGeneration = refusalOf(409)\n)\n'
    );
    expect(claimRefusal(checkout, "InvalidSecret")).toEqual({
      status: 403,
      message: "no such secret here",
    });
    expect(() => claimRefusal(checkout, "StaleGeneration")).toThrow(
      'which is no Refusal{Status: http.<Status>, Message: "<message>"}'
    );
    expect(() => claimRefusal(checkout, "InvalidBootToken")).toThrow(
      "declares no claim refusal InvalidBootToken"
    );
    expect(claimRefusal(path.resolve(DAEMON_MODULE, "../.."), "InvalidSecret").status).toBe(403);
  },
  600_000
);

// The launch failure limit the stand-in fails a claim's refused turns at, read out of the daemon's
// config package in either of the two shapes the daemon has kept a count's default in: an entry
// of config.go's table of count keys (an older daemon's, main ff2a10ef's) and a row of
// keytable.go's key table (this checkout's, which holds no entry, so no reading of this
// checkout's own reaches the entry's shape). Each shape is read through a substitute checkout of
// the test's own, and this checkout's through its own package.
/** A substitute checkout whose daemon's config package holds each of `files`, a file name mapped
 * to the source after its package clause; a name given again is written over. */
function configOver(name: string, files: Readonly<Record<string, string>>): string {
  const checkout = path.join(rig, `count-defaults-${name}`);
  const config = path.join(checkout, "packages", "daemon", "internal", "config");
  mkdirSync(config, { recursive: true });
  for (const [file, body] of Object.entries(files))
    writeFileSync(path.join(config, file), `package config\n\n${body}\n`);
  return checkout;
}
/** config.go's table of count keys holding `entries`, as main ff2a10ef writes it. */
function countKeys(...entries: string[]): string {
  return `var countKeys = []struct{ key string; defaultValue int }{\n${entries.map((entry) => `\t${entry},\n`).join("")}}`;
}
/** keytable.go's key table holding `rows`, as this checkout writes it. */
function keyTable(...rows: string[]): string {
  return `var topLevelKeys = func() []keyDescriptor {\n\ttable := []keyDescriptor{\n${rows.join("")}\t}\n\treturn table\n}()`;
}
/** The key table's row of the count setting `key`, whose default is `value`. */
function countRow(key: string, value: string): string {
  return `\t\t{\n\t\t\tName: "${key}", RequiredBy: "no",\n\t\t\tDescription: "How many.",\n\t\t\tPositive:    &positiveKey{Value: ${value}, Count: func(c *Config) *int { return &c.Count }},\n\t\t},\n`;
}
/** A key table row of a count kept some other way: a reader and a Default of its own, no Positive. */
const ROUND_CAP_ROW =
  '\t\t{\n\t\t\tName: "review_round_cap", Default: "`3`", RequiredBy: "no",\n\t\t\tRead: reads(roundCap, func(f *fileConfig, v *int) { f.ReviewRoundCap = v }),\n\t\t},\n';

// Catches: the entry's branch dropped, a number this program writes for itself, and an entry of
// another shape read as a default, or none read as a default rather than refused naming both
// shapes.
test.skipIf(skip)(
  "the count default reading takes an older daemon's table entry and refuses what it cannot read",
  () => {
    const checkout = configOver("entry", {
      "config.go": countKeys('{"launch_failure_limit", 7}', '{"prompt_failure_limit", limit}'),
    });
    expect(countDefault(checkout, "launch_failure_limit")).toBe(7);
    expect(() => countDefault(checkout, "prompt_failure_limit")).toThrow(
      "gives prompt_failure_limit a default of limit, which is no integer literal"
    );
    expect(() => countDefault(checkout, "worker_wake_burst")).toThrow(
      'holds no {"worker_wake_burst", <default>, …} entry and no {Name: "worker_wake_burst", …, Positive: &positiveKey{Value: <default>, …}} row'
    );
  },
  600_000
);

// Catches: the row's branch dropped, config.go read alone, a test file's row read as the daemon's
// (its 99 would disagree with the row's 9), and a row naming the key with no Positive read as a
// default rather than refused.
test.skipIf(skip)(
  "the count default reading takes the key table's row out of the package's sources but its tests",
  () => {
    const checkout = configOver("row", {
      "config.go": "type Config struct{ Count int }",
      "keytable.go": keyTable(countRow("launch_failure_limit", "9"), ROUND_CAP_ROW),
      "keytable_test.go": keyTable(countRow("launch_failure_limit", "99")),
    });
    expect(countDefault(checkout, "launch_failure_limit")).toBe(9);
    expect(() => countDefault(checkout, "review_round_cap")).toThrow(
      'holds no {"review_round_cap", <default>, …} entry and no {Name: "review_round_cap", …, Positive: &positiveKey{Value: <default>, …}} row'
    );
  },
  600_000
);

// Catches: a package holding both shapes read as whichever one is found first where they
// disagree, under which the stand-in fails claims at a limit the daemon may not run with, and
// refused where they agree.
test.skipIf(skip)(
  "the count default reading takes a package holding both shapes only where they agree",
  () => {
    const checkout = configOver("both", {
      "config.go": countKeys('{"launch_failure_limit", 4}', '{"prompt_failure_limit", 3}'),
      "keytable.go": keyTable(
        countRow("launch_failure_limit", "4"),
        countRow("prompt_failure_limit", "5")
      ),
    });
    expect(countDefault(checkout, "launch_failure_limit")).toBe(4);
    expect(() => countDefault(checkout, "prompt_failure_limit")).toThrow(
      /gives prompt_failure_limit a default of 3 at \S+\/config\.go:\d+:\d+ and of 5 at \S+\/keytable\.go:\d+:\d+/
    );
  },
  600_000
);

// This checkout's own default, held to the number its key table's row writes, read here by a
// second reading of the row's text. Catches: the reading that refused this checkout's daemon at
// services_up (f7aa520e), which read config.go's entry alone after the defaults moved into the
// key table.
test.skipIf(skip)(
  "the count default reading takes this checkout's own default out of its key table",
  () => {
    const keytable = readFileSync(
      path.join(DAEMON_MODULE, "internal", "config", "keytable.go"),
      "utf8"
    );
    const [, value = ""] =
      /Name: "launch_failure_limit",[^}]*Positive: +&positiveKey\{Value: (\d+),/.exec(keytable) ??
      [];
    expect(value).not.toBe("");
    expect(countDefault(path.resolve(DAEMON_MODULE, "../.."), "launch_failure_limit")).toBe(
      Number(value)
    );
  },
  600_000
);

// The count default is kept in the facts cache under every source of the config package it
// parses (daemon-pane.ts's `cachedRead`). Catches: an answer kept under config.go's bytes alone,
// under which a changed key table is answered with the default it wrote before.
test.skipIf(skip)(
  "a count default is kept under every source of the config package, and a changed key table is read again",
  () => {
    const kept = process.env.SKILL_SCENARIOS_WORK;
    process.env.SKILL_SCENARIOS_WORK = path.join(rig, "count-facts-work");
    try {
      const checkout = configOver("cached", {
        "config.go": "type Config struct{ Count int }",
        "keytable.go": keyTable(countRow("launch_failure_limit", "9")),
      });
      expect(countDefault(checkout, "launch_failure_limit")).toBe(9);
      configOver("cached", { "keytable.go": keyTable(countRow("launch_failure_limit", "11")) });
      expect(countDefault(checkout, "launch_failure_limit")).toBe(11);
    } finally {
      if (kept === undefined) delete process.env.SKILL_SCENARIOS_WORK;
      else process.env.SKILL_SCENARIOS_WORK = kept;
    }
  },
  600_000
);

// worker-pane.ts reads the repository its addressing names, and the branch it gives as the
// default, out of the run's own world (rig.sh's worker world comment), so the rig names neither
// a second time. Catches: a pane built on names of its own, which is the addressing defect
// above in the one file that composes a real run's pane.
test.skipIf(skip)(
  "worker-pane.ts addresses the pane at the world the run's world.json names",
  () => {
    const dir = path.join(rig, "worker-pane");
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        repo: "widgets-org/widgets",
        pr: 1001,
        branch: "legion/LWEVAL-1",
        base: "trunk",
        head: "0".repeat(40),
      })
    );
    // The pane's real gh is the run's recording stand-in, which rig.sh's standins writes here
    // and worker-pane.ts names: the daemon is asked for a pane, so the file has to be there.
    mkdirSync(path.join(dir, "bin"), { recursive: true });
    writeFileSync(path.join(dir, "bin", "gh"), "#!/bin/sh\nexit 0\n", { mode: 0o700 });
    writeFileSync(
      path.join(dir, "base.env"),
      Object.entries({ ...inherited, HOME: path.join(dir, "home") })
        .map(([key, value]) => `${key}=${value}\n`)
        .join("")
    );
    const built = spawnChecked([
      "bun",
      WORKER_PANE,
      dir,
      path.join(dir, "base.env"),
      "13399",
      "rig-profile",
      "skillsrig",
      "LWEVAL-1",
      "tester",
      path.resolve(import.meta.dir, "../../../.."),
    ]);
    expect(built.stderr.toString()).toBe("");
    expect(built.exitCode).toBe(0);
    expect(systemPromptOf(dir)?.text).toContain(
      "Your issue's repository is widgets-org/widgets (default branch trunk)"
    );
    expect(readFileSync(path.join(dir, "worker.env"), "utf8")).toContain(
      `GH_CONFIG_DIR=${dir}/state/secrets/legion-skillsrig-lweval-1-tester-gh/widgets-org\n`
    );
  },
  600_000
);

// And a world that names no branch at all is refused there too, by the same parse: the row above
// covers the branch the pane is given, this one the branch no world holds. Catches: `base` given
// a default of its own (`main`), where a world written without it would compose a pane addressed
// at a branch the world never named and a worker would follow it — the addressing defect the row
// above catches, arriving through the reader rather than through the caller. The refusal is the
// world parse, which runs before the daemon is asked for anything, so this row needs no go.
test("worker-pane.ts refuses a world that names no default branch", () => {
  const dir = path.join(rig, "worker-pane-no-base");
  mkdirSync(dir, { recursive: true });
  writeFileSync(
    path.join(dir, "world.json"),
    JSON.stringify({
      key: "LWEVAL-1",
      repo: "widgets-org/widgets",
      pr: 1001,
      branch: "legion/LWEVAL-1",
      head: "0".repeat(40),
    })
  );
  writeFileSync(
    path.join(dir, "base.env"),
    Object.entries({ ...inherited, HOME: path.join(dir, "home") })
      .map(([key, value]) => `${key}=${value}\n`)
      .join("")
  );
  const built = spawnChecked([
    "bun",
    WORKER_PANE,
    dir,
    path.join(dir, "base.env"),
    "13399",
    "rig-profile",
    "skillsrig",
    "LWEVAL-1",
    "tester",
    path.resolve(import.meta.dir, "../../../.."),
  ]);
  expect(built.stderr.toString()).toContain(`${dir}/world.json is no world`);
  expect(built.exitCode).toBe(2);
  expect(existsSync(path.join(dir, "worker.env"))).toBe(false);
});

// And a world.json that is no JSON at all is refused the same way, by the one reader every
// program of the run reads the world with (gh-call.ts's `worldOf`). Catches: a reader whose
// `JSON.parse` throws uncaught, which ends the program with a stack trace and exit 1 rather than
// the documented "is no world" and 2 the gh stand-in gives for the same file — a reader of the
// run's output then has a crash where it should have the file and the program that stopped.
test("worker-pane.ts refuses a world.json that is no JSON, naming the file", () => {
  const dir = path.join(rig, "worker-pane-no-json");
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, "world.json"), "{not json");
  writeFileSync(path.join(dir, "base.env"), "");
  const built = spawnChecked([
    "bun",
    WORKER_PANE,
    dir,
    path.join(dir, "base.env"),
    "13399",
    "rig-profile",
    "skillsrig",
    "LWEVAL-1",
    "tester",
    path.resolve(import.meta.dir, "../../../.."),
  ]);
  expect(built.stderr.toString()).toStartWith(`worker-pane.ts: ${dir}/world.json is no world: `);
  expect(built.exitCode).toBe(2);
  expect(existsSync(path.join(dir, "worker.env"))).toBe(false);
});

// The label's build of daemon-pane.go (rig.sh's daemon_pane_binary) and the facts services_up
// reads through it before any run starts (rig.sh's label_facts, `bun daemon-pane.ts facts`).
describe("the label's build and its facts", () => {
  const FACTS = bundled(path.join(import.meta.dir, "daemon-pane.ts"));
  const STANDIN = bundled(path.join(import.meta.dir, "daemon-standin.ts"));
  const CHECKOUT = path.resolve(import.meta.dir, "../../../..");
  const BOOTS = [
    "tester",
    "reviewer",
    "implementer:reviewing",
    "implementer:accepting",
    "reviewer:reviewing",
    "reviewer:accepting",
  ];
  /** A stand-in for a build of daemon-pane.go: it answers each reading the stand-in's facts
   * need, in the program's own printout, and keeps each command it ran for under <dir>/calls.
   * `refuse`, a command and its first argument, is answered as the program answers a reading it
   * cannot make. */
  function fakeBuild(dir: string, refuse = ""): string {
    mkdirSync(dir, { recursive: true });
    const build = path.join(dir, "daemon-pane");
    writeFileSync(
      build,
      [
        "#!/bin/sh",
        `printf '%s\\n' "$*" >>"${path.join(dir, "calls")}"`,
        `[ "$1 $2" != "${refuse}" ] || { echo "daemon-pane $1: refused by the row" >&2; exit 1; }`,
        "case $1 in",
        "phase) case $2 in tester) printf 'testing\\ntest\\n' ;; reviewer) printf 'reviewing\\nreview\\n' ;; implementer) printf 'implementing\\nimplement\\n' ;; *) exit 1 ;; esac ;;",
        "claim-refusal) printf '403\\nrefused as %s\\n' \"$2\" ;;",
        "count-default) echo 3 ;;",
        'refusal) printf \'HANDOFF_NOT_CURRENT_PHASE\\nthe %s may not finish %s of %s\\n\' "$3" "$4" "$5" ;;',
        "*) exit 1 ;;",
        "esac",
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    return build;
  }
  /** Each command the build at `dir` ran for, its first two words. */
  const calls = (dir: string) =>
    existsSync(path.join(dir, "calls"))
      ? readFileSync(path.join(dir, "calls"), "utf8")
          .split("\n")
          .filter((line) => line !== "")
          .map((line) => line.split(" ").slice(0, 2).join(" "))
      : [];
  /** `bun daemon-pane.ts facts` of `checkout` (this one) for every boot of BOOTS, under `env`. */
  const facts = (env: Record<string, string>, checkout = CHECKOUT) =>
    spawnChecked(["bun", FACTS, "facts", checkout, ...BOOTS], {
      env: { PATH: process.env.PATH ?? "", HOME: rig, ...env },
    });

  // Catches: a read that compiles the daemon module with `go run` although the rig named a build,
  // which is the boot cost the build is for; a build run outside the module or without its
  // arguments or input; and a named build that is not there answered by `go run` in its place.
  test("a read runs the build SKILL_SCENARIOS_DAEMON_PANE names, in the module, and a build that is not there is an error", () => {
    const dir = path.join(rig, "named-build");
    mkdirSync(dir, { recursive: true });
    const build = path.join(dir, "daemon-pane");
    writeFileSync(build, '#!/bin/sh\nprintf "%s|%s|" "$(pwd -P)" "$*"\ncat\n', { mode: 0o755 });
    const kept = process.env.SKILL_SCENARIOS_DAEMON_PANE;
    try {
      process.env.SKILL_SCENARIOS_DAEMON_PANE = build;
      expect(runDaemonPane(DAEMON_MODULE, ["pane"], '{"role":"tester"}')).toBe(
        `${realpathSync(DAEMON_MODULE)}|pane|{"role":"tester"}`
      );
      process.env.SKILL_SCENARIOS_DAEMON_PANE = path.join(dir, "gone");
      expect(() => runDaemonPane(DAEMON_MODULE, ["phase", "tester"])).toThrow(
        `SKILL_SCENARIOS_DAEMON_PANE names ${path.join(dir, "gone")}, which is not there`
      );
    } finally {
      if (kept === undefined) delete process.env.SKILL_SCENARIOS_DAEMON_PANE;
      else process.env.SKILL_SCENARIOS_DAEMON_PANE = kept;
    }
  });

  // Catches: a facts read that leaves a fact some stand-in of worker_boots reads at its boot out
  // of the cache, one that runs a reading twice, and one made without a build or a work directory,
  // which would compile the daemon module per fact or keep the facts where no stand-in reads them.
  test.skipIf(skip)(
    "facts reads every fact of every boot through the build, once, into the work directory's cache",
    () => {
      const work = path.join(rig, "facts-every-boot");
      const build = fakeBuild(path.join(rig, "facts-every-boot-build"));
      const env = { SKILL_SCENARIOS_WORK: work, SKILL_SCENARIOS_DAEMON_PANE: build };
      expect(facts(env)).toMatchObject({ exitCode: 0 });
      expect(calls(path.dirname(build)).sort()).toEqual(
        [
          "count-default launch_failure_limit",
          "claim-refusal InvalidSecret",
          "claim-refusal InvalidBootToken",
          "claim-refusal StaleGeneration",
          "phase tester",
          "phase reviewer",
          "refusal not-current-phase",
          "phase implementer",
          "refusal not-current-phase",
        ].sort()
      );
      const kept = readdirSync(path.join(work, "daemon-facts")).sort();
      // A second read reads each out of the cache and runs nothing.
      expect(facts(env)).toMatchObject({ exitCode: 0 });
      expect(calls(path.dirname(build))).toHaveLength(9);
      expect(readdirSync(path.join(work, "daemon-facts")).sort()).toEqual(kept);
      for (const [unset, says] of [
        ["SKILL_SCENARIOS_DAEMON_PANE", "names no build of daemon-pane.go"],
        ["SKILL_SCENARIOS_WORK", "names no work directory"],
      ] as const) {
        const read = facts({ ...env, [unset]: "" });
        expect(read.exitCode).toBe(2);
        expect(read.stderr.toString()).toContain(`daemon-pane facts: ${unset} ${says}`);
      }
    },
    600_000
  );

  // Catches: a fact that cannot be read passed over, to be met by the run whose stand-in boots
  // on it.
  test.skipIf(skip)(
    "a fact that cannot be read fails the read, naming the boot and the fact",
    () => {
      const build = fakeBuild(path.join(rig, "facts-refused-build"), "phase reviewer");
      const read = facts({
        SKILL_SCENARIOS_WORK: path.join(rig, "facts-refused"),
        SKILL_SCENARIOS_DAEMON_PANE: build,
      });
      expect(read.exitCode).toBe(1);
      expect(read.stderr.toString()).toContain(
        `daemon-pane facts: the reviewer's stand-in: the daemon fact \`phase reviewer\` of ${CHECKOUT} could not be read: daemon-pane phase reviewer in ${CHECKOUT}/packages/daemon exited 1: daemon-pane phase: refused by the row`
      );
    },
    600_000
  );

  /** A checkout under the row's directory holding what facts reads, copied from this one as
   * gh-standin.test.ts's per-label rows copy the wrapper: the daemon's API package and engine.go,
   * the other files the source shapes are read from (labelShapes), and the wrapper's package,
   * internal/ghconfig, internal/leakguard (whose words the wrapper refuses an owner in) and saved
   * gh help, with `edit` applied to the wrapper's gh_help.go: a later wrapper's own source. */
  function factsCheckout(at: string, edit: (source: string) => string): string {
    const checkout = path.join(rig, at);
    const daemon = path.join(checkout, "packages", "daemon");
    for (const dir of ["internal/api", "cmd/legion", "internal/ghconfig", "internal/leakguard"]) {
      mkdirSync(path.join(daemon, dir), { recursive: true });
      for (const name of readdirSync(path.join(DAEMON_MODULE, dir)))
        if (name.endsWith(".go") && !name.endsWith("_test.go"))
          cpSync(path.join(DAEMON_MODULE, dir, name), path.join(daemon, dir, name));
    }
    for (const file of [
      "internal/workflow/engine.go",
      "internal/workflow/effects.go",
      "internal/workflow/review_round.go",
      "internal/workflow/review.go",
      "internal/intake/facts.go",
      "internal/reviewround/reviewround.go",
      "internal/dispatch/client.go",
      "internal/dispatch/types.go",
      "internal/record/outbox.go",
      "internal/daemon/outbox.go",
      "internal/workflow/steps.go",
      "internal/workflow/plan.go",
      "internal/workflow/line.go",
      "internal/plan/plan.go",
      "internal/record/plan.go",
      "internal/config/stage3.go",
      "internal/phase/phase.go",
      "cmd/legion/testdata/gh-help",
    ]) {
      mkdirSync(path.dirname(path.join(daemon, file)), { recursive: true });
      cpSync(path.join(DAEMON_MODULE, file), path.join(daemon, file), { recursive: true });
    }
    const routes = path.join("packages", "envoy", "internal", "dispatch", "api", "routes_table.go");
    mkdirSync(path.dirname(path.join(checkout, routes)), { recursive: true });
    cpSync(path.join(DAEMON_MODULE, "..", "..", routes), path.join(checkout, routes));
    const help = path.join(daemon, "cmd", "legion", "gh_help.go");
    writeFileSync(help, edit(readFileSync(help, "utf8")));
    return checkout;
  }

  // The gh selectors facts reads once every boot's facts are read (daemon-pane.ts's
  // labelGhSelectors), by which the gh stand-in of every run of the label refuses an owner. The
  // checkout is factsCheckout's, with an option kind the world's gh does not model added
  // to the wrapper's table. Catches: a facts read that passes over the label's gh selectors,
  // which leaves that form to worker_boot to refuse run by run, after the batch started.
  test.skipIf(skip)(
    "a selector form the label's wrapper holds that the world does not model fails the read, naming it",
    () => {
      const table = "var ghOptionKinds = map[string]string{\n";
      const checkout = factsCheckout("facts-unmodelled-checkout", (source) => {
        expect(source).toContain(table);
        return source.replace(table, `${table}\t"--mirror string": "mirror",\n`);
      });
      const read = facts(
        {
          SKILL_SCENARIOS_WORK: path.join(rig, "facts-unmodelled"),
          SKILL_SCENARIOS_DAEMON_PANE: fakeBuild(path.join(rig, "facts-unmodelled-build")),
        },
        checkout
      );
      expect(read.exitCode).toBe(1);
      expect(read.stderr.toString()).toContain(
        `daemon-pane facts: the gh stand-in: the wrapper's table holds the option kind "mirror" (ghOptionKinds --mirror string), which the world's gh does not model (gh-selectors.ts's MODELLED)`
      );
    },
    600_000
  );

  // The wrapper's ghFlagLine has had two shapes, 4 capture groups before #4418 and 5 since
  // (gh-selectors.ts's optionLines); here a third, the pattern of #4418 with an empty group added
  // at its end, which reads every option line as that one does. Catches: a pattern of a shape the
  // world does not read taken as one it does (a reader taking any count of 5 or more as five),
  // which reads the help's options by the wrong groups and says nothing; a refusal left to
  // worker_boot, inside each run, rather than made by facts, which label_facts runs at services_up
  // before any run of the batch starts; and a refusal that names neither the file the pattern was
  // read from nor the counts the world reads.
  test.skipIf(skip)(
    "a ghFlagLine of neither shape the world reads fails the read, naming its file, its count and the two the world reads",
    () => {
      const six = String.raw`^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(\[=true\|false\])?(?: (\S+))?(?:\s{2,}(.*)|\s*)()$`;
      const declaration = /^(\s*ghFlagLine\s*=\s*regexp\.MustCompile\()`[^`]*`\)/m;
      const checkout = factsCheckout("facts-six-groups-checkout", (source) => {
        expect(source).toMatch(declaration);
        return source.replace(declaration, (_, opening: string) => `${opening}\`${six}\`)`);
      });
      const read = facts(
        {
          SKILL_SCENARIOS_WORK: path.join(rig, "facts-six-groups"),
          SKILL_SCENARIOS_DAEMON_PANE: fakeBuild(path.join(rig, "facts-six-groups-build")),
        },
        checkout
      );
      const file = path.join(checkout, "packages", "daemon", "cmd", "legion", "gh_help.go");
      expect({ code: read.exitCode, stderr: read.stderr.toString() }).toEqual({
        code: 1,
        stderr: `daemon-pane facts: the gh stand-in: the wrapper's ghFlagLine ${JSON.stringify(six)}, read from ${file}, has 6 capture groups, which is neither shape of its option line this reads: 4 before #4418 (shorthand, name, placeholder, description), or 5 since (the boolean marker after the name third)\n`,
      });
    },
    600_000
  );

  /** The daemon stand-in of a reviewer run of the head label on `work`'s facts cache, booted as
   * rig.sh's worker_boot boots it but with no go on its PATH and a build that refuses every
   * reading: its exit code and output once it listened or exited. */
  async function bootOn(work: string, name: string) {
    const dir = path.join(rig, name);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "boot-token"), `${fixtureSecret("boot")}\n`);
    writeFileSync(path.join(dir, "built-from"), `checkout ${CHECKOUT}\n`);
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        repo: WORLD_REPO,
        base: "main",
        branch: "legion/LWEVAL-1",
        pr: 1001,
        head: "0".repeat(40),
      })
    );
    const refusing = path.join(dir, "daemon-pane");
    writeFileSync(refusing, '#!/bin/sh\necho "no read was to run" >&2\nexit 1\n', { mode: 0o755 });
    const nothing = path.join(dir, "no-go");
    mkdirSync(nothing);
    const child = Bun.spawn(
      [
        process.execPath,
        STANDIN,
        "0",
        path.join(dir, "daemon.log"),
        path.join(dir, "boot-token"),
      ].concat(["skills1", "LWEVAL-1", dir, "reviewer"]),
      {
        env: {
          PATH: nothing,
          HOME: rig,
          SKILL_SCENARIOS_WORK: work,
          SKILL_SCENARIOS_DAEMON_PANE: refusing,
        },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    const reader = child.stdout.getReader();
    let printed = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      printed += new TextDecoder().decode(value);
      if (printed.includes("listening on")) break;
    }
    child.kill();
    const code = await child.exited;
    return { printed, code, stderr: await new Response(child.stderr).text() };
  }

  // The boot of a stand-in after services_up read its label's facts: every fact out of a file, so
  // a boot needs neither go nor the build to listen. Catches: a boot that reads a fact the read
  // before it left out, and a batch that drops that read, whose boots then compile the daemon
  // module per fact, 6-7 s each with a warm Go cache, past worker_boot's 30 s bound.
  test.skipIf(skip)(
    "a stand-in boots on the facts read before it, running no go and no build, and fails on an empty cache",
    async () => {
      const work = path.join(rig, "facts-boot");
      const build = fakeBuild(path.join(rig, "facts-boot-build"));
      expect(
        facts({ SKILL_SCENARIOS_WORK: work, SKILL_SCENARIOS_DAEMON_PANE: build })
      ).toMatchObject({ exitCode: 0 });
      const read = await bootOn(work, "reviewer-head-1");
      expect(read.printed).toContain("listening on http://127.0.0.1:");
      expect(read.stderr).toBe("");
      const empty = await bootOn(path.join(rig, "facts-boot-empty"), "reviewer-head-2");
      expect(empty.printed).not.toContain("listening on");
      expect(empty.code).toBe(2);
      expect(empty.stderr).toContain(
        `daemon-standin: the daemon fact \`routes\` of ${CHECKOUT} could not be read`
      );
    },
    600_000
  );
});

// The stand-in answers a label's daemon by what its source says it posts, refuses, publishes,
// ends a reviewer's task with and takes a review for, each read as one of two shapes
// (labelShapes): main's since the change that brought it, by its own marker, or main `ff2a10ef`'s,
// which predates it, by the code it has in its place. A marker it cannot find refuses the batch,
// naming the marker and its file, and is never read as an older daemon (the architect's ruling of
// 2026-11-18). Each source here is a copy of the twenty-eight files the readers read, so a row
// reads them and nothing else.
describe("the shapes of a label's daemon source", () => {
  const CHECKOUT = path.resolve(import.meta.dir, "../../../..");
  /** The files labelShapes reads, under a checkout's `packages/`. */
  const SOURCES = [
    "daemon/internal/api/handoff.go",
    "daemon/internal/api/claims.go",
    "daemon/internal/api/storable_text.go",
    "daemon/internal/api/state.go",
    "daemon/internal/api/architect.go",
    "daemon/internal/record/store.go",
    "daemon/internal/workflow/engine.go",
    "daemon/internal/workflow/effects.go",
    "daemon/internal/workflow/review_round.go",
    "daemon/internal/workflow/review.go",
    "daemon/internal/intake/facts.go",
    "daemon/internal/reviewround/reviewround.go",
    "daemon/internal/dispatch/client.go",
    "daemon/internal/record/outbox.go",
    "daemon/internal/daemon/outbox.go",
    "daemon/internal/dispatch/types.go",
    "daemon/internal/workflow/steps.go",
    "daemon/internal/workflow/plan.go",
    "envoy/internal/dispatch/api/routes_table.go",
    "daemon/internal/api/server.go",
    "daemon/internal/api/shaping.go",
    "daemon/internal/api/version.go",
    "daemon/internal/api/updatestate.go",
    // Main's since #4611 reads the step a run stands at off the walk of its run.
    "daemon/internal/workflow/line.go",
    "daemon/internal/plan/plan.go",
    "daemon/internal/record/plan.go",
    "daemon/internal/config/stage3.go",
    "daemon/internal/phase/phase.go",
  ];
  const EFFECTS = "daemon/internal/workflow/effects.go";
  const ROUND_WORDS = "daemon/internal/reviewround/reviewround.go";
  const REVIEW = "daemon/internal/workflow/review.go";
  const FACTS = "daemon/internal/intake/facts.go";
  let copies = 0;
  /** A checkout holding SOURCES of `from` alone, with `marker` written as `renamed` in `file`,
   * which must hold it. */
  function copyOf(from: string, file = "", marker = "", renamed = ""): string {
    return copyWith(from, file === "" ? [] : [[file, marker, renamed]]);
  }
  /** A checkout holding SOURCES of `from` alone, with each edit `[file, marker, renamed]` of
   * `edits` made in turn, each file holding its marker when its edit is made. */
  function copyWith(from: string, edits: readonly (readonly [string, string, string])[]): string {
    const dir = path.join(rig, `source-shapes-${++copies}`);
    for (const one of SOURCES) {
      const source = path.join(from, "packages", one);
      // main ff2a10ef predates review_round.go, reviewround.go, storable_text.go, plan.go,
      // shaping.go and updatestate.go, so a copy of it has none of them.
      if (!existsSync(source)) continue;
      let text = readFileSync(source, "utf8");
      for (const [file, marker, renamed] of edits) {
        if (one !== file) continue;
        expect(text).toContain(marker);
        text = text.replaceAll(marker, renamed);
      }
      mkdirSync(path.dirname(path.join(dir, "packages", one)), { recursive: true });
      writeFileSync(path.join(dir, "packages", one), text);
    }
    return dir;
  }
  // The words of the decoder's refusal of a body holding U+0000: main's since #4463, and #4488's
  // before them (daemon-pane.ts's `NUL_REFUSAL`, read by `refusesNul`).
  const NUL_REFUSED =
    "%s holds a NUL character (U+0000) at character %d, which the daemon API refuses rather than store it as U+FFFD";
  const NUL_UNSTORABLE =
    "%s holds a NUL character (U+0000) at character %d, which the daemon cannot store";
  /** The contract this checkout's daemon declares (its api's version.go), the one its answers
   * state. */
  const DAEMON_API_VERSION = Number(
    /^const DaemonAPIVersion = (\d+)$/m.exec(
      readFileSync(path.join(CHECKOUT, "packages", "daemon/internal/api/version.go"), "utf8")
    )?.[1]
  );
  const MAIN = {
    postsBackward: true,
    refusesReason: true,
    publishesHolders: true,
    namesReviewRound: true,
    decidesBySession: true,
    decidesByFooterHead: true,
    writesSteps: true,
    refusesRating: true,
    nulRefusal: NUL_REFUSED,
    servesFixAttempts: false,
    statesContract: DAEMON_API_VERSION,
    retiresReady: true,
    repositoriesLive: true,
  };
  const OLDER = {
    postsBackward: false,
    refusesReason: false,
    publishesHolders: false,
    namesReviewRound: false,
    decidesBySession: false,
    decidesByFooterHead: false,
    writesSteps: false,
    refusesRating: false,
    nulRefusal: undefined,
    servesFixAttempts: true,
    statesContract: undefined,
    retiresReady: false,
    repositoriesLive: false,
  };
  const olderHost = process.env.SKILL_SCENARIOS_OLDER_CHECKOUT;

  // Catches: a reader whose marker is not this checkout's source (gone stale), read as an older
  // daemon that lacks the behaviour, which the head label's runs met as no sentence, no refusal,
  // no publication, a reviewer's task with no sentence of its round, a review of the review App
  // taken from any session and one taken on its `commit_id`, no step record, a rating taken where
  // the daemon refuses it, a body holding U+0000 taken or refused in #4488's words, a pull
  // request counting fix attempts main's daemon keeps no count of, answers stating no contract
  // and a completion's `ready: true` taken; and a reader that needs a file the twenty-eight are
  // not.
  test("this checkout's daemon source posts a backward move's sentence, refuses a reason, publishes the role holders, names the review round, takes the reviewer's session alone, holds a review to its footer's head, records each step, refuses an unasked rating and a body holding U+0000 in main's words since #4463, counts no fix attempt, states its contract and refuses ready", () => {
    expect(DAEMON_API_VERSION).toBeGreaterThan(188);
    expect(labelShapes(CHECKOUT)).toEqual(MAIN);
    expect(labelShapes(copyOf(CHECKOUT))).toEqual(MAIN);
  });

  // Catches: a step record written in a shape the label's daemon does not write, which `dispatch
  // steps` then lists otherwise than production (main's since #4400). Each text the stand-in and
  // the seeds restate is held to the label's source: a field the executor stops sending, a write
  // that sends another body, a Dispatch older than the route no longer waited for, a phase whose
  // step moves or whose type changes (main's steps of a phase since #4488, read off the walk of
  // its run since #4611), a result a move gives otherwise, and an id named another way each leave
  // the source of no shape, refused naming the reader, never read as a daemon that writes none.
  test("a daemon source that writes a step record otherwise than step-records.ts restates is of no shape", () => {
    for (const [file, marker, renamed] of [
      [
        "daemon/internal/daemon/outbox.go",
        "ReturnedTo: payload.ReturnedTo, Report: payload.Report}",
        "ReturnedTo: payload.ReturnedTo}",
      ],
      [
        "daemon/internal/daemon/outbox.go",
        "r.dispatch.PutStep(ctx, row.Issue, payload.ID, stepWriteOf(payload))",
        "r.dispatch.PutStep(ctx, row.Issue, payload.ID, stepWriteOf(record.StepRecord{}))",
      ],
      [
        "daemon/internal/daemon/outbox.go",
        "if waitsForTheRoute(row.Kind) && dispatch.NoRoute(err) {",
        "if dispatch.NoRoute(err) && false {",
      ],
      [
        "daemon/internal/daemon/outbox.go",
        "return record.LaneOf(kind) == record.LaneRecord || kind == record.OutboxKindRoleHoldersPublish",
        "return kind == record.OutboxKindRoleHoldersPublish",
      ],
      [
        "daemon/internal/config/stage3.go",
        '"planner", "implementer", "tester", "reviewer", "acceptance:owner"',
        '"planner", "tester", "implementer", "reviewer", "acceptance:owner"',
      ],
      [
        "daemon/internal/plan/plan.go",
        '{Name: "reviewer", Type: TypeReview, Phase: phase.Reviewing}',
        '{Name: "reviewer", Type: TypeTesting, Phase: phase.Reviewing}',
      ],
      ["daemon/internal/workflow/line.go", "Position: at + 1}", "Position: at}"],
      [
        "daemon/internal/workflow/steps.go",
        'case record.PlaceRetro:\n\t\treturn "implementer", "", true',
        'case record.PlaceRetro:\n\t\treturn "architect", "", true',
      ],
      [
        "daemon/internal/phase/phase.go",
        "var Order = []Phase{Planning, Implementing,",
        "var Order = []Phase{Implementing, Planning,",
      ],
      [
        "daemon/internal/workflow/steps.go",
        "case TriggerTesterFailed, TriggerReviewRejected, TriggerBackward:",
        "case TriggerTesterFailed, TriggerReviewRejected:",
      ],
      [
        "daemon/internal/workflow/steps.go",
        '[]byte("legion-step-record")',
        '[]byte("legion-step")',
      ],
    ] as const)
      expect(() => labelShapes(copyOf(CHECKOUT, file, marker, renamed))).toThrow(
        "is no shape of the record of each run of a step this rig reads"
      );
  });

  // Catches: a reviewer's task whose end is not the daemon's two sentences, word for word as
  // reviewround.go's `Sentences` writes them (#4328): `none` where the row holds no commit or no
  // entry, and the commit and the number where it holds them. The base label, at main 9aff682b,
  // tells its reviewers the same as the head's; a reader that wrote nothing left both labels'
  // reviewers with no entry to put in their footers.
  test("a reviewer's task ends with the daemon's two sentences of its round, in its words", () => {
    expect(reviewRoundSentences(CHECKOUT, "", 1)).toBe(
      " Last reviewed commit: none. Review entry: 1."
    );
    expect(reviewRoundSentences(copyOf(CHECKOUT), "a".repeat(40), 3)).toBe(
      ` Last reviewed commit: ${"a".repeat(40)}. Review entry: 3.`
    );
    expect(reviewRoundSentences(CHECKOUT, "", 0)).toBe(
      " Last reviewed commit: none. Review entry: none."
    );
  });

  // Catches: an older daemon's reviewer told the sentences main `ff2a10ef`'s daemon never wrote.
  test.skipIf(olderHost === undefined)(
    "main ff2a10ef's daemon ends a reviewer's task with no sentence of its round",
    () => {
      expect(reviewRoundSentences(olderHost ?? "", "", 1)).toBe("");
    }
  );

  // Catches: an older daemon answered with this checkout's sentences, refusal, publication, round
  // or footer's head, which main `ff2a10ef`'s never gave; and an older shape's marker that is not
  // that daemon's code.
  test.skipIf(olderHost === undefined)(
    "main ff2a10ef's daemon source reads as that older daemon's: no sentence, no refusal of a reason, no publication, no round named, no footer's head read",
    () => {
      expect(labelShapes(olderHost ?? "")).toEqual(OLDER);
      expect(labelShapes(copyOf(olderHost ?? ""))).toEqual(OLDER);
    }
  );

  // The texts that read how a review of the review App is held to its footer's head, main's since
  // #2539 (daemon-pane.ts's `BY_FOOTER_HEAD`), where every daemon the rig runs finds the footer
  // (`FOOTER_FOUND`), and main `ff2a10ef`'s, which records a review on its `commit_id`
  // (`BY_COMMIT_ID`).
  const FOOTER_MARKER = 'const legionFooterMarker = "<!-- legion:"';
  const FOOTER_LAST = "at := strings.LastIndex(r.Body, legionFooterMarker)";
  const FOOTER_END = 'end := strings.Index(rest, "-->")';
  const FOOTER_STRUCT =
    'type legionFooter struct {\n\tSession string          `json:"session"`\n\tHead    string          `json:"head"`\n\tEntry   json.RawMessage `json:"entry"`\n}';
  const FOOTER_DECODED =
    "var footer legionFooter\n\tif err := json.Unmarshal([]byte(rest[:end]), &footer); err != nil {\n\t\treturn legionFooter{}\n\t}";
  // A footer's head read as a commit, 40 lowercase hexadecimal characters: through intake's own
  // `fullCommitSHA`, main's since #2539, and through reviewround's `IsHead`, which #3995
  // (b537812fb5) moved that test into unchanged (daemon-pane.ts's `HEAD_SPELLED`).
  const HEAD_WHOLE =
    'head := r.lastLegionFooter().Head\n\tif !fullCommitSHA(head) {\n\t\treturn ""\n\t}';
  const FORTY = "\tif len(s) != 40 {\n\t\treturn false\n\t}";
  const LOWER_HEX = "if (c < '0' || c > '9') && (c < 'a' || c > 'f') {";
  const HEAD_IS =
    'head := r.lastLegionFooter().Head\n\tif !reviewround.IsHead(head) {\n\t\treturn ""\n\t}';
  const FORTY_IS = "\tif len(commit) != 40 {\n\t\treturn false\n\t}";
  const LOWER_HEX_IS =
    "for _, c := range []byte(commit) {\n\t\tif (c < '0' || c > '9') && (c < 'a' || c > 'f') {";
  const NO_HEAD = 'switch head := review.LegionHead(); {\n\tcase head == "":';
  const OTHER_HEAD = "\tcase head != pr.HeadSHA:";
  const RECORDED_ON =
    "reviewer.Decision = &record.ReviewDecision{State: state, Body: fact.Body, Head: e.reviewHead(fact)}";
  const FOOTER_HEAD_KEPT =
    "if e.byReviewApp(review.Author) {\n\t\treturn review.LegionHead()\n\t}\n\treturn review.CommitID";
  const SESSION_ALONE =
    'var footer struct {\n\t\tSession string `json:"session"`\n\t}\n\tif err := json.Unmarshal([]byte(rest[:end]), &footer); err != nil {';
  const ON_COMMIT_ID =
    "reviewer.Decision = &record.ReviewDecision{State: state, Body: fact.Body, Head: fact.CommitID}";
  // The route's verdict rule, every daemon's the rig runs, in the writer of main's since #3659 and
  // in the one before it (daemon-pane.ts's `verdictRule`, `FAILURE_WRITERS`).
  const TESTER_VERDICT =
    'if grant.Role == claim.RoleTester && req.Verdict != "pass" && req.Verdict != "fail" {\n\t\ts.writeFailure(w, r, http.StatusBadRequest, "TESTER_VERDICT_REQUIRED", "tester verdict must be pass or fail")';
  const OTHER_VERDICT =
    'if grant.Role != claim.RoleTester && req.Verdict != "" {\n\t\ts.writeFailure(w, r, http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN", "only a tester may report a verdict")';
  const TESTER_VERDICT_BEFORE = TESTER_VERDICT.replace("s.writeFailure(w, r, ", "writeFailure(w, ");
  const OTHER_VERDICT_BEFORE = OTHER_VERDICT.replace("s.writeFailure(w, r, ", "writeFailure(w, ");
  // The refusal of a rating the completion does not take, main's since #4488 (daemon-pane.ts's
  // `REFUSES_RATING`), whose fact goes on past the rating since #4611.
  const RATING_FIELDS =
    'PlanRating     *int       `json:"planRating,omitempty"`\n\tPlanReason     *string    `json:"planReason,omitempty"`';
  const RATING_ON_FACT = "PlanRating: rating, MustRate: c.PluginContract >= PlanRatingContract";
  const RATING_UNSPENT =
    "if refusal := planRatingRefusal(fact, current && issue.Phase == phase.Retro, reported); refusal != nil {\n\t\treturn intake.Result{Refusal: refusal, Unspent: true}, nil\n\t}";
  const RATING_WHAT =
    'what := fmt.Sprintf("this completion of phase %s of %s", reported, fact.Issue)';
  const RATING_UNASKED =
    "case !endsRetro && fact.PlanRating != nil:\n\t\treturn intake.PlanRatingUnexpected(what)";
  const RATING_WORDS =
    'func PlanRatingUnexpected(what string) *Refusal {\n\treturn &Refusal{Status: 400, Code: "PLAN_RATING_UNEXPECTED", Message: fmt.Sprintf(\n\t\t"%s names planRating or planReason, and only the completion that ends the retro rates the plan; send it again without both, and nothing was recorded", what)}';
  // The refusal of a body holding U+0000, main's since #4488, in main's words since #4463 and in
  // #4488's before them (daemon-pane.ts's `refusesNul`).
  const STORABLE = "daemon/internal/api/storable_text.go";
  const NUL_DECODED = "\treturn body, unstorableJSON(body)\n}";
  const NUL_WALK = "place, value, found := dispatchbody.NulInJSON(body)";
  const NUL_WORDS = `return fmt.Errorf("${NUL_REFUSED}", place, utf8.RuneCountInString(value[:at])+1)`;
  const UNSTORABLE_WORDS = NUL_WORDS.replace(NUL_REFUSED, NUL_UNSTORABLE);
  const BAD_BODY = 's.refuse(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())';
  const BAD_BODY_BEFORE =
    'writeJSON(w, http.StatusBadRequest, errorBody("invalid request body: "+err.Error()))';
  // The state's pull request with no count of fix attempts but for a caller below contract 59,
  // main's since #3659 (daemon-pane.ts's `PULL_REQUEST_SHOWN_BELOW`), and with none at all, main's
  // from contract 59 until #3659 (`PULL_REQUEST_UNCOUNTED`).
  const SHOWN_BELOW =
    'MergeState     string `json:"mergeState,omitempty"`\n\tReviewDecision string `json:"reviewDecision,omitempty"`\n\t// fixAttemptsShown is set on the copy the state route serves a caller below the contract\n';
  const UNCOUNTED =
    'MergeState     string `json:"mergeState,omitempty"`\n\tReviewDecision string `json:"reviewDecision,omitempty"`\n}';
  // What main's daemon since #3659 holds of its answers (daemon-pane.ts's `STATES_CONTRACT`) and of
  // a completion's `ready` (`RETIRES_READY`), and what its health route was before it
  // (`HEALTH_UNSTATED`).
  const SERVER = "daemon/internal/api/server.go";
  const CONTRACT_HEADER = 'const daemonContractHeader = "Legion-Daemon-Contract"';
  const STATING =
    "Handler:           statingContract{handler: trackTransactions(noteRequesters(mux))},";
  const HEALTH_PROJECT =
    's.writeAnswer(w, r, AnswerHealth, http.StatusOK, Health{Status: "ok", Project: s.project})';
  const HEALTH_BEFORE = 'writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})';
  const RECORD_REFUSED =
    'issue, recorded := state.Issues[key]\n\tif !recorded {\n\t\ts.writeFailure(w, r, http.StatusNotFound, recordNotFound, "The daemon has no record for "+key)';
  const OWN_BODY = "var ownBody = placement{restore: DaemonAPIVersion, strip: DaemonAPIVersion}";
  const CONTRACT_MARK = "state.Contract = &ContractView{Daemon: DaemonAPIVersion, Caller: strip}";
  const UPDATE_NONE = 'UpdateStateNone UpdateState = "none"';
  const READY_FIELD = 'Ready *bool `json:"ready,omitempty"`\n}';
  const READY_REFUSED =
    'if req.Ready != nil && *req.Ready {\n\t\ts.writeFailure(w, r, http.StatusBadRequest, "READY_RETIRED", readyRetired)\n\t\treturn\n\t}';
  const READY_WORDS =
    'const readyRetired = "this completion sends ready: true, which at contract 85 and below armed the daemon\'s merge of the pull request. " +\n\t"No daemon at contract 86 or later acts on READY: the merger merges the pull request itself, and the daemon closes " +\n\t"the issue when it reads that merge. Nothing was recorded. Merge the pull request, then complete again without ready"';
  // The state's repositories block as main's daemon writes it since #4493 (daemon-pane.ts's
  // `REPOSITORIES_LIVE`): its `live` list, an entry's `apps` and the band serving a caller below
  // contract 202 the older block; and the lists of the project's mapping every daemon before it
  // writes (`REPOSITORIES_MAPPED`), which a copy holds in comments beside a `live` field that
  // writes no such key.
  const STATE_SOURCE = "daemon/internal/api/state.go";
  const LIVE_FIELD = 'Live              []LiveRepositoryView `json:"live"`';
  const LIVE_APPS = 'Apps      *AppInstallationsView `json:"apps,omitempty"`';
  const LIVE_BAND = "{contract: repositoriesLiveContract, state: withoutRepositoriesLive,";
  const MAPPED_LIST = 'Mapped            []MappedRepositoryView  `json:"mapped"`';
  const REFUSED_LIST = 'Refused           []RefusedRepositoryView `json:"refused"`';
  const UNLISTED = LIVE_FIELD.replace('json:"live"', 'json:"-"');
  const MAPPED_FIELDS = `${UNLISTED}\n\t// ${MAPPED_LIST}\n\t// ${REFUSED_LIST}`;

  // Catches: a marker renamed in the label's source, read as a daemon that lacks what it names,
  // which answers the label as an older daemon and names no shape; and a refusal that names
  // neither the marker nor the file it was looked for in.
  for (const [file, marker, renamed] of [
    ["daemon/internal/api/handoff.go", '"HANDOFF_SESSION_MISMATCH"', '"HANDOFF_SESSION_REFUSED"'],
    ["daemon/internal/api/handoff.go", '"HANDOFF_ALREADY_RECORDED"', '"HANDOFF_ALREADY_KEPT"'],
    ["daemon/internal/workflow/engine.go", '"HANDOFF_NOT_NEW"', '"HANDOFF_NOT_FRESH"'],
    [
      "daemon/internal/api/handoff.go",
      TESTER_VERDICT,
      TESTER_VERDICT.replace("must be pass or fail", "is pass or fail"),
    ],
    [
      "daemon/internal/api/handoff.go",
      OTHER_VERDICT,
      OTHER_VERDICT.replace('req.Verdict != ""', 'req.Verdict == "pending"'),
    ],
    [EFFECTS, "func backwardMoveReason(", "func backwardMoveSentence("],
    [EFFECTS, '"BACKWARD_REASON_TOO_LONG"', '"BACKWARD_REASON_OVERLONG"'],
    [
      "daemon/internal/workflow/review_round.go",
      "func namingTheReviewRound(",
      "func withTheReviewRound(",
    ],
    [
      ROUND_WORDS,
      'const CommitLabel = "Last reviewed commit"',
      'const CommitLabel = "Last read commit"',
    ],
    [ROUND_WORDS, 'const EntryLabel = "Review entry"', 'const EntryLabel = "Round entry"'],
    [ROUND_WORDS, 'const none = "none"', 'const none = "-"'],
    [
      ROUND_WORDS,
      'return " " + CommitLabel + ": " + commit + ". " + EntryLabel + ": " + named + "."',
      'return " " + CommitLabel + " " + commit + ". " + EntryLabel + " " + named + "."',
    ],
    [
      REVIEW,
      'return e.byReviewApp(review.Author) && reviewerSession != "" && review.LegionSession() == reviewerSession',
      "return e.byReviewApp(review.Author) && review.LegionSession() == reviewerSession",
    ],
    [REVIEW, "func (e *Engine) reviewersClaim(", "func (e *Engine) reviewerClaim("],
    [FACTS, FOOTER_STRUCT, FOOTER_STRUCT.replace("Head    string", "Head    any   ")],
    [
      FACTS,
      FOOTER_DECODED,
      FOOTER_DECODED.replace("if err := json.Unmarshal", "if err := jsonLenient.Unmarshal"),
    ],
    [FACTS, HEAD_IS, HEAD_IS.replace("!reviewround.IsHead(head)", 'head == ""')],
    [ROUND_WORDS, FORTY_IS, FORTY_IS.replace("!= 40", "< 7")],
    [ROUND_WORDS, LOWER_HEX_IS, LOWER_HEX_IS.replace(" {", " && (c < 'A' || c > 'F') {")],
    [FACTS, FOOTER_MARKER, FOOTER_MARKER.replace("legion:", "legion ")],
    [FACTS, FOOTER_LAST, FOOTER_LAST.replace("LastIndex", "Index")],
    [FACTS, FOOTER_END, FOOTER_END.replace("-->", "\\n")],
    [REVIEW, NO_HEAD, NO_HEAD.replace('head == ""', "false")],
    [
      REVIEW,
      OTHER_HEAD,
      OTHER_HEAD.replace("head != pr.HeadSHA", "!strings.HasPrefix(pr.HeadSHA, head)"),
    ],
    [REVIEW, RECORDED_ON, RECORDED_ON.replace("e.reviewHead(fact)", "fact.CommitID")],
    [REVIEW, FOOTER_HEAD_KEPT, FOOTER_HEAD_KEPT.replace("review.LegionHead()", "review.CommitID")],
    ["daemon/internal/dispatch/client.go", '"/role-holders"', '"/role-sessions"'],
    [
      "envoy/internal/dispatch/api/routes_table.go",
      '"/api/v1/issues/{key}/role-holders"',
      '"/api/v1/issues/{key}/role-sessions"',
    ],
    ["daemon/internal/api/handoff.go", RATING_FIELDS, RATING_FIELDS.replace("*int ", "int  ")],
    [
      "daemon/internal/api/handoff.go",
      RATING_ON_FACT,
      RATING_ON_FACT.replace("PlanRating: rating, ", ""),
    ],
    [
      "daemon/internal/workflow/engine.go",
      RATING_UNSPENT,
      RATING_UNSPENT.replace("Unspent: true", "Unspent: false"),
    ],
    ["daemon/internal/workflow/plan.go", RATING_WHAT, RATING_WHAT.replace("phase %s", "%s")],
    [
      "daemon/internal/workflow/plan.go",
      RATING_UNASKED,
      RATING_UNASKED.replace("!endsRetro && ", ""),
    ],
    [
      FACTS,
      RATING_WORDS,
      RATING_WORDS.replace("send it again without both", "send it without both"),
    ],
    ["daemon/internal/api/claims.go", NUL_DECODED, "\treturn body, checkBody(body)\n}"],
    [STORABLE, NUL_WALK, NUL_WALK.replace("dispatchbody.NulInJSON", "nulIn")],
    [STORABLE, NUL_WORDS, NUL_WORDS.replace("at character %d", "at byte %d")],
    [
      "daemon/internal/api/claims.go",
      BAD_BODY,
      BAD_BODY.replace("invalid request body: ", "bad body: "),
    ],
    [
      "daemon/internal/api/state.go",
      SHOWN_BELOW,
      SHOWN_BELOW.replace("a caller below the contract", "every caller"),
    ],
    [SERVER, CONTRACT_HEADER, CONTRACT_HEADER.replace("Legion-Daemon-Contract", "Legion-Contract")],
    [SERVER, STATING, STATING.replace(/statingContract\{handler: (.*)\},/, "$1,")],
    [SERVER, HEALTH_PROJECT, HEALTH_PROJECT.replace(", Project: s.project", "")],
    [SERVER, RECORD_REFUSED, RECORD_REFUSED.replace("has no record for", "records nothing of")],
    ["daemon/internal/api/shaping.go", OWN_BODY, OWN_BODY.replace("strip: DaemonAPIVersion", "")],
    ["daemon/internal/api/shaping.go", CONTRACT_MARK, CONTRACT_MARK.replace("strip", "restore")],
    ["daemon/internal/api/updatestate.go", UPDATE_NONE, UPDATE_NONE.replace('"none"', '""')],
    ["daemon/internal/api/handoff.go", READY_FIELD, READY_FIELD.replace("*bool", "bool ")],
    [
      "daemon/internal/api/handoff.go",
      READY_REFUSED,
      READY_REFUSED.replace("req.Ready != nil && *req.Ready", "req.Ready != nil"),
    ],
    ["daemon/internal/api/handoff.go", READY_WORDS, READY_WORDS.replace("acts on", "reads")],
    [STATE_SOURCE, LIVE_FIELD, LIVE_FIELD.replace('json:"live"', 'json:"repositories"')],
    [STATE_SOURCE, LIVE_APPS, LIVE_APPS.replace("*AppInstallationsView", "AppInstallationsView ")],
    [
      "daemon/internal/api/shaping.go",
      LIVE_BAND,
      LIVE_BAND.replace("withoutRepositoriesLive", "withRepositoriesAt152"),
    ],
  ] as const)
    test(`this checkout's source with ${marker} renamed is refused, naming it and its file`, () => {
      expect(() => labelShapes(copyOf(CHECKOUT, file, marker, renamed))).toThrow(
        `${JSON.stringify(marker)} in packages/${file}, and this source does not`
      );
    });

  // Catches: a reader that knows a footer's head test in one place alone. Main's daemon since
  // #3995 (b537812fb5) reads the head through reviewround's `IsHead`, which took intake's own
  // `fullCommitSHA` unchanged, and refused the head label's batch until the reader knew it; a label
  // between #2539 and #3995 holds `fullCommitSHA`. Each is the one decision, a review App review held
  // to its footer's head (`decidesByFooterHead`), and a source of the older spelling with its test
  // changed is of no shape.
  test("a source that reads a footer's head through reviewround.IsHead, main's since #3995, and one through intake's fullCommitSHA, main's since #2539, each hold a review to its footer's head", () => {
    expect(labelShapes(copyOf(CHECKOUT)).decidesByFooterHead).toBe(true);
    const older: (readonly [string, string, string])[] = [
      [FACTS, HEAD_IS, HEAD_WHOLE],
      [
        FACTS,
        "func (r PullRequestReview) LegionHead() string {",
        `func fullCommitSHA(s string) bool {\n${FORTY}\n\tfor _, c := range s {\n\t\t${LOWER_HEX}\n\t\t\treturn false\n\t\t}\n\t}\n\treturn true\n}\n\nfunc (r PullRequestReview) LegionHead() string {`,
      ],
    ];
    expect(labelShapes(copyWith(CHECKOUT, older)).decidesByFooterHead).toBe(true);
    expect(() =>
      labelShapes(copyWith(CHECKOUT, [...older, [FACTS, FORTY, FORTY.replace("!= 40", "< 7")]]))
    ).toThrow(`${JSON.stringify(FORTY)} in packages/${FACTS}, and this source does not`);
  });

  // Catches: a source in #4488's words, the base label's at main 724196ef, read as main's since
  // #4463 or as no shape, so its stand-in answered a body holding U+0000 in words that daemon never
  // gave or refused its batch; a source whose words are either's with one word edited read as that
  // daemon all the same; and a source holding both read as one of them.
  test("a daemon source refuses a body holding U+0000 in its own words, main's since #4463 or #4488's before them, and in no other", () => {
    const unstorable = copyOf(CHECKOUT, STORABLE, NUL_WORDS, UNSTORABLE_WORDS);
    expect(labelShapes(unstorable)).toEqual({ ...MAIN, nulRefusal: NUL_UNSTORABLE });
    const neither = `main's daemon since #4463 holds ${JSON.stringify(NUL_WORDS)} in packages/${STORABLE}, and this source does not; main's daemon from #4488 until #4463 holds ${JSON.stringify(UNSTORABLE_WORDS)} in packages/${STORABLE}, and this source does not`;
    const refusing = NUL_WORDS.replace("refuses rather than store", "refuses rather than keep");
    expect(() => labelShapes(copyOf(CHECKOUT, STORABLE, NUL_WORDS, refusing))).toThrow(neither);
    const unstoring = UNSTORABLE_WORDS.replace("cannot store", "cannot keep");
    expect(() => labelShapes(copyOf(unstorable, STORABLE, UNSTORABLE_WORDS, unstoring))).toThrow(
      neither
    );
    const both = copyOf(CHECKOUT, STORABLE, NUL_WORDS, `${NUL_WORDS}\n\t// ${UNSTORABLE_WORDS}`);
    expect(() => labelShapes(both)).toThrow(
      "it holds the markers of two shapes at once, main's daemon since #4463, and main's daemon from #4488 until #4463"
    );
  });

  // Catches: a source of main's before #4493, every label's before a86c31c9fd, whose state's
  // repositories block is the project's mapping, read as main's since #4493 or as no shape, so its
  // stand-in served that label the live records' block its daemon never wrote, or refused its
  // batch; a block's shape read off this build rather than the label's own source; and a source
  // holding both blocks' markers, or neither, read as one of them.
  test("a daemon source before #4493 serves the project's mapping as its repositories block, and one of both blocks or neither is refused", () => {
    expect(repositoriesLive(CHECKOUT)).toBe(true);
    const mapping = copyOf(CHECKOUT, STATE_SOURCE, LIVE_FIELD, MAPPED_FIELDS);
    expect(repositoriesLive(mapping)).toBe(false);
    expect(labelShapes(mapping)).toEqual({ ...MAIN, repositoriesLive: false });
    const both = copyOf(
      CHECKOUT,
      STATE_SOURCE,
      LIVE_FIELD,
      `${LIVE_FIELD}\n\t// ${MAPPED_LIST}\n\t// ${REFUSED_LIST}`
    );
    expect(() => labelShapes(both)).toThrow(
      "the state's repositories block this rig reads: it holds the markers of two shapes at once, main's daemon since #4493, and every daemon the rig runs before #4493, whose block is the project's mapping"
    );
    const neither = copyOf(CHECKOUT, STATE_SOURCE, LIVE_FIELD, UNLISTED);
    expect(() => labelShapes(neither)).toThrow(
      `the state's repositories block this rig reads: main's daemon since #4493 holds ${JSON.stringify(LIVE_FIELD)} in packages/${STATE_SOURCE}, and this source does not; every daemon the rig runs before #4493, whose block is the project's mapping holds ${JSON.stringify(MAPPED_LIST)} in packages/${STATE_SOURCE} and ${JSON.stringify(REFUSED_LIST)} in packages/${STATE_SOURCE}, and this source does not`
    );
  });

  /** A source of main's shape from #4488 until #3659, the base label's at main 5570d0ac: this
   * checkout's with what #3659 changed of the answers the stand-in models put back as it stood —
   * the health route answering its status alone, the completion request ending at the rating,
   * each refusal written as a body of its own, and the pull request ending at the round's
   * decision — and its state's repositories block the project's mapping, as #4493 found it. */
  function beforeAnswers(): string {
    const handoff = "daemon/internal/api/handoff.go";
    const state = "daemon/internal/api/state.go";
    const read = (file: string) => readFileSync(path.join(CHECKOUT, "packages", file), "utf8");
    const readyBlock =
      /\tPlanReason {5}\*string {4}`json:"planReason,omitempty"`\n[\s\S]*?\tReady \*bool `json:"ready,omitempty"`\n\}/.exec(
        read(handoff)
      )?.[0] ?? "";
    const shownTail =
      /MergeState {5}string `json:"mergeState,omitempty"`\n\tReviewDecision string `json:"reviewDecision,omitempty"`\n\t\/\/ fixAttemptsShown[\s\S]*?\tfixAttemptsShown bool\n\}/.exec(
        read(state)
      )?.[0] ?? "";
    expect([readyBlock === "", shownTail === ""]).toEqual([false, false]);
    return copyWith(CHECKOUT, [
      [SERVER, HEALTH_PROJECT, HEALTH_BEFORE],
      [handoff, readyBlock, '\tPlanReason     *string    `json:"planReason,omitempty"`\n}'],
      [handoff, TESTER_VERDICT, TESTER_VERDICT_BEFORE],
      [handoff, OTHER_VERDICT, OTHER_VERDICT_BEFORE],
      ["daemon/internal/api/claims.go", BAD_BODY, BAD_BODY_BEFORE],
      [state, shownTail, UNCOUNTED],
      [state, LIVE_FIELD, MAPPED_FIELDS],
    ]);
  }

  // Catches: a label of main's before #3659, the base label's at 5570d0ac, answered as a daemon
  // that states a contract, names its project on `GET /healthz` and refuses `ready: true`, which
  // it never did, or refused as no shape because its refusals are written by the writer before
  // #3659's or its pull request ends at the round's decision; and the same source with any of
  // those four put back read otherwise than as main's since #3659.
  test("a daemon source before #3659 states no contract and refuses no ready, and its refusals are read in the writer it has", () => {
    const before = beforeAnswers();
    const older = {
      ...MAIN,
      statesContract: undefined,
      retiresReady: false,
      repositoriesLive: false,
    };
    expect(labelShapes(before)).toEqual(older);
    expect(
      labelShapes(
        copyWith(before, [
          ["daemon/internal/api/handoff.go", TESTER_VERDICT_BEFORE, TESTER_VERDICT],
          ["daemon/internal/api/handoff.go", OTHER_VERDICT_BEFORE, OTHER_VERDICT],
          ["daemon/internal/api/claims.go", BAD_BODY_BEFORE, BAD_BODY],
        ])
      )
    ).toEqual(older);
    expect(() =>
      labelShapes(copyOf(before, "daemon/internal/api/handoff.go", TESTER_VERDICT_BEFORE, ""))
    ).toThrow(
      `${JSON.stringify(TESTER_VERDICT_BEFORE)} in packages/daemon/internal/api/handoff.go, and this source does not`
    );
    expect(() =>
      labelShapes(copyOf(before, "daemon/internal/api/claims.go", BAD_BODY_BEFORE, ""))
    ).toThrow(
      `the 400 a body holding U+0000 is refused in this rig reads: main's daemon since #3659, whose refusals go through its answer writer holds ${JSON.stringify(BAD_BODY)} in packages/daemon/internal/api/claims.go, and this source does not; main's daemon from #4488 until #3659 holds ${JSON.stringify(BAD_BODY_BEFORE)} in packages/daemon/internal/api/claims.go, and this source does not`
    );
    expect(() => labelShapes(copyOf(before, SERVER, HEALTH_BEFORE, ""))).toThrow(
      `${JSON.stringify(HEALTH_BEFORE)} in packages/${SERVER}, and this source does not`
    );
  });

  // Catches: a source holding #3659's answers and the health route before it, read as either; and
  // one whose version.go declares no number for its answers to state, answered with a contract
  // read from elsewhere or none.
  test("a daemon source that states its contract states the one its version.go declares, and one of both shapes is refused", () => {
    expect(() =>
      labelShapes(
        copyOf(CHECKOUT, SERVER, HEALTH_PROJECT, `${HEALTH_PROJECT}\n\t// ${HEALTH_BEFORE}`)
      )
    ).toThrow(
      "the contract its answers state this rig reads: it holds the markers of two shapes at once, main's daemon since #3659, and every daemon the rig runs before #3659, whose health route answers its status alone"
    );
    const declared = `const DaemonAPIVersion = ${DAEMON_API_VERSION}`;
    expect(() =>
      labelShapes(copyOf(CHECKOUT, "daemon/internal/api/version.go", declared, `${declared} + 0`))
    ).toThrow(
      "packages/daemon/internal/api/version.go declares no `const DaemonAPIVersion = <n>` for it to state"
    );
    expect(
      labelShapes(
        copyOf(CHECKOUT, "daemon/internal/api/version.go", declared, "const DaemonAPIVersion = 207")
      ).statesContract
    ).toBe(207);
  });

  // Catches: an older shape's marker renamed, read as that older daemon all the same, or as
  // this checkout's: the code main `ff2a10ef` has in the place of each sentence, of the
  // publication, of the round's sentences, of the reviewer's recorded session, of the footer's
  // head, of the refusal of a rating and of a body holding U+0000, its pull request's count of
  // fix attempts and its state's mapping of repositories, whose loss leaves a source of neither
  // shape.
  const STATUS =
    "func (e *Engine) status(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {";
  const OUTBOX_END = '\tOutboxKindGatedStepsPublish OutboxKind = "gated_steps_publish"\n)';
  const QUEUED =
    "payload record.OutboxPayload, at time.Time) error {\n\trow, err := record.NewOutboxRow(issue, payload, at)";
  const ANY_SESSION =
    'return e.byReviewApp(review.Author) && (reviewerSession == "" || review.LegionSession() == reviewerSession)';
  const NO_RATING = 'WakeAt         *time.Time `json:"wakeAt,omitempty"`\n}';
  const NO_WALK =
    '\t\treturn body, errors.New("more than one JSON value")\n\t}\n\treturn body, nil\n}';
  const COUNTED =
    'ReviewDecision string `json:"reviewDecision,omitempty"`\n\tFixAttempts    int    `json:"fixAttempts"`\n}';
  // What main `ff2a10ef`'s store writes a taken request's agent text into, and how its routes
  // answer the failed apply (daemon-pane.ts's `STORES_NUL`).
  const ROW_TEXT =
    "row.Claim, row.HandoffCommit, row.Verdict, row.Summary, row.CompletedAt = fact.Claim, fact.Commit, fact.Verdict, fact.Summary, e.now()";
  const PHASE_COLUMNS =
    "phase.Issue, string(phase.Role), string(phase.Claim), phase.HandoffCommit, phase.HandoffRecord, phase.HandoffVersion, phase.Rounds, phase.Verdict, phase.Summary, phase.LastHandoff,";
  const COMPLETION_FAILED =
    'if err != nil {\n\t\twriteFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact")';
  const MOVE_FACT =
    "intake.BackwardMove{Issue: grant.Issue, Requester: grant.Role, To: req.To, Reason: req.Reason}, PhaseBackwardResponse{})";
  const STARTED_WITH_REASON =
    "if err := e.start(ctx, tx, issue, starting, task(issue, handoff, pr, reason)); err != nil {";
  const REASON_IN_TASK = 'text += " Reason: " + reason + "."';
  const PAYLOAD_JSON =
    'encoded, err := json.Marshal(payload)\n\tif err != nil {\n\t\treturn OutboxRow{}, fmt.Errorf("encode outbox payload: %w", err)';
  const FACT_FAILED =
    'if err != nil {\n\t\twriteFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply fact")';
  for (const [file, marker, renamed] of [
    [EFFECTS, STATUS, STATUS.replace("status string", "state string")],
    [
      "daemon/internal/record/outbox.go",
      OUTBOX_END,
      OUTBOX_END.replace("\n)", '\n\tOutboxKindClaimRelease OutboxKind = "claim_release"\n)'),
    ],
    [EFFECTS, QUEUED, QUEUED.replace("record.NewOutboxRow", "record.NewQueuedRow")],
    [REVIEW, ANY_SESSION, ANY_SESSION.replace('reviewerSession == "" || ', "")],
    [REVIEW, "func (e *Engine) claimSession(", "func (e *Engine) rowSession("],
    [
      FACTS,
      SESSION_ALONE,
      SESSION_ALONE.replace("\n\t}", '\n\t\tHead    string `json:"head"`\n\t}'),
    ],
    [REVIEW, ON_COMMIT_ID, ON_COMMIT_ID.replace("fact.CommitID", "fact.HeadSHA")],
    [
      "daemon/internal/api/handoff.go",
      NO_RATING,
      NO_RATING.replace("\n}", '\n\tNote           string     `json:"note,omitempty"`\n}'),
    ],
    [
      "daemon/internal/api/claims.go",
      NO_WALK,
      NO_WALK.replace("return body, nil", "return body, err"),
    ],
    [
      "daemon/internal/api/state.go",
      COUNTED,
      COUNTED.replace('`json:"fixAttempts"`', '`json:"fixAttempts,omitempty"`'),
    ],
    ["daemon/internal/workflow/engine.go", ROW_TEXT, ROW_TEXT.replace(", fact.Summary,", ', "",')],
    [
      "daemon/internal/record/store.go",
      PHASE_COLUMNS,
      PHASE_COLUMNS.replace("phase.Summary, ", "cleanText(phase.Summary), "),
    ],
    [
      "daemon/internal/api/handoff.go",
      COMPLETION_FAILED,
      COMPLETION_FAILED.replace("could not apply handoff fact", "could not apply the completion"),
    ],
    [
      "daemon/internal/api/architect.go",
      MOVE_FACT,
      MOVE_FACT.replace("Reason: req.Reason", 'Reason: ""'),
    ],
    [
      "daemon/internal/workflow/engine.go",
      STARTED_WITH_REASON,
      STARTED_WITH_REASON.replace(", reason)", ', "")'),
    ],
    [EFFECTS, REASON_IN_TASK, REASON_IN_TASK.replace("reason", "cleanText(reason)")],
    [
      "daemon/internal/record/outbox.go",
      PAYLOAD_JSON,
      PAYLOAD_JSON.replace("json.Marshal(payload)", "storableJSON(payload)"),
    ],
    [
      "daemon/internal/api/architect.go",
      FACT_FAILED,
      FACT_FAILED.replace("could not apply fact", "could not apply the fact"),
    ],
    [
      STATE_SOURCE,
      MAPPED_LIST,
      MAPPED_LIST.replace("MappedRepositoryView", "ServedRepositoryView"),
    ],
  ] as const)
    test.skipIf(olderHost === undefined)(
      `main ff2a10ef's source with its older marker in ${file} renamed is refused, naming it and its file`,
      () => {
        expect(() => labelShapes(copyOf(olderHost ?? "", file, marker, renamed))).toThrow(
          `${JSON.stringify(marker)} in packages/${file}, and this source does not`
        );
      }
    );

  // Catches: a source that holds the markers of both shapes, read as either one: a daemon of no
  // shape the stand-in has the answers of.
  test("a source holding the markers of both shapes is refused, naming both", () => {
    const both = copyOf(CHECKOUT);
    writeFileSync(
      path.join(both, "packages", EFFECTS),
      `${readFileSync(path.join(both, "packages", EFFECTS), "utf8")}\n// func (e *Engine) status(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {\n`
    );
    expect(() => labelShapes(both)).toThrow(
      "it holds the markers of two shapes at once, main's daemon since #4309, and main ff2a10ef's daemon, whose status write posts no message"
    );
  });
});

// The rig's stand-in daemon serves a phase worker's routes alone, none of a tree architect's (its
// design gate's among them), so the rig builds no pane prompt for one; `composeRolePrompt` below
// composes that architect's prompt for a reader of it.
test.skipIf(skip)(
  "refuses the pane prompt of a tree architect",
  async () => {
    await expect(
      workerPane(
        { rig, port: 13399, profile: "rig-profile" },
        inherited,
        { ...claim, role: "architect" },
        { daemonModule: DAEMON_MODULE, systemPrompt: true }
      )
    ).rejects.toThrow(
      "a tree architect is also told its design gate policy, which the rig does not stand in for"
    );
  },
  600_000
);

// A part of the daemon module's own role prompts, as the composition holds it: each part's text in
// order, so every part but the last ends with its own newline, which this drops.
const promptPart = (name: string) =>
  readFileSync(path.join(DAEMON_MODULE, "internal", "prompts", name), "utf8").trimEnd();

// rig.sh's assignments, read once. rigWorld gives the value rig.sh sets for the worker world:
// `worker_repo`, the repository its fixture's pull request is on, or `worker_base`, the default
// branch of its fixture's remote.
const rigAssignment = scriptAssignments(path.join(import.meta.dir, "rig.sh"));
const rigWorld = (name: string) => rigAssignment(name).slice(name.length + 1);

// The prompt a launch of a role starts with is the daemon's composition of that role's parts and
// its addressing, and a tree's root architect is told the design gate steps after its addressing;
// the text starts with the role's first part and ends with the addressing's last sentence. The
// addressing names the repository and default branch of rig.sh's worker world, so a tester
// that reads its repository there reads the one the gh stand-in serves the pull request of.
test.skipIf(skip)(
  "composes the prompt a launch of a role starts with: its parts, its addressing, and a root architect's design gate",
  () => {
    const root = composeRolePrompt(DAEMON_MODULE, "architect", true);
    expect(root.startsWith(promptPart("roles/architect-root.md"))).toBe(true);
    expect(root).toContain(promptPart("go/architect-root.md"));
    expect(root).toMatch(
      /\n\nLegion addressing: your role topic is [^\n]* Design gate steps: [^\n]+\.$/
    );

    const child = composeRolePrompt(DAEMON_MODULE, "architect", false);
    expect(child.startsWith(promptPart("roles/architect.md"))).toBe(true);
    expect(child).toContain(promptPart("go/architect.md"));
    expect(child).not.toContain(promptPart("roles/architect-root.md"));
    expect(child).not.toContain("Design gate steps: ");

    const reviewer = composeRolePrompt(DAEMON_MODULE, "reviewer", false);
    expect(reviewer).toContain(promptPart("go/reviewer.md"));
    for (const prompt of [child, reviewer]) {
      expect(prompt).toMatch(/\n\nLegion addressing: your role topic is [^\n]*\.$/);
    }
    const repository = `Your issue's repository is ${rigWorld("worker_repo")} (default branch ${rigWorld("worker_base")}); `;
    for (const prompt of [root, child, reviewer]) expect(prompt).toContain(repository);
  },
  600_000
);
