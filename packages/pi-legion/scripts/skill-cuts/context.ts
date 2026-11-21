/**
 * The case context of a capture: what one case's arm installs before
 * Oh My Pi builds its request, rebuilt from the case's `profile`, since a transcript keeps no
 * system prompt. `buildContext` writes it under one scratch directory, the same path for every arm
 * of a case, so the paths Oh My Pi puts in its prompt are the same in every arm:
 *
 * - `skills/`: the arm's installed skills (`arm.profileSkills`), one directory each under its
 *   name. The case's skill comes from the arm's own directory; every other skill is its
 *   repository's directory at its commit, read through `source` and kept under the cache
 *   (`skills/<key of repo, commit, path>`).
 * - `plugins/<name>/`: each plugin of `profile.plugins` that is an Oh My Pi extension (its
 *   directory holds a `package.json` with an `omp` field), built as the worker image builds the
 *   Legion plugins: the repository at the commit (`trees/<key of repo, commit>`), its packages
 *   installed from its own lockfile (`bun install --frozen-lockfile --ignore-scripts`, only the
 *   plugin's workspace when the repository is a workspace), `bun run build` when the plugin has a
 *   build script, then the files its `files` list ships with each `omp.extensions` entry pointed at
 *   its bundle in `dist/` (`plugins/<key of repo, commit, path>`). Each skills path the plugin's
 *   manifest names (`omp.skills`, `dist/skills` for the Legion plugins, where the Envoy plugin
 *   also looks for its skills at load) is a link to `skills/`, so a plugin lists the arm's skills
 *   and no copy of its own: only the case's skill differs between arms. A plugin with no
 *   `package.json` of that kind installs skills alone (a verification-skills plugin), which the
 *   profile's skills already hold. A plugin that cannot be installed (its repository cannot be
 *   read, its manifest cannot be parsed, its build fails, or a plugin of its name is installed
 *   already) is left out, with its reason.
 * - `work/`: the session's working directory. Each context file named `AGENTS.md` or `CLAUDE.md`
 *   (the session repository's, at its default branch's commit at the session's start) is written
 *   there at its path, where Oh My Pi reads context files.
 * - `claude-config/`: for a `devbox` context, whose one context file is the user-level one its
 *   session's host read (build.ts), that file as `CLAUDE.md` in a scratch Claude Code config
 *   directory (`claudeConfigDir`), which the capture names as `CLAUDE_CONFIG_DIR`: Oh My Pi 18.18.6
 *   reads `CLAUDE.md` there as its one user-level context file (discovery/claude.ts), so the
 *   block's path, its rank and its dedupe are Oh My Pi's own, as on the dev box. A devbox profile
 *   of more than one context file is refused.
 * - `system-prompt.md`, the one `--append-system-prompt` file, as the daemon writes it
 *   (packages/daemon/internal/runtime/systemprompt.go, `ComposePrompt`): for a Legion context
 *   (`legion:<project>:<role>`, the role `architect-root` for a tree's root architect), the role
 *   prompt, then every other context file (the project's instructions file, and `shared.md` when
 *   the project's prompt includes it), each with its trailing newlines dropped and joined by a
 *   blank line. The role prompt is `profile.appendSystemPrompt` when the case holds it, else the
 *   parts `prompts.Composer.Compose(role, isRoot)` (packages/daemon/internal/prompts/prompts.go)
 *   names at the commit of the plugin repository that holds that file: Compose answers with paths
 *   of the parts it embeds, so it is run once per distinct `prompts.go` blob and role, by a small
 *   Go test written into the cached copy of the repository (never the workspace) and removed after
 *   (`composes/<blob>-<role>`), and each part is then read at the session's commit. For the
 *   controller (`legion:<project>:controller`) the parts are those `ControllerPromptPaths(true)`
 *   names: a Legion store's controller runs headless in a pod. The daemon's addressing sentence,
 *   which sits between the two, names the session's claim, which a case does not hold, so it is
 *   left out and the record says so. A subagent's context (`subagent:<agent>`) has no role prompt:
 *   its case carries the system prompt its session recorded, which the capture sends whole.
 *
 * `source` reads the repositories: `ghSource` with read-only `gh api` calls (a directory from the
 * commit's tree and its blobs, a whole repository from its tarball), `localSource` from a
 * directory per repository and commit (the tests' made-up repositories). Repository and owner
 * names come only from the case. Every cache entry is `<cacheDir>/<kind>s/<key>` (`skills`,
 * `trees`, `plugins`, `composes`, and `files` for each single file read), made into a fresh path
 * and renamed into place. `record` says what was used: each commit and cache key, each plugin
 * left out, and each build with its time and whether the cache already held it.
 */
