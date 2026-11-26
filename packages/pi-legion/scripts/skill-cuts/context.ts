/**
 * The case context of a capture: what one case's arm installs before
 * Oh My Pi builds its request, rebuilt from the case's `profile`, since a transcript keeps no
 * system prompt. `buildContext` writes it under one scratch directory, the same path for every arm
 * of a case, so the paths Oh My Pi puts in its prompt are the same in every arm:
 *
 * - `skills/`: the arm's installed skills (`arm.profileSkills`), one directory each under its
 *   name. Each copy of the case's skill's source (cases.ts's `testedCopy`) comes from the arm's
 *   own directory, and none does in an arm with no skill directory (`none`; arms.ts's
 *   `installedFromArm`); every other skill is its repository's directory at its commit, read
 *   through `source` and kept under the cache (`skills/<key of repo, commit, path>`). A skill
 *   directory the repository holds as a symbolic link is read at the directory the link names in
 *   the repository (`skillReadDir`), and one whose link leads out of the repository, or to no
 *   directory of it, is an UnreadableDirectory naming the case, the link and its target. A
 *   second copy of a name, whose text differs (a profile skill's `namespace`), goes in
 *   `namespaced/<namespace>/skills/<name>`, one more skills directory of its own
 *   (`namespacedDirs`), so Oh My Pi lists it as `<namespace>/<name>`. A skill a plugin reads as
 *   it loads and puts into the session itself (arms.ts's `Injection` of a plugin: the Envoy
 *   plugin's dispatch-first) gets a hidden stand-in at `skills/<name>` where the arm installs no
 *   copy under that name (`none`, or a load test under another name), so the plugin loads in
 *   every arm and Oh My Pi lists nothing more (`from: "stand-in"`).
 * - `plugins/<name>/`: each plugin of `profile.plugins` that is an Oh My Pi extension (its
 *   directory holds a `package.json` with an `omp` field), built as the worker image builds the
 *   Legion plugins: the repository at the commit (`trees/<key of repo, commit>`), its packages
 *   installed from its own lockfile (`bun install --frozen-lockfile --ignore-scripts`, only the
 *   plugin's workspace when the repository is a workspace), `bun run build` when the plugin has a
 *   build script (with `legion.daemonApiVersion` in the plugin's package.json, as a pack writes
 *   it, when that file holds none and the checkout has `scripts/daemon-contract.sh`: the number
 *   it prints, the file put back to its bytes after; a script that fails or prints no number
 *   fails the build), then the files its `files` list ships with each `omp.extensions` entry
 *   pointed at its bundle in `dist/` (`plugins/<key of repo, commit, path>`). Each skills path the
 *   plugin's manifest names (`omp.skills`, `dist/skills` for the Legion plugins, where the Envoy
 *   plugin also looks for its skills at load) is a link to `skills/`, so a plugin lists the arm's
 *   skills and no copy of its own: only the case's skill differs between arms. A plugin with no
 *   `package.json` of that kind installs skills alone (a verification-skills plugin), which the
 *   profile's skills already hold. A plugin that cannot be installed (its manifest cannot be
 *   parsed, its build fails, or a plugin of its name is installed already) is left out, with its
 *   reason; a plugin whose repository cannot be read stops the case with the reason, as a failed
 *   role-prompt read does, so every arm of a case has the same plugins. For a subagent's context
 *   (`subagent:<agent>`), the record names the agent definition the subagent ran (`agent`,
 *   `agentIn`) and the skills it autoloads, read in Oh My Pi's order: the project's agents, the
 *   nearest `.omp/agents` of the session repository from the working directory up, a link there
 *   followed within the repository (`projectAgents`), else, for a dev box's session under its
 *   home, the directory its installers link as `~/.omp/agents`; then a dev box's user agents, the
 *   directory its installers link as `~/.omp/agent/agents` (both at the commit of the subagent's
 *   start, `devboxAgents`); then each plugin's `agents/`; capture.ts checks the arm against it.
 * - `work/`: the checkout of the session repository. Each of its context files (a file named
 *   `AGENTS.md`, `CLAUDE.md`, `GEMINI.md` or `copilot-instructions.md`, REPOSITORY_CONTEXT_FILES,
 *   at its default branch's commit at the session's start) is written there at its path in the
 *   repository, and the session's working directory is `work/<profile.workingDirectory>` (`work/`
 *   when the profile names none), so Oh My Pi reads each of them from the working directory or
 *   from the parent it is in, at the depth the session read it.
 * - `claude-config/`: a dev box's user-level context file, the one its session's host read
 *   (build.ts: `.claude/CLAUDE.md` of the repository that holds the dev box's Claude Code
 *   settings, marked `user: true` in the profile), as `CLAUDE.md` in a scratch Claude Code config
 *   directory (`claudeConfigDir`), which the capture names as `CLAUDE_CONFIG_DIR`: Oh My Pi
 *   18.18.6 reads `CLAUDE.md` there as its one user-level context file (discovery/claude.ts), so
 *   the block's path, its rank and its dedupe are Oh My Pi's own, as on the dev box. A profile of
 *   more than one user-level file is refused.
 * - `system-prompt.md`, the one `--append-system-prompt` file, as the daemon writes it
 *   (packages/daemon/internal/runtime/systemprompt.go, `ComposePrompt`): for a Legion context
 *   (`legion:<project>:<role>`, the role `architect-root` for a tree's root architect), the role
 *   prompt, then every other context file (the project's instructions file, and `shared.md` when
 *   the project's prompt includes it), each with its trailing newlines dropped and joined by a
 *   blank line. The role prompt is `profile.appendSystemPrompt` when the case holds it, else the
 *   parts `prompts.Composer.Compose(role, isRoot)` (packages/daemon/internal/prompts/prompts.go)
 *   names in the plugin repository that holds that file, at the commit the daemon ran: the daemon
 *   composes the role prompt, so where the case's evidence item `the daemon image's commit of
 *   <repo>` (build.ts, DAEMON_IMAGE_ITEM) names the commit its daemon image was built from, that
 *   commit, else the profile's commit of the repository (for a Legion profile, the worker
 *   image's); the record says which (`commitOf`). Compose answers with paths
 *   of the parts it embeds, so it is run once per distinct `prompts.go` blob and role, by a small
 *   Go test written into the cached copy of the repository (never the workspace) and removed after
 *   (`composes/<blob>-<role>`), and each part is then read at that commit. For the
 *   controller (`legion:<project>:controller`) the parts are those `ControllerPromptPaths(true)`
 *   names: a Legion store's controller runs headless in a pod. The daemon's addressing sentence,
 *   which sits between the two, names the session's claim, which a case does not hold, so it is
 *   left out and the record says so. A subagent's context (`subagent:<agent>`) has no role prompt:
 *   its case carries the system prompt its session recorded, which the capture sends whole.
 *
 * `source` reads the repositories: `ghSource` with read-only `gh api` calls (a directory from the
 * commit's tree and its blobs, read along the directory's path where GitHub cuts the commit's tree
 * short, and a whole repository, a plugin's checkout, from its tarball), `localSource` from a
 * directory per repository and commit (the tests' made-up repositories). A directory the commit
 * does not hold, or one GitHub cuts short even one level at a time, is an UnreadableDirectory,
 * which stops the context; no source reads a whole repository in its place. Repository and owner
 * names come only from the case. Every cache entry is `<cacheDir>/<kind>s/<key>` (`skills`,
 * `trees`, `plugins`, `composes`, `files` for each single file read, and `dirs` for another
 * directory of a repository: a dev box's installers and the agents they link, and the directory
 * each level's `.omp/agents` of the session repository leads to, as `projectAgents` reads them),
 * made into a fresh path and renamed into place. `record` says what was used: each commit and
 * cache key, each plugin left out, and each build with its time and whether the cache already
 * held it.
 */
