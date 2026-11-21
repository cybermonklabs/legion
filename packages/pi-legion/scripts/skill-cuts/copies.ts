/**
 * The list of skill copies, without their reads (attribute.ts counts those): every skill copy of
 * the configured GitHub owners' repositories, the manifests and installers that ship, install or
 * load each, the files outside a skill that name one, and the copy-path prefix table attribute.ts
 * places each read by.
 *
 *   bun scripts/skill-cuts/copies.ts --config <skill-cuts.json> --at <ISO instant> --out <dir>
 *     [--only <owner/repo>]... [--max-search-calls <n>] [--cache-dir <dir>]
 *
 * cli.ts runs it as `skill-cuts copies`; `main(argv, io)` and the pure parts are exported for
 * that and for copies.test.ts, which injects the command runner (`io.gh`).
 *
 * It reads GitHub only through `gh` (`gh repo list`, `gh api`), with whatever token the pod's `gh`
 * already uses: this file holds no token, sets no GH_TOKEN and needs none of its own. The pod's gh
 * sends each call with the token of the one owner the call names, so every call here names exactly
 * one owner (a search names one `org:`, `user:` or `repo:` qualifier). Blobs are read in batches of
 * up to 50 with `gh api graphql`, whose `-f owner=<owner>` field (the query's `$owner` variable) is
 * what the pod's gh routes a GraphQL call by, so it takes that owner's token; a blob a batch does
 * not give whole (binary, truncated, missing) is read alone with
 * `gh api repos/<r>/git/blobs/<sha>`.
 *
 * The run's peak resident memory (memory.ts) is checked before every gh call and at each step:
 * above MEMORY_LIMIT_BYTES (1.5 GiB) the run stops, exit 3, writing nothing; it is in the summary
 * line and skill-copies.md otherwise. The run keeps no blob text past the repository it came from
 * (`readRepositoryStep`): each repository's SKILL.md and manifest files are parsed in its step and
 * only the parsed fields kept (`RepositoryRead`), its tree is dropped once its step is done, and a
 * fork's text files are written to disk as they are read and read back one at a time by its scan,
 * which keeps only its hits. So the memory a run holds grows with the skills and manifests it
 * lists, not with the blobs it reads.
 *
 * `--cache-dir <dir>` keeps each finished read (`ReadCache`), so a run stopped and started again
 * loses nothing: each owner's listing, each repository whole (its commit at `--at`, kept tree
 * entries and their count, the SKILL.md and manifest blobs it read, and apart from them a fork's
 * text files, written the moment that repository is done, keyed by repository, default branch
 * and `--at`, which name one commit), each level a truncated tree's walk finishes (keyed by
 * repository, root tree, fork flag, KEEP_RULES_VERSION and level: a tree's id fixes its content,
 * so a walk stopped midway goes on from its next level), and each code search run whole, keyed by
 * its query. A cached read makes no GitHub call; a read that failed is not cached, so it is tried
 * again.
 * Without `--cache-dir` the same files go to a temporary directory, removed when the run ends.
 * stderr gets one line per repository done (`copies: tree <n>/<total> <owner/repo>: <entries>
 * entries, <calls> tree reads, <seconds> s, cached|read`) and one per 25 searches with the calls
 * so far.
 *
 * 1. Repositories. `gh repo list <owner> --limit 2000 --json nameWithOwner,isFork,isArchived,
 *    defaultBranchRef,parent` for each configured owner (with `--only`, for the owners of the named
 *    repositories, keeping those alone). For each repository, the default branch's commit at `--at`
 *    (`repos/<r>/commits?sha=<branch>&until=<at>&per_page=1`) and that commit's tree
 *    (`git/trees/<tree>?recursive=1`, through gh's jq as one line per entry, so the answer's JSON
 *    never sits in this process). Of each tree only the entries this list uses are kept, as each
 *    answer is read (`keepsEntry`: each `SKILL.md`, each file of a manifest kind, and in a fork
 *    each text file its scan may read); the rest are counted (`repositories.tsv`'s `tree_entries`)
 *    and dropped. A tree GitHub answers `truncated` is walked a level at a time, each level's
 *    subtrees in GraphQL batches of up to 50 (`readTree`), with REST where GitHub refuses a batch
 *    (but for its rate limit: a batch, of subtrees, blobs or files, refused for the GraphQL rate
 *    limit is sent again after its reset, or for the secondary rate limit after a minute, doubled
 *    at each refusal, `Github.rateWait`); the walk holds only the kept entries and the subtrees
 *    of its next level. A listing, commit or tree read that fails names its repository with gh's
 *    error. Forks and archived repositories stay, marked (a fork with its parent).
 *    REPOSITORY_JOBS (6) repositories are read at once, and every gh call reading one (commit,
 *    tree, subtrees, blobs), and every code search (step 5, which runs beside these reads), takes
 *    one of READ_CALLS (8) slots across the run, at most READ_CALLS_PER_REPOSITORY (4) of them its
 *    repository's, so a large repository's walk, the small repositories and the searches share
 *    them; the output is the same, byte for byte, as one repository at a time.
 * 2. Skills. Each `SKILL.md` blob outside a configured `excludePaths` prefix of its repository: its
 *    frontmatter `name` (else its directory's name) and `description`, read from the blob. The
 *    frontmatter is read as YAML (Bun.YAML); one that is not valid YAML is read by `lenientFields`
 *    (top-level `key: value`, plain or quoted, folded `>` or literal `|` blocks, flow lists).
 * 3. Shared names. Each name in two or more repositories, and each name twice in one repository,
 *    forks and archived copies included and marked.
 * 4. Manifests, read whole from the trees, by path (`MANIFEST_KINDS`). Each gives installer rows:
 *    which skills it ships, installs, links or loads, into which unit and host. A `package.json` is
 *    read only in a repository that holds a `SKILL.md` (a package that ships skills ships them from
 *    such a repository); every other kind is read wherever its path is. Every manifest read is
 *    resolved (a package.json without omp.skills still names the package an npm-sourced plugin
 *    comes from); only those of their kind are listed, the rest counted.
 * 5. Loaders by code search. For each skill name, one `gh api -i -X GET search/code` for the quoted
 *    name, once per owner whose listing succeeded (`org:` or `user:`, as `users/<owner>` gives its
 *    type) or, with `--only`, once per named repository (`repo:`). One search stands for the five
 *    patterns a file can name a skill by (`skill://<name>`, `/skill:<name>`, `skills/<name>`,
 *    `<plugin>:<name>` and the quoted name): `searchTerms` says why. Each hit file is read by its
 *    path at its repository's
 *    commit of step 1 (GraphQL batches of 50, routed by owner, a REST read where a batch fails),
 *    once for every name whose search hit it, and dropped once checked; it is kept only where the
 *    skill's name is in it as a whole name (not inside a longer name), outside a skill;
 *    `mentions` then sorts each mention by those patterns. Each query's
 *    `total_count` and `incomplete_results`, its hits and their verdicts, and each failure go to
 *    `queries.jsonl`. An owner's query whose `total_count` reaches 1,000 is run again once per
 *    repository of that owner that holds a skill; so is an owner's query of a name in
 *    `COMMON_WORD_NAMES` (a short list of plain English words kept in this file, `dispatch` and
 *    `envoy` among them; a plain word not on it is run again only when it reaches 1,000) when that
 *    query was not read whole (`rerunsOf` says why one read whole is not). A fork, which code
 *    search leaves out, is read from its tree instead when it holds a skill: each text blob
 *    (`TEXT_EXTENSIONS`, or a name in `TEXT_NAMES`) of at most `FORK_SCAN_MAX_BYTES` bytes outside
 *    a skill that it changed against its parent (`forkChanges`: GitHub's compare through their
 *    merge base; its whole tree where the compare cannot answer whole) is read and searched for
 *    every skill name; queries.jsonl and skill-copies.md say which files of each fork were read.
 *    Its skills, manifests and installers are read from its whole tree. Pacing: GitHub allows 10
 *    code searches a
 *    minute per token, and each owner has its own token, so the owners' searches run at once, each
 *    owner's one at a time (GitHub asks for one client's requests to be serial) and paced by its
 *    own limit: at most `x-ratelimit-limit` (10 when absent) calls started in any 60 seconds, a
 *    wait for `x-ratelimit-reset` when `x-ratelimit-remaining` is 0, and a call refused for its
 *    rate limit run again after its `retry-after`, else its reset, else the reset `gh api
 *    rate_limit` gives. The searches start once the listing and the `users/<owner>` calls are
 *    done, beside the tree reads: each name is queued for every scope as the repository step that
 *    first gives it ends, and the reruns once every tree is read (`SearchQueues`). The records are
 *    written in one order (the names sorted, each in scope order, then each owner's reruns), and
 *    a record depends only on its query, so the output is the same as searching after the trees,
 *    unless `--max-search-calls` stops the searches (the queries it leaves not run may differ).
 *    `--max-search-calls <n>` stops searching after n calls; every query it leaves out is in
 *    `queries.jsonl` as not run. A query whose hits GitHub counts past its 1,000-result cap (or the
 *    budget cut short) keeps how many it left unread (`unread`), and skill-copies.md lists it.
 * 6. The prefix table. From the installers: a skill an Oh My Pi plugin package ships is at
 *    `**\/node_modules/<package>/<dir>/<name>/`, and, for a package the worker image carries (one
 *    the prepack script stages or whose directory holds the Claude Code build script), at
 *    `/opt/legion/<package's last name>/<dir>/<name>/` (packages/daemon/docker/worker.Dockerfile
 *    puts pi-legion and pi-envoy there); a Claude Code marketplace plugin's skill at
 *    `**\/plugins/cache/plugins/<marketplace>___<plugin>___*\/<dir>/` (Oh My Pi's cache) and
 *    `**\/plugins/cache/<marketplace>/<plugin>/*\/<dir>/` (Claude Code's); the plugin the build
 *    script generates, in a directory named as the plugin, at
 *    `/opt/legion/<package directory>/<plugin>/skills/<name>/`; a skill an installer links into a
 *    host's user skills at `**\/.omp/agent/skills/<name>/` or `**\/.claude/skills/<name>/`. Each
 *    row maps the prefix to its repository copy. A row whose repository the configuration's
 *    `targets` do not name is left out of the table and reported in `skill-copies.md`, so the
 *    operator can add that repository to `targets`.
 * 7. Same-job pairs and the follow-the-code sentence are not found here, and no model is called:
 *    `skills.jsonl` holds each copy with its whole description (`readerSkills`), and no command
 *    reads it. The `read` command's `same-job` and `sentence` readers take an input file of their
 *    own shape (readers.ts's `SameJobInput` and `SentenceInput`).
 *
 * Output under `--out`. Each `.tsv` starts with a row of its column names; no cell holds a tab or
 * a line break (`tsvCell` writes one as `\t`, `\n` or `\r`, and a backslash as `\\`; `parseTsv`
 * reads them back). An empty path cell is written `.` (the repository's root).
 *   skill-copies.tsv  one row per copy, columns `SKILL_COPIES_COLUMNS`, in this order: repository,
 *                     path (the skill's directory), name, description_bytes (UTF-8 bytes of the
 *                     frontmatter description), fork, archived (yes or no), parent (a fork's),
 *                     installers (`<action> <unit> (<repository>:<manifest path>)`, joined by
 *                     `; `), loaded_by (each file that loads the skill: `<repository>:<path>
 *                     (<how>)`, joined by `; `). attribute.ts reads these columns in this order and
 *                     adds its reads columns after them.
 *   prefix-table.tsv  `PREFIX_TABLE_COLUMNS`: prefix (a pattern: `**\/` at its start is any
 *                     leading directories, `*` is one path segment's text; `prefixMatches` is the
 *                     one matcher), repository, path, name, via (the unit and manifest it comes
 *                     from).
 *   repositories.tsv  `REPOSITORIES_COLUMNS`: each repository read, its commit, fork and archived
 *                     marks, how its tree was read, its skills, or the error.
 *   loaders.tsv       `LOADERS_COLUMNS`: each file outside a skill that names a skill: repository,
 *                     path, skill, match (the kinds of mention, `MATCH_KINDS`, or the manifest kind
 *                     that names it), source.
 *   installers.tsv    `INSTALLERS_COLUMNS`: each installer row, one per manifest, unit and copy.
 *   manifests.tsv     `MANIFESTS_COLUMNS`: each manifest file read, its kind and what it says.
 *   queries.jsonl     every search query (and every fork scan) with its counts, hits, failures.
 *   skills.jsonl      `readerSkills`: each copy with its whole description; no command reads it.
 *   skill-copies.md   the repositories read and not listable, the shared names, the loaders and the
 *                     installers, the prefix rows outside the targets, and every failure.
 */
