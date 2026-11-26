/**
 * The rig's reading of the Legion daemon: `daemon-pane.go`, run as a main inside a checkout's
 * daemon module, which is how it reaches the daemon's internal packages (a build of it there,
 * `SKILL_SCENARIOS_DAEMON_PANE`, or `go run -overlay`), and `daemon-routes.go`,
 * `daemon-completion.go` and `gh-selectors.go`, run with `go run` outside every module, since
 * reading the routes, how the daemon takes a completion and the selector table of its gh wrapper
 * needs only the daemon's files. The rig asks them rather than restating the daemon: a change to
 * the daemon reaches the rig by construction. The exceptions are `daemon-pane.go`'s own copies of
 * the daemon's table of the handoff words, its resolution of the real gh, and its join and write
 * of a system prompt, which the program carries because one program runs against each label's
 * checkout of the daemon module, and an older one names them otherwise or lacks them; each one's
 * comment says so. `workerPane` builds a phase worker's whole pane environment over it,
 * `composeRolePrompt` the prompt a launch of any role starts with, `standinFacts` reads every
 * fact the daemon stand-in answers by, and `labelGhSelectors` what the gh stand-in refuses an
 * owner by.
 *
 * Usage: `bun daemon-pane.ts facts <checkout> <role>[:<phase>]...` reads, through the build
 * `SKILL_SCENARIOS_DAEMON_PANE` names, the facts `standinFacts` reads for a stand-in of each role
 * booted in each phase (none: the role's own), and the checkout's gh selectors
 * (`labelGhSelectors`), into the facts cache under `SKILL_SCENARIOS_WORK`, so a stand-in's boot
 * after it reads each out of a file (rig.sh's label_facts). It exits 1, naming the fact, when one
 * cannot be read, and 2 on a usage error or with no build or no work directory named. `bun
 * daemon-pane.ts gh-selectors <checkout> <file>` writes the checkout's gh selectors to `file`
 * (gh-selectors.ts's RUN_SELECTORS, rig.sh's worker_boot), exiting 1 when they cannot be read.
 * `bun daemon-pane.ts review-round <checkout> <commit> <entry>` prints the two sentences the
 * checkout's daemon ends a reviewer's task in `reviewing` with, for the commit its row holds (""
 * for none) and the entry it stands at (0 for none), and nothing for a daemon that writes none
 * (`reviewRoundSentences`, rig.sh's run_reviewer_text_findings), exiting 1 when its source is of
 * no shape this reads.
 */
import { createHash, randomInt, randomUUID } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import { isLegionRole, type LegionRole } from "@legion/contracts";
import { daemonContract } from "@legion/contracts/daemon-contract";
import { DEFAULT_ENVOY_URL } from "@legion/envoy-client/envoy-settings";
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings";
import {
  LATE_ROLES,
  type LabelDaemon,
  NOT_CURRENT_PHASE,
  RECORD_ROUTES,
} from "./completion-answers";
import { type GhSelectors, type GhTable, selectorsOf, tableOf } from "./gh-selectors";
import { Refused } from "./run-records";
import {
  DEFAULT_STEPS,
  FAILED_TRIGGERS,
  STEP_NAMESPACE_NAME,
  STEP_TASK,
  STEP_WRITE_FIELDS,
  STEPS,
} from "./step-records";

/** This checkout's daemon module, the one the rig reads unless a run names another checkout's. */
export const DAEMON_MODULE = path.resolve(import.meta.dir, "../../../daemon");

const PROGRAM = path.join(import.meta.dir, "daemon-pane.go");
const ROUTES_PROGRAM = path.join(import.meta.dir, "daemon-routes.go");
const COMPLETION_PROGRAM = path.join(import.meta.dir, "daemon-completion.go");
const GH_SELECTORS_PROGRAM = path.join(import.meta.dir, "gh-selectors.go");

/** Runs `command` in `cwd` under the caller's environment and `env` over it, `input` on its stdin
 * (the caller's own stdin when there is none), and returns its stdout. Throws naming `what`, with
 * the program's own stderr, when the run exits other than 0. */
function runProgram(
  what: string,
  command: readonly string[],
  cwd: string,
  env: Record<string, string>,
  input?: string
): string {
  const result = Bun.spawnSync([...command], {
    cwd,
    env: { ...process.env, ...env },
    stdin: input === undefined ? "inherit" : new TextEncoder().encode(input),
    stdout: "pipe",
    stderr: "pipe",
  });
  if (result.exitCode !== 0)
    throw new Error(`${what} exited ${result.exitCode}: ${result.stderr.toString().trim()}`);
  return result.stdout.toString();
}

/** The build of daemon-pane.go a process runs in place of `go run` (`runDaemonPane`): the binary
 * `SKILL_SCENARIOS_DAEMON_PANE` names, or undefined where it names none and every read is a `go
 * run`. rig.sh's daemon_pane_binary builds it in the daemon module of a label's checkout once a
 * batch (services_up), and the rig hands it to each process that reads that label's daemon
 * (label_facts, worker_boot). A `go run` compiles the daemon module first: 6.2-7.4 s a read with
 * a warm Go build cache and 282 s with a cold one (measured on 2026-11-15), where the build's run
 * takes milliseconds. One process reads one label's daemon, so one build serves every read it
 * makes: `phase` and `pane` run that module's own functions, and `refusal`, `count-default` and
 * `claim-refusal` are the program's own parse of the source files they are given, the same in a
 * build of any module. A build named and not there is an error, never a `go run` in its place. */
function daemonPaneBuild(): string | undefined {
  const build = process.env.SKILL_SCENARIOS_DAEMON_PANE;
  if (build === undefined || build === "") return undefined;
  if (!existsSync(build))
    throw new Error(
      `SKILL_SCENARIOS_DAEMON_PANE names ${build}, which is not there: rig.sh's services_up builds it (daemon_pane_binary)`
    );
  return build;
}

/** Runs daemon-pane.go in `module` with `args`, `input` on its stdin (the caller's own stdin when
 * there is none), and returns its stdout: the build `daemonPaneBuild` names, or else `go run
 * -overlay` of the program as `<module>/cmd/daemon-pane/main.go`. */