import { createHash, randomBytes } from "node:crypto";
import {
  chmodSync,
  cpSync,
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  renameSync,
  rmSync,
  type Stats,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { hasErrnoCode, messageFor } from "@legion/envoy-client/errors";
import { mapLimit } from "../skill-scenarios/production-dispatch";
import { type Arm, type InstalledCopy, installedFromArm, legionStore } from "./arms";
import { type Case, listedName, type ProfileSkill } from "./cases";
import {
  eachChunk,
  firstLine,
  GhRetries,
  ghLimiter,
  installationRateWait,
  json,
  ompSkillName,
  ompText,
  parseJson,
  pastLimit,
  Refused,
  readFrontmatter,
  textOf,
  timeLimit,
} from "./common";
import {
  BlobBatch,
  blobBatches,
  blobQuery,
  GH_CALL_TIMEOUT_MS,
  graphqlArgs,
  joinPath,
  shellVariables,
  type TreeEntry,
} from "./copies";

/** Where the Legion daemon's role prompt composer is, in the repository that holds it. */
export const PROMPTS_GO = "packages/daemon/internal/prompts/prompts.go";
/** The start of the evidence item `the daemon image's commit of <repo>`, which build.ts writes for
 *  a Legion case and a Legion task's profile: which commit of `<repo>` the project's daemon image
 *  was built from. The daemon composes the role prompt, so `rolePrompt` composes it at that commit
 *  when the item's source names one in the words `composedAt` gives (`daemonCommit`); a source
 *  that says why no rule gave one names none. */
export const DAEMON_IMAGE_ITEM = "the daemon image's commit of ";
/** The words of a DAEMON_IMAGE_ITEM source that name `commit` of `repo` as the daemon image's. */
export const composedAt = (repo: string, commit: string) =>
  `so the role prompt is composed from ${repo} at ${commit}`;
/** The evidence item build.ts writes that names a profile's session repository
 *  (`<SESSION_REPOSITORY_ITEM><repo>`), and the one that says there is none; the source of each
 *  names the session's absolute working directory after "working directory " (`underHome`). */
export const SESSION_REPOSITORY_ITEM = "session repository ";
export const NO_SESSION_REPOSITORY_ITEM = "no session repository";
/** The start of the evidence item `read times of <repo>`, which build.ts writes for a pool's host
 *  of a subagent's profile: its source names the commit of the pool directories after "its pool
 *  directories at" (`poolCommit`). */
export const READ_TIMES_ITEM = "read times of ";
/** The repository a load test's own entry of the tested skill names (load.ts's `caseOf`): a name
 *  for no repository, since a context installs the arm's skill from the arm's directory
 *  (`testedCopy`). Every read of a profile entry's repository passes it over; a real repository an
 *  arm's tested copy names (a behaviour case's) is read as any other. */
export const LOCAL_SKILL_REPO = "local/skill-directory";
/** The names of a repository's context files (build.ts lists the session repository's), which a
 *  context writes in its checkout. A profile's user-level file (`user: true`) goes to the scratch
 *  Claude Code config directory whatever its name, and every other context file goes in the
 *  appended system prompt. */
const REPOSITORY_CONTEXT_FILES: Readonly<Record<string, true>> = {
  "AGENTS.md": true,
  "CLAUDE.md": true,
  "GEMINI.md": true,
  "copilot-instructions.md": true,
};

/** The repositories a context is read from. */
export interface RepoSource {
  /** What the record calls this source. */
  readonly name: string;
  /** The bytes of `file` of `repo` (`owner/name`) at `commit`. Throws NoSuchFile when it holds
   *  none, and another error when the read fails. */
  file(repo: string, commit: string, file: string): Promise<Uint8Array>;
  /** Writes each file of `repo` at `commit` under `dir` (a path inside the repository, or "" for
   *  all of it) into `dest`, at its path below `dir`. Throws UnreadableDirectory when the commit
   *  holds no file under `dir`, or `dir` cannot be read on any try; another error when a read
   *  fails. */
  tree(repo: string, commit: string, dir: string, dest: string): Promise<void>;
  /** Reads ahead, in as few calls as it can, the files of `repo` at `commit` under each of `dirs`,
   *  so the `tree` calls for them that follow need no read of their own. What it reads is held
   *  until `tree` writes each file of it, or until the release it gives is called, which drops
   *  what no `tree` wrote. Optional. */
  prefetch?(repo: string, commit: string, dirs: readonly string[]): Promise<() => void>;
  /** The target of `file` of `repo` at `commit` when the commit holds it as a symbolic link (git's
   *  mode 120000), as the link's text; undefined when it holds none there, or holds a file or a
   *  directory. Throws when a read fails. Optional: a source without it holds no links. */
  link?(repo: string, commit: string, file: string): Promise<string | undefined>;
  /** The reads this source has made of GitHub so far: how many `gh` calls, the milliseconds in
   *  which at least one of them was running, and the calls sent again after a transient failure,
   *  by reason (common.ts's `GhRetries.record`). Absent for a source that reads no GitHub. */
  reads?(): {
    readonly calls: number;
    readonly ms: number;
    readonly retries: Readonly<Record<string, number>>;
  };
}

/** What `RepoSource.file` throws when the repository holds no such file at the commit, as opposed
 *  to a read that failed. */
export class NoSuchFile extends Error {}

/** What `RepoSource.tree` throws when `repo` at `commit` holds no file under `dir`, or when GitHub
 *  cuts a level of `dir`'s tree short even read alone, so no try can read it: a failure no retry
 *  mends, for which a behaviour screen drops its case and a load test its task (cli.ts, load.ts),
 *  as opposed to a read that failed. */
export class UnreadableDirectory extends Error {
  constructor(
    readonly repo: string,
    readonly commit: string,
    readonly dir: string,
    message: string
  ) {
    super(message);
  }
}

/** How the drop of a behaviour screen's case, or of a load test's task, begins when its context
 *  holds an UnreadableDirectory: the rest of the drop is the error's message. */
export const UNREADABLE_DIRECTORY = "unreadable-directory";

/** What a hit of a cache entry throws when a path of the entry's file list (`Builds`) is missing
 *  from the entry, or is no longer the kind, the size or the link target it was made with: the
 *  cache lost or changed a file, and a context built from it would differ in silence. Its message
 *  names the entry, how many listed paths differ, and up to 5 of them in full with what differs. */
export class IncompleteCacheEntry extends Error {
  constructor(
    entry: string,
    list: string,
    listed: number,
    changed: readonly { readonly file: string; readonly differs: string }[]
  ) {
    const shown = changed.slice(0, 5).map(({ file, differs }) => `${file} (${differs})`);
    if (changed.length > shown.length) shown.push(`and ${changed.length - shown.length} more`);
    super(
      `the cache entry ${entry} is not what it was made with: ${changed.length} of the ${listed} paths its list ${list} holds ${changed.length === 1 ? "is" : "are"} missing or changed: ${shown.join("; ")}. Removing the entry and its list builds it again.`
    );
  }
}

/** One build the context needed: what it is, its cache key, whether the cache held it already,
 *  and how long it took, in milliseconds. */
export interface Build {
  readonly kind: "skill" | "tree" | "plugin" | "compose" | "file" | "dir";
  readonly key: string;
  readonly hit: boolean;
  readonly ms: number;
}

/** What a capture's context used. */
export interface ContextRecord {
  readonly source: string;
  readonly skills: readonly {
    readonly name: string;
    /** The arm's own directory, the repository directory and its cache key, or a hidden stand-in
     *  of a skill a plugin reads as it loads (see the file's header). */
    readonly from:
      | "arm"
      | "stand-in"
      | { readonly repo: string; readonly commit: string; readonly path: string; key: string };
  }[];
  readonly plugins: readonly {
    readonly name: string;
    readonly repo: string;
    readonly commit: string;
    readonly path: string;
    /** `extension`: built and loaded with `--extension`; `skills only`: nothing to load beyond
     *  the profile's skills; `left out`: it could not be installed, for `reason`, which starts
     *  with the step that failed (its package.json does not parse, its build failed, or it could
     *  not be installed). */
    readonly as: "extension" | "skills only" | "left out";
    readonly key?: string;
    readonly reason?: string;
  }[];
  readonly contextFiles: readonly {
    readonly repo: string;
    readonly commit: string;
    readonly path: string;
    readonly key: string;
    readonly as: "working directory" | "system prompt" | "user level";
  }[];
  /** The role prompt: the case's own text, or the parts Compose named, or none. Compose ran at
   *  `commit` of `repo`: the commit the case's DAEMON_IMAGE_ITEM names (`commitOf: "daemon
   *  image"`), else the commit the profile names for that plugin repository (`"plugin"`). */
  readonly rolePrompt:
    | "case"
    | {
        readonly repo: string;
        readonly commit: string;
        readonly commitOf: "daemon image" | "plugin";
        readonly role: string;
        readonly isRoot: boolean;
        readonly promptsBlob: string;
        readonly key: string;
        readonly parts: readonly string[];
      }
    | null;
  /** For a subagent's context, the agent definition it ran, with the skills it autoloads
   *  (`agentIn`): the project's agent of its name (the checkout's, else the dev box's
   *  `~/.omp/agents`), else the dev box's user agent, else the profile's plugins'; null for
   *  another context or an agent none of them defines. */
  readonly agent: AgentDefinition | null;
  /** What of the real session's context the capture does not reproduce. */
  readonly leftOut: readonly string[];
  readonly builds: readonly Build[];
  /** The source's gh calls sent again after a transient failure while this context was built, by
   *  reason (`RepoSource.reads`; empty for a source that reads no GitHub). A context built at the
   *  same time from the same source adds its own. */
  readonly githubRetries: Readonly<Record<string, number>>;
}

/** The installed copy of each repository skill a context's `record` names (a `from` that names a
 *  cache key), as replay.ts answers a read of it (arms.ts's `otherSkillRead`): the name it is
 *  listed under, and its cache entry `<cacheDir>/skills/<key>`, the directory the context copied
 *  it from. Every arm of a case installs the same entry, so a read of it gets the same text in
 *  every arm. The arm's own skill (`arm`) and a stand-in (`stand-in`) are not repository skills,
 *  and are left out. */
export function installedCopies(
  record: Pick<ContextRecord, "skills">,
  cacheDir: string
): InstalledCopy[] {
  return record.skills.flatMap(({ name, from }) =>
    typeof from === "string" ? [] : [{ name, dir: path.join(cacheDir, "skills", from.key) }]
  );
}

/** A subagent's agent definition: its name, which of Oh My Pi's agent directories holds it
 *  (`source`: the project's, the user's or a plugin's, as Oh My Pi's own AgentDefinition names
 *  the first two), where it is (the repository and commit of the session repository for a project
 *  agent of the checkout, of the dev box's repository for a user agent or one of `~/.omp/agents`,
 *  or of a plugin of the profile, and the file's path in that repository: for a project agent of
 *  the checkout, under the `.omp/agents` Oh My Pi found nearest the working directory, or the
 *  directory a link there names), what links its directory where Oh My Pi reads it (`linkedBy`:
 *  the installer line, `<script>:<line>`, for the user agents and `~/.omp/agents`; each link of
 *  the repository followed, `<link> -> <target>`, for a project directory reached through one),
 *  and the skills its `autoloadSkills` names, which Oh My Pi puts into the subagent's session
 *  before its task (arms.ts). */
export interface AgentDefinition {
  readonly name: string;
  readonly source: "project" | "user" | "plugin";
  readonly repo: string;
  readonly commit: string;
  readonly path: string;
  readonly linkedBy?: string;
  readonly autoloadSkills: readonly string[];
}

/** A skill the context installs: its name, its directory, its frontmatter `description`, and
 *  whether Oh My Pi leaves it out of the system prompt's listing (`hidden`). */
export interface InstalledSkill {
  readonly name: string;
  readonly dir: string;
  readonly description: string;
  readonly hidden: boolean;
}

/** The context of one capture, written under `root`. */
export interface CaseContext {
  readonly skillsDir: string;
  /** The skills directory of each namespace of the profile (`namespaced/<namespace>/skills`), in
   *  the profile's order, which the capture gives Oh My Pi after `skillsDir`. */
  readonly namespacedDirs: readonly string[];
  readonly skills: readonly InstalledSkill[];
  /** Each built plugin's directory, as `--extension` takes it. */
  readonly extensions: readonly string[];
  /** The session's working directory: `checkout`, or the profile's `workingDirectory` in it. */
  readonly workspace: string;
  /** The session repository's checkout, where its context files are written at their paths. */
  readonly checkout: string;
  /** The `--append-system-prompt` file, when the context has one. */
  readonly appendSystemPrompt: string | undefined;
  /** The Claude Code config directory that holds a profile's user-level context file
   *  (`CLAUDE.md`), which the host's run names as `CLAUDE_CONFIG_DIR`; none for a profile with no
   *  such file. */
  readonly claudeConfigDir: string | undefined;
  readonly record: ContextRecord;
}

export interface ContextOptions {
  readonly source: RepoSource;
  /** The cache every build is kept in. */
  readonly cacheDir: string;
  /** The case's scratch directory: the same path for every arm of the case. */
  readonly root: string;
}

/** What a context reads of a case: its id, its context, its profile, and its evidence, which
 *  says whose session a subagent's is (arms.ts's `legionStore`). */
export type ContextCase = Pick<Case, "id" | "context" | "profile" | "evidence">;

/** The hex SHA-256 of `parts`, each ended by a NUL, cut to 32 characters: a cache key. */
export function cacheKey(...parts: string[]): string {
  const hash = createHash("sha256");
  for (const part of parts) hash.update(part).update("\0");
  return hash.digest("hex").slice(0, 32);
}

/** The git blob id of `bytes`, as `git hash-object` gives it. */
export function gitBlobId(bytes: Uint8Array): string {
  return createHash("sha1").update(`blob ${bytes.length}\0`).update(bytes).digest("hex");
}

/** The frontmatter of the SKILL.md text `text`, as Oh My Pi reads it for the listing (common.ts's
 *  `readFrontmatter`): its `description` ("" when it has none), and whether it is hidden from the
 *  listing, which Oh My Pi does for `hide: true` or `disable-model-invocation: true`
 *  (extensibility/skills.ts at the pinned tag). load.ts's `sessionDescription` reads the session's
 *  copy with it, and build.ts the copies of a repository. */
export function skillTextFrontmatter(text: string): { description: string; hidden: boolean } {
  return listingFrontmatter(readFrontmatter(text));
}

/** `skillTextFrontmatter` of the frontmatter `frontmatter`, read already (load.ts's
 *  `resolveTestSetup` reads the name from it too). */
export function listingFrontmatter(frontmatter: Readonly<Record<string, unknown>>): {
  description: string;
  hidden: boolean;
} {
  const description = frontmatter.description;
  return {
    description: typeof description === "string" ? description : "",
    hidden: frontmatter.hide === true || frontmatter.disableModelInvocation === true,
  };
}

/** The frontmatter of the skill in `dir` (`skillTextFrontmatter`), its file read as Oh My Pi reads
 *  a SKILL.md (common.ts's `ompText`: a leading byte order mark dropped). */
export function skillFrontmatter(dir: string): { description: string; hidden: boolean } {
  return skillTextFrontmatter(ompText(readFileSync(path.join(dir, "SKILL.md"))));
}

/** What a finished command gave; `timedOut` when its limit ended it. */
interface Ran {
  readonly code: number;
  readonly stdout: Uint8Array;
  readonly stderr: string;
  readonly timedOut: boolean;
}

/** Runs `argv` to its end, or until `timeoutMs` has passed, when it is ended and its stderr says
 *  so. */
async function run(
  argv: readonly string[],
  options: {
    readonly cwd?: string;
    readonly env?: Record<string, string | undefined>;
    readonly timeoutMs?: number;
  } = {}
): Promise<Ran> {
  const child = Bun.spawn([...argv], {
    cwd: options.cwd,
    env: options.env ?? process.env,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  // Ended at its limit (common.ts's `timeLimit`), its output let go then (`eachChunk`, `textOf`):
  // a process it started can hold the pipes open after it is gone.
  const limit = timeLimit(child, options.timeoutMs);
  const chunks: Uint8Array[] = [];
  const [, stderr, code] = await Promise.all([
    eachChunk(child.stdout, limit, (chunk) => chunks.push(chunk)),
    textOf(child.stderr, limit),
    child.exited,
  ]);
  limit.clear();
  const stdout = Buffer.concat(chunks);
  return {
    code,
    // The concatenated bytes as a plain Uint8Array, with no copy of them.
    stdout: new Uint8Array(stdout.buffer, stdout.byteOffset, stdout.byteLength),
    stderr: limit.timedOut() ? `${stderr}\ntimed out after ${options.timeoutMs} ms` : stderr,
    timedOut: limit.timedOut(),
  };
}

/** The longest each command that builds a context may run, from one run of each on the pod
 *  (2026-11-23, warm caches): `tar -xzf` of a 71 MB tarball (this repository's) took 5.1 s, so 10
 *  minutes is over 100 times that; `bun install --frozen-lockfile --ignore-scripts --filter` of a
 *  plugin took 2.0 s with bun's cache filled, and 15 minutes leaves room for an empty cache that
 *  downloads every package; `bun run build` took 1.1 s, so 10 minutes; the role prompt's `go test`
 *  took 8.4 s with Go's build cache filled, and 15 minutes leaves room for an empty one; a
 *  checkout's `scripts/daemon-contract.sh` took 0.08 s (2026-11-25), so 1 minute. */
export const COMMAND_TIMEOUT_MS = {
  tar: 600_000,
  bunInstall: 900_000,
  bunBuild: 600_000,
  goTest: 900_000,
  daemonContract: 60_000,
} as const;

/** Runs `argv` and throws, naming it and its last error lines, when it fails; it is ended once
 *  `timeoutMs` pass (common.ts's `timeLimit`), and throws naming it and the limit (`pastLimit`).
 *  Exported for context.test.ts. */
export async function mustRun(
  argv: readonly string[],
  timeoutMs: number,
  options: { readonly cwd?: string; readonly env?: Record<string, string | undefined> } = {}
): Promise<Ran> {
  const ran = await run(argv, { ...options, timeoutMs });
  if (ran.timedOut) throw new Error(pastLimit(argv.join(" "), timeoutMs));
  if (ran.code !== 0) {
    const tail = ran.stderr.trim().split("\n").slice(-12).join("\n");
    throw new Error(`\`${argv.join(" ")}\` failed with exit ${ran.code}: ${tail}`);
  }
  return ran;
}

/** One blob entry of a git tree, as `git/trees?recursive=1` lists it: copies.ts's TreeEntry less
 *  its type, which is `blob`. */
type BlobEntry = Omit<TreeEntry, "type">;

/** How many commits' tree listings `ghSource` keeps, the one used longest ago dropped first. One
 *  context reads ahead every repository and commit its skills come from before it writes any of
 *  them, so the bound is well above the groups of one profile; across cases the listings of old
 *  commits are dropped. */
const TREE_LISTINGS = 16;

/** How many one-level tree reads `ghSource` keeps, the one used longest ago dropped first: where
 *  GitHub cuts a commit's tree short, the levels along a skill directory's path, which `link`
 *  reads to find whether the directory is a link and `tree` then walks again. */
const TREE_LEVELS = 64;

/** Writes a blob of `mode` (git's: 100755 executable, 120000 a link) at `file`. */
function writeBlob(file: string, mode: string, bytes: Uint8Array): void {
  mkdirSync(path.dirname(file), { recursive: true });
  if (mode === "120000") {
    symlinkSync(new TextDecoder().decode(bytes), file);
    return;
  }
  writeFileSync(file, bytes);
  if (mode === "100755") chmodSync(file, 0o755);
}

/** The most blobs, and the most bytes, one GraphQL read of blobs asks for. */
const BLOB_BATCH = 50;
const BLOB_BATCH_BYTES = 4_000_000;
/** The longest the gh read of a repository's tarball may run; each other gh read runs at most
 *  copies.ts's GH_CALL_TIMEOUT_MS. A read ended at its limit is sent again, as common.ts's
 *  `GhRetries` sends a read that failed for a transient reason. */
const GH_TARBALL_TIMEOUT_MS = 900_000;
/** The most files a skill at a repository's root (a profile path of `.`) is read with: a root of
 *  more is a repository, not a skill's directory, and is never read whole for one. */
const ROOT_SKILL_FILES = 200;

/** The refusal of a skill at the root of `repo` at `commit` whose root holds `count` files, or
 *  `count` and more (`more`): more than ROOT_SKILL_FILES. */
function rootTooLarge(repo: string, commit: string, count: number, more: boolean): never {
  throw new UnreadableDirectory(
    repo,
    commit,
    ".",
    `${repo} at ${commit}: its root holds ${count}${more ? " or more" : ""} files, more than the ${ROOT_SKILL_FILES} a skill at a repository's root is read with`
  );
}

/** What `ghSource` takes beside the binary, each a test's when given: the wait before a read is
 *  sent again, and where each retry is said (`Bun.sleep` and standard error by default). */
export interface GhOptions {
  readonly sleep?: (ms: number) => Promise<void>;
  readonly err?: (line: string) => void;
}

/**
 * The repositories as GitHub serves them, read with `gh api` reads only: a file from its contents,
 * a directory from the commit's recursive tree and its blobs, the whole repository from its
 * tarball. A tree is read once per repository and commit while it is among the last TREE_LISTINGS
 * used, and only its blob entries' path, mode, id and size are kept; a tree read that failed is
 * not kept, so the next case at that commit reads it again. Where GitHub cuts a commit's
 * recursive tree short (`truncated`: a repository of too many entries), a directory is read
 * alone (`walk`): its tree found by one-level reads from the root along its path, then its own
 * recursive tree, or each of its levels read alone where GitHub cuts that short too, and each of
 * its blobs read alone over REST; `prefetch` reads nothing ahead for such a commit. A one-level
 * read is kept while it is among the last TREE_LEVELS used, so the levels `link` reads along a
 * path are not read again when `walk` follows it. A skill at the
 * repository's root (`.`) is its root's files, read the same way, at their whole paths: from the
 * commit's tree, or, where GitHub cuts it short, the root walked one level at a time; a root of
 * more than ROOT_SKILL_FILES files is an UnreadableDirectory, and `prefetch` reads none of it
 * ahead. Only `tree(repo, commit, "")`, a plugin's checkout, reads a whole repository, from its
 * tarball. `prefetch` reads the
 * blobs of many directories of one commit in GraphQL batches, `object(oid: …)` up to BLOB_BATCH
 * blobs and BLOB_BATCH_BYTES bytes a call, with `-f owner=<owner> -f name=<repo>` so the pod's gh
 * sends it with that owner's token (as copies.ts does); a blob a batch does not give whole
 * (binary, truncated, missing, in a batch that failed, or a text whose bytes are not the blob's,
 * checked by its git blob id) is read alone over REST.
 * A blob a batch gave is kept until each file of those directories that holds it is written, or
 * until the release `prefetch` gives is called (`held()` counts the blobs kept). Each read is
 * sent again after a transient failure, by the rule copies.ts's and build.ts's GitHub reads share
 * (common.ts's `GhRetries`), so one network error stops no screen; a 404 is not.
 * `reads()` counts the `gh` calls made, the time in which one ran, and the calls sent again.
 */
export function ghSource(
  gh = "gh",
  options: GhOptions = {}
): RepoSource & { readonly held: () => number } {
  const retries = new GhRetries(
    {
      sleep: options.sleep ?? Bun.sleep,
      err: options.err ?? ((line) => process.stderr.write(`${line}\n`)),
    },
    "context"
  );
  type Listing = { readonly entries: readonly BlobEntry[]; readonly truncated: boolean };
  const trees = new Map<string, Promise<Listing>>();
  // The gh calls made so far, and the milliseconds in which at least one was running.
  let calls = 0;
  let running = 0;
  let busyFrom = 0;
  let busyMs = 0;
  const counted = async <T>(call: () => Promise<T>): Promise<T> => {
    calls += 1;
    if (running === 0) busyFrom = performance.now();
    running += 1;
    try {
      return await call();
    } finally {
      running -= 1;
      if (running === 0) busyMs += performance.now() - busyFrom;
    }
  };
  // Every gh call waits for a place in the runner's one bound on gh processes (common.ts's
  // `ghLimiter`) before it starts, so the wait is not counted as a call running.
  const ghRun = (argv: readonly string[]) =>
    ghLimiter().run(() => counted(() => run(argv, { timeoutMs: GH_CALL_TIMEOUT_MS })));
  // A rate limit's reset is read with the gh of these reads, as common.ts's
  // `untilInstallationReset` reads it.
  const resetIo = {
    gh: async (args: readonly string[]) => {
      const ran = await ghRun([gh, ...args]);
      return { code: ran.code, stdout: new TextDecoder().decode(ran.stdout) };
    },
    say: (line: string) => retries.say(line),
  };
  // The first try of `attempt`, one try of the gh call `args` (said as `command`), that exits 0,
  // the call sent again as `GhRetries.send` says (`splits`: a GraphQL batch of more than one
  // blob); throws naming `command` and each try's error when none does. A refusal for the primary
  // rate limit waits for the reset of the installation that refused the call, the one of the
  // owner it names (common.ts's `installationRateWait`).
  const retried = async <T extends { readonly code: number; readonly stderr: string }>(
    command: string,
    args: readonly string[],
    attempt: () => Promise<T>,
    splits = false
  ): Promise<T> => {
    const ran = await retries.send(attempt, {
      splits,
      rateWait: installationRateWait(args, resetIo),
    });
    if (ran.code !== 0) {
      throw new Error(`\`${command}\` failed: ${firstLine(ran.stderr) || `gh exited ${ran.code}`}`);
    }
    return ran;
  };
  const api = async (route: string, raw: boolean): Promise<Uint8Array> => {
    const accept = raw ? ["-H", "Accept: application/vnd.github.raw"] : [];
    const args = ["api", "--method", "GET", ...accept, route];
    const argv = [gh, ...args];
    return (await retried(argv.join(" "), args, () => ghRun(argv))).stdout;
  };
  const tarball = async (repo: string, commit: string, dest: string): Promise<void> => {
    mkdirSync(dest, { recursive: true });
    const archive = `${dest}.tar.gz`;
    try {
      const args = ["api", "--method", "GET", `repos/${repo}/tarball/${commit}`];
      const argv = [gh, ...args];
      await retried(argv.join(" "), args, () =>
        ghLimiter().run(() =>
          counted(async () => {
            const child = Bun.spawn(argv, {
              stdin: "ignore",
              stdout: Bun.file(archive),
              stderr: "pipe",
            });
            const limit = timeLimit(child, GH_TARBALL_TIMEOUT_MS);
            const [stderr, code] = await Promise.all([textOf(child.stderr, limit), child.exited]);
            limit.clear();
            return {
              code,
              stderr: limit.timedOut()
                ? `${stderr}\ntimed out after ${GH_TARBALL_TIMEOUT_MS} ms`
                : stderr,
            };
          })
        )
      );
      await mustRun(
        ["tar", "-xzf", archive, "-C", dest, "--strip-components=1"],
        COMMAND_TIMEOUT_MS.tar
      );
    } finally {
      rmSync(archive, { force: true });
    }
  };
  // The tree `sha` of `repo` (a commit's id names its root tree): one level of it, or all of it
  // when `recursive`, and whether GitHub cut the listing short.
  const treeRead = async (
    repo: string,
    sha: string,
    recursive: boolean
  ): Promise<{ tree: TreeEntry[]; truncated: boolean }> => {
    const route = `repos/${repo}/git/trees/${sha}${recursive ? "?recursive=1" : ""}`;
    return JSON.parse(new TextDecoder().decode(await api(route, false)));
  };
  // `read()`, kept in `map` under `id` while it is among the last `limit` used, the one used
  // longest ago dropped first. A read that failed is dropped, so the next one reads again.
  const keptRead = <T>(
    map: Map<string, Promise<T>>,
    id: string,
    limit: number,
    read: () => Promise<T>
  ): Promise<T> => {
    const kept = map.get(id);
    if (kept !== undefined) {
      // Used again: it goes to the end, the last to be dropped.
      map.delete(id);
      map.set(id, kept);
      return kept;
    }
    const fresh = read();
    fresh.catch(() => {
      if (map.get(id) === fresh) map.delete(id);
    });
    map.set(id, fresh);
    for (const old of map.keys()) {
      if (map.size <= limit) break;
      map.delete(old);
    }
    return fresh;
  };
  // The commit's recursive tree, its blob entries only, read once while it is among the last
  // TREE_LISTINGS used.
  const listingOf = (repo: string, commit: string): Promise<Listing> =>
    keptRead(trees, `${repo}@${commit}`, TREE_LISTINGS, () =>
      treeRead(repo, commit, true).then((parsed) => {
        const entries = parsed.tree
          .filter((entry) => entry.type === "blob")
          .map((entry) => ({
            path: entry.path,
            mode: entry.mode,
            sha: entry.sha,
            size: entry.size,
          }));
        return { entries, truncated: parsed.truncated };
      })
    );
  // One level of the tree `sha` of `repo` (a commit's id names its root tree), read once while it
  // is among the last TREE_LEVELS used: a tree's id fixes what it holds.
  const levels = new Map<string, Promise<{ tree: TreeEntry[]; truncated: boolean }>>();
  const levelOf = (repo: string, sha: string) =>
    keptRead(levels, `${repo}@${sha}`, TREE_LEVELS, () => treeRead(repo, sha, false));
  // One level of the tree `sha` of `repo` at `commit`, the tree of `at` ("" the root), read for
  // `named` (the directory `walk` reads, or the parent of the path `link` looks up): an
  // UnreadableDirectory naming `named` when GitHub cuts even that one level short.
  const levelAlone = async (
    repo: string,
    commit: string,
    named: string,
    sha: string,
    at: string
  ): Promise<TreeEntry[]> => {
    const read = await levelOf(repo, sha);
    if (!read.truncated) return read.tree;
    throw new UnreadableDirectory(
      repo,
      commit,
      named,
      `${repo} at ${commit}: ${named} cannot be read: GitHub cuts the tree of ${at === "" ? "the root" : at} short even one level at a time`
    );
  };
  // The blob entries under `dir` of `repo` at `commit`, a commit whose recursive tree GitHub cuts
  // short, at their paths in the repository, with no whole repository read: `dir`'s tree found by
  // one-level reads from the root along its path, then its own recursive tree, or, where GitHub
  // cuts that short too, each of its levels read alone, four at once. `dir` `.` is the root, whose
  // own recursive tree is the commit's that GitHub cut short: its levels are read alone from the
  // first, and the walk stops with a refusal once it has found more than ROOT_SKILL_FILES files.
  // Throws UnreadableDirectory when the commit holds no such directory, or GitHub cuts a level
  // short even read alone.
  const walk = async (repo: string, commit: string, dir: string): Promise<BlobEntry[]> => {
    const root = dir === ".";
    let sha = commit;
    let at = "";
    if (!root) {
      for (const name of dir.split("/")) {
        const entry = (await levelAlone(repo, commit, dir, sha, at)).find(
          (each) => each.path === name
        );
        if (entry?.type !== "tree") {
          throw new UnreadableDirectory(
            repo,
            commit,
            dir,
            `${repo} at ${commit} holds no directory ${dir}`
          );
        }
        sha = entry.sha;
        at = at === "" ? name : `${at}/${name}`;
      }
      const whole = await treeRead(repo, sha, true);
      if (!whole.truncated) {
        return whole.tree
          .filter((entry) => entry.type === "blob")
          .map((entry) => ({ ...entry, path: `${dir}/${entry.path}` }));
      }
    }
    const files: BlobEntry[] = [];
    for (let trees = [{ sha, at }]; trees.length > 0; ) {
      const levels = await mapLimit(trees, 4, async (tree) => ({
        parent: tree.at,
        entries: await levelAlone(repo, commit, dir, tree.sha, tree.at),
      }));
      trees = [];
      for (const { parent, entries } of levels) {
        for (const entry of entries) {
          const where = { ...entry, path: parent === "" ? entry.path : `${parent}/${entry.path}` };
          if (entry.type === "tree") trees.push({ sha: entry.sha, at: where.path });
          else if (entry.type === "blob") files.push(where);
        }
      }
      if (root && files.length > ROOT_SKILL_FILES) {
        rootTooLarge(repo, commit, files.length, trees.length > 0);
      }
    }
    return files;
  };
  // The prefix of the paths of the files under `dir` in the repository: `<dir>/`, or none for
  // `.`, the root.
  const prefixOf = (dir: string): string => {
    const inside = dir.replace(/\/+$/, "");
    return inside === "." ? "" : `${inside}/`;
  };
  // Each blob a GraphQL batch gave whole, by its id: its bytes, and the files of the directories
  // read ahead still to be written from it, each as its `fileKey` (a file under two such
  // directories is written once for each). It is dropped once the last is written, or released
  // (`prefetch`'s release).
  const blobs = new Map<string, { readonly bytes: Uint8Array; readonly waiting: Set<string> }>();
  // The file `file` of `repo` at `commit` under the directory `prefix` (`prefixOf`) as `blobs`
  // holds it: `prefetch` adds it and `tree` marks it written, so a blob is let go only when both
  // build it alike.
  const fileKey = (repo: string, commit: string, prefix: string, file: string) =>
    `${repo}@${commit}\0${prefix}\0${file}`;
  /** Marks the file `key` of the blob `sha` written, or no longer to be written; drops the blob
   *  when no file waits on it. */
  const done = (sha: string, key: string) => {
    const held = blobs.get(sha);
    if (held === undefined) return;
    held.waiting.delete(key);
    if (held.waiting.size === 0) blobs.delete(sha);
  };
  return {
    name: "gh",
    held: () => blobs.size,
    reads: () => ({
      calls,
      ms: busyMs + (running > 0 ? performance.now() - busyFrom : 0),
      retries: retries.record(),
    }),
    file: async (repo, commit, file) => {
      const route = file.split("/").map(encodeURIComponent).join("/");
      try {
        return await api(`repos/${repo}/contents/${route}?ref=${encodeURIComponent(commit)}`, true);
      } catch (error) {
        // GitHub answers a path the commit does not hold with 404; any other failure is a failure.
        const said = messageFor(error);
        if (/HTTP 404/.test(said))
          throw new NoSuchFile(`${repo} at ${commit} holds no file ${file}: ${said}`);
        throw error;
      }
    },
    async prefetch(repo, commit, dirs) {
      // Each blob this read ahead holds files of, by its id, with those files.
      const mine = new Map<string, Set<string>>();
      const release = () => {
        for (const [sha, keys] of mine) for (const key of keys) done(sha, key);
      };
      const { entries, truncated } = await listingOf(repo, commit);
      // A commit whose tree GitHub cuts short is read one directory at a time, in `tree` (`walk`).
      if (truncated) return release;
      // Each blob to read, by its id.
      const wanted = new Map<string, BlobEntry>();
      for (const prefix of dirs.map(prefixOf)) {
        // A root of more files than a skill at the root is read with is refused in `tree`.
        if (prefix === "" && entries.length > ROOT_SKILL_FILES) continue;
        for (const entry of entries) {
          if (!/^[0-9a-f]{40}$/.test(entry.sha) || !entry.path.startsWith(prefix)) continue;
          const key = fileKey(repo, commit, prefix, entry.path);
          mine.set(entry.sha, (mine.get(entry.sha) ?? new Set()).add(key));
          const held = blobs.get(entry.sha);
          if (held !== undefined) held.waiting.add(key);
          else if (!wanted.has(entry.sha)) wanted.set(entry.sha, entry);
        }
      }
      const entriesWanted = [...wanted.values()];
      for (const batch of blobBatches(entriesWanted, BLOB_BATCH, BLOB_BATCH_BYTES)) {
        const args = graphqlArgs(repo, blobQuery(batch));
        let answer: unknown;
        try {
          const ran = await retried(
            `${[gh, ...args.slice(0, -2)].join(" ")} -f query=<${batch.length} blobs>`,
            args,
            () => ghRun([gh, ...args]),
            batch.length > 1
          );
          answer = parseJson(new TextDecoder().decode(ran.stdout));
        } catch {
          // The batch failed for good, or GitHub answered it as a batch too large while it held
          // more than one blob: each of its blobs is read alone, over REST, in `tree`.
          continue;
        }
        // An answer not of its shape gives no blob: each is read alone, over REST, in `tree`.
        const parsed = BlobBatch.safeParse(answer);
        if (!parsed.success) continue;
        batch.forEach((entry, index) => {
          const blob = parsed.data.data.repository?.[`b${index}`];
          if (blob?.isBinary !== false || blob.isTruncated || blob.text === null) return;
          // Kept only when its bytes are the blob's: a text GraphQL decoded with a loss reads
          // alone.
          const bytes = new TextEncoder().encode(blob.text);
          if (gitBlobId(bytes) === entry.sha) {
            blobs.set(entry.sha, { bytes, waiting: new Set(mine.get(entry.sha)) });
          }
        });
      }
      return release;
    },
    async link(repo, commit, file) {
      const { entries, truncated } = await listingOf(repo, commit);
      let entry: { readonly mode: string; readonly sha: string } | undefined;
      if (!truncated) entry = entries.find((each) => each.path === file);
      else {
        // A commit whose recursive tree GitHub cuts short: the one level of the link's parent,
        // read one level at a time from the root along its path, as `walk` reads a directory; a
        // level GitHub cuts short even alone is an UnreadableDirectory, as in `walk`.
        const parent = path.posix.dirname(file);
        let listed = await levelAlone(repo, commit, parent, commit, "");
        let at = "";
        for (const name of parent === "." ? [] : parent.split("/")) {
          const tree = listed.find((each) => each.path === name);
          if (tree?.type !== "tree") return undefined;
          at = at === "" ? name : `${at}/${name}`;
          listed = await levelAlone(repo, commit, parent, tree.sha, at);
        }
        entry = listed.find((each) => each.path === path.posix.basename(file));
      }
      if (entry?.mode !== "120000") return undefined;
      return new TextDecoder().decode(await api(`repos/${repo}/git/blobs/${entry.sha}`, true));
    },
    async tree(repo, commit, dir, dest) {
      if (dir === "") return tarball(repo, commit, dest);
      const { entries, truncated } = await listingOf(repo, commit);
      const inside = dir.replace(/\/+$/, "");
      const prefix = prefixOf(inside);
      const files = truncated
        ? await walk(repo, commit, inside)
        : entries.filter((entry) => entry.path.startsWith(prefix));
      if (prefix === "" && files.length > ROOT_SKILL_FILES) {
        rootTooLarge(repo, commit, files.length, false);
      }
      if (files.length === 0) {
        throw new UnreadableDirectory(
          repo,
          commit,
          inside,
          `${repo} at ${commit} holds no file under ${inside}`
        );
      }
      await mapLimit(files, 4, async (entry) => {
        const held = blobs.get(entry.sha);
        const bytes = held?.bytes ?? (await api(`repos/${repo}/git/blobs/${entry.sha}`, true));
        writeBlob(path.join(dest, entry.path.slice(prefix.length)), entry.mode, bytes);
        if (held !== undefined) done(entry.sha, fileKey(repo, commit, prefix, entry.path));
      });
    },
  };
}

/** Repositories as directories: `root/<owner>/<name>/<commit>/` holds the repository at that
 *  commit. */
export function localSource(root: string): RepoSource {
  const at = (repo: string, commit: string) => path.join(root, repo, commit);
  return {
    name: `local directory ${root}`,
    async file(repo, commit, file) {
      const where = path.join(at(repo, commit), file);
      if (!existsSync(where)) throw new NoSuchFile(`${repo} at ${commit} holds no file ${file}`);
      return new Uint8Array(readFileSync(where));
    },
    async link(repo, commit, file) {
      const where = path.join(at(repo, commit), file);
      try {
        return lstatSync(where).isSymbolicLink() ? readlinkSync(where) : undefined;
      } catch {
        return undefined;
      }
    },
    async tree(repo, commit, dir, dest) {
      const where = path.join(at(repo, commit), dir);
      if (!existsSync(where)) {
        throw new UnreadableDirectory(
          repo,
          commit,
          dir,
          `${repo} at ${commit} holds no directory ${dir}`
        );
      }
      cpSync(where, dest, { recursive: true, verbatimSymlinks: true });
    },
  };
}

/** One file or symbolic link of a cache entry, as the entry's file list keeps it (`Builds`): its
 *  path below the entry (`.` for an entry that is one file), and its size in bytes or its target. */
type Listed =
  | { readonly path: string; readonly kind: "file"; readonly size: number }
  | { readonly path: string; readonly kind: "link"; readonly target: string };

/** Each file and symbolic link of the entry at `at` (a directory, or one file). */
function entryFiles(at: string): Listed[] {
  const files: Listed[] = [];
  const visit = (full: string, below: string): void => {
    const stats = lstatSync(full);
    if (stats.isSymbolicLink()) {
      files.push({ path: below, kind: "link", target: readlinkSync(full) });
    } else if (stats.isFile()) {
      files.push({ path: below, kind: "file", size: stats.size });
    } else if (stats.isDirectory()) {
      for (const name of readdirSync(full).sort()) {
        visit(path.join(full, name), path.join(below, name));
      }
    }
  };
  visit(at, ".");
  return files;
}

/** How `full`, the path of `listed` in its entry, differs from it, or undefined when it does not. */
function listedChange(full: string, listed: Listed): string | undefined {
  let found: Stats | undefined;
  try {
    found = lstatSync(full, { throwIfNoEntry: false });
  } catch (error) {
    // A directory on its path is no directory now.
    if (!hasErrnoCode(error, "ENOTDIR")) throw error;
  }
  if (found === undefined) return "missing";
  const kind = found.isSymbolicLink()
    ? "link"
    : found.isFile()
      ? "file"
      : found.isDirectory()
        ? "directory"
        : "special file";
  if (kind !== listed.kind) return `a ${listed.kind} recorded, a ${kind} found`;
  if (listed.kind === "file") {
    return found.size === listed.size
      ? undefined
      : `size ${listed.size} recorded, ${found.size} found`;
  }
  const target = readlinkSync(full);
  return target === listed.target ? undefined : `target ${listed.target} recorded, ${target} found`;
}

/** The builds of one context, each timed and kept in the cache. Each entry
 *  `cacheDir/<kind>s/<key>` has its file list, `cacheDir/lists/<kind>s/<key>.json`: each file and
 *  symbolic link the entry was made with, by its path, its size or its target (`entryFiles`), kept
 *  apart so that nothing that reads an entry or lists a kind's entries finds it. A hit is checked
 *  against its list, and a listed path that is missing or changed is an IncompleteCacheEntry: a
 *  cache restored from a save that kept only files newer than a cutoff held checkouts without
 *  their plugins' package.json, which installed those plugins as `skills only`, with nothing to
 *  say so. A file the entry holds beyond its list is no change (a compose writes its Go test into
 *  a checkout while it runs). A plugin's build (`buildPlugin`) runs inside its cached checkout:
 *  measured on real plugins, a build adds files and rewrites no listed one (a capture on a fresh
 *  cache, then a second capture that hit every checkout with its list checked), and a build that
 *  did rewrite one is refused here with its path, never taken in silence. An entry with no list
 *  was made before lists were kept: a tree is made again, since only a tree's files keep their
 *  source's file times (the tarball's), so only a tree can be partly kept by a save that cuts by
 *  file time; any other entry, written by the runner at build time, is taken unchecked, since
 *  making each again would cost a GitHub read per file of thousands of entries. */
class Builds {
  readonly list: Build[] = [];
  constructor(readonly cacheDir: string) {}

  /** `cacheDir/<kind>s/<key>`, checked against its list when the cache holds it; else made by
   *  `make` into a fresh path, whose list is renamed into place before the entry is. */
  async entry(
    kind: Build["kind"],
    key: string,
    make: (dest: string) => Promise<void>
  ): Promise<string> {
    const started = performance.now();
    const where = path.join(this.cacheDir, `${kind}s`, key);
    const listFile = path.join(this.cacheDir, "lists", `${kind}s`, `${key}.json`);
    let hit = existsSync(where);
    if (hit && existsSync(listFile)) {
      const listed = JSON.parse(readFileSync(listFile, "utf8")) as Listed[];
      const changed = listed.flatMap((each) => {
        const file = path.join(where, each.path);
        const differs = listedChange(file, each);
        return differs === undefined ? [] : [{ file, differs }];
      });
      if (changed.length > 0) {
        throw new IncompleteCacheEntry(where, listFile, listed.length, changed);
      }
    } else if (hit && kind === "tree") {
      // Made before lists were kept: moved aside, removed, and made again.
      const aside = `${where}.unlisted-${randomBytes(4).toString("hex")}`;
      try {
        renameSync(where, aside);
      } catch (error) {
        // Another capture moved it aside first.
        if (!hasErrnoCode(error, "ENOENT")) throw error;
      }
      rmSync(aside, { recursive: true, force: true });
      hit = false;
    }
    if (!hit) {
      mkdirSync(path.dirname(where), { recursive: true });
      mkdirSync(path.dirname(listFile), { recursive: true });
      const part = `part-${randomBytes(4).toString("hex")}`;
      const fresh = `${where}.${part}`;
      const freshList = `${listFile}.${part}`;
      try {
        await make(fresh);
        writeFileSync(freshList, `${JSON.stringify(entryFiles(fresh))}\n`);
        renameSync(freshList, listFile);
        renameSync(fresh, where);
      } catch (error) {
        rmSync(fresh, { recursive: true, force: true });
        rmSync(freshList, { force: true });
        // Another capture made the same entry first: it is the same build.
        if (!existsSync(where)) throw error;
        hit = true;
      }
    }
    this.list.push({ kind, key, hit, ms: Math.round(performance.now() - started) });
    return where;
  }
}

/** The text of `file` of `repo` at `commit`, through the cache. */
async function fileText(
  builds: Builds,
  source: RepoSource,
  repo: string,
  commit: string,
  file: string
): Promise<{ text: string; key: string }> {
  const key = cacheKey(repo, commit, file);
  const where = await builds.entry("file", key, async (dest) => {
    writeFileSync(dest, await source.file(repo, commit, file));
  });
  return { text: readFileSync(where, "utf8"), key };
}

/** The whole of `repo` at `commit`, through the cache. */
function checkout(builds: Builds, source: RepoSource, repo: string, commit: string) {
  return builds.entry("tree", cacheKey(repo, commit), (dest) =>
    source.tree(repo, commit, "", dest)
  );
}

/** A plugin's `package.json`, as far as the build reads it. */
interface Manifest {
  files?: unknown;
  scripts?: { build?: unknown };
  workspaces?: unknown;
  omp?: { extensions?: unknown; skills?: unknown };
  legion?: { daemonApiVersion?: unknown };
  [field: string]: unknown;
}

/** The strings of `value` when it is a list of them, else none. */
function strings(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === "string")
    : [];
}

/** A path a manifest names, without its leading `./`. */
function manifestPath(entry: string): string {
  return path.normalize(entry).replace(/^\.\//, "");
}

/** Where a checkout holds the script that prints its daemon API contract. */
const DAEMON_CONTRACT_SCRIPT = path.join("scripts", "daemon-contract.sh");

/** The contract a pack writes into the manifest of a plugin built from `tree` as
 *  `legion.daemonApiVersion`: what the checkout's own script prints, since a plugin of 2026-11-25
 *  or later reads the field as it loads (pi-legion's `daemonContract`) and refuses to load without
 *  it. None when `manifest` holds the field already or `tree` has no script, as every checkout
 *  from before the field is. The script reads the checkout's files alone, so it runs with no
 *  `.git`, as in a cached checkout. */
async function packedContract(tree: string, manifest: Manifest): Promise<number | undefined> {
  if (manifest.legion?.daemonApiVersion !== undefined) return undefined;
  if (!existsSync(path.join(tree, DAEMON_CONTRACT_SCRIPT))) return undefined;
  const argv = ["bash", DAEMON_CONTRACT_SCRIPT];
  const ran = await mustRun(argv, COMMAND_TIMEOUT_MS.daemonContract, { cwd: tree });
  const printed = new TextDecoder().decode(ran.stdout).trim();
  const contract = /^(0|[1-9][0-9]*)$/.test(printed) ? Number(printed) : Number.NaN;
  if (!Number.isSafeInteger(contract)) {
    throw new Error(
      `\`${argv.join(" ")}\` printed ${JSON.stringify(printed)}, not a non-negative integer, so the plugin cannot be built the way a pack builds it`
    );
  }
  return contract;
}

/** The last build of each checkout, by its path: a build puts the contract into the plugin's
 *  package.json in the checkout and the original back after (`buildPlugin`), so builds in one
 *  checkout run one after the other. A second that read the file while it held the contract would
 *  take it as the plugin's own, write none, and build once the first had put the original back:
 *  a bundle with no contract, kept under the plugin's cache key. */
const lastBuilds = new Map<string, Promise<unknown>>();

/** Runs `build` once the builds of `tree` that began before it have ended, failed or not. */
function inTurn<T>(tree: string, build: () => Promise<T>): Promise<T> {
  const turn = (lastBuilds.get(tree) ?? Promise.resolve()).then(build, build);
  lastBuilds.set(
    tree,
    turn.catch(() => undefined)
  );
  return turn;
}

/** The plugin `path` of the repository checked out at `tree`, built into `dest` (see the file's
 *  header). Where a pack would write the daemon API contract into the manifest (`packedContract`),
 *  the build runs with it in the plugin's package.json, which the bundle inlines, and the file is
 *  put back to its bytes after, so the cached checkout is as it was; `dest` holds it as well. */
async function buildPlugin(tree: string, pluginPath: string, dest: string): Promise<void> {
  const dir = path.join(tree, pluginPath);
  const manifestFile = path.join(dir, "package.json");
  const original = readFileSync(manifestFile);
  const manifest = JSON.parse(original.toString("utf8")) as Manifest;
  const contract = await packedContract(tree, manifest);
  if (contract !== undefined) manifest.legion = { ...manifest.legion, daemonApiVersion: contract };
  const lockfile = (at: string) =>
    existsSync(path.join(at, "bun.lock")) || existsSync(path.join(at, "bun.lockb"));
  const install = ["bun", "install", "--frozen-lockfile", "--ignore-scripts"];
  const rootFile = path.join(tree, "package.json");
  const root: Manifest = existsSync(rootFile) ? JSON.parse(readFileSync(rootFile, "utf8")) : {};
  if (pluginPath !== "" && root.workspaces !== undefined && lockfile(tree)) {
    await mustRun([...install, "--filter", `./${pluginPath}`], COMMAND_TIMEOUT_MS.bunInstall, {
      cwd: tree,
    });
  } else if (lockfile(dir)) {
    await mustRun(install, COMMAND_TIMEOUT_MS.bunInstall, { cwd: dir });
  }
  if (typeof manifest.scripts?.build === "string") {
    try {
      if (contract !== undefined) {
        writeFileSync(manifestFile, json(manifest));
      }
      await mustRun(["bun", "run", "build"], COMMAND_TIMEOUT_MS.bunBuild, { cwd: dir });
    } finally {
      if (contract !== undefined) writeFileSync(manifestFile, original);
    }
  }
  const files = strings(manifest.files);
  if (files.length === 0) {
    throw new Error("its package.json has no `files` list, so what the plugin ships is not known");
  }
  const skills = strings(manifest.omp?.skills).map((entry) => path.join(dir, manifestPath(entry)));
  mkdirSync(dest, { recursive: true });
  for (const entry of files) {
    const from = path.join(dir, manifestPath(entry));
    if (!existsSync(from)) continue;
    cpSync(from, path.join(dest, manifestPath(entry)), {
      recursive: true,
      verbatimSymlinks: true,
      filter: (source) =>
        !skills.some((skill) => source === skill || source.startsWith(`${skill}/`)),
    });
  }
  const extensions = strings(manifest.omp?.extensions).map((entry) => {
    const bundle = path.join("dist", path.basename(entry).replace(/\.[cm]?tsx?$/, ".js"));
    if (/\.[cm]?tsx?$/.test(entry) && existsSync(path.join(dest, bundle))) return bundle;
    if (existsSync(path.join(dest, manifestPath(entry)))) return manifestPath(entry);
    throw new Error(`its extension ${entry} has no built file among the files it ships`);
  });
  writeFileSync(
    path.join(dest, "package.json"),
    json({ ...manifest, omp: { ...manifest.omp, extensions } })
  );
}

/** The role of a Legion context (`legion:<project>:<role>`), as Compose takes it, or undefined
 *  for a context that is not Legion's. */
export function legionRole(
  context: string
): { readonly role: string; readonly isRoot: boolean } | undefined {
  const match = /^legion:[^:\s]+:([^:\s]+)$/.exec(context);
  if (match?.[1] === undefined) return undefined;
  return match[1] === "architect-root"
    ? { role: "architect", isRoot: true }
    : { role: match[1], isRoot: false };
}

/** The Go test that asks the composer of `module` for a role's parts: it writes the paths Compose
 *  answers, relative to the state directory it gave `New`, as a JSON list. For the controller it
 *  asks `ControllerPromptPaths(true)`, the parts of a controller the daemon runs headless in a pod,
 *  as it runs every controller of a Legion store's sessions. */
function composeTest(module: string, controller: boolean): string {
  const call = controller
    ? "paths, err := composer.ControllerPromptPaths(true)"
    : `parts, err := composer.Compose(claim.Role(os.Getenv("SKILL_CUTS_ROLE")), os.Getenv("SKILL_CUTS_ROOT") == "1")
\tpaths := parts.RolePromptPaths`;
  return `package zzskillcuts

import (
\t"encoding/json"
\t"os"
\t"path/filepath"
\t"testing"
${controller ? "" : `\n\t"${module}/internal/claim"`}
\t"${module}/internal/prompts"
)

func TestSkillCutsCompose(t *testing.T) {
\tstate := os.Getenv("SKILL_CUTS_STATE")
\tcomposer, err := prompts.New(state)
\tif err != nil {
\t\tt.Fatal(err)
\t}
\t${call}
\tif err != nil {
\t\tt.Fatal(err)
\t}
\trelative := make([]string, 0, len(paths))
\tfor _, part := range paths {
\t\tname, err := filepath.Rel(state, part)
\t\tif err != nil {
\t\t\tt.Fatal(err)
\t\t}
\t\trelative = append(relative, filepath.ToSlash(name))
\t}
\tout, err := json.Marshal(relative)
\tif err != nil {
\t\tt.Fatal(err)
\t}
\tif err := os.WriteFile(os.Getenv("SKILL_CUTS_OUT"), out, 0o600); err != nil {
\t\tt.Fatal(err)
\t}
}
`;
}

/** The parts Compose names for `role` in the repository checked out at `tree`, as paths inside
 *  the repository, each checked to be the file `New` wrote for it. */
async function composeParts(
  tree: string,
  role: string,
  isRoot: boolean,
  scratch: string
): Promise<string[]> {
  const promptsDir = path.dirname(path.join(tree, PROMPTS_GO));
  let moduleDir = promptsDir;
  while (!existsSync(path.join(moduleDir, "go.mod"))) {
    if (moduleDir === tree) throw new Error(`no go.mod holds ${PROMPTS_GO}`);
    moduleDir = path.dirname(moduleDir);
  }
  const module = /^module\s+(\S+)/m.exec(readFileSync(path.join(moduleDir, "go.mod"), "utf8"))?.[1];
  if (module === undefined) throw new Error(`the go.mod above ${PROMPTS_GO} names no module`);
  const name = `zz_skill_cuts_${randomBytes(4).toString("hex")}`;
  const testDir = path.join(moduleDir, "internal", name);
  mkdirSync(scratch, { recursive: true });
  const state = mkdtempSync(path.join(scratch, "compose-state-"));
  const out = path.join(state, "parts.json");
  try {
    mkdirSync(testDir, { recursive: true });
    writeFileSync(
      path.join(testDir, "compose_test.go"),
      composeTest(module, role === "controller")
    );
    await mustRun(
      ["go", "test", "-count=1", "-run", "^TestSkillCutsCompose$", `./internal/${name}/`],
      COMMAND_TIMEOUT_MS.goTest,
      {
        cwd: moduleDir,
        env: {
          ...process.env,
          SKILL_CUTS_STATE: state,
          SKILL_CUTS_ROLE: role,
          SKILL_CUTS_ROOT: isRoot ? "1" : "0",
          SKILL_CUTS_OUT: out,
        },
      }
    );
    const written = JSON.parse(readFileSync(out, "utf8")) as string[];
    // `New` snapshots the embedded `roles/` parts under prompts/shared and the `go/` parts under
    // prompts/go; each part named is checked against that file of the repository.
    return written.map((part) => {
      const [top, kind, ...rest] = part.split("/");
      const embedded = kind === "shared" ? "roles" : kind === "go" ? "go" : undefined;
      if (top !== "prompts" || embedded === undefined || rest.length === 0) {
        throw new Error(`Compose named ${part}, which is no part this reader knows`);
      }
      const inRepo = path.relative(tree, path.join(promptsDir, embedded, ...rest));
      const repoBytes = readFileSync(path.join(tree, inRepo));
      if (!repoBytes.equals(readFileSync(path.join(state, part)))) {
        throw new Error(`Compose's part ${part} is not the repository's ${inRepo}`);
      }
      return inRepo;
    });
  } finally {
    rmSync(testDir, { recursive: true, force: true });
    rmSync(state, { recursive: true, force: true });
  }
}

/** The git blob id of PROMPTS_GO in each `<repo>@<commit>` a source has read it from: a commit's
 *  file never changes, so it is read once per source. A read that failed is not kept. */
const promptsBlobs = new WeakMap<RepoSource, Map<string, string>>();

/** The commit of `repo` that the evidence item DAEMON_IMAGE_ITEM of `c` names as its daemon
 *  image's, in `composedAt`'s words; undefined when `c` has no such item of `repo` or its item
 *  names no commit (no rule gave one). load.ts reads it too, to check a saved capture. */
export function daemonCommit(
  c: { readonly evidence?: readonly { readonly item: string; readonly source: string }[] },
  repo: string
): string | undefined {
  const item = c.evidence?.find(
    (each) => each.item.toLowerCase() === `${DAEMON_IMAGE_ITEM}${repo}`.toLowerCase()
  );
  return /\bso the role prompt is composed from \S+ at ([0-9a-f]{40})\b/.exec(
    item?.source ?? ""
  )?.[1];
}

/** The role prompt of `c`'s Legion context and its record: the parts Compose names in the first
 *  plugin repository that holds PROMPTS_GO, joined as `ComposePrompt` joins them. Each repository
 *  is read at the commit its daemon image was built from where `c`'s evidence names one
 *  (`daemonCommit`), since the daemon composes the role prompt, else at the profile's commit of
 *  it; the record says which (`commitOf`). */
async function rolePrompt(
  c: ContextCase,
  role: { readonly role: string; readonly isRoot: boolean },
  builds: Builds,
  options: ContextOptions
): Promise<{ text: string; record: ContextRecord["rolePrompt"] }> {
  if (c.profile.appendSystemPrompt !== undefined) {
    return { text: c.profile.appendSystemPrompt, record: "case" };
  }
  const { source } = options;
  const repositories = [
    ...new Map(
      c.profile.plugins.map((p) => {
        const daemon = daemonCommit(c, p.repo);
        const at =
          daemon === undefined
            ? { repo: p.repo, commit: p.commit, commitOf: "plugin" as const }
            : { repo: p.repo, commit: daemon, commitOf: "daemon image" as const };
        return [`${at.repo}@${at.commit}`, at];
      })
    ).values(),
  ];
  let known = promptsBlobs.get(source);
  if (known === undefined) {
    known = new Map();
    promptsBlobs.set(source, known);
  }
  for (const { repo, commit, commitOf } of repositories) {
    let promptsBlob = known.get(`${repo}@${commit}`);
    if (promptsBlob === undefined) {
      try {
        promptsBlob = gitBlobId(await source.file(repo, commit, PROMPTS_GO));
      } catch (error) {
        // A repository that holds no PROMPTS_GO is passed over; a read that failed stops the case.
        if (error instanceof NoSuchFile) continue;
        throw error;
      }
      known.set(`${repo}@${commit}`, promptsBlob);
    }
    const key = `${promptsBlob}-${role.role}${role.isRoot ? "-root" : ""}`;
    const where = await builds.entry("compose", key, async (dest) => {
      const tree = await checkout(builds, source, repo, commit);
      const parts = await composeParts(
        tree,
        role.role,
        role.isRoot,
        path.join(options.cacheDir, "tmp")
      );
      writeFileSync(dest, `${JSON.stringify(parts)}\n`);
    });
    const parts = JSON.parse(readFileSync(where, "utf8")) as string[];
    let text = "";
    for (const part of parts) text += (await fileText(builds, source, repo, commit, part)).text;
    return {
      text: text.replace(/\n+$/, ""),
      record: {
        repo,
        commit,
        commitOf,
        role: role.role,
        isRoot: role.isRoot,
        promptsBlob,
        key,
        parts,
      },
    };
  }
  throw new Error(
    `case ${c.id}: its Legion context needs a role prompt, and no plugin repository holds ${PROMPTS_GO}`
  );
}

/** The retries of `after` less those of `before`, two `RepoSource.reads` records: each reason with
 *  at least one. */
function retriesSince(
  before: Readonly<Record<string, number>>,
  after: Readonly<Record<string, number>>
): Record<string, number> {
  return Object.fromEntries(
    Object.entries(after)
      .map(([reason, count]): [string, number] => [reason, count - (before[reason] ?? 0)])
      .filter(([, count]) => count > 0)
  );
}

/** A plugin of a profile. */
type ProfilePlugin = Case["profile"]["plugins"][number];

/** The checkout of the repository of `c`'s profile plugin `plugin` at its commit, through the
 *  cache. A read that failed stops the case, naming the plugin, as a failed role-prompt read does,
 *  so every arm of a case has the same plugins; an IncompleteCacheEntry stops it as it is. */
async function pluginCheckout(
  c: ContextCase,
  plugin: ProfilePlugin,
  builds: Builds,
  source: RepoSource
): Promise<string> {
  try {
    return await checkout(builds, source, plugin.repo, plugin.commit);
  } catch (error) {
    if (error instanceof IncompleteCacheEntry) throw error;
    throw new Error(
      `case ${c.id}: the repository of the plugin ${plugin.name}, ${plugin.repo} at ${plugin.commit}, could not be read: ${messageFor(error)}`
    );
  }
}

/** Where a dev box's installer scripts are in its repository (build.ts's DEVBOX_INSTALLERS_RULE:
 *  `installers/*.sh`). */
const INSTALLERS = "installers";
/** The two agent directories under the home that an installer can make, as it names the place:
 *  under `$HOME`, `${HOME}` or `~`. `user` is Oh My Pi's user agents directory, `.omp/agent/agents`,
 *  which it reads after the project's and before any plugin's (packages/coding-agent/src/task/
 *  discovery.ts, lines 93-120 at the pin), at each subagent's start (task/structured-subagent.ts,
 *  lines 538-541). `home` is `.omp/agents` in the home itself: a project's agents directory
 *  (PROJECT_AGENTS) at the home's level, which Oh My Pi's walk from the working directory up
 *  reaches when the working directory is under the home (config.ts, lines 218-246). */
const HOME_AGENTS = {
  user: /^(?:\$\{?HOME\}?|~)\/\.omp\/agent\/agents\/?$/,
  home: /^(?:\$\{?HOME\}?|~)\/\.omp\/agents\/?$/,
} as const;
type HomeAgents = keyof typeof HOME_AGENTS;
/** Each of HOME_AGENTS as the record names it: what it holds, and what a link makes of it. */
const HOME_AGENTS_WORDS: Readonly<Record<HomeAgents, { what: string; as: string }>> = {
  user: { what: "the user agents of a dev box", as: "Oh My Pi's user agents directory" },
  home: {
    what: "the project agents of a dev box's home",
    as: "~/.omp/agents, which Oh My Pi reads as a project's agents",
  },
};
/** A word of an installer line that names `~/.omp/agents` or a path under it. */
const HOME_AGENTS_NAMED = /^(?:\$\{?HOME\}?|~)\/\.omp\/agents(?:\/|$)/;
/** An installer line that links `<from>` at `<to>`: `ensure_link <from> <to>` (the helper a
 *  dotfiles repository's installers/lib.sh defines), or `ln` and its flags. */
const LINK_LINE = /^\s*(?:ensure_link|ln((?:\s+-[A-Za-z-]+)+))\s+(\S+)\s+(\S+)\s*(?:#.*)?$/;

/** An installer's path word with its quotes taken off and each variable the script assigns put
 *  in, five deep at most: the text it comes to, and the path in the repository it names
 *  (`inRepository`) when that text opens with a variable the script does not assign or one that
 *  computes the script's own directory, either taken as the repository's root as copies.ts's
 *  `shellPath` takes it; HOME names the home, no repository's path. */
function installerPath(
  word: string,
  vars: ReadonlyMap<string, string>,
  depth = 0
): { readonly text: string; readonly inRepository?: string } {
  const text = word.replace(/["']/g, "");
  const lead = /^\$\{?([A-Za-z_]\w*)\}?(.*)$/.exec(text);
  if (lead === null) return { text };
  const [, name = "", rest = ""] = lead;
  const value = vars.get(name);
  if (value !== undefined && depth < 5 && !/dirname|BASH_SOURCE|pwd/.test(value)) {
    return installerPath(value + rest, vars, depth + 1);
  }
  return name === "HOME" ? { text } : { text, inRepository: joinPath(rest.replace(/^\/+/, "")) };
}

/** What an installer script makes of the agent directories under the home (HOME_AGENTS): for
 *  each, the directory of its repository that the first line linking one there links, and that
 *  line (from 1); and each other line one of whose words, its variables put in, names
 *  `~/.omp/agents` or a path under it (`homeNamed`: a copy into it, a `mkdir` of it), which the
 *  runner does not read. A comment line is none, and a link of the repository's root is none: the
 *  root holds no agents directory of its own. */
function installerAgents(text: string): {
  readonly links: Partial<Record<HomeAgents, { readonly dir: string; readonly line: number }>>;
  readonly homeNamed: readonly number[];
} {
  const vars = shellVariables(text);
  const links: Partial<Record<HomeAgents, { dir: string; line: number }>> = {};
  const homeNamed: number[] = [];
  for (const [index, line] of text.split("\n").entries()) {
    if (line.trim().startsWith("#")) continue;
    const link = LINK_LINE.exec(line);
    const flags = link?.[1]?.trim().split(/\s+/);
    const symbolic =
      flags === undefined ||
      flags.some((flag) => flag === "--symbolic" || /^-[A-Za-z]*s/.test(flag));
    const from =
      link !== null && symbolic ? installerPath(link[2] ?? "", vars).inRepository : undefined;
    const to = link === null ? "" : installerPath(link[3] ?? "", vars).text;
    const linked = (["user", "home"] as const).filter((kind) => from && HOME_AGENTS[kind].test(to));
    for (const kind of linked) links[kind] ??= { dir: from ?? "", line: index + 1 };
    const named = line
      .trim()
      .split(/\s+/)
      .some((word) =>
        HOME_AGENTS_NAMED.test(installerPath(word.split("=").at(-1) ?? "", vars).text)
      );
    if (named && !linked.includes("home")) homeNamed.push(index + 1);
  }
  return { links, homeNamed };
}

/** The directory `dir` of `repo` at `commit`, through the cache (`dirs/<key>`): an empty directory
 *  when no try can read a file under it (UnreadableDirectory), as Oh My Pi finds no file there.
 *  Another failed read throws. */
function repoDirectory(
  builds: Builds,
  source: RepoSource,
  repo: string,
  commit: string,
  dir: string
): Promise<string> {
  return builds.entry("dir", cacheKey(repo, commit, dir), async (dest) => {
    const release = await source.prefetch?.(repo, commit, [dir]);
    try {
      await source.tree(repo, commit, dir, dest);
    } catch (error) {
      if (!(error instanceof UnreadableDirectory)) throw error;
      rmSync(dest, { recursive: true, force: true });
      mkdirSync(dest, { recursive: true });
    } finally {
      release?.();
    }
  });
}

/** A directory of agent definitions Oh My Pi reads for a subagent: which of its agent
 *  directories it is (`source`), where it is written here (`dir`), the repository, commit and
 *  path it is read from, and what links it where Oh My Pi reads it (`linkedBy`; AgentDefinition). */
interface AgentDir {
  readonly source: AgentDefinition["source"];
  readonly dir: string;
  readonly repo: string;
  readonly commit: string;
  readonly path: string;
  readonly linkedBy?: string;
}

/** A directory of a repository that an installer line links under the home: the repository, the
 *  commit it is read at, the directory's path in it, and the line (`<script>:<line>`). */
interface AgentLink {
  readonly repo: string;
  readonly commit: string;
  readonly path: string;
  readonly linkedBy: string;
}

/** Oh My Pi's project agents directory under one level of a session's working directory: the
 *  `agents` of its config directory `.omp` (task/discovery.ts, lines 34 and 100-109 at the pin,
 *  which keep only that base of config.ts's `.omp`, `.claude`, `.codex` and `.gemini`: lines
 *  11-16 and 90-93; `.claude/agents` and the like are skipped by design, lines 13-16). */
const PROJECT_AGENTS = ".omp/agents";
/** The most symbolic links in a row `followLinks` follows; past them the path names nothing, as
 *  Oh My Pi's stat fails on a loop. */
const LINK_HOPS = 8;

/** Where a path of a repository leads (`followLinks`): its path in the repository and each link
 *  followed to reach it, or the link whose target leaves the repository. */
type LinkedPath =
  | { readonly path: string; readonly via: readonly string[] }
  | { readonly outside: { readonly link: string; readonly target: string } };

/** The path in its repository that the symbolic link `link` (a path in the repository) with the
 *  target `target` leads to, taken from the link's directory; undefined when the target is not in
 *  the repository (an absolute path, or one above the repository's root). */
export function linkTargetPath(link: string, target: string): string | undefined {
  const resolved = path.posix.normalize(path.posix.join(path.posix.dirname(link), target));
  return path.posix.isAbsolute(target) || resolved === ".." || resolved.startsWith("../")
    ? undefined
    : resolved;
}

/** Where `file` of `repo` at `commit` leads, as Oh My Pi's stat reaches it: while the repository
 *  holds the path as a symbolic link (`RepoSource.link`), the link's target, taken from the
 *  link's directory, LINK_HOPS in a row at most, with each link followed (`<link> -> <target>`);
 *  or the first such link whose target is not in the repository (`linkTargetPath`), which no read
 *  of the repository gives. */
async function followLinks(
  source: RepoSource,
  repo: string,
  commit: string,
  file: string
): Promise<LinkedPath> {
  let at = file;
  const via: string[] = [];
  for (let hops = 0; hops < LINK_HOPS; hops++) {
    const target = await source.link?.(repo, commit, at);
    if (target === undefined) break;
    const resolved = linkTargetPath(at, target);
    if (resolved === undefined) return { outside: { link: at, target } };
    via.push(`${at} -> ${target}`);
    at = resolved === "." ? "" : resolved;
  }
  return { path: at, via };
}

/** Where `<level>/.omp/agents` of `repo` at `commit` is, as Oh My Pi's stat reaches it: each
 *  symbolic link the repository holds at `<level>/.omp` or at its `agents` followed to its target
 *  in the repository (`followLinks`); or the first such link whose target is not in the
 *  repository, which the context cannot read. */
async function projectAgentsPath(
  source: RepoSource,
  repo: string,
  commit: string,
  level: string
): Promise<LinkedPath> {
  let at = level;
  const via: string[] = [];
  for (const name of [".omp", "agents"]) {
    const reached = await followLinks(source, repo, commit, path.posix.join(at, name));
    if ("outside" in reached) return reached;
    via.push(...reached.via);
    at = reached.path;
  }
  return { path: at, via };
}

/** The directory of `skill`'s repository that its files are read from: its own path, or, where
 *  the repository holds that path as a symbolic link, the directory the link names in the
 *  repository (`followLinks`), with each link followed (`via`; the root as `.`). build.ts names a
 *  project skill held as a link by the link's path, where Oh My Pi lists it: Oh My Pi's scan of a
 *  skills directory takes a link as a skill directory and reads what it names
 *  (discovery/helpers.ts, lines 579-580 and 594-596, at the pinned tag). Throws an
 *  UnreadableDirectory naming the case, the link and its target when a link leads out of the
 *  repository, which no read of the repository gives; another error when a read fails. */
async function skillReadDir(
  c: ContextCase,
  source: RepoSource,
  skill: ProfileSkill
): Promise<Extract<LinkedPath, { path: string }>> {
  const reached = await followLinks(source, skill.repo, skill.commit, skill.path);
  if (!("outside" in reached)) {
    return { path: reached.path === "" ? "." : reached.path, via: reached.via };
  }
  const { link, target } = reached.outside;
  throw new UnreadableDirectory(
    skill.repo,
    skill.commit,
    link,
    `case ${c.id}: the profile's skill ${listedName(skill)} is ${skill.path} of ${skill.repo} at ${skill.commit}, which holds ${link} as a symbolic link to ${target}, outside the repository, so no read of the repository gives the skill`
  );
}

/** Whether `dir` holds an entry: a link that names nothing holds none. */
function holdsEntries(dir: string): boolean {
  try {
    return readdirSync(dir).length > 0;
  } catch {
    return false;
  }
}

/**
 * The project's agents of `c`'s subagent session in its checkout, as Oh My Pi finds them
 * (task/discovery.ts, lines 100-109, and config.ts's `findAllNearestProjectConfigDirs`, lines
 * 218-246, at the pin): the nearest `.omp/agents` directory from the session's working directory
 * up through each parent, the profile's `workingDirectory` in its session repository (the
 * checkout's root when it names none) and each directory above it up to the checkout's root, read
 * from the session repository at its commit, a symbolic link at `.omp` or `agents` followed to
 * its target in the repository (`projectAgentsPath`; the record's `linkedBy` names each link).
 * Only that one directory is read, as Oh My Pi reads only the nearest; git holds no empty
 * directory, so a level whose `.omp/agents` holds no file, or links to nothing in the
 * repository, has none. None when the profile names no session repository, or no level of its
 * checkout holds one: Oh My Pi walks on above the checkout to the filesystem's root, where the
 * dev box's home may hold one (`devboxAgents`), and the levels between the checkout and the home
 * are no repository's. None, with why (`stopped`), at a level whose link leaves the repository:
 * Oh My Pi reads its target there when it is a directory, and reads no farther one then, so the
 * nearest is not known. Throws naming the case when a read fails, and an UnreadableDirectory as
 * it is.
 */
async function projectAgents(
  c: ContextCase,
  builds: Builds,
  source: RepoSource
): Promise<{ readonly dir?: AgentDir; readonly stopped?: string }> {
  const repository = c.profile.sessionRepository;
  if (repository === undefined) return {};
  const { repo, commit } = repository;
  // The working directory, then each parent of it, up to the checkout's root ("").
  const levels: string[] = [];
  for (let at = c.profile.workingDirectory ?? ""; ; ) {
    levels.push(at);
    if (at === "") break;
    const up = path.posix.dirname(at);
    at = up === "." ? "" : up;
  }
  for (const level of levels) {
    const where = path.posix.join(level, PROJECT_AGENTS);
    let reached: LinkedPath;
    let dir: string | undefined;
    try {
      reached = await projectAgentsPath(source, repo, commit, level);
      if (!("outside" in reached)) {
        const at = reached.path === "" ? "." : reached.path;
        dir = await repoDirectory(builds, source, repo, commit, at);
      }
    } catch (error) {
      if (error instanceof UnreadableDirectory) throw error;
      throw new Error(
        `case ${c.id}: ${where} of the session repository ${repo} at ${commit}, where Oh My Pi looks for the project's agents, could not be read: ${messageFor(error)}`
      );
    }
    if ("outside" in reached) {
      return {
        stopped: `the project agents: the session repository ${repo} at ${commit} holds ${reached.outside.link} as a link to ${reached.outside.target}, outside the repository, so the context reads neither its target, which Oh My Pi reads as the project's agents when it is a directory, nor any .omp/agents farther from the working directory`,
      };
    }
    if (dir !== undefined && holdsEntries(dir)) {
      return {
        dir: {
          source: "project",
          dir,
          repo,
          commit,
          path: reached.path,
          ...(reached.via.length === 0 ? {} : { linkedBy: reached.via.join(", ") }),
        },
      };
    }
  }
  return {};
}

/** The commit at which a subagent's session read the pool directories of `repo`, the last
 *  commit at or before its start, as the evidence item READ_TIMES_ITEM of `repo` that build.ts
 *  writes for a pool's host names it: the commit after "its pool directories at" in its text.
 *  Undefined when the case has no such item. */
function poolCommit(c: ContextCase, repo: string): string | undefined {
  const item = c.evidence?.find(
    (each) => each.item.toLowerCase() === `${READ_TIMES_ITEM}${repo}`.toLowerCase()
  );
  return /\bits pool directories at ([0-9a-f]{7,40})\b/.exec(item?.source ?? "")?.[1];
}

/** Whether the session's working directory is under its home, as far as the case says: the
 *  absolute working directory build.ts writes into its `session repository <repo>` or `no session
 *  repository` evidence item (the path after "working directory "), under `/home/<user>` or
 *  `/root`; undefined when no such item names one. */
function underHome(c: ContextCase): boolean | undefined {
  for (const each of c.evidence ?? []) {
    if (
      !each.item.startsWith(SESSION_REPOSITORY_ITEM) &&
      each.item !== NO_SESSION_REPOSITORY_ITEM
    ) {
      continue;
    }
    const cwd = /\bworking directory (\/[^\s,;]+)/.exec(each.source)?.[1];
    if (cwd !== undefined) return /^(?:\/home\/[^/]+|\/root)(?:\/|$)/.test(cwd);
  }
  return undefined;
}

/** What the installer scripts of a repository at a commit make of the agent directories under
 *  the home (`installersAt`): for each, the first link of a directory of the repository there,
 *  with the line (`<script>:<line>`) that makes it; and each other line that names
 *  `~/.omp/agents`. */
interface InstallerLinks {
  readonly links: Partial<Record<HomeAgents, { readonly path: string; readonly linkedBy: string }>>;
  readonly homeNamed: readonly string[];
}

/** What the installer scripts of `repo` at `commit` make of the agent directories under the home
 *  (InstallerLinks; `installerAgents`, the scripts in name order). Throws naming the case when the
 *  installers cannot be read. */
async function installersAt(
  c: ContextCase,
  builds: Builds,
  source: RepoSource,
  repo: string,
  commit: string
): Promise<InstallerLinks> {
  let installers: string;
  try {
    installers = await repoDirectory(builds, source, repo, commit, INSTALLERS);
  } catch (error) {
    throw new Error(
      `case ${c.id}: the installers of ${repo} at ${commit} could not be read: ${messageFor(error)}`
    );
  }
  const scripts = readdirSync(installers, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".sh"))
    .map((entry) => entry.name)
    .sort((a, b) => a.localeCompare(b));
  const links: Partial<Record<HomeAgents, { path: string; linkedBy: string }>> = {};
  const homeNamed: string[] = [];
  for (const script of scripts) {
    const read = installerAgents(readFileSync(path.join(installers, script), "utf8"));
    for (const kind of ["user", "home"] as const) {
      const link = read.links[kind];
      if (link === undefined) continue;
      links[kind] ??= { path: link.dir, linkedBy: `${INSTALLERS}/${script}:${link.line}` };
    }
    homeNamed.push(...read.homeNamed.map((line) => `${INSTALLERS}/${script}:${line}`));
  }
  return { links, homeNamed };
}

/**
 * The agent directories a dev box's installers link under its home (HOME_AGENTS), for `c`'s
 * profile: for each, the directory a repository of the profile links there from one of its
 * installer scripts (`installers/*.sh`; a dotfiles repository's installers/omp.sh links its
 * `omp/agents` as the user agents). Oh My Pi reads them at each subagent's start, so the
 * installers are read at the commit of the subagent's start: the one the case's evidence names
 * for the repository's pool directories (`poolCommit`), else the one commit the profile holds
 * the repository at. `~/.omp/agents` is one only when the session's working directory is under
 * the home (`underHome`), where Oh My Pi's walk reaches it; elsewhere it is none, and nothing is
 * recorded of it. None for a Legion store's session (arms.ts's `legionStore`), which runs no dev
 * box's installers, or for a directory no repository links. None, with why (`leftOut`, by
 * directory), when the profile holds a linking repository at more than one commit and the
 * evidence names neither, when more than one repository links it (a dev box runs one
 * repository's installers), and for `~/.omp/agents` when an installer line names it other than
 * as such a link, or when the case does not say where the working directory is. Throws naming
 * the repository when its installers cannot be read.
 */
async function devboxAgents(
  c: ContextCase,
  builds: Builds,
  source: RepoSource
): Promise<{
  readonly links: Partial<Record<HomeAgents, AgentLink>>;
  readonly leftOut: Readonly<Record<HomeAgents, readonly string[]>>;
}> {
  const leftOut: Record<HomeAgents, string[]> = { user: [], home: [] };
  if (legionStore(c) !== undefined) return { links: {}, leftOut };
  // Each repository of the profile, with each commit the profile holds it at; not a load test's
  // own entry (LOCAL_SKILL_REPO), which names no repository.
  const held = new Map<string, string[]>();
  for (const each of [...c.profile.skills, ...c.profile.plugins, ...c.profile.contextFiles]) {
    if (each.repo === LOCAL_SKILL_REPO) continue;
    const commits = held.get(each.repo) ?? [];
    if (!commits.includes(each.commit)) commits.push(each.commit);
    held.set(each.repo, commits);
  }
  const found: Record<HomeAgents, AgentLink[]> = { user: [], home: [] };
  const named: string[] = [];
  for (const [repo, commits] of held) {
    const atStart = poolCommit(c, repo);
    const reads: { commit: string; read: InstallerLinks }[] = [];
    for (const commit of atStart === undefined ? commits : [atStart]) {
      reads.push({ commit, read: await installersAt(c, builds, source, repo, commit) });
    }
    const ambiguous = atStart === undefined && commits.length > 1;
    for (const kind of ["user", "home"] as const) {
      const links = reads.flatMap(({ commit, read }) => {
        const link = read.links[kind];
        return link === undefined ? [] : [{ repo, commit, ...link }];
      });
      if (!ambiguous || links.length === 0) {
        found[kind].push(...links);
        continue;
      }
      const { what, as } = HOME_AGENTS_WORDS[kind];
      leftOut[kind].push(
        `${what}: the profile holds ${repo} at ${commits.join(" and ")}, its ${links.map((link) => `${link.linkedBy} at ${link.commit}`).join(" and ")} ${links.length === 1 ? "links" : "each link"} a directory as ${as}, and no evidence item "${READ_TIMES_ITEM}${repo}" names the commit of the subagent's start, when Oh My Pi reads that directory, so the context reads none`
      );
    }
    for (const { commit, read } of reads) {
      named.push(...read.homeNamed.map((line) => `${repo}'s ${line} at ${commit}`));
    }
  }
  for (const kind of ["user", "home"] as const) {
    if (found[kind].length < 2) continue;
    const { what, as } = HOME_AGENTS_WORDS[kind];
    leftOut[kind].push(
      `${what}: ${found[kind].map((link) => `${link.repo}'s ${link.linkedBy}`).join(" and ")} each link a directory as ${as}, and a dev box runs one repository's installers, so the context reads none of them`
    );
  }
  if (named.length > 0) {
    leftOut.home.push(
      `${HOME_AGENTS_WORDS.home.what}: ${named.join(" and ")} ${named.length === 1 ? "names" : "each name"} ~/.omp/agents in a line the runner does not read as a link of a directory of the repository (ensure_link or ln -s), so the context reads none of it`
    );
  }
  const [user] = found.user;
  const [home] = found.home;
  const under = underHome(c);
  if (under === false) leftOut.home = [];
  else if (home !== undefined && leftOut.home.length === 0 && under === undefined) {
    leftOut.home.push(
      `${HOME_AGENTS_WORDS.home.what}: ${home.repo}'s ${home.linkedBy} links ${home.path} as ~/.omp/agents, which Oh My Pi reads as a project's agents when the session's working directory is under the home, and no evidence item names the working directory, so the context reads none`
    );
  }
  return {
    links: {
      ...(user !== undefined && leftOut.user.length === 0 ? { user } : {}),
      ...(home !== undefined && leftOut.home.length === 0 && under === true ? { home } : {}),
    },
    leftOut,
  };
}

/** The directory `link` names, read through the cache, as the agent directory `which` of the
 *  home: Oh My Pi's user agents for `user`, a project's agents for `home` (`~/.omp/agents`), the
 *  record naming it in HOME_AGENTS_WORDS' words. Throws naming the case when it cannot be read. */
async function linkedAgents(
  c: ContextCase,
  builds: Builds,
  source: RepoSource,
  link: AgentLink,
  which: HomeAgents
): Promise<AgentDir> {
  try {
    return {
      ...link,
      source: which === "user" ? "user" : "project",
      dir: await repoDirectory(builds, source, link.repo, link.commit, link.path),
    };
  } catch (error) {
    throw new Error(
      `case ${c.id}: ${link.path} of ${link.repo} at ${link.commit}, which ${link.linkedBy} links as ${HOME_AGENTS_WORDS[which].as}, could not be read: ${messageFor(error)}`
    );
  }
}

/** The directories of agent definitions `c`'s subagent context reads, in Oh My Pi's order
 *  (task/discovery.ts, lines 100-120 at the pin: the nearest project's, the user's, then each
 *  extension package's): the project's agents, those of the checkout (`projectAgents`), else the
 *  dev box's `~/.omp/agents`, which is farther from the working directory than every level of
 *  the checkout (`devboxAgents`); the dev box's user agents; then the `agents/` of each of
 *  `plugins` (the profile's, each with its checkout). With what of them the context leaves out:
 *  of `~/.omp/agents` only where no level of the checkout holds the project's. None for another
 *  context. */
async function agentDirs(
  c: ContextCase,
  plugins: readonly { readonly plugin: ProfilePlugin; readonly tree: string }[],
  builds: Builds,
  source: RepoSource
): Promise<{ readonly dirs: readonly AgentDir[]; readonly leftOut: readonly string[] }> {
  if (!c.context.startsWith("subagent:")) return { dirs: [], leftOut: [] };
  const project = await projectAgents(c, builds, source);
  const devbox = await devboxAgents(c, builds, source);
  const inCheckout = project.dir !== undefined || project.stopped !== undefined;
  const { home, user } = devbox.links;
  const nearest =
    project.dir ??
    (inCheckout || home === undefined
      ? undefined
      : await linkedAgents(c, builds, source, home, "home"));
  return {
    dirs: [
      ...(nearest === undefined ? [] : [nearest]),
      ...(user === undefined ? [] : [await linkedAgents(c, builds, source, user, "user")]),
      ...plugins.map(({ plugin, tree }) => ({
        source: "plugin" as const,
        dir: path.join(tree, plugin.path, "agents"),
        repo: plugin.repo,
        commit: plugin.commit,
        path: path.posix.join(plugin.path, "agents"),
      })),
    ],
    leftOut: [
      ...(project.stopped === undefined ? [] : [project.stopped]),
      ...devbox.leftOut.user,
      ...(inCheckout ? [] : devbox.leftOut.home),
    ],
  };
}

/**
 * The agent definition a subagent context (`subagent:<agent>`) runs, as Oh My Pi finds it
 * (task/discovery.ts at the pin, lines 53-74 and 157-162): the first `*.md` file, in name order,
 * of the first of `dirs` (`agentDirs`, in Oh My Pi's order) whose frontmatter `name` is the agent
 * and that gives a `description` (discovery/helpers.ts's `parseAgentFields`), with the skills its
 * `autoloadSkills` names (a list, or names split at commas; each trimmed). A file that cannot be
 * read (a link that names nothing) is passed over, as Oh My Pi drops it. Null for another
 * context, or for an agent no directory defines: one of Oh My Pi's own (none of which autoloads a
 * skill at the pin).
 */
function agentIn(context: string, dirs: readonly AgentDir[]): AgentDefinition | null {
  const agent = /^subagent:(.+)$/.exec(context)?.[1];
  if (agent === undefined) return null;
  for (const at of dirs) {
    if (!existsSync(at.dir)) continue;
    const files = readdirSync(at.dir, { withFileTypes: true })
      .filter((entry) => (entry.isFile() || entry.isSymbolicLink()) && entry.name.endsWith(".md"))
      .map((entry) => entry.name)
      .sort((a, b) => a.localeCompare(b));
    for (const file of files) {
      let text: string;
      try {
        text = readFileSync(path.join(at.dir, file), "utf8");
      } catch {
        continue;
      }
      const frontmatter = readFrontmatter(text);
      if (frontmatter.name !== agent || typeof frontmatter.description !== "string") continue;
      const named = frontmatter.autoloadSkills;
      const listed = typeof named === "string" ? named.split(",") : strings(named);
      return {
        name: agent,
        source: at.source,
        repo: at.repo,
        commit: at.commit,
        path: path.posix.join(at.path, file),
        ...(at.linkedBy === undefined ? {} : { linkedBy: at.linkedBy }),
        autoloadSkills: listed.map((each) => each.trim()).filter((each) => each !== ""),
      };
    }
  }
  return null;
}

/** The agent definition `c`'s subagent context runs (`agentIn`), read from the project's agents,
 *  the dev box's user agents and its profile's plugins at their commits through the cache, as
 *  buildContext reads them; null for a context that is no subagent's. Throws as buildContext does
 *  when a repository it reads cannot be read. */
export async function subagentDefinition(
  c: ContextCase,
  options: Pick<ContextOptions, "source" | "cacheDir">
): Promise<AgentDefinition | null> {
  if (!c.context.startsWith("subagent:")) return null;
  const builds = new Builds(options.cacheDir);
  const trees: { readonly plugin: ProfilePlugin; readonly tree: string }[] = [];
  for (const plugin of c.profile.plugins) {
    trees.push({ plugin, tree: await pluginCheckout(c, plugin, builds, options.source) });
  }
  return agentIn(c.context, (await agentDirs(c, trees, builds, options.source)).dirs);
}

/** The record of a repository skill a context installs, as its ContextRecord's `skills` names it:
 *  the name the profile lists it under, and its repository, commit, path and cache key. */
type CachedSkillRecord = {
  readonly name: string;
  readonly from: Exclude<ContextRecord["skills"][number]["from"], string>;
};

/** A skill of an arm's profile as its context installs it, under the name the profile lists it
 *  under (`listed`, cases.ts's `listedName`): from the arm's directory (`armDir`), or from its
 *  repository at its commit, kept in the cache under the key of its repository, commit and path
 *  (`record`, as the context's record names it). */
type ProfileCopy = { readonly skill: ProfileSkill; readonly listed: string } & (
  | { readonly armDir: string; readonly record?: never }
  | { readonly armDir?: never; readonly record: CachedSkillRecord }
);

/**
 * Each skill of `arm.profileSkills`, in order, as its context installs it (ProfileCopy): each
 * copy of the case's skill's source, under its own name or a namespace, from the arm's directory
 * (arms.ts's `installedFromArm`), and every other skill from its repository. An arm with no skill
 * directory (`none`) installs no copy from it: a copy it lists under the case's skill's name is
 * another source's (arms.ts's `Arm.relisted`). It reads the arm alone: no repository, and no
 * cache. `buildContext` installs by it, and capture.ts's `restoreCopies` compares a saved
 * capture's record with its repository records.
 */
export function profileCopies(
  arm: Pick<Arm, "skill" | "skillDir" | "profileSkills">
): ProfileCopy[] {
  const fromArm = installedFromArm(arm);
  return arm.profileSkills.map((skill): ProfileCopy => {
    const listed = listedName(skill);
    const armDir = fromArm(skill);
    if (armDir !== undefined) return { skill, listed, armDir };
    const key = cacheKey(skill.repo, skill.commit, skill.path);
    const from = { repo: skill.repo, commit: skill.commit, path: skill.path, key };
    return { skill, listed, record: { name: listed, from } };
  });
}

/** The records of the repository copies of `arm`'s profile (`profileCopies`), in order: the
 *  skills its context installs from their repositories, as its ContextRecord's `skills` names
 *  them. capture.ts's `restoreCopies` compares a saved capture's record with them, and cli.ts
 *  answers a read of another skill from their cache entries where an arm's capture is not saved. */
export function profileRecords(
  arm: Pick<Arm, "skill" | "skillDir" | "profileSkills">
): CachedSkillRecord[] {
  return profileCopies(arm).flatMap(({ record }) => (record === undefined ? [] : [record]));
}

/** Writes the context of `c`'s arm `arm` under `options.root`, replacing one written there. */
export async function buildContext(
  c: ContextCase,
  arm: Arm,
  options: ContextOptions
): Promise<CaseContext> {
  const { source, root } = options;
  const retriedBefore = source.reads?.().retries ?? {};
  const builds = new Builds(options.cacheDir);
  const skillsDir = path.join(root, "skills");
  // A namespaced copy (ProfileSkill's `namespace`) goes in `namespaced/<namespace>/skills/<name>`,
  // a skills directory the capture gives Oh My Pi after `skills/`, so Oh My Pi admits the bare copy
  // first and lists this one as `<namespace>/<name>`: skillNamespace (extensibility/skills.ts,
  // lines 96-115) takes the directory that holds `skills/` for a configured directory's skill.
  const namespacedDir = path.join(root, "namespaced");
  const namespacedDirs: string[] = [];
  const pluginsDir = path.join(root, "plugins");
  const checkoutDir = path.join(root, "work");
  const workspace =
    c.profile.workingDirectory === undefined
      ? checkoutDir
      : path.join(checkoutDir, c.profile.workingDirectory);
  const promptFile = path.join(root, "system-prompt.md");
  const claudeConfigDir = path.join(root, "claude-config");
  for (const owned of [
    skillsDir,
    namespacedDir,
    pluginsDir,
    checkoutDir,
    promptFile,
    claudeConfigDir,
  ]) {
    rmSync(owned, { recursive: true, force: true });
  }
  // A dev box's user-level context file (`user: true`) is the one its session's host read
  // (build.ts), and Oh My Pi reads one file at user level.
  const userFiles = c.profile.contextFiles.filter((file) => file.user === true).length;
  if (userFiles > 1) {
    throw new Error(
      `case ${c.id}: its profile holds ${userFiles} user-level context files, and Oh My Pi reads one`
    );
  }
  mkdirSync(skillsDir, { recursive: true });
  mkdirSync(workspace, { recursive: true });

  const copies = profileCopies(arm);
  // The repository skills the cache lacks, read ahead per repository and commit: one tree and a
  // GraphQL batch per BLOB_BATCH blobs, where each file would take a call of its own. Each is read
  // at the directory `skillReadDir` gives, kept by its cache key: its own, or the one its link
  // names.
  const ahead = new Map<string, { repo: string; commit: string; dirs: string[] }>();
  const readDirs = new Map<string, Extract<LinkedPath, { path: string }>>();
  for (const { skill, record } of copies) {
    if (
      record === undefined ||
      existsSync(path.join(options.cacheDir, "skills", record.from.key))
    ) {
      continue;
    }
    const read = await skillReadDir(c, source, skill);
    readDirs.set(record.from.key, read);
    const id = `${skill.repo}@${skill.commit}`;
    const group = ahead.get(id) ?? { repo: skill.repo, commit: skill.commit, dirs: [] };
    group.dirs.push(read.path);
    ahead.set(id, group);
  }
  // What the read ahead holds is released once the skills are installed, or once their step
  // fails: a file no `tree` wrote (a step that threw first, or a skill whose cache entry another
  // capture made first) holds nothing after it.
  const releases: (() => void)[] = [];
  const skills: InstalledSkill[] = [];
  const skillRecords: ContextRecord["skills"][number][] = [];
  try {
    for (const { repo, commit, dirs } of ahead.values()) {
      const release = await source.prefetch?.(repo, commit, dirs);
      if (release !== undefined) releases.push(release);
    }
    for (const copy of copies) {
      const { skill, listed } = copy;
      const dir =
        skill.namespace === undefined
          ? path.join(skillsDir, skill.name)
          : path.join(namespacedDir, skill.namespace, "skills", skill.name);
      if (existsSync(dir)) {
        throw new Error(`case ${c.id}: the profile installs ${listed} twice`);
      }
      if (skill.namespace !== undefined) {
        const parent = path.dirname(dir);
        mkdirSync(parent, { recursive: true });
        if (!namespacedDirs.includes(parent)) namespacedDirs.push(parent);
      }
      // Where the copy comes from, as the refusal below names it.
      let from: string;
      if (copy.armDir !== undefined) {
        cpSync(copy.armDir, dir, { recursive: true, verbatimSymlinks: true });
        skillRecords.push({ name: listed, from: "arm" });
        from = `the arm's text, ${copy.armDir}`;
      } else {
        const { record } = copy;
        const { key } = record.from;
        const cached = await builds.entry("skill", key, async (dest) => {
          // A skill whose cache entry was there at the read ahead is not in `readDirs`.
          const read = readDirs.get(key) ?? (await skillReadDir(c, source, skill));
          try {
            await source.tree(skill.repo, skill.commit, read.path, dest);
          } catch (error) {
            if (!(error instanceof UnreadableDirectory) || read.via.length === 0) throw error;
            throw new UnreadableDirectory(
              skill.repo,
              skill.commit,
              read.path,
              `case ${c.id}: the profile's skill ${listed} is ${skill.path} of ${skill.repo} at ${skill.commit}, which holds it as a symbolic link (${read.via.join(", ")}) to no directory of the repository: ${error.message}`
            );
          }
        });
        cpSync(cached, dir, { recursive: true, verbatimSymlinks: true });
        skillRecords.push(record);
        from = `${skill.path} of ${skill.repo} at ${skill.commit}`;
      }
      // Oh My Pi names the copy by its frontmatter `name`, else its directory's (common.ts's
      // `ompSkillName`), and lists it, asks its hint and finds its collisions under that name: a
      // copy it names otherwise than the profile does would be listed under a name the capture
      // owes nothing of. A load under another name rewrote the `name:` line (load.ts's
      // `renamedSkill`), so its copy passes; an arm text whose own `name:` is not the case
      // skill's is refused here.
      const text = ompText(readFileSync(path.join(dir, "SKILL.md")));
      const frontmatter = readFrontmatter(text);
      const named = ompSkillName(frontmatter, skill.name);
      if (named !== skill.name) {
        throw new Refused(
          `case ${c.id}: the profile's skill ${listed} is installed from ${from}, whose SKILL.md's frontmatter names it ${named}, and Oh My Pi names a skill by its frontmatter name, not ${skill.name}, the name the profile gives it${copy.armDir === undefined ? "" : ": an arm's text must keep the case skill's name"}`
        );
      }
      skills.push({ name: listed, dir, ...listingFrontmatter(frontmatter) });
    }
  } finally {
    for (const release of releases) release();
  }
  // The file a plugin reads the skill it injects from as it loads, where the arm installs no copy
  // of the skill under that name (`none`; a load test under another name): a hidden stand-in, so
  // the plugin loads and the listing names nothing more. No model reads it: an arm answers each
  // read of the skill itself (arms.ts).
  for (const injection of arm.injections ?? []) {
    if (injection.by !== "plugin") continue;
    const dir = path.join(skillsDir, injection.name);
    if (existsSync(dir)) continue;
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      path.join(dir, "SKILL.md"),
      `---\nname: ${injection.name}\ndescription: A stand-in the skill-cuts runner installs where an arm has no copy of this skill, so the ${injection.plugin} plugin, which reads this file as it loads, loads.\nhide: true\n---\n`
    );
    skillRecords.push({ name: injection.name, from: "stand-in" });
    skills.push({ name: injection.name, dir, ...skillFrontmatter(dir) });
  }

  const extensions: string[] = [];
  const pluginRecords: ContextRecord["plugins"][number][] = [];
  const trees: { readonly plugin: ProfilePlugin; readonly tree: string }[] = [];
  for (const plugin of c.profile.plugins) {
    const where = {
      name: plugin.name,
      repo: plugin.repo,
      commit: plugin.commit,
      path: plugin.path,
    };
    const key = cacheKey(plugin.repo, plugin.commit, plugin.path);
    const tree = await pluginCheckout(c, plugin, builds, source);
    trees.push({ plugin, tree });
    let step = "its package.json does not parse";
    try {
      const manifestFile = path.join(tree, plugin.path, "package.json");
      const manifest = existsSync(manifestFile)
        ? (JSON.parse(readFileSync(manifestFile, "utf8")) as Manifest)
        : undefined;
      if (manifest?.omp === undefined) {
        pluginRecords.push({ ...where, as: "skills only" });
        continue;
      }
      step = "its build failed";
      const built = await builds.entry("plugin", key, (dest) =>
        inTurn(tree, () => buildPlugin(tree, plugin.path, dest))
      );
      step = "it could not be installed";
      const dir = path.join(pluginsDir, plugin.name);
      if (existsSync(dir)) throw new Error(`a plugin named ${plugin.name} is installed already`);
      cpSync(built, dir, { recursive: true, verbatimSymlinks: true });
      for (const entry of strings(manifest.omp.skills)) {
        const link = path.join(dir, manifestPath(entry));
        rmSync(link, { recursive: true, force: true });
        mkdirSync(path.dirname(link), { recursive: true });
        symlinkSync(skillsDir, link, "dir");
      }
      extensions.push(dir);
      pluginRecords.push({ ...where, as: "extension", key });
    } catch (error) {
      // A cache entry that lost a file stops the context: leaving the plugin out would change it
      // in silence.
      if (error instanceof IncompleteCacheEntry) throw error;
      pluginRecords.push({
        ...where,
        as: "left out",
        key,
        reason: `${step}: ${messageFor(error)}`,
      });
    }
  }

  const prompts: string[] = [];
  const leftOut: string[] = [];
  let roleRecord: ContextRecord["rolePrompt"] = null;
  const role = legionRole(c.context);
  if (role !== undefined) {
    const composed = await rolePrompt(c, role, builds, options);
    prompts.push(composed.text);
    roleRecord = composed.record;
    leftOut.push(
      "the Legion addressing sentence, which names the session's claim and repository, is left out of the system prompt: a case does not name the claim"
    );
  } else if (c.profile.appendSystemPrompt !== undefined) {
    prompts.push(c.profile.appendSystemPrompt);
    roleRecord = "case";
  }
  const fileRecords: ContextRecord["contextFiles"][number][] = [];
  for (const file of c.profile.contextFiles) {
    const { text, key } = await fileText(builds, source, file.repo, file.commit, file.path);
    const where = { repo: file.repo, commit: file.commit, path: file.path, key };
    if (file.user === true) {
      // Oh My Pi's user-level context file: CLAUDE.md in Claude Code's config directory, which
      // the run names as CLAUDE_CONFIG_DIR (discovery/claude.ts at the pinned tag).
      mkdirSync(claudeConfigDir, { recursive: true });
      writeFileSync(path.join(claudeConfigDir, "CLAUDE.md"), text);
      fileRecords.push({ ...where, as: "user level" });
    } else if (Object.hasOwn(REPOSITORY_CONTEXT_FILES, path.basename(file.path))) {
      // A context file of the session repository, at its path in the checkout, where Oh My Pi
      // reads it from the working directory or a parent of it.
      const dest = path.join(checkoutDir, file.path);
      mkdirSync(path.dirname(dest), { recursive: true });
      writeFileSync(dest, text);
      fileRecords.push({ ...where, as: "working directory" });
    } else {
      prompts.push(text.replace(/\n+$/, ""));
      fileRecords.push({ ...where, as: "system prompt" });
    }
  }
  if (prompts.length > 0) writeFileSync(promptFile, prompts.join("\n\n"));
  const agents = await agentDirs(c, trees, builds, source);
  leftOut.push(...agents.leftOut);

  return {
    skillsDir,
    namespacedDirs,
    skills,
    extensions,
    workspace,
    checkout: checkoutDir,
    appendSystemPrompt: prompts.length > 0 ? promptFile : undefined,
    claudeConfigDir: userFiles > 0 ? claudeConfigDir : undefined,
    record: {
      source: source.name,
      skills: skillRecords,
      plugins: pluginRecords,
      contextFiles: fileRecords,
      rolePrompt: roleRecord,
      agent: agentIn(c.context, agents.dirs),
      leftOut,
      builds: builds.list,
      githubRetries: retriesSince(retriedBefore, source.reads?.().retries ?? {}),
    },
  };
}
