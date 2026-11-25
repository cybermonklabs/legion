import { beforeAll, expect, test } from "bun:test";
import { mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptAssignments } from "../../../../scripts/e2e/lib/script-functions";
import { composeRolePrompt, DAEMON_MODULE, RIG_GH_TOKEN, workerPane } from "./daemon-pane";

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

// The rig's stand-in daemon serves a phase worker's routes alone, none of a tree architect's (its
// design gate's among them), so the rig builds no pane prompt for one; `composeRolePrompt` below
// composes that architect's prompt for a reader of it.
test.skipIf(skip)(
  "refuses the pane prompt of a tree architect",
  () => {
    expect(() =>
      workerPane(
        { rig, port: 13399, profile: "rig-profile" },
        inherited,
        { project: "skillsrig", issue: "LWEVAL-1", role: "architect" },
        { daemonModule: DAEMON_MODULE, systemPrompt: true }
      )
    ).toThrow(
      "a tree architect is also told its design gate policy, which the rig does not stand in for"
    );
  },
  600_000
);

// A part of the daemon module's own role prompts, as the composition holds it: each part's text in
// order, so every part but the last ends with its own newline, which this drops.
const promptPart = (name: string) =>
  readFileSync(path.join(DAEMON_MODULE, "internal", "prompts", name), "utf8").trimEnd();

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
    const repository = `Your issue's repository is ${rigWorld("worker_repo")} (default branch ${rigWorld("worker_default_branch")}); `;
    for (const prompt of [root, child, reviewer]) expect(prompt).toContain(repository);
  },
  600_000
);