import { createHash } from "node:crypto";
import {
  closeSync,
  createReadStream,
  existsSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
  writeSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { parseArgs } from "node:util";
import { z } from "zod";
import { mapLimit } from "../skill-scenarios/production-dispatch";
import { schemaFaults } from "./common";
import { loadConfig, type SkillCutsConfig } from "./config";
import { procPeakRssBytes, startPeakRssSampling } from "./memory";

// ---------------------------------------------------------------------------------------------
// gh

/** What one gh call returned. */
export interface GhResult {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** Runs gh with `args`; the test injects a stand-in. */
export type Gh = (args: readonly string[]) => Promise<GhResult>;

/** The pod's own gh, with the environment this process has. */
export const spawnGh: Gh = async (args) => {
  const child = Bun.spawn(["gh", ...args], { stdin: "ignore", stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout, stderr };
};

/** What `main` uses of the world. */
export interface CopiesIo {
  readonly gh: Gh;
  sleep(ms: number): Promise<void>;
  /** Epoch milliseconds. */
  now(): number;
  out(text: string): void;
  /** One progress or refusal line. */
  err(text: string): void;
  /** The process's peak resident memory so far, in bytes; memory.ts's `procPeakRssBytes` when
   *  absent (cli.ts's `skill-cuts copies`, whose io already samples it). */
  peakRssBytes?(): number;
}

/** The peak resident memory above which a run stops: it would crowd the pod's other runs. */
export const MEMORY_LIMIT_BYTES = 1.5 * 1024 ** 3;

/** A run stopped because its peak resident memory passed MEMORY_LIMIT_BYTES. */
export class MemoryStop extends Error {
  constructor(readonly peakBytes: number) {
    super(
      `its resident memory reached ${Math.round(peakBytes / 1024 ** 2)} MiB, above the limit of ${MEMORY_LIMIT_BYTES / 1024 ** 2} MiB`
    );
  }
}

/** Throws `error` again when it is a MemoryStop: a step that records its own failures and goes
 *  on must not record the run's stop as one of them. */
function rethrowStop(error: unknown): void {
  if (error instanceof MemoryStop) throw error;
}

const RATE_LIMITED = /rate limit|HTTP 429/i;
/** GitHub's refusal for its secondary rate limit, which has no reset to wait for (`rateWait`). */
const SECONDARY_LIMITED = /secondary rate limit/i;
/** The first wait after a refusal for the secondary rate limit, doubled at each refusal after. */
const SECONDARY_WAIT_MS = 60_000;
const MAX_RATE_RETRIES = 5;
const REST_JOBS = 4;
/** The repositories read at once, each its commit, tree and blobs, the slower of them sharing the
 *  read slots below with the rest. */
export const REPOSITORY_JOBS = 6;
/** The most gh calls reading repositories (a commit, a tree or its subtrees, a blob or a batch of
 *  blobs) or searching code in flight at once, across a run and per repository, so one large
 *  repository's walk does not hold every slot. Each call is a gh process of its own, about 110 MB
 *  resident in the pod whatever it reads (measured 2026-11-20), so the caps are what the run costs
 *  the pod beside its own memory. The code searches run beside the tree reads (`SearchQueues`),
 *  so each takes a slot too, keyed by its owner (one search of an owner at a time): the gh calls
 *  in flight stay at most READ_CALLS however the searches and the reads overlap, and a search,
 *  whose key holds no slot while it waits, takes the next slot a read frees (`ReadSlots`). */
export const READ_CALLS = 8;
export const READ_CALLS_PER_REPOSITORY = 4;
/** The most subtrees one GraphQL tree read asks for. */
const TREE_BATCH = 50;
/** The most blobs, and bytes of blobs, one GraphQL read asks for (build.ts's profile reads take
 *  the same). */
export const BLOB_BATCH = 50;
export const BLOB_BATCH_BYTES = 2_000_000;

function firstLine(text: string): string {
  return text.trim().split("\n")[0] ?? "";
}

/** `text` as JSON, or undefined when it is none. */
export function parseJson(text: string): unknown {
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return undefined;
  }
}

// GitHub's answers, parsed at the boundary: only the fields this file reads.
const Listing = z.array(
  z.looseObject({
    nameWithOwner: z.string(),
    isFork: z.boolean(),
    isArchived: z.boolean(),
    defaultBranchRef: z.looseObject({ name: z.string() }).nullable(),
    parent: z
      .looseObject({ name: z.string(), owner: z.looseObject({ login: z.string() }) })
      .nullable(),
  })
);
const Commits = z.array(
  z.looseObject({
    sha: z.string(),
    commit: z.looseObject({ tree: z.looseObject({ sha: z.string() }) }),
  })
);
/** gh's jq filter for a tree read: the answer's `truncated` flag, then one JSON array per entry,
 *  `[path, mode, type, sha, size]` (size -1 for a tree). gh reads the answer's JSON, which can be
 *  megabytes, so this process never holds it whole. */
const TREE_JQ =
  "(.truncated // false | tostring), (.tree[] | [.path, .mode, .type, .sha, (.size // -1)] | @json)";
const TreeLine = z.tuple([z.string(), z.string(), z.string(), z.string(), z.number()]);
/** A blob as `gh api repos/<r>/git/blobs/<sha>` gives it. */
export const GitBlob = z.looseObject({ content: z.string(), encoding: z.string() });
const ContentsFile = z.looseObject({ type: z.string(), sha: z.string() });
const SearchAnswer = z.looseObject({
  total_count: z.number(),
  incomplete_results: z.boolean(),
  items: z.array(
    z.looseObject({ path: z.string(), repository: z.looseObject({ full_name: z.string() }) })
  ),
});
const RateLimit = z.looseObject({
  resources: z.record(z.string(), z.looseObject({ reset: z.number() })),
});
const GithubUser = z.looseObject({ type: z.string() });
/** GitHub's answer to a GraphQL read of blobs by id (`blobQuery`): field `b<n>` the n-th blob,
 *  null where GitHub gives none; a blob's text null when it is binary or cut short. */
export const BlobBatch = z.looseObject({
  data: z.looseObject({
    repository: z
      .record(
        z.string(),
        z
          .looseObject({
            isBinary: z.boolean().nullable(),
            isTruncated: z.boolean(),
            text: z.string().nullable(),
          })
          .nullable()
      )
      .nullable(),
  }),
});
const FileBatch = z.looseObject({
  data: z.looseObject({
    repository: z
      .record(
        z.string(),
        z
          .looseObject({
            oid: z.string(),
            isBinary: z.boolean().nullable(),
            isTruncated: z.boolean(),
            text: z.string().nullable(),
          })
          .nullable()
      )
      .nullable(),
  }),
});

/** A file read by its path at a commit: its blob and text (undefined for a binary file), not at
 *  that path, or a read that failed. */
export type FileRead =
  | { readonly kind: "file"; readonly oid: string; readonly text: string | undefined }
  | { readonly kind: "missing" }
  | { readonly kind: "failed"; readonly error: string };

const TreeBatch = z.looseObject({
  data: z.looseObject({
    repository: z
      .record(
        z.string(),
        z
          .looseObject({
            entries: z.array(
              z.looseObject({
                name: z.string(),
                type: z.string(),
                mode: z.number(),
                oid: z.string(),
                size: z.number().nullish(),
              })
            ),
          })
          .nullable()
      )
      .nullable(),
  }),
});

const CachedEntry = z.object({
  path: z.string(),
  mode: z.string(),
  type: z.string(),
  sha: z.string(),
  size: z.number().optional(),
});
/** A repository read whole, as `--cache-dir` keeps it: its kept tree and the texts of its SKILL.md
 *  and manifest files (`blobs`). A fork's text files are kept apart (`forkFilesApart`, the lines
 *  of `fork-files`, `[path, blob id, text or null]`), read back one at a time by its scan. A file
 *  written before that holds them in `blobs` too, and one written before forks were compared
 *  with their parents has no `forkRead` or `changed`. */
const CachedRepository = z.object({
  commit: z.string(),
  treeEntries: z.number(),
  treeRead: z.string(),
  treeCalls: z.number(),
  tree: z.array(CachedEntry),
  blobs: z.record(z.string(), z.string()),
  forkRead: z.string().optional(),
  changed: z.array(z.string()).optional(),
  forkFilesApart: z.boolean().optional(),
});
type CachedRepository = z.infer<typeof CachedRepository>;
/** One level a truncated tree's walk finished, as `--cache-dir` keeps it (`readTree`): the entries
 *  it kept, the subtrees of the next level, how many entries it held, and the gh calls it took
 *  (the first level's with the whole read before it). */
const CachedLevel = z.object({
  kept: z.array(CachedEntry),
  subtrees: z.array(z.object({ sha: z.string(), prefix: z.string() })),
  count: z.number(),
  calls: z.number(),
});
type CachedLevel = z.infer<typeof CachedLevel>;
/** One line of a fork's text files on disk: its path, blob id, and text (null when its read
 *  failed, so its scan reads it again). */
const ForkFileLine = z.tuple([z.string(), z.string(), z.string().nullable()]);
/** A search run whole, as `--cache-dir` keeps it. */
const CachedSearch = z.object({
  status: z.literal("ok"),
  total_count: z.number(),
  incomplete_results: z.boolean(),
  pages: z.number(),
  calls: z.number(),
  rate_limit_retries: z.number(),
  items_read: z.number(),
  unread: z.number(),
  hits: z.array(z.object({ repository: z.string(), path: z.string() })),
});

/**
 * The finished reads of a run (`--cache-dir`), so that a run started again skips them: each value
 * is one JSON file under `<dir>/<kind>/`, named by the SHA-1 of its key and holding the key, which
 * is checked on reading. A value is written to a temporary name and renamed, so a killed write
 * leaves no file under a name this reads; a file that is not of its shape is not read. A run with
 * no `--cache-dir` keeps its reads in a temporary directory, removed when the run ends: the run
 * reads a fork's text files back from disk, never holding them (`lines`).
 */
class ReadCache {
  private writes = 0;
  private readonly dir: string;
  private readonly temporary: boolean;

  constructor(dir: string | undefined) {
    this.temporary = dir === undefined;
    this.dir = dir ?? mkdtempSync(path.join(tmpdir(), "skill-copies-"));
  }

  /** Removes the directory of a run with no `--cache-dir`. */
  dispose(): void {
    if (this.temporary) rmSync(this.dir, { recursive: true, force: true });
  }

  private file(kind: string, key: string, extension = ".json"): string {
    const name = createHash("sha1").update(key).digest("hex");
    return path.join(this.dir, kind, `${name}${extension}`);
  }

  get<T>(kind: string, key: string, schema: z.ZodType<T>): T | undefined {
    let text: string;
    try {
      text = readFileSync(this.file(kind, key), "utf8");
    } catch {
      return undefined;
    }
    const parsed = z.object({ key: z.literal(key), value: schema }).safeParse(parseJson(text));
    return parsed.success ? parsed.data.value : undefined;
  }

  put(kind: string, key: string, value: unknown): void {
    const file = this.file(kind, key);
    mkdirSync(path.dirname(file), { recursive: true });
    const temporary = `${file}.tmp-${process.pid}-${this.writes++}`;
    writeFileSync(temporary, JSON.stringify({ key, value }));
    renameSync(temporary, file);
  }

  /** A writer of the lines of `kind` and `key`, one JSON value a line after a first line holding
   *  the key; the lines are found under their name only once `close` renames them there, and
   *  `abandon` removes them. */
  lineWriter(
    kind: string,
    key: string
  ): { write(line: string): void; close(): void; abandon(): void } {
    const file = this.file(kind, key, ".jsonl");
    mkdirSync(path.dirname(file), { recursive: true });
    const temporary = `${file}.tmp-${process.pid}-${this.writes++}`;
    const fd = openSync(temporary, "w");
    writeSync(fd, `${JSON.stringify({ key })}\n`);
    return {
      write: (line) => {
        writeSync(fd, `${line}\n`);
      },
      close: () => {
        closeSync(fd);
        renameSync(temporary, file);
      },
      abandon: () => {
        closeSync(fd);
        rmSync(temporary, { force: true });
      },
    };
  }

  /** Whether the lines of `kind` and `key` are on disk. */
  hasLines(kind: string, key: string): boolean {
    return existsSync(this.file(kind, key, ".jsonl"));
  }

  /** The lines `lineWriter` wrote for `kind` and `key`, read from disk one at a time (none when
   *  their first line names another key), or undefined when they are not on disk. */
  lines(kind: string, key: string): AsyncGenerator<string> | undefined {
    let fd: number;
    try {
      fd = openSync(this.file(kind, key, ".jsonl"), "r");
    } catch (error) {
      if (Reflect.get(Object(error), "code") === "ENOENT") return undefined;
      throw error;
    }
    return this.keyedLines(fd, key);
  }

  private async *keyedLines(fd: number, key: string): AsyncGenerator<string> {
    const reader = createInterface({ input: createReadStream("", { fd }), crlfDelay: Infinity });
    let first = true;
    for await (const line of reader) {
      if (first) {
        first = false;
        const head = z.object({ key: z.literal(key) }).safeParse(parseJson(line));
        if (!head.success) return;
        continue;
      }
      if (line !== "") yield line;
    }
  }
}

/** The read slots of a run: at most `total` gh calls in flight, at most `perRepository` of one
 *  repository's. A call that ends hands its slot to the waiting call whose repository holds the
 *  fewest slots (the first of them to wait among equals), so a small repository's few calls pass
 *  the queue of a large tree's walk rather than waiting behind every subtree of its level. */
export class ReadSlots {
  private taken = 0;
  private readonly heldBy = new Map<string, number>();
  private readonly waiting: { repository: string; start: () => void }[] = [];

  constructor(
    private readonly total: number,
    private readonly perRepository: number
  ) {}

  private held(repository: string): number {
    return this.heldBy.get(repository) ?? 0;
  }

  private mayStart(repository: string): boolean {
    return this.taken < this.total && this.held(repository) < this.perRepository;
  }

  private count(repository: string, by: number): void {
    this.taken += by;
    this.heldBy.set(repository, this.held(repository) + by);
  }

  /** `read`, a gh call reading `repository`, run once it may hold a slot. */
  async run<T>(repository: string, read: () => Promise<T>): Promise<T> {
    if (this.mayStart(repository)) this.count(repository, 1);
    else {
      const slot = Promise.withResolvers<void>();
      this.waiting.push({ repository, start: slot.resolve });
      await slot.promise;
    }
    try {
      return await read();
    } finally {
      this.count(repository, -1);
      let next = -1;
      for (const [index, candidate] of this.waiting.entries()) {
        if (!this.mayStart(candidate.repository)) continue;
        const best = this.waiting[next];
        if (best === undefined || this.held(candidate.repository) < this.held(best.repository))
          next = index;
      }
      const [chosen] = next < 0 ? [] : this.waiting.splice(next, 1);
      if (chosen !== undefined) {
        this.count(chosen.repository, 1);
        chosen.start();
      }
    }
  }
}

/** gh's reads, with a sleep to the reset when the core limit is spent and the run's memory limit
 *  checked before each call. It keeps no blob text: each read is handed to its caller. */
class Github {
  private readonly slots = new ReadSlots(READ_CALLS, READ_CALLS_PER_REPOSITORY);
  /** The largest peak resident memory read so far, in bytes. */
  peakBytes = 0;
  constructor(
    readonly io: CopiesIo,
    private readonly peakRss: () => number
  ) {}

  /** The peak resident memory so far, in bytes, read again now. */
  readPeak(): number {
    this.peakBytes = Math.max(this.peakBytes, this.peakRss());
    return this.peakBytes;
  }

  /** Reads the peak resident memory; throws MemoryStop above MEMORY_LIMIT_BYTES. */
  checkMemory(): void {
    if (this.readPeak() > MEMORY_LIMIT_BYTES) throw new MemoryStop(this.peakBytes);
  }

  /** gh with `args`, once the memory limit holds. */
  call(args: readonly string[]): Promise<GhResult> {
    this.checkMemory();
    return this.io.gh(args);
  }

  /** `read`, a gh call reading `repository` (or a code search, by its owner's key), in one of the
   *  run's read slots (`ReadSlots`). */
  inSlot<T>(repository: string, read: () => Promise<T>): Promise<T> {
    return this.slots.run(repository, read);
  }

  /** gh's answer to a GraphQL read `send` makes, sent again while GitHub refuses it for its rate
   *  limit (RATE_LIMITED), after the wait `rateWait` gives, at most MAX_RATE_RETRIES times: a
   *  batch halved, or read alone over REST, would spend the limit faster. Any other refusal is its
   *  caller's to handle. */
  async graphql(send: () => Promise<GhResult>): Promise<GhResult> {
    for (let attempt = 0; ; attempt++) {
      const result = await send();
      if (result.code === 0 || attempt >= MAX_RATE_RETRIES || !RATE_LIMITED.test(result.stderr))
        return result;
      await this.io.sleep(await this.rateWait(result.stderr, attempt, "graphql"));
    }
  }

  /** The milliseconds to wait before a call GitHub refused for its rate limit is sent again, at
   *  its `attempt`-th refusal (from 0), whose error is `stderr`. GitHub's secondary rate limit has
   *  no reset: GitHub asks a client to wait its `retry-after` (which gh shows only with `-i`, so
   *  never for these calls), else at least a minute, and to back off; so the wait is
   *  SECONDARY_WAIT_MS, doubled at each refusal. A primary limit is waited out to `resource`'s
   *  reset (`waitForReset`). */
  private async rateWait(
    stderr: string,
    attempt: number,
    resource: "core" | "graphql"
  ): Promise<number> {
    if (!SECONDARY_LIMITED.test(stderr)) return this.waitForReset(resource);
    const wait = SECONDARY_WAIT_MS * 2 ** attempt;
    this.io.err(`copies: GitHub's secondary rate limit refused a call; waiting ${wait / 1000} s`);
    return wait;
  }

  /** gh's standard output for `args`, sent again after the wait `rateWait` gives while GitHub
   *  refuses it for its rate limit, at most MAX_RATE_RETRIES times; throws gh's first error
   *  line. */
  async text(args: readonly string[]): Promise<string> {
    for (let attempt = 0; ; attempt++) {
      const result = await this.call(args);
      if (result.code === 0) return result.stdout;
      if (attempt < MAX_RATE_RETRIES && RATE_LIMITED.test(result.stderr)) {
        await this.io.sleep(await this.rateWait(result.stderr, attempt, "core"));
        continue;
      }
      throw new Error(firstLine(result.stderr) || `gh exited ${result.code}`);
    }
  }

  /** gh's answer to `args`, parsed by `schema`; throws gh's first error line, or the faults. */
  async json<T>(args: readonly string[], schema: z.ZodType<T>): Promise<T> {
    const parsed = schema.safeParse(parseJson(await this.text(args)));
    if (!parsed.success)
      throw new Error(`gh's answer is not of its shape: ${schemaFaults(parsed.error)}`);
    return parsed.data;
  }

  api<T>(endpoint: string, schema: z.ZodType<T>): Promise<T> {
    return this.json(["api", endpoint], schema);
  }

  /** The milliseconds to the reset `gh api rate_limit` gives for `resource`, at most an hour; a
   *  minute when it gives none or one already past. */
  async waitForReset(resource: "core" | "code_search" | "graphql"): Promise<number> {
    const result = await this.call(["api", "rate_limit"]);
    const parsed = RateLimit.safeParse(result.code === 0 ? parseJson(result.stdout) : undefined);
    const reset = parsed.success ? parsed.data.resources[resource]?.reset : undefined;
    const until = reset === undefined ? 0 : reset * 1000 + 1000;
    const wait = until > this.io.now() ? Math.min(until - this.io.now(), 3_600_000) : 60_000;
    this.io.err(`copies: the ${resource} rate limit is spent; waiting ${Math.ceil(wait / 1000)} s`);
    return wait;
  }

  /** The text of the blob `sha` of `repository`, read alone over REST and kept nowhere. */
  async readBlob(repository: string, sha: string): Promise<string> {
    const blob = await this.inSlot(repository, () =>
      this.api(`repos/${repository}/git/blobs/${sha}`, GitBlob)
    );
    if (blob.encoding !== "base64")
      throw new Error(`the blob is encoded ${blob.encoding}, not base64`);
    return Buffer.from(blob.content, "base64").toString("utf8");
  }

  /** Hands each of the blobs `entries` of `repository` to `each`, in their order, with its text or
   *  the error that kept it from being read, and keeps none: a caller parses or stores each and
   *  drops it. They are read in GraphQL batches (`graphqlArgs`), up to BLOB_BATCH blobs and
   *  BLOB_BATCH_BYTES bytes a call and one batch at a time, so at most one batch's texts are held
   *  at once; a blob a batch does not give whole (binary, truncated, missing, or in a batch that
   *  fails for another reason than the rate limit, `graphql`) is read alone. Entries of one blob
   *  id are read once. */
  async readBlobs(
    repository: string,
    entries: readonly TreeEntry[],
    each: (entry: TreeEntry, read: { text: string } | { error: string }) => void
  ): Promise<void> {
    const bySha = new Map<string, TreeEntry[]>();
    for (const entry of entries) bySha.set(entry.sha, [...(bySha.get(entry.sha) ?? []), entry]);
    const firsts = [...bySha.values()].flatMap(([first]) => (first === undefined ? [] : [first]));
    for (const batch of blobBatches(firsts, BLOB_BATCH, BLOB_BATCH_BYTES)) {
      let given: z.infer<typeof BlobBatch>["data"]["repository"] | undefined;
      // A blob id that is not 40 hex characters goes in no query: its batch is read alone.
      if (batch.every((entry) => /^[0-9a-f]{40}$/.test(entry.sha))) {
        const result = await this.graphql(() =>
          this.inSlot(repository, () => this.call(graphqlArgs(repository, blobQuery(batch))))
        );
        const parsed = BlobBatch.safeParse(
          result.code === 0 ? parseJson(result.stdout) : undefined
        );
        if (parsed.success) given = parsed.data.data.repository;
        else {
          const why = firstLine(result.stderr) || "an answer not of its shape";
          this.io.err(
            `copies: a batch of ${batch.length} blobs of ${repository} failed (${why}); each is read alone`
          );
        }
      }
      for (const [index, entry] of batch.entries()) {
        const blob = given?.[`b${index}`];
        const read =
          blob?.isBinary === false && !blob.isTruncated && blob.text !== null
            ? { text: blob.text }
            : await this.readAlone(repository, entry.sha);
        for (const same of bySha.get(entry.sha) ?? []) each(same, read);
      }
    }
  }

  private async readAlone(
    repository: string,
    sha: string
  ): Promise<{ text: string } | { error: string }> {
    try {
      return { text: await this.readBlob(repository, sha) };
    } catch (error) {
      rethrowStop(error);
      return { error: (error as Error).message };
    }
  }

  /** The files `paths` of `repository` at `commit`, read by path in one GraphQL batch (routed as
   *  `readBlobs` is) and cached nowhere: a caller checks each and drops it. A file the batch does
   *  not give whole (truncated, or a batch that fails for another reason than the rate limit,
   *  `graphql`) is read alone over REST. */
  async filesAt(
    repository: string,
    commit: string,
    paths: readonly string[]
  ): Promise<Map<string, FileRead>> {
    const fields = paths
      .map(
        (file, index) =>
          `b${index}: object(expression: ${JSON.stringify(`${commit}:${file}`)}) { ... on Blob { oid isBinary isTruncated text } }`
      )
      .join(" ");
    const query = `query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { ${fields} } }`;
    const result = await this.graphql(() => this.call(graphqlArgs(repository, query)));
    const parsed = FileBatch.safeParse(result.code === 0 ? parseJson(result.stdout) : undefined);
    if (!parsed.success) {
      const why = firstLine(result.stderr) || "an answer not of its shape";
      this.io.err(
        `copies: a batch of ${paths.length} files of ${repository} failed (${why}); each is read alone`
      );
    }
    const found = new Map<string, FileRead>();
    for (const [index, file] of paths.entries()) {
      const blob = parsed.success ? parsed.data.data.repository?.[`b${index}`] : undefined;
      if (parsed.success && (blob === null || blob === undefined))
        found.set(file, { kind: "missing" });
      else if (blob?.isBinary === true)
        found.set(file, { kind: "file", oid: blob.oid, text: undefined });
      else if (blob !== undefined && blob !== null && !blob.isTruncated && blob.text !== null)
        found.set(file, { kind: "file", oid: blob.oid, text: blob.text });
      else found.set(file, await this.fileAlone(repository, commit, file, blob?.oid));
    }
    return found;
  }

  /** The file `file` of `repository` at `commit`, read alone: its blob `oid` when a batch gave it,
   *  else the contents API's (`repos/<r>/contents/<path>?ref=<commit>`, 404 when it is not
   *  there). */
  private async fileAlone(
    repository: string,
    commit: string,
    file: string,
    oid: string | undefined
  ): Promise<FileRead> {
    try {
      let sha = oid;
      if (sha === undefined) {
        const encoded = file.split("/").map(encodeURIComponent).join("/");
        const answer = await this.api(
          `repos/${repository}/contents/${encoded}?ref=${commit}`,
          ContentsFile
        );
        if (answer.type !== "file") return { kind: "failed", error: `${file} is a ${answer.type}` };
        sha = answer.sha;
      }
      const text = await this.readBlob(repository, sha);
      return { kind: "file", oid: sha, text: text.includes("\u0000") ? undefined : text };
    } catch (error) {
      rethrowStop(error);
      const message = (error as Error).message;
      return /\(HTTP 404\)/.test(message)
        ? { kind: "missing" }
        : { kind: "failed", error: message };
    }
  }
}

/** gh's arguments for a GraphQL `query` over `repository`, whose owner and name go in as the
 *  fields `owner` and `name` (the query's variables): a `gh api graphql` call names no repository,
 *  and the pod's gh routes one by its `owner` field, so the call takes that owner's token (GH_REPO
 *  does not route it: gh reads GH_REPO only for a placeholder). */
export function graphqlArgs(repository: string, query: string): string[] {
  const [owner = "", name = ""] = repository.split("/");
  return ["api", "graphql", "-f", `owner=${owner}`, "-f", `name=${name}`, "-f", `query=${query}`];
}

/** `blobs` in batches, in their order: each of at most `maxCount` blobs and, by their sizes, at
 *  most `maxBytes` bytes, but that a blob larger than that is a batch alone. Each caller checks
 *  its blob ids itself: a query (`blobQuery`) takes only ids of 40 hex characters. */
export function blobBatches<E extends Pick<TreeEntry, "sha" | "size">>(
  blobs: readonly E[],
  maxCount: number,
  maxBytes: number
): E[][] {
  const batches: E[][] = [];
  let bytes = 0;
  for (const blob of blobs) {
    const last = batches.at(-1);
    if (last === undefined || last.length >= maxCount || bytes + (blob.size ?? 0) > maxBytes) {
      batches.push([blob]);
      bytes = blob.size ?? 0;
    } else {
      last.push(blob);
      bytes += blob.size ?? 0;
    }
  }
  return batches;
}

/** The GraphQL query that reads the blobs `batch` by their ids, the n-th as the field `b<n>`
 *  (BlobBatch), over the repository the call's `owner` and `name` fields name (`graphqlArgs`). */
export function blobQuery(batch: readonly Pick<TreeEntry, "sha">[]): string {
  const fields = batch
    .map(
      (entry, index) =>
        `b${index}: object(oid: "${entry.sha}") { ... on Blob { isBinary isTruncated text } }`
    )
    .join(" ");
  return `query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { ${fields} } }`;
}

// ---------------------------------------------------------------------------------------------
// paths

/** A repository path from parts: normalized, no leading `./`, and `""` for the root. */
export function joinPath(...parts: string[]): string {
  const joined = path.posix.normalize(path.posix.join(...parts.filter((part) => part !== "")));
  const trimmed = joined.replace(/\/+$/, "").replace(/^\.\//, "");
  return trimmed === "." ? "" : trimmed;
}

/** A path's directory, `""` for the root. */
function dirOf(file: string): string {
  const dir = path.posix.dirname(file);
  return dir === "." ? "" : dir;
}

/** A repository path as the output writes it: `.` for the root. */
function shown(dir: string): string {
  return dir === "" ? "." : dir;
}

/** A repository path as the output writes it (`shown`), as a path: `""` for the root. */
export function repoPath(written: string): string {
  return written === "." ? "" : written;
}

function unquote(text: string): string {
  const match = /^(["'])(.*)\1$/s.exec(text);
  return match === null ? text : (match[2] ?? "");
}

/** A list of strings given as a list, one string, or a YAML-style flow list in one string. */
function strings(value: unknown): string[] {
  if (Array.isArray(value)) return value.filter((item): item is string => typeof item === "string");
  if (typeof value !== "string" || value.trim() === "") return [];
  const flow = /^\[(.*)\]$/s.exec(value.trim());
  if (flow === null) return [value.trim()];
  return (flow[1] ?? "")
    .split(",")
    .map((item) => unquote(item.trim()))
    .filter((item) => item !== "");
}

// ---------------------------------------------------------------------------------------------
// repositories and trees

/** One entry of a git tree, as GitHub's trees API gives it. */
export interface TreeEntry {
  readonly path: string;
  readonly mode: string;
  readonly type: string;
  readonly sha: string;
  readonly size?: number;
}

/** A repository the listing gave, and what was read of it. */
export interface Repository {
  readonly repository: string;
  readonly owner: string;
  readonly fork: boolean;
  readonly archived: boolean;
  readonly parent: string;
  readonly branch: string;
  commit: string;
  /** The entries of its tree this list uses (`keepsEntry`; the rest are counted, not kept), while
   *  its step runs: once it is done they are on disk (the cache), and this is empty. */
  tree: TreeEntry[];
  /** How many entries its whole tree holds. */
  treeEntries: number;
  /** `whole`, or `by directory, <n> reads` for a truncated tree. */
  treeRead: string;
  /** In a fork holding a skill, which files its scan reads (`forkChanges`); else `""`. */
  forkRead: string;
  /** The paths of the files that fork changed against its parent, or undefined when it is read
   *  from its whole tree. */
  changed: string[] | undefined;
  error: string;
}

/** The version of the rules `keepsEntry` keeps by. A walk's saved levels (`readTree`) hold only the
 *  entries the rules of the run that saved them kept, so the key of each holds this version, and a
 *  level saved under other rules is read again. Bump it with every change to what `keepsEntry`
 *  keeps, `manifestKind`, MANIFEST_KINDS, TEXT_EXTENSIONS and TEXT_NAMES included. */
export const KEEP_RULES_VERSION = 1;

/** Whether a tree entry is one this list uses, of a fork's tree when `fork`: a `SKILL.md`, a file
 *  of a manifest kind, or, in a fork, a text file its scan may read. Every other entry is
 *  counted and dropped as the tree is read: a data repository's tree holds hundreds of thousands
 *  of entries, and a hit file is read by its path, not looked up in the tree. A change to what it
 *  keeps bumps KEEP_RULES_VERSION, so no walk level saved under the old rules is replayed. */
export function keepsEntry(
  entry: Pick<TreeEntry, "path" | "mode" | "type">,
  fork: boolean
): boolean {
  if (entry.type !== "blob") return false;
  const name = path.posix.basename(entry.path);
  return (
    name === "SKILL.md" ||
    manifestKind(entry) !== undefined ||
    (fork &&
      entry.mode !== "120000" &&
      (Object.hasOwn(TEXT_EXTENSIONS, path.posix.extname(entry.path).toLowerCase()) ||
        Object.hasOwn(TEXT_NAMES, name)))
  );
}

/** A subtree a walk reads: its id and its path in the repository. */
interface Subtree {
  readonly sha: string;
  readonly prefix: string;
}

/** What outlives one read of tree entries: the entries `readTree`'s `keep` keeps, how many entries
 *  the read gave, and, for a read of one level, its subtrees, which the walk reads next. */
interface TreePart {
  readonly kept: TreeEntry[];
  readonly subtrees: Subtree[];
  count: number;
}

/** `part` added after what `into` holds. */
function appendPart(into: TreePart, part: TreePart): void {
  into.count += part.count;
  for (const entry of part.kept) into.kept.push(entry);
  for (const subtree of part.subtrees) into.subtrees.push(subtree);
}

/** Where a walk keeps each level it finishes, and finds it again (`readTree`). */
interface WalkLevels {
  get(level: number): CachedLevel | undefined;
  put(level: number, saved: CachedLevel): void;
}

/** The tree `sha` of `repository`: the entries `keep` keeps, and how many it holds. The whole tree
 *  is asked for in one REST read (through gh's jq, TREE_JQ, one line per entry). A tree GitHub
 *  answers truncated, or whose whole read fails (gh cut off the answer of a large tree: "unexpected
 *  end of JSON input"), is walked a level at a time: each level's subtrees are read in GraphQL
 *  batches of up to TREE_BATCH (`object(oid:)` fields, one level of entries each, routed as
 *  `graphqlArgs` says), READ_CALLS_PER_REPOSITORY batches at once under the read slots
 *  (`Github.inSlot`). A batch GitHub refuses (a node limit, a timeout) is halved, and a subtree
 *  refused alone is read over REST, one level; a batch refused for the GraphQL rate limit is sent
 *  again after the wait `Github.graphql` gives instead. The entries and their count are the same
 *  whichever way a subtree is read. Each read keeps, as it parses its answer, only the entries
 *  `keep` keeps, the subtrees of a one-level read and a count of the entries: a data repository's
 *  tree holds millions of entries, so no read and no level holds them all, and a walk holds only
 *  the kept entries and the subtrees of its next level. Each level the walk finishes goes to
 *  `levels` (its kept entries, the next level's subtrees, its count and its gh calls), and a walk
 *  that finds its levels there replays them and reads on from the first it does not find, with
 *  no whole read once the first is there: the same entries, order, count and `reads`, the gh
 *  calls, those of the replayed levels included. */
export async function readTree(
  github: Github,
  repository: string,
  sha: string,
  keep: (entry: TreeEntry) => boolean,
  levels: WalkLevels
): Promise<{ entries: TreeEntry[]; count: number; reads: number; truncated: boolean }> {
  let reads = 0;
  /** Counts `entry` into `part`, keeping it when `keep` does, and its subtree in a one-level
   *  read. */
  const take = (part: TreePart, entry: TreeEntry, oneLevel: boolean): void => {
    part.count++;
    if (keep(entry)) part.kept.push(entry);
    if (oneLevel && entry.type === "tree") {
      part.subtrees.push({ sha: entry.sha, prefix: entry.path });
    }
  };
  /** A tree's entries over REST, the whole tree when `recursive`, else one level. */
  const restRead = async (treeSha: string, prefix: string, recursive: boolean) => {
    reads++;
    const endpoint = `repos/${repository}/git/trees/${treeSha}${recursive ? "?recursive=1" : ""}`;
    const text = await github.inSlot(repository, () =>
      github.text(["api", endpoint, "--jq", TREE_JQ])
    );
    // The answer is read a line at a time, so an entry not kept outlives only its line.
    let end = text.indexOf("\n");
    const flag = end < 0 ? text : text.slice(0, end);
    if (flag !== "true" && flag !== "false") throw new Error("the tree answer is not of its shape");
    // A truncated whole tree is walked instead: its entries are not read.
    if (recursive && flag === "true")
      return { part: { kept: [], subtrees: [], count: 0 }, truncated: true };
    const part: TreePart = { kept: [], subtrees: [], count: 0 };
    while (end >= 0) {
      const start = end + 1;
      end = text.indexOf("\n", start);
      const line = end < 0 ? text.slice(start) : text.slice(start, end);
      if (line === "") continue;
      const parsed = TreeLine.safeParse(parseJson(line));
      if (!parsed.success)
        throw new Error(`a tree answer line is not of its shape: ${schemaFaults(parsed.error)}`);
      const [entryPath, mode, type, entrySha, size] = parsed.data;
      take(
        part,
        {
          path: prefix === "" ? entryPath : `${prefix}/${entryPath}`,
          mode,
          type,
          sha: entrySha,
          ...(size >= 0 ? { size } : {}),
        },
        !recursive
      );
    }
    return { part, truncated: flag === "true" };
  };
  /** One level of each of `trees`, in one GraphQL read; halved, then read over REST, where
   *  GitHub refuses it for another reason than its rate limit. */
  const levelOf = async (trees: readonly Subtree[]): Promise<TreePart> => {
    const fields = trees
      .map(
        (tree, index) =>
          `t${index}: object(oid: "${tree.sha}") { ... on Tree { entries { name type mode oid size } } }`
      )
      .join(" ");
    const query = `query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { ${fields} } }`;
    const result = await github.graphql(() => {
      reads++;
      return github.inSlot(repository, () => github.call(graphqlArgs(repository, query)));
    });
    const parsed = TreeBatch.safeParse(result.code === 0 ? parseJson(result.stdout) : undefined);
    const answers = parsed.success
      ? trees.map((_, index) => parsed.data.data.repository?.[`t${index}`])
      : [];
    const part: TreePart = { kept: [], subtrees: [], count: 0 };
    if (answers.length === trees.length && answers.every((answer) => answer != null)) {
      for (const [index, answer] of answers.entries()) {
        const prefix = trees[index]?.prefix ?? "";
        for (const entry of answer?.entries ?? []) {
          take(
            part,
            {
              path: prefix === "" ? entry.name : `${prefix}/${entry.name}`,
              // GraphQL gives the mode as a number; REST as its octal digits, six of them.
              mode: entry.mode.toString(8).padStart(6, "0"),
              type: entry.type,
              sha: entry.oid,
              ...(entry.type === "blob" && entry.size != null ? { size: entry.size } : {}),
            },
            true
          );
        }
      }
      return part;
    }
    if (trees.length > 1) {
      const half = Math.ceil(trees.length / 2);
      const halves = await Promise.all([levelOf(trees.slice(0, half)), levelOf(trees.slice(half))]);
      for (const each of halves) appendPart(part, each);
      return part;
    }
    const [tree] = trees;
    if (tree === undefined) return part;
    const one = await restRead(tree.sha, tree.prefix, false);
    if (one.truncated)
      throw new Error(`the tree of ${shown(tree.prefix)} is truncated even one level at a time`);
    return one.part;
  };
  // A walk whose first level an earlier run saved needs no whole read: that read was truncated, or
  // failed.
  let saved = levels.get(0);
  if (saved === undefined) {
    let whole: { part: TreePart; truncated: boolean } | undefined;
    try {
      whole = await restRead(sha, "", true);
    } catch (error) {
      rethrowStop(error);
    }
    if (whole !== undefined && !whole.truncated) {
      return { entries: whole.part.kept, count: whole.part.count, reads, truncated: false };
    }
  }
  // mapLimit gives a level's batches in their order: the kept entries keep one order every run.
  const walk: TreePart = { kept: [], subtrees: [{ sha, prefix: "" }], count: 0 };
  /** The gh calls counted into the levels so far. */
  let counted = 0;
  for (let depth = 0; walk.subtrees.length > 0; depth++) {
    const level = walk.subtrees.splice(0);
    if (depth > 0) saved = levels.get(depth);
    if (saved === undefined) {
      const batches: Subtree[][] = [];
      for (let start = 0; start < level.length; start += TREE_BATCH)
        batches.push(level.slice(start, start + TREE_BATCH));
      const part: TreePart = { kept: [], subtrees: [], count: 0 };
      for (const each of await mapLimit(batches, READ_CALLS_PER_REPOSITORY, levelOf)) {
        appendPart(part, each);
      }
      saved = { ...part, calls: reads - counted };
      levels.put(depth, saved);
    } else {
      reads += saved.calls;
    }
    counted = reads;
    appendPart(walk, saved);
  }
  return { entries: walk.kept, count: walk.count, reads, truncated: true };
}

/** Reads the commit of `repo` at `at` and its tree, a walk's levels kept in `cache` (keyed by
 *  repository, root tree, fork flag, KEEP_RULES_VERSION and level); returns the tree's gh calls.
 *  A read that fails sets `repo.error`. */
async function readRepository(
  github: Github,
  cache: ReadCache,
  repo: Repository,
  at: string
): Promise<number> {
  if (repo.branch === "") {
    repo.error = "the listing gives no default branch (an empty repository)";
    return 0;
  }
  let treeSha: string;
  try {
    const commits = await github.inSlot(repo.repository, () =>
      github.api(
        `repos/${repo.repository}/commits?sha=${encodeURIComponent(repo.branch)}&until=${encodeURIComponent(at)}&per_page=1`,
        Commits
      )
    );
    const first = commits[0];
    if (first === undefined) {
      repo.error = `no commit on ${repo.branch} at or before ${at}`;
      return 0;
    }
    repo.commit = first.sha;
    treeSha = first.commit.tree.sha;
  } catch (error) {
    rethrowStop(error);
    repo.error = `commit read failed: ${(error as Error).message}`;
    return 0;
  }
  try {
    const levelKey = (level: number) =>
      [repo.repository, treeSha, repo.fork, KEEP_RULES_VERSION, level].join("\0");
    const tree = await readTree(
      github,
      repo.repository,
      treeSha,
      (entry) => keepsEntry(entry, repo.fork),
      {
        get: (level) => cache.get("walk", levelKey(level), CachedLevel),
        put: (level, saved) => cache.put("walk", levelKey(level), saved),
      }
    );
    repo.tree = tree.entries;
    repo.treeEntries = tree.count;
    repo.treeRead = tree.truncated ? `by directory, ${tree.reads} reads` : "whole";
    return tree.reads;
  } catch (error) {
    rethrowStop(error);
    repo.error = `tree read failed: ${(error as Error).message}`;
    return 0;
  }
}

/** The jq filter a compare is read through: the merge base, how many files it lists, then one
 *  JSON array per file, `[filename, status, previous filename]`, so the answer's patches and
 *  commits never sit in this process. */
const COMPARE_JQ =
  '.merge_base_commit.sha, (.files // [] | length), ((.files // [])[] | [.filename, .status, (.previous_filename // "")] | @json)';
/** The most files GitHub lists of a compare, on its first page only, so a list this long may be
 *  cut. */
export const COMPARE_MAX_FILES = 300;
const ForkParent = z.looseObject({
  parent: z.looseObject({ full_name: z.string(), default_branch: z.string() }).nullish(),
});
const CompareFile = z.tuple([z.string(), z.string(), z.string()]);

/** Which files of the fork `repo` (holding a skill, its commit read) its scan reads: those it
 *  changed against its parent, from GitHub's compare of the parent's default branch with the
 *  fork's commit at --at, through their merge base (`repos/<fork>/compare/<parent owner>:<parent
 *  branch>...<commit>`, routed by the fork's owner). GitHub lists a compare's files on its first
 *  page only, so one page is read, with one commit (`per_page=1`). A removed file is not read; a
 *  renamed one is read under its new name. Where the compare cannot answer whole (no parent, or
 *  one gone or not readable, a compare that fails, or a list of COMPARE_MAX_FILES files) the
 *  whole tree is read, and `read` says why. */
async function forkChanges(
  github: Github,
  repo: Repository
): Promise<{ read: string; changed: string[] | undefined }> {
  const whole = (why: string) => ({ read: `whole tree: ${why}`, changed: undefined });
  if (repo.parent === "") return whole("the listing names no parent");
  let parent: { full_name: string; default_branch: string };
  try {
    const info = await github.inSlot(repo.repository, () =>
      github.api(`repos/${repo.repository}`, ForkParent)
    );
    if (info.parent == null) return whole(`its parent ${repo.parent} is gone or not readable`);
    parent = info.parent;
  } catch (error) {
    rethrowStop(error);
    return whole(`its parent could not be read (${(error as Error).message})`);
  }
  const owner = parent.full_name.split("/")[0] ?? "";
  const basehead = `${owner}:${encodeURIComponent(parent.default_branch)}...${repo.commit}`;
  let lines: string[];
  try {
    lines = (
      await github.inSlot(repo.repository, () =>
        github.text([
          "api",
          `repos/${repo.repository}/compare/${basehead}?per_page=1`,
          "--jq",
          COMPARE_JQ,
        ])
      )
    )
      .split("\n")
      .filter((line) => line !== "");
  } catch (error) {
    rethrowStop(error);
    return whole(`the compare with ${parent.full_name} failed (${(error as Error).message})`);
  }
  const [base = "", listed = "", ...files] = lines;
  if (!/^[0-9a-f]{40}$/.test(base) || !/^\d+$/.test(listed) || Number(listed) !== files.length)
    return whole(`the compare with ${parent.full_name} is not of its shape`);
  if (files.length >= COMPARE_MAX_FILES)
    return whole(
      `the compare with ${parent.full_name} lists ${files.length} files, the most GitHub lists, so its list may be cut`
    );
  const changed: string[] = [];
  for (const line of files) {
    const parsed = CompareFile.safeParse(parseJson(line));
    if (!parsed.success) return whole(`the compare with ${parent.full_name} is not of its shape`);
    const [filename, status] = parsed.data;
    if (status !== "removed") changed.push(filename);
  }
  return { read: `changed files against ${parent.full_name}@${base}`, changed: changed.sort() };
}

/** The files read of one repository (`repositoryFiles`). */
export interface RepositoryFiles {
  readonly skillFiles: TreeEntry[];
  readonly excluded: TreeEntry[];
  readonly manifests: { entry: TreeEntry; kind: ManifestKind }[];
}

/** The files read of the repository `repository` whose kept tree is `tree` (the entries
 *  `keepsEntry` keeps): its SKILL.md files outside a configured `excludePaths` prefix of it
 *  (`skillFiles`) and those in one (`excluded`), and its manifest files (`manifests`), each of a
 *  manifest kind (`manifestKind`) outside a skill, a skill link wherever it is, a `package.json`
 *  only in a repository that holds a SKILL.md, and an Oh My Pi extension only where a
 *  skills-sources.json is. build.ts reads a repository at a commit by these rules too. */
export function repositoryFiles(
  repository: string,
  tree: readonly TreeEntry[],
  excludePaths: SkillCutsConfig["excludePaths"]
): RepositoryFiles {
  const skillFiles: TreeEntry[] = [];
  const excluded: TreeEntry[] = [];
  for (const entry of tree) {
    if (entry.type !== "blob" || path.posix.basename(entry.path) !== "SKILL.md") continue;
    const left = excludePaths.some(
      (exclude) =>
        exclude.repo.toLowerCase() === repository.toLowerCase() &&
        entry.path.startsWith(exclude.prefix)
    );
    (left ? excluded : skillFiles).push(entry);
  }
  const skills = skillFiles.map((entry) => ({ path: dirOf(entry.path), file: entry.path }));
  const withSkill = skillFiles.length + excluded.length > 0;
  // An extension is read only where a skills-sources.json is, which it may read.
  const sources = tree.some((entry) => path.posix.basename(entry.path) === SKILL_SOURCES);
  const manifests: { entry: TreeEntry; kind: ManifestKind }[] = [];
  for (const entry of tree) {
    const kind = manifestKind(entry);
    if (
      kind === undefined ||
      (kind === "omp-package" && !withSkill) ||
      (kind === "omp-extension" && !sources) ||
      (kind !== "skill-link" && insideSkill(skills, entry.path) !== undefined)
    )
      continue;
    manifests.push({ entry, kind });
  }
  return { skillFiles, excluded, manifests };
}

/** What a run reads of one repository beside its tree: its files (`repositoryFiles`), and, in a
 *  fork that holds a skill, the text files outside its skills that the fork scan reads (only
 *  those it changed, when `changed` gives them). */
function repositoryReads(
  repo: Repository,
  excludePaths: SkillCutsConfig["excludePaths"]
): RepositoryFiles & { readonly forkFiles: TreeEntry[] } {
  const files = repositoryFiles(repo.repository, repo.tree, excludePaths);
  const skills = files.skillFiles.map((entry) => ({ path: dirOf(entry.path), file: entry.path }));
  const withSkill = files.skillFiles.length + files.excluded.length > 0;
  // A fork's scan reads its text files outside its skills, only those it changed when `changed`
  // gives them (`forkChanges`).
  const changed = repo.changed === undefined ? undefined : new Set(repo.changed);
  const forkFiles =
    repo.fork && withSkill
      ? repo.tree.filter(
          (entry) =>
            entry.type === "blob" &&
            entry.mode !== "120000" &&
            (Object.hasOwn(TEXT_EXTENSIONS, path.posix.extname(entry.path).toLowerCase()) ||
              Object.hasOwn(TEXT_NAMES, path.posix.basename(entry.path))) &&
            insideSkill(skills, entry.path) === undefined &&
            (changed === undefined || changed.has(entry.path))
        )
      : [];
  return { ...files, forkFiles };
}

/** Whether the kept tree `tree` holds a `SKILL.md`. */
function holdsSkill(tree: readonly TreeEntry[]): boolean {
  return tree.some(
    (entry) => entry.type === "blob" && path.posix.basename(entry.path) === "SKILL.md"
  );
}

/** What a run keeps of one repository once its step is done: its skills parsed (names, paths,
 *  descriptions), its manifests parsed, the SKILL.md files `excludePaths` leaves out, the
 *  failures of those reads, and, in a fork holding a skill, how many text files its scan reads
 *  back from disk and how many larger ones it leaves out. It holds no blob text and no tree. */
interface RepositoryRead {
  readonly copies: SkillCopy[];
  readonly excluded: string[];
  readonly manifests: Manifest[];
  readonly skillFailures: string[];
  readonly manifestFailures: string[];
  readonly forkFiles: number;
  readonly forkLarge: number;
}

/** The key of a repository's reads in the cache: its name, default branch and --at, which name
 *  one commit. */
function repositoryKey(repo: Repository, at: string): string {
  return `${repo.repository}\0${repo.branch}\0${at}`;
}

/** Takes `repo` from the cache when it holds it: its commit, kept tree, and fork reads, and the
 *  texts of its SKILL.md and manifest files into `texts`. A file written before trees kept only
 *  the entries this list uses holds its whole tree: only those entries (`keepsEntry`) are taken,
 *  and the file is written again with them. A file written before a fork's text files were kept
 *  apart is rewritten: those files move to their own lines (`fork-files`), so its scan reads them
 *  one at a time. Returns the tree's gh calls of the run that read it, or undefined when the cache
 *  does not hold it whole. It runs without a pause, so two never hold a large file at once. */
function takeCached(
  cache: ReadCache,
  key: string,
  repo: Repository,
  excludePaths: SkillCutsConfig["excludePaths"],
  texts: Map<string, string>
): { treeCalls: number } | undefined {
  const cached: CachedRepository | undefined = cache.get("repository", key, CachedRepository);
  if (cached === undefined) return undefined;
  const scanned = repo.fork && holdsSkill(cached.tree);
  if (scanned && cached.forkFilesApart === true && !cache.hasLines("fork-files", key))
    return undefined;
  repo.commit = cached.commit;
  repo.tree = cached.tree.filter((entry) => keepsEntry(entry, repo.fork));
  repo.treeEntries = cached.treeEntries;
  repo.treeRead = cached.treeRead;
  // A fork cached before the compare rule was read from its whole tree, and keeps it.
  repo.forkRead = cached.forkRead ?? (scanned ? "whole tree: read before the compare rule" : "");
  repo.changed = cached.changed;
  const reads = repositoryReads(repo, excludePaths);
  for (const entry of [...reads.skillFiles, ...reads.manifests.map(({ entry }) => entry)]) {
    const text = cached.blobs[entry.sha];
    if (text !== undefined) texts.set(entry.sha, text);
  }
  if (scanned && cached.forkFilesApart !== true) {
    const writer = cache.lineWriter("fork-files", key);
    for (const entry of reads.forkFiles) {
      if ((entry.size ?? 0) > FORK_SCAN_MAX_BYTES) continue;
      writer.write(JSON.stringify([entry.path, entry.sha, cached.blobs[entry.sha] ?? null]));
    }
    writer.close();
    cache.put("repository", key, {
      ...cached,
      tree: repo.tree,
      blobs: Object.fromEntries(texts),
      forkRead: repo.forkRead,
      forkFilesApart: true,
    });
  } else if (repo.tree.length < cached.tree.length) {
    cache.put("repository", key, { ...cached, tree: repo.tree });
  }
  return { treeCalls: cached.treeCalls };
}

/** One repository whole, read or taken from the cache (`takeCached`): its commit at --at and kept
 *  tree, a fork's compare with its parent (`forkChanges`), then its SKILL.md and manifest blobs,
 *  parsed as they come, and a fork's text files, written to disk as they come for its scan. The
 *  cache gets the repository the moment it is done, and the step keeps only its
 *  `RepositoryRead`: its tree is emptied and no blob text outlives it. Returns undefined for a
 *  repository whose commit or tree read failed (`repo.error`), and the tree's gh calls. */
async function readRepositoryStep(
  github: Github,
  cache: ReadCache,
  repo: Repository,
  at: string,
  excludePaths: SkillCutsConfig["excludePaths"]
): Promise<{ read: RepositoryRead | undefined; calls: number; cached: boolean }> {
  const key = repositoryKey(repo, at);
  const texts = new Map<string, string>();
  const cached = takeCached(cache, key, repo, excludePaths, texts);
  const calls = cached === undefined ? await readRepository(github, cache, repo, at) : 0;
  if (repo.error !== "") return { read: undefined, calls, cached: false };
  const scanned = repo.fork && holdsSkill(repo.tree);
  if (cached === undefined && scanned) {
    const changes = await forkChanges(github, repo);
    repo.forkRead = changes.read;
    repo.changed = changes.changed;
  }
  const reads = repositoryReads(repo, excludePaths);
  const small = reads.forkFiles.filter((entry) => (entry.size ?? 0) <= FORK_SCAN_MAX_BYTES);
  if (cached === undefined && scanned) {
    const writer = cache.lineWriter("fork-files", key);
    try {
      await github.readBlobs(repo.repository, small, (entry, read) => {
        writer.write(JSON.stringify([entry.path, entry.sha, "text" in read ? read.text : null]));
      });
    } catch (error) {
      writer.abandon();
      throw error;
    }
    writer.close();
  }
  const errors = new Map<string, string>();
  const wanted = [...reads.skillFiles, ...reads.manifests.map(({ entry }) => entry)].filter(
    (entry) => !texts.has(entry.sha)
  );
  await github.readBlobs(repo.repository, wanted, (entry, read) => {
    if ("text" in read) texts.set(entry.sha, read.text);
    else errors.set(entry.sha, read.error);
  });
  const textOf = (entry: TreeEntry): string => {
    const text = texts.get(entry.sha);
    if (text === undefined) throw new Error(errors.get(entry.sha) ?? "the blob was not read");
    return text;
  };
  const skillFailures: string[] = [];
  const copies = reads.skillFiles.map((entry): SkillCopy => {
    const dir = dirOf(entry.path);
    const base = {
      repository: repo.repository,
      path: dir,
      file: entry.path,
      blob: entry.sha,
      fork: repo.fork,
      archived: repo.archived,
      parent: repo.parent,
    };
    try {
      return { ...base, ...skillFields(dir, repo.repository, textOf(entry)) };
    } catch (error) {
      const fault = `SKILL.md read failed: ${(error as Error).message}`;
      skillFailures.push(`${repo.repository}:${entry.path}: ${fault}`);
      const name = dir === "" ? (repo.repository.split("/")[1] ?? "") : path.posix.basename(dir);
      return { ...base, name, nameFrom: "directory", description: "", descriptionBytes: 0, fault };
    }
  });
  const manifestFailures: string[] = [];
  const manifests = reads.manifests.map(({ entry, kind }): Manifest => {
    const base = { repository: repo.repository, path: entry.path, kind, source: "tree" as const };
    try {
      return { ...base, rows: 0, ...parseManifest(kind, entry.path, textOf(entry)), error: "" };
    } catch (error) {
      const message = (error as Error).message;
      manifestFailures.push(`${repo.repository}:${entry.path} (${kind}): ${message}`);
      return { ...base, rows: 0, facts: [], ofKind: true, says: "", error: message };
    }
  });
  if (cached === undefined || wanted.length > 0) {
    cache.put("repository", key, {
      commit: repo.commit,
      treeEntries: repo.treeEntries,
      treeRead: repo.treeRead,
      treeCalls: cached?.treeCalls ?? calls,
      tree: repo.tree,
      blobs: Object.fromEntries(texts),
      forkRead: repo.forkRead,
      ...(repo.changed === undefined ? {} : { changed: repo.changed }),
      forkFilesApart: true,
    });
  }
  repo.tree = [];
  return {
    read: {
      copies,
      excluded: reads.excluded.map((entry) => entry.path),
      manifests,
      skillFailures,
      manifestFailures,
      forkFiles: scanned ? small.length : 0,
      forkLarge: scanned ? reads.forkFiles.length - small.length : 0,
    },
    calls,
    cached: cached !== undefined,
  };
}

// ---------------------------------------------------------------------------------------------
// frontmatter and skills

/** The YAML frontmatter block of a markdown text, or undefined when it has none. */
function frontmatterBlock(text: string): string | undefined {
  const lines = text.replace(/^\uFEFF/, "").split(/\r?\n/);
  if (lines[0]?.trimEnd() !== "---") return undefined;
  const end = lines.findIndex((line, index) => index > 0 && /^(---|\.\.\.)\s*$/.test(line));
  return end < 0 ? undefined : lines.slice(1, end).join("\n");
}

/** A block scalar's text (`|` or `>`, with its chomping indicator) from its lines. */
function blockScalar(header: string, lines: readonly string[]): string {
  const chomp = header.includes("-") ? "strip" : header.includes("+") ? "keep" : "clip";
  const content = [...lines];
  let trailing = 0;
  while (content.length > 0 && (content[content.length - 1] ?? "").trim() === "") {
    content.pop();
    trailing++;
  }
  const indent = Math.min(
    ...content.filter((line) => line.trim() !== "").map((line) => /^ */.exec(line)?.[0].length ?? 0)
  );
  const stripped = content.map((line) => (line.trim() === "" ? "" : line.slice(indent)));
  let body = "";
  if (header.startsWith("|")) {
    body = stripped.join("\n");
  } else {
    for (const line of stripped) {
      if (line === "") body += "\n";
      else body += body === "" || body.endsWith("\n") ? line : ` ${line}`;
    }
  }
  if (body === "" || chomp === "strip") return body;
  return chomp === "keep" ? body + "\n".repeat(trailing + 1) : `${body}\n`;
}

/** A quoted scalar's line breaks folded as YAML folds them: one break a space, n breaks n-1. */
function foldQuoted(text: string): string {
  return text.replace(/[ \t]*\n[ \t]*(\n[ \t]*)*/g, (run) => {
    const breaks = run.split("\n").length - 2;
    return breaks > 0 ? "\n".repeat(breaks) : " ";
  });
}

/** One top-level value of `lenientFields`: its text after `key:` and its continuation lines. */
function lenientValue(first: string, more: readonly string[]): unknown {
  if (/^[|>][-+0-9]*$/.test(first)) return blockScalar(first, more);
  const joined = [first, ...more.map((line) => line.trim())].join("\n").trim();
  if (first.startsWith('"')) {
    const inner = foldQuoted(/^"((?:[^"\\]|\\.)*)"/s.exec(joined)?.[1] ?? joined.slice(1));
    const parsed = parseJson(`"${inner.replace(/\n/g, "\\n").replace(/\t/g, "\\t")}"`);
    return typeof parsed === "string" ? parsed : inner;
  }
  if (first.startsWith("'")) {
    return foldQuoted(/^'((?:[^']|'')*)'/s.exec(joined)?.[1] ?? joined.slice(1)).replace(
      /''/g,
      "'"
    );
  }
  if (first.startsWith("[")) return strings(joined.replace(/\s*\n\s*/g, " "));
  if (first === "" && more.some((line) => /^\s*-\s/.test(line))) {
    return more
      .map((line) => /^\s*-\s+(.*)$/.exec(line)?.[1])
      .filter((item): item is string => item !== undefined)
      .map((item) => unquote(item.trim()));
  }
  return [first, ...more.map((line) => line.trim())]
    .map((line) => line.replace(/\s+#.*$/, ""))
    .filter((line) => line !== "")
    .join(" ");
}

/**
 * The top-level fields of a frontmatter block that is not valid YAML, read leniently: each line
 * `key: value` at the left margin starts a field, and the indented or blank lines after it continue
 * it. A value is plain (its lines joined by spaces), double- or single-quoted (over lines, folded),
 * a folded `>` or literal `|` block with its chomping indicator, a flow list `[a, b]` or a block
 * list of `- item` lines.
 */
export function lenientFields(block: string): Record<string, unknown> {
  const lines = block.split(/\r?\n/);
  const fields: Record<string, unknown> = {};
  let index = 0;
  while (index < lines.length) {
    const key = /^([A-Za-z_][\w-]*):(?:[ \t]+(.*)|[ \t]*)$/.exec(lines[index] ?? "");
    index++;
    if (key === null) continue;
    const more: string[] = [];
    while (index < lines.length && /^(\s+\S|\s*$)/.test(lines[index] ?? "")) {
      more.push(lines[index] ?? "");
      index++;
    }
    fields[key[1] ?? ""] = lenientValue((key[2] ?? "").trim(), more);
  }
  return fields;
}

const YamlMapping = z.record(z.string(), z.unknown());

/** The frontmatter fields of a markdown text: YAML, else `lenientFields`; undefined with none. */
export function frontmatter(text: string): Record<string, unknown> | undefined {
  const block = frontmatterBlock(text);
  if (block === undefined) return undefined;
  try {
    const parsed = YamlMapping.safeParse(Bun.YAML.parse(block));
    if (parsed.success) return parsed.data;
  } catch {
    // Not valid YAML (a description holding `: ` unquoted is common): read it leniently below.
  }
  return lenientFields(block);
}

/** A skill copy: one `SKILL.md` of one repository at the commit read. */
export interface SkillCopy {
  readonly repository: string;
  /** The skill's directory, `""` for the root. */
  readonly path: string;
  readonly file: string;
  readonly blob: string;
  readonly fork: boolean;
  readonly archived: boolean;
  readonly parent: string;
  readonly name: string;
  readonly nameFrom: "frontmatter" | "directory";
  readonly description: string;
  readonly descriptionBytes: number;
  /** Why the name or description could not be read, or `""`. */
  readonly fault: string;
}

/** What a `SKILL.md` text gives of its skill. */
export type SkillFields = Pick<
  SkillCopy,
  "name" | "nameFrom" | "description" | "descriptionBytes" | "fault"
>;

/** A skill's name and description from its `SKILL.md` text, its directory's name standing in for
 *  a missing frontmatter name. */
export function skillFields(dir: string, repository: string, text: string): SkillFields {
  const fields = frontmatter(text);
  const fallback = dir === "" ? (repository.split("/")[1] ?? repository) : path.posix.basename(dir);
  if (fields === undefined) {
    return {
      name: fallback,
      nameFrom: "directory",
      description: "",
      descriptionBytes: 0,
      fault: "no frontmatter",
    };
  }
  const name = typeof fields.name === "string" ? fields.name.trim() : "";
  const raw = fields.description;
  const description =
    typeof raw === "string" ? raw : raw === undefined || raw === null ? "" : String(raw);
  return {
    name: name === "" ? fallback : name,
    nameFrom: name === "" ? "directory" : "frontmatter",
    description,
    descriptionBytes: Buffer.byteLength(description, "utf8"),
    fault: raw === undefined ? "no description" : "",
  };
}

// ---------------------------------------------------------------------------------------------
// manifests

/**
 * The manifest kinds read from the trees, each by its path. `listed` is `always` for a kind every
 * file of which is shown (the few files that say where skills go), and `when of its kind` for one
 * shown only when it names skills (a package.json with omp.skills, an agent that loads skills, an
 * installer that installs a plugin or links skills); the others are counted.
 */
export const MANIFEST_KINDS = [
  {
    kind: "claude-marketplace",
    about: "a Claude Code marketplace: the plugins it lists and their sources",
    path: /(^|\/)\.claude-plugin\/marketplace\.json$/,
    listed: "always",
  },
  {
    kind: "claude-plugin",
    about: "a Claude Code plugin manifest: its name and skills",
    path: /(^|\/)\.claude-plugin\/plugin\.json$/,
    listed: "always",
  },
  {
    kind: "omp-plugins",
    about: "the Oh My Pi plugins a profile installs (a package.json of dependencies)",
    path: /(^|\/)(omp-plugins|omp\/plugins|\.omp\/plugins)\/package\.json$/,
    listed: "always",
  },
  {
    kind: "omp-package",
    about: "an Oh My Pi plugin package: a package.json with omp.skills",
    path: /(^|\/)package\.json$/,
    listed: "when of its kind",
  },
  {
    kind: "claude-settings",
    about: "Claude Code settings: the plugins enabled and the marketplaces known",
    path: /(^|\/)\.claude\/settings(\.local)?\.json$/,
    listed: "when of its kind",
  },
  {
    kind: "omp-config",
    about: "an Oh My Pi config.yml: skills.ignoredSkills",
    path: /(^|\/)(omp|\.omp|\.omp\/agent)\/config\.yml$/,
    listed: "when of its kind",
  },
  {
    kind: "agent-definition",
    about:
      "an agent definition whose frontmatter loads skills (autoloadSkills, or Claude Code's skills)",
    path: /(^|\/)agents\/[^/]+\.md$/,
    listed: "when of its kind",
  },
  {
    kind: "legion-project",
    about: "a Legion project file: verificationSkillsLeaveOut",
    path: /(^|\/)legion\/projects\/[^/]+\.ya?ml$/,
    listed: "always",
  },
  {
    kind: "prepack-script",
    about: "the prepack script that copies skills/ into the Oh My Pi plugin packages",
    path: /(^|\/)scripts\/pi-plugin-prepack\.sh$/,
    listed: "always",
  },
  {
    kind: "build-script",
    about: "the Claude Code plugin build that stages a package's skills into a generated plugin",
    path: /(^|\/)packages\/claude-envoy\/scripts\/build\.ts$/,
    listed: "always",
  },
  {
    kind: "installer-script",
    about: "an installer script that installs plugins or links skills into a host",
    path: /(^|\/)installers\/[^/]+\.sh$/,
    listed: "when of its kind",
  },
  {
    kind: "python-loader",
    about: "a Python loader naming the skills a session loads (a skills = [...] list)",
    path: /(^|\/)tree\/init\.py$/,
    listed: "always",
  },
  {
    kind: "skill-link",
    about: "a symbolic link (mode 120000) in a skills/ directory, pointing at a skill",
    path: /(^|\/)skills\/[^/]+$/,
    listed: "always",
  },
  {
    kind: "omp-skill-sources",
    about:
      "a skills-sources.json: skill pools an Oh My Pi extension of the same repository feeds to Oh My Pi",
    path: /(^|\/)skills-sources\.json$/,
    listed: "always",
  },
  {
    kind: "omp-extension",
    about: "an Oh My Pi extension that reads skills-sources.json (read where that file is)",
    path: /(^|\/)extensions\/[^/]+\.(ts|js|mjs)$/,
    listed: "when of its kind",
  },
] as const;

export type ManifestKind = (typeof MANIFEST_KINDS)[number]["kind"];

/** The manifest kinds manifests.tsv lists whether or not the file is of its kind. */
const ALWAYS_LISTED: ReadonlySet<ManifestKind> = new Set(
  MANIFEST_KINDS.filter((kind) => kind.listed === "always").map((kind) => kind.kind)
);

/** Whether manifests.tsv lists `manifest`: a file of its kind, or one of a kind it always lists. */
export function listedManifest(manifest: Pick<Manifest, "ofKind" | "kind">): boolean {
  return manifest.ofKind || ALWAYS_LISTED.has(manifest.kind);
}

/** The manifest kind of a tree entry, by its path (and, for a link, its mode). */
export function manifestKind(
  entry: Pick<TreeEntry, "path" | "mode" | "type">
): ManifestKind | undefined {
  if (entry.type !== "blob" || entry.path.split("/").includes("node_modules")) return undefined;
  if (entry.mode === "120000")
    return /(^|\/)skills\/[^/]+$/.test(entry.path) ? "skill-link" : undefined;
  return MANIFEST_KINDS.find((kind) => kind.kind !== "skill-link" && kind.path.test(entry.path))
    ?.kind;
}

export type Host = "omp" | "claude-code" | "any";

/** What a manifest says, before it is resolved against the copies. */
type Fact =
  | {
      readonly type: "marketplace-plugin";
      readonly marketplace: string;
      readonly plugin: string;
      readonly root: string | undefined;
      readonly repo: string;
      readonly npm: string;
      readonly skills: readonly string[];
      readonly other: string;
    }
  | {
      readonly type: "plugin-manifest";
      readonly plugin: string;
      readonly root: string;
      readonly skills: readonly string[];
    }
  | {
      readonly type: "package";
      readonly name: string;
      readonly root: string;
      readonly ompSkills: readonly string[];
    }
  | {
      readonly type: "plugin-switch";
      readonly host: Host;
      readonly plugin: string;
      readonly marketplace: string;
      readonly action: "enables" | "disables" | "installs";
    }
  | { readonly type: "marketplace-source"; readonly marketplace: string; readonly repo: string }
  | { readonly type: "ignores"; readonly patterns: readonly string[] }
  | {
      readonly type: "loads";
      readonly host: Host;
      readonly unit: string;
      readonly skills: readonly string[];
    }
  | { readonly type: "leaves-out"; readonly project: string; readonly plugins: readonly string[] }
  | { readonly type: "installs-package"; readonly package: string; readonly version: string }
  | {
      readonly type: "stages";
      readonly package: string;
      readonly sourceDir: string;
      readonly destDir: string;
      readonly skills: readonly string[];
    }
  | {
      readonly type: "generated-plugin";
      readonly plugin: string;
      readonly package: string;
      readonly packageRoot: string;
    }
  | {
      readonly type: "links-skills";
      readonly host: Host;
      readonly dir: string;
      readonly assumed: string;
    }
  | { readonly type: "links-config"; readonly file: string }
  | { readonly type: "skill-link"; readonly link: string; readonly target: string }
  | {
      readonly type: "skill-sources";
      readonly base: string;
      readonly sources: readonly SkillSource[];
    }
  | { readonly type: "reads-skill-sources"; readonly name: string }
  | { readonly type: "links-extension"; readonly file: string };

/** One skill pool of a skills-sources.json: its root (its skills at `<root>/skills`), or the
 *  skills directory `ompSkillsDir` instead, and `omp: false` to leave it out of Oh My Pi. */
export interface SkillSource {
  readonly root: string;
  readonly ompSkillsDir?: string | undefined;
  readonly omp?: boolean | undefined;
}

/** What one manifest file says. */
export interface ParsedManifest {
  readonly facts: readonly Fact[];
  /** Whether the file is of its kind (it names skills, plugins or packages). */
  readonly ofKind: boolean;
  /** What it says, in one line. */
  readonly says: string;
}

/** One manifest file read. */
export interface Manifest extends ParsedManifest {
  readonly repository: string;
  readonly path: string;
  readonly kind: ManifestKind;
  readonly source: "tree" | "code search";
  readonly error: string;
  rows: number;
}

/** A string list as manifests write one: a list, or one string. */
const StringList = z.union([z.string(), z.array(z.string())]).optional();
const MarketplaceFile = z.looseObject({
  name: z.string(),
  metadata: z.looseObject({ pluginRoot: z.string().optional() }).optional(),
  plugins: z.array(
    z.looseObject({
      name: z.string(),
      source: z.union([
        z.string(),
        z.looseObject({
          source: z.string(),
          repo: z.string().optional(),
          package: z.string().optional(),
          url: z.string().optional(),
        }),
      ]),
      skills: StringList,
    })
  ),
});
const PluginFile = z.looseObject({
  name: z.string().optional(),
  version: z.string().optional(),
  skills: StringList,
});
const PackageFile = z.looseObject({
  name: z.string().optional(),
  omp: z.looseObject({ skills: StringList }).optional(),
});
const PluginListFile = z.looseObject({
  name: z.string().optional(),
  dependencies: z.record(z.string(), z.string()).optional(),
});
const SettingsFile = z.looseObject({
  enabledPlugins: z.record(z.string(), z.boolean()).optional(),
  extraKnownMarketplaces: z
    .record(
      z.string(),
      z.looseObject({
        source: z
          .looseObject({ repo: z.string().optional(), url: z.string().optional() })
          .optional(),
      })
    )
    .optional(),
});
const OmpConfigFile = z.looseObject({
  skills: z.looseObject({ ignoredSkills: z.array(z.string()).optional() }).nullish(),
});
const ProjectFile = z.looseObject({
  key: z.string().optional(),
  verificationSkillsLeaveOut: z.array(z.string()).nullish(),
});
/** The skill-pool manifest an Oh My Pi extension may read (`omp-skill-sources`). */
const SKILL_SOURCES = "skills-sources.json";
const SkillSourcesFile = z.looseObject({
  sources: z.array(
    z.looseObject({
      root: z.string(),
      ompSkillsDir: z.string().optional(),
      omp: z.boolean().optional(),
    })
  ),
});

/** `text` as JSON or YAML, parsed by `schema`; throws naming each fault. */
function parseFile<T>(schema: z.ZodType<T>, text: string, format: "json" | "yaml"): T {
  const value = format === "json" ? (JSON.parse(text) as unknown) : Bun.YAML.parse(text);
  const parsed = schema.safeParse(value);
  if (!parsed.success) throw new Error(`not of its shape: ${schemaFaults(parsed.error)}`);
  return parsed.data;
}

/** The `owner/name` of a GitHub URL, or `""`. */
function githubRepository(url: string): string {
  return /github\.com[/:]([\w.-]+\/[\w.-]+?)(?:\.git)?\/?$/.exec(url)?.[1] ?? "";
}

function parseMarketplace(file: string, text: string): ParsedManifest {
  const value = parseFile(MarketplaceFile, text, "json");
  const base = dirOf(dirOf(file));
  const facts: Fact[] = [];
  const named: string[] = [];
  for (const entry of value.plugins) {
    const source = entry.source;
    let root: string | undefined;
    let repo = "";
    let npm = "";
    let other = "";
    if (typeof source === "string" && !source.includes("://")) {
      root = joinPath(
        base,
        source.startsWith(".") ? "" : (value.metadata?.pluginRoot ?? ""),
        source
      );
    } else if (typeof source === "string") {
      repo = githubRepository(source);
      if (repo === "") other = source;
    } else {
      if (source.source === "github") repo = source.repo ?? "";
      else if (source.source === "npm") npm = source.package ?? "";
      else if (source.source === "url" || source.source === "git")
        repo = githubRepository(source.url ?? "");
      if (repo === "" && npm === "") other = JSON.stringify(source);
    }
    const plugin = entry.name;
    facts.push({
      type: "marketplace-plugin",
      marketplace: value.name,
      plugin,
      root,
      repo,
      npm,
      skills: strings(entry.skills),
      other,
    });
    const from =
      root !== undefined
        ? shown(root)
        : repo !== ""
          ? `github ${repo}`
          : npm !== ""
            ? `npm ${npm}`
            : other;
    named.push(`${plugin} (${from})`);
  }
  return {
    facts,
    ofKind: true,
    says: `marketplace ${value.name}: ${named.join(", ") || "no plugins"}`,
  };
}

function parsePluginManifest(file: string, text: string): ParsedManifest {
  const value = parseFile(PluginFile, text, "json");
  const root = dirOf(dirOf(file));
  const plugin = value.name ?? path.posix.basename(root);
  const skills = strings(value.skills);
  return {
    facts: [{ type: "plugin-manifest", plugin, root, skills }],
    ofKind: true,
    says: `plugin ${plugin}${value.version ? ` ${value.version}` : ""}: skills ${skills.length > 0 ? skills.join(", ") : "from skills/"}`,
  };
}

function parsePackage(file: string, text: string): ParsedManifest {
  const value = parseFile(PackageFile, text, "json");
  const ompSkills = strings(value.omp?.skills);
  const name = value.name ?? "";
  return {
    facts: [{ type: "package", name, root: dirOf(file), ompSkills }],
    ofKind: ompSkills.length > 0,
    says: `package ${name}: omp.skills ${ompSkills.join(", ") || "none"}`,
  };
}

function parsePluginList(file: string, text: string): ParsedManifest {
  const value = parseFile(PluginListFile, text, "json");
  const dependencies = Object.entries(value.dependencies ?? {});
  const facts: Fact[] = dependencies.map(([name, version]) => ({
    type: "installs-package",
    package: name,
    version,
  }));
  facts.push({ type: "package", name: value.name ?? "", root: dirOf(file), ompSkills: [] });
  return {
    facts,
    ofKind: true,
    says: `installs ${dependencies.map(([name, version]) => `${name}@${version}`).join(", ") || "nothing"}`,
  };
}

function parseClaudeSettings(text: string): ParsedManifest {
  const value = parseFile(SettingsFile, text, "json");
  const facts: Fact[] = [];
  const enabled = Object.entries(value.enabledPlugins ?? {});
  for (const [key, on] of enabled) {
    const [plugin = "", marketplace = ""] = key.split("@");
    facts.push({
      type: "plugin-switch",
      host: "claude-code",
      plugin,
      marketplace,
      action: on ? "enables" : "disables",
    });
  }
  const known = Object.entries(value.extraKnownMarketplaces ?? {});
  for (const [marketplace, entry] of known) {
    const repo = entry.source?.repo ?? githubRepository(entry.source?.url ?? "");
    if (repo !== "") facts.push({ type: "marketplace-source", marketplace, repo });
  }
  return {
    facts,
    ofKind: facts.length > 0,
    says: `plugins ${enabled.map(([key, on]) => `${key} ${on ? "on" : "off"}`).join(", ") || "none"}; marketplaces ${
      known.map(([marketplace]) => marketplace).join(", ") || "none"
    }`,
  };
}

function parseOmpConfig(text: string): ParsedManifest {
  const patterns = parseFile(OmpConfigFile, text, "yaml").skills?.ignoredSkills ?? [];
  return {
    facts: patterns.length > 0 ? [{ type: "ignores", patterns }] : [],
    ofKind: patterns.length > 0,
    says: `skills.ignoredSkills ${patterns.join(", ") || "none"}`,
  };
}

function parseAgent(file: string, text: string): ParsedManifest {
  const fields = frontmatter(text) ?? {};
  const agent = typeof fields.name === "string" ? fields.name : path.posix.basename(file, ".md");
  const autoload = strings(fields.autoloadSkills);
  const claude = strings(fields.skills);
  const facts: Fact[] = [];
  if (autoload.length > 0)
    facts.push({ type: "loads", host: "omp", unit: `agent:${agent}`, skills: autoload });
  if (claude.length > 0)
    facts.push({ type: "loads", host: "claude-code", unit: `agent:${agent}`, skills: claude });
  const says = [
    autoload.length > 0 ? `autoloadSkills ${autoload.join(", ")}` : "",
    claude.length > 0 ? `skills ${claude.join(", ")}` : "",
  ].filter((part) => part !== "");
  return {
    facts,
    ofKind: facts.length > 0,
    says: `agent ${agent}: ${says.join("; ") || "loads no skill"}`,
  };
}

function parseLegionProject(file: string, text: string): ParsedManifest {
  const value = parseFile(ProjectFile, text, "yaml");
  const project = value.key ?? path.posix.basename(file).replace(/\.ya?ml$/, "");
  const plugins = value.verificationSkillsLeaveOut ?? [];
  return {
    facts: plugins.length > 0 ? [{ type: "leaves-out", project, plugins }] : [],
    ofKind: true,
    says: `project ${project}: verificationSkillsLeaveOut ${plugins.join(", ") || "none"}`,
  };
}

/** The prepack script: each `case` arm's package name and the `skills=( … )` list under it,
 *  copied from `$root/<dir>/$skill` (the repository's `skills/`) into the directory its last
 *  `stage_skills <dir>` names (`dist/skills`). */
function parsePrepack(file: string, text: string): ParsedManifest {
  const sourceDir = joinPath(
    dirOf(dirOf(file)),
    /"\$root\/([\w./-]+)\/\$skill"/.exec(text)?.[1] ?? "skills"
  );
  const destDir = /^\s*stage_skills\s+([^\s"'$]+)\s*$/m.exec(text)?.[1] ?? "dist/skills";
  const lines = text.split("\n");
  const facts: Fact[] = [];
  const says: string[] = [];
  let arm = "";
  for (let index = 0; index < lines.length; index++) {
    const line = (lines[index] ?? "").trim();
    const armLine = /^([@\w./-]+)\)$/.exec(line);
    if (armLine !== null) {
      arm = armLine[1] ?? "";
      continue;
    }
    if (line === ";;") arm = "";
    if (arm === "" || !line.startsWith("skills=(")) continue;
    let body = line.slice("skills=(".length);
    while (!body.includes(")") && index + 1 < lines.length) {
      index++;
      body += ` ${lines[index] ?? ""}`;
    }
    const skills = body
      .slice(0, body.indexOf(")"))
      .split(/\s+/)
      .filter((word) => word !== "");
    facts.push({ type: "stages", package: arm, sourceDir, destDir, skills });
    says.push(`${arm}: ${skills.join(" ")}`);
  }
  return {
    facts,
    ofKind: facts.length > 0,
    says: `stages ${shown(sourceDir)}/<name> into ${destDir}: ${says.join("; ")}`,
  };
}

/** The Claude Code build script: the package whose staged skills its generated plugin ships. */
function parseBuildScript(file: string, text: string): ParsedManifest {
  const staged = /"--stage-skills",\s*"([^"]+)"/.exec(text)?.[1];
  const plugin = /"plugin\.json"\s*\)[\s\S]{0,400}?\bname:\s*"([^"]+)"/.exec(text)?.[1];
  if (staged === undefined || plugin === undefined) {
    return {
      facts: [],
      ofKind: false,
      says: "no --stage-skills package or generated plugin name found",
    };
  }
  return {
    facts: [{ type: "generated-plugin", plugin, package: staged, packageRoot: dirOf(dirOf(file)) }],
    ofKind: true,
    says: `generates the plugin ${plugin} from the skills prepack stages for ${staged}`,
  };
}