export function runDaemonPane(module: string, args: readonly string[], input?: string): string {
  const what = `daemon-pane ${args.join(" ")} in ${module}`;
  const build = daemonPaneBuild();
  if (build !== undefined) return runProgram(what, [build, ...args], module, {}, input);
  const scratch = mkdtempSync(path.join(os.tmpdir(), "daemon-pane-"));
  try {
    const overlay = path.join(scratch, "overlay.json");
    const main = path.join(module, "cmd", "daemon-pane", "main.go");
    writeFileSync(overlay, JSON.stringify({ Replace: { [main]: PROGRAM } }));
    return runProgram(
      what,
      ["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args],
      module,
      {},
      input
    );
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

/** The printout of `program`, daemon-routes.go, daemon-completion.go or gh-selectors.go, over the
 * directory `dir` of a daemon's source. Each imports the standard library alone and runs from its
 * own directory with go.work off, where no go.mod above it makes it part of a module: building it
 * compiles a few standard packages and downloads nothing, so a Go cache that holds nothing yet
 * costs seconds rather than the daemon module's whole build. `dir` is resolved against the
 * caller's directory first, as `goSources` reads it, since the program runs in another. */
function runStandalone(program: string, dir: string): string {
  const resolved = path.resolve(dir);
  return runProgram(
    `${path.basename(program, ".go")} ${resolved}`,
    ["go", "run", program, resolved],
    import.meta.dir,
    { GOWORK: "off" }
  );
}

/** The daemon's refusal of a completion of a phase the completing role does not run: its code
 * and the daemon's own message for it. */
export interface Refusal {
  readonly code: string;
  readonly message: string;
}

/** The refusal daemon-pane.go's `refusal not-current-phase` printed as `printed`, or undefined
 * when it printed anything but what that command prints: the code, then the message, each a
 * non-empty line of its own. Fewer lines, an empty one, or more than two are a refusal half
 * read, which is a sentence the daemon never sends, so the caller reads such a printout as none
 * rather than answering part of it. */
export function refusalIn(printed: string): Refusal | undefined {
  const [code = "", message = "", ...rest] = printed.split("\n");
  if (code === "" || message === "" || rest.length !== 1 || rest[0] !== "") return undefined;
  return { code, message };
}

/** The daemon module of `checkout`. */
function daemonOf(checkout: string): string {
  return path.join(checkout, "packages", "daemon");
}

/** The Go sources of the package at `dir` but its tests, in byte order, each a full path; none for
 * a directory that is not there, whose reading then refuses on its own terms. */
function goSources(dir: string): string[] {
  if (!existsSync(dir)) return [];
  return readdirSync(dir)
    .filter((name) => name.endsWith(".go") && !name.endsWith("_test.go"))
    .sort()
    .map((name) => path.join(dir, name));
}

/** Where daemon-pane.go's answers are kept between processes (`cachedRead`): under the rig's work
 * directory (`SKILL_SCENARIOS_WORK`, which rig.sh exports), else under `TMPDIR`, in a directory of
 * a name that holds no word of the rig's vocabulary (`legion-daemon-cache`: `facts` is an askgate
 * case); never under HOME or a `/tmp` nobody named.
 * With neither set nothing is kept, and every read runs the program. */
function factsCache(): string | undefined {
  const work = process.env.SKILL_SCENARIOS_WORK;
  if (work !== undefined && work !== "") return path.join(work, "daemon-facts");
  const tmp = process.env.TMPDIR;
  if (tmp !== undefined && tmp !== "") return path.join(tmp, "legion-daemon-cache");
  return undefined;
}

/** The answer `run` prints, kept in the facts cache under a key of exactly what decides it: the
 * source of `program`, the Go program `run` runs, `keyArgs` (the command with no path in it),
 * and every source file the read parses, each by its path under `root` and its bytes. A boot
 * whose key is kept reads the answer from the file, with no `go run`; any other runs the program
 * and writes the answer whole (a temporary file renamed into place), so a reader never sees half
 * of one. A read that fails writes nothing, and throws as the program refused. The key names no
 * absolute path, so a second checkout of the same sources is the same answer. */
function cachedRead(
  program: string,
  run: () => string,
  keyArgs: readonly string[],
  root: string,
  sources: readonly string[]
): string {
  const dir = factsCache();
  if (dir === undefined) return run();
  const hash = createHash("sha256").update(readFileSync(program)).update(JSON.stringify(keyArgs));
  for (const source of sources)
    hash.update(`\0${path.relative(root, source)}\0`).update(readFileSync(source));
  const file = path.join(dir, `${hash.digest("hex")}.out`);
  if (existsSync(file)) return readFileSync(file, "utf8");
  const answer = run();
  mkdirSync(dir, { recursive: true });
  const written = `${file}.${process.pid}.${randomUUID()}.tmp`;
  writeFileSync(written, answer);
  renameSync(written, file);
  return answer;
}

/** The daemon's own refusal of `role`'s completion while the issue `issue` stands in `phase`, as
 * the daemon of `checkout` writes it: read out of that checkout's engine and intake sources by
 * daemon-pane.go (`refusalIn`), or undefined when that printout is no refusal. The daemon
 * stand-in answers such a completion with it and its tests read it the same way, so no copy of
 * the sentence and no second reading of it lives here. */
export function notCurrentPhase(
  checkout: string,
  role: string,
  phase: string,
  issue: string
): Refusal | undefined {
  const daemon = daemonOf(checkout);
  const engine = path.join(daemon, "internal", "workflow", "engine.go");
  const intake = path.join(daemon, "internal", "intake");
  const command = ["refusal", "not-current-phase", role, phase, issue];
  return refusalIn(
    cachedRead(
      PROGRAM,
      () => runDaemonPane(DAEMON_MODULE, [...command, engine, intake]),
      command,
      daemon,
      [engine, ...goSources(intake)]
    )
  );
}

/** The phase `role`'s worker is started in by the daemon of `checkout`, the first of its
 * transition table the role works, and the word of the handoff record that phase ends with, ""
 * when it ends with none: daemon-pane.go's `phase`, which runs the daemon's own functions and so
 * runs inside that checkout's module. Its answer is the phase and workflow tables and the claim
 * roles they name, so those three packages are what keeps it (`cachedRead`). */
export function labelPhase(checkout: string, role: string): { phase: string; word: string } {
  const daemon = daemonOf(checkout);
  const internal = path.join(daemon, "internal");
  const [phase = "", word = ""] = cachedRead(
    PROGRAM,
    () => runDaemonPane(daemon, ["phase", role]),
    ["phase", role],
    daemon,
    [
      ...goSources(path.join(internal, "phase")),
      ...goSources(path.join(internal, "workflow")),
      ...goSources(path.join(internal, "claim")),
    ]
  ).split("\n");
  return { phase, word };
}

/** The checkout the run under `runDir` was built from: the `checkout` line of its built-from,
 * which rig.sh's cmd_profile writes for the label and cmd_run copies into the run before any
 * agent boots. Its daemon is the one the label's runs stand in for (`labelRoutes`,
 * `labelDaemon`). Throws for a run with no such line: what that run's daemon serves is unknown. */
export function labelCheckout(runDir: string): string {
  const file = path.join(runDir, "built-from");
  const line = existsSync(file)
    ? readFileSync(file, "utf8")
        .split("\n")
        .find((one) => one.startsWith("checkout "))
    : undefined;
  const checkout = line?.slice("checkout ".length).trim() ?? "";
  if (checkout === "")
    throw new Error(
      `${file} names no checkout (rig.sh's cmd_profile writes a \`checkout <dir>\` line), so the daemon that run stands in for is unknown`
    );
  return checkout;
}

/** Dispatch's table of the scopes each route needs, under a checkout's root, which
 * person-routes.ts's `labelPersonRoutes` reads. It lives here, not there, so a boot that loads
 * this module loads no reader of Dispatch's routes. */
export const SCOPES_GO = "packages/envoy/internal/dispatch/api/routes_table.go";
/** The agents' client of Dispatch, which every `dispatch` command sends its requests through,
 * under a checkout's root, which person-routes.ts's `labelPersonRoutes` reads; here as SCOPES_GO
 * is. */
export const CLIENT_TS = "packages/envoy-client/src/dispatch-http.ts";
/** The sources a reader reads of a run's label, under the checkout's root: the daemon's
 * (`labelRoutes`, `labelDaemon`, whose engine asks methods across its workflow package since
 * main's #4611), and its Dispatch's table of the scopes each route needs and its agents' client
 * (SCOPES_GO and CLIENT_TS, which person-routes.ts's `labelPersonRoutes` reads). */
export const LABEL_SOURCES = [
  "packages/daemon/internal/api",
  "packages/daemon/internal/workflow",
  SCOPES_GO,
  CLIENT_TS,
];
/** The directory each built-from commit's label sources were rebuilt into in this process. */
const rebuilt = new Map<string, string>();

/** The sources of the label the run under `runDir` ran, rebuilt from what the run recorded and
 * never read off the checkout its built-from names, which the pod may since have moved or lost:
 * the `commit <sha>` line of its built-from (rig.sh's checkout_commit), read with `git archive`
 * out of the first of `repos` (git directories the caller names, `--repo`) that holds that
 * commit, into a directory of this process's own, removed when it exits. The commit id is the
 * digest git reads every file of it by, so the sources are the ones the run ran. Of LABEL_SOURCES
 * only those the commit holds are rebuilt: a commit holding no daemon is a label with no daemon
 * there, which `labelRoutes` and `labelDaemon` then say, as they say it of any checkout, and one
 * holding no Dispatch source is one `labelPersonRoutes` refuses so. Returns that directory, a
 * checkout holding those sources alone, and the commit. Throws a Refused naming the run and the
 * input for a built-from with no commit, a commit of a checkout with changes no commit holds
 * (`dirty`), no repository named, and none that holds the commit. */
export function recordedCheckout(
  runDir: string,
  repos: readonly string[]
): { dir: string; commit: string } {
  const file = path.join(runDir, "built-from");
  const line = existsSync(file)
    ? readFileSync(file, "utf8")
        .split("\n")
        .find((one) => one.startsWith("commit "))
    : undefined;
  const [, commit = "", ...rest] = (line ?? "").trim().split(" ");
  const run = path.basename(runDir);
  if (!/^[0-9a-f]{40}$/.test(commit))
    throw new Refused(
      `${run}: its built-from records no commit (\`commit <sha>\`), so the daemon sources its label ran cannot be rebuilt`
    );
  if (rest.includes("dirty"))
    throw new Refused(
      `${run}: its built-from's commit ${commit} is of a checkout with changes no commit holds (\`dirty\`), so the daemon sources its label ran cannot be rebuilt`
    );
  const known = rebuilt.get(commit);
  if (known !== undefined) return { dir: known, commit };
  if (repos.length === 0)
    throw new Refused(
      `${run}: its label's daemon sources are rebuilt from commit ${commit}, which its built-from records, out of a repository the caller names (--repo <git dir>), and none was named`
    );
  const repo = repos.find(
    (one) =>
      Bun.spawnSync(["git", `--git-dir=${one}`, "cat-file", "-e", `${commit}^{commit}`])
        .exitCode === 0
  );
  if (repo === undefined)
    throw new Refused(
      `${run}: no repository named (${repos.join(", ")}) holds commit ${commit}, which its built-from records`
    );
  const git = (...args: string[]) => {
    const done = Bun.spawnSync(["git", `--git-dir=${repo}`, ...args]);
    if (done.exitCode !== 0)
      throw new Error(`git ${args.join(" ")} of ${repo} failed: ${done.stderr.toString().trim()}`);
    return done.stdout;
  };
  const held = git("ls-tree", "-r", "-z", "--name-only", commit, "--", ...LABEL_SOURCES)
    .toString()
    .split("\0")
    .filter((name) => name !== "");
  const dir = mkdtempSync(path.join(os.tmpdir(), "label-sources-"));
  process.on("exit", () => rmSync(dir, { recursive: true, force: true }));
  if (held.length > 0) {
    const unpacked = Bun.spawnSync(["tar", "-x", "-C", dir], {
      stdin: git("archive", "--format=tar", commit, "--", ...held),
    });
    if (unpacked.exitCode !== 0)
      throw new Error(`tar could not unpack commit ${commit}'s label sources: ${unpacked.stderr}`);
  }
  rebuilt.set(commit, dir);
  return { dir, commit };
}

/** Every route the daemon of `checkout` registers, `<METHOD> <path>` each, read out of that
 * checkout's API source by daemon-routes.go (`runStandalone`), which parses the checkout's files
 * and compiles nothing of them or of any daemon module, so a substitute checkout holding the
 * source alone is read the same way. The stand-in answers by this set and health holds every 404
 * to it, so both read one set by one reading. Throws what the reading refused. */
export function labelRoutes(checkout: string): readonly string[] {
  const daemon = daemonOf(checkout);
  const api = path.join(daemon, "internal", "api");
  return cachedRead(
    ROUTES_PROGRAM,
    () => runStandalone(ROUTES_PROGRAM, api),
    ["routes"],
    daemon,
    goSources(api)
  )
    .split("\n")
    .filter((line) => line !== "");
}

/** The selector table of the Legion gh wrapper of `checkout` (gh-selectors.ts's `GhTable`), read
 * out of that checkout's cmd/legion and internal/ghconfig by gh-selectors.go (`runStandalone`),
 * which parses their files and compiles none of them, and kept in the facts cache keyed by those
 * packages' bytes, as the routes are. So each label's is read from its own checkout, and a
 * selector form its wrapper learns is read with no edit, or refused, naming it, where this rig
 * does not model it (gh-selectors.ts's `tableOf`). Each pattern's file is named under the
 * checkout's daemon module, which the printout, kept for any checkout of the same sources, does
 * not name. Throws what the reading refused. */
export function labelGhTable(checkout: string): GhTable {
  const daemon = daemonOf(checkout);
  return tableOf(
    cachedRead(
      GH_SELECTORS_PROGRAM,
      () => runStandalone(GH_SELECTORS_PROGRAM, daemon),
      ["gh-selectors"],
      daemon,
      [
        ...goSources(path.join(daemon, "cmd", "legion")),
        ...goSources(path.join(daemon, "internal", "ghconfig")),
      ]
    ),
    daemon
  );
}

/** The gh selectors of the daemon of `checkout` (gh-selectors.ts's `GhSelectors`): its gh
 * wrapper's table (`labelGhTable`) joined with the gh help that checkout saves for the wrapper's
 * own tests (cmd/legion/testdata/gh-help), which is what the wrapper's table is held to there,
 * and whether that wrapper runs a read naming only owners it holds no directory for with the
 * pane's own GH_CONFIG_DIR (`PANE_TOKEN_READ`, main's since #4035) or refuses it
 * (`OWNER_ALWAYS_REFUSED`, main's before), and in which words it refuses such an owner
 * (`OWNER_NOT_SERVED`, main's since #4459, or `OWNER_SETUP_ERROR`, main ff2a10ef's), each read off
 * its source as `labelShapes` reads the daemon's.
 * rig.sh's label_facts reads them at services_up and its worker_boot writes them into each run,
 * which the gh stand-in reads them from; health reads the run's label's here. Throws what the
 * reading refused, a wrapper of neither shape among it. */
export function labelGhSelectors(checkout: string): GhSelectors {
  const table = labelGhTable(checkout);
  const helps = path.join(daemonOf(checkout), "cmd", "legion", "testdata", "gh-help");
  if (!existsSync(path.join(helps, "reference.txt")))
    throw new Error(
      `${helps}/reference.txt is not there, so which commands the gh wrapper of ${checkout} reads, and how, is unknown`
    );
  const saved = Object.fromEntries(
    readdirSync(helps)
      .filter((name) => name.endsWith(".txt") && name !== "reference.txt")
      .map((name) => [name, readFileSync(path.join(helps, name), "utf8")])
  );
  const paneToken = sourceShape(
    checkout,
    "the gh wrapper's answer to a read naming only owners it holds no directory for",
    [
      { name: "main's wrapper since #4035", markers: PANE_TOKEN_READ, value: true },
      {
        name: "main's wrapper before #4035, which refuses every such call",
        markers: OWNER_ALWAYS_REFUSED,
        value: false,
      },
    ]
  );
  const unservedOwner = sourceShape(
    checkout,
    "the gh wrapper's refusal of an owner it holds no directory for",
    [
      { name: "main's wrapper since #4459", markers: OWNER_NOT_SERVED, value: "one-line" as const },
      {
        name: "main ff2a10ef's wrapper, whose refusal the leak guard's setup error carries",
        markers: OWNER_SETUP_ERROR,
        value: "setup-error" as const,
      },
    ]
  );
  return {
    ...selectorsOf(
      table,
      saved,
      readFileSync(path.join(helps, "reference.txt"), "utf8"),
      paneToken
    ),
    unservedOwner,
  };
}

const GH_SELECTORS_GO = "daemon/cmd/legion/gh_selectors.go";
const GH_GO = "daemon/cmd/legion/gh.go";
/** The wrapper since #4035 (cac1693d): its selectors' `dir` takes a fallback, GH_CONFIG_DIR's for a
 * call that writes nothing or is a GraphQL query read, and runs a call whose every item is an
 * owner the bundle holds no directory for with it. */
const PANE_TOKEN_READ: readonly Marker[] = [
  {
    file: GH_SELECTORS_GO,
    text: "func (s *selectors) dir(dirFor func(owner, name string) (string, error), fallback string) (string, error) {",
  },
  {
    file: GH_GO,
    text: 'if writesNothing || ghGraphQLQueryRead(argv) {\n\t\tfallback = env["GH_CONFIG_DIR"]',
  },
  { file: GH_GO, text: "dir, err := sel.dir(dirFor, fallback)" },
];
/** The wrapper before #4035: its selectors' `dir` takes no fallback, and refuses the first item the
 * bundle holds no directory for. */
const OWNER_ALWAYS_REFUSED: readonly Marker[] = [
  {
    file: GH_SELECTORS_GO,
    text: "func (s *selectors) dir(dirFor func(owner, name string) (string, error)) (string, error) {",
  },
  { file: GH_GO, text: "dir, err := sel.dir(dirFor)\n" },
];
/** The wrapper since #4459: its bundle's lookup of an owner it holds no directory for refuses with
 * leakguard's OwnerNotServedError, whose words `legion gh` prints alone (visibility.go). */
const OWNER_NOT_SERVED: readonly Marker[] = [
  {
    file: "daemon/internal/leakguard/visibility.go",
    text: `return fmt.Sprintf("the role's GitHub App is not installed on %q: %s holds no gh directory for it", e.Owner, e.OwnersDir)`,
  },
  {
    file: "daemon/cmd/legion/gitcredential.go",
    text: 'return "", &leakguard.OwnerNotServedError{Owner: owner, OwnersDir: ownersDir}',
  },
];
/** The wrapper of main ff2a10ef: its lookup refuses an owner in older words (gitcredential.go),
 * and it refuses a call naming two owners before either (gh.go). */
const OWNER_SETUP_ERROR: readonly Marker[] = [
  {
    file: "daemon/cmd/legion/gitcredential.go",
    text: 'return "", fmt.Errorf("%q is not an owner of this project: %s holds no gh directory for it", owner, ownersDir)',
  },
  {
    file: GH_GO,
    text: 'return ghTarget{}, fmt.Errorf("the command names more than one owner, %s, so it sends no token; name one repository", strings.Join(listed, " and "))',
  },
];

/** Each refusal `labelDaemon` met in this process, by the daemon module and the bytes of the
 * sources it read. The facts cache keeps no failed read (`cachedRead`), so without this a source
 * that cannot be read was run again for each answer that asked for it: health asks once a rule a
 * run, and the tester's fixture of 21 runs, whose built-from names checkouts that are not there,
 * paid 15 `go run`s of daemon-completion.go and timed out at 60 s. Keyed by the bytes too, so a
 * source rewritten in place is read again. */
const daemonRefusals = new Map<string, Error>();

/** How the daemon of `checkout` takes a completion (completion-answers.ts's `LabelDaemon`): its
 * engine's late role and its route's record, read out of that checkout's internal/workflow
 * package (engine.go's `handoff` and the engine's methods it asks, which main's since #4611 asks
 * across engine.go and plan.go) and internal/api/handoff.go by daemon-completion.go
 * (`runStandalone`), which parses those files and compiles nothing of them, so a substitute
 * checkout holding engine.go and handoff.go alone is read the same way. A label's contract is so
 * read once per label source: the stand-in reads it at its boot (`standinFacts`) out of the facts
 * cache, which rig.sh's label_facts fills for each label before any run starts, and health and the
 * scorer read the run's own label's through the same cache (rig.sh gives both the work directory),
 * keyed by the bytes of the files it parses (`cachedRead`), so a label whose checkout moves is read
 * anew and never by its name. An engine.go or a handoff.go that is not there is refused here,
 * naming it, with no program run: what a checkout lacks needs no compile to say, and a run whose
 * built-from names a checkout that is gone costs health nothing a slow host could stretch. A
 * refusal is kept for the process (`daemonRefusals`). Throws what the reading refused, naming the
 * file and the shapes it looked for, and for a printout that is no late role and record route. */
export function labelDaemon(checkout: string): LabelDaemon {
  const daemon = daemonOf(checkout);
  const workflow = path.join(daemon, "internal", "workflow");
  const route = path.join(daemon, "internal", "api", "handoff.go");
  const absent = [path.join(workflow, "engine.go"), route].find((source) => !existsSync(source));
  if (absent !== undefined)
    throw new Error(
      `${absent} is not there, so how the daemon of ${checkout} takes a completion is unknown (daemon-completion.go reads it)`
    );
  const sources = [...goSources(workflow), route];
  const key = [daemon, ...sources.map((source) => readFileSync(source, "utf8"))].join("\0");
  const refused = daemonRefusals.get(key);
  if (refused !== undefined) throw refused;
  try {
    const printed = cachedRead(
      COMPLETION_PROGRAM,
      () => runStandalone(COMPLETION_PROGRAM, daemon),
      ["completion"],
      daemon,
      sources
    );
    const [late, route, ...rest] = printed.split("\n");
    const lateRole = LATE_ROLES.find((shape) => shape === late);
    const recordRoute = RECORD_ROUTES.find((shape) => shape === route);
    if (lateRole === undefined || recordRoute === undefined || rest.join("") !== "")
      throw new Error(
        `daemon-completion ${daemon} printed ${JSON.stringify(printed)}, which is no late role (${LATE_ROLES.join(" or ")}) and record route (${RECORD_ROUTES.join(" or ")})`
      );
    return { lateRole, recordRoute };
  } catch (error) {
    const kept = error instanceof Error ? error : new Error(String(error));
    daemonRefusals.set(key, kept);
    throw kept;
  }
}

/** `judge`'s verdict on the run under `runDir`, `judge` given how that run's label's daemon
 * takes a completion (`labelDaemon` of the sources its built-from's commit holds, rebuilt out of
 * `repos` by `recordedCheckout`) to read where an answer turns on it (completion-answers.ts's
 * `refusedCompletion` and `takenCompletions`), or `unread`'s, with why, where `judge` asked for
 * it and those sources hold no completion the reading knows, the rebuilt directory named by its
 * commit so no path of this process's is in a verdict. health.ts's completion rules and score.ts's
 * `unscored` read a run's daemon so, never by the label's name nor off the pod's checkout; a
 * Refused (`recordedCheckout`) and any other error `judge` throws is thrown on. */
export function judgedOnDaemon<T>(
  runDir: string,
  judge: (daemon: () => LabelDaemon) => T,
  unread: (why: string) => T,
  repos: readonly string[]
): T {
  let why: string | undefined;
  try {
    return judge(() => {
      const { dir, commit } = recordedCheckout(runDir, repos);
      try {
        return labelDaemon(dir);
      } catch (error) {
        why = (error instanceof Error ? error.message : String(error)).replaceAll(
          dir,
          `<commit ${commit}>`
        );
        throw error;
      }
    });
  } catch (error) {
    if (why === undefined) throw error;
    return unread(why);
  }
}

/** Whether `routes` (`labelRoutes`) serves a request of `method` to `asked`; `method` undefined
 * is a request whose method is not recorded, which any route of that path serves. A segment the
 * daemon names `{name}` matches any one non-empty segment, and `{name...}` the rest of the path,
 * as Go's ServeMux matches them. */
export function serves(
  routes: readonly string[],
  method: string | undefined,
  asked: string
): boolean {
  const parts = asked.split("/");
  return routes.some((pattern) => {
    const space = pattern.indexOf(" ");
    if (method !== undefined && pattern.slice(0, space) !== method) return false;
    const segments = pattern.slice(space + 1).split("/");
    for (const [at, segment] of segments.entries()) {
      if (/^\{[^}]*\.\.\.\}$/.test(segment)) return parts.length > at;
      const part = parts[at];
      if (part === undefined) return false;
      if (/^\{[^}]*\}$/.test(segment) ? part === "" : part !== segment) return false;
    }
    return parts.length === segments.length;
  });
}

/** The default the daemon of `checkout` gives the count setting `key` when legion.yaml leaves it
 * out, which is the value the rig's daemon runs with, having no legion.yaml: daemon-pane.go's
 * `count-default`, over that checkout's config package, every source of which the read parses and
 * so keys the answer on (`cachedRead`). Throws what the reading refused. */
export function countDefault(checkout: string, key: string): number {
  const daemon = daemonOf(checkout);
  const config = path.join(daemon, "internal", "config");
  const command = ["count-default", key];
  const printed = cachedRead(
    PROGRAM,
    () => runDaemonPane(DAEMON_MODULE, [...command, config]),
    command,
    daemon,
    goSources(config)
  ).trim();
  const value = Number(printed);
  if (!Number.isSafeInteger(value))
    throw new Error(`daemon-pane count-default ${key} printed ${JSON.stringify(printed)}`);
  return value;
}

/** The claim refusal `name` (claim.InvalidSecret, claim.StaleGeneration, …) of the daemon of
 * `checkout`, its status and its message: daemon-pane.go's `claim-refusal`, over that checkout's
 * claim source, which the stand-in answers a caller it refuses with. Throws what the reading
 * refused. */
export function claimRefusal(checkout: string, name: string): ClaimRefusal {
  const daemon = daemonOf(checkout);
  const claim = path.join(daemon, "internal", "claim");
  const command = ["claim-refusal", name];
  const [status = "", message = ""] = cachedRead(
    PROGRAM,
    () => runDaemonPane(DAEMON_MODULE, [...command, claim]),
    command,
    daemon,
    goSources(claim)
  ).split("\n");
  if (!/^\d+$/.test(status) || message === "")
    throw new Error(`daemon-pane claim-refusal ${name} printed no status and message`);
  return { status: Number(status), message };
}

/** Dispatch's refusal of a message the daemon posts (`postRefusal`): whether it is the refusal of
 * a decision block, rather than of a bound, and Dispatch's own sentence for it. */
export interface PostRefusal {
  readonly decision: boolean;
  readonly sentence: string;
}

/** Dispatch's refusal of a message the daemon of `checkout` posts with `body`, as that daemon
 * decides it before it records a move whose reason it posts, or undefined where Dispatch takes it:
 * daemon-pane.go's `post-check`, which runs that checkout's own reading (the longest post of the
 * body its outbox could make, read by Dispatch's reader) and so runs inside its module, as `phase`
 * does. The daemon stand-in asks it of a backward move's reason at the move, so a run reads it
 * through its label's build (`daemonPaneBuild`) in milliseconds; nothing is kept, since the
 * answer turns on the body and on the whole of Dispatch's reader. Throws what the program
 * refused, and for a printout that is neither nothing nor a kind and a sentence. */
export function postRefusal(checkout: string, body: string): PostRefusal | undefined {
  const printed = runDaemonPane(daemonOf(checkout), ["post-check"], body);
  if (printed === "") return undefined;
  const [kind = "", sentence = "", ...rest] = printed.split("\n");
  if (!["decision", "bound"].includes(kind) || sentence === "" || rest.join("") !== "")
    throw new Error(
      `daemon-pane post-check printed ${JSON.stringify(printed)}, which is no refusal's kind and sentence`
    );
  return { decision: kind === "decision", sentence };
}

/** Stands in for the phase the issue stands in, in the refusal `standinFacts` reads: the
 * stand-in replaces it with the phase it serves when it sends the refusal. */
export const PHASE_SLOT = "PHASESLOTOFTHEISSUE";
/** Stands in for the issue's key in that refusal: the stand-in replaces it with its run's issue.
 * Each run files an issue of its own, so a refusal read with the run's key is a fact no read
 * before the run (rig.sh's label_facts) could have read for it. */
export const ISSUE_SLOT = "ISSUESLOTOFTHERUN";

/** The phases that end with a handoff record (`phase.Handoff` in
 * packages/daemon/internal/phase/phase.go): the stand-in refuses a completion naming a record in
 * any other phase. daemon-pane.go answers only the role's own phase, at a read each, so the rest
 * is written here, and `standinFacts` refuses a daemon whose answer for the role's own phase
 * disagrees with it. */
export const RECORD_PHASES: readonly string[] = [
  "planning",
  "implementing",
  "testing",
  "reviewing",
];

/** A claim refusal of the daemon's (`claimRefusal`): its status and its sentence. */
export interface ClaimRefusal {
  readonly status: number;
  readonly message: string;
}

/** Every fact of a label's daemon the daemon stand-in answers by, read at its boot. */
export interface StandinFacts {
  /** The routes the daemon registers (`labelRoutes`): the one answer to "does this daemon serve
   * that route", which the stand-in asks of every request and health asks of every 404. */
  readonly routes: readonly string[];
  /** How the daemon takes a completion: its engine's answer to a late role and the phase its
   * route holds a completion's record to (`labelDaemon`), which the stand-in answers every
   * completion by. */
  readonly daemon: LabelDaemon;
  /** The run of refused turns at which the daemon fails the claim: the launch failure limit
   * (supervise's `turnOutcome`, `run < m.deps.Limits.LaunchFailures`), which the daemon is given
   * as `launch_failure_limit` and the rig, having no legion.yaml, runs at its default
   * (`countDefault`). Read only for a daemon that serves `claims/turn-outcome`, and 0 for one
   * that serves none. */
  readonly turnOutcomeLimit: number;
  /** The claim refusals the claim routes answer a caller with (packages/daemon/internal/claim's
   * `InvalidSecret`, `InvalidBootToken` and `StaleGeneration`, `claimRefusal`). */
  readonly invalidSecret: ClaimRefusal;
  readonly invalidBootToken: ClaimRefusal;
  readonly staleGeneration: ClaimRefusal;
  /** The phase the role's own worker is started in, the first of the transition table it works,
   * and the word of the handoff record that phase ends with, empty when it ends with none
   * (`labelPhase`). */
  readonly rolePhase: string;
  readonly handoffWord: string;
  /** The phase the issue stands in for the run: the one the stand-in is started in, or the
   * role's own when it is started in none. One that is not the role's is a run of a loop the
   * daemon starts no phase for, where the role's completion is a late one. */
  readonly phase: string;
  /** The daemon's own refusal of a late completion of the role, its code and its message
   * (`notCurrentPhase`), with `PHASE_SLOT` for the issue's phase and `ISSUE_SLOT` for its key:
   * the skill has the worker quote that sentence to its architect. Read only for a run whose
   * role can be late, one started in a phase that is not the role's or in `reviewing`, the one
   * phase the review round moves; undefined for any other. */
  readonly notCurrentPhase: Refusal | undefined;
  /** What the daemon's source says it posts, refuses and publishes (`labelShapes`). */
  readonly shapes: LabelShapes;
}

/** A text a label's daemon source holds, and the file, under the checkout's `packages/`, it is
 * read in. */
interface Marker {
  readonly file: string;
  readonly text: string;
}

/** One shape of a label's daemon source a reader pins: its name, the markers a daemon of that
 * shape holds, each in its file, and what the stand-in answers such a daemon by. */
interface SourceShape<T> {
  readonly name: string;
  readonly markers: readonly Marker[];
  readonly value: T;
}

/** The value of the one shape of `shapes` whose every marker the daemon source of `checkout`
 * holds, for `reader`. A source that holds every marker of no shape, or of two, is refused, naming
 * each marker of each shape it does not hold and the file it was looked for in: a marker this
 * cannot find is a daemon of no shape the stand-in has the answers of, or a marker gone stale, and
 * neither is read as an older daemon that lacks the behaviour (the architect's ruling of
 * 2026-11-18). daemon-pane.go's `addressingOf` reads a checkout the same way: the shapes pinned,
 * any other refused. */
function sourceShape<T>(checkout: string, reader: string, shapes: readonly SourceShape<T>[]): T {
  const texts = new Map<string, string>();
  const holds = ({ file, text }: Marker) => {
    const at = path.join(checkout, "packages", file);
    if (!texts.has(at)) texts.set(at, existsSync(at) ? readFileSync(at, "utf8") : "");
    return texts.get(at)?.includes(text) === true;
  };
  const held = shapes.filter(({ markers }) => markers.every(holds));
  const [one] = held;
  if (held.length === 1 && one !== undefined) return one.value;
  const why =
    held.length === 0
      ? shapes
          .map(
            ({ name, markers }) =>
              `${name} holds ${markers
                .filter((marker) => !holds(marker))
                .map(({ file, text }) => `${JSON.stringify(text)} in packages/${file}`)
                .join(" and ")}, and this source does not`
          )
          .join("; ")
      : `it holds the markers of two shapes at once, ${held.map(({ name }) => name).join(", and ")}`;
  throw new Error(
    `the daemon source of ${checkout} is no shape of ${reader} this rig reads: ${why}`
  );
}

/** What a label's daemon posts on the issue, refuses and publishes, as its source reads
 * (`labelShapes`). */
export interface LabelShapes {
  /** A worker's backward move posts its sentence on the issue (effects.go's `backwardMoveReason`,
   * main's since #4309, 26f27a997a). */
  readonly postsBackward: boolean;
  /** A backward move whose sentence Dispatch would refuse is refused (effects.go's
   * `backwardReasonRefusal`, main's since #4309). */
  readonly refusesReason: boolean;
  /** The daemon publishes the issue's role holders to a Dispatch that serves their route (its
   * client's `PutRoleHolders` and the route's row in Dispatch's table, main's since #4228,
   * 5a2d1f0637). */
  readonly publishesHolders: boolean;
  /** Every task of the reviewer ends with the daemon's two sentences of its review round: the
   * commit the reviewer's newest deciding review read, and the entry of its round
   * (review_round.go's `namingTheReviewRound`, through reviewround.go's `Sentences`, main's since
   * #4328, 480e2dc4ae), which `reviewRoundSentences` writes as that source does. */
  readonly namesReviewRound: boolean;
  /** A review the review App submitted is the reviewer's own, and so decides the round, only
   * where its Legion footer names the session the claims record holds for the claim the daemon
   * starts the reviewer under, which the reviewer's agent records at its registration; while that
   * claim records none, such a review waits (review.go's `byReviewer`, `reviewersClaim` and
   * `RefusedWhileNoSession`, main's since #4328). False for main `ff2a10ef`'s engine, which reads
   * the session off the claim the reviewer's row names, which only the reviewer's completion
   * writes there, and lets the login decide alone while the row names none. */
  readonly decidesBySession: boolean;
  /** A review the review App submitted decides the round only where the head its Legion footer
   * names, 40 lowercase hexadecimal characters, is the pull request's head, compared exactly, and
   * its decision is recorded against that head: a footer that names no head, or another, decides
   * nothing (review.go's `refusesRound` and `reviewHead`, intake's `LegionHead` and
   * `fullCommitSHA`, main's since #2539, a4c0ff994a, the same test reviewround's `IsHead` since
   * #3995). Its footer is decoded as a JSON object whose
   * `session` and `head` are strings, beside an `entry` kept raw (intake's `legionFooter`). False
   * for main `ff2a10ef`'s engine, which records a review's decision against its `commit_id` and
   * decodes the footer's `session` alone. */
  readonly decidesByFooterHead: boolean;
  /** The daemon records each run of a step of an issue on its Dispatch: an outbox `step_record`
   * row at the run's start and at its end, and one of the report alone of a completion that lands
   * after that end, whose executor sends `PUT /api/v1/issues/{key}/steps/{id}` under the daemon's
   * own session (outbox.go's `publishStepRecord` and `stepWriteOf`, workflow/steps.go's
   * `startStep`, `endStep` and `reportEndedStep`, main's since #4400, 53980e0e32, the steps of a
   * phase read off workflow/plan.go's `DefaultSteps` from #4488 and off the walk of the run since
   * #4611, 5dfd9cdbb0a), in the fields, the words, the places and the id step-records.ts restates
   * (`WRITES_STEPS` and `STEP_PUT`, with `STEPS_OF_PHASE` or `STEPS_ON_WALK`). False for main
   * `ff2a10ef`'s daemon, whose outbox has no such kind. */
  readonly writesSteps: boolean;
  /** A completion that carries a rating of the plan (`planRating` with `planReason`) and does not
   * end the retro is refused 400 PLAN_RATING_UNEXPECTED, in the words `PLAN_RATING_UNEXPECTED`
   * restates, by the workflow past HANDOFF_NOT_NEW, its fact left unspent (workflow/plan.go's
   * `planRatingRefusal`, engine.go's `handoff`, main's since #4488, a16c218ee3). False for main
   * `ff2a10ef`'s daemon, whose completion request carries no rating. */
  readonly refusesRating: boolean;
  /** The words of the decoder's error for a request body any string of which, a member's name or a
   * value, holds U+0000, which is refused 400 before its route reads it, naming the first such
   * string's place and where the character stands in it (claims.go's `decodeJSON` and
   * `refuseBadBody`, storable_text.go's `unstorableJSON`): `NUL_REFUSAL.refused` for main's daemon
   * since #4463, `NUL_REFUSAL.unstorable` for main's from #4488 until #4463, each read by its own
   * words (`refusesNul`) and either written in `{"error"}` (`NUL_WRITERS`). Undefined for main
   * `ff2a10ef`'s daemon, whose decoder reads no string of a body and whose apply of a completion or
   * a backward move it takes fails on such text where it writes it into a text or jsonb column,
   * answered 500 `FACT_APPLY_FAILED` (`STORES_NUL`). */
  readonly nulRefusal: string | undefined;
  /** The state's pull request carries `fixAttempts`, the count of heads pushed onto a red check
   * verdict (api's `PullRequestView.FixAttempts`, main `ff2a10ef`'s), which that label's contract
   * requires. False for main's daemon since contract 59 (#2780, 7e7bcf212f), which keeps no such
   * count (`servesFixAttempts`). */
  readonly servesFixAttempts: boolean;
  /** The contract the daemon states in `Legion-Daemon-Contract` on every answer and shapes its
   * bodies to, its `DaemonAPIVersion`, where it serves the answers that came with that (main's
   * since #3659, dc1bbc995e, `statesContract`): `GET /healthz` naming the project, the per-issue
   * record and its RECORD_NOT_FOUND, and a state marked with the contract and the claims' lowest,
   * each claim with its update state. Undefined for a daemon before it, whose health route
   * answers its status alone and whose answers state no contract. */
  readonly statesContract: number | undefined;
  /** A completion that sends `ready: true` is refused 400 READY_RETIRED, in the words
   * `READY_RETIRED` restates, once its grant is redeemed, and `ready: false` is ignored
   * (handoff.go's `readyRetired`, main's since #3659). False for main's daemon before it, whose
   * completion request has no `ready` (`REQUEST_WITHOUT_READY`), and for main `ff2a10ef`'s. */
  readonly retiresReady: boolean;
  /** The state's `repositories` block is the repositories the project's live records name,
   * `{live, withoutWebhookApp, caches}`, each `live` entry with its role Apps' installations
   * (state.go's `LiveRepositoryView`, main's since #4493, contract 202, `repositoriesLive`). False
   * for main's daemon before it and main `ff2a10ef`'s, whose block is the project's mapping,
   * `{owners, mapped, refused, withoutWebhookApp, caches}`. */
  readonly repositoriesLive: boolean;
}

const EFFECTS = "daemon/internal/workflow/effects.go";
/** The completion refusals every daemon the rig runs gives, each by its code in the file that
 * writes it: this checkout's and main `ff2a10ef`'s (2026-11-07) alike, so a source that names one
 * nowhere is a marker gone stale, which no older shape answers. */
const COMPLETION_REFUSALS = [
  ["HANDOFF_SESSION_MISMATCH", "daemon/internal/api/handoff.go"],
  ["HANDOFF_ALREADY_RECORDED", "daemon/internal/api/handoff.go"],
  ["HANDOFF_NOT_NEW", "daemon/internal/workflow/engine.go"],
] as const;
/** The completion route's refusals of a completion's verdict, in the words every daemon the rig
 * runs gives them (handoff.go's `answerCompletion`, this checkout's and main `ff2a10ef`'s alike):
 * a tester's that is neither `pass` nor `fail`, and one any other role reports. The daemon
 * stand-in answers by them; `labelShapes` holds a label's source to them (`verdictRule`). */
export const VERDICT_REFUSALS = {
  tester: { code: "TESTER_VERDICT_REQUIRED", message: "tester verdict must be pass or fail" },
  other: { code: "VERDICT_ROLE_FORBIDDEN", message: "only a tester may report a verdict" },
} as const;
/** How a daemon writes a coded refusal, `{code, error}` in each: main's since #3659 (dc1bbc995e)
 * hands the request to the one writer that shapes every answer to its caller's contract
 * (`s.writeFailure(w, r, <status>, …)`, credentials.go), and every daemon before it writes the body
 * itself (`writeFailure(w, <status>, …)`). A plain refusal is the same body at every contract
 * (shaping.go's `placedByPod`), so a reader of a refusal's words holds a source to either writer. */
const FAILURE_WRITERS = [
  {
    name: "main's daemon since #3659, whose refusals go through its answer writer",
    writer: "s.writeFailure(w, r, ",
  },
  { name: "every daemon the rig runs before #3659", writer: "writeFailure(w, " },
] as const;
/** The route's verdict rule as every daemon the rig runs has it, its refusals written by
 * `writer` (`FAILURE_WRITERS`): each condition, then its 400 in the words `VERDICT_REFUSALS`
 * restates. */
function verdictRule(writer: string): readonly Marker[] {
  return [
    {
      file: "daemon/internal/api/handoff.go",
      text: `if grant.Role == claim.RoleTester && req.Verdict != "pass" && req.Verdict != "fail" {\n\t\t${writer}http.StatusBadRequest, "${VERDICT_REFUSALS.tester.code}", "${VERDICT_REFUSALS.tester.message}")`,
    },
    {
      file: "daemon/internal/api/handoff.go",
      text: `if grant.Role != claim.RoleTester && req.Verdict != "" {\n\t\t${writer}http.StatusBadRequest, "${VERDICT_REFUSALS.other.code}", "${VERDICT_REFUSALS.other.message}")`,
    },
  ];
}
/** The workflow's status write as main `ff2a10ef`'s daemon has it, with no message to post: what
 * that older daemon has in the place of each sentence a move posts on the issue, and so of the
 * refusal of a backward move's reason that sentence would carry. */
const STATUS_WITHOUT_MESSAGE: Marker = {
  file: EFFECTS,
  text: "func (e *Engine) status(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {",
};
/** The outbox's kinds as main `ff2a10ef`'s daemon has them, ending at the gated steps'
 * publication with no kind of the role holders': what that older daemon has in the place of
 * their publication. */
const OUTBOX_WITHOUT_HOLDERS: Marker = {
  file: "daemon/internal/record/outbox.go",
  text: '\tOutboxKindGatedStepsPublish OutboxKind = "gated_steps_publish"\n)',
};
/** The file that words the two sentences ending a reviewer's task (main's since #4328). */
const ROUND_WORDS = "daemon/internal/reviewround/reviewround.go";
/** Those sentences' words as reviewround.go declares them: the label of the commit, the label of
 * the entry, and the word either names where it has no value. */
const ROUND_COMMIT = "Last reviewed commit";
const ROUND_ENTRY = "Review entry";
const ROUND_NONE = "none";
/** What a daemon that ends every task of the reviewer with those sentences holds: the function
 * that appends them to each task of the reviewer, the three words above as their constants
 * declare them, and the one return of `Sentences` that joins them, so a source that words or
 * joins the sentences otherwise is of no shape this reads. */
const NAMES_ROUND: readonly Marker[] = [
  { file: "daemon/internal/workflow/review_round.go", text: "func namingTheReviewRound(" },
  { file: ROUND_WORDS, text: `const CommitLabel = "${ROUND_COMMIT}"` },
  { file: ROUND_WORDS, text: `const EntryLabel = "${ROUND_ENTRY}"` },
  { file: ROUND_WORDS, text: `const none = "${ROUND_NONE}"` },
  {
    file: ROUND_WORDS,
    text: 'return " " + CommitLabel + ": " + commit + ". " + EntryLabel + ": " + named + "."',
  },
];
/** The queue's row as main `ff2a10ef`'s daemon builds it, from the payload as it came: what that
 * older daemon has in the place of the sentences, which main's since #4328 writes into each task
 * of the reviewer as it queues it (effects.go's `queuedRow`). */
const ROW_AS_QUEUED: Marker = {
  file: EFFECTS,
  text: "payload record.OutboxPayload, at time.Time) error {\n\trow, err := record.NewOutboxRow(issue, payload, at)",
};
/** The file the engine judges who wrote a review in. */
const REVIEW = "daemon/internal/workflow/review.go";
/** Who a review of the review App is the reviewer's own for, as main's engine since #4328 judges
 * it: the session the reviewer's claim records, found by the claim's token, and none while it
 * records none. */
const BY_RECORDED_SESSION: readonly Marker[] = [
  {
    file: REVIEW,
    text: 'return e.byReviewApp(review.Author) && reviewerSession != "" && review.LegionSession() == reviewerSession',
  },
  { file: REVIEW, text: "func (e *Engine) reviewersClaim(" },
];
/** The same judgement as main `ff2a10ef`'s engine makes it: any session while the reviewer's row
 * names no claim, read off the claim that row names (`claimSession`). */
const BY_ROW_SESSION: readonly Marker[] = [
  {
    file: REVIEW,
    text: 'return e.byReviewApp(review.Author) && (reviewerSession == "" || review.LegionSession() == reviewerSession)',
  },
  { file: REVIEW, text: "func (e *Engine) claimSession(" },
];
/** The file the daemon's intake reads a review's Legion footer in. */
const FACTS = "daemon/internal/intake/facts.go";
/** Where every daemon the rig runs finds a review's Legion footer: the JSON object between the
 * last `<!-- legion:` of its body and the first `-->` after it. */
const FOOTER_FOUND: readonly Marker[] = [
  { file: FACTS, text: 'const legionFooterMarker = "<!-- legion:"' },
  { file: FACTS, text: "at := strings.LastIndex(r.Body, legionFooterMarker)" },
  { file: FACTS, text: 'end := strings.Index(rest, "-->")' },
];
/** The head a review of the review App decides on, as main's engine since #2539 reads it: the
 * footer decoded whole into `session` and `head` strings and a raw `entry`, a decoding error
 * costing the whole footer; its head a commit only at 40 lowercase hexadecimal characters
 * (`HEAD_SPELLED`, in either of its two places); a review App review that names no head, or
 * another than the pull request's, refused; and the decision of a review App review recorded
 * against that head, a person's against its `commit_id`. */
const BY_FOOTER_HEAD: readonly Marker[] = [
  ...FOOTER_FOUND,
  {
    file: FACTS,
    text: 'type legionFooter struct {\n\tSession string          `json:"session"`\n\tHead    string          `json:"head"`\n\tEntry   json.RawMessage `json:"entry"`\n}',
  },
  {
    file: FACTS,
    text: "var footer legionFooter\n\tif err := json.Unmarshal([]byte(rest[:end]), &footer); err != nil {\n\t\treturn legionFooter{}\n\t}",
  },
  { file: REVIEW, text: 'switch head := review.LegionHead(); {\n\tcase head == "":' },
  { file: REVIEW, text: "\tcase head != pr.HeadSHA:" },
  {
    file: REVIEW,
    text: "reviewer.Decision = &record.ReviewDecision{State: state, Body: fact.Body, Head: e.reviewHead(fact)}",
  },
  {
    file: REVIEW,
    text: "if e.byReviewApp(review.Author) {\n\t\treturn review.LegionHead()\n\t}\n\treturn review.CommitID",
  },
];
/** Where main's engine reads a footer's head as a commit, 40 lowercase hexadecimal characters,
 * nothing shorter compared with a head: since #2539, intake's own `fullCommitSHA`; since #3995
 * (b537812fb5), which moved that test unchanged into reviewround's `IsHead` (the spelling a task's
 * review-round sentences are read back by too), that one. The same decision in either place. */
const HEAD_SPELLED: Readonly<Record<"since2539" | "since3995", readonly Marker[]>> = {
  since2539: [
    {
      file: FACTS,
      text: 'head := r.lastLegionFooter().Head\n\tif !fullCommitSHA(head) {\n\t\treturn ""\n\t}',
    },
    { file: FACTS, text: "\tif len(s) != 40 {\n\t\treturn false\n\t}" },
    { file: FACTS, text: "if (c < '0' || c > '9') && (c < 'a' || c > 'f') {" },
  ],
  since3995: [
    {
      file: FACTS,
      text: 'head := r.lastLegionFooter().Head\n\tif !reviewround.IsHead(head) {\n\t\treturn ""\n\t}',
    },
    { file: ROUND_WORDS, text: "func IsHead(commit string) bool {\n\tif len(commit) != 40 {" },
    { file: ROUND_WORDS, text: "\tif len(commit) != 40 {\n\t\treturn false\n\t}" },
    {
      file: ROUND_WORDS,
      text: "for _, c := range []byte(commit) {\n\t\tif (c < '0' || c > '9') && (c < 'a' || c > 'f') {",
    },
  ],
};
/** The same as main `ff2a10ef`'s engine has it: the footer's `session` decoded alone, and every
 * review's decision recorded against its `commit_id`, which no footer's head refuses. */
const BY_COMMIT_ID: readonly Marker[] = [
  ...FOOTER_FOUND,
  {
    file: FACTS,
    text: 'var footer struct {\n\t\tSession string `json:"session"`\n\t}\n\tif err := json.Unmarshal([]byte(rest[:end]), &footer); err != nil {',
  },
  {
    file: REVIEW,
    text: "reviewer.Decision = &record.ReviewDecision{State: state, Body: fact.Body, Head: fact.CommitID}",
  },
];

/** The files the daemon writes a run's record of a step through (main's since #4400); the one it
 * read the steps of a phase from from #4488 until #4611, which builds a plan's rows of steps since;
 * and those main's daemon since #4611 (5dfd9cdbb0a) reads the step a run stands at from: the walk
 * of the run (line.go), the roles a step runs as and a task's line (plan/plan.go), a task's place
 * (record/plan.go), the project's default steps (config/stage3.go) and today's line of phases
 * (phase/phase.go). */
const STEPS_GO = "daemon/internal/workflow/steps.go";
const PLAN_GO = "daemon/internal/workflow/plan.go";
const DAEMON_OUTBOX = "daemon/internal/daemon/outbox.go";
const LINE_GO = "daemon/internal/workflow/line.go";
const PLAN_PACKAGE_GO = "daemon/internal/plan/plan.go";
const RECORD_PLAN_GO = "daemon/internal/record/plan.go";
const STAGE3_GO = "daemon/internal/config/stage3.go";
const PHASE_GO = "daemon/internal/phase/phase.go";
/** `word` after `prefix`, its first letter capitalised, as a Go constant names it
 * (`steprecord.KindStep`, `TypeReview`, `record.PlaceRetro`, …). */
const goConstant = (prefix: string, word: string) =>
  `${prefix}${word[0]?.toUpperCase() ?? ""}${word.slice(1)}`;
/** `kind` as the constant of steprecord names it (`steprecord.KindStep`, `KindRetro`, …). */
const kindConstant = (kind: string) => goConstant("steprecord.Kind", kind);
/** STEPS' runs of a step of the task, at a place from 1, and its retro and its merge, at place 0,
 * each in STEPS' order. */
const STEP_RUNS = Object.values(STEPS).filter(({ kind }) => kind === "step");
const ACT_RUNS = Object.values(STEPS).filter(({ kind }) => kind !== "step");
/** The texts every daemon that records each run of a step holds, each built from step-records.ts's
 * tables where one restates it, so a daemon that writes another field, word, result or id is of no
 * shape the rig reads: the outbox's kind, its executor's write of every field of the row
 * (`stepWriteOf`) through the client's route and the body that route takes, and the wait `execute`
 * makes of the router's 404 of a Dispatch older than the route (`waitsForTheRoute`, for the record
 * lane and the role holders' publication); the result of each move (`endOf`, which ends no step
 * stopped since #4464: a head that conflicts with its base moves no issue); the id's namespace and
 * name. The client's call of the route under the daemon's actor is in `STEP_PUT`, as each shape's
 * client spells that actor. The identity every write carries (`stepWrite`), the moment alone at
 * the start and the report alone after the end are in `STEPS_WITHOUT_VARIANT` (before #5014) or
 * `STEPS_WITH_VARIANT` (since #5014, where each write carries the issue's experiment and variant,
 * nil for an issue of no group). Where the task, its steps and a phase's place are read, and how
 * the end is written, are each shape's own (`STEPS_OF_PHASE`, `STEPS_ON_WALK`). */
const WRITES_STEPS: readonly Marker[] = [
  {
    file: "daemon/internal/record/outbox.go",
    text: 'OutboxKindStepRecord OutboxKind = "step_record"',
  },
  {
    file: DAEMON_OUTBOX,
    text: "func stepWriteOf(payload record.StepRecord) dispatch.StepWrite {\n\treturn dispatch.StepWrite{Task: payload.Task, Step: payload.Step, Kind: payload.Kind, Role: payload.Role,\n\t\tType: payload.Type, Agent: payload.Agent, Generation: payload.Generation, Session: payload.Session,\n\t\tModel: payload.Model, StartedAt: payload.StartedAt, EndedAt: payload.EndedAt, Result: payload.Result,\n\t\tReturnedTo: payload.ReturnedTo, Report: payload.Report}",
  },
  {
    file: DAEMON_OUTBOX,
    text: "if err := r.dispatch.PutStep(ctx, row.Issue, payload.ID, stepWriteOf(payload)); err != nil {",
  },
  {
    file: DAEMON_OUTBOX,
    text: "if waitsForTheRoute(row.Kind) && dispatch.NoRoute(err) {\n\t\treturn wait.Errorf(",
  },
  {
    file: DAEMON_OUTBOX,
    text: "return record.LaneOf(kind) == record.LaneRecord || kind == record.OutboxKindRoleHoldersPublish",
  },
  {
    file: "daemon/internal/dispatch/types.go",
    text: `type StepWrite struct {\n${STEP_WRITE_FIELDS.map(([name, type, key]) => `\t${name.padEnd(11)}${type.padEnd(11)}\`json:"${key}"\``).join("\n")}\n}`,
  },
  {
    file: STEPS_GO,
    text: `\tswitch trigger {\n\tcase ${FAILED_TRIGGERS.join(", ")}:\n\t\treturn ResultFailed, true\n\t}\n\treturn ResultPassed, false`,
  },
  {
    file: STEPS_GO,
    text: `var stepRecordNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("${STEP_NAMESPACE_NAME}"))`,
  },
  {
    file: STEPS_GO,
    text: 'name := fmt.Sprintf("%s/%d/%s/%s/%d/%d", issue, generation, place.Task, place.Kind, place.Place, starts)',
  },
];
/** The client's call of the route of a step record (client.go's `PutStep`), under the actor the
 * daemon writes it as, as main's daemon spells it: `daemonActor(key)`, the session of the key's
 * project, from #4400 (53980e0e32c) until #4682 (5833fb7f86a), and `c.actor(key)` since, the
 * session the client writes as (`WritesAs`) or, for a client made with no option, the session of
 * the key's project (`DaemonSession`). Under dispatch_token_broker the daemon's client writes as
 * the session of its own project, so on an issue of that project either spelling names
 * `legion-daemon:<PROJECT>`, as the stand-in writes. */
const putStep = (actor: string): Marker => ({
  file: "daemon/internal/dispatch/client.go",
  text: `return c.request(ctx, http.MethodPut, "/api/v1/issues/"+url.PathEscape(key)+"/steps/"+url.PathEscape(id), stepWrite{\n\t\tStepWrite: write,\n\t\tActor:     ${actor},\n\t}, nil)`,
});
const STEP_PUT: Readonly<Record<"since4400" | "since4682", Marker>> = {
  since4400: putStep("daemonActor(key)"),
  since4682: putStep("c.actor(key)"),
};
/** The three `stepWrite` lines common to main's shapes from #4488 until #5014 (0986c960d9e),
 * before each write carried the issue's experiment and variant (`drawn`). */
const STEPS_WITHOUT_VARIANT: readonly Marker[] = [
  {
    file: STEPS_GO,
    text: "return record.StepRecord{ID: StepID(issue.Key, issue.Generation, place, starts), Task: place.Task,\n\t\tStep: place.Place, Kind: place.Kind, Role: string(role), Type: string(typ), Agent: agent, Generation: issue.Generation}",
  },
  {
    file: STEPS_GO,
    text: "write := stepWrite(issue, step.place, step.role, step.typ, string(agent), starts)\n\t\twrite.StartedAt = new(now)\n\t\treturn write",
  },
  {
    file: STEPS_GO,
    text: "write := stepWrite(issue, step.place, step.role, step.typ, latest.Agent, latest.Starts)\n\twrite.Report = new(report)",
  },
];
/** The end's `stepWrite` on the walk from #4611 until #5014, without `drawn`. */
const STEP_END_ON_WALK_WITHOUT_VARIANT: Marker = {
  file: STEPS_GO,
  text: "return stepWrite(before, step.place, step.role, step.typ, start.Agent, start.Starts)",
};
/** The same four `stepWrite` lines since #5014, where each write carries `drawn` (the issue's
 * experiment and variant), and `variantOf`'s nil return and the outbox's nil skip: a world issue
 * belongs to no experiment group, so its `StepExperiment` is nil and its step write carries
 * neither, and no experiment route is called. */
const STEPS_WITH_VARIANT: readonly Marker[] = [
  {
    file: STEPS_GO,
    text: "return record.StepRecord{ID: StepID(issue.Key, issue.Generation, place, starts), Task: place.Task,\n\t\tStep: place.Place, Kind: place.Kind, Role: string(role), Type: string(typ), Agent: agent, Generation: issue.Generation,\n\t\tStepExperiment: drawn}",
  },
  {
    file: STEPS_GO,
    text: "write := stepWrite(issue, drawn, step.place, step.role, step.typ, string(agent), starts)\n\t\twrite.StartedAt = new(now)\n\t\treturn write",
  },
  {
    file: STEPS_GO,
    text: "write := stepWrite(issue, drawn, step.place, step.role, step.typ, latest.Agent, latest.Starts)\n\twrite.Report = new(report)",
  },
  {
    file: STEPS_GO,
    text: "return stepWrite(before, drawn, step.place, step.role, step.typ, start.Agent, start.Starts)",
  },
  {
    file: STEPS_GO,
    text: 'if err != nil || group.Experiment == "" || group.Variant == "" {\n\t\treturn nil, err\n\t}',
  },
  {
    file: DAEMON_OUTBOX,
    text: "if err := publish(ctx, row, value); err != nil || value.StepExperiment == nil {",
  },
];
/** The end's moments, result, return, report and agent's session and model, its agent read in the
 * fact's own transaction (`e.readAgent(ctx, tx, …)`, main's since #4220, 46795520eb), which names
 * the claim's session only while the claim holds it: a world's one claim holds the session it
 * registered, so the end names that session, as before #4220. `startedAt` and `agent` are how a
 * shape's source names the moment the run's start wrote and the claim it read the run's agent by;
 * the two shapes write the end alike but for those names. */
const endWrite = (startedAt: string, agent: string) =>
  `if !${startedAt}.IsZero() {\n\t\twrite.StartedAt = new(${startedAt})\n\t}\n\twrite.EndedAt, write.Result, write.ReturnedTo = new(e.now()), new(string(end.Result)), end.ReturnedTo\n\tif end.Report != "" {\n\t\twrite.Report = new(end.Report)\n\t}\n\tread := e.readAgent(ctx, tx, ${agent})\n\tif read.Session != "" {\n\t\twrite.Session = new(read.Session)\n\t}\n\tif read.Model != "" {\n\t\twrite.Model = new(read.Model)\n\t}`;
/** What main's daemon from #4488 until #4611 holds where the step of a phase is read and the end
 * written: the task (steps.go's `defaultTask`); the steps of the task in order (plan.go's
 * `DefaultSteps`), the role and the type of the step each working phase runs (`stepWork`), its
 * place, one plus the index of the first step of its role (`stepIn`), and the retro's and the
 * merge's step at place 0, which steps.go's `stepOf` reads through `stepIn` over `DefaultSteps`;
 * the end (`endStep`); and the return an end names (`endOfTransition`, the place of the step the
 * work goes back into). */
const STEPS_OF_PHASE: readonly Marker[] = [
  { file: STEPS_GO, text: `const defaultTask = "${STEP_TASK}"` },
  { file: STEPS_GO, text: "\treturn stepIn(DefaultSteps(), p)\n}" },
  {
    file: PLAN_GO,
    text: `func DefaultSteps() []string {\n\treturn []string{${DEFAULT_STEPS.map((step) => JSON.stringify(step)).join(", ")}}\n}`,
  },
  {
    file: PLAN_GO,
    text: `\tswitch p {\n${STEP_RUNS.map(({ go, role, type }) => `\tcase phase.${go}:\n\t\treturn "${role}", "${type}", true`).join("\n")}\n\t}\n\treturn "", "", false`,
  },
  {
    file: PLAN_GO,
    text: `\tswitch p {\n${ACT_RUNS.map(
      ({ go, kind, role, type }) =>
        `\tcase phase.${go}:\n\t\treturn record.StepPlace{Task: defaultTask, Kind: ${kindConstant(kind)}}, "${role}", "${type}", true`
    ).join("\n")}\n\t}\n\trole, typ, ok = stepWork(p)`,
  },
  {
    file: PLAN_GO,
    text: `if head, _, _ := strings.Cut(named, ":"); stepRole(head) == role {\n\t\t\treturn record.StepPlace{Task: defaultTask, Kind: ${kindConstant("step")}, Place: i + 1}, role, typ, true`,
  },
  {
    file: STEPS_GO,
    text: `write := stepWrite(before, step.place, step.role, step.typ, string(agent), start.Starts)\n\t${endWrite("startedAt", "agent")}`,
  },
  {
    file: STEPS_GO,
    text: "if into, _, _, ok := stepOf(to); returns && ok {\n\t\tend.ReturnedTo = new(into.Place)\n\t}\n\tif handoff.Role == phase.RoleFor(from) {\n\t\tend.Report = handoff.HandoffRecord\n\t}",
  },
];
/** What main's daemon since #4611 (5dfd9cdbb0a) holds in their place, where the step a run stands
 * at is read off the walk of its run (line.go's `walk`): a run with plan rows walks its one task's
 * steps, which the first step of each run of this release writes, the project's default steps for
 * an owner who gives no plan (workflow/plan.go's `planFirstStep` and `defaultRows`); a run with
 * none walks today's line, `phase.Order`, as the default steps of a project that names none
 * (`todaysWalk`, `todaysSteps`). No owner gives a world's issue a plan and the rig's project names
 * no `default_steps`, so either walk runs the steps DEFAULT_STEPS restates, and both place a
 * phase's step at the place STEPS restates, as these hold: the task's id (steps.go's
 * `defaultTask`, plan's `DefaultTask`); the default steps (config's `defaultStepWords`, read where
 * a project names none, `RunsByDefault`) as the rows of both walks' steps (`planSteps`: each at
 * one plus its index, with its role's type); the type and the phase each step's role runs as
 * (plan's `builtinRoles` and its types' words), which a stored walk's line reads (`plan.Line`),
 * and today's line, in the same order (phase's `Order`); a phase's place, one plus the index of
 * its step on the line but for the retro and the merge, each at place 0 (`PlaceOf`), the role and
 * the type each place runs (`stepOfWalk`) and the place's kind as steprecord names it (record's
 * `StepPlace` and its kinds), under the walk's task (`placeOnWalk`); the end, which `endStep`
 * writes through `endRun` with the identity it builds from the start the end reads; and the return
 * an end names (`endOfTransition`, through `placeOnWalk`). At main 7e75126f: steps.go:22, :45-61,
 * :105, :213 and :230-245, :434-439; line.go:54, :61, :65, :81, :103-122; workflow/plan.go:194 and
 * :202-203; plan/plan.go:30-33, :50, :116-122 and :163-166; record/plan.go:55-58 and :85;
 * config/stage3.go:123, :127 and :141-144; phase/phase.go:63. */
const STEPS_ON_WALK: readonly Marker[] = [
  { file: STEPS_GO, text: "const defaultTask = plan.DefaultTask" },
  { file: PLAN_PACKAGE_GO, text: `const DefaultTask = "${STEP_TASK}"` },
  {
    file: STAGE3_GO,
    text: `var defaultStepWords = []string{${DEFAULT_STEPS.map((step) => JSON.stringify(step)).join(", ")}}`,
  },
  { file: STAGE3_GO, text: 'steps, err := plan.ParseSteps(strings.Join(defaultStepWords, " "))' },
  {
    file: STAGE3_GO,
    text: "\tif p.DefaultSteps != nil {\n\t\treturn slices.Clone(p.DefaultSteps)\n\t}\n\treturn slices.Clone(unsetDefaultSteps)",
  },
  {
    file: PLAN_GO,
    text: "planSteps(issue.Key, issue.Generation, defaultTask, e.cfg.DefaultSteps)",
  },
  {
    file: LINE_GO,
    text: 'var todaysSteps = planSteps("", 0, defaultTask, config.Project{}.RunsByDefault())',
  },
  {
    file: PLAN_GO,
    text: "rows[i] = record.PlanStep{Issue: issue, Generation: generation, Task: task, Position: i + 1, Role: step.Role,\n\t\t\tType: builtinDeployment.Roles[step.Role].Type, AgentKey: step.Key}",
  },
  {
    file: LINE_GO,
    text: "var builtinDeployment = plan.Deployment{Roles: plan.BuiltinRoles()}",
  },
  {
    file: PLAN_PACKAGE_GO,
    text: `var builtinRoles = []RoleDef{\n${STEP_RUNS.map(({ go, role, type }) => `\t{Name: "${role}", Type: ${goConstant("Type", type)}, Phase: phase.${go}},`).join("\n")}\n}`,
  },
  ...[...new Set(STEP_RUNS.map(({ type }) => type))].map((type) => ({
    file: PLAN_PACKAGE_GO,
    text: `\t${goConstant("Type", type).padEnd(18)} Type = "${type}"`,
  })),
  {
    file: PLAN_PACKAGE_GO,
    text: "\tfor _, step := range steps {\n\t\tline = append(line, d.Roles[step.Role].Phase)\n\t}\n\treturn append(line, phase.Retro, phase.Merging)",
  },
  { file: LINE_GO, text: "line := plan.Line(namedSteps(steps), builtinDeployment)" },
  {
    file: LINE_GO,
    text: "func todaysWalk() Walk { return Walk{Task: defaultTask, Steps: todaysSteps, Line: phase.Order} }",
  },
  {
    file: PHASE_GO,
    text: `var Order = []Phase{${Object.values(STEPS)
      .map(({ go }) => go)
      .join(", ")}}`,
  },
  {
    file: LINE_GO,
    text: `\tswitch p {\n${ACT_RUNS.map(({ go, kind }) => `\tcase phase.${go}:\n\t\treturn record.TaskPlace{Kind: ${goConstant("record.Place", kind)}}, true`).join("\n")}\n\t}\n\tif at := slices.Index(w.steps(), p); at >= 0 {\n\t\treturn record.TaskPlace{Kind: record.PlaceStep, Position: at + 1}, true\n\t}`,
  },
  { file: LINE_GO, text: "\treturn w.Line[:len(w.Line)-2]" },
  {
    file: STEPS_GO,
    text: `\tswitch place.Kind {\n${ACT_RUNS.map(({ kind, role, type }) => `\tcase ${goConstant("record.Place", kind)}:\n\t\treturn "${role}", "${type}", true`).join("\n")}\n\tcase record.PlaceStep:\n\t\tif place.Position >= 1 && place.Position <= len(w.Steps) {\n\t\t\tnamed := w.Steps[place.Position-1]\n\t\t\treturn stepRole(named.Role), named.Type, true\n\t\t}\n\t}`,
  },
  { file: STEPS_GO, text: "return stands.StepPlace(w.Task), role, typ, true" },
  { file: RECORD_PLAN_GO, text: "return StepPlace{Task: task, Kind: p.Kind, Place: p.Position}" },
  {
    file: RECORD_PLAN_GO,
    text: [...new Set(Object.values(STEPS).map(({ kind }) => kind))]
      .map((kind) => `\t${goConstant("Place", kind).padEnd(10)} = ${kindConstant(kind)}`)
      .join("\n"),
  },
  {
    file: STEPS_GO,
    text: `write := identity(start)\n\t${endWrite("start.StartedAt", "claim.Token(start.Agent)")}`,
  },
  {
    file: STEPS_GO,
    text: "if into, _, _, ok := placeOnWalk(w, to); returns && ok {\n\t\tend.ReturnedTo = new(into.Place)\n\t}\n\tif handoff.Role == phase.RoleFor(from) {\n\t\tend.Report = handoff.HandoffRecord\n\t}",
  },
];
/** Dispatch's route of a step write, which every daemon that records each run of a step writes
 * through. */
const STEP_ROUTE: Marker = {
  file: "envoy/internal/dispatch/api/routes_table.go",
  text: '{http.MethodPut, "/api/v1/issues/{key}/steps/{id}", authAny,',
};

/** The refusal of a completion that rates the plan and does not end the retro, as main's daemon
 * since #4488 words it: its code, the description of the completion the workflow fills with the
 * phase it is of and the issue (workflow/plan.go's `planRatingRefusal`), and the message the
 * intake fills with that description (intake's `PlanRatingUnexpected`). The daemon stand-in
 * answers by them; `labelShapes` holds a label's source to them (`REFUSES_RATING`). */
export const PLAN_RATING_UNEXPECTED = {
  code: "PLAN_RATING_UNEXPECTED",
  what: "this completion of phase %s of %s",
  message:
    "%s names planRating or planReason, and only the completion that ends the retro rates the plan; send it again without both, and nothing was recorded",
} as const;
/** What a daemon that refuses such a completion holds: the route's request taking both members and
 * handing the rating to the fact; the workflow's refusal past HANDOFF_NOT_NEW with its fact left
 * unspent, of every completion that does not end the retro; and its description and message in
 * the words `PLAN_RATING_UNEXPECTED` restates. The request's members end at the rating in main's
 * daemon from #4488 until #3659, and #3659 reads `ready` after them (`RETIRES_READY`), so the
 * marker names the two members and not the end of the request. The fact ends at the rating from
 * #4488 until #4611, and #4611 (5dfd9cdbb0a) hands it the step a fail returns to after it
 * (`ReturnTo: returnTo`, handoff.go:428-429 at main 7e75126f), so that marker names the rating and
 * not the end of the fact. */
const REFUSES_RATING: readonly Marker[] = [
  {
    file: "daemon/internal/api/handoff.go",
    text: 'PlanRating     *int       `json:"planRating,omitempty"`\n\tPlanReason     *string    `json:"planReason,omitempty"`',
  },
  {
    file: "daemon/internal/api/handoff.go",
    text: "PlanRating: rating, MustRate: c.PluginContract >= PlanRatingContract",
  },
  {
    file: "daemon/internal/workflow/engine.go",
    text: "if refusal := planRatingRefusal(fact, current && issue.Phase == phase.Retro, reported); refusal != nil {\n\t\treturn intake.Result{Refusal: refusal, Unspent: true}, nil\n\t}",
  },
  {
    file: PLAN_GO,
    text: `what := fmt.Sprintf("${PLAN_RATING_UNEXPECTED.what}", reported, fact.Issue)`,
  },
  {
    file: PLAN_GO,
    text: "case !endsRetro && fact.PlanRating != nil:\n\t\treturn intake.PlanRatingUnexpected(what)",
  },
  {
    file: "daemon/internal/intake/facts.go",
    text: `func PlanRatingUnexpected(what string) *Refusal {\n\treturn &Refusal{Status: 400, Code: "${PLAN_RATING_UNEXPECTED.code}", Message: fmt.Sprintf(\n\t\t"${PLAN_RATING_UNEXPECTED.message}", what)}`,
  },
];
/** The completion request as main `ff2a10ef`'s route decodes it, ending at `wakeAt` with no rating
 * of the plan: what that older daemon has in the place of the refusal. */
const REQUEST_WITHOUT_RATING: Marker = {
  file: "daemon/internal/api/handoff.go",
  text: 'WakeAt         *time.Time `json:"wakeAt,omitempty"`\n}',
};

/** The refusal of a completion that sends `ready: true`, as main's daemon since #3659 words it
 * (handoff.go's `readyRetired`, three Go literals joined): `ready` armed the daemon's merge at
 * contract 85 and below, and no daemon from 86 on merges. The daemon stand-in answers by it;
 * `labelShapes` holds a label's source to it (`RETIRES_READY`). */
const READY_RETIRED_PARTS = [
  "this completion sends ready: true, which at contract 85 and below armed the daemon's merge of the pull request. ",
  "No daemon at contract 86 or later acts on READY: the merger merges the pull request itself, and the daemon closes ",
  "the issue when it reads that merge. Nothing was recorded. Merge the pull request, then complete again without ready",
] as const;
export const READY_RETIRED = {
  code: "READY_RETIRED",
  message: READY_RETIRED_PARTS.join(""),
} as const;
/** What a daemon that refuses `ready: true` holds: the request reading `ready` by name, last of
 * its members, the 400 the route writes for `true` once the grant is redeemed, and the sentence
 * as `READY_RETIRED` restates it. `false` and no `ready` at all are ignored. */
const RETIRES_READY: readonly Marker[] = [
  { file: "daemon/internal/api/handoff.go", text: 'Ready *bool `json:"ready,omitempty"`\n}' },
  {
    file: "daemon/internal/api/handoff.go",
    text: `if req.Ready != nil && *req.Ready {\n\t\ts.writeFailure(w, r, http.StatusBadRequest, "${READY_RETIRED.code}", readyRetired)\n\t\treturn\n\t}`,
  },
  {
    file: "daemon/internal/api/handoff.go",
    text: `const readyRetired = ${READY_RETIRED_PARTS.map((part) => JSON.stringify(part)).join(" +\n\t")}`,
  },
];
/** The completion request as main's route decodes it from #4488 until #3659, ending at the rating
 * of the plan with no `ready`: what that daemon has in the place of the refusal. */
const REQUEST_WITHOUT_READY: Marker = {
  file: "daemon/internal/api/handoff.go",
  text: 'PlanReason     *string    `json:"planReason,omitempty"`\n}',
};

/** The refusal of a request body holding U+0000 in a string: the sentence every undecodable body
 * is refused 400 with (claims.go's `refuseBadBody`), around the decoder's error, which names the
 * first such string's place in the body and where the first U+0000 stands in it, counted from 1 in
 * code points (storable_text.go's `unstorableText`), in the words of main's daemon since #4463
 * (`refused`, 2d3d73331b) or of main's from #4488 until #4463 (`unstorable`). On every route the
 * daemon stand-in models, #4463 changed those words alone: its store keeps U+FFFD for a U+0000 and
 * for a byte that is not UTF-8 in the text it writes (storetext's `Text`), but its decoder still
 * refuses a body holding U+0000 before any route reads it (claims.go's `decodeJSON`), and a byte
 * that is not UTF-8 is decoded as U+FFFD before any store sees it, by every daemon the rig runs
 * (Go's encoding/json) and by the stand-in (`request.json()`) alike. */
export const NUL_REFUSAL = {
  prefix: "invalid request body: ",
  refused:
    "%s holds a NUL character (U+0000) at character %d, which the daemon API refuses rather than store it as U+FFFD",
  unstorable: "%s holds a NUL character (U+0000) at character %d, which the daemon cannot store",
} as const;
/** What a daemon that refuses such a body in the words `format` holds: the decoder of every
 * request body ending in the walk of its strings (`decodeJSON`'s `unstorableJSON`, through
 * Dispatch's own walk, `dispatchbody.NulInJSON`) and the error in those words. */
function refusesNul(format: string): readonly Marker[] {
  return [
    { file: "daemon/internal/api/claims.go", text: "\treturn body, unstorableJSON(body)\n}" },
    {
      file: "daemon/internal/api/storable_text.go",
      text: "place, value, found := dispatchbody.NulInJSON(body)",
    },
    {
      file: "daemon/internal/api/storable_text.go",
      text: `return fmt.Errorf("${format}", place, utf8.RuneCountInString(value[:at])+1)`,
    },
  ];
}
/** The 400 a daemon that refuses such a body writes the error in, `{"error"}` in each: main's since
 * #3659 through the writer that shapes every answer (`s.refuse`, credentials.go), and every daemon
 * from #4488 until it as a body of its own (`errorBody`). Read beside the words, whichever they
 * are, since #3659 moved the writer and kept them. */
const NUL_WRITERS = [
  {
    name: "main's daemon since #3659, whose refusals go through its answer writer",
    markers: [
      {
        file: "daemon/internal/api/claims.go",
        text: `s.refuse(w, r, http.StatusBadRequest, "${NUL_REFUSAL.prefix}"+err.Error())`,
      },
    ],
    value: true,
  },
  {
    name: "main's daemon from #4488 until #3659",
    markers: [
      {
        file: "daemon/internal/api/claims.go",
        text: `writeJSON(w, http.StatusBadRequest, errorBody("${NUL_REFUSAL.prefix}"+err.Error()))`,
      },
    ],
    value: true,
  },
];
/** What main `ff2a10ef`'s daemon answers a request whose apply fails, by route: a completion's and
 * any other fact's (its api's handoff.go and architect.go `applyFact`). */
export const FACT_APPLY_FAILED = {
  code: "FACT_APPLY_FAILED",
  completion: "could not apply handoff fact",
  fact: "could not apply fact",
} as const;
/** What main `ff2a10ef`'s daemon has in the place of the refusal: a decoder that ends at the one
 * value a body holds with no walk of its strings, and writes that put a taken request's agent text
 * into a column PostgreSQL refuses U+0000 in, each failing the fact's apply, which its routes
 * answer 500 `FACT_APPLY_FAILED`:
 *   - a completion the engine takes writes its summary and its commit onto the role's row
 *     (engine.go's `handoff`), whose columns are text (store.go's `PutPhase`), and its route
 *     answers a failed apply in `FACT_APPLY_FAILED.completion`'s words (handoff.go);
 *   - a backward move the engine takes starts the role of the phase it enters with a task holding
 *     the move's reason (effects.go's `task`, through engine.go's `transition` and `start`), which
 *     the outbox keeps as a JSON payload in its jsonb column (record's `NewOutboxRow`), and
 *     `applyFact` answers a failed apply in `FACT_APPLY_FAILED.fact`'s words (architect.go).
 * PostgreSQL refuses U+0000 in text (`invalid byte sequence for encoding "UTF8": 0x00`) and in
 * jsonb (`unsupported Unicode escape sequence`), as read on this pod's test Postgres on
 * 2026-11-19 and as main's own storable_text.go says of both. A completion or a move the engine
 * refuses writes nothing and is answered its refusal; a ready's capability report is cleaned of
 * every control character before it is kept (capabilities' `cleanDetail`), and no other route a
 * phase worker's tool calls writes the agent's own text. */
const STORES_NUL: readonly Marker[] = [
  {
    file: "daemon/internal/api/claims.go",
    text: '\t\treturn body, errors.New("more than one JSON value")\n\t}\n\treturn body, nil\n}',
  },
  {
    file: "daemon/internal/workflow/engine.go",
    text: "row.Claim, row.HandoffCommit, row.Verdict, row.Summary, row.CompletedAt = fact.Claim, fact.Commit, fact.Verdict, fact.Summary, e.now()",
  },
  {
    file: "daemon/internal/record/store.go",
    text: "phase.Issue, string(phase.Role), string(phase.Claim), phase.HandoffCommit, phase.HandoffRecord, phase.HandoffVersion, phase.Rounds, phase.Verdict, phase.Summary, phase.LastHandoff,",
  },
  {
    file: "daemon/internal/api/handoff.go",
    text: `if err != nil {\n\t\twriteFailure(w, http.StatusInternalServerError, "${FACT_APPLY_FAILED.code}", "${FACT_APPLY_FAILED.completion}")`,
  },
  {
    file: "daemon/internal/api/architect.go",
    text: "intake.BackwardMove{Issue: grant.Issue, Requester: grant.Role, To: req.To, Reason: req.Reason}, PhaseBackwardResponse{})",
  },
  {
    file: "daemon/internal/workflow/engine.go",
    text: "if err := e.start(ctx, tx, issue, starting, task(issue, handoff, pr, reason)); err != nil {",
  },
  { file: "daemon/internal/workflow/effects.go", text: 'text += " Reason: " + reason + "."' },
  {
    file: "daemon/internal/record/outbox.go",
    text: 'encoded, err := json.Marshal(payload)\n\tif err != nil {\n\t\treturn OutboxRow{}, fmt.Errorf("encode outbox payload: %w", err)',
  },
  {
    file: "daemon/internal/api/architect.go",
    text: `if err != nil {\n\t\twriteFailure(w, http.StatusInternalServerError, "${FACT_APPLY_FAILED.code}", "${FACT_APPLY_FAILED.fact}")`,
  },
];

/** The file the daemon builds its state answer's views in. */
const STATE_GO = "daemon/internal/api/state.go";
/** The state's pull request as main `ff2a10ef`'s daemon writes it, ending at the count of fix
 * attempts. */
const PULL_REQUEST_COUNTED: Marker = {
  file: STATE_GO,
  text: 'ReviewDecision string `json:"reviewDecision,omitempty"`\n\tFixAttempts    int    `json:"fixAttempts"`\n}',
};
/** The same as main's daemon writes it from contract 59 until #3659, ending at the round's
 * decision. */
const PULL_REQUEST_UNCOUNTED: Marker = {
  file: STATE_GO,
  text: 'MergeState     string `json:"mergeState,omitempty"`\n\tReviewDecision string `json:"reviewDecision,omitempty"`\n}',
};
/** The same as main's daemon writes it since #3659, which ends it at the round's decision for
 * every caller but one below contract 59, whose copy alone writes the count back, 0 (its
 * MarshalJSON on `fixAttemptsShown`, shaping.go's `withFixAttempts`): the rig's callers state
 * their own label's contract, which is that daemon's own, so none of them is served the count. */
const PULL_REQUEST_SHOWN_BELOW: readonly Marker[] = [
  {
    file: STATE_GO,
    text: 'MergeState     string `json:"mergeState,omitempty"`\n\tReviewDecision string `json:"reviewDecision,omitempty"`\n\t// fixAttemptsShown is set on the copy the state route serves a caller below the contract\n',
  },
  { file: STATE_GO, text: "\tfixAttemptsShown bool\n}" },
];

/** Whether the state the daemon of `checkout` answers carries the count of fix attempts on its
 * pull request (`LabelShapes`' `servesFixAttempts`): main `ff2a10ef`'s by `PULL_REQUEST_COUNTED`,
 * none for main's from contract 59 by `PULL_REQUEST_UNCOUNTED`, and none for main's since #3659
 * by `PULL_REQUEST_SHOWN_BELOW`, which writes it back to a caller below 59 alone. The count rises
 * only at a push onto a head whose check verdict is red (that daemon's classify's `advance`), and
 * every check of every world's head concludes `success` (rig.sh's `concluded_checks`), so a
 * world's count is 0. standin-record.ts's `contractRefusal` reads it to build the state bodies a
 * label's contract is handed, as the stand-in serves them. Throws, naming each marker and its
 * file, for a source of no shape or of two. */
export function servesFixAttempts(checkout: string): boolean {
  return sourceShape(checkout, "the state's pull request", [
    {
      name: "main's daemon since #3659, which counts for a caller below contract 59 alone",
      markers: PULL_REQUEST_SHOWN_BELOW,
      value: false,
    },
    {
      name: "main's daemon from contract 59 until #3659",
      markers: [PULL_REQUEST_UNCOUNTED],
      value: false,
    },
    {
      name: "main ff2a10ef's daemon, whose pull request counts its fix attempts",
      markers: [PULL_REQUEST_COUNTED],
      value: true,
    },
  ]);
}

/** The files the daemon's answers are written and shaped in, its contract declared in, and its
 * claims' update state read in. */
const SERVER_GO = "daemon/internal/api/server.go";
const SHAPING_GO = "daemon/internal/api/shaping.go";
const VERSION_GO = "daemon/internal/api/version.go";
const UPDATE_STATE_GO = "daemon/internal/api/updatestate.go";
/** The refusal of the per-issue record of an issue the daemon records nothing of, as main's
 * daemon since #3659 words it: its code, and the sentence the issue's key ends (server.go's
 * `issueRecordRoute`). The daemon stand-in answers by it; `labelShapes` holds a label's source to
 * it (`STATES_CONTRACT_SHARED`). */
export const RECORD_NOT_FOUND = {
  code: "RECORD_NOT_FOUND",
  message: "The daemon has no record for ",
} as const;
/** What a daemon that states its contract on every answer and shapes its bodies to its caller's
 * holds (main's since #3659, dc1bbc995e), in either form of its contract number: the handler
 * around the whole mux that sets `Legion-Daemon-Contract` to its `DaemonAPIVersion` before any
 * route answers, the catch-all's 404 and every refusal among them (server.go's
 * `statingContract`); `GET /healthz` answering the project beside its status (`Health`,
 * `healthRoute`); `GET /legion/v1/issues/{key}/record` answering the issue the state holds under
 * that key, and 404 RECORD_NOT_FOUND in the words `RECORD_NOT_FOUND` restates for any other
 * (`issueRecordRoute`); and the state shaped to the caller's contract (shaping.go): a caller placed
 * by its `Legion-Plugin-Contract` header, or with none by the claims of its pod, of which a claim
 * recording this daemon's contract counts for nothing, so such a caller is served this daemon's
 * own body (`ownBody`); that body marked with the daemon's contract and the caller's (`contract`),
 * the lowest contract the claims that hold a process record (`lowestPluginContract`,
 * `recordsContract`), and each claim's update state, `none` for a claim no update round is open
 * over (`updateState`, updatestate.go's `UpdateStateOf`). The three lines that name the contract
 * itself are in `CONTRACT_IS_A_CONSTANT` or `CONTRACT_IS_THE_REGISTRYS`. */
const STATES_CONTRACT_SHARED: readonly Marker[] = [
  { file: SERVER_GO, text: 'const daemonContractHeader = "Legion-Daemon-Contract"' },
  {
    file: SERVER_GO,
    text: "Handler:           statingContract{handler: trackTransactions(noteRequesters(mux))},",
  },
  {
    file: SERVER_GO,
    text: 'type Health struct {\n\tStatus  string `json:"status"`\n\tProject string `json:"project"`\n}',
  },
  {
    file: SERVER_GO,
    text: 's.writeAnswer(w, r, AnswerHealth, http.StatusOK, Health{Status: "ok", Project: s.project})',
  },
  { file: SERVER_GO, text: `const recordNotFound = "${RECORD_NOT_FOUND.code}"` },
  {
    file: SERVER_GO,
    text: `issue, recorded := state.Issues[key]\n\tif !recorded {\n\t\ts.writeFailure(w, r, http.StatusNotFound, recordNotFound, "${RECORD_NOT_FOUND.message}"+key)`,
  },
  {
    file: SERVER_GO,
    text: "s.writeAnswerFrom(w, r, AnswerIssueRecord, http.StatusOK, issue, claims)",
  },
  { file: SERVER_GO, text: "state.LowestPluginContract = lowestPluginContract(claims)" },
  { file: SHAPING_GO, text: 'const pluginContractHeader = "Legion-Plugin-Contract"' },
  {
    file: SHAPING_GO,
    text: "return c.Locator != nil && c.Locator.Sandbox != nil && c.PluginContract != 0",
  },
  { file: STATE_GO, text: 'UpdateState UpdateState `json:"updateState,omitempty"`' },
  {
    file: UPDATE_STATE_GO,
    text: '\tswitch {\n\tcase c.UpdateSignalledAt != nil:\n\t\treturn UpdateStateAnswered\n\tcase c.UpdateRound == "":\n\t\treturn UpdateStateNone',
  },
  { file: UPDATE_STATE_GO, text: 'UpdateStateNone UpdateState = "none"' },
];
/** The three lines of a daemon that states its contract that name the contract, as main's daemon
 * holds them from #3659 until #5399 (d41b74eb, INFRA-1569): `DaemonAPIVersion` is a constant,
 * which version.go declares as `const DaemonAPIVersion = <n>`. */
const CONTRACT_IS_A_CONSTANT: readonly Marker[] = [
  {
    file: SERVER_GO,
    text: "\tw.Header().Set(daemonContractHeader, strconv.Itoa(DaemonAPIVersion))\n\ts.handler.ServeHTTP(w, r)\n}",
  },
  {
    file: SHAPING_GO,
    text: "var ownBody = placement{restore: DaemonAPIVersion, strip: DaemonAPIVersion}",
  },
  {
    file: SHAPING_GO,
    text: "state.Contract = &ContractView{Daemon: DaemonAPIVersion, Caller: strip}",
  },
];
/** The same three lines since #5399, where `DaemonAPIVersion()` is the number the build's registry
 * gives it (contracts.go): `LastNumberedContract` plus the build's count of entries, each entry
 * one file of the package and one fixture under `packages/contracts/fixtures/daemon-api/contracts/`,
 * so no file declares the number. */
const CONTRACT_IS_THE_REGISTRYS: readonly Marker[] = [
  {
    file: SERVER_GO,
    text: "\tw.Header().Set(daemonContractHeader, strconv.Itoa(DaemonAPIVersion()))\n\ts.handler.ServeHTTP(w, r)\n}",
  },
  {
    file: SHAPING_GO,
    text: "func ownBody() placement {\n\town := DaemonAPIVersion()\n\treturn placement{restore: own, strip: own}\n}",
  },
  {
    file: SHAPING_GO,
    text: "state.Contract = &ContractView{Daemon: DaemonAPIVersion(), Caller: strip}",
  },
];
/** What every daemon before #3659 has in their place: `GET /healthz` answering `{"status":"ok"}`
 * and nothing else, written as a body of its own, which no handler around it marks with a
 * contract. */
const HEALTH_UNSTATED: Marker = {
  file: SERVER_GO,
  text: 'writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})',
};

/** Where a daemon source that states its contract has the number it states: the `constant` of
 * version.go, or the build's `registry`. */
type ContractSource = "constant" | "registry";

/** The contract the daemon of `checkout` states on every answer and shapes its bodies to
 * (`LabelShapes`' `statesContract`): for a source of main's shape since #3659, the number its
 * version.go declares, by `CONTRACT_IS_A_CONSTANT`, or, since #5399, the number its registry gives
 * the build, by `CONTRACT_IS_THE_REGISTRYS`. That is `daemonContract`'s own number for the
 * checkout, which no manifest of it holds, since main's plugins read it the same way. It is
 * undefined for a source before #3659, by `HEALTH_UNSTATED`. standin-record.ts's
 * `contractRefusal` reads it to build the state bodies a label's contract is handed, as the
 * stand-in serves them. Throws, naming each marker and its file, for a source of no shape or of
 * two, and for a source of the constant's shape that declares no constant. */
export function statesContract(checkout: string): number | undefined {
  const states = sourceShape<ContractSource | undefined>(
    checkout,
    "the contract its answers state",
    [
      {
        name: "main's daemon from #3659 to #5399, whose contract is a constant of version.go",
        markers: [...STATES_CONTRACT_SHARED, ...CONTRACT_IS_A_CONSTANT],
        value: "constant",
      },
      {
        name: "main's daemon since #5399, whose contract is its registry's",
        markers: [...STATES_CONTRACT_SHARED, ...CONTRACT_IS_THE_REGISTRYS],
        value: "registry",
      },
      {
        name: "every daemon the rig runs before #3659, whose health route answers its status alone",
        markers: [HEALTH_UNSTATED],
        value: undefined,
      },
    ]
  );
  if (states === undefined) return undefined;
  if (states === "registry") return daemonContract({}, checkout);
  const file = path.join(checkout, "packages", VERSION_GO);
  const declared = existsSync(file)
    ? /^const DaemonAPIVersion = ([1-9]\d*)$/m.exec(readFileSync(file, "utf8"))?.[1]
    : undefined;
  if (declared === undefined)
    throw new Error(
      `the daemon source of ${checkout} states a contract on every answer, and packages/${VERSION_GO} declares no \`const DaemonAPIVersion = <n>\` for it to state`
    );
  return Number(declared);
}

/** What a daemon whose state's `repositories` block is the repositories the project's live records
 * name holds (main's since #4493, a86c31c9fd, contract 202): the block's `live` list, one entry
 * for each such repository (state.go's `RepositoriesView`); an entry's `apps`, each role App's
 * installation on it once GitHub answered both reads (`LiveRepositoryView`); and the band that
 * serves a caller below that contract the block in contract 152's shape instead (shaping.go's
 * `withoutRepositoriesLive`), so the daemon's own body is this block. */
const REPOSITORIES_LIVE: readonly Marker[] = [
  { file: STATE_GO, text: 'Live              []LiveRepositoryView `json:"live"`' },
  { file: STATE_GO, text: 'Apps      *AppInstallationsView `json:"apps,omitempty"`' },
  {
    file: SHAPING_GO,
    text: "{contract: repositoriesLiveContract, state: withoutRepositoriesLive,",
  },
];
/** What every daemon the rig runs before #4493 has in their place: the project's Dispatch mapping,
 * its block's `mapped` and `refused` lists, each written in a view of its own (state.go's
 * `MappedRepositoryView` and `RefusedRepositoryView`). */
const REPOSITORIES_MAPPED: readonly Marker[] = [
  { file: STATE_GO, text: 'Mapped            []MappedRepositoryView  `json:"mapped"`' },
  { file: STATE_GO, text: 'Refused           []RefusedRepositoryView `json:"refused"`' },
];

/** Whether the state the daemon of `checkout` serves at its own contract carries the repositories
 * its live records name (`LabelShapes`' `repositoriesLive`): main's since #4493, by
 * `REPOSITORIES_LIVE`, `{live, withoutWebhookApp, caches}`; and false for one before it, by
 * `REPOSITORIES_MAPPED`, whose block is the project's mapping, `{owners, mapped, refused,
 * withoutWebhookApp, caches}`. standin-record.ts's `stateBody` writes the block in the label's
 * shape, and its `contractRefusal` reads this to build the bodies a label's contract is handed.
 * Throws, naming each marker and its file, for a source of neither shape or both. */
export function repositoriesLive(checkout: string): boolean {
  return sourceShape(checkout, "the state's repositories block", [
    { name: "main's daemon since #4493", markers: REPOSITORIES_LIVE, value: true },
    {
      name: "every daemon the rig runs before #4493, whose block is the project's mapping",
      markers: REPOSITORIES_MAPPED,
      value: false,
    },
  ]);
}

/** What the daemon source of `checkout` posts, refuses, publishes, ends a reviewer's task with,
 * takes a review of the review App for, records of each run of a step, serves on its pull
 * request, states on its answers and refuses of a completion's `ready`, each read as one of two
 * shapes: main's since the change that brought it, by its own marker, or main `ff2a10ef`'s, which
 * predates every one of those changes, by the code it has in their place (`STATUS_WITHOUT_MESSAGE`,
 * `OUTBOX_WITHOUT_HOLDERS`, `ROW_AS_QUEUED`, `BY_ROW_SESSION`, `BY_COMMIT_ID`,
 * `REQUEST_WITHOUT_RATING`, `PULL_REQUEST_COUNTED`, `HEALTH_UNSTATED`). The refusal of `ready` has
 * a third, main's from #4488 until #3659, whose request ends at the rating
 * (`REQUEST_WITHOUT_READY`), and so has the record of each run of a step, main's from #4488 until
 * #4611, which read the step of a phase off `DefaultSteps` (`STEPS_OF_PHASE`) where main's since
 * reads it off the walk of the run (`STEPS_ON_WALK`). The refusal of a body holding U+0000 has
 * four: main's words since #4463 written through the answer writer since #3659 and as a body of
 * their own before it, #4488's words before them, and main `ff2a10ef`'s store in their place
 * (`STORES_NUL`). The state's repositories block is main's since #4493 or the mapping every daemon
 * before it serves (`REPOSITORIES_MAPPED`). The completion refusals have one shape, every daemon
 * the rig runs holding them, and the route's verdict rule one in each writer of a refusal
 * (`FAILURE_WRITERS`). Throws, naming each marker and its file, for a source of no shape or of
 * more than one (`sourceShape`), which refuses the batch at rig.sh's label_facts and a stand-in's
 * boot. */
export function labelShapes(checkout: string): LabelShapes {
  for (const [code, file] of COMPLETION_REFUSALS)
    sourceShape(checkout, `the completion refusal ${code}`, [
      { name: "every daemon the rig runs", markers: [{ file, text: `"${code}"` }], value: true },
    ]);
  sourceShape(
    checkout,
    "the completion route's verdict rule",
    FAILURE_WRITERS.map(({ name, writer }) => ({ name, markers: verdictRule(writer), value: true }))
  );
  const older = {
    name: "main ff2a10ef's daemon, whose status write posts no message",
    markers: [STATUS_WITHOUT_MESSAGE],
    value: false,
  };
  const posted = (reader: string, since: string, text: string) =>
    sourceShape(checkout, reader, [
      { name: `main's daemon since ${since}`, markers: [{ file: EFFECTS, text }], value: true },
      older,
    ]);
  const nulRefusal = sourceShape<string | undefined>(
    checkout,
    "the refusal of a body holding U+0000",
    [
      {
        name: "main's daemon since #4463",
        markers: refusesNul(NUL_REFUSAL.refused),
        value: NUL_REFUSAL.refused,
      },
      {
        name: "main's daemon from #4488 until #4463",
        markers: refusesNul(NUL_REFUSAL.unstorable),
        value: NUL_REFUSAL.unstorable,
      },
      {
        name: "main ff2a10ef's daemon, whose decoder reads no string of a body and whose store fails on U+0000",
        markers: STORES_NUL,
        value: undefined,
      },
    ]
  );
  if (nulRefusal !== undefined)
    sourceShape(checkout, "the 400 a body holding U+0000 is refused in", NUL_WRITERS);
  return {
    postsBackward: posted("a backward move's sentence", "#4309", "func backwardMoveReason("),
    refusesReason: posted(
      "the refusal of a backward move's reason",
      "#4309",
      '"BACKWARD_REASON_TOO_LONG"'
    ),
    publishesHolders: sourceShape(checkout, "the role holders' publication", [
      {
        name: "main's daemon and Dispatch since #4228",
        markers: [
          { file: "daemon/internal/dispatch/client.go", text: '"/role-holders"' },
          {
            file: "envoy/internal/dispatch/api/routes_table.go",
            text: '"/api/v1/issues/{key}/role-holders"',
          },
        ],
        value: true,
      },
      {
        name: "main ff2a10ef's daemon, whose outbox kinds end at gated_steps_publish",
        markers: [OUTBOX_WITHOUT_HOLDERS],
        value: false,
      },
    ]),
    namesReviewRound: sourceShape(checkout, "the sentences that end a reviewer's task", [
      { name: "main's daemon since #4328", markers: NAMES_ROUND, value: true },
      {
        name: "main ff2a10ef's daemon, whose queue writes a task as it came",
        markers: [ROW_AS_QUEUED],
        value: false,
      },
    ]),
    decidesBySession: sourceShape(checkout, "whose review of the review App decides a round", [
      { name: "main's daemon since #4328", markers: BY_RECORDED_SESSION, value: true },
      {
        name: "main ff2a10ef's daemon, which reads the session off the reviewer's row",
        markers: BY_ROW_SESSION,
        value: false,
      },
    ]),
    decidesByFooterHead: sourceShape(checkout, "the head a review of the review App decides on", [
      {
        name: "main's daemon since #3995, which reads the head through reviewround.IsHead",
        markers: [...BY_FOOTER_HEAD, ...HEAD_SPELLED.since3995],
        value: true,
      },
      {
        name: "main's daemon since #2539",
        markers: [...BY_FOOTER_HEAD, ...HEAD_SPELLED.since2539],
        value: true,
      },
      {
        name: "main ff2a10ef's daemon, which records a review's decision against its commit_id",
        markers: BY_COMMIT_ID,
        value: false,
      },
    ]),
    writesSteps: sourceShape(checkout, "the record of each run of a step", [
      {
        name: "main's daemon and Dispatch since #4682, whose client writes each step record as the session it writes as",
        markers: [
          ...WRITES_STEPS,
          STEP_PUT.since4682,
          ...STEPS_ON_WALK,
          ...STEPS_WITH_VARIANT,
          STEP_ROUTE,
        ],
        value: true,
      },
      {
        name: "main's daemon and Dispatch from #5014 until #4682, which name the issue's experiment and variant on each step record",
        markers: [
          ...WRITES_STEPS,
          STEP_PUT.since4400,
          ...STEPS_ON_WALK,
          ...STEPS_WITH_VARIANT,
          STEP_ROUTE,
        ],
        value: true,
      },
      {
        name: "main's daemon and Dispatch from #4611 until #5014, which read the step a run stands at off the walk of its run",
        markers: [
          ...WRITES_STEPS,
          STEP_PUT.since4400,
          ...STEPS_ON_WALK,
          ...STEPS_WITHOUT_VARIANT,
          STEP_END_ON_WALK_WITHOUT_VARIANT,
          STEP_ROUTE,
        ],
        value: true,
      },
      {
        name: "main's daemon and Dispatch from #4488 until #4611, which read the step of a phase off DefaultSteps",
        markers: [
          ...WRITES_STEPS,
          STEP_PUT.since4400,
          ...STEPS_WITHOUT_VARIANT,
          ...STEPS_OF_PHASE,
          STEP_ROUTE,
        ],
        value: true,
      },
      {
        name: "main ff2a10ef's daemon, whose outbox kinds end at gated_steps_publish",
        markers: [OUTBOX_WITHOUT_HOLDERS],
        value: false,
      },
    ]),
    refusesRating: sourceShape(checkout, "the refusal of a rating the completion does not take", [
      { name: "main's daemon since #4488", markers: REFUSES_RATING, value: true },
      {
        name: "main ff2a10ef's daemon, whose completion request ends at wakeAt",
        markers: [REQUEST_WITHOUT_RATING],
        value: false,
      },
    ]),
    nulRefusal,
    servesFixAttempts: servesFixAttempts(checkout),
    statesContract: statesContract(checkout),
    retiresReady: sourceShape(checkout, "the refusal of a completion's ready", [
      { name: "main's daemon since #3659", markers: RETIRES_READY, value: true },
      {
        name: "main's daemon from #4488 until #3659, whose completion request ends at planReason",
        markers: [REQUEST_WITHOUT_READY],
        value: false,
      },
      {
        name: "main ff2a10ef's daemon, whose completion request ends at wakeAt",
        markers: [REQUEST_WITHOUT_RATING],
        value: false,
      },
    ]),
    repositoriesLive: repositoriesLive(checkout),
  };
}

/** The two sentences the daemon of `checkout` ends a task of the reviewer in `reviewing` with, as
 * reviewround.go's `Sentences` writes them: `commit`, the head its row holds as the one the
 * reviewer's newest deciding review read ("" for none), and `entry`, the entry its row stands at
 * while it holds that entry's task (0 for none), each `none` where it has no value
 * (review_round.go's `namingTheReviewRound`). "" for a daemon of main `ff2a10ef`'s shape, which
 * ends no task with them (`labelShapes`' `namesReviewRound`). Throws for a source of no shape
 * this reads, naming each marker and its file. */
export function reviewRoundSentences(checkout: string, commit: string, entry: number): string {
  if (!labelShapes(checkout).namesReviewRound) return "";
  return ` ${ROUND_COMMIT}: ${commit === "" ? ROUND_NONE : commit}. ${ROUND_ENTRY}: ${entry > 0 ? entry : ROUND_NONE}.`;
}

/** `read()`, or an error naming `fact` of the daemon of `checkout` and why it could not be read. */
function readFact<T>(checkout: string, fact: string, read: () => T): T {
  try {
    return read();
  } catch (error) {
    throw new Error(
      `the daemon fact \`${fact}\` of ${checkout} could not be read: ${error instanceof Error ? error.message : String(error)}`
    );
  }
}

/** Every fact of the daemon of `checkout` a daemon stand-in of `role` started in `phase` (""
 * for the role's own) answers by (`StandinFacts`), each read through the facts cache. The
 * stand-in reads them at its boot and rig.sh's label_facts reads them for every stand-in a batch
 * boots before any of its runs starts, through this one function, so a boot reads each out of a
 * file. Throws naming the fact that could not be read, or the one the daemon answered in a way
 * this rig cannot answer by. */
export function standinFacts(checkout: string, role: string, phase: string): StandinFacts {
  const routes = readFact(checkout, "routes", () => labelRoutes(checkout));
  const daemon = readFact(checkout, "completion", () => labelDaemon(checkout));
  const turnOutcomeLimit = serves(routes, "POST", "/legion/v1/claims/turn-outcome")
    ? readFact(checkout, "count-default launch_failure_limit", () =>
        countDefault(checkout, "launch_failure_limit")
      )
    : 0;
  const refusal = (name: string) =>
    readFact(checkout, `claim-refusal ${name}`, () => claimRefusal(checkout, name));
  const invalidSecret = refusal("InvalidSecret");
  const invalidBootToken = refusal("InvalidBootToken");
  const staleGeneration = refusal("StaleGeneration");
  const { phase: rolePhase, word: handoffWord } = readFact(checkout, `phase ${role}`, () =>
    labelPhase(checkout, role)
  );
  if ((handoffWord !== "") !== RECORD_PHASES.includes(rolePhase))
    throw new Error(
      `the daemon fact \`phase ${role}\` of ${checkout} gives ${rolePhase} the handoff word '${handoffWord}', which RECORD_PHASES does not agree with`
    );
  const served = phase === "" ? rolePhase : phase;
  const late = served !== rolePhase || served === "reviewing";
  const command = `refusal not-current-phase ${role}`;
  const lateRefusal = late
    ? readFact(checkout, command, () => notCurrentPhase(checkout, role, PHASE_SLOT, ISSUE_SLOT))
    : undefined;
  if (late && lateRefusal?.code !== NOT_CURRENT_PHASE)
    throw new Error(
      `the daemon fact \`${command}\` of ${checkout} is no ${NOT_CURRENT_PHASE} code and message for the refusal of ${role}'s late completion`
    );
  const shapes = readFact(checkout, "shapes", () => labelShapes(checkout));
  return {
    routes,
    daemon,
    turnOutcomeLimit,
    invalidSecret,
    invalidBootToken,
    staleGeneration,
    rolePhase,
    handoffWord,
    phase: served,
    notCurrentPhase: lateRefusal,
    shapes,
  };
}

/** The prompt a launch of `role` starts with, as the daemon in `module` composes it (daemon-pane.go
 * `prompt`): the role's parts, a blank line, then its addressing, and for the architect of a tree's
 * root (`root`) the design gate steps after that, byte for byte as the daemon writes the prompt
 * file. The claim is the rig's: its issue is the root of its tree, or a child under that root. */
export function composeRolePrompt(module: string, role: LegionRole, root: boolean): string {
  return runDaemonPane(module, ["prompt", role, root ? "root" : "child"]);
}

/** The claim a pane is launched for and the rig's stand-ins for what the daemon knows at boot
 * (daemon-pane.go's paneRequest). `repository` and `defaultBranch` are the run's own world's,
 * which the pane's addressing names and whose owner the claim's gh bundle is the directory of.
 * `ghToken` stands in for the claim's GitHub App token: it is
 * rendered into the claim's gh bundle under `<stateDir>/secrets/<claim>-gh`, the pane's
 * `LEGION_GH_OWNERS_DIR`, whose one owner's directory is its `GH_CONFIG_DIR`, and its
 * `LEGION_GH_PATH` is `ghPath` when the rig names one (its recording stand-in), else the real gh as
 * the daemon resolves it at boot, the first `gh` on `path`, refused when that is a script; omitted,
 * the pane gets no gh variable, as from a daemon with no GitHub Apps. */
interface PaneRequest {
  readonly project: string;
  readonly issue: string;
  readonly role: string;
  readonly repository: string;
  readonly defaultBranch: string;
  readonly stateDir: string;
  readonly workspace: string;
  readonly daemonUrl: string;
  readonly envoyUrl: string;
  readonly natsUrls: readonly string[];
  readonly bootTokenFile: string;
  readonly ghToken?: string;
  readonly path: string;
  readonly ghPath?: string;
  readonly systemPrompt?: boolean;
}

/** What the daemon tells the pane: every variable, PATH included, and, for a request that asks for
 * it, the one `--append-system-prompt` argument as shell text. */
export interface DaemonPane {
  readonly env: Record<string, string>;
  readonly systemPromptArgument?: string;
}

/** The stand-in for the claim's GitHub App token: `daemon-pane.go` renders it into the claim's gh
 * files, where production's daemon renders the App's token. Production's `gh auth token` and `gh
 * auth status -t` print that token; the world's gh refuses both, since no stand-in reads a token
 * (gh-standin.ts's `authStatus`), and health's rule 21 names a run that makes either. It has the
 * shape of an installation token, `ghs_` and 36 letters and digits, drawn afresh by each process,
 * as a real one is random: a process that reads it holds it and scrubs every run of 10 of its
 * characters from each line it prints (`@legion/contracts/held-secrets`), so a token spelled from
 * words (it was `ghs_rig_token`) would take those words out of the lines a run reads, and no
 * literal of one is in the source a push's leak check reads. It is not a `fixtureSecret`, whose
 * `<prefix>-` and 24 capitals are not the shape gh masks. A placeholder, never a secret. */
const TOKEN_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
export const RIG_GH_TOKEN = `ghs_${Array.from(
  { length: 36 },
  () => TOKEN_ALPHABET[randomInt(TOKEN_ALPHABET.length)]
).join("")}`;

/** Where a worker pane runs: the run directory that stands for the daemon's state directory and
 * the issue's workspace, the stand-in daemon's port, and the Oh My Pi profile. */
export interface WorkerLaunch {
  readonly rig: string;
  readonly port: number;
  readonly profile: string;
}

/** The claim a worker pane is launched for: its project, issue and role, and the repository and
 * default branch of the run's own world, which the pane's addressing names and whose owner the
 * claim's gh bundle is the directory of. The caller reads both out of the run's world
 * (worker-pane.ts); why they are the world's and no name of this rig's own: rig.sh's worker
 * world comment, which holds that story once. */
export interface WorkerClaim {
  readonly project: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly repository: string;
  readonly defaultBranch: string;
}

/** Where a pane's daemon is read from: a checkout's daemon module (`DAEMON_MODULE` for this one),
 * whose embedded role prompts compose the pane's system prompt when `systemPrompt` is set. */
export interface PaneSource {
  readonly daemonModule: string;
  readonly systemPrompt?: boolean;
  /** The real gh the pane is told, as an operator names one with `LEGION_GH_PATH`: the rig's
   * recording stand-in. Omitted, the daemon's own resolution on the inherited PATH. */
  readonly ghPath?: string;
}

/** The pane the Legion daemon's tmux runtime gives a phase worker for `claim`, pointed at the
 * scratch state directory and the stand-in daemon, over the caller's own environment. Everything
 * the daemon tells the pane comes from the daemon's own functions in `source`'s module
 * (`daemon-pane.go`): the claim's identity, the daemon URL, the state directory and workspace,
 * Envoy (the listener and NATS the pane's own extension reaches from the caller's environment and
 * the profile's envoy.json, `envoySettings`, which reads no Envoy from the workspace's
 * `.envoy/envoy.json`; the default listener and no NATS when that Envoy is off), the claim's gh
 * bundle, which holds `RIG_GH_TOKEN` (`LEGION_GH_OWNERS_DIR`, `GH_CONFIG_DIR` and `LEGION_GH_PATH`,
 * `source.ghPath`, or the first `gh` on the inherited PATH, which must be no script), with
 * `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` set empty,
 * `PI_SHELL_PREFIX`, the four XDG base directories, the boot token pointer
 * (`<state>/secrets/boot`, the file the stand-in checks), and PATH with the launcher directory
 * first; no grant file, since the `legion` tool mints its grants in-process and no agent runs
 * `legion` from bash; and, with `systemPrompt`, the system prompt argument, whose addressing
 * names `claim.repository` and `claim.defaultBranch`, the world's own. An inherited
 * `ANTHROPIC_API_KEY`, every `LEGION_*` and `DISPATCH_*` value, the caller's own GitHub variables
 * and its session variables (`OMP_SESSION_ID`, `TMUX`, `TMUX_PANE` and `OMP_SESSION_EXCLUSIVE`,
 * the hold the daemon gives a resumed launch alone, where a scenario pane is a fresh launch) are
 * dropped first. The profile's agent directory, and so its envoy.json, is under the inherited
 * `HOME`. Rejects `envoy: <why>` when that Envoy is off with an error, or on with inbound messages
 * off (`inbound.off`: no NATS server, or its seed refused), since the pane's Legion claim refuses
 * then. */
export async function workerPane(
  launch: WorkerLaunch,
  inherited: NodeJS.ProcessEnv,
  claim: WorkerClaim,
  source: PaneSource
): Promise<DaemonPane> {
  const env: Record<string, string> = {};
  for (const [key, value] of Object.entries(inherited)) {
    if (value === undefined) continue;
    if (key === "ANTHROPIC_API_KEY" || key.startsWith("LEGION_") || key.startsWith("DISPATCH_"))
      continue;
    if (["GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST"].includes(key)) continue;
    if (["OMP_SESSION_ID", "OMP_SESSION_EXCLUSIVE", "TMUX", "TMUX_PANE"].includes(key)) continue;
    env[key] = value;
  }
  const home = env.HOME ?? os.homedir();
  const stateDir = path.join(launch.rig, "state");
  const workspace = path.join(launch.rig, "ws");
  const profileEnv = {
    ...env,
    OMP_PROFILE: launch.profile,
    PI_CODING_AGENT_DIR: path.join(home, ".omp", "profiles", launch.profile, "agent"),
  };
  const envoy = await sessionEnvoySettings(sessionConfigFile("omp", profileEnv), profileEnv, {
    cwd: workspace,
  });
  if (!envoy.enabled && envoy.error !== null) throw new Error(`envoy: ${envoy.error}`);
  if (envoy.enabled && "off" in envoy.inbound) throw new Error(`envoy: ${envoy.inbound.off}`);
  const request: PaneRequest = {
    ...claim,
    stateDir,
    workspace,
    daemonUrl: `http://127.0.0.1:${launch.port}`,
    envoyUrl: envoy.enabled ? envoy.url : DEFAULT_ENVOY_URL,
    natsUrls: envoy.enabled && "natsUrls" in envoy.inbound ? envoy.inbound.natsUrls : [],
    bootTokenFile: path.join(stateDir, "secrets", "boot"),
    ghToken: RIG_GH_TOKEN,
    path: env.PATH ?? "",
    systemPrompt: source.systemPrompt,
    ghPath: source.ghPath,
  };
  const pane = JSON.parse(
    runDaemonPane(source.daemonModule, ["pane"], JSON.stringify(request))
  ) as DaemonPane;
  return {
    ...pane,
    env: {
      ...env,
      OMP_PROFILE: profileEnv.OMP_PROFILE,
      PI_PROFILE: launch.profile,
      PI_CODING_AGENT_DIR: profileEnv.PI_CODING_AGENT_DIR,
      PI_NOTIFICATIONS: "off",
      PI_NO_TITLE: "1",
      ...pane.env,
    },
  };
}

if (import.meta.main) {
  const [command, checkout, ...rest] = Bun.argv.slice(2);
  const message = (error: unknown) => (error instanceof Error ? error.message : String(error));
  if (command === "gh-selectors" && checkout !== undefined && rest.length === 1) {
    try {
      writeFileSync(rest[0] ?? "", `${JSON.stringify(labelGhSelectors(checkout))}\n`);
    } catch (error) {
      console.error(`daemon-pane gh-selectors: ${message(error)}`);
      process.exit(1);
    }
    process.exit(0);
  }
  const [commit = "", entry = ""] = rest;
  if (command === "review-round" && checkout !== undefined && rest.length === 2) {
    if (!/^(?:0|[1-9][0-9]*)$/.test(entry)) {
      console.error(`daemon-pane review-round: entry ${JSON.stringify(entry)} is no entry number`);
      process.exit(2);
    }
    try {
      process.stdout.write(reviewRoundSentences(checkout, commit, Number(entry)));
    } catch (error) {
      console.error(`daemon-pane review-round: ${message(error)}`);
      process.exit(1);
    }
    process.exit(0);
  }
  if (command !== "facts" || checkout === undefined || rest.length === 0) {
    console.error(
      "usage: bun daemon-pane.ts facts <checkout> <role>[:<phase>]... | gh-selectors <checkout> <file> | review-round <checkout> <commit> <entry>"
    );
    process.exit(2);
  }
  // Without a build each fact is a `go run`, which is the cost this read is made to take off a
  // stand-in's boot; without the work directory the facts go to a cache no stand-in reads.
  for (const [variable, what] of [
    ["SKILL_SCENARIOS_DAEMON_PANE", "build of daemon-pane.go to read the daemon's facts with"],
    ["SKILL_SCENARIOS_WORK", "work directory, whose facts cache the label's stand-ins read"],
  ] as const) {
    if ((process.env[variable] ?? "") === "") {
      console.error(`daemon-pane facts: ${variable} names no ${what}`);
      process.exit(2);
    }
  }
  for (const boot of rest) {
    const [role = "", phase = ""] = boot.split(":");
    if (!isLegionRole(role)) {
      console.error(`daemon-pane facts: ${boot} names no Legion role`);
      process.exit(2);
    }
    try {
      standinFacts(checkout, role, phase);
    } catch (error) {
      console.error(
        `daemon-pane facts: the ${role}'s stand-in${phase === "" ? "" : ` in ${phase}`}: ${message(error)}`
      );
      process.exit(1);
    }
  }
  // The gh stand-in of every run of the label refuses an owner by these (worker_boot writes them
  // into the run), so a wrapper whose table holds a form this rig does not model fails the batch
  // here, naming it, before any run starts.
  try {
    labelGhSelectors(checkout);
  } catch (error) {
    console.error(`daemon-pane facts: the gh stand-in: ${message(error)}`);
    process.exit(1);
  }
}