import { createHash, randomBytes } from "node:crypto";
import {
  chmodSync,
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  renameSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { mapLimit } from "../skill-scenarios/production-dispatch";
import type { Arm } from "./arms";
import type { Case } from "./cases";
import {
  BlobBatch,
  blobBatches,
  blobQuery,
  graphqlArgs,
  parseJson,
  type TreeEntry,
} from "./copies";

/** Where the Legion daemon's role prompt composer is, in the repository that holds it. */
export const PROMPTS_GO = "packages/daemon/internal/prompts/prompts.go";

/** The repositories a context is read from. */
export interface RepoSource {
  /** What the record calls this source. */
  readonly name: string;
  /** The bytes of `file` of `repo` (`owner/name`) at `commit`. Throws NoSuchFile when it holds
   *  none, and another error when the read fails. */
  file(repo: string, commit: string, file: string): Promise<Uint8Array>;
  /** Writes each file of `repo` at `commit` under `dir` (a path inside the repository, or "" for
   *  all of it) into `dest`, at its path below `dir`. Throws when `dir` holds no file. */
  tree(repo: string, commit: string, dir: string, dest: string): Promise<void>;
  /** Reads ahead, in as few calls as it can, the files of `repo` at `commit` under each of `dirs`,
   *  so the `tree` calls for them that follow need no read of their own. Optional. */
  prefetch?(repo: string, commit: string, dirs: readonly string[]): Promise<void>;
  /** The reads this source has made of GitHub so far: how many `gh` calls, and the milliseconds
   *  in which at least one of them was running. Absent for a source that reads no GitHub. */
  reads?(): { readonly calls: number; readonly ms: number };
}

/** What `RepoSource.file` throws when the repository holds no such file at the commit, as opposed
 *  to a read that failed. */
export class NoSuchFile extends Error {}

/** One build the context needed: what it is, its cache key, whether the cache held it already,
 *  and how long it took, in milliseconds. */
export interface Build {
  readonly kind: "skill" | "tree" | "plugin" | "compose" | "file";
  readonly key: string;
  readonly hit: boolean;
  readonly ms: number;
}

/** What a capture's context used. */
export interface ContextRecord {
  readonly source: string;
  readonly skills: readonly {
    readonly name: string;
    /** The arm's own directory, or the repository directory and its cache key. */
    readonly from:
      | "arm"
      | { readonly repo: string; readonly commit: string; readonly path: string; key: string };
  }[];
  readonly plugins: readonly {
    readonly name: string;
    readonly repo: string;
    readonly commit: string;
    readonly path: string;
    /** `extension`: built and loaded with `--extension`; `skills only`: nothing to load beyond
     *  the profile's skills; `left out`: it could not be installed, for `reason` (a failed read,
     *  manifest, build or copy). */
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
  /** The role prompt: the case's own text, or the parts Compose named, or none. */
  readonly rolePrompt:
    | "case"
    | {
        readonly repo: string;
        readonly commit: string;
        readonly role: string;
        readonly isRoot: boolean;
        readonly promptsBlob: string;
        readonly key: string;
        readonly parts: readonly string[];
      }
    | null;
  /** What of the real session's context the capture does not reproduce. */
  readonly leftOut: readonly string[];
  readonly builds: readonly Build[];
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
  readonly skills: readonly InstalledSkill[];
  /** Each built plugin's directory, as `--extension` takes it. */
  readonly extensions: readonly string[];
  readonly workspace: string;
  /** The `--append-system-prompt` file, when the context has one. */
  readonly appendSystemPrompt: string | undefined;
  /** The Claude Code config directory that holds a devbox context's user-level context file
   *  (`CLAUDE.md`), which the host's run names as `CLAUDE_CONFIG_DIR`; none for other contexts. */
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

/** What a context reads of a case: its id, its context and its profile. */
export type ContextCase = Pick<Case, "id" | "context" | "profile">;

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

/** An ambiguous plain scalar of a frontmatter line, as Oh My Pi finds one. */
const PLAIN_SCALAR_KEY_VALUE = /^(\s*[A-Za-z_][\w-]*:\s+)(\S.*?)(\s*)$/;
/** The first characters of a value that is no plain scalar. */
const FLOW_OR_EXPLICIT_VALUE_START = new Set(['"', "'", "[", "{", "|", ">", "!", "&", "*", "#"]);

/** `text`'s frontmatter block parsed as one YAML mapping, tabs read as two spaces, or null. */
function yamlRecord(text: string): Record<string, unknown> | null {
  const loaded: unknown = Bun.YAML.parse(text.replaceAll("\t", "  "));
  if (typeof loaded !== "object" || loaded === null || Array.isArray(loaded)) return null;
  return { ...loaded };
}

/**
 * `text` with each `<!--` … `-->` span dropped, as one pass of Oh My Pi's
 * `replace(/<!--[\s\S]*?-->/g, "")` drops them in packages/utils/src/frontmatter.ts at the tag
 * v18.18.6-sami.20261117-013345, so the runner reads the description Oh My Pi reads: a span runs
 * from a `<!--` to the first `-->` that starts after it, and the scan goes on after that `-->`, so
 * the text a drop joins together is not read again (`<!<!---->--` gives `<!--`, as Oh My Pi's pass
 * does). A `<!--` with no `-->` after it stays. This is no sanitizer: its output is frontmatter
 * text for a YAML parse and is never rendered as HTML.
 */
function dropComments(text: string): string {
  let kept = "";
  let at = 0;
  for (;;) {
    const open = text.indexOf("<!--", at);
    if (open === -1) break;
    const close = text.indexOf("-->", open + 4);
    if (close === -1) break;
    kept += text.slice(at, open);
    at = close + 3;
  }
  return kept + text.slice(at);
}

/**
 * The frontmatter of a SKILL.md's text, read as Oh My Pi's `parseFrontmatter` reads a skill's
 * with its defaults (packages/utils/src/frontmatter.ts of cybermonklabs/oh-my-pi at the tag
 * v18.18.6-sami.20261117-013345, the pin): CRLF as LF; leading HTML comments before the opening
 * `---` dropped, and comments inside the block; the block from after `---` to the next line that
 * starts `---`; parsed as YAML with each tab replaced by two spaces; when that fails, parsed again
 * with each plain scalar that holds `: ` quoted (`quoteAmbiguousPlainScalars`); when that fails
 * too, each `key: value` line read on its own, a value that does not parse as YAML kept as its
 * text. Top-level kebab-case keys become camelCase. A text with no block has none (`{}`).
 */
export function readFrontmatter(content: string): Record<string, unknown> {
  let text = content.replace(/\r\n?/g, "\n");
  const comments = /^(?:(?:\s*<!--[\s\S]*?-->)+)/.exec(text);
  if (comments && /^\s*---/.test(text.slice(comments[0].length))) {
    text = text.slice(comments[0].length).replace(/^\s+/, "");
  }
  if (!text.startsWith("---")) return {};
  const end = text.indexOf("\n---", 3);
  if (end === -1) return {};
  const block = dropComments(text.slice(4, end));
  let read: Record<string, unknown> | null | undefined;
  try {
    read = yamlRecord(block);
  } catch {
    let quoted = false;
    const repaired = block
      .split("\n")
      .map((line) => {
        const match = PLAIN_SCALAR_KEY_VALUE.exec(line);
        const value = match?.[2]?.trimEnd() ?? "";
        if (!value.includes(": ") || FLOW_OR_EXPLICIT_VALUE_START.has(value[0] ?? "")) return line;
        quoted = true;
        return `${match?.[1]}${JSON.stringify(value)}${match?.[3]}`;
      })
      .join("\n");
    try {
      read = quoted ? yamlRecord(repaired) : undefined;
    } catch {
      read = undefined;
    }
    if (read === undefined) {
      read = {};
      for (const line of block.split("\n")) {
        const match = /^([\w-]+):\s*(.*)$/.exec(line);
        if (match?.[1] === undefined) continue;
        const raw = (match[2] ?? "").trim();
        let value: unknown = raw;
        if (raw !== "") {
          try {
            const parsed: unknown = Bun.YAML.parse(raw);
            if ((parsed !== null && typeof parsed !== "object") || Array.isArray(parsed)) {
              value = parsed;
            }
          } catch {
            // The value's own text stands.
          }
        }
        read[match[1]] = value;
      }
    }
  }
  return Object.fromEntries(
    Object.entries(read ?? {}).map(([key, value]) => [
      key.replace(/-([a-z])/g, (_, letter: string) => letter.toUpperCase()),
      value,
    ])
  );
}

/** The frontmatter of the skill in `dir`, as Oh My Pi reads it for the listing (`readFrontmatter`):
 *  its `description` ("" when it has none), and whether it is hidden from the listing, which Oh My
 *  Pi does for `hide: true` or `disable-model-invocation: true` (extensibility/skills.ts at the
 *  pinned tag). */
export function skillFrontmatter(dir: string): { description: string; hidden: boolean } {
  const frontmatter = readFrontmatter(readFileSync(path.join(dir, "SKILL.md"), "utf8"));
  const description = frontmatter.description;
  return {
    description: typeof description === "string" ? description : "",
    hidden: frontmatter.hide === true || frontmatter.disableModelInvocation === true,
  };
}

/** What a finished command gave. */
interface Ran {
  readonly code: number;
  readonly stdout: Uint8Array;
  readonly stderr: string;
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
  const limit = timeLimit(child, options.timeoutMs);
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).arrayBuffer(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  limit.clear();
  return {
    code,
    stdout: new Uint8Array(stdout),
    stderr: limit.timedOut() ? `${stderr}\ntimed out after ${options.timeoutMs} ms` : stderr,
  };
}

/** Ends `child` (SIGTERM) once `ms` have passed, if `ms` is given. `timedOut()` says whether that
 *  ended it, so a child a signal from elsewhere ended is not reported as out of time. */
function timeLimit(
  child: { kill(): void },
  ms: number | undefined
): { timedOut(): boolean; clear(): void } {
  let timedOut = false;
  const timer =
    ms === undefined
      ? undefined
      : setTimeout(() => {
          timedOut = true;
          child.kill();
        }, ms);
  return { timedOut: () => timedOut, clear: () => clearTimeout(timer) };
}

/** Runs `argv` and throws, naming it and its last error lines, when it fails. */
async function mustRun(
  argv: readonly string[],
  options: { readonly cwd?: string; readonly env?: Record<string, string | undefined> } = {}
): Promise<Ran> {
  const ran = await run(argv, options);
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

/** How many times one `gh` read is made before it fails, by default. */
const GH_TRIES = 5;
/** The most blobs, and the most bytes, one GraphQL read of blobs asks for. */
const BLOB_BATCH = 50;
const BLOB_BATCH_BYTES = 4_000_000;
/** The wait before a read's second try; each try after waits twice the one before, at most
 *  GH_MAX_WAIT_MS. A rate limit waits until its reset instead, at most GH_MAX_RATE_WAIT_MS. */
const GH_FIRST_WAIT_MS = 2_000;
const GH_MAX_WAIT_MS = 60_000;
const GH_MAX_RATE_WAIT_MS = 15 * 60_000;
/** The longest one `gh` read may run: an API read, and a tarball. */
const GH_READ_TIMEOUT_MS = 120_000;
const GH_TARBALL_TIMEOUT_MS = 900_000;
/** A failure worth another try: a network error (a timeout, a reset or refused connection, a
 *  name that did not resolve), or an HTTP 5xx, as gh reports them on stderr. */
const TRANSIENT_GH =
  /i\/o timeout|timed out|timeout|connection reset|connection refused|reset by peer|ECONNRESET|ECONNREFUSED|TLS handshake|no such host|temporary failure|broken pipe|unexpected EOF|HTTP 5\d\d/i;
/** A rate limit, primary or secondary, as gh reports it. */
const RATE_LIMITED_GH = /rate limit|HTTP 429/i;

/** What `ghSource` takes beside the binary: how many tries a read gets, and the wait (a test's). */
export interface GhOptions {
  readonly tries?: number;
  readonly sleep?: (ms: number) => Promise<void>;
}

/**
 * The value of the first try of `attempt` that exits 0. A try that fails transiently or on a rate
 * limit (`TRANSIENT_GH`, `RATE_LIMITED_GH`) is made again after a growing wait, or after the rate
 * limit's reset (gh prints no `retry-after` header on stderr, so `rateReset` reads the reset of
 * the call's own limit, REST's or GraphQL's, from `rate_limit`, and a limit whose reset cannot be
 * read waits a minute), at most `tries` times in all; any other failure, or the last try's,
 * throws naming `command`, each try's error and the count.
 */
async function ghRetried<T>(
  command: string,
  attempt: () => Promise<{ readonly code: number; readonly stderr: string; readonly value: T }>,
  options: GhOptions,
  rateReset: () => Promise<number | undefined>
): Promise<T> {
  const tries = options.tries ?? GH_TRIES;
  const sleep = options.sleep ?? Bun.sleep;
  const errors: string[] = [];
  for (let n = 1; ; n++) {
    const ran = await attempt();
    if (ran.code === 0) return ran.value;
    const said = ran.stderr.trim().split("\n").slice(-4).join(" / ") || "no error text";
    errors.push(`try ${n}: exit ${ran.code}: ${said}`);
    const limited = RATE_LIMITED_GH.test(ran.stderr);
    const transient = limited || TRANSIENT_GH.test(ran.stderr);
    if (!transient || n >= tries) {
      throw new Error(
        `\`${command}\` failed on ${n === 1 ? "its one try" : `each of ${n} tries`}${transient ? "" : " (not a transient failure, so not tried again)"}: ${errors.join("; ")}`
      );
    }
    let wait = Math.min(GH_FIRST_WAIT_MS * 2 ** (n - 1), GH_MAX_WAIT_MS);
    if (limited) {
      const reset = await rateReset();
      const untilReset = reset === undefined ? 60_000 : reset * 1000 - Date.now() + 1_000;
      wait = Math.min(Math.max(wait, untilReset), GH_MAX_RATE_WAIT_MS);
    }
    await sleep(wait);
  }
}

/**
 * The repositories as GitHub serves them, read with `gh api` reads only: a file from its contents,
 * a directory from the commit's recursive tree and its blobs, the whole repository from its
 * tarball. A tree is read once per repository and commit while it is among the last TREE_LISTINGS
 * used, and only its blob entries' path, mode, id and size are kept; a tree read that failed is
 * not kept, so the next case at that commit reads it again. A tree GitHub cuts short (`truncated`)
 * is read from the tarball instead. `prefetch` reads the blobs of many directories of one commit in
 * GraphQL batches, `object(oid: …)` up to BLOB_BATCH blobs and BLOB_BATCH_BYTES bytes a call, with
 * `-f owner=<owner> -f name=<repo>` so the pod's gh sends it with that owner's token (as copies.ts
 * does); a blob a batch does not give whole (binary, truncated, missing, in a batch that failed,
 * or a text whose bytes are not the blob's, checked by its git blob id) is read alone over REST.
 * A blob a batch gave is kept until each file of those directories that holds it is written. Each
 * read is tried again on a transient failure (`ghRetried`), so one network error stops no screen.
 * `reads()` counts the `gh` calls made and the time in which one ran.
 */
export function ghSource(gh = "gh", options: GhOptions = {}): RepoSource {
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
  const ghRun = (argv: readonly string[]) =>
    counted(() => run(argv, { timeoutMs: GH_READ_TIMEOUT_MS }));
  // When the rate limit of `resource` (`core`, REST's, or `graphql`) resets, in seconds since the
  // epoch, or undefined when it cannot be read.
  const rateReset = async (resource: "core" | "graphql"): Promise<number | undefined> => {
    const ran = await ghRun([gh, "api", "--method", "GET", "rate_limit"]);
    try {
      const reset: unknown = JSON.parse(new TextDecoder().decode(ran.stdout))?.resources?.[resource]
        ?.reset;
      return ran.code === 0 && typeof reset === "number" ? reset : undefined;
    } catch {
      return undefined;
    }
  };
  const restReset = () => rateReset("core");
  const api = async (route: string, raw: boolean): Promise<Uint8Array> => {
    const accept = raw ? ["-H", "Accept: application/vnd.github.raw"] : [];
    const argv = [gh, "api", "--method", "GET", ...accept, route];
    return ghRetried(
      argv.join(" "),
      async () => {
        const ran = await ghRun(argv);
        return { code: ran.code, stderr: ran.stderr, value: ran.stdout };
      },
      options,
      restReset
    );
  };
  const tarball = async (repo: string, commit: string, dest: string): Promise<void> => {
    mkdirSync(dest, { recursive: true });
    const archive = `${dest}.tar.gz`;
    try {
      const argv = [gh, "api", "--method", "GET", `repos/${repo}/tarball/${commit}`];
      await ghRetried(
        argv.join(" "),
        () =>
          counted(async () => {
            const child = Bun.spawn(argv, {
              stdin: "ignore",
              stdout: Bun.file(archive),
              stderr: "pipe",
            });
            const limit = timeLimit(child, GH_TARBALL_TIMEOUT_MS);
            const [stderr, code] = await Promise.all([
              new Response(child.stderr).text(),
              child.exited,
            ]);
            limit.clear();
            return {
              code,
              stderr: limit.timedOut()
                ? `${stderr}\ntimed out after ${GH_TARBALL_TIMEOUT_MS} ms`
                : stderr,
              value: undefined,
            };
          }),
        options,
        restReset
      );
      await mustRun(["tar", "-xzf", archive, "-C", dest, "--strip-components=1"]);
    } finally {
      rmSync(archive, { force: true });
    }
  };
  // The commit's recursive tree, its blob entries only, read once while it is among the last
  // TREE_LISTINGS used. A read that failed is dropped, so the next one reads again.
  const listingOf = (repo: string, commit: string): Promise<Listing> => {
    const id = `${repo}@${commit}`;
    const kept = trees.get(id);
    if (kept !== undefined) {
      // Used again: it goes to the end, the last to be dropped.
      trees.delete(id);
      trees.set(id, kept);
      return kept;
    }
    const listing = api(`repos/${repo}/git/trees/${commit}?recursive=1`, false).then((bytes) => {
      const parsed = JSON.parse(new TextDecoder().decode(bytes)) as {
        tree: TreeEntry[];
        truncated: boolean;
      };
      const entries = parsed.tree
        .filter((entry) => entry.type === "blob")
        .map((entry) => ({ path: entry.path, mode: entry.mode, sha: entry.sha, size: entry.size }));
      return { entries, truncated: parsed.truncated };
    });
    listing.catch(() => {
      if (trees.get(id) === listing) trees.delete(id);
    });
    trees.set(id, listing);
    for (const old of trees.keys()) {
      if (trees.size <= TREE_LISTINGS) break;
      trees.delete(old);
    }
    return listing;
  };
  // Each blob a GraphQL batch gave whole, by its id: its bytes, and how many files of the
  // directories read ahead are still to be written from it. It is dropped once the last is.
  const blobs = new Map<string, { readonly bytes: Uint8Array; waiting: number }>();
  return {
    name: "gh",
    reads: () => ({ calls, ms: busyMs + (running > 0 ? performance.now() - busyFrom : 0) }),
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
      const { entries, truncated } = await listingOf(repo, commit);
      if (truncated) return;
      const prefixes = dirs.map((dir) => `${dir.replace(/\/+$/, "")}/`);
      // Each blob to read, and how many files under `dirs` hold it.
      const wanted = new Map<string, { entry: BlobEntry; files: number }>();
      for (const entry of entries) {
        if (!/^[0-9a-f]{40}$/.test(entry.sha)) continue;
        if (!prefixes.some((prefix) => entry.path.startsWith(prefix))) continue;
        const held = blobs.get(entry.sha);
        const want = wanted.get(entry.sha);
        if (held !== undefined) held.waiting += 1;
        else if (want !== undefined) want.files += 1;
        else wanted.set(entry.sha, { entry, files: 1 });
      }
      const entriesWanted = [...wanted.values()].map(({ entry }) => entry);
      for (const batch of blobBatches(entriesWanted, BLOB_BATCH, BLOB_BATCH_BYTES)) {
        const args = graphqlArgs(repo, blobQuery(batch));
        let answer: unknown;
        try {
          const stdout = await ghRetried(
            `${[gh, ...args.slice(0, -2)].join(" ")} -f query=<${batch.length} blobs>`,
            async () => {
              const ran = await ghRun([gh, ...args]);
              return { code: ran.code, stderr: ran.stderr, value: ran.stdout };
            },
            options,
            () => rateReset("graphql")
          );
          answer = parseJson(new TextDecoder().decode(stdout));
        } catch {
          // The batch failed for good: each of its blobs is read alone, over REST, in `tree`.
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
            blobs.set(entry.sha, { bytes, waiting: wanted.get(entry.sha)?.files ?? 1 });
          }
        });
      }
    },
    async tree(repo, commit, dir, dest) {
      if (dir === "") return tarball(repo, commit, dest);
      const { entries, truncated } = await listingOf(repo, commit);
      const prefix = `${dir.replace(/\/+$/, "")}/`;
      if (truncated) {
        const whole = mkdtempSync(`${dest}.whole-`);
        try {
          await tarball(repo, commit, whole);
          if (!existsSync(path.join(whole, prefix))) {
            throw new Error(`${repo} at ${commit} holds no directory ${dir}`);
          }
          cpSync(path.join(whole, prefix), dest, { recursive: true, verbatimSymlinks: true });
        } finally {
          rmSync(whole, { recursive: true, force: true });
        }
        return;
      }
      const files = entries.filter((entry) => entry.path.startsWith(prefix));
      if (files.length === 0) throw new Error(`${repo} at ${commit} holds no file under ${dir}`);
      await mapLimit(files, 4, async (entry) => {
        const held = blobs.get(entry.sha);
        const bytes = held?.bytes ?? (await api(`repos/${repo}/git/blobs/${entry.sha}`, true));
        writeBlob(path.join(dest, entry.path.slice(prefix.length)), entry.mode, bytes);
        if (held !== undefined) {
          held.waiting -= 1;
          if (held.waiting <= 0 && blobs.get(entry.sha) === held) blobs.delete(entry.sha);
        }
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
    async tree(repo, commit, dir, dest) {
      const where = path.join(at(repo, commit), dir);
      if (!existsSync(where)) throw new Error(`${repo} at ${commit} holds no directory ${dir}`);
      cpSync(where, dest, { recursive: true, verbatimSymlinks: true });
    },
  };
}

/** The builds of one context, each timed and kept in the cache. */
class Builds {
  readonly list: Build[] = [];
  constructor(readonly cacheDir: string) {}

  /** `cacheDir/<kind>s/<key>`, made by `make` into a fresh path that is then renamed into place,
   *  unless the cache holds it already. */
  async entry(
    kind: Build["kind"],
    key: string,
    make: (dest: string) => Promise<void>
  ): Promise<string> {
    const started = performance.now();
    const where = path.join(this.cacheDir, `${kind}s`, key);
    let hit = existsSync(where);
    if (!hit) {
      mkdirSync(path.dirname(where), { recursive: true });
      const fresh = `${where}.part-${randomBytes(4).toString("hex")}`;
      try {
        await make(fresh);
        renameSync(fresh, where);
      } catch (error) {
        rmSync(fresh, { recursive: true, force: true });
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

/** The plugin `path` of the repository checked out at `tree`, built into `dest` (see the file's
 *  header). */
async function buildPlugin(tree: string, pluginPath: string, dest: string): Promise<void> {
  const dir = path.join(tree, pluginPath);
  const manifest = JSON.parse(readFileSync(path.join(dir, "package.json"), "utf8")) as Manifest;
  const lockfile = (at: string) =>
    existsSync(path.join(at, "bun.lock")) || existsSync(path.join(at, "bun.lockb"));
  const install = ["bun", "install", "--frozen-lockfile", "--ignore-scripts"];
  const rootFile = path.join(tree, "package.json");
  const root: Manifest = existsSync(rootFile) ? JSON.parse(readFileSync(rootFile, "utf8")) : {};
  if (pluginPath !== "" && root.workspaces !== undefined && lockfile(tree)) {
    await mustRun([...install, "--filter", `./${pluginPath}`], { cwd: tree });
  } else if (lockfile(dir)) {
    await mustRun(install, { cwd: dir });
  }
  if (typeof manifest.scripts?.build === "string") {
    await mustRun(["bun", "run", "build"], { cwd: dir });
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
    `${JSON.stringify({ ...manifest, omp: { ...manifest.omp, extensions } }, null, 2)}\n`
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

/** The role prompt of `c`'s Legion context and its record: the parts Compose names at the commit
 *  of the plugin repository that holds PROMPTS_GO, joined as `ComposePrompt` joins them. */
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
    ...new Map(c.profile.plugins.map((p) => [`${p.repo}@${p.commit}`, p])).values(),
  ];
  let known = promptsBlobs.get(source);
  if (known === undefined) {
    known = new Map();
    promptsBlobs.set(source, known);
  }
  for (const { repo, commit } of repositories) {
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
      record: { repo, commit, role: role.role, isRoot: role.isRoot, promptsBlob, key, parts },
    };
  }
  throw new Error(
    `case ${c.id}: its Legion context needs a role prompt, and no plugin repository holds ${PROMPTS_GO}`
  );
}

/** Writes the context of `c`'s arm `arm` under `options.root`, replacing one written there. */
export async function buildContext(
  c: ContextCase,
  arm: Arm,
  options: ContextOptions
): Promise<CaseContext> {
  const { source, root } = options;
  const builds = new Builds(options.cacheDir);
  const skillsDir = path.join(root, "skills");
  const pluginsDir = path.join(root, "plugins");
  const workspace = path.join(root, "work");
  const promptFile = path.join(root, "system-prompt.md");
  const claudeConfigDir = path.join(root, "claude-config");
  for (const owned of [skillsDir, pluginsDir, workspace, promptFile, claudeConfigDir]) {
    rmSync(owned, { recursive: true, force: true });
  }
  // A devbox profile's context file is the user-level one its session's host read (build.ts: a
  // devbox session's working directory names no repository, so it has no other).
  const devbox = c.context === "devbox";
  if (devbox && c.profile.contextFiles.length > 1) {
    throw new Error(
      `case ${c.id}: its devbox profile holds ${c.profile.contextFiles.length} context files, and Oh My Pi reads one at user level`
    );
  }
  mkdirSync(skillsDir, { recursive: true });
  mkdirSync(workspace, { recursive: true });

  // The repository skills the cache lacks, read ahead per repository and commit: one tree and a
  // GraphQL batch per BLOB_BATCH blobs, where each file would take a call of its own.
  const ahead = new Map<string, { repo: string; commit: string; dirs: string[] }>();
  for (const skill of arm.profileSkills) {
    const key = cacheKey(skill.repo, skill.commit, skill.path);
    if (skill.name === arm.skill || existsSync(path.join(options.cacheDir, "skills", key)))
      continue;
    const id = `${skill.repo}@${skill.commit}`;
    const group = ahead.get(id) ?? { repo: skill.repo, commit: skill.commit, dirs: [] };
    group.dirs.push(skill.path);
    ahead.set(id, group);
  }
  for (const { repo, commit, dirs } of ahead.values()) {
    await source.prefetch?.(repo, commit, dirs);
  }
  const skills: InstalledSkill[] = [];
  const skillRecords: ContextRecord["skills"][number][] = [];
  for (const skill of arm.profileSkills) {
    const dir = path.join(skillsDir, skill.name);
    if (existsSync(dir)) throw new Error(`case ${c.id}: the profile installs ${skill.name} twice`);
    if (skill.name === arm.skill) {
      if (arm.skillDir === undefined) throw new Error(`case ${c.id}: arm ${arm.name} has no skill`);
      cpSync(arm.skillDir, dir, { recursive: true, verbatimSymlinks: true });
      skillRecords.push({ name: skill.name, from: "arm" });
    } else {
      const key = cacheKey(skill.repo, skill.commit, skill.path);
      const cached = await builds.entry("skill", key, (dest) =>
        source.tree(skill.repo, skill.commit, skill.path, dest)
      );
      cpSync(cached, dir, { recursive: true, verbatimSymlinks: true });
      skillRecords.push({
        name: skill.name,
        from: { repo: skill.repo, commit: skill.commit, path: skill.path, key },
      });
    }
    skills.push({ name: skill.name, dir, ...skillFrontmatter(dir) });
  }

  const extensions: string[] = [];
  const pluginRecords: ContextRecord["plugins"][number][] = [];
  for (const plugin of c.profile.plugins) {
    const where = {
      name: plugin.name,
      repo: plugin.repo,
      commit: plugin.commit,
      path: plugin.path,
    };
    const key = cacheKey(plugin.repo, plugin.commit, plugin.path);
    try {
      const tree = await checkout(builds, source, plugin.repo, plugin.commit);
      const manifestFile = path.join(tree, plugin.path, "package.json");
      const manifest = existsSync(manifestFile)
        ? (JSON.parse(readFileSync(manifestFile, "utf8")) as Manifest)
        : undefined;
      if (manifest?.omp === undefined) {
        pluginRecords.push({ ...where, as: "skills only" });
        continue;
      }
      const built = await builds.entry("plugin", key, (dest) =>
        buildPlugin(tree, plugin.path, dest)
      );
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
      pluginRecords.push({ ...where, as: "left out", key, reason: messageFor(error) });
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
    if (devbox) {
      // Oh My Pi's user-level context file: CLAUDE.md in Claude Code's config directory, which
      // the run names as CLAUDE_CONFIG_DIR (discovery/claude.ts at the pinned tag).
      mkdirSync(claudeConfigDir, { recursive: true });
      writeFileSync(path.join(claudeConfigDir, "CLAUDE.md"), text);
      fileRecords.push({ ...file, key, as: "user level" });
    } else if (["AGENTS.md", "CLAUDE.md"].includes(path.basename(file.path))) {
      // Oh My Pi reads these context files from the working directory.
      const dest = path.join(workspace, file.path);
      mkdirSync(path.dirname(dest), { recursive: true });
      writeFileSync(dest, text);
      fileRecords.push({ ...file, key, as: "working directory" });
    } else {
      prompts.push(text.replace(/\n+$/, ""));
      fileRecords.push({ ...file, key, as: "system prompt" });
    }
  }
  if (prompts.length > 0) writeFileSync(promptFile, prompts.join("\n\n"));

  return {
    skillsDir,
    skills,
    extensions,
    workspace,
    appendSystemPrompt: prompts.length > 0 ? promptFile : undefined,
    claudeConfigDir: devbox && fileRecords.length > 0 ? claudeConfigDir : undefined,
    record: {
      source: source.name,
      skills: skillRecords,
      plugins: pluginRecords,
      contextFiles: fileRecords,
      rolePrompt: roleRecord,
      leftOut,
      builds: builds.list,
    },
  };
}