/** Shell variable assignments of a script, the last one of each name winning. Only a line that is
 *  an assignment alone counts (one quoted string, one command substitution, or one bare word): a
 *  line that sets a variable for one command (`NAME="$NAME" "$NAME/bin/tool"`) sets none. */
function shellVariables(text: string): Map<string, string> {
  const vars = new Map<string, string>();
  for (const match of text.matchAll(
    /^\s*(?:local\s+|export\s+|readonly\s+)?([A-Za-z_]\w*)=(\S.*)$/gm
  )) {
    const value = (match[2] ?? "").trim();
    const whole =
      /^"\$\(.*\)"$/.test(value) ||
      /^"[^"]*"$/.test(value) ||
      /^'[^']*'$/.test(value) ||
      /^[^\s"'(]+$/.test(value);
    if (whole) vars.set(match[1] ?? "", unquote(value));
  }
  return vars;
}

/** A shell path as a repository path: a leading variable is replaced by its assignment, and one
 *  that is unknown or computes the script's own directory is taken as the repository's root. */
function shellPath(
  text: string,
  vars: Map<string, string>,
  depth = 0
): { dir: string; assumed: string } {
  const bare = text.replace(/["']/g, "");
  const variable = /^\$\{?([A-Za-z_]\w*)\}?(.*)$/.exec(bare);
  if (variable === null) return { dir: joinPath(bare), assumed: "" };
  const name = variable[1] ?? "";
  const rest = variable[2] ?? "";
  const value = vars.get(name);
  if (value === undefined || depth > 5 || /dirname|BASH_SOURCE|pwd/.test(value)) {
    return { dir: joinPath(rest.replace(/^\/+/, "")), assumed: value === undefined ? name : "" };
  }
  return shellPath(value + rest, vars, depth + 1);
}

/** An installer script: plugin installs (`<plugin>@<marketplace>` on a line holding `plugin`),
 *  loops that link each skill of a directory into a host's user skills, and the Oh My Pi
 *  config.yml it links. */
function parseInstaller(file: string, text: string): ParsedManifest {
  const script = path.posix.basename(file);
  const scriptHost: Host = script.includes("omp")
    ? "omp"
    : script.includes("claude")
      ? "claude-code"
      : "any";
  const vars = shellVariables(text);
  const lines = text.split("\n");
  const facts: Fact[] = [];
  const says: string[] = [];
  for (const raw of lines) {
    const line = raw.trim();
    if (line.startsWith("#") || /^(echo|printf)\b/.test(line) || !/plugin/i.test(line)) continue;
    for (const match of line.matchAll(
      /(?<![\w.@/$%{-])([A-Za-z0-9][\w.-]*)@([A-Za-z0-9][\w-]*)(?![\w.@-])/g
    )) {
      const host: Host = /\bomp\b/.test(line)
        ? "omp"
        : /\bclaude\b/.test(line)
          ? "claude-code"
          : scriptHost;
      const plugin = match[1] ?? "";
      const marketplace = match[2] ?? "";
      facts.push({ type: "plugin-switch", host, plugin, marketplace, action: "installs" });
      says.push(`installs ${plugin}@${marketplace} (${host})`);
      const repo = line
        .split(/\s+/)
        .map(unquote)
        .find((word) => /^[A-Za-z0-9][\w-]*\/[\w.-]+$/.test(word));
      if (repo !== undefined) facts.push({ type: "marketplace-source", marketplace, repo });
    }
  }
  for (let index = 0; index < lines.length; index++) {
    const loop = /^\s*for\s+\w+\s+in\s+(.+?)\s*;\s*do\b/.exec(lines[index] ?? "");
    if (loop === null) continue;
    let depth = 1;
    const body: string[] = [];
    for (let next = index + 1; next < lines.length && depth > 0; next++) {
      const line = lines[next] ?? "";
      if (/^\s*(for|while|until)\b.*\bdo\b/.test(line)) depth++;
      if (/^\s*done\b/.test(line)) depth--;
      body.push(line);
    }
    const hosts: Host[] = [];
    if (body.some((line) => line.includes(".claude/skills"))) hosts.push("claude-code");
    if (body.some((line) => line.includes(".omp/agent/skills"))) hosts.push("omp");
    const { dir, assumed } = shellPath((loop[1] ?? "").replace(/\/?\*\/?$/, ""), vars);
    for (const host of hosts) {
      facts.push({ type: "links-skills", host, dir, assumed });
      const root = assumed === "" ? "" : ` (taking $${assumed} as the repository root)`;
      says.push(`links each skill of ${shown(dir)}/ into the ${host} user skills${root}`);
    }
  }
  for (const raw of lines) {
    if (raw.trim().startsWith("#") || /^\s*(echo|printf)\b/.test(raw)) continue;
    for (const word of raw.trim().split(/\s+/).map(unquote)) {
      if (!/\/extensions\/[^/\s]+\.(ts|js|mjs)$/.test(word)) continue;
      const { dir } = shellPath(word, vars);
      // A link's other end in a home directory (`$HOME/.omp/agent/extensions/…`) is no file here.
      if (dir.startsWith(".")) continue;
      facts.push({ type: "links-extension", file: dir });
      says.push(`links ${dir} as an Oh My Pi extension`);
    }
  }
  for (const raw of lines) {
    if (!raw.includes(".omp/agent/config.yml") || raw.trim().startsWith("#")) continue;
    const source = raw
      .trim()
      .split(/\s+/)
      .map(unquote)
      .find((word) => word.endsWith("config.yml") && !word.includes(".omp/agent/config.yml"));
    if (source === undefined || !source.startsWith("$")) continue;
    const { dir } = shellPath(source, vars);
    facts.push({ type: "links-config", file: dir });
    says.push(`links ${dir} as the Oh My Pi config.yml`);
  }
  return {
    facts,
    ofKind: facts.length > 0,
    says: says.join("; ") || "installs no skill or plugin",
  };
}

const SKILLS_LIST = /["']?\bskills["']?\s*[=:]\s*\[([^\]]*)\]/;

/** A Python loader: each `skills = [ … ]` list of quoted names (in code, or in a TOML template it
 *  holds), with the `[roles.<role>]` table it sits under. */
function parsePythonLoader(text: string): ParsedManifest {
  const facts: Fact[] = [];
  const says: string[] = [];
  const roles = [...text.matchAll(/^\[roles\.([\w-]+)\]/gm)];
  for (const match of text.matchAll(new RegExp(SKILLS_LIST.source, "g"))) {
    const skills = [...(match[1] ?? "").matchAll(/["']([^"'\n]+)["']/g)].map(
      (item) => item[1] ?? ""
    );
    if (skills.length === 0) continue;
    const role = roles.filter((header) => (header.index ?? 0) < (match.index ?? 0)).pop()?.[1];
    const unit = role === undefined ? "list" : `roles.${role}`;
    facts.push({ type: "loads", host: "any", unit, skills });
    says.push(`${unit}: ${skills.join(", ")}`);
  }
  return {
    facts,
    ofKind: facts.length > 0,
    says: says.length === 0 ? "no skills list" : `loads ${says.join("; ")}`,
  };
}

/** The facts of one manifest file's text; throws when the text is not of its format. */
export function parseManifest(kind: ManifestKind, file: string, text: string): ParsedManifest {
  switch (kind) {
    case "claude-marketplace":
      return parseMarketplace(file, text);
    case "claude-plugin":
      return parsePluginManifest(file, text);
    case "omp-plugins":
      return parsePluginList(file, text);
    case "omp-package":
      return parsePackage(file, text);
    case "claude-settings":
      return parseClaudeSettings(text);
    case "omp-config":
      return parseOmpConfig(text);
    case "agent-definition":
      return parseAgent(file, text);
    case "legion-project":
      return parseLegionProject(file, text);
    case "prepack-script":
      return parsePrepack(file, text);
    case "build-script":
      return parseBuildScript(file, text);
    case "installer-script":
      return parseInstaller(file, text);
    case "python-loader":
      return parsePythonLoader(text);
    case "omp-skill-sources": {
      const sources = parseFile(SkillSourcesFile, text, "json").sources;
      return {
        facts: [{ type: "skill-sources", base: dirOf(file), sources }],
        ofKind: true,
        says: `skill pools: ${sources
          .map((source) =>
            [
              source.root,
              source.ompSkillsDir === undefined ? "" : ` (skills at ${source.ompSkillsDir})`,
              source.omp === false ? " (omp: false)" : "",
            ].join("")
          )
          .join(", ")}`,
      };
    }
    case "omp-extension": {
      const reads = text.includes(SKILL_SOURCES);
      return {
        facts: reads ? [{ type: "reads-skill-sources", name: SKILL_SOURCES }] : [],
        ofKind: reads,
        says: reads ? `an Oh My Pi extension that reads ${SKILL_SOURCES}` : "no skill pools",
      };
    }
    case "skill-link": {
      const target = joinPath(dirOf(file), text.trim());
      return {
        facts: [{ type: "skill-link", link: file, target }],
        ofKind: true,
        says: `links ${text.trim()} (${shown(target)})`,
      };
    }
  }
}

// ---------------------------------------------------------------------------------------------
// installers: the units that ship skills, and what installs each unit

/** One installer row: a manifest's action on one skill copy (or on a name no copy has). */
export interface InstallerRow {
  readonly manifestRepository: string;
  readonly manifestPath: string;
  readonly kind: ManifestKind;
  readonly action: string;
  readonly unit: string;
  readonly host: Host;
  readonly skill: string;
  readonly copyRepository: string;
  /** The copy's directory, or undefined when the row reaches no copy read. */
  readonly copyPath: string | undefined;
  readonly note: string;
}

/** A skill a unit ships: the copy, and its path inside the unit as installed. */
interface Member {
  readonly copy: SkillCopy;
  readonly rel: string;
}

/** Something that ships skills into sessions: a plugin, a package, a host's user skills. */
interface Unit {
  readonly id: string;
  readonly host: Host;
  readonly manifest: Manifest;
  readonly action: string;
  readonly members: readonly Member[];
  /** Where the unit is installed: prefix patterns, each ending in `/`, a member's `rel` after. */
  readonly locations: readonly string[];
  readonly note: string;
}

/** A row of the copy-path prefix table. */
export interface PrefixRow {
  readonly prefix: string;
  readonly repository: string;
  readonly path: string;
  readonly name: string;
  readonly via: string;
}

/** The copies of every repository read, indexed for the installers' lookups. */
export class CopyIndex {
  private readonly byDir = new Map<string, SkillCopy>();
  private readonly links = new Map<string, string>();
  readonly byName = new Map<string, SkillCopy[]>();

  constructor(readonly copies: readonly SkillCopy[]) {
    for (const copy of copies) {
      this.byDir.set(`${copy.repository}\0${copy.path}`, copy);
      this.byName.set(copy.name, [...(this.byName.get(copy.name) ?? []), copy]);
    }
  }

  addLink(repository: string, link: string, target: string): void {
    this.links.set(`${repository}\0${link}`, target);
  }

  /** The copy whose directory is `dir`, directly or through a link. */
  at(repository: string, dir: string): SkillCopy | undefined {
    const target = this.links.get(`${repository}\0${dir}`);
    return (
      this.byDir.get(`${repository}\0${dir}`) ??
      (target === undefined ? undefined : this.byDir.get(`${repository}\0${target}`))
    );
  }

  /** The copies directly under `dir` (a skills directory), each with its child name. */
  childrenOf(repository: string, dir: string): { child: string; copy: SkillCopy }[] {
    const found = new Map<string, SkillCopy>();
    for (const copy of this.copies) {
      if (copy.repository === repository && copy.path !== "" && dirOf(copy.path) === dir) {
        found.set(path.posix.basename(copy.path), copy);
      }
    }
    for (const [key, target] of this.links) {
      const [repo = "", link = ""] = key.split("\0");
      const copy = this.byDir.get(`${repo}\0${target}`);
      if (repo === repository && dirOf(link) === dir && copy !== undefined)
        found.set(path.posix.basename(link), copy);
    }
    return [...found]
      .map(([child, copy]) => ({ child, copy }))
      .sort((a, b) => a.child.localeCompare(b.child));
  }

  /** The members a skill path list gives under a root: each path a skill directory, or a
   *  directory of skill directories. */
  membersAt(repository: string, root: string, rels: readonly string[]): Member[] {
    const members = new Map<string, Member>();
    for (const rel of rels) {
      const dir = joinPath(root, rel);
      const copy = this.at(repository, dir);
      const found =
        copy !== undefined
          ? [{ copy, rel: joinPath(rel) }]
          : this.childrenOf(repository, dir).map((child) => ({
              copy: child.copy,
              rel: joinPath(rel, child.child),
            }));
      for (const member of found)
        members.set(`${member.copy.repository}\0${member.copy.path}\0${member.rel}`, member);
    }
    return [...members.values()];
  }
}

/** A glob of skill names (`*` any run of characters) as a regular expression. */
function nameGlob(pattern: string): RegExp {
  return new RegExp(`^${pattern.split("*").map(escapeRegExp).join(".*")}$`);
}

/** What the installers give: their rows, the units that ship skills, the prefix rows, and notes on
 *  manifests whose skills this list cannot hold (a skill pool outside the repository's tree). */
export interface Resolved {
  readonly rows: InstallerRow[];
  readonly units: Unit[];
  readonly prefixes: PrefixRow[];
  readonly notes: ManifestNote[];
}

/** A note on one manifest, which skill-copies.md prints under its line. */
export interface ManifestNote {
  readonly repository: string;
  readonly path: string;
  readonly note: string;
}

/** The facts of one type across the manifests, each with its manifest. */
function factsOf<T extends Fact["type"]>(
  manifests: readonly Manifest[],
  type: T
): { manifest: Manifest; fact: Extract<Fact, { type: T }> }[] {
  return manifests.flatMap((manifest) =>
    manifest.facts
      .filter((fact): fact is Extract<Fact, { type: T }> => fact.type === type)
      .map((fact) => ({ manifest, fact }))
  );
}

/** The installer rows, units, prefix rows and notes of the manifests read, against the copies. */
export function resolveInstallers(manifests: readonly Manifest[], index: CopyIndex): Resolved {
  const rows: InstallerRow[] = [];
  const units: Unit[] = [];
  const notes: ManifestNote[] = [];
  const row = (
    manifest: Manifest,
    action: string,
    unit: string,
    host: Host,
    skill: string,
    copy: SkillCopy | undefined,
    note = ""
  ) =>
    rows.push({
      manifestRepository: manifest.repository,
      manifestPath: manifest.path,
      kind: manifest.kind,
      action,
      unit,
      host,
      skill,
      copyRepository: copy?.repository ?? "",
      copyPath: copy?.path,
      note,
    });

  const links = factsOf(manifests, "skill-link");
  for (const { manifest, fact } of links)
    index.addLink(manifest.repository, fact.link, fact.target);
  for (const { manifest, fact } of links) {
    const copy = index.at(manifest.repository, fact.target);
    row(
      manifest,
      "links",
      `link:${fact.link}`,
      "any",
      copy?.name ?? "",
      copy,
      copy ? "" : `no skill at ${shown(fact.target)}`
    );
  }

  // Every package.json read, by name, for npm sources and plugin lists.
  const packages = new Map<
    string,
    { manifest: Manifest; root: string; ompSkills: readonly string[] }
  >();
  for (const { manifest, fact } of factsOf(manifests, "package")) {
    if (fact.name !== "" && !packages.has(fact.name))
      packages.set(fact.name, { manifest, root: fact.root, ompSkills: fact.ompSkills });
  }
  const stages = factsOf(manifests, "stages");
  const builds = factsOf(manifests, "generated-plugin");
  /** The copies the prepack script stages for a package, at `<dir>/<name>` (dist/skills). */
  const staged = (name: string, dir?: string): Member[] =>
    stages
      .filter((stage) => stage.fact.package === name)
      .flatMap((stage) =>
        stage.fact.skills.flatMap((skill) => {
          const copy = index.at(stage.manifest.repository, joinPath(stage.fact.sourceDir, skill));
          return copy === undefined
            ? []
            : [{ copy, rel: joinPath(dir ?? stage.fact.destDir, skill) }];
        })
      );
  const imageRoot = (name: string) => `/opt/legion/${name.split("/").pop() ?? name}/`;

  // Oh My Pi plugin packages (package.json with omp.skills): skills in the tree, or staged.
  const packageUnits = new Map<string, Unit>();
  for (const [name, entry] of packages) {
    if (entry.ompSkills.length === 0) continue;
    const members = [
      ...index.membersAt(entry.manifest.repository, entry.root, entry.ompSkills),
      ...staged(name),
    ];
    const inImage =
      stages.some((stage) => stage.fact.package === name) ||
      builds.some((build) => build.fact.packageRoot === entry.root);
    const unit: Unit = {
      id: `package:${name}`,
      host: "omp",
      manifest: entry.manifest,
      action: "ships",
      members,
      locations: inImage
        ? [`**/node_modules/${name}/`, imageRoot(name)]
        : [`**/node_modules/${name}/`],
      note:
        members.length > 0
          ? ""
          : `omp.skills ${entry.ompSkills.join(", ")} holds no SKILL.md in the tree, and no prepack script stages it`,
    };
    packageUnits.set(name, unit);
    units.push(unit);
  }
  for (const { manifest, fact } of stages) {
    for (const skill of fact.skills) {
      const copy = index.at(manifest.repository, joinPath(fact.sourceDir, skill));
      const note =
        copy === undefined
          ? `no SKILL.md at ${joinPath(fact.sourceDir, skill)}`
          : packageUnits.has(fact.package)
            ? ""
            : `no package.json of ${fact.package} with omp.skills was read`;
      row(manifest, "stages", `package:${fact.package}`, "omp", skill, copy, note);
    }
  }

  // The plugin the build script generates from a package's staged skills.
  for (const { manifest, fact } of builds) {
    const members = staged(fact.package, "skills");
    units.push({
      id: `plugin:${fact.plugin}`,
      host: "claude-code",
      manifest,
      action: "ships",
      members,
      locations: [`/opt/legion/${path.posix.basename(fact.packageRoot)}/${fact.plugin}/`],
      note: members.length > 0 ? "" : `the prepack script stages nothing for ${fact.package}`,
    });
  }

  // Claude Code plugins: from marketplaces, and plugin.json files no marketplace names.
  const pluginManifests = factsOf(manifests, "plugin-manifest");
  const claimed = new Set<Manifest>();
  const pluginUnits = new Map<string, Unit>();
  for (const { manifest, fact } of factsOf(manifests, "marketplace-plugin")) {
    let repository = manifest.repository;
    let root = fact.root;
    let note = "";
    if (root === undefined && fact.repo !== "") {
      repository = fact.repo;
      root = "";
    } else if (root === undefined && fact.npm !== "") {
      const found = packages.get(fact.npm);
      if (found !== undefined) {
        repository = found.manifest.repository;
        root = found.root;
      } else note = `the npm package ${fact.npm} is in no package.json read`;
    } else if (root === undefined) note = `the source ${fact.other} is not read`;
    const own = pluginManifests.find(
      (entry) => entry.manifest.repository === repository && entry.fact.root === root
    );
    if (own !== undefined) claimed.add(own.manifest);
    const members =
      root === undefined
        ? []
        : index.membersAt(repository, root, [
            "skills",
            ...fact.skills,
            ...(own?.fact.skills ?? []),
          ]);
    const id = `plugin:${fact.plugin}@${fact.marketplace}`;
    const locations = [
      `**/plugins/cache/plugins/${fact.marketplace}___${fact.plugin}___*/`,
      `**/plugins/cache/${fact.marketplace}/${fact.plugin}/*/`,
    ];
    if (fact.npm !== "" && builds.some((build) => build.fact.packageRoot === root))
      locations.push(imageRoot(fact.npm));
    const unit: Unit = {
      id,
      host: "claude-code",
      manifest,
      action: "ships",
      members,
      locations,
      note:
        note ||
        (members.length === 0 ? `no SKILL.md under ${repository} ${shown(root ?? "")}/skills` : ""),
    };
    pluginUnits.set(id, unit);
    units.push(unit);
  }
  for (const { manifest, fact } of pluginManifests) {
    if (claimed.has(manifest)) continue;
    units.push({
      id: `plugin:${fact.plugin}`,
      host: "claude-code",
      manifest,
      action: "ships",
      members: index.membersAt(manifest.repository, fact.root, ["skills", ...fact.skills]),
      locations: [],
      note: "no marketplace read names this plugin, so where it is installed is not known",
    });
  }

  // A host's user skills, linked by an installer.
  for (const { manifest, fact } of factsOf(manifests, "links-skills")) {
    const members = index
      .childrenOf(manifest.repository, fact.dir)
      .map(({ child, copy }) => ({ copy, rel: child }));
    units.push({
      id: `user-skills:${fact.host}`,
      host: fact.host,
      manifest,
      action: "links",
      members,
      locations: [fact.host === "omp" ? "**/.omp/agent/skills/" : "**/.claude/skills/"],
      note: members.length > 0 ? "" : `no SKILL.md under ${shown(fact.dir)}/ in the tree`,
    });
  }

  // Skill pools: a skills-sources.json that an Oh My Pi extension of the same repository reads,
  // the extension linked into Oh My Pi by an installer of that repository (dotfiles-skills.ts
  // answers `resources_discover` with each pool's skills directory). Each pool whose skills are in
  // the tree installs them into Oh My Pi; a pool outside it (another checkout, a vendor clone, a
  // home directory) is a note, since this list cannot hold its skills.
  for (const { manifest, fact } of factsOf(manifests, "skill-sources")) {
    const sameRepository = <T>(entries: { manifest: Manifest; fact: T }[]) =>
      entries.filter((entry) => entry.manifest.repository === manifest.repository);
    const readers = sameRepository(factsOf(manifests, "reads-skill-sources")).filter(
      ({ fact: reads }) => reads.name === path.posix.basename(manifest.path)
    );
    const links = sameRepository(factsOf(manifests, "links-extension"));
    const linked = readers.filter((reader) =>
      links.some(({ fact: link }) => link.file === reader.manifest.path)
    );
    if (linked.length === 0) {
      notes.push({
        repository: manifest.repository,
        path: manifest.path,
        note:
          readers.length === 0
            ? "no Oh My Pi extension of the repository reads it, so it installs nothing"
            : `no installer of the repository links ${readers.map((reader) => reader.manifest.path).join(", ")} into Oh My Pi, so it installs nothing`,
      });
      continue;
    }
    for (const source of fact.sources) {
      if (source.omp === false) continue;
      const skills = source.ompSkillsDir ?? joinPath(source.root, "skills");
      const dir = /^[~/]/.test(skills) ? undefined : joinPath(fact.base, skills);
      const members =
        dir === undefined || dir === ".." || dir.startsWith("../")
          ? []
          : index
              .childrenOf(manifest.repository, dir)
              .map(({ child, copy }) => ({ copy, rel: child }));
      if (dir === undefined || members.length === 0) {
        notes.push({
          repository: manifest.repository,
          path: manifest.path,
          note: `pool ${source.root}: its skills (${skills}) are not in the repository's tree, so this list cannot hold them`,
        });
        continue;
      }
      units.push({
        id: `pool:${source.root}`,
        host: "omp",
        manifest,
        action: "installs",
        members,
        locations: [`**/${dir}/`],
        note: `read by ${linked.map((reader) => reader.manifest.path).join(", ")}`,
      });
    }
  }

  for (const unit of units) {
    if (unit.members.length === 0)
      row(unit.manifest, unit.action, unit.id, unit.host, "", undefined, unit.note);
    for (const member of unit.members)
      row(unit.manifest, unit.action, unit.id, unit.host, member.copy.name, member.copy, unit.note);
  }

  // What installs a unit: settings and installers (plugins), plugin lists (packages), projects.
  const marketplaces = new Set(
    factsOf(manifests, "marketplace-plugin").map(({ fact }) => fact.marketplace)
  );
  for (const { manifest, fact } of factsOf(manifests, "plugin-switch")) {
    const id = `plugin:${fact.plugin}@${fact.marketplace}`;
    const unit = pluginUnits.get(id);
    if (unit === undefined || unit.members.length === 0) {
      const why = marketplaces.has(fact.marketplace)
        ? `the marketplace ${fact.marketplace} lists no such plugin with skills`
        : `the marketplace ${fact.marketplace} is not in the trees read`;
      row(manifest, fact.action, id, fact.host, "", undefined, why);
    }
    for (const member of unit?.members ?? [])
      row(manifest, fact.action, id, fact.host, member.copy.name, member.copy);
  }
  for (const { manifest, fact } of factsOf(manifests, "installs-package")) {
    const id = `package:${fact.package}`;
    const members = packageUnits.get(fact.package)?.members ?? [];
    if (members.length === 0)
      row(
        manifest,
        "installs",
        id,
        "omp",
        "",
        undefined,
        `no package ${fact.package} with skills was read`
      );
    for (const member of members)
      row(
        manifest,
        "installs",
        id,
        "omp",
        member.copy.name,
        member.copy,
        `version ${fact.version}`
      );
  }
  for (const { manifest, fact } of factsOf(manifests, "leaves-out")) {
    for (const plugin of fact.plugins) {
      const matching = units.filter((unit) => unit.id.startsWith(`plugin:${plugin}@`));
      const id = `project:${fact.project}`;
      if (matching.length === 0)
        row(
          manifest,
          "leaves out",
          id,
          "omp",
          "",
          undefined,
          `the plugin ${plugin} is in no marketplace read`
        );
      for (const unit of matching) {
        for (const member of unit.members)
          row(manifest, "leaves out", id, "omp", member.copy.name, member.copy, unit.id);
      }
    }
  }
  for (const { manifest, fact } of factsOf(manifests, "ignores")) {
    for (const pattern of fact.patterns) {
      const glob = nameGlob(pattern);
      const matched = index.copies.filter((copy) => glob.test(copy.name));
      if (matched.length === 0)
        row(
          manifest,
          "ignores",
          "omp-settings",
          "omp",
          pattern,
          undefined,
          "no copy of this name was read"
        );
      for (const copy of matched)
        row(manifest, "ignores", "omp-settings", "omp", copy.name, copy, `pattern ${pattern}`);
    }
  }
  for (const { manifest, fact } of factsOf(manifests, "links-config")) {
    row(
      manifest,
      "links config",
      "omp-settings",
      "omp",
      "",
      undefined,
      `${fact.file} is the Oh My Pi config.yml`
    );
  }
  for (const { manifest, fact } of factsOf(manifests, "loads")) {
    const unit = fact.unit.startsWith("agent:")
      ? fact.unit
      : `loader:${manifest.path}#${fact.unit}`;
    for (const skill of fact.skills) {
      const named = index.byName.get(skill) ?? [];
      const own = named.filter((copy) => copy.repository === manifest.repository);
      const chosen = own.length > 0 ? own : named;
      if (chosen.length === 0)
        row(manifest, "loads", unit, fact.host, skill, undefined, "no copy of this name was read");
      for (const copy of chosen) row(manifest, "loads", unit, fact.host, skill, copy);
    }
  }

  const prefixes = new Map<string, PrefixRow>();
  for (const unit of units) {
    const via = `${unit.id} (${unit.manifest.repository}:${unit.manifest.path})`;
    for (const member of unit.members) {
      for (const location of unit.locations) {
        const prefix = `${location}${member.rel}/`;
        const key = `${prefix}\0${member.copy.repository}\0${member.copy.path}`;
        const known = prefixes.get(key);
        prefixes.set(
          key,
          known === undefined
            ? {
                prefix,
                repository: member.copy.repository,
                path: member.copy.path,
                name: member.copy.name,
                via,
              }
            : {
                ...known,
                via: known.via.split("; ").includes(via) ? known.via : `${known.via}; ${via}`,
              }
        );
      }
    }
  }
  return {
    rows,
    units,
    notes,
    prefixes: [...prefixes.values()].sort(
      (a, b) => a.prefix.localeCompare(b.prefix) || a.repository.localeCompare(b.repository)
    ),
  };
}

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** The regular expression of a prefix pattern: `**\/` at its start any leading directories, `*`
 *  one path segment's text, the rest literal; it matches a path that starts with the prefix. */
export function prefixRegExp(pattern: string): RegExp {
  const anywhere = pattern.startsWith("**/");
  const body = (anywhere ? pattern.slice(3) : pattern).split("*").map(escapeRegExp).join("[^/]+");
  return new RegExp(`^${anywhere ? "(?:.*/)?" : ""}${body}`);
}

/** Whether `copyPath` (a read's path) is under the prefix `pattern`. */
export function prefixMatches(pattern: string, copyPath: string): boolean {
  return prefixRegExp(pattern).test(copyPath);
}

// ---------------------------------------------------------------------------------------------
// names in files

/** The kinds of mention of a skill name in a file. */
export const MATCH_KINDS = [
  "skill-uri",
  "slash-skill",
  "plugin-qualified",
  "skills-path",
  "quoted-string",
  "name",
  "code-load",
] as const;
export type MatchKind = (typeof MATCH_KINDS)[number];

const NAME_CHAR = "[A-Za-z0-9_-]";
/** The extensions of code files, where a quoted name on a skills line is a load. */
const CODE_EXTENSIONS: Readonly<Record<string, true>> = {
  ".ts": true,
  ".tsx": true,
  ".js": true,
  ".mjs": true,
  ".cjs": true,
  ".py": true,
  ".go": true,
  ".rs": true,
  ".rb": true,
  ".lua": true,
  ".sh": true,
  ".bash": true,
};

/**
 * The kinds of whole-name mention of `name` in `text`: each place the name stands with no name
 * character (a letter, digit, `_` or `-`) on either side, so `tool-loop-alpha` is no mention of
 * `alpha`. Each is read by what stands before it: `skill://`, `/skill:`, `<plugin>:` (one of
 * `plugins`), `skills/`, a quote that closes after it, or nothing of these (`name`). In a code file
 * (`CODE_EXTENSIONS`) a quoted mention on a line that also says `skills`, `Skills` or `SKILL.md`
 * is also `code-load`: code that loads the skill (a `skills = [...]` list, a path to its SKILL.md).
 */
export function mentions(
  text: string,
  name: string,
  plugins: readonly string[],
  file = ""
): MatchKind[] {
  const kinds = new Set<MatchKind>();
  const code = Object.hasOwn(CODE_EXTENSIONS, path.posix.extname(file));
  for (const match of text.matchAll(
    new RegExp(`(?<!${NAME_CHAR})${escapeRegExp(name)}(?!${NAME_CHAR})`, "g")
  )) {
    const at = match.index ?? 0;
    const before = text.slice(Math.max(0, at - 80), at);
    const quote = before.slice(-1);
    if (before.endsWith("skill://")) kinds.add("skill-uri");
    else if (before.endsWith("/skill:")) kinds.add("slash-skill");
    else if (plugins.some((plugin) => before.endsWith(`${plugin}:`))) kinds.add("plugin-qualified");
    else if (before.endsWith("skills/")) kinds.add("skills-path");
    else if (quote !== "" && "\"'`".includes(quote) && text[at + name.length] === quote) {
      kinds.add("quoted-string");
      const lineEnd = text.indexOf("\n", at);
      const line = text.slice(text.lastIndexOf("\n", at) + 1, lineEnd < 0 ? text.length : lineEnd);
      if (code && /skills|Skills|SKILL\.md/.test(line)) kinds.add("code-load");
    } else kinds.add("name");
  }
  return MATCH_KINDS.filter((kind) => kinds.has(kind));
}

// ---------------------------------------------------------------------------------------------
// code search

/** Plain English words that are, or may be, skill names: each query of such a name is run again
 *  once per repository that holds a skill. A short list, not a dictionary: a plain word not on it
 *  is run again only when its query reaches 1,000 hits. */
export const COMMON_WORD_NAMES: Readonly<Record<string, true>> = {
  dispatch: true,
  envoy: true,
  architect: true,
  autonomous: true,
  brainstorming: true,
  browser: true,
  commit: true,
  controller: true,
  debugging: true,
  deploy: true,
  design: true,
  docs: true,
  engineering: true,
  memory: true,
  merge: true,
  oracle: true,
  plan: true,
  planning: true,
  proof: true,
  refactor: true,
  release: true,
  research: true,
  retro: true,
  review: true,
  search: true,
  security: true,
  simplify: true,
  spec: true,
  tasks: true,
  testing: true,
  triage: true,
  verification: true,
  worker: true,
  writing: true,
};

/**
 * The search terms for a skill name: the quoted name, each run between colons quoted alone, since
 * the pod's gh refuses a quoted search word that holds a colon (`a:b` is sent as `"a" "b"`).
 *
 * One search for the quoted name, per owner, stands for the five patterns a file can name a skill
 * by (`skill://<name>`, `/skill:<name>`, `skills/<name>`, `<plugin>:<name>` and the quoted name):
 *   - every one of the five holds the name, and GitHub's code search reads punctuation as a break
 *     between words, so a file that matches any of them holds the name's own words in a row, which
 *     is what the quoted name matches;
 *   - every hit file is read and kept only where the name stands whole (`mentions`), and
 *     `mentions` sorts each mention by those five patterns, so a pattern's own search would find
 *     no file, and no kind of mention, that the one search misses;
 *   - a search whose count reaches GitHub's cap of 1,000 results is run again per repository, so
 *     the cap on the one search hides no repository a narrower pattern's search would reach.
 * Checked on cybermonklabs/legion on 2026-11-20, with all five searched: for each of
 * `agent-secrets`, `approval-review` and `asd-ste100`, every file the four other patterns found
 * (54, 19 and 24 files) was among the quoted name's hits. One search instead of five takes a run
 * over three owners' 632 names from about 8,100 searches to about 1,900.
 */
export function searchTerms(name: string): string {
  return name
    .replace(/"/g, "")
    .split(":")
    .filter((part) => part !== "")
    .map((part) => `"${part}"`)
    .join(" ");
}

/** One search query: a skill name in one scope. */
interface SearchJob {
  readonly name: string;
  /** `org:<o>`, `user:<o>` or `repo:<r>`. */
  readonly scope: string;
  readonly owner: string;
  readonly reason: string;
}

/** A search hit and what reading it found. */
export interface SearchHit {
  readonly repository: string;
  readonly path: string;
  verdict: string;
  match: string;
}

/** One line of queries.jsonl for a search. */
export interface QueryRecord {
  readonly type: "search";
  readonly name: string;
  readonly q: string;
  readonly scope: string;
  readonly reason: string;
  status: "ok" | "failed" | "not run" | "partial";
  total_count: number | null;
  incomplete_results: boolean | null;
  pages: number;
  calls: number;
  rate_limit_retries: number;
  items_read: number;
  /** Hits GitHub counted that were not read: past its 1,000-result cap, or past the budget. */
  unread: number;
  hits: SearchHit[];
  error: string;
}

/** One line of queries.jsonl for a fork read from its tree: `read` says which files, the ones it
 *  changed against its parent (`changed files against <parent>@<merge base>`, `changed_files` of
 *  them listed) or all of them (`whole tree: <why>`, `changed_files` null). */
export interface ForkScanRecord {
  readonly type: "fork-scan";
  readonly repository: string;
  readonly read: string;
  readonly changed_files: number | null;
  readonly max_bytes: number;
  files_read: number;
  skipped_large: number;
  skipped_binary: number;
  files_naming_a_skill: number;
  failures: string[];
}

/** One page of a search's answer. */
interface SearchPage {
  readonly total: number;
  readonly incomplete: boolean;
  readonly items: z.infer<typeof SearchAnswer>["items"];
}

/** HTTP status, headers and body of a `gh api -i` answer. */
function splitIncluded(text: string): {
  status: number;
  headers: Map<string, string>;
  body: string;
} {
  const gap = /\r?\n\r?\n/.exec(text);
  if (!text.startsWith("HTTP/") || gap === null)
    return { status: 0, headers: new Map(), body: text };
  const head = text.slice(0, gap.index).split(/\r?\n/);
  const headers = new Map<string, string>();
  for (const line of head.slice(1)) {
    const colon = line.indexOf(":");
    if (colon > 0)
      headers.set(line.slice(0, colon).trim().toLowerCase(), line.slice(colon + 1).trim());
  }
  return {
    status: Number(/^HTTP\/\S+\s+(\d+)/.exec(head[0] ?? "")?.[1] ?? 0),
    headers,
    body: text.slice(gap.index + gap[0].length),
  };
}

/** One owner's code-search pacing: the start times of its calls in the last minute, its limit a
 *  minute, and when its spent limit resets. */
interface OwnerPace {
  starts: number[];
  limit: number;
  blockedUntil: number;
}

/** The calls a minute GitHub allows code search when an answer gives no `x-ratelimit-limit`. */
const SEARCH_LIMIT = 10;
const MINUTE_MS = 60_000;

/** Code search calls under a call budget, each owner paced by its own token's limit (the file
 *  header, step 5). */
class CodeSearch {
  calls = 0;
  private readonly paces = new Map<string, OwnerPace>();

  constructor(
    private readonly github: Github,
    readonly budget: number,
    readonly cache: ReadCache
  ) {}

  private pace(owner: string): OwnerPace {
    let pace = this.paces.get(owner);
    if (pace === undefined) {
      pace = { starts: [], limit: SEARCH_LIMIT, blockedUntil: 0 };
      this.paces.set(owner, pace);
    }
    return pace;
  }

  /** Waits until `owner` may start a call: fewer than its limit started in the last minute, and
   *  past any reset its answers or a refusal named. */
  private async waitFor(owner: string): Promise<void> {
    const io = this.github.io;
    const pace = this.pace(owner);
    for (;;) {
      const now = io.now();
      pace.starts = pace.starts.filter((start) => start > now - MINUTE_MS);
      const windowOpens = pace.starts.length < pace.limit ? 0 : (pace.starts[0] ?? now) + MINUTE_MS;
      const until = Math.max(pace.blockedUntil, windowOpens);
      if (until <= now) return;
      io.err(
        `copies: code search of ${owner} waits ${Math.ceil((until - now) / 1000)} s for its rate limit`
      );
      await io.sleep(until - now);
    }
  }

  /** One page of a search; undefined when the call budget is spent before it. */
  async page(
    job: SearchJob,
    q: string,
    page: number,
    record: QueryRecord
  ): Promise<SearchPage | undefined> {
    const io = this.github.io;
    const pace = this.pace(job.owner);
    for (;;) {
      await this.waitFor(job.owner);
      if (this.calls >= this.budget) return undefined;
      this.calls++;
      record.calls++;
      // A search takes a read slot as a tree read does (READ_CALLS), keyed by its owner, whose
      // searches run one at a time; it starts, for its pace, once it holds the slot.
      const result = await this.github.inSlot(`search ${job.owner}`, () => {
        pace.starts.push(io.now());
        return this.github.call([
          "api",
          "-i",
          "-X",
          "GET",
          "search/code",
          "-f",
          `q=${q}`,
          "-f",
          "per_page=100",
          "-f",
          `page=${page}`,
        ]);
      });
      const answer = splitIncluded(result.stdout);
      const header = (name: string) => {
        const value = Number(answer.headers.get(name));
        return Number.isFinite(value) && value > 0 ? value : undefined;
      };
      const limit = header("x-ratelimit-limit");
      if (limit !== undefined) pace.limit = limit;
      const reset = header("x-ratelimit-reset");
      const resetMs = reset === undefined ? undefined : reset * 1000 + 1000;
      if (answer.headers.get("x-ratelimit-remaining") === "0" && resetMs !== undefined)
        pace.blockedUntil = resetMs;
      if (result.code === 0 && answer.status < 300) {
        const parsed = SearchAnswer.safeParse(parseJson(answer.body));
        if (!parsed.success)
          throw new Error(`the search answer is not of its shape: ${schemaFaults(parsed.error)}`);
        return {
          total: parsed.data.total_count,
          incomplete: parsed.data.incomplete_results,
          items: parsed.data.items,
        };
      }
      if (
        record.rate_limit_retries < MAX_RATE_RETRIES &&
        (answer.status === 429 || RATE_LIMITED.test(`${result.stderr}\n${answer.body}`))
      ) {
        record.rate_limit_retries++;
        const retryAfter = header("retry-after");
        pace.blockedUntil =
          retryAfter !== undefined
            ? io.now() + retryAfter * 1000
            : (resetMs ?? io.now() + (await this.github.waitForReset("code_search")));
        continue;
      }
      throw new Error(firstLine(result.stderr) || `HTTP ${answer.status}`);
    }
  }
}

function newRecord(job: SearchJob): QueryRecord {
  return {
    type: "search",
    name: job.name,
    q: `${searchTerms(job.name)} ${job.scope}`,
    scope: job.scope,
    reason: job.reason,
    status: "not run",
    total_count: null,
    incomplete_results: null,
    pages: 0,
    calls: 0,
    rate_limit_retries: 0,
    items_read: 0,
    unread: 0,
    hits: [],
    error: "",
  };
}

/** Runs one search job, every page up to GitHub's 1,000 results; one an earlier run finished
 *  (`--cache-dir`) is taken from the cache, with no call, and one finished here is written to
 *  it. */
async function runSearch(search: CodeSearch, job: SearchJob, record: QueryRecord): Promise<void> {
  const cached = search.cache.get("search", record.q, CachedSearch);
  if (cached !== undefined) {
    Object.assign(record, cached, {
      hits: cached.hits.map((hit) => ({ ...hit, verdict: "", match: "" })),
    });
    return;
  }
  for (let page = 1; page <= 10; page++) {
    let answer: SearchPage | undefined;
    try {
      answer = await search.page(job, record.q, page, record);
    } catch (error) {
      rethrowStop(error);
      record.status = "failed";
      record.error = (error as Error).message;
      return;
    }
    if (answer === undefined) {
      record.status = page === 1 ? "not run" : "partial";
      record.error = `the search-call budget (${search.budget}) is spent`;
      record.unread = Math.max(0, (record.total_count ?? 0) - record.items_read);
      return;
    }
    record.pages = page;
    record.total_count = answer.total;
    record.incomplete_results = (record.incomplete_results ?? false) || answer.incomplete;
    for (const item of answer.items) {
      record.hits.push({
        repository: item.repository.full_name,
        path: item.path,
        verdict: "",
        match: "",
      });
    }
    record.items_read += answer.items.length;
    if (answer.items.length < 100 || record.items_read >= Math.min(answer.total, 1000)) break;
  }
  record.status = "ok";
  record.unread = Math.max(0, (record.total_count ?? 0) - record.items_read);
  search.cache.put("search", record.q, {
    status: record.status,
    total_count: record.total_count,
    incomplete_results: record.incomplete_results,
    pages: record.pages,
    calls: record.calls,
    rate_limit_retries: record.rate_limit_retries,
    items_read: record.items_read,
    unread: record.unread,
    hits: record.hits.map(({ repository, path: file }) => ({ repository, path: file })),
  });
}

/**
 * The searches an owner-wide search (`job`, which ran as `record`) is run again as: one per
 * repository of that owner in `holders` (each repository holding a skill, forks left out since
 * code search does not index them), or none.
 *   - A search whose `total_count` reaches GitHub's cap of 1,000 results is run again, whatever
 *     its name: the cap hid hits.
 *   - A search of a name in COMMON_WORD_NAMES is run again only when it was not read whole: it
 *     failed, GitHub marked it `incomplete_results`, or it left hits unread. An owner-wide search
 *     (`org:` or `user:`) covers each repository of the owner, so a search of the same terms in
 *     one of them gives a subset of its hits; once every hit of the owner-wide search was read,
 *     the per-repository searches can give no file it did not. A common word is named here
 *     because its owner-wide search is the one likely to be cut short, not because its hits
 *     differ.
 * A search the call budget left not run is not run again: the budget is spent.
 */
function rerunsOf(job: SearchJob, record: QueryRecord, holders: readonly string[]): SearchJob[] {
  if (job.reason !== "owner" || record.status === "not run") return [];
  const capped = (record.total_count ?? 0) >= 1000;
  const readWhole =
    record.status === "ok" && record.incomplete_results === false && record.unread === 0;
  const common = Object.hasOwn(COMMON_WORD_NAMES, job.name);
  if (!capped && !(common && !readWhole)) return [];
  const reason = capped
    ? "run again per repository: total_count reached 1,000"
    : "run again per repository: a common word whose owner-wide search was not read whole";
  return holders
    .filter((repository) => repository.split("/")[0] === job.owner)
    .map((repository) => ({
      name: job.name,
      scope: `repo:${repository}`,
      owner: job.owner,
      reason,
    }));
}

/** A search job but its name: the scope a name is searched in. */
type SearchScope = Omit<SearchJob, "name">;

/** A search queued (`SearchQueues`): its job and record, its scope's place among the run's
 *  scopes, the follow-ups it gave, and whether it ran. */
interface QueuedSearch {
  readonly job: SearchJob;
  readonly record: QueryRecord;
  readonly scopeIndex: number;
  readonly followUps: QueuedSearch[];
  ran: boolean;
}

/** One owner's searches in the order they were queued, the next to run, the run of them, and how
 *  to wake that run while it waits for more. */
interface OwnerQueue {
  readonly entries: QueuedSearch[];
  next: number;
  wake: (() => void) | undefined;
  running: Promise<void>;
}

/** Queued searches by name (as `Array.prototype.sort` orders strings), then by scope. */
function byNameThenScope(a: QueuedSearch, b: QueuedSearch): number {
  if (a.job.name !== b.job.name) return a.job.name < b.job.name ? -1 : 1;
  return a.scopeIndex - b.scopeIndex;
}

/**
 * The code searches of a run, started before its trees are all read: one queue per owner, each
 * running its searches one at a time in the order they were queued, and the owners' at once, so
 * each owner's token is paced by its own limit while the others' run (`CodeSearch`). `add` queues
 * a search of each name a repository's step gives that no step gave before, in each scope;
 * `finish`, once every tree is read, queues each search's follow-ups (`rerunsOf`, which needs
 * every repository that holds a skill) at the end of its owner's queue, waits for every queue,
 * and returns the records in the order the output writes them: the names sorted, each in scope
 * order, then each owner's follow-ups, in its names' order. A record depends only on its query,
 * and a search's follow-ups on its record, so they are those of searching after the trees, unless
 * the call budget stops the searches: then the order they ran in decides which are not run.
 */
class SearchQueues {
  private readonly queues = new Map<string, OwnerQueue>();
  private readonly base: QueuedSearch[] = [];
  private readonly named = new Set<string>();
  private followUps: ((job: SearchJob, record: QueryRecord) => readonly SearchJob[]) | undefined;
  private closed = false;
  private stopped = false;
  private queued = 0;
  private done = 0;
  private readonly started: number;

  constructor(
    private readonly search: CodeSearch,
    private readonly scopes: readonly SearchScope[],
    private readonly io: CopiesIo
  ) {
    this.started = io.now();
  }

  /** Queues a search of each of `names` not queued yet, in each scope, the names sorted. */
  add(names: readonly string[]): void {
    for (const name of [...new Set(names)].sort()) {
      if (this.named.has(name)) continue;
      this.named.add(name);
      for (const [scopeIndex, scope] of this.scopes.entries()) {
        const job = { name, ...scope };
        const entry: QueuedSearch = {
          job,
          record: newRecord(job),
          scopeIndex,
          followUps: [],
          ran: false,
        };
        this.base.push(entry);
        this.enqueue(entry);
      }
    }
  }

  /** Once every tree is read: queues the follow-ups `followUps` gives each search, in that
   *  search's owner's queue (those of a search that ran already now, the others' as each ends),
   *  lets each queue end once it is empty, and returns every record in the order the output
   *  writes them. Throws what a search threw. */
  async finish(
    followUps: (job: SearchJob, record: QueryRecord) => readonly SearchJob[]
  ): Promise<QueryRecord[]> {
    this.followUps = followUps;
    const base = [...this.base].sort(byNameThenScope);
    for (const entry of base) if (entry.ran) this.queueFollowUps(entry);
    this.closed = true;
    for (const queue of this.queues.values()) queue.wake?.();
    await Promise.all([...this.queues.values()].map((queue) => queue.running));
    const withFollowUps = (entry: QueuedSearch): QueuedSearch[] => [
      entry,
      ...entry.followUps.flatMap(withFollowUps),
    ];
    const followed = [...base]
      .sort((a, b) => a.scopeIndex - b.scopeIndex)
      .flatMap((entry) => entry.followUps.flatMap(withFollowUps));
    return [...base, ...followed].map((entry) => entry.record);
  }

  /** Stops each queue before its next search: the run ends with an error. */
  stop(): void {
    this.stopped = true;
    for (const queue of this.queues.values()) queue.wake?.();
  }

  private enqueue(entry: QueuedSearch): void {
    this.queued++;
    const queue = this.queues.get(entry.job.owner);
    if (queue !== undefined) {
      queue.entries.push(entry);
      queue.wake?.();
      return;
    }
    const started: OwnerQueue = {
      entries: [entry],
      next: 0,
      wake: undefined,
      running: Promise.resolve(),
    };
    this.queues.set(entry.job.owner, started);
    started.running = this.run(started);
    // A search that throws (the run's memory stop) stops every queue; `finish` throws it again.
    started.running.catch(() => this.stop());
  }

  /** Runs `queue`'s searches as they come, until it is empty and no more will come. */
  private async run(queue: OwnerQueue): Promise<void> {
    for (;;) {
      if (this.stopped) return;
      const entry = queue.entries[queue.next];
      if (entry === undefined) {
        if (this.closed) return;
        const more = Promise.withResolvers<void>();
        queue.wake = more.resolve;
        await more.promise;
        queue.wake = undefined;
        continue;
      }
      queue.next++;
      // runSearch takes a cached search first, then stops at the budget (status "not run").
      await runSearch(this.search, entry.job, entry.record);
      entry.ran = true;
      this.queueFollowUps(entry);
      this.done++;
      if (this.done % 25 === 0) {
        const minutes = Math.round((this.io.now() - this.started) / MINUTE_MS);
        this.io.err(
          `copies: ${this.done}/${this.queued} searches, ${this.search.calls} calls, ${minutes} min`
        );
      }
    }
  }

  /** Queues the follow-ups of `entry`, which ran, once `finish` gave how to know them. */
  private queueFollowUps(entry: QueuedSearch): void {
    if (this.followUps === undefined) return;
    for (const job of this.followUps(entry.job, entry.record)) {
      const next: QueuedSearch = {
        job,
        record: newRecord(job),
        scopeIndex: entry.scopeIndex,
        followUps: [],
        ran: false,
      };
      entry.followUps.push(next);
      this.enqueue(next);
    }
  }
}

// ---------------------------------------------------------------------------------------------
// forks read from their trees

/** The largest text blob a fork's scan reads, in bytes. */
export const FORK_SCAN_MAX_BYTES = 100_000;
/** The extensions of the text files a fork's scan reads. */
export const TEXT_EXTENSIONS: Readonly<Record<string, true>> = {
  ".md": true,
  ".mdx": true,
  ".txt": true,
  ".rst": true,
  ".ts": true,
  ".tsx": true,
  ".js": true,
  ".mjs": true,
  ".cjs": true,
  ".jsx": true,
  ".json": true,
  ".jsonc": true,
  ".yaml": true,
  ".yml": true,
  ".toml": true,
  ".ini": true,
  ".cfg": true,
  ".conf": true,
  ".py": true,
  ".sh": true,
  ".bash": true,
  ".zsh": true,
  ".fish": true,
  ".go": true,
  ".rs": true,
  ".rb": true,
  ".lua": true,
  ".nix": true,
  ".el": true,
  ".vim": true,
  ".tmpl": true,
};
/** Whole file names a fork's scan reads beside the extensions. */
export const TEXT_NAMES: Readonly<Record<string, true>> = {
  Dockerfile: true,
  Makefile: true,
  Justfile: true,
  AGENTS: true,
  README: true,
};

// ---------------------------------------------------------------------------------------------
// loaders and the readers' seam

/** One file outside a skill that names a skill. */
export interface LoaderRow {
  readonly repository: string;
  readonly path: string;
  readonly skill: string;
  match: string;
  source: string;
}

/** The copy of `copies` (one repository's) that `file` is inside: under its directory, or, for a
 *  skill at the root, its SKILL.md alone. */
function insideSkill<T extends Pick<SkillCopy, "path" | "file">>(
  copies: readonly T[],
  file: string
): T | undefined {
  return copies.find((copy) =>
    copy.path === "" ? file === copy.file : file.startsWith(`${copy.path}/`)
  );
}

/** The skills list for the same-job and follow-the-code readers: each copy with its whole
 *  description. It is written to skills.jsonl; no command reads it, and no model is called here. */
export interface ReaderSkill {
  readonly repository: string;
  readonly path: string;
  readonly file: string;
  readonly name: string;
  readonly description: string;
  readonly fork: boolean;
  readonly archived: boolean;
}

export function readerSkills(copies: readonly SkillCopy[]): ReaderSkill[] {
  return copies.map((copy) => ({
    repository: copy.repository,
    path: shown(copy.path),
    file: copy.file,
    name: copy.name,
    description: copy.description,
    fork: copy.fork,
    archived: copy.archived,
  }));
}

/** Each name in two or more repositories, or twice in one, with its copies. */
export function sharedNames(
  copies: readonly SkillCopy[]
): { name: string; copies: SkillCopy[]; why: string }[] {
  const byName = new Map<string, SkillCopy[]>();
  for (const copy of copies) byName.set(copy.name, [...(byName.get(copy.name) ?? []), copy]);
  const shared: { name: string; copies: SkillCopy[]; why: string }[] = [];
  for (const [name, named] of byName) {
    const repositories = [...new Set(named.map((copy) => copy.repository))];
    const twice = repositories.filter(
      (repository) => named.filter((copy) => copy.repository === repository).length > 1
    );
    const why = [
      repositories.length > 1 ? `in ${repositories.length} repositories` : "",
      twice.length > 0 ? `twice or more in ${twice.join(", ")}` : "",
    ].filter((part) => part !== "");
    if (why.length > 0) shared.push({ name, copies: named, why: why.join("; ") });
  }
  return shared.sort((a, b) => a.name.localeCompare(b.name));
}

// ---------------------------------------------------------------------------------------------
// output

export const SKILL_COPIES_COLUMNS = [
  "repository",
  "path",
  "name",
  "description_bytes",
  "fork",
  "archived",
  "parent",
  "installers",
  "loaded_by",
] as const;
export const PREFIX_TABLE_COLUMNS = ["prefix", "repository", "path", "name", "via"] as const;
export const REPOSITORIES_COLUMNS = [
  "repository",
  "branch",
  "commit",
  "fork",
  "archived",
  "parent",
  "tree_entries",
  "tree_read",
  "skills",
  "error",
] as const;
export const LOADERS_COLUMNS = ["repository", "path", "skill", "match", "source"] as const;
export const INSTALLERS_COLUMNS = [
  "manifest_repository",
  "manifest_path",
  "kind",
  "action",
  "unit",
  "host",
  "skill",
  "copy_repository",
  "copy_path",
  "note",
] as const;
export const MANIFESTS_COLUMNS = [
  "repository",
  "path",
  "kind",
  "source",
  "rows",
  "says",
  "error",
] as const;

const CELL_ESCAPES: Readonly<Record<string, string>> = {
  "\\": "\\\\",
  "\t": "\\t",
  "\n": "\\n",
  "\r": "\\r",
};
const CELL_UNESCAPES: Readonly<Record<string, string>> = { "\\": "\\", t: "\t", n: "\n", r: "\r" };

/** A TSV cell: a backslash, tab, line feed or carriage return written as `\\`, `\t`, `\n`, `\r`. */
export function tsvCell(value: string | number | boolean): string {
  return String(value).replace(/[\\\t\n\r]/g, (char) => CELL_ESCAPES[char] ?? char);
}

/** A TSV of `columns` and `rows`, the header row first. */
export function tsv(
  columns: readonly string[],
  rows: readonly (readonly (string | number | boolean)[])[]
): string {
  return `${[columns, ...rows].map((row) => row.map(tsvCell).join("\t")).join("\n")}\n`;
}

/** The rows of a TSV this file writes, each keyed by the header row's column names. */
export function parseTsv(text: string): Record<string, string>[] {
  const lines = text.split("\n").filter((line) => line !== "");
  const header = (lines[0] ?? "").split("\t");
  return lines.slice(1).map((line) => {
    const cells = line.split("\t");
    return Object.fromEntries(
      header.map((column, index) => [
        column,
        (cells[index] ?? "").replace(
          /\\([\\tnr])/g,
          (_, char: string) => CELL_UNESCAPES[char] ?? char
        ),
      ])
    );
  });
}

const yesNo = (value: boolean) => (value ? "yes" : "no");

/** A copy's or repository's fork and archived marks, in parentheses, or `""`. */
function marks(copy: Pick<SkillCopy, "fork" | "archived" | "parent">): string {
  const said = [
    copy.fork ? `fork of ${copy.parent || "an unnamed parent"}` : "",
    copy.archived ? "archived" : "",
  ]
    .filter((part) => part !== "")
    .join(", ");
  return said === "" ? "" : ` (${said})`;
}

/** Everything one run read and found; `main` writes it. */
export interface CopiesResult {
  readonly at: string;
  readonly scope: string;
  readonly listingErrors: { owner: string; error: string }[];
  readonly missing: string[];
  readonly repositories: Repository[];
  readonly copies: SkillCopy[];
  readonly excluded: { repository: string; file: string }[];
  readonly manifests: Manifest[];
  readonly notOfKind: Map<string, number>;
  readonly rows: InstallerRow[];
  /** Notes on manifests whose skills this list cannot hold (`Resolved.notes`). */
  readonly notes: ManifestNote[];
  readonly prefixes: PrefixRow[];
  readonly offTargets: PrefixRow[];
  readonly targetsUnused: string[];
  readonly loaders: LoaderRow[];
  readonly records: QueryRecord[];
  readonly forkScans: ForkScanRecord[];
  readonly forksNotScanned: string[];
  readonly failures: string[];
  readonly searchCalls: number;
  /** The run's peak resident memory, in bytes. */
  readonly peakBytes: number;
  readonly budget: number;
}

function installersCell(rows: readonly InstallerRow[], copy: SkillCopy): string {
  const items = new Set(
    rows
      .filter(
        (row) =>
          row.copyRepository === copy.repository &&
          row.copyPath === copy.path &&
          row.action !== "loads"
      )
      .map((row) => `${row.action} ${row.unit} (${row.manifestRepository}:${row.manifestPath})`)
  );
  return [...items].sort().join("; ");
}

function loadedByCell(
  rows: readonly InstallerRow[],
  loaders: readonly LoaderRow[],
  copy: SkillCopy
): string {
  const items = new Set<string>();
  for (const row of rows) {
    if (
      row.action === "loads" &&
      row.copyRepository === copy.repository &&
      row.copyPath === copy.path
    ) {
      items.add(`${row.manifestRepository}:${row.manifestPath} (${row.kind} ${row.unit})`);
    }
  }
  for (const loader of loaders) {
    if (loader.skill === copy.name && loader.match.split(",").includes("code-load")) {
      items.add(`${loader.repository}:${loader.path} (code-load, ${loader.source})`);
    }
  }
  return [...items].sort().join("; ");
}

function markdown(result: CopiesResult): string {
  const lines: string[] = [];
  const say = (...text: string[]) => lines.push(...text);
  const readable = result.repositories.filter((repo) => repo.error === "");
  const failed = result.repositories.filter((repo) => repo.error !== "");
  say(
    "# Skill copies and loaders",
    "",
    `Each repository is read at its default branch's last commit at or before ${result.at}. ${result.scope}`,
    "",
    `${result.repositories.length} repositories listed: ${readable.length} read, ${failed.length} not read. ` +
      `${result.copies.length} skill copies, ${new Set(result.copies.map((copy) => copy.name)).size} names. ` +
      `${result.manifests.length} manifests, ${result.rows.length} installer rows, ${result.loaders.length} loader rows. ` +
      `${result.searchCalls} code search calls (budget ${Number.isFinite(result.budget) ? result.budget : "none"}). ` +
      `Peak resident memory ${Math.round(result.peakBytes / 1024 ** 2)} MiB (the run stops above ${MEMORY_LIMIT_BYTES / 1024 ** 2} MiB).`,
    "",
    "Same-job pairs and the follow-the-code sentence are not in this list: the `read` command's `same-job` and `sentence` readers find them, from an input file of their own shape. skills.jsonl holds each copy with its whole description.",
    "",
    "## Repositories that could not be listed or read",
    ""
  );
  if (result.listingErrors.length + result.missing.length + failed.length === 0) say("None.");
  for (const { owner, error } of result.listingErrors)
    say(`- owner ${owner}: the listing failed: ${error}`);
  for (const repository of result.missing) say(`- ${repository}: not in its owner's listing`);
  for (const repo of failed) say(`- ${repo.repository}${marks(repo)}: ${repo.error}`);
  say("", "## Repositories read", "");
  for (const repo of readable) {
    const count = result.copies.filter((copy) => copy.repository === repo.repository).length;
    say(
      `- ${repo.repository}${marks(repo)} at ${repo.commit} (${repo.branch}; tree read ${repo.treeRead}): ${count} skills`
    );
  }
  if (result.excluded.length > 0) {
    const where = [...new Set(result.excluded.map((entry) => entry.repository))].join(", ");
    say("", `Left out by excludePaths: ${result.excluded.length} SKILL.md files (${where}).`);
  }
  say("", "## Shared names", "");
  const shared = sharedNames(result.copies);
  if (shared.length === 0) say("None.");
  for (const entry of shared) {
    const copies = entry.copies
      .map((copy) => `${copy.repository} ${shown(copy.path)}${marks(copy)}`)
      .join("; ");
    say(`- \`${entry.name}\` (${entry.why}): ${copies}`);
  }
  say("", "## Installers and manifests", "");
  for (const manifest of result.manifests) {
    const found = manifest.source === "tree" ? "" : `, found by ${manifest.source}`;
    const says = manifest.error === "" ? manifest.says : `read failed: ${manifest.error}`;
    say(
      `- ${manifest.repository}:${manifest.path} (${manifest.kind}${found}): ${says} [${manifest.rows} rows]`
    );
    for (const { note } of result.notes.filter(
      (candidate) =>
        candidate.repository === manifest.repository && candidate.path === manifest.path
    ))
      say(`  - ${note}`);
  }
  if (result.notOfKind.size > 0) {
    const counts = [...result.notOfKind].map(([kind, count]) => `${count} ${kind}`).join(", ");
    say("", `Read by path and naming no skill, plugin or package, so not listed: ${counts}.`);
  }
  say("", "Installer rows, by copy (installers.tsv holds each row):", "");
  for (const copy of result.copies) {
    const cell = installersCell(result.rows, copy);
    if (cell !== "") say(`- ${copy.repository} ${shown(copy.path)} (\`${copy.name}\`): ${cell}`);
  }
  const unresolved = result.rows.filter((row) => row.copyPath === undefined);
  if (unresolved.length > 0) {
    say("", "Installer rows that reach no copy read:", "");
    for (const row of unresolved) {
      say(
        `- ${row.manifestRepository}:${row.manifestPath} ${row.action} ${row.unit}${row.skill ? ` \`${row.skill}\`` : ""}: ${row.note}`
      );
    }
  }
  say("", "## Loaders: files outside a skill that name a skill", "");
  const bySkill = new Map<string, LoaderRow[]>();
  for (const loader of result.loaders)
    bySkill.set(loader.skill, [...(bySkill.get(loader.skill) ?? []), loader]);
  if (bySkill.size === 0) say("None found.");
  for (const [skill, found] of bySkill) {
    say(
      `- \`${skill}\`: ${found.map((loader) => `${loader.repository}:${loader.path} (${loader.match}; ${loader.source})`).join("; ")}`
    );
  }
  say("", "## Code search", "");
  const counts = new Map<string, number>();
  for (const record of result.records)
    counts.set(record.status, (counts.get(record.status) ?? 0) + 1);
  say(
    `${result.records.length} queries: ${[...counts].map(([status, count]) => `${count} ${status}`).join(", ") || "none"}. ` +
      "queries.jsonl holds each query, its total_count and incomplete_results, every hit with its verdict, and every failure."
  );
  for (const record of result.records.filter((candidate) => candidate.status === "failed"))
    say(`- failed: ${record.q}: ${record.error}`);
  for (const record of result.records.filter((candidate) => candidate.unread > 0)) {
    const why =
      record.status === "partial"
        ? "the budget ended it"
        : (record.total_count ?? 0) > 1000
          ? "GitHub gives at most 1,000 results a query"
          : "GitHub's pages held fewer hits than its count";
    say(
      `- ${record.q}: ${record.total_count} hits, ${record.items_read} read, ${record.unread} not read (${why})`
    );
  }
  for (const scan of result.forkScans) {
    say(
      `- the fork ${scan.repository} (${scan.read}): ${scan.files_read} text files of at most ${scan.max_bytes} bytes read, ` +
        `${scan.files_naming_a_skill} naming a skill, ${scan.skipped_large} larger left out, ${scan.failures.length} reads failed`
    );
  }
  if (result.forksNotScanned.length > 0)
    say(
      `- forks holding no skill, so not read from their trees: ${result.forksNotScanned.join(", ")}`
    );
  say(
    "",
    "## Prefix table",
    "",
    `${result.prefixes.length} rows in prefix-table.tsv, each to a repository the configuration's targets name.`
  );
  if (result.offTargets.length > 0) {
    const repositories = [...new Set(result.offTargets.map((row) => row.repository))].sort();
    say(
      "",
      `${result.offTargets.length} rows point at repositories the configuration's targets do not name, so they are not in the table: ` +
        `${repositories.join(", ")}. Add each to targets to keep its rows:`,
      ""
    );
    for (const row of result.offTargets)
      say(`- \`${row.prefix}\` -> ${row.repository} ${shown(row.path)}`);
  }
  if (result.targetsUnused.length > 0)
    say("", `Targets no row maps to: ${result.targetsUnused.join(", ")}.`);
  say("", "## Failures", "");
  if (result.failures.length === 0) say("None.");
  for (const failure of result.failures) say(`- ${failure}`);
  return `${lines.join("\n")}\n`;
}

/** Writes every output file under `out`; returns their paths. */
export function writeOutputs(out: string, result: CopiesResult): string[] {
  mkdirSync(out, { recursive: true });
  const copiesOf = (repository: string) =>
    result.copies.filter((copy) => copy.repository === repository).length;
  const files: [string, string][] = [
    [
      "skill-copies.tsv",
      tsv(
        SKILL_COPIES_COLUMNS,
        result.copies.map((copy) => [
          copy.repository,
          shown(copy.path),
          copy.name,
          copy.descriptionBytes,
          yesNo(copy.fork),
          yesNo(copy.archived),
          copy.parent,
          installersCell(result.rows, copy),
          loadedByCell(result.rows, result.loaders, copy),
        ])
      ),
    ],
    [
      "prefix-table.tsv",
      tsv(
        PREFIX_TABLE_COLUMNS,
        result.prefixes.map((row) => [
          row.prefix,
          row.repository,
          shown(row.path),
          row.name,
          row.via,
        ])
      ),
    ],
    [
      "repositories.tsv",
      tsv(
        REPOSITORIES_COLUMNS,
        result.repositories.map((repo) => [
          repo.repository,
          repo.branch,
          repo.commit,
          yesNo(repo.fork),
          yesNo(repo.archived),
          repo.parent,
          repo.treeEntries,
          repo.treeRead,
          copiesOf(repo.repository),
          repo.error,
        ])
      ),
    ],
    [
      "loaders.tsv",
      tsv(
        LOADERS_COLUMNS,
        result.loaders.map((loader) => [
          loader.repository,
          loader.path,
          loader.skill,
          loader.match,
          loader.source,
        ])
      ),
    ],
    [
      "installers.tsv",
      tsv(
        INSTALLERS_COLUMNS,
        result.rows.map((row) => [
          row.manifestRepository,
          row.manifestPath,
          row.kind,
          row.action,
          row.unit,
          row.host,
          row.skill,
          row.copyRepository,
          row.copyPath === undefined ? "" : shown(row.copyPath),
          row.note,
        ])
      ),
    ],
    [
      "manifests.tsv",
      tsv(
        MANIFESTS_COLUMNS,
        result.manifests.map((manifest) => [
          manifest.repository,
          manifest.path,
          manifest.kind,
          manifest.source,
          manifest.rows,
          manifest.says,
          manifest.error,
        ])
      ),
    ],
    [
      "queries.jsonl",
      [...result.records, ...result.forkScans]
        .map((record) => `${JSON.stringify(record)}\n`)
        .join(""),
    ],
    [
      "skills.jsonl",
      readerSkills(result.copies)
        .map((skill) => `${JSON.stringify(skill)}\n`)
        .join(""),
    ],
    ["skill-copies.md", markdown(result)],
  ];
  for (const [name, text] of files) writeFileSync(path.join(out, name), text);
  return files.map(([name]) => path.join(out, name));
}

// ---------------------------------------------------------------------------------------------
// the run

/** The options of one run. */
export interface CopiesOptions {
  readonly at: string;
  readonly only: readonly string[];
  readonly maxSearchCalls: number;
  /** `--cache-dir`: where finished reads are kept, and read back by a run started again. */
  readonly cacheDir?: string | undefined;
  /** How many repositories are read at once (REPOSITORY_JOBS when absent); the output is the same
   *  whatever it is. */
  readonly repositoryJobs?: number | undefined;
}

/** Reads the repositories, skills, manifests and loaders of one run. */
export async function listCopies(
  config: SkillCutsConfig,
  options: CopiesOptions,
  io: CopiesIo
): Promise<CopiesResult> {
  const cache = new ReadCache(options.cacheDir);
  try {
    return await readAll(config, options, io, cache);
  } finally {
    cache.dispose();
  }
}

async function readAll(
  config: SkillCutsConfig,
  options: CopiesOptions,
  io: CopiesIo,
  cache: ReadCache
): Promise<CopiesResult> {
  const github = new Github(io, () =>
    io.peakRssBytes === undefined ? procPeakRssBytes() : io.peakRssBytes()
  );
  const failures: string[] = [];
  const only = options.only.map((repository) => repository.toLowerCase());
  const owners =
    only.length > 0
      ? [...new Set(options.only.map((repository) => repository.split("/")[0] ?? ""))]
      : config.owners;

  // 1. Repositories.
  const listingErrors: { owner: string; error: string }[] = [];
  let repositories: Repository[] = [];
  for (const owner of owners) {
    io.err(`copies: listing ${owner}`);
    try {
      const listed =
        cache.get("listing", owner, Listing) ??
        (await github.json(
          [
            "repo",
            "list",
            owner,
            "--limit",
            "2000",
            "--json",
            "nameWithOwner,isFork,isArchived,defaultBranchRef,parent",
          ],
          Listing
        ));
      cache.put("listing", owner, listed);
      for (const entry of listed) {
        repositories.push({
          repository: entry.nameWithOwner,
          owner: entry.nameWithOwner.split("/")[0] ?? "",
          fork: entry.isFork,
          archived: entry.isArchived,
          parent: entry.parent === null ? "" : `${entry.parent.owner.login}/${entry.parent.name}`,
          branch: entry.defaultBranchRef?.name ?? "",
          commit: "",
          tree: [],
          treeEntries: 0,
          treeRead: "",
          forkRead: "",
          changed: undefined,
          error: "",
        });
      }
    } catch (error) {
      rethrowStop(error);
      listingErrors.push({ owner, error: (error as Error).message });
    }
  }
  const missing: string[] = [];
  if (only.length > 0) {
    repositories = repositories.filter((repo) => only.includes(repo.repository.toLowerCase()));
    const found = new Set(repositories.map((repo) => repo.repository.toLowerCase()));
    missing.push(...options.only.filter((repository) => !found.has(repository.toLowerCase())));
  }
  repositories.sort((a, b) => a.repository.localeCompare(b.repository));

  // Step 5's scopes are known before any tree is read, so its searches start beside the reads.
  const search = new CodeSearch(github, options.maxSearchCalls, cache);
  const scopes: SearchScope[] = [];
  // A `users/<owner>` call that fails goes to `failures` where step 5 is, after the manifests'.
  const ownerFailures: string[] = [];
  if (only.length > 0) {
    for (const repo of repositories) {
      if (!repo.fork)
        scopes.push({ scope: `repo:${repo.repository}`, owner: repo.owner, reason: "--only" });
    }
  } else {
    // Each owner whose listing succeeded: one not listed has no repository read, so no hit there
    // could be checked.
    const listed = owners.filter(
      (owner) => !listingErrors.some((failed) => failed.owner === owner)
    );
    for (const owner of listed) {
      let qualifier = "user";
      try {
        const type =
          cache.get("owner-type", owner, z.string()) ??
          (await github.api(`users/${owner}`, GithubUser)).type;
        cache.put("owner-type", owner, type);
        if (type === "Organization") qualifier = "org";
      } catch (error) {
        rethrowStop(error);
        ownerFailures.push(
          `users/${owner}: ${(error as Error).message}; searched as user:${owner}`
        );
      }
      scopes.push({ scope: `${qualifier}:${owner}`, owner, reason: "owner" });
    }
  }
  const searches = new SearchQueues(search, scopes, io);

  io.err(`copies: reading ${repositories.length} repositories at ${options.at}`);
  // Each repository whole, read or taken from the cache (`readRepositoryStep`), written to the
  // cache the moment it is done; the run keeps only what the later steps and the output need of
  // it (`RepositoryRead`), so its memory does not grow with the blobs it read.
  const reads = new Map<Repository, RepositoryRead>();
  let done = 0;
  // Repositories finish in any order; each result lands on its own repository, and every later
  // step goes through them in name order, so the output does not depend on that order.
  try {
    await mapLimit(repositories, options.repositoryJobs ?? REPOSITORY_JOBS, async (repo) => {
      const started = io.now();
      const step = await readRepositoryStep(github, cache, repo, options.at, config.excludePaths);
      if (step.read !== undefined) {
        reads.set(repo, step.read);
        // Its skills' names are searched now, beside the trees still being read.
        searches.add(step.read.copies.map((copy) => copy.name));
      }
      done++;
      const seconds = Math.round((io.now() - started) / 1000);
      io.err(
        repo.error === ""
          ? `copies: tree ${done}/${repositories.length} ${repo.repository}: ${repo.treeEntries} entries, ${step.calls} tree reads, ${seconds} s, ${step.cached ? "cached" : "read"}`
          : `copies: tree ${done}/${repositories.length} ${repo.repository}: failed (${repo.error}), ${seconds} s`
      );
    });
  } catch (error) {
    searches.stop();
    throw error;
  }

  // 2. Skills, parsed in each repository's step.
  const excluded: { repository: string; file: string }[] = [];
  const copies: SkillCopy[] = [];
  const read: Manifest[] = [];
  const manifestFailures: string[] = [];
  for (const repo of repositories) {
    const kept = reads.get(repo);
    if (kept === undefined) continue;
    for (const file of kept.excluded) excluded.push({ repository: repo.repository, file });
    copies.push(...kept.copies);
    read.push(...kept.manifests);
    failures.push(...kept.skillFailures);
    manifestFailures.push(...kept.manifestFailures);
  }
  github.checkMemory();
  io.err(`copies: reading ${copies.length} SKILL.md files`);
  copies.sort((a, b) => a.repository.localeCompare(b.repository) || a.path.localeCompare(b.path));
  const copiesByRepository = new Map<string, SkillCopy[]>();
  for (const copy of copies)
    copiesByRepository.set(copy.repository, [
      ...(copiesByRepository.get(copy.repository) ?? []),
      copy,
    ]);
  const copiesIn = (repository: string) => copiesByRepository.get(repository) ?? [];
  const withSkill = new Set(
    [...reads]
      .filter(([, kept]) => kept.copies.length + kept.excluded.length > 0)
      .map(([repo]) => repo.repository)
  );

  // 4. Manifests, parsed in each repository's step.
  github.checkMemory();
  io.err(`copies: reading ${read.length} manifest files`);
  failures.push(...manifestFailures);
  // Every manifest read is resolved (a package.json without omp.skills still names the package an
  // npm-sourced plugin comes from); only those `listedManifest` takes are listed.
  const manifests = read.filter(listedManifest);
  const notOfKind = new Map<string, number>();
  for (const manifest of read) {
    if (!listedManifest(manifest))
      notOfKind.set(manifest.kind, (notOfKind.get(manifest.kind) ?? 0) + 1);
  }

  // 5. Code search: each name, with the plugins that ship it. Its searches started beside the
  // tree reads (`SearchQueues`): one per name and scope (`searchTerms` says why one stands for all
  // five patterns).
  failures.push(...ownerFailures);
  const names = [...new Set(copies.map((copy) => copy.name))].sort();
  const pluginsOf = new Map<string, string[]>();
  for (const unit of resolveInstallers(read, new CopyIndex(copies)).units) {
    if (!unit.id.startsWith("plugin:")) continue;
    const plugin = unit.id.slice("plugin:".length).split("@")[0] ?? "";
    for (const member of unit.members) {
      const known = pluginsOf.get(member.copy.name) ?? [];
      if (!known.includes(plugin)) pluginsOf.set(member.copy.name, [...known, plugin].sort());
    }
  }
  io.err(`copies: ${names.length * scopes.length} code searches for ${names.length} names`);
  // An owner's search that `rerunsOf` names is run again once per repository of that owner
  // holding a skill, queued behind that owner's own searches now that every tree is read.
  const holders = repositories
    .filter((repo) => !repo.fork && withSkill.has(repo.repository))
    .map((repo) => repo.repository);
  const records = await searches.finish((job, record) => rerunsOf(job, record, holders));

  // Each hit file, read by its path at its repository's commit (the trees keep only the entries
  // this list uses), and kept where it names the skill. Each file is read once for all the names
  // whose searches hit it, a batch of files at a time per repository, and dropped once checked.
  const byRepository = new Map(repositories.map((repo) => [repo.repository.toLowerCase(), repo]));
  const loaders = new Map<string, LoaderRow>();
  const addLoader = (
    repository: string,
    file: string,
    skill: string,
    kinds: readonly string[],
    source: string
  ) => {
    const key = `${repository}\0${file}\0${skill}`;
    const known = loaders.get(key);
    if (known === undefined) {
      loaders.set(key, { repository, path: file, skill, match: kinds.join(","), source });
      return;
    }
    known.match = [...new Set([...known.match.split(","), ...kinds])].join(",");
    if (!known.source.split(", ").includes(source)) known.source = `${known.source}, ${source}`;
  };
  const hits = records.flatMap((record) => record.hits.map((hit) => ({ record, hit })));
  github.checkMemory();
  io.err(`copies: checking ${hits.length} search hits`);
  const filesByRepository = new Map<
    Repository,
    Map<string, { record: QueryRecord; hit: SearchHit }[]>
  >();
  for (const { record, hit } of hits) {
    const repo = byRepository.get(hit.repository.toLowerCase());
    if (repo === undefined || repo.error !== "") {
      hit.verdict = "repository not read";
      continue;
    }
    const inside = insideSkill(copiesIn(repo.repository), hit.path);
    if (inside !== undefined) {
      hit.verdict = `inside the skill ${shown(inside.path)}`;
      continue;
    }
    let files = filesByRepository.get(repo);
    if (files === undefined) {
      files = new Map();
      filesByRepository.set(repo, files);
    }
    files.set(hit.path, [...(files.get(hit.path) ?? []), { record, hit }]);
  }
  const extraManifests: Manifest[] = [];
  const checkFile = (
    repo: Repository,
    file: string,
    body: string,
    named: { record: QueryRecord; hit: SearchHit }[]
  ) => {
    let kept = false;
    for (const { record, hit } of named) {
      const kinds = mentions(body, record.name, pluginsOf.get(record.name) ?? [], file);
      if (kinds.length === 0) {
        hit.verdict = body.includes(record.name)
          ? "dropped: only inside a longer name"
          : "dropped: the name is not in it";
        continue;
      }
      kept = true;
      hit.verdict = "kept";
      hit.match = kinds.join(",");
      addLoader(repo.repository, file, record.name, kinds, "code search");
    }
    const kind =
      manifestKind({ path: file, mode: "100644", type: "blob" }) ??
      (file.endsWith(".py") && SKILLS_LIST.test(body) ? "python-loader" : undefined);
    const known = [...manifests, ...extraManifests].some(
      (manifest) => manifest.repository === repo.repository && manifest.path === file
    );
    if (!kept || kind === undefined || known) return;
    try {
      const parsed = parseManifest(kind, file, body);
      if (parsed.ofKind) {
        extraManifests.push({
          repository: repo.repository,
          path: file,
          kind,
          source: "code search",
          ...parsed,
          error: "",
          rows: 0,
        });
      }
    } catch (error) {
      failures.push(
        `${repo.repository}:${file} (${kind}, a search hit): ${(error as Error).message}`
      );
    }
  };
  await mapLimit([...filesByRepository], REST_JOBS, async ([repo, files]) => {
    const paths = [...files.keys()];
    for (let start = 0; start < paths.length; start += BLOB_BATCH) {
      const chunk = paths.slice(start, start + BLOB_BATCH);
      const read = await github.filesAt(repo.repository, repo.commit, chunk);
      for (const file of chunk) {
        const named = files.get(file) ?? [];
        const found: FileRead = read.get(file) ?? { kind: "missing" };
        if (found.kind === "file" && found.text !== undefined) {
          checkFile(repo, file, found.text, named);
          continue;
        }
        if (found.kind === "failed")
          failures.push(`${repo.repository}:${file} (a search hit): ${found.error}`);
        const verdict =
          found.kind === "missing"
            ? `not in the tree at ${repo.commit}`
            : found.kind === "failed"
              ? `read failed: ${found.error}`
              : "dropped: a binary file";
        for (const { hit } of named) hit.verdict = verdict;
      }
    }
  });

  // Forks: read from their trees when they hold a skill, only the files each changed against its
  // parent where the compare answered whole (`forkChanges`).
  github.checkMemory();
  const forkScans: ForkScanRecord[] = [];
  const forksNotScanned: string[] = [];
  const longestFirst = [...names].sort((a, b) => b.length - a.length).map(escapeRegExp);
  const nameRe = new RegExp(`(?<!${NAME_CHAR})(${longestFirst.join("|")})(?!${NAME_CHAR})`, "g");
  for (const repo of repositories.filter((candidate) => candidate.fork && candidate.error === "")) {
    if (!withSkill.has(repo.repository) || names.length === 0) {
      forksNotScanned.push(repo.repository);
      continue;
    }
    const scan: ForkScanRecord = {
      type: "fork-scan",
      repository: repo.repository,
      read: repo.forkRead,
      changed_files: repo.changed === undefined ? null : repo.changed.length,
      max_bytes: FORK_SCAN_MAX_BYTES,
      files_read: 0,
      skipped_large: reads.get(repo)?.forkLarge ?? 0,
      skipped_binary: 0,
      files_naming_a_skill: 0,
      failures: [],
    };
    io.err(
      `copies: reading the fork ${repo.repository} from its tree (${reads.get(repo)?.forkFiles ?? 0} text files)`
    );
    // Its text files, from disk one at a time (its step wrote them), each dropped once matched.
    const key = repositoryKey(repo, options.at);
    const lines = cache.lines("fork-files", key);
    if (lines === undefined) scan.failures.push("its text files are not on disk");
    for await (const line of lines ?? []) {
      const parsed = ForkFileLine.safeParse(parseJson(line));
      if (!parsed.success) {
        scan.failures.push("a line of its text files on disk is not of its shape");
        continue;
      }
      const [file, sha, stored] = parsed.data;
      let body = stored;
      if (body === null) {
        try {
          body = await github.readBlob(repo.repository, sha);
        } catch (error) {
          rethrowStop(error);
          scan.failures.push(`${file}: ${(error as Error).message}`);
          continue;
        }
      }
      scan.files_read++;
      if (body.includes("\u0000")) {
        scan.skipped_binary++;
        continue;
      }
      let named = false;
      for (const name of new Set([...body.matchAll(nameRe)].map((match) => match[1] ?? ""))) {
        const kinds = mentions(body, name, pluginsOf.get(name) ?? [], file);
        if (kinds.length === 0) continue;
        named = true;
        addLoader(repo.repository, file, name, kinds, "fork scan");
      }
      if (named) scan.files_naming_a_skill++;
    }
    for (const failure of scan.failures) failures.push(`${repo.repository}:${failure} (fork scan)`);
    forkScans.push(scan);
  }

  // Resolve with the manifests the hits found too, and list what each manifest names as a loader.
  github.checkMemory();
  const allManifests = [...manifests, ...extraManifests].sort(
    (a, b) => a.repository.localeCompare(b.repository) || a.path.localeCompare(b.path)
  );
  const index = new CopyIndex(copies);
  const resolved = resolveInstallers([...read, ...extraManifests], index);
  for (const manifest of allManifests) {
    manifest.rows = resolved.rows.filter(
      (row) => row.manifestRepository === manifest.repository && row.manifestPath === manifest.path
    ).length;
  }
  for (const row of resolved.rows) {
    if (
      row.skill !== "" &&
      index.byName.has(row.skill) &&
      (row.action === "loads" || row.action === "stages" || row.action === "ignores")
    ) {
      addLoader(
        row.manifestRepository,
        row.manifestPath,
        row.skill,
        [`manifest:${row.kind}`],
        "manifest"
      );
    }
  }

  // 6. The prefix table, held to the configuration's targets.
  const targets = new Set(config.targets.map((target) => target.toLowerCase()));
  const prefixes = resolved.prefixes.filter((row) => targets.has(row.repository.toLowerCase()));
  const mapped = new Set(prefixes.map((row) => row.repository.toLowerCase()));

  return {
    at: options.at,
    scope:
      only.length > 0
        ? `Only ${options.only.join(", ")} (--only).`
        : `Owners: ${config.owners.join(", ")}.`,
    listingErrors,
    missing,
    repositories,
    copies,
    excluded,
    manifests: allManifests,
    notOfKind,
    rows: resolved.rows,
    notes: resolved.notes,
    prefixes,
    offTargets: resolved.prefixes.filter((row) => !targets.has(row.repository.toLowerCase())),
    targetsUnused: config.targets.filter((target) => !mapped.has(target.toLowerCase())),
    loaders: [...loaders.values()].sort(
      (a, b) =>
        a.skill.localeCompare(b.skill) ||
        a.repository.localeCompare(b.repository) ||
        a.path.localeCompare(b.path)
    ),
    records,
    forkScans,
    forksNotScanned,
    failures,
    // Each search's calls, those an earlier run made for a cached search included.
    searchCalls: records.reduce((sum, record) => sum + record.calls, 0),
    peakBytes: github.readPeak(),
    budget: options.maxSearchCalls,
  };
}

const USAGE =
  "usage: copies.ts --config <skill-cuts.json> --at <ISO instant> --out <dir> [--only <owner/repo>]... [--max-search-calls <n>] [--cache-dir <dir>]";

/** The command. Returns the exit status: 0 (the list is written), 1 (the run failed), 2 (refused),
 *  3 (stopped: the peak resident memory passed MEMORY_LIMIT_BYTES; nothing is written). */
export async function main(argv: readonly string[], io: CopiesIo): Promise<number> {
  const refuse = (why: string) => {
    io.err(`copies: ${why}`);
    return 2;
  };
  let values: {
    config?: string;
    at?: string;
    out?: string;
    only?: string[];
    "max-search-calls"?: string;
    "cache-dir"?: string;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        config: { type: "string" },
        at: { type: "string" },
        out: { type: "string" },
        only: { type: "string", multiple: true },
        "max-search-calls": { type: "string" },
        "cache-dir": { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch (error) {
    return refuse(`${(error as Error).message}. ${USAGE}`);
  }
  if (values.config === undefined || values.at === undefined || values.out === undefined) {
    return refuse(`--config, --at and --out are required. ${USAGE}`);
  }
  if (
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/.test(values.at) ||
    Number.isNaN(Date.parse(values.at))
  ) {
    return refuse(`--at ${JSON.stringify(values.at)} is not an ISO instant ending in Z`);
  }
  const only = values.only ?? [];
  const badRepository = only.find(
    (repository) => !/^[A-Za-z0-9][A-Za-z0-9-]*\/[A-Za-z0-9._-]+$/.test(repository)
  );
  if (badRepository !== undefined)
    return refuse(`--only ${JSON.stringify(badRepository)} is not owner/name`);
  const budget = values["max-search-calls"];
  if (budget !== undefined && !/^\d+$/.test(budget))
    return refuse(`--max-search-calls ${JSON.stringify(budget)} is not a whole number`);
  let config: SkillCutsConfig;
  try {
    config = loadConfig(values.config);
  } catch (error) {
    return refuse((error as Error).message);
  }
  try {
    const result = await listCopies(
      config,
      {
        at: values.at,
        only,
        maxSearchCalls: budget === undefined ? Number.POSITIVE_INFINITY : Number(budget),
        cacheDir: values["cache-dir"],
      },
      io
    );
    const written = writeOutputs(values.out, result);
    io.out(
      `${result.repositories.length} repositories, ${result.copies.length} skill copies, ${result.manifests.length} manifests, ` +
        `${result.loaders.length} loader rows, ${result.prefixes.length} prefix rows, ${result.searchCalls} code search calls, ` +
        `peak memory ${Math.round(result.peakBytes / 1024 ** 2)} MiB\n` +
        written.map((file) => `${file}\n`).join("")
    );
    return 0;
  } catch (error) {
    if (error instanceof MemoryStop) {
      io.err(`copies: the run stops: ${error.message}; nothing is written`);
      return 3;
    }
    io.err(`copies: ${(error as Error).stack ?? (error as Error).message}`);
    return 1;
  }
}

if (import.meta.main) {
  startPeakRssSampling();
  process.exit(
    await main(Bun.argv.slice(2), {
      gh: spawnGh,
      sleep: (ms) => Bun.sleep(ms),
      now: () => Date.now(),
      out: (text) => process.stdout.write(text),
      err: (text) => process.stderr.write(`${text}\n`),
      peakRssBytes: procPeakRssBytes,
    })
  );
}
