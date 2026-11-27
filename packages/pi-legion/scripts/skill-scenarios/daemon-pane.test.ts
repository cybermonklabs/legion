import { beforeAll, expect, test } from "bun:test";
import { mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptAssignments } from "../../../../scripts/e2e/lib/script-functions";
import {
  composeRolePrompt,
  DAEMON_MODULE,
  declaresProductionCheck,
  RIG_GH_TOKEN,
  workerPane,
} from "./daemon-pane";

// Every row below asks the daemon's own functions for a pane environment or a role's prompt, which
// `go run` compiles and runs, so a host with no go runs none of them.
const skip = skipWithout("go");

// The run directory: the daemon's pane environment makes the pane's home and the claim's gh
// directory under its state directory.
const rig = temporaryDirectory("skill-scenarios-pane");

// The caller's home, where the profile's agent directory and its envoy.json live: a scratch one, so
// no envoy.json of the person running the test is read.
const home = path.join(rig, "caller-home");

// A pane's jj and its git are the launcher directory's jj and git wrappers where the daemon's boot
// writes each, and the first jj and the first git on its PATH otherwise. Its gh is the launcher
// directory's wrapper over the real gh the daemon resolves on its PATH at boot: the inherited PATH
// holds a stand-in gh, the one tool the daemon needs on it for the pane to be built. It reads as a
// compiled program (no `#!` line), since the daemon refuses a script as the first gh on PATH.
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

// The pane environment comes from the daemon's own functions, compiled and run by `go run`, which
// can take minutes on a cold build cache.
test.skipIf(skip)(
  "strips an inherited Anthropic key and GitHub token, and gives the pane the claim's gh files and no grant file",
  async () => {
    const workerEnv = (
      await workerPane(
        { rig, port: 13399, profile: "rig-profile" },
        inherited,
        { project: "skillsrig", issue: "LWEVAL-1", role: "tester" },
        { daemonModule: DAEMON_MODULE }
      )
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
    // The bundle's one directory is that of the owner of the rig's repository, example/widgets
    // (daemon-pane.go's rigOwner), which the pane's addressing names.
    const ghConfigDir = `${ghRoot}/example`;
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
      await workerPane(
        { rig, port: 13399, profile: "rig-envoy-profile" },
        inherited,
        { project: "skillsrig", issue: "LWEVAL-1", role: "tester" },
        { daemonModule: DAEMON_MODULE }
      )
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
        { project: "skillsrig", issue: "LWEVAL-1", role: "tester" },
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
      workerPane(
        { rig, port: 13399, profile: "rig-no-nats-profile" },
        inherited,
        { project: "skillsrig", issue: "LWEVAL-1", role: "tester" },
        { daemonModule: DAEMON_MODULE }
      )
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
    const claim = { project: "skillsrig", issue: "LWEVAL-1", role: "tester" } as const;
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

// The panes the daemon gives the other claims that a role's `legion` tool serves: a tree's root
// architect, a child's architect (whose tree is its root's) and the daemon's own controller, which
// has no issue, no tree and no workspace. Each pane's system prompt is the claim's own, written to
// its prompt file.
const promptOf = (token: string) =>
  readFileSync(path.join(rig, "state", "secrets", `${token}-system-prompt.md`), "utf8");

test.skipIf(skip)(
  "gives a root architect, a child architect and the headless controller their panes and prompt files",
  async () => {
    const launch = { rig, port: 13399, profile: "rig-profile" };
    const source = { daemonModule: DAEMON_MODULE, systemPrompt: true };
    const project = "skillsrig";

    const root = await workerPane(
      launch,
      inherited,
      { project, issue: "LWEVAL-1", role: "architect" },
      source
    );
    expect(root.env).toMatchObject({
      LEGION_ISSUE: "LWEVAL-1",
      LEGION_TREE: "LWEVAL-1",
      LEGION_ROLE: "architect",
    });
    expect(root.systemPromptArgument).toContain(
      "legion-skillsrig-lweval-1-architect-system-prompt.md"
    );
    expect(promptOf("legion-skillsrig-lweval-1-architect")).toMatch(
      /\n\nLegion addressing: [^\n]* Design gate steps: [^\n]+ Production check: tester\.$/
    );

    const child = await workerPane(
      launch,
      inherited,
      { project, issue: "LWEVAL-2", tree: "LWEVAL-1", role: "architect" },
      source
    );
    expect(child.env).toMatchObject({
      LEGION_ISSUE: "LWEVAL-2",
      LEGION_TREE: "LWEVAL-1",
      LEGION_ROLE: "architect",
    });
    const childPrompt = promptOf("legion-skillsrig-lweval-2-architect");
    expect(childPrompt).toContain(
      "the architect above you is `notifications.role.legion-skillsrig-lweval-1-architect`"
    );
    expect(childPrompt).not.toContain("Design gate steps: ");
    expect(childPrompt).toMatch(/ Production check: tester\.$/);

    const controller = await workerPane(
      launch,
      inherited,
      { project, issue: "", role: "controller", controller: "headless" },
      source
    );
    expect(controller.env).toMatchObject({ LEGION_CONTROLLER: "1", LEGION_ROLE: "controller" });
    for (const name of ["LEGION_ISSUE", "LEGION_TREE", "LEGION_WORKSPACE"]) {
      expect(controller.env).not.toHaveProperty(name);
    }
    expect(promptOf("legion-skillsrig-controller")).toContain(
      promptPart("go/controller-headless.md")
    );
  },
  600_000
);

// The interactive controller runs in the operator's terminal under `legion controller start`,
// whose environment is its own, so the rig builds no pane for it; and a backend that does not run
// a role is refused as the daemon refuses it.
test.skipIf(skip)(
  "refuses the pane of the interactive controller and a backend that does not run the role",
  async () => {
    const launch = { rig, port: 13399, profile: "rig-profile" };
    const source = { daemonModule: DAEMON_MODULE, systemPrompt: true };
    await expect(
      workerPane(
        launch,
        inherited,
        { project: "skillsrig", issue: "", role: "controller", controller: "interactive" },
        source
      )
    ).rejects.toThrow("the interactive controller runs in the operator's terminal");
    await expect(
      workerPane(
        launch,
        inherited,
        { project: "skillsrig", issue: "LWEVAL-1", role: "architect" },
        { ...source, backend: "claude" }
      )
    ).rejects.toThrow("the Claude Code backend does not run the architect");
    await expect(
      workerPane(
        launch,
        inherited,
        { project: "skillsrig", issue: "", role: "controller", controller: "headless" },
        { ...source, backend: "claude" }
      )
    ).rejects.toThrow("the Claude Code backend does not run the controller");
  },
  600_000
);

// A part of the daemon module's own role prompts, as the composition holds it: each part's text in
// order, so every part but the last ends with its own newline, which this drops.
const promptPart = (name: string) =>
  readFileSync(path.join(DAEMON_MODULE, "internal", "prompts", name), "utf8").trimEnd();

// The role prompt files a launch concatenates, as the daemon writes them: each file whole, in
// order, and the newlines that end the last one dropped (runtime.ComposePrompt).
const concatenated = (...names: string[]) =>
  names
    .map((name) => readFileSync(path.join(DAEMON_MODULE, "internal", "prompts", name), "utf8"))
    .join("")
    .trimEnd();

// The parts of a phase worker's launch: the core and the role's own, the headless mechanics, the
// role's part, then the daemon's two (prompts.Composer.Compose).
const workerParts = (role: string) => [
  "roles/core/common.md",
  `roles/core/${role}.md`,
  "roles/mechanics/headless.md",
  `roles/${role}.md`,
  `go/${role}.md`,
  "go/worker-common.md",
];

// rig.sh's assignments, read once. rigWorld gives the value rig.sh sets for the tester-proof world:
// `worker_repo`, the repository its fixture's pull request is on, or `worker_default_branch`, the
// default branch of its fixture's remote.
const rigAssignment = scriptAssignments(path.join(import.meta.dir, "rig.sh"));
const rigWorld = (name: string) => rigAssignment(name).slice(name.length + 1);

// The prompt a launch of a role starts with is the daemon's composition of that role's parts and
// its addressing, and a tree's root architect is told the design gate steps after its addressing;
// the text starts with the role's first part and ends with the addressing's last sentence. The
// addressing names the repository and default branch of rig.sh's tester-proof world, so a tester
// that reads its repository there reads the one the gh stand-in serves the pull request of.
test.skipIf(skip)(
  "composes the prompt a launch of a role starts with: its parts, its addressing, and a root architect's design gate",
  () => {
    const root = composeRolePrompt(DAEMON_MODULE, "architect", "root");
    expect(root.startsWith(promptPart("roles/architect-root.md"))).toBe(true);
    expect(root).toContain(promptPart("go/architect-root.md"));
    expect(root).toMatch(
      /\n\nLegion addressing: your role topic is [^\n]* Design gate steps: [^\n]+\.$/
    );

    const child = composeRolePrompt(DAEMON_MODULE, "architect", "child");
    expect(child.startsWith(promptPart("roles/architect.md"))).toBe(true);
    expect(child).toContain(promptPart("go/architect.md"));
    expect(child).not.toContain(promptPart("roles/architect-root.md"));
    expect(child).not.toContain("Design gate steps: ");

    const reviewer = composeRolePrompt(DAEMON_MODULE, "reviewer", "child");
    expect(reviewer).toContain(promptPart("go/reviewer.md"));
    expect(reviewer).toMatch(/\n\nLegion addressing: your role topic is [^\n]*\.$/);
    const repository = `Your issue's repository is ${rigWorld("worker_repo")} (default branch ${rigWorld("worker_default_branch")}); `;
    for (const prompt of [root, child, reviewer]) expect(prompt).toContain(repository);
  },
  600_000
);

// One row for each launch the daemon makes: its prompt is its parts concatenated in the order the
// daemon's composition gives them, then its addressing.
test.skipIf(skip)(
  "composes a root architect's prompt: its three parts, its addressing, the design gate and the production check",
  () => {
    const prompt = composeRolePrompt(DAEMON_MODULE, "architect", "root");
    const parts = concatenated(
      "roles/architect-root.md",
      "go/architect-root.md",
      "go/architect-common.md"
    );
    expect(prompt.startsWith(`${parts}\n\nLegion addressing: your role topic is `)).toBe(true);
    expect(prompt).toMatch(
      / the architect that owns your issue is [^\n]* Design gate steps: `spec`\.[^\n]* Production check: tester\.$/
    );
  },
  600_000
);

test.skipIf(skip)(
  "composes a child architect's prompt: its three parts, the architect above it, no design gate and the production check",
  () => {
    const prompt = composeRolePrompt(DAEMON_MODULE, "architect", "child");
    const parts = concatenated("roles/architect.md", "go/architect.md", "go/architect-common.md");
    expect(prompt.startsWith(`${parts}\n\nLegion addressing: your role topic is `)).toBe(true);
    expect(prompt).toContain("; the architect above you is `");
    expect(prompt).not.toContain("Design gate steps: ");
    expect(prompt).toMatch(/ Production check: tester\.$/);
  },
  600_000
);

// The daemon appends its production check sentence to every architect's addressing, whatever the
// architect's role text says. The rig takes that from the daemon's code (declaresProductionCheck),
// never from the text it composes. This checkout's architect parts do not hold `Production check:
// off`, the phrase the rig once read as the sign of that sentence.
test.skipIf(skip)(
  "tells every architect the daemon's production check sentence, though its role text lacks the phrase",
  () => {
    expect(declaresProductionCheck(DAEMON_MODULE)).toBe(true);
    for (const [position, parts] of [
      [
        "root",
        concatenated("roles/architect-root.md", "go/architect-root.md", "go/architect-common.md"),
      ],
      ["child", concatenated("roles/architect.md", "go/architect.md", "go/architect-common.md")],
    ] as const) {
      expect(parts).not.toContain("Production check: off");
      const prompt = composeRolePrompt(DAEMON_MODULE, "architect", position);
      expect(prompt.startsWith(`${parts}\n\nLegion addressing: `)).toBe(true);
      expect(prompt).toMatch(/ Production check: tester\.$/);
    }
  },
  600_000
);

// A label's daemon older than ProductionCheckFragment tells no architect the sentence, and the
// program must build against it. Such a daemon is this module with that function taken out of
// specs.go by a `go -overlay` replacement: the function renamed, so that nothing exports it, and
// its one call gone. daemon-pane.ts reads the declaration from the Go files of the module's
// package daemon that are not tests, finds none there, and so builds daemon-pane.go alone, as
// this row does.
test.skipIf(skip)(
  "tells no architect the production check where the daemon declares no ProductionCheckFragment",
  () => {
    const specs = path.join("internal", "daemon", "specs.go");
    const older = readFileSync(path.join(DAEMON_MODULE, specs), "utf8")
      .replace(/^func ProductionCheckFragment\(/m, "func productionCheckFragmentLeftOut(")
      .replace('addressing += " " + ProductionCheckFragment(s.productionCheck)', "");
    expect(older).not.toContain("ProductionCheckFragment(");
    const module = path.join(rig, "older-daemon");
    mkdirSync(path.join(module, "internal", "daemon"), { recursive: true });
    writeFileSync(path.join(module, specs), older);
    // A test of the package may declare the function: the daemon is not built from its tests.
    writeFileSync(
      path.join(module, "internal", "daemon", "specs_test.go"),
      'package daemon\n\nfunc ProductionCheckFragment() string { return "" }\n'
    );
    expect(declaresProductionCheck(module)).toBe(false);

    const overlay = path.join(module, "overlay.json");
    writeFileSync(
      overlay,
      JSON.stringify({
        Replace: {
          [path.join(DAEMON_MODULE, "cmd", "daemon-pane", "main.go")]: path.join(
            import.meta.dir,
            "daemon-pane.go"
          ),
          [path.join(DAEMON_MODULE, specs)]: path.join(module, specs),
        },
      })
    );
    for (const position of ["root", "child"] as const) {
      const run = Bun.spawnSync(
        ["go", "run", "-overlay", overlay, "./cmd/daemon-pane", "prompt", "architect", position],
        { cwd: DAEMON_MODULE, stdout: "pipe", stderr: "pipe" }
      );
      expect({ position, code: run.exitCode, stderr: run.stderr.toString() }).toEqual({
        position,
        code: 0,
        stderr: "",
      });
      // The prompt is this daemon's, but for the sentence.
      expect(`${run.stdout.toString()} Production check: tester.`).toBe(
        composeRolePrompt(DAEMON_MODULE, "architect", position)
      );
    }
  },
  600_000
);

test.skipIf(skip)(
  "composes the headless and the interactive controller's prompts: its role part, its launch's part, the design gate and its reach, no addressing line",
  () => {
    const reach =
      /\n\nDesign gate steps: `spec`\.[^\n]* Your role's GitHub App is installed on the owner example, and its token reaches every repository of that owner\.$/;
    for (const launch of ["headless", "interactive"] as const) {
      const prompt = composeRolePrompt(DAEMON_MODULE, "controller", launch);
      const parts = concatenated("roles/controller-root.md", `go/controller-${launch}.md`);
      expect(prompt.startsWith(`${parts}\n\nDesign gate steps: `)).toBe(true);
      expect(prompt).toMatch(reach);
      expect(prompt).not.toContain("Legion addressing: ");
      expect(prompt).not.toContain("Production check:");
    }
    expect(composeRolePrompt(DAEMON_MODULE, "controller", "headless")).not.toContain(
      promptPart("go/controller-interactive.md")
    );
  },
  600_000
);

test.skipIf(skip)(
  "composes the merger's prompt: the headless mechanics, its part, the daemon's two and its addressing",
  () => {
    const prompt = composeRolePrompt(DAEMON_MODULE, "merger", "child");
    const parts = concatenated(
      "roles/mechanics/headless.md",
      "roles/merger.md",
      "go/merger.md",
      "go/worker-common.md"
    );
    expect(prompt.startsWith(`${parts}\n\nLegion addressing: your role topic is `)).toBe(true);
    expect(prompt).toMatch(/ Your issue's repository is [^\n]*\.$/);
    expect(prompt).not.toContain("Production check: ");
  },
  600_000
);

// The claude backend's own part follows the worker's parts (runtime.Backends): it is told no more
// to Oh My Pi, and the backend runs the phase workers alone.
test.skipIf(skip)(
  "composes a worker's prompt for the claude backend: the claude mechanics part after the worker parts",
  () => {
    const omp = composeRolePrompt(DAEMON_MODULE, "reviewer", "child", "omp");
    const claude = composeRolePrompt(DAEMON_MODULE, "reviewer", "child", "claude");
    const parts = concatenated(...workerParts("reviewer"), "go/claude-mechanics.md");
    expect(claude.startsWith(`${parts}\n\nLegion addressing: your role topic is `)).toBe(true);
    expect(omp).not.toContain(promptPart("go/claude-mechanics.md"));
    expect(
      omp.startsWith(`${concatenated(...workerParts("reviewer"))}\n\nLegion addressing: `)
    ).toBe(true);
    // The backend's part changes nothing after the parts: the addressing is the same.
    const addressing = (prompt: string) => prompt.slice(prompt.indexOf("\n\nLegion addressing: "));
    expect(addressing(claude)).toBe(addressing(omp));
    for (const role of ["architect", "controller"] as const) {
      expect(() =>
        composeRolePrompt(
          DAEMON_MODULE,
          role,
          role === "controller" ? "headless" : "root",
          "claude"
        )
      ).toThrow(`the Claude Code backend does not run the ${role}`);
    }
  },
  600_000
);
