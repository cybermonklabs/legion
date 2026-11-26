import { heapSize } from "bun:jsc";
import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  existsSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { GithubReads } from "./build";
import {
  GH_CONCURRENCY,
  GhRetries,
  ghLimiter,
  Limiter,
  rateLimitWait,
  SECONDARY_WAIT_MS,
  transientReason,
} from "./common";
import { parseConfig } from "./config";
import {
  CopyIndex,
  FORMER_STAGES_COLUMNS,
  formerStagePrefixes,
  type Gh,
  type GhResult,
  type InstallerRow,
  installerPins,
  KEEP_RULES_VERSION,
  keepsEntry,
  listCopies,
  listedManifest,
  MANIFEST_KINDS,
  type Manifest,
  type ManifestKind,
  MEMORY_LIMIT_BYTES,
  main,
  manifestKind,
  mentions,
  npmPins,
  PREFIX_TABLE_COLUMNS,
  parseManifest,
  parseTsv,
  prefixMatches,
  type QueryRecord,
  READ_CALLS,
  READ_CALLS_PER_REPOSITORY,
  REPOSITORY_JOBS,
  type ReaderSkill,
  ReadSlots,
  releaseTag,
  repositoryFiles,
  repositoryKey,
  resolveInstallers,
  SKILL_COPIES_COLUMNS,
  type SkillCopy,
  searchTerms,
  skillFields,
  spawnGh,
  spawnGhWithin,
  type TreeEntry,
  tarballSkills,
  tsv,
  walkLevelKey,
  writeOutputs,
} from "./copies";
import { madeManifest } from "./testdata/fixtures";

// copies.ts on F6 (testdata/CAPTURE.md, "F6: GitHub's answers for the lister, and made-up
// manifests"): GitHub's own answers for the public cybermonklabs/legion, captured with read-only gh
// calls (CAPTURE.md gives each command), and the made-up files of testdata/F6-madeup/, each in the
// shape of a real file of its kind that no public repository of that owner holds (CAPTURE.md's
// table says which). gh is the stand-in `World.gh` below: no test reaches the network.
//
// The world the stand-in answers for, beside F6:
// - cybermonklabs/legion: F6-repo-list.json, F6-commits.json (its tree is F6-tree.json, cut to the
//   skills and the manifests) and the F6-contents-*.json blobs. Each SKILL.md and agent definition
//   F6 holds no content for gets a stand-in frontmatter (its directory's name, a made-up
//   description), but skills/asd-ste100/SKILL.md, whose read fails, to show a failed read named.
// - cybermonklabs/oh-my-pi (F6's fork): its tree read fails (HTTP 404), to show a repository named
//   with its error.
// - acme/skills, a made-up repository: the F6-madeup files, plus files made here, each following
//   a real file: `.claude-plugin/marketplace.json` (F6-contents-marketplace.json's shape, and F3's
//   made-up acme-market marketplace of CAPTURE.md), SKILL.md files in each frontmatter shape,
//   `packages/acme-envoy/package.json` (F6-contents-pi-envoy-package.json's `omp.skills`,
//   `dist/skills`, which the prepack script `scripts/pi-plugin-prepack.sh` fills with copies of
//   skills/<name>, `cp -R`, as cybermonklabs/legion's fills packages/pi-envoy's), a symbolic link
//   `packages/acme-envoy/skills/beta` (as packages/claude-envoy/skills/* link to skills/* in
//   cybermonklabs/legion, for the Claude Code plugin of the same npm package), a draft skill under
//   the excluded `pipeline/candidates/`, and a guide naming skills.
// - acme/old-skills, a made-up archived fork of acme/skills holding one skill, read in the files
//   its made-up compare with acme/skills lists (in the shape of GitHub's compare answer: a
//   `merge_base_commit`, and `files` each with `filename`, `status` and a renamed file's
//   `previous_filename`).
// - acme-gone, an owner whose listing fails.

const TESTDATA = path.join(import.meta.dir, "testdata");
const AT = "2026-11-20T00:00:00Z";
const CLOCK_START = 1_795_140_000_000;

const sha1 = (text: string) => createHash("sha1").update(text).digest("hex");
/** A git blob id: what GitHub's trees give for a file of this text. */
const blobSha = (text: string) => sha1(`blob ${Buffer.byteLength(text)}\0${text}`);
const fixture = (name: string) => readFileSync(path.join(TESTDATA, name), "utf8");
const fixtureJson = (name: string): unknown => JSON.parse(fixture(name));

/** A made-up file of a made-up repository. */
interface MadeFile {
  readonly path: string;
  readonly text: string;
  readonly mode?: string;
}

/** A tree of git entries for files: blobs, and a tree entry for each directory. */
function treeOf(repository: string, files: readonly MadeFile[]): TreeEntry[] {
  const dirs = new Set<string>();
  for (const file of files) {
    const parts = file.path.split("/");
    for (let depth = 1; depth < parts.length; depth++) dirs.add(parts.slice(0, depth).join("/"));
  }
  const entries: TreeEntry[] = [...dirs].map((dir) => ({
    path: dir,
    mode: "040000",
    type: "tree",
    sha: sha1(`tree ${repository} ${dir}`),
  }));
  for (const file of files) {
    entries.push({
      path: file.path,
      mode: file.mode ?? "100644",
      type: "blob",
      sha: blobSha(file.text),
      size: Buffer.byteLength(file.text),
    });
  }
  return entries.sort((a, b) => a.path.localeCompare(b.path));
}

/** Every file of testdata/F6-madeup, as one made-up repository's files. */
function madeUpFiles(): MadeFile[] {
  const root = path.join(TESTDATA, "F6-madeup");
  const walk = (dir: string): string[] =>
    readdirSync(dir).flatMap((name) => {
      const full = path.join(dir, name);
      return statSync(full).isDirectory() ? walk(full) : [full];
    });
  return walk(root).map((full) => ({
    path: path.relative(root, full),
    text: readFileSync(full, "utf8"),
  }));
}

const skill = (frontmatter: string, body = "# A made-up skill\n") =>
  `---\n${frontmatter}\n---\n\n${body}`;

/** A skills-sources.json, the Oh My Pi extension that reads it and the installer that links that
 *  extension, each in the shape of a real dotfiles repository's file (its skills-sources.json, its
 *  omp/extensions/dotfiles-skills.ts, and the ensure_link line of its installers/omp.sh). */
const SKILL_SOURCES_TEXT = `${JSON.stringify(
  {
    sources: [
      { root: "plugins/acme-tools" },
      { root: "plugins/plugin-gamma", omp: false },
      { root: "vendor/acme-pack" },
      { root: "../src/acme-elsewhere", ompSkillsDir: "~/src/acme-elsewhere/skills" },
    ],
  },
  null,
  2
)}\n`;
const EXTENSION_TEXT = [
  "// A made-up Oh My Pi extension of a fixture: feeds the skill pools to Oh My Pi.",
  'import * as fs from "node:fs";',
  'import * as path from "node:path";',
  'const MANIFEST = path.join(process.env.ACME_DIR ?? ".", "skills-sources.json");',
  "export default function (pi) {",
  '  pi.on("resources_discover", async () => ({',
  '    skillPaths: JSON.parse(fs.readFileSync(MANIFEST, "utf8")).sources.map((source) =>',
  '      source.ompSkillsDir ?? path.join(path.dirname(MANIFEST), source.root, "skills")',
  "    ),",
  "  }));",
  "}",
  "",
].join("\n");
const OMP_INSTALLER_TEXT = [
  "#!/bin/bash",
  "# A made-up installer of a fixture: links the skill-pool extension into Oh My Pi.",
  'REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"',
  'OMP_AGENT_DIR="$HOME/.omp/agent"',
  'mkdir -p "$OMP_AGENT_DIR/extensions"',
  'ln -sfn "$REPO_DIR/omp/extensions/skill-pools.ts" "$OMP_AGENT_DIR/extensions/skill-pools.ts"',
  "",
].join("\n");

/** acme/skills beside the F6-madeup files. */
const ACME_FILES: MadeFile[] = [
  { path: "skills-sources.json", text: SKILL_SOURCES_TEXT },
  { path: "omp/extensions/skill-pools.ts", text: EXTENSION_TEXT },
  { path: "installers/omp.sh", text: OMP_INSTALLER_TEXT },
  {
    path: "legion/acme-setup.sh",
    text: [
      "#!/bin/sh",
      "# A made-up role-start setup of a fixture: installs plugins of a made-up catalog.",
      "repo=acme/catalog",
      'omp plugin marketplace add "$repo"',
      'omp plugin install "$p@$catalog" --force',
      "",
    ].join("\n"),
  },
  {
    path: "legion/main.go",
    text: [
      "package main",
      "",
      "// The made-up catalog plugins every role installs.",
      'var everyRolePlugins = []string{"acme-tools"}',
      "",
    ].join("\n"),
  },
  {
    path: "legion/worker/Dockerfile",
    text: [
      "FROM scratch",
      "ARG ACME_RELEASE_URL=https://github.com/acme/skills/releases/download",
      "ARG ACME_VERSION=1.0.0",
      'RUN release="${ACME_RELEASE_URL}/v${ACME_VERSION}"; \\',
      '    install -D -m 0644 "$t/skills/alpha/SKILL.md" /opt/acme-alpha/skills/alpha/SKILL.md; \\',
      '    printf \'{"name": "acme-alpha", "omp": {}}\\n\' > /opt/acme-alpha/package.json',
      "",
    ].join("\n"),
  },
  {
    path: ".claude-plugin/marketplace.json",
    text: `${JSON.stringify(
      {
        name: "acme-market",
        owner: { name: "Acme Fixtures" },
        description: "A made-up marketplace of a fixture.",
        plugins: [
          { name: "acme-tools", source: "./plugins/acme-tools", description: "Made-up tools." },
          {
            name: "plugin-gamma",
            source: "./plugins/plugin-gamma",
            description: "A made-up plugin a project leaves out.",
          },
          {
            name: "acme-envoy",
            source: { source: "npm", package: "@acme/acme-envoy", version: ">=1.0.0" },
          },
          // In the shape of cybermonklabs/legion's claude-envoy: an npm package with no
          // omp.skills, whose plugin.json and skills/ links sit in its directory.
          { name: "acme-claude", source: { source: "npm", package: "@acme/acme-claude" } },
        ],
      },
      null,
      2
    )}\n`,
  },
  {
    path: "plugins/acme-tools/skills/alpha/SKILL.md",
    text: skill(
      "name: alpha\ndescription: Use when the alpha checklist of a made-up project is needed."
    ),
  },
  {
    path: "plugins/acme-tools/skills/tool-loop-alpha/SKILL.md",
    text: skill(
      'name: tool-loop-alpha\ndescription: "Use when the \\"loop\\" of alpha runs:\\nstep by step."'
    ),
  },
  {
    path: "plugins/plugin-gamma/skills/gamma-one/SKILL.md",
    text: skill("description: |\n  Line one of gamma.\n  Line two of gamma."),
  },
  {
    path: "skills/alpha/SKILL.md",
    text: skill("name: alpha\ndescription: >-\n  Use when the alpha\n  steps are needed."),
  },
  {
    path: "skills/beta/SKILL.md",
    text: skill("name: beta\ndescription: 'Use when beta''s greeting is needed.'"),
  },
  {
    path: "skills/legion-worker/SKILL.md",
    text: skill("name: legion-worker\ndescription: A made-up copy of a shared name."),
  },
  {
    path: "skills/dispatch/SKILL.md",
    text: skill("name: dispatch\ndescription: Use when: a made-up task needs a ticket"),
  },
  {
    path: "pipeline/candidates/draft-one/SKILL.md",
    text: skill("name: draft-one\ndescription: A draft no plugin ships."),
  },
  {
    path: "packages/acme-envoy/package.json",
    text: `${JSON.stringify({ name: "@acme/acme-envoy", version: "1.0.0", omp: { skills: ["dist/skills"] } }, null, 2)}\n`,
  },
  {
    path: "scripts/pi-plugin-prepack.sh",
    text: [
      "#!/usr/bin/env bash",
      "# A made-up prepack script: stages the Envoy package's skills, as copies, into dist/skills.",
      "plugin() {",
      '  case "$1" in',
      "  @acme/acme-envoy)",
      "    skills=( beta )",
      "    ;;",
      "  esac",
      "}",
      'cp -R "$root/skills/$skill" "$dest/$skill"',
      "stage_skills dist/skills",
      "",
    ].join("\n"),
  },
  { path: "packages/acme-envoy/skills/beta", text: "../../../skills/beta", mode: "120000" },
  {
    path: "packages/acme-claude/package.json",
    text: `${JSON.stringify({ name: "@acme/acme-claude", version: "1.0.0" }, null, 2)}\n`,
  },
  {
    path: "packages/acme-claude/.claude-plugin/plugin.json",
    text: `${JSON.stringify({ name: "acme-claude", description: "Made-up." }, null, 2)}\n`,
  },
  { path: "packages/acme-claude/skills/alpha", text: "../../../skills/alpha", mode: "120000" },
  {
    path: "docs/guide.md",
    text: "# Guide\n\nLoad skill://beta first. The tool-loop-alpha skill runs the loop.\n",
  },
];

const OLD_FILES: MadeFile[] = [
  {
    path: "skills/alpha/SKILL.md",
    text: skill("name: alpha\ndescription: >-\n  Use when the alpha\n  steps are needed."),
  },
  { path: "README.md", text: "# Old skills\n\nRead skill://beta before you start.\n" },
  { path: "notes/big.md", text: `${"x".repeat(200_001)}\n` },
  { path: "assets/data.json", text: '{"bytes": "\u0000\u0001"}\n' },
];

/** The made-up merge base of acme/old-skills and acme/skills. */
const OLD_BASE = sha1("merge base acme/old-skills");

/** The part of GitHub's compare answer that copies.ts reads. */
interface CompareAnswer {
  readonly merge_base_commit: { readonly sha: string };
  readonly files: readonly {
    readonly filename: string;
    readonly status: string;
    readonly previous_filename?: string;
  }[];
}

interface SearchReply {
  readonly status?: number;
  readonly code?: number;
  readonly stderr?: string;
  readonly headers?: Record<string, string>;
  readonly body?: unknown;
}

const EMPTY_SEARCH = { total_count: 0, incomplete_results: false, items: [] };

/** The stand-in gh and the world it answers for. */
class World {
  readonly calls: string[][] = [];
  readonly sleeps: number[] = [];
  clock = CLOCK_START;
  readonly listings = new Map<string, unknown>();
  readonly users = new Map<string, string>();
  readonly commits = new Map<string, unknown>();
  /** `<repository> <sha>` or `<repository> <sha> recursive`: a tree answer, or an error line. */
  readonly trees = new Map<string, unknown>();
  readonly blobs = new Map<string, string>();
  readonly failingBlobs = new Set<string>();
  /** `repos/<repository>`: GitHub's answer for it (a fork's `parent`), or an error line. */
  readonly repositoryInfo = new Map<string, unknown>();
  /** `<fork> <base>...<head>`: a compare answer (`merge_base_commit` and `files`), or an error
   *  line. */
  readonly compares = new Map<string, unknown>();
  /** `<repository> <path>`: the commits of the default branch that changed the file, newest
   *  first, each with its committer date, which `repos/<r>/commits?sha=…&path=…&since=…` answers
   *  from (those at or after `since`, as GitHub lists them); a file none names has none. */
  readonly pathCommits = new Map<string, { sha: string; date: string }[]>();
  /** `<repository> <sha>`: a commit's files (`filename`, `status`, a renamed file's
   *  `previous_filename`), which `repos/<r>/commits/<sha>` answers through copies.ts's jq, or an
   *  error line. */
  readonly commitFiles = new Map<string, CompareAnswer["files"] | string>();
  /** Each GraphQL blob batch: the repository its `owner` and `name` fields name, and its blobs. */
  readonly batches: { repository: string; blobs: number }[] = [];
  search: (q: string, page: number, call: number) => SearchReply = () => ({ body: EMPTY_SEARCH });
  /** When set, each owner's first search answer is held until this many owners have a search in
   *  flight: a run that searched the owners one after another would never release it. */
  holdFirstSearches = 0;
  /** When set, every GraphQL tree batch is refused, as GitHub refuses one past a node limit. */
  refuseTreeBatches = false;
  /** The GraphQL tree batches answered, each with its subtrees. */
  readonly treeBatches: number[] = [];
  /** When set, called as each GraphQL tree batch is answered, before its answer is made, with the
   *  ids of the subtrees it asks for. */
  onTreeBatch: ((shas: readonly string[]) => void) | undefined = undefined;
  /** When set, called with each call's arguments: an error line it gives is gh's answer to that
   *  call, as when GitHub refuses it for a rate limit. */
  refuse: ((args: readonly string[]) => string | undefined) | undefined = undefined;
  /** The seconds from now to each limit's reset that `gh api rate_limit` gives: it names no
   *  owner, so the pod's gh answers it from another installation than the one a call names, and
   *  the runner does not read it. */
  rateLimitResetIn = 45;
  /** The seconds from now to the reset a read of an installation's limit gives (`gh api -i`, as
   *  common.ts's `untilInstallationReset` reads it), for any owner. */
  installationResetIn = 300;
  /** When set, each tree read of a repository waits for what this gives it before it is answered,
   *  `key` the read's key in `trees`. */
  treeGate: ((repository: string, key: string) => Promise<void>) | undefined = undefined;
  /** When set, each search waits for it before it is answered. */
  searchGate: Promise<void> | undefined = undefined;
  /** The tree reads and searches answered, in their order: `tree <repository>`, `search <q>`. */
  readonly answered: string[] = [];
  /** When set, called with each line `answered` takes, as it takes it. */
  onAnswer: ((what: string) => void) | undefined = undefined;
  /** Each tree answer waits this many microtask turns (no timer), or its repository's in
   *  `treeTurnsOf`, so reads made at once overlap; the most tree reads in flight at once, in all
   *  and of one repository, and the most repositories with one in flight. */
  treeTurns = 0;
  readonly treeTurnsOf = new Map<string, number>();
  maxTreeReads = 0;
  maxTreeReadsOfOne = 0;
  maxTreeRepositories = 0;
  private readonly treeReadsOf = new Map<string, number>();
  /** Each search call: the owner its scope names, the clock when it started, searches in flight. */
  readonly searchLog: { owner: string; at: number; inFlight: number; ownerInFlight: number }[] = [];
  private readonly inFlight = new Map<string, number>();
  private readonly searched = new Set<string>();
  private readonly firstSearches = Promise.withResolvers<void>();
  private searchCalls = 0;
  /** `<repository>:<commit>:<path>` → the blob id of each file of a repository's whole tree, which
   *  a GraphQL `object(expression: "<commit>:<path>")` field answers by. */
  readonly filesAt = new Map<string, string>();
  /** `<repository> <path>`: the commits that changed the file, newest first, each with its date
   *  and its text there (`""` where the commit holds no such file), which a GraphQL
   *  `history(path:)` read answers from; a file none names has no history. */
  readonly histories = new Map<string, { oid: string; date: string; text: string }[]>();

  readonly now = () => this.clock;
  readonly sleep = async (ms: number) => {
    this.sleeps.push(ms);
    this.clock += ms;
  };

  /** A repository whose tree is `entries`, at a made-up commit. */
  addRepository(
    repository: string,
    entries: readonly TreeEntry[],
    rootSha = sha1(`root ${repository}`)
  ) {
    const commit = sha1(`commit ${repository}`);
    this.commits.set(repository, [{ sha: commit, commit: { tree: { sha: rootSha } } }]);
    this.trees.set(`${repository} ${rootSha} recursive`, {
      sha: rootSha,
      tree: entries,
      truncated: false,
    });
    this.addFilesAt(repository, commit, entries);
  }

  addFilesAt(repository: string, commit: string, entries: readonly TreeEntry[]) {
    for (const entry of entries) {
      if (entry.type === "blob")
        this.filesAt.set(`${repository}:${commit}:${entry.path}`, entry.sha);
    }
  }

  addFiles(files: readonly MadeFile[]) {
    for (const file of files) this.blobs.set(blobSha(file.text), file.text);
  }

  /** The tree reads in flight now. */
  treeReadsInFlight(): number {
    return [...this.treeReadsOf.values()].reduce((sum, count) => sum + count, 0);
  }

  readonly gh = async (args: readonly string[]): Promise<GhResult> => {
    this.calls.push([...args]);
    const ok = (value: unknown): GhResult => ({
      code: 0,
      stdout: JSON.stringify(value),
      stderr: "",
    });
    const fail = (line: string): GhResult => ({ code: 1, stdout: "", stderr: `${line}\n` });
    const refusal = this.refuse?.(args);
    if (refusal !== undefined) return fail(refusal);
    // A read of the reset of the installation that refused a call, answered as GitHub answers it
    // with `-i`: the reset in the X-RateLimit-Reset header and, for a GraphQL query, in its
    // `rateLimit`.
    if (args[0] === "api" && args[1] === "-i" && !args.includes("search/code")) {
      const reset = Math.floor(this.clock / 1000) + this.installationResetIn;
      const body =
        args[2] === "graphql"
          ? { data: { rateLimit: { resetAt: new Date(reset * 1000).toISOString() } } }
          : {};
      return {
        code: 0,
        stdout: `HTTP/2.0 200 OK\r\nX-Ratelimit-Reset: ${reset}\r\n\r\n${JSON.stringify(body)}`,
        stderr: "",
      };
    }
    const answer = (value: unknown) => (typeof value === "string" ? fail(value) : ok(value));
    if (args[0] === "repo" && args[1] === "list") {
      const listing = this.listings.get(args[2] ?? "");
      return listing === undefined
        ? fail(`GraphQL: Could not resolve to a User with the login of '${args[2]}'.`)
        : answer(listing);
    }
    if (args[0] !== "api") return fail(`stand-in gh: no answer for ${args.join(" ")}`);
    if (args[1] === "graphql") {
      // As the pod's gh routes it: a GraphQL call takes the token of the owner its `owner` field
      // names, and any other token cannot see a private repository. A blob batch as GitHub answers
      // it: a blob it cannot give is null, a binary one has no text.
      const field = (name: string) =>
        args.find((arg) => arg.startsWith(`${name}=`))?.slice(name.length + 1);
      const query = field("query") ?? "";
      const [owner, name] = [field("owner"), field("name")];
      if (
        owner === undefined ||
        name === undefined ||
        !query.includes("repository(owner: $owner, name: $name)")
      ) {
        return fail(`gh: Could not resolve to a Repository with the name '${query}'.`);
      }
      if (query.includes("history(path:")) {
        // A file's history as GitHub answers it: the commits since one time, newest first, and
        // the last one at or before another, each with the file there.
        const literal = (field: string) =>
          JSON.parse(new RegExp(`${field}: ("(?:[^"\\\\]|\\\\.)*")`).exec(query)?.[1] ?? '""');
        const commits = this.histories.get(`${owner}/${name} ${literal("path")}`) ?? [];
        const node = (commit: { oid: string; date: string; text: string }) => ({
          oid: commit.oid,
          committedDate: commit.date,
          file: commit.text === "" ? null : { object: { text: commit.text, isTruncated: false } },
        });
        const since = commits.filter((commit) => commit.date >= literal("since"));
        return ok({
          data: {
            repository: {
              object: {
                inWindow: {
                  pageInfo: { hasNextPage: since.length > 100 },
                  nodes: since.slice(0, 100).map(node),
                },
                before: {
                  nodes: commits
                    .filter((commit) => commit.date <= literal("until"))
                    .slice(0, 1)
                    .map(node),
                },
              },
            },
          },
        });
      }
      if (query.includes("... on Tree")) {
        // A tree batch: each field one subtree by its id, one level of entries, the mode a number.
        if (this.refuseTreeBatches)
          return fail("gh: Something went wrong while executing your query.");
        const trees = [...query.matchAll(/(t\d+): object\(oid: "([0-9a-f]{40})"\)/g)];
        this.treeBatches.push(trees.length);
        this.onTreeBatch?.(trees.map(([, , sha = ""]) => sha));
        const repository = Object.fromEntries(
          trees.map(([, alias = "", sha = ""]) => {
            const found = this.trees.get(`${owner}/${name} ${sha}`) as
              | { tree: TreeEntry[] }
              | undefined;
            if (found === undefined) return [alias, null];
            const entries = found.tree.map((entry) => ({
              name: entry.path,
              type: entry.type,
              mode: Number.parseInt(entry.mode, 8),
              oid: entry.sha,
              size: entry.size ?? 0,
            }));
            return [alias, { entries }];
          })
        );
        return ok({ data: { repository } });
      }
      // A field names a blob by its id (`oid`) or by `<commit>:<path>` (`expression`).
      const fields = [
        ...query.matchAll(/(b\d+): object\((oid|expression): ("(?:[^"\\]|\\.)*")\)/g),
      ].map(([, alias = "", how = "", literal = '""']) => {
        const value = JSON.parse(literal) as string;
        return {
          alias,
          sha: how === "oid" ? value : this.filesAt.get(`${owner}/${name}:${value}`),
        };
      });
      this.batches.push({ repository: `${owner}/${name}`, blobs: fields.length });
      const repository = Object.fromEntries(
        fields.map(({ alias, sha }) => {
          const text = sha === undefined ? undefined : this.blobs.get(sha);
          if (sha === undefined || text === undefined || this.failingBlobs.has(sha))
            return [alias, null];
          const binary = text.includes("\u0000");
          return [
            alias,
            { oid: sha, isBinary: binary, isTruncated: false, text: binary ? null : text },
          ];
        })
      );
      return ok({ data: { repository } });
    }
    if (args.includes("search/code")) {
      const field = (name: string) =>
        args.find((arg) => arg.startsWith(`${name}=`))?.slice(name.length + 1) ?? "";
      this.searchCalls++;
      const q = field("q");
      const owner = /(?:org|user|repo):([^/\s]+)/.exec(q)?.[1] ?? "";
      const ownerInFlight = (this.inFlight.get(owner) ?? 0) + 1;
      this.inFlight.set(owner, ownerInFlight);
      const inFlight = [...this.inFlight.values()].reduce((sum, count) => sum + count, 0);
      this.searchLog.push({ owner, at: this.clock, inFlight, ownerInFlight });
      const reply = this.search(q, Number(field("page")), this.searchCalls);
      if (this.holdFirstSearches > 0 && !this.searched.has(owner)) {
        this.searched.add(owner);
        if (this.searched.size >= this.holdFirstSearches) this.firstSearches.resolve();
        await this.firstSearches.promise;
      }
      if (this.searchGate !== undefined) await this.searchGate;
      this.noteAnswered(`search ${q}`);
      this.inFlight.set(owner, (this.inFlight.get(owner) ?? 1) - 1);
      const status = reply.status ?? 200;
      const headers = {
        "Content-Type": "application/json",
        "X-Ratelimit-Limit": "10",
        "X-Ratelimit-Remaining": "9",
        "X-Ratelimit-Reset": String(Math.floor(this.clock / 1000) + 60),
        "X-Ratelimit-Resource": "code_search",
        ...reply.headers,
      };
      const head = [
        `HTTP/2.0 ${status} ${status === 200 ? "OK" : "Forbidden"}`,
        ...Object.entries(headers).map(([k, v]) => `${k}: ${v}`),
      ];
      return {
        code: reply.code ?? (status < 300 ? 0 : 1),
        stdout: `${head.join("\r\n")}\r\n\r\n${JSON.stringify(reply.body ?? {})}`,
        stderr: reply.stderr ?? "",
      };
    }
    const endpoint = args[1] ?? "";
    if (endpoint === "rate_limit") {
      const reset = Math.floor(this.clock / 1000) + this.rateLimitResetIn;
      return ok({
        resources: {
          core: { limit: 15000, remaining: 0, reset },
          code_search: { limit: 10, remaining: 0, reset },
          graphql: { limit: 5000, remaining: 0, reset },
        },
      });
    }
    const user = /^users\/([^/]+)$/.exec(endpoint);
    if (user !== null) {
      const type = this.users.get(user[1] ?? "");
      return type === undefined ? fail("gh: Not Found (HTTP 404)") : ok({ login: user[1], type });
    }
    // A repository's commit at main (`commits`, by repository), or at another ref (by
    // `<repository>@<ref>`), as a pinned read asks for it.
    const commits =
      /^repos\/([^/]+\/[^/]+)\/commits\?sha=([^&]+)&until=2026-11-20T00%3A00%3A00Z&per_page=1$/.exec(
        endpoint
      );
    if (commits !== null) {
      const ref = decodeURIComponent(commits[2] ?? "");
      const found = this.commits.get(ref === "main" ? (commits[1] ?? "") : `${commits[1]}@${ref}`);
      return found === undefined
        ? fail(`gh: No commit found for SHA: ${ref} (HTTP 422)`)
        : answer(found);
    }
    const pathCommits =
      /^repos\/([^/]+\/[^/]+)\/commits\?sha=[0-9a-f]+&path=([^&]+)&since=([^&]+)&per_page=1$/.exec(
        endpoint
      );
    if (pathCommits !== null) {
      const since = decodeURIComponent(pathCommits[3] ?? "");
      const listed = (
        this.pathCommits.get(`${pathCommits[1]} ${decodeURIComponent(pathCommits[2] ?? "")}`) ?? []
      ).filter((commit) => commit.date >= since);
      return ok(
        listed.slice(0, 1).map((commit) => ({
          sha: commit.sha,
          commit: { committer: { date: commit.date } },
        }))
      );
    }
    const commitFiles = /^repos\/([^/]+\/[^/]+)\/commits\/([0-9a-f]{40})$/.exec(endpoint);
    if (commitFiles !== null) {
      const found = this.commitFiles.get(`${commitFiles[1]} ${commitFiles[2]}`);
      if (found === undefined) return fail("gh: Not Found (HTTP 404)");
      if (typeof found === "string") return fail(found);
      if (!args.includes("--jq")) return fail("stand-in gh: a commit read without --jq");
      // gh's jq of copies.ts's FILES_JQ: how many files, then one array per file.
      const lines = [
        String(found.length),
        ...found.map((file) =>
          JSON.stringify([file.filename, file.status, file.previous_filename ?? ""])
        ),
      ];
      return { code: 0, stdout: `${lines.join("\n")}\n`, stderr: "" };
    }
    const info = /^repos\/([^/]+\/[^/]+)$/.exec(endpoint);
    if (info !== null) {
      const found = this.repositoryInfo.get(info[1] ?? "");
      return found === undefined ? fail("gh: Not Found (HTTP 404)") : answer(found);
    }
    const compare = /^repos\/([^/]+\/[^/]+)\/compare\/([^?]+)\?per_page=1$/.exec(endpoint);
    if (compare !== null) {
      const found = this.compares.get(`${compare[1]} ${decodeURIComponent(compare[2] ?? "")}`);
      if (found === undefined) return fail("gh: Not Found (HTTP 404)");
      if (typeof found === "string") return fail(found);
      if (!args.includes("--jq")) return fail("stand-in gh: a compare read without --jq");
      // gh's jq of copies.ts's COMPARE_JQ: the merge base, how many files, one array per file.
      const { merge_base_commit, files } = found as CompareAnswer;
      const lines = [
        merge_base_commit.sha,
        String(files.length),
        ...files.map((file) =>
          JSON.stringify([file.filename, file.status, file.previous_filename ?? ""])
        ),
      ];
      return { code: 0, stdout: `${lines.join("\n")}\n`, stderr: "" };
    }
    const tree = /^repos\/([^/]+\/[^/]+)\/git\/trees\/([0-9a-f]+)(\?recursive=1)?$/.exec(endpoint);
    if (tree !== null) {
      const repository = tree[1] ?? "";
      this.treeReadsOf.set(repository, (this.treeReadsOf.get(repository) ?? 0) + 1);
      this.maxTreeReads = Math.max(this.maxTreeReads, this.treeReadsInFlight());
      const reads = [...this.treeReadsOf.values()];
      this.maxTreeReadsOfOne = Math.max(this.maxTreeReadsOfOne, ...reads);
      this.maxTreeRepositories = Math.max(
        this.maxTreeRepositories,
        reads.filter((count) => count > 0).length
      );
      const turns = this.treeTurnsOf.get(repository) ?? this.treeTurns;
      for (let turn = 0; turn < turns; turn++) await Promise.resolve();
      const key = `${tree[1]} ${tree[2]}${tree[3] ? " recursive" : ""}`;
      if (this.treeGate !== undefined) await this.treeGate(repository, key);
      this.noteAnswered(`tree ${repository}`);
      this.treeReadsOf.set(repository, (this.treeReadsOf.get(repository) ?? 1) - 1);
      const found = this.trees.get(key);
      if (found === undefined) return fail("gh: Not Found (HTTP 404)");
      if (typeof found === "string") return fail(found);
      if (!args.includes("--jq")) return fail("stand-in gh: a tree read without --jq");
      // gh's jq of copies.ts's TREE_JQ: the truncated flag, then one JSON array per entry.
      const { tree: entries, truncated } = found as { tree: TreeEntry[]; truncated?: boolean };
      const lines = [
        String(truncated === true),
        ...entries.map((entry) =>
          JSON.stringify([entry.path, entry.mode, entry.type, entry.sha, entry.size ?? -1])
        ),
      ];
      return { code: 0, stdout: `${lines.join("\n")}\n`, stderr: "" };
    }
    const blob = /^repos\/([^/]+\/[^/]+)\/git\/blobs\/([0-9a-f]+)$/.exec(endpoint);
    if (blob !== null) {
      const sha = blob[2] ?? "";
      const text = this.blobs.get(sha);
      if (text === undefined || this.failingBlobs.has(sha)) return fail("gh: Not Found (HTTP 404)");
      return ok({
        sha,
        size: Buffer.byteLength(text),
        content: Buffer.from(text).toString("base64"),
        encoding: "base64",
      });
    }
    return fail(`stand-in gh: no answer for ${args.join(" ")}`);
  };

  private noteAnswered(what: string) {
    this.answered.push(what);
    this.onAnswer?.(what);
  }
}

/** F6's tree of cybermonklabs/legion. */
function legionTree(): { sha: string; entries: TreeEntry[] } {
  const tree = fixtureJson("F6-tree.json") as { sha: string; tree: TreeEntry[] };
  return { sha: tree.sha, entries: tree.tree };
}

/** F6's commit's tree id: the stand-in answers F6-tree.json for it. */
const LEGION_ROOT =
  (fixtureJson("F6-commits.json") as { commit: { tree: { sha: string } } }[])[0]?.commit.tree.sha ??
  "";

/** The world of F6 and the made-up repositories. */
function world(): World {
  const w = new World();
  w.listings.set("cybermonklabs", fixtureJson("F6-repo-list.json"));
  w.listings.set("acme", [
    {
      nameWithOwner: "acme/skills",
      isFork: false,
      isArchived: false,
      defaultBranchRef: { name: "main" },
      parent: null,
    },
    {
      nameWithOwner: "acme/old-skills",
      isFork: true,
      isArchived: true,
      defaultBranchRef: { name: "main" },
      parent: { id: "R_madeup", name: "skills", owner: { id: "U_madeup", login: "acme" } },
    },
  ]);
  w.users.set("cybermonklabs", "Organization");
  w.users.set("acme", "User");

  w.commits.set("cybermonklabs/legion", fixtureJson("F6-commits.json"));
  const legion = legionTree();
  w.trees.set(`cybermonklabs/legion ${LEGION_ROOT} recursive`, fixtureJson("F6-tree.json"));
  w.addFilesAt("cybermonklabs/legion", "63b3a613feb15cb191f87f4239467cdd37b62c82", legion.entries);
  for (const name of readdirSync(TESTDATA).filter((file) => file.startsWith("F6-contents-"))) {
    const contents = fixtureJson(name) as { sha: string; content: string };
    w.blobs.set(
      contents.sha,
      Buffer.from(contents.content.replace(/\n/g, ""), "base64").toString("utf8")
    );
  }
  for (const entry of legion.entries) {
    if (entry.type !== "blob" || w.blobs.has(entry.sha)) continue;
    const dir = path.posix.dirname(entry.path);
    if (entry.path.endsWith("/SKILL.md")) {
      w.blobs.set(
        entry.sha,
        skill(
          `name: ${path.posix.basename(dir)}\ndescription: A stand-in description of ${path.posix.basename(dir)}.`
        )
      );
    } else if (dir === "packages/pi-legion/agents") {
      w.blobs.set(
        entry.sha,
        skill(
          `name: ${path.posix.basename(entry.path, ".md")}\ndescription: A stand-in agent.`,
          "# Agent\n"
        )
      );
    }
  }
  const failing = legion.entries.find((entry) => entry.path === "skills/asd-ste100/SKILL.md");
  if (failing !== undefined) w.failingBlobs.add(failing.sha);

  w.commits.set("cybermonklabs/oh-my-pi", [
    { sha: sha1("commit oh-my-pi"), commit: { tree: { sha: sha1("root oh-my-pi") } } },
  ]);

  const acme = [...madeUpFiles(), ...ACME_FILES];
  w.addFiles(acme);
  w.addRepository("acme/skills", treeOf("acme/skills", acme));
  w.addFiles(OLD_FILES);
  w.addRepository("acme/old-skills", treeOf("acme/old-skills", OLD_FILES));
  // The fork changed every file but its skill against acme/skills.
  w.repositoryInfo.set("acme/old-skills", {
    full_name: "acme/old-skills",
    fork: true,
    parent: { full_name: "acme/skills", default_branch: "main" },
  });
  w.compares.set(`acme/old-skills acme:main...${sha1("commit acme/old-skills")}`, {
    merge_base_commit: { sha: OLD_BASE },
    files: OLD_FILES.filter((file) => !file.path.endsWith("/SKILL.md")).map((file) => ({
      filename: file.path,
      status: "modified",
    })),
  });
  w.search = acmeSearch;
  return w;
}

const CONFIG = {
  owners: ["cybermonklabs", "acme", "acme-gone"],
  stores: [{ name: "store_a", context: "devbox" }],
  psqlCommand: "/usr/bin/psql",
  gatewayBaseUrl: "http://127.0.0.1:4000",
  servedModels: ["anthropic/claude-alpha", "openai/gpt-alpha"],
  excludePaths: [{ repo: "acme/skills", prefix: "pipeline/candidates/" }],
  targets: ["cybermonklabs/legion", "acme/skills"],
  cuts: [{ id: "skills-1", repo: "acme/skills", pr: 1 }],
  windowStart: "2026-10-23T00:00:00Z",
  windowEnd: AT,
};

const scratch: string[] = [];
afterAll(() => {
  for (const dir of scratch) rmSync(dir, { recursive: true, force: true });
});

interface Run {
  readonly code: number;
  readonly errors: string[];
  readonly out: string;
  /** What the run wrote to standard output: its summary line and the files written. */
  readonly stdout: string;
  text(name: string): string;
  table(name: string): Record<string, string>[];
  queries(): Record<string, unknown>[];
}

/** The peak resident memory the stand-in reports unless a test gives another. */
const PEAK_BYTES = 100 * 1024 ** 2;

async function run(
  w: World,
  args: readonly string[] = [],
  config: Record<string, unknown> = {},
  peakRssBytes: () => number = () => PEAK_BYTES
): Promise<Run> {
  const dir = mkdtempSync(path.join(tmpdir(), "copies-test-"));
  scratch.push(dir);
  const file = path.join(dir, "skill-cuts.json");
  writeFileSync(file, JSON.stringify({ ...CONFIG, ...config }));
  const out = path.join(dir, "out");
  const errors: string[] = [];
  const outs: string[] = [];
  const code = await main(["--config", file, "--at", AT, "--out", out, ...args], {
    gh: w.gh,
    sleep: w.sleep,
    now: w.now,
    out: (text) => outs.push(text),
    err: (line) => errors.push(line),
    peakRssBytes,
  });
  const text = (name: string) => readFileSync(path.join(out, name), "utf8");
  return {
    code,
    errors,
    stdout: outs.join(""),
    out,
    text,
    table: (name) => parseTsv(text(name)),
    queries: () =>
      text("queries.jsonl")
        .split("\n")
        .filter((line) => line !== "")
        .map((line) => JSON.parse(line) as Record<string, unknown>),
  };
}

/** Every output file of a run, by name. */
function outputs(r: Run): Record<string, string> {
  return Object.fromEntries(readdirSync(r.out).map((file) => [file, r.text(file)]));
}

/** The file the cache under `cacheDir` holds for `repository`. */
function cachedEntry(
  cacheDir: string,
  repository: string
):
  | {
      treeEntries: number;
      tree: TreeEntry[];
      blobs: Record<string, string>;
      forkRead?: string;
      forkFilesApart?: boolean;
    }
  | undefined {
  const dir = path.join(cacheDir, "repository");
  for (const file of readdirSync(dir)) {
    if (!file.endsWith(".json")) continue;
    const entry = JSON.parse(readFileSync(path.join(dir, file), "utf8")) as {
      key: string;
      value: { treeEntries: number; tree: TreeEntry[]; blobs: Record<string, string> };
    };
    if (entry.key.startsWith(`${repository}\0`)) return entry.value;
  }
  return undefined;
}

/** The installer rows of a manifest, as `<action> <unit> <skill> <copy repository> <copy path>`. */
function installerRows(r: Run, repository: string, manifestPath: string): string[] {
  return r
    .table("installers.tsv")
    .filter((row) => row.manifest_repository === repository && row.manifest_path === manifestPath)
    .map((row) =>
      [row.action, row.unit, row.skill, row.copy_repository, row.copy_path].join(" ").trim()
    );
}

/** One example of each manifest kind in the world, what it says, and a row it gives. A kind added
 *  to MANIFEST_KINDS without an example here fails to typecheck and fails the test below. */
const KIND_EXAMPLES: Record<
  ManifestKind,
  { repository: string; path: string; says: string; row: string }
> = {
  "claude-marketplace": {
    repository: "acme/skills",
    path: ".claude-plugin/marketplace.json",
    says: "marketplace acme-market: acme-tools (plugins/acme-tools), plugin-gamma (plugins/plugin-gamma), acme-envoy (npm @acme/acme-envoy), acme-claude (npm @acme/acme-claude)",
    row: "ships plugin:acme-tools@acme-market tool-loop-alpha acme/skills plugins/acme-tools/skills/tool-loop-alpha",
  },
  "claude-plugin": {
    repository: "acme/skills",
    path: "plugins/acme-tools/.claude-plugin/plugin.json",
    says: "plugin acme-tools 0.3.0: skills ./skills/tool-loop-alpha, ./skills/alpha",
    row: "",
  },
  "omp-plugins": {
    repository: "cybermonklabs/legion",
    path: "packages/daemon/docker/omp-plugins/package.json",
    says: "installs @bopstack/pi-codegraph@0.1.1",
    row: "installs package:@bopstack/pi-codegraph",
  },
  "omp-package": {
    repository: "cybermonklabs/legion",
    path: "packages/pi-legion/package.json",
    says: "package @acme/pi-legion: omp.skills dist/skills",
    row: "ships package:@acme/pi-legion legion-worker cybermonklabs/legion skills/legion-worker",
  },
  "claude-settings": {
    repository: "acme/skills",
    path: ".claude/settings.json",
    says: "plugins acme-tools@acme-market on, beta-pack@acme-market off; marketplaces acme-market",
    row: "enables plugin:acme-tools@acme-market alpha acme/skills plugins/acme-tools/skills/alpha",
  },
  "omp-config": {
    repository: "acme/skills",
    path: "omp/config.yml",
    says: "skills.ignoredSkills beta, gamma-*",
    row: "ignores omp-settings gamma-one acme/skills plugins/plugin-gamma/skills/gamma-one",
  },
  "agent-definition": {
    repository: "cybermonklabs/legion",
    path: "packages/pi-legion/agents/thermonuclear-deep-review.md",
    says: "agent thermonuclear-deep-review: autoloadSkills thermonuclear-deep-review",
    row: "loads agent:thermonuclear-deep-review thermonuclear-deep-review cybermonklabs/legion skills/thermonuclear-deep-review",
  },
  "legion-project": {
    repository: "acme/skills",
    path: "legion/projects/10-ACME.yaml",
    says: "project ACME: verificationSkillsLeaveOut plugin-gamma, plugin-delta",
    row: "leaves out project:ACME gamma-one acme/skills plugins/plugin-gamma/skills/gamma-one",
  },
  "legion-role-setup": {
    repository: "acme/skills",
    path: "legion/acme-setup.sh",
    says: "installs plugins of the catalog acme/catalog at each role start",
    row: "",
  },
  "legion-program": {
    repository: "acme/skills",
    path: "legion/main.go",
    says: "every role installs acme-tools of the catalog",
    row: "",
  },
  "worker-image": {
    repository: "acme/skills",
    path: "legion/worker/Dockerfile",
    says: "installs alpha of acme/skills v1.0.0 as the Oh My Pi plugin /opt/acme-alpha",
    row: "installs image:acme-alpha alpha acme/skills skills/alpha",
  },
  "prepack-script": {
    repository: "cybermonklabs/legion",
    path: "scripts/pi-plugin-prepack.sh",
    says: "stages skills/<name> into dist/skills: @acme/pi-envoy: agent-secrets approval-review asd-ste100 dispatch",
    row: "stages package:@acme/pi-legion legion-worker cybermonklabs/legion skills/legion-worker",
  },
  "build-script": {
    repository: "cybermonklabs/legion",
    path: "packages/claude-envoy/scripts/build.ts",
    says: "generates the plugin legion from the skills prepack stages for @acme/pi-legion",
    row: "ships plugin:legion legion-worker cybermonklabs/legion skills/legion-worker",
  },
  "installer-script": {
    repository: "acme/skills",
    path: "installers/skills.sh",
    says: "links each skill of skills/ into the claude-code user skills; links each skill of skills/ into the omp user skills; links omp/config.yml as the Oh My Pi config.yml",
    row: "links user-skills:omp beta acme/skills skills/beta",
  },
  "python-loader": {
    repository: "acme/skills",
    path: "tree/init.py",
    says: "loads roles.proposer: alpha, beta; roles.prover: alpha, tool-loop-alpha; roles.critic: alpha",
    row: "loads loader:tree/init.py#roles.prover tool-loop-alpha acme/skills plugins/acme-tools/skills/tool-loop-alpha",
  },
  "skill-link": {
    repository: "acme/skills",
    path: "packages/acme-envoy/skills/beta",
    says: "links ../../../skills/beta (skills/beta)",
    row: "links link:packages/acme-envoy/skills/beta beta acme/skills skills/beta",
  },
  "omp-skill-sources": {
    repository: "acme/skills",
    path: "skills-sources.json",
    says: "skill pools: plugins/acme-tools, plugins/plugin-gamma (omp: false), vendor/acme-pack, ../src/acme-elsewhere (skills at ~/src/acme-elsewhere/skills)",
    row: "installs pool:plugins/acme-tools tool-loop-alpha acme/skills plugins/acme-tools/skills/tool-loop-alpha",
  },
  "omp-extension": {
    repository: "acme/skills",
    path: "omp/extensions/skill-pools.ts",
    says: "an Oh My Pi extension that reads skills-sources.json",
    row: "",
  },
};

describe("skill pools an Oh My Pi extension reads from skills-sources.json", () => {
  const repository = "acme/dotfiles";
  const copyAt = (dir: string): SkillCopy => ({
    repository,
    path: dir,
    file: `${dir}/SKILL.md`,
    blob: sha1(dir),
    fork: false,
    archived: false,
    parent: "",
    name: path.posix.basename(dir),
    nameFrom: "frontmatter",
    description: "Use when a made-up pool's skill is needed.",
    descriptionBytes: 42,
    fault: "",
  });
  const copies = [
    copyAt("plugins/acme-tools/skills/alpha"),
    copyAt("plugins/plugin-gamma/skills/gamma-one"),
  ];
  const sources = madeManifest(
    repository,
    "skills-sources.json",
    "omp-skill-sources",
    SKILL_SOURCES_TEXT
  );
  const extension = madeManifest(
    repository,
    "omp/extensions/skill-pools.ts",
    "omp-extension",
    EXTENSION_TEXT
  );
  const installer = madeManifest(
    repository,
    "installers/omp.sh",
    "installer-script",
    OMP_INSTALLER_TEXT
  );
  const resolve = (manifests: Manifest[]) => resolveInstallers(manifests, new CopyIndex(copies));
  const poolRows = (manifests: Manifest[]) =>
    resolve(manifests)
      .rows.filter((row) => row.unit.startsWith("pool:"))
      .map((row) => `${row.action} ${row.unit} ${row.host} ${row.skill} ${row.copyPath}`);
  const notes = (manifests: Manifest[]) => resolve(manifests).notes.map((note) => note.note);

  test("with the extension that reads it and the installer that links it, each pool in the tree installs its skills into Oh My Pi", () => {
    expect(poolRows([sources, extension, installer])).toEqual([
      "installs pool:plugins/acme-tools omp alpha plugins/acme-tools/skills/alpha",
    ]);
    expect(resolve([sources, extension, installer]).prefixes.map((row) => row.prefix)).toEqual([
      "**/plugins/acme-tools/skills/alpha/",
    ]);
  });

  test("a pool with omp: false installs nothing and is no note", () => {
    expect(
      poolRows([sources, extension, installer]).some((row) => row.includes("plugin-gamma"))
    ).toBe(false);
    expect(
      notes([sources, extension, installer]).some((note) => note.includes("plugin-gamma"))
    ).toBe(false);
  });

  test("a pool outside the repository's tree is a note on the manifest, never an installer row", () => {
    expect(notes([sources, extension, installer])).toEqual([
      "pool vendor/acme-pack: its skills (vendor/acme-pack/skills) are not in the repository's tree, so this list cannot hold them",
      "pool ../src/acme-elsewhere: its skills (~/src/acme-elsewhere/skills) are not in the repository's tree, so this list cannot hold them",
    ]);
  });

  test("a skills-sources.json no extension reads, or whose extension no installer links, installs nothing", () => {
    const unread = madeManifest(
      repository,
      "omp/extensions/other.ts",
      "omp-extension",
      "export default () => {};\n"
    );
    expect(poolRows([sources, unread, installer])).toEqual([]);
    expect(notes([sources, unread, installer])).toEqual([
      "no Oh My Pi extension of the repository reads it, so it installs nothing",
    ]);
    expect(poolRows([sources, extension])).toEqual([]);
    expect(notes([sources, extension])).toEqual([
      "no installer of the repository links omp/extensions/skill-pools.ts into Oh My Pi, so it installs nothing",
    ]);
  });
});

// An Oh My Pi plugin package's skills, as Oh My Pi 18.18.6 scans them (copies.ts's parsePackage):
// made-up packages of the made-up repository acme/pkgs, each package.json in the shape of a real
// plugin package's (F6-contents-pi-envoy-package.json's `omp`, and a `pi` field of that shape).
describe("the skills of an Oh My Pi plugin package, as Oh My Pi scans them", () => {
  const repository = "acme/pkgs";
  const packageAt = (dir: string, manifest: Record<string, unknown>): Manifest =>
    madeManifest(repository, `${dir}/package.json`, "omp-package", JSON.stringify(manifest));
  const copyAt = (dir: string): SkillCopy => ({
    repository,
    path: dir,
    file: `${dir}/SKILL.md`,
    blob: sha1(dir),
    fork: false,
    archived: false,
    parent: "",
    name: path.posix.basename(dir),
    nameFrom: "frontmatter",
    description: "Use when a made-up package's skill is needed.",
    descriptionBytes: 45,
    fault: "",
  });
  /** Each `ships` row of `manifest`'s package, as `<unit> <skill> <copy path>`. */
  const ships = (manifest: Manifest, dirs: readonly string[]) =>
    resolveInstallers([manifest], new CopyIndex(dirs.map(copyAt)))
      .rows.filter((row) => row.action === "ships")
      .map((row) => `${row.unit} ${row.skill} ${row.copyPath ?? "-"}`);

  test("a package whose pi manifest declares skills ships them, under node_modules/<package>/", () => {
    const manifest = packageAt("packages/pi-only", {
      name: "@acme/pi-only",
      pi: { extensions: ["./extensions/pi-only.ts"], skills: ["./skills"] },
    });
    expect(manifest.says).toBe("package @acme/pi-only: pi.skills skills");
    expect(manifest.ofKind).toBe(true);
    expect(ships(manifest, ["packages/pi-only/skills/pi-one"])).toEqual([
      "package:@acme/pi-only pi-one packages/pi-only/skills/pi-one",
    ]);
    const index = new CopyIndex([copyAt("packages/pi-only/skills/pi-one")]);
    expect(resolveInstallers([manifest], index).prefixes.map((row) => row.prefix)).toEqual([
      "**/node_modules/@acme/pi-only/skills/pi-one/",
    ]);
  });

  test("omp wins over pi: its skills when it declares them, else skills/, never pi's", () => {
    const both = packageAt("packages/both", {
      name: "@acme/both",
      omp: { skills: ["omp-skills"] },
      pi: { skills: ["pi-skills"] },
    });
    expect(
      ships(both, ["packages/both/omp-skills/o-one", "packages/both/pi-skills/p-one"])
    ).toEqual(["package:@acme/both o-one packages/both/omp-skills/o-one"]);
    // An omp field that declares no skills still wins over pi's: Oh My Pi falls back to skills/.
    const ompFirst = packageAt("packages/omp-first", {
      name: "@acme/omp-first",
      omp: { extensions: ["./extensions/omp-first.ts"] },
      pi: { skills: ["pi-skills"] },
    });
    expect(ompFirst.says).toBe("package @acme/omp-first: omp declares no skills, so skills/");
    expect(
      ships(ompFirst, ["packages/omp-first/skills/f-one", "packages/omp-first/pi-skills/p-two"])
    ).toEqual(["package:@acme/omp-first f-one packages/omp-first/skills/f-one"]);
  });

  test("a package whose manifest declares no skills falls back to the skills of skills/, and one with neither omp nor pi is no plugin", () => {
    const fallback = packageAt("packages/fallback", {
      name: "@acme/fallback",
      pi: { extensions: ["./extensions/fallback.ts"] },
    });
    // skills/ is a directory of skills only: a SKILL.md in skills/ itself is no skill of it.
    expect(
      ships(fallback, ["packages/fallback/skills", "packages/fallback/skills/fallback-one"])
    ).toEqual(["package:@acme/fallback fallback-one packages/fallback/skills/fallback-one"]);
    const plain = packageAt("packages/plain", { name: "@acme/plain" });
    expect(plain.ofKind).toBe(false);
    expect(plain.says).toBe("package @acme/plain: no omp or pi manifest, so no Oh My Pi plugin");
    expect(ships(plain, ["packages/plain/skills/plain-one"])).toEqual([]);
  });

  test("a skills entry that leaves the package is skipped, as Oh My Pi skips it, and a skills field that is no list declares none", () => {
    const out = packageAt("packages/out", {
      name: "@acme/out",
      omp: { skills: ["skills", "../shared-skills", "/srv/skills"] },
    });
    expect(out.says).toBe(
      "package @acme/out: omp.skills skills; ../shared-skills, /srv/skills outside the package, which Oh My Pi skips"
    );
    expect(ships(out, ["packages/out/skills/in-one", "packages/shared-skills/out-one"])).toEqual([
      "package:@acme/out in-one packages/out/skills/in-one",
    ]);
    // Only outside entries: the unit ships nothing, and says why.
    const away = packageAt("packages/away", { name: "@acme/away", omp: { skills: ["../x"] } });
    expect(
      resolveInstallers([away], new CopyIndex([copyAt("packages/x/x-one")])).rows.map(
        (row) => `${row.unit} ${row.copyPath ?? "-"} ${row.note}`
      )
    ).toEqual([
      "package:@acme/away - omp.skills ../x leaves the package, so Oh My Pi skips it, and no prepack script stages it",
    ]);
    const single = packageAt("packages/single", {
      name: "@acme/single",
      omp: { skills: "skills" },
    });
    expect(ships(single, ["packages/single/skills/s-one"])).toEqual([]);
  });

  /** The `ships` rows that reach a copy of the skills `manifests` ship into Oh My Pi, over the
   *  copies at `dirs`, those of `untold` with no description. */
  const shipped = (manifests: Manifest[], dirs: readonly string[], untold: string[] = []) =>
    resolveInstallers(
      manifests,
      new CopyIndex(
        dirs.map((dir) => ({
          ...copyAt(dir),
          description: untold.includes(dir) ? "" : copyAt(dir).description,
        }))
      )
    ).rows.filter((row) => row.action === "ships" && row.copyPath !== undefined);

  test("a declared directory is scanned as Oh My Pi scans it: no skill through a link out of the package, none without a description, and a namespace's skills one level deeper", () => {
    // A link out of the package: Oh My Pi lists it from the skills/ fallback, never from a
    // declared directory (helpers.ts lines 579-594).
    const link = (dir: string): Manifest =>
      madeManifest(repository, `${dir}/skills/far`, "skill-link", "../../shared/far");
    const declared = packageAt("packages/linked", {
      name: "@acme/linked",
      omp: { skills: ["skills"] },
    });
    expect(
      shipped(
        [declared, link("packages/linked")],
        ["packages/linked/skills/near", "packages/shared/far"]
      ).map((row) => row.skill)
    ).toEqual(["near"]);
    const fallback = packageAt("packages/open", { name: "@acme/open", omp: { extensions: [] } });
    expect(
      shipped(
        [fallback, link("packages/open")],
        ["packages/open/skills/near", "packages/shared/far"]
      ).map((row) => row.skill)
    ).toEqual(["far", "near"]);
    // No description: left out, and the unit says so.
    const quiet = packageAt("packages/quiet", { name: "@acme/quiet", pi: { skills: ["skills"] } });
    const rows = shipped(
      [quiet],
      ["packages/quiet/skills/told", "packages/quiet/skills/untold"],
      ["packages/quiet/skills/untold"]
    );
    expect(rows.map((row) => row.skill)).toEqual(["told"]);
    expect(rows[0]?.note).toBe("Oh My Pi lists no skill of it without a description: untold");
    // A namespace (an entry holding no SKILL.md) is read one level deeper, and no further.
    const nest = packageAt("packages/nest", { name: "@acme/nest", pi: { skills: ["skills"] } });
    const index = new CopyIndex(
      ["packages/nest/skills/group/inner", "packages/nest/skills/group/x/too-deep"].map(copyAt)
    );
    expect(
      resolveInstallers([nest], index).prefixes.map((row) => `${row.prefix} ${row.name}`)
    ).toEqual(["**/node_modules/@acme/nest/skills/group/inner/ inner"]);
  });

  test("a pool's skills less those an installer's `find … -exec rm` removes from its clone", () => {
    const sources = madeManifest(
      repository,
      "skills-sources.json",
      "omp-skill-sources",
      JSON.stringify({ sources: [{ root: "vendor/pack" }] })
    );
    const installer = madeManifest(
      repository,
      "installers/omp.sh",
      "installer-script",
      [
        OMP_INSTALLER_TEXT.trimEnd(),
        "ensure_vendor https://github.com/acme/pack.git pack",
        "# The pack ships many dd-* skills; only dd-keep is wanted.",
        `find "\${DOTFILES_DIR}/vendor/pack/skills" -mindepth 1 -maxdepth 1 -type d -name 'dd-*' ! -name 'dd-keep' -exec rm -rf {} +`,
        "",
      ].join("\n")
    );
    expect(installer.says).toContain(
      "removes each entry of vendor/pack/skills named dd-* but dd-keep"
    );
    const extension = madeManifest(
      repository,
      "omp/extensions/skill-pools.ts",
      "omp-extension",
      EXTENSION_TEXT
    );
    const copies = ["skills/dd-drop", "skills/dd-keep", "skills/other"].map((dir) => ({
      ...copyAt(dir),
      repository: "acme/pack",
    }));
    const rows = resolveInstallers([sources, extension, installer], new CopyIndex(copies)).rows;
    expect(
      rows.map((row) => `${row.action} ${row.unit} ${row.skill} ${row.copyRepository}`)
    ).toEqual([
      "installs pool:vendor/pack dd-keep acme/pack",
      "installs pool:vendor/pack other acme/pack",
    ]);
    expect(rows[0]?.note).toContain("installers/omp.sh removes dd-drop from it");
  });

  test("a clone an installer makes as one entry of a pool's skills directory (`ensure_vendor <url> <pool>/skills/<name>`) gives the cloned repository's root skill under that entry", () => {
    const pin = sha1("one-skill pin");
    const manifests = [
      madeManifest(
        repository,
        "skills-sources.json",
        "omp-skill-sources",
        JSON.stringify({ sources: [{ root: "vendor/one" }] })
      ),
      madeManifest(repository, "omp/extensions/skill-pools.ts", "omp-extension", EXTENSION_TEXT),
      madeManifest(
        repository,
        "installers/omp.sh",
        "installer-script",
        [
          OMP_INSTALLER_TEXT.trimEnd(),
          "# The made-up repository is the skill itself, so it is cloned one level down.",
          `ensure_vendor https://github.com/acme/one-skill.git one/skills/one ${pin}`,
          "",
        ].join("\n")
      ),
    ];
    // The cloned repository's one SKILL.md is at its root.
    const root: SkillCopy = {
      repository: "acme/one-skill",
      path: "",
      file: "SKILL.md",
      blob: sha1("one-skill root"),
      fork: false,
      archived: false,
      parent: "",
      name: "one",
      nameFrom: "frontmatter",
      description: "Use when the made-up one skill is due.",
      descriptionBytes: 38,
      fault: "",
    };
    const resolved = resolveInstallers(manifests, new CopyIndex([root]));
    expect(
      resolved.rows.map((row) => `${row.action} ${row.unit} ${row.skill} ${row.copyRepository}`)
    ).toEqual(["installs pool:vendor/one one acme/one-skill"]);
    expect(resolved.rows[0]?.note).toContain(
      `installers/omp.sh clones acme/one-skill into vendor/one/skills/one at ${pin}`
    );
    expect(
      resolved.prefixes.map((row) => [row.prefix, row.repository, row.path, row.name])
    ).toEqual([["**/vendor/one/skills/one/", "acme/one-skill", "", "one"]]);
    expect(installerPins(manifests).map((each) => `${each.repository} ${each.ref}`)).toEqual([
      `acme/one-skill ${pin}`,
    ]);
  });

  /** A made-up copy of `repository` at `dir`, with a description. */
  const madeCopy = (repository: string, dir: string): SkillCopy => ({
    repository,
    path: dir,
    file: `${dir}/SKILL.md`,
    blob: sha1(`${repository} ${dir}`),
    fork: false,
    archived: false,
    parent: "",
    name: path.posix.basename(dir),
    nameFrom: "frontmatter",
    description: "Use when a made-up skill is needed.",
    descriptionBytes: 35,
    fault: "",
  });

  // Catches: a package whose skills all lack a description dropped as no unit, its note that Oh
  // My Pi lists none of them lost; and a declared directory of such skills said to hold no
  // SKILL.md.
  test("a package whose skills all lack a description is a unit with no skill, whose note says so, and a declared directory of them is not said to hold no SKILL.md", () => {
    const quiet = (dir: string): SkillCopy => ({
      ...madeCopy("acme/quiet", dir),
      description: "",
      descriptionBytes: 0,
      fault: "no description",
    });
    for (const manifest of [{ pi: {} }, { omp: { skills: ["skills"] } }]) {
      const pkg = madeManifest(
        "acme/quiet",
        "package.json",
        "omp-package",
        JSON.stringify({ name: "@acme/quiet", ...manifest })
      );
      const rows = resolveInstallers([pkg], new CopyIndex([quiet("skills/quiet-one")])).rows;
      expect(rows.map((row) => `${row.action} ${row.unit} ${row.skill}: ${row.note}`)).toEqual([
        "ships package:@acme/quiet : Oh My Pi lists no skill of it without a description: quiet-one",
      ]);
    }
  });

  // Catches: skills.ignoredSkills matched here by a glob that knows `*` alone, where Oh My Pi and
  // build.ts match each pattern as a Bun.Glob, so a pattern with `?` or `{…}` ignored nothing.
  test("skills.ignoredSkills patterns match skill names as Oh My Pi matches them, as a Bun.Glob: ? and {…} too", () => {
    const config = madeManifest(
      "acme/home",
      "omp/config.yml",
      "omp-config",
      'skills:\n  ignoredSkills:\n    - "{alpha,beta}"\n    - gamma-?ne\n'
    );
    const resolved = resolveInstallers(
      [config],
      new CopyIndex(
        ["alpha", "beta", "gamma-one", "delta"].map((name) =>
          madeCopy("acme/skills", `skills/${name}`)
        )
      )
    );
    expect(resolved.rows.map((row) => `${row.action} ${row.skill} ${row.note}`)).toEqual([
      "ignores alpha pattern {alpha,beta}",
      "ignores beta pattern {alpha,beta}",
      "ignores gamma-one pattern gamma-?ne",
    ]);
  });

  test("an installer that installs a plugin from a marketplace into Oh My Pi pins the marketplace, and the marketplace pins the plugin's source at its sha; a marketplace whose name holds the word claude names no host", () => {
    const pin = sha1("chat plugin pin");
    const installer = madeManifest(
      "acme/pkgs",
      "installers/omp.sh",
      "installer-script",
      [
        OMP_INSTALLER_TEXT.trimEnd(),
        "install_marketplace_plugin acme/claude-market chat@claude-market",
        "",
      ].join("\n")
    );
    expect(installerPins([installer]).map((each) => `${each.repository} ${each.ref}`)).toEqual([
      "acme/claude-market ",
    ]);
    const marketplace = madeManifest(
      "acme/claude-market",
      ".claude-plugin/marketplace.json",
      "claude-marketplace",
      JSON.stringify({
        name: "claude-market",
        plugins: [
          {
            name: "chat",
            source: { source: "url", url: "https://github.com/acme/chat-plugin.git", sha: pin },
          },
          {
            name: "other",
            source: { source: "url", url: "https://github.com/acme/other-plugin.git", sha: pin },
          },
        ],
      })
    );
    // Only the plugin an installer installs is pinned, at the sha its marketplace names.
    expect(
      installerPins([installer, marketplace]).map((each) => `${each.repository} ${each.ref}`)
    ).toEqual(["acme/claude-market ", `acme/chat-plugin ${pin}`]);
    const rows = resolveInstallers(
      [installer, marketplace],
      new CopyIndex([madeCopy("acme/chat-plugin", "skills/chat-one")])
    ).rows.filter((row) => row.manifestPath === "installers/omp.sh");
    expect(rows.map((row) => `${row.action} ${row.unit} ${row.host} ${row.skill}`)).toEqual([
      "installs plugin:chat@claude-market omp chat-one",
    ]);
  });

  test("`gh skill install <repo> <skill> --pin <ref>` installs that repository's skills/<skill> at the ref into ~/.agents/skills, which Oh My Pi lists", () => {
    const pin = sha1("tool skill pin");
    const installer = madeManifest(
      "acme/pkgs",
      "installers/omp.sh",
      "installer-script",
      [
        OMP_INSTALLER_TEXT.trimEnd(),
        `TOOL_PIN="${pin}"`,
        'gh skill install acme/cli tool --agent universal --scope user --pin "$TOOL_PIN" --force || true',
        "",
      ].join("\n")
    );
    expect(installerPins([installer]).map((each) => `${each.repository} ${each.ref}`)).toEqual([
      `acme/cli ${pin}`,
    ]);
    const resolved = resolveInstallers(
      [installer],
      new CopyIndex([madeCopy("acme/cli", "skills/tool"), madeCopy("acme/cli", ".github/skills/x")])
    );
    expect(
      resolved.rows.map(
        (row) => `${row.action} ${row.unit} ${row.host} ${row.skill} ${row.copyPath}`
      )
    ).toEqual(["installs gh-skill:tool omp tool skills/tool"]);
    expect(resolved.prefixes.map((row) => `${row.prefix} ${row.repository} ${row.path}`)).toEqual([
      "**/.agents/skills/tool/ acme/cli skills/tool",
    ]);
  });

  // Catches: a variable's value unquoted twice (shellVariables unquotes it, then the gh skill
  // install reader again), so `REPO="'acme/cli'"`, whose value the shell passes as `'acme/cli'`
  // with its quotes, was read as acme/cli.
  test("a gh skill install whose repository variable's value holds quotes pins nothing: the value is unquoted once, as the shell reads it", () => {
    const installer = (assignment: string) =>
      madeManifest(
        "acme/pkgs",
        "installers/omp.sh",
        "installer-script",
        [
          OMP_INSTALLER_TEXT.trimEnd(),
          assignment,
          'gh skill install "$TOOL_REPO" tool --agent universal --scope user --force || true',
          "",
        ].join("\n")
      );
    const pinned = (assignment: string) =>
      installerPins([installer(assignment)]).map((each) => each.repository);
    expect(pinned('TOOL_REPO="acme/cli"')).toEqual(["acme/cli"]);
    expect(pinned(`TOOL_REPO="'acme/cli'"`)).toEqual([]);
  });
});

// A repository and a fork of it, each with its own prepack script that stages the same package
// name and its own build script, as cybermonklabs/legion and theorem-labs/legion each stage
// @sjawhar/pi-envoy and @sjawhar/pi-legion. Only the parent's package.json is the package's unit
// (the first of its name read), as in R4's list of the three owners.
describe("the prepack and build scripts of a fork that stage its parent's package name", () => {
  const PARENT = "acme/legion";
  const FORK = "acme-fork/legion";
  const copyOf = (repository: string, name: string): SkillCopy => ({
    repository,
    path: `skills/${name}`,
    file: `skills/${name}/SKILL.md`,
    blob: sha1(`${repository} ${name}`),
    fork: repository === FORK,
    archived: false,
    parent: repository === FORK ? PARENT : "",
    name,
    nameFrom: "frontmatter",
    description: "Use when a made-up skill is needed.",
    descriptionBytes: 35,
    fault: "",
  });
  const index = () =>
    new CopyIndex([
      ...["agent-secrets", "dispatch", "envoy"].map((name) => copyOf(PARENT, name)),
      ...["dispatch", "envoy", "brainstorm", "retired"].map((name) => copyOf(FORK, name)),
    ]);
  /** A prepack script in the shape of scripts/pi-plugin-prepack.sh: one arm per package, its
   *  skills=( … ) copied from skills/ into dist/skills. */
  const prepackText = (skills: readonly string[]) =>
    [
      "#!/usr/bin/env bash",
      "plugin() {",
      '  case "$1" in',
      "  @acme/pi-envoy)",
      `    skills=(${skills.join(" ")})`,
      "    ;;",
      "  esac",
      "}",
      "stage_skills dist/skills",
      "",
    ].join("\n");
  const prepack = (repository: string, skills: readonly string[]) =>
    madeManifest(repository, "scripts/pi-plugin-prepack.sh", "prepack-script", prepackText(skills));
  const pkg = (repository: string) =>
    madeManifest(
      repository,
      "packages/pi-envoy/package.json",
      "omp-package",
      JSON.stringify({ name: "@acme/pi-envoy", omp: { skills: ["dist/skills"] } })
    );
  const build = (repository: string) =>
    madeManifest(
      repository,
      "packages/claude-envoy/scripts/build.ts",
      "build-script",
      [
        'const args = ["--stage-skills", "@acme/pi-envoy"];',
        'writeFileSync(join(out, "plugin.json"), JSON.stringify({ name: "legion" }));',
        "",
      ].join("\n")
    );
  const manifests = () => [
    pkg(PARENT),
    prepack(PARENT, ["agent-secrets", "dispatch", "envoy"]),
    build(PARENT),
    pkg(FORK),
    prepack(FORK, ["dispatch", "envoy", "brainstorm"]),
    build(FORK),
  ];
  /** The installer rows of `unit` whose manifest is of `repository`, with `action`, as
   *  `<skill> <copy repository> <copy path>`. */
  const rowsOf = (
    rows: readonly InstallerRow[],
    unit: string,
    repository: string,
    action: string
  ) =>
    rows
      .filter(
        (row) => row.unit === unit && row.manifestRepository === repository && row.action === action
      )
      .map((row) => `${row.skill} ${row.copyRepository} ${row.copyPath ?? "-"}`);

  // Catches: R4's list of the three owners, where cybermonklabs/legion's @sjawhar/pi-envoy
  // shipped theorem-labs/legion's dispatch, envoy and dispatch-first (the fork's stages taken
  // with the parent's, and keyed without their repository, so the fork's copy overwrote the
  // parent's), and the image rows /opt/legion/pi-envoy/dist/skills/<name>/ named the fork, a
  // repository outside the targets, so the table held no row for them: 49,375 of the 60,233
  // unattributed read sessions of R1 on that list.
  test("the package ships its own repository's staged copies, at node_modules and the image, and none of the fork's", () => {
    const resolved = resolveInstallers(manifests(), index());
    const own = [
      "agent-secrets acme/legion skills/agent-secrets",
      "dispatch acme/legion skills/dispatch",
      "envoy acme/legion skills/envoy",
    ];
    expect(rowsOf(resolved.rows, "package:@acme/pi-envoy", PARENT, "ships")).toEqual(own);
    expect(rowsOf(resolved.rows, "package:@acme/pi-envoy", PARENT, "stages")).toEqual(own);
    expect(
      resolved.prefixes
        .filter((row) => row.via.startsWith("package:"))
        .map((row) => `${row.prefix} ${row.repository} ${row.path}`)
        .sort()
    ).toEqual(
      ["agent-secrets", "dispatch", "envoy"]
        .flatMap((name) => [
          `**/node_modules/@acme/pi-envoy/dist/skills/${name}/ acme/legion skills/${name}`,
          `/opt/legion/pi-envoy/dist/skills/${name}/ acme/legion skills/${name}`,
        ])
        .sort()
    );
    // The fork's own stages stay its installer rows, each saying the package read is not its own.
    expect(
      resolved.rows
        .filter((row) => row.manifestRepository === FORK && row.action === "stages")
        .map((row) => `${row.skill} ${row.copyRepository}: ${row.note}`)
    ).toEqual(
      ["dispatch", "envoy", "brainstorm"].map(
        (name) =>
          `${name} acme-fork/legion: the package.json of @acme/pi-envoy read is acme/legion's, and a prepack script stages into its own repository's package alone`
      )
    );
  });

  test("each build script's plugin ships its own repository's staged copies alone", () => {
    const resolved = resolveInstallers(manifests(), index());
    expect(rowsOf(resolved.rows, "plugin:legion", PARENT, "ships")).toEqual([
      "agent-secrets acme/legion skills/agent-secrets",
      "dispatch acme/legion skills/dispatch",
      "envoy acme/legion skills/envoy",
    ]);
    expect(rowsOf(resolved.rows, "plugin:legion", FORK, "ships")).toEqual([
      "dispatch acme-fork/legion skills/dispatch",
      "envoy acme-fork/legion skills/envoy",
      "brainstorm acme-fork/legion skills/brainstorm",
    ]);
    expect(
      resolved.prefixes
        .filter((row) => row.name === "dispatch" && row.via.startsWith("plugin:legion"))
        .map((row) => `${row.prefix} ${row.repository} ${row.via}`)
        .sort()
    ).toEqual([
      "/opt/legion/claude-envoy/legion/skills/dispatch/ acme-fork/legion plugin:legion (acme-fork/legion:packages/claude-envoy/scripts/build.ts)",
      "/opt/legion/claude-envoy/legion/skills/dispatch/ acme/legion plugin:legion (acme/legion:packages/claude-envoy/scripts/build.ts)",
    ]);
  });

  test("a path the fork's prepack script staged at and stages no longer gets no row of its parent's package", async () => {
    const copies = index();
    const resolved = resolveInstallers(manifests(), copies);
    const read = ["dispatch", "envoy", "brainstorm"];
    const older = sha1("an older fork script");
    const current = sha1("the fork script read");
    const asked: string[] = [];
    const former = await formerStagePrefixes(
      [
        {
          manifest: prepack(FORK, read),
          versions: [
            {
              commit: older,
              date: "2026-10-30T00:00:00Z",
              text: prepackText([...read, "retired"]),
            },
            { commit: current, date: "2026-11-10T00:00:00Z", text: prepackText(read) },
          ],
          sourceOf: async (from) => {
            asked.push(from);
            return { kind: "unchanged", since: "2026-10-23T00:00:00Z" };
          },
        },
      ],
      resolved,
      copies
    );
    expect(former.prefixes).toEqual([]);
    const why =
      "no prefix row maps that path: @acme/pi-envoy is no package unit of acme-fork/legion";
    expect(former.notes.map((note) => note.note)).toEqual([
      `staged retired for @acme/pi-envoy at dist/skills/retired/ from ${older} (2026-10-30T00:00:00Z) until ${current} (2026-11-10T00:00:00Z), and stages it there no longer; ${why}`,
    ]);
    // Its one former-stages.tsv row has no prefix (the package is no unit of the fork) and no
    // copy, and git is not asked about a path no row could map.
    expect(former.stages).toEqual([
      {
        prefix: "",
        scriptRepository: FORK,
        scriptPath: "scripts/pi-plugin-prepack.sh",
        package: "@acme/pi-envoy",
        skill: "retired",
        until: current,
        renamedTo: "",
        repository: "",
        path: "",
        note: why,
      },
    ]);
    expect(asked).toEqual([]);
  });
});

// Catches: a blob read with its leading byte order mark kept (a GraphQL batch's text as given, a
// REST blob through `Buffer.toString`), where Oh My Pi drops the mark as it reads a file: a
// package.json that starts with one was no JSON, so its package shipped nothing. A SKILL.md that
// starts with one is read by its frontmatter (copies.ts's frontmatter parse dropped the mark
// itself before; now the read does). The second run refuses every GraphQL blob batch, so each
// blob is read alone over REST.
test("a package.json and a SKILL.md that start with a byte order mark are read as Oh My Pi reads them, in a GraphQL batch and alone over REST: the package ships the skill, under its frontmatter name", async () => {
  for (const rest of [false, true]) {
    const w = world();
    w.listings.set("acme", [
      ...(w.listings.get("acme") as unknown[]),
      {
        nameWithOwner: "acme/marked",
        isFork: false,
        isArchived: false,
        defaultBranchRef: { name: "main" },
        parent: null,
      },
    ]);
    const files: MadeFile[] = [
      {
        path: "skills/marked-dir/SKILL.md",
        text: "\uFEFF---\nname: marked\ndescription: Use when a marked step is due.\n---\n",
      },
      { path: "package.json", text: `\uFEFF${JSON.stringify({ name: "@acme/marked", pi: {} })}\n` },
    ];
    w.addFiles(files);
    w.addRepository("acme/marked", treeOf("acme/marked", files));
    const answer = w.gh;
    if (rest) {
      Object.assign(w, {
        gh: async (args: readonly string[]) =>
          args.some((arg) => arg.includes("on Blob {"))
            ? { code: 1, stdout: "", stderr: "gh: Something went wrong (HTTP 500)\n" }
            : answer(args),
      });
    }
    const r = await run(w, ["--only", "acme/marked", "--max-search-calls", "0"], {
      targets: [...CONFIG.targets, "acme/marked"],
    });
    expect(r.code).toBe(0);
    const copy = r.table("skill-copies.tsv").find((row) => row.repository === "acme/marked");
    expect({ rest, name: copy?.name, bytes: copy?.description_bytes }).toEqual({
      rest,
      name: "marked",
      bytes: String(Buffer.byteLength("Use when a marked step is due.")),
    });
    expect({ rest, rows: installerRows(r, "acme/marked", "package.json") }).toEqual({
      rest,
      rows: ["ships package:@acme/marked marked acme/marked skills/marked-dir"],
    });
  }
}, 60_000);

/** A made-up repository `name` as an owner's listing gives it: on main, no fork, not archived. */
const madeListing = (name: string) => ({
  nameWithOwner: name,
  isFork: false,
  isArchived: false,
  defaultBranchRef: { name: "main" },
  parent: null,
});
/** The `repos/<repo>` answer of a made-up repository no listing gives: on main, no fork, not
 *  archived. */
const madeInfo = (repo: string) => ({
  full_name: repo,
  default_branch: "main",
  fork: false,
  archived: false,
  parent: null,
});

// An omp-plugins `github:` dependency and an `ensure_vendor` clone, each read at the ref it names:
// acme/dots, a made-up repository in the shape of a dotfiles repository (its
// omp/plugins/package.json, skills-sources.json, omp/extensions/dotfiles-skills.ts and
// installers/omp.sh and installers/lib.sh, each made up here); other/powers, a made-up repository
// of an owner the configuration does not name, at the tag its dependency names; and acme/pack,
// listed at main, cloned at a pinned commit.
describe("repositories an installer pins", () => {
  const PACK_PIN = sha1("pinned acme/pack");
  const DOTS_FILES: MadeFile[] = [
    {
      path: "omp/plugins/package.json",
      text: `${JSON.stringify(
        {
          name: "omp-plugins",
          private: true,
          dependencies: {
            "@acme/registry-one": "1.0.0",
            "acme-powers": "github:other/powers#v1.2.0",
          },
        },
        null,
        2
      )}\n`,
    },
    {
      path: "skills-sources.json",
      text: `${JSON.stringify({ sources: [{ root: "vendor/acme-pack" }] }, null, 2)}\n`,
    },
    { path: "omp/extensions/skill-pools.ts", text: EXTENSION_TEXT },
    {
      path: "installers/omp.sh",
      text: [
        OMP_INSTALLER_TEXT.trimEnd(),
        "# The made-up pool is cloned at a pinned commit.",
        `ensure_vendor https://github.com/acme/pack.git acme-pack ${PACK_PIN}`,
        "",
      ].join("\n"),
    },
    {
      path: "installers/lib.sh",
      text: [
        "#!/bin/bash",
        "# A made-up installer library: the clone helper, whose own body names no clone.",
        "ensure_vendor() {",
        '    local url="$1" name="$2" ref="${3:-}"',
        '    git clone --depth 1 "$url" "vendor/${name}"',
        "}",
        "",
      ].join("\n"),
    },
  ];
  const POWERS_FILES: MadeFile[] = [
    {
      path: "package.json",
      text: `${JSON.stringify(
        { name: "powers-of-acme", version: "1.2.0", pi: { skills: ["./skills"] } },
        null,
        2
      )}\n`,
    },
    {
      path: "skills/power-one/SKILL.md",
      text: skill("name: power-one\ndescription: Use when a made-up power is needed."),
    },
  ];
  const PACK_MAIN: MadeFile[] = [
    {
      path: "skills/pack-later/SKILL.md",
      text: skill("name: pack-later\ndescription: A made-up skill added after the pin."),
    },
  ];
  const PACK_PINNED: MadeFile[] = [
    {
      path: "skills/pack-one/SKILL.md",
      text: skill("name: pack-one\ndescription: Use when the made-up pack's first step is due."),
    },
  ];
  /** `repository` at `ref` holding `files`, its commit and root tree made up from both. */
  const addAt = (w: World, repository: string, ref: string, files: readonly MadeFile[]) => {
    const commit = sha1(`commit ${repository} ${ref}`);
    const root = sha1(`root ${repository} ${ref}`);
    const entries = treeOf(repository, files);
    w.addFiles(files);
    w.commits.set(`${repository}@${ref}`, [{ sha: commit, commit: { tree: { sha: root } } }]);
    w.trees.set(`${repository} ${root} recursive`, { sha: root, tree: entries, truncated: false });
    w.addFilesAt(repository, commit, entries);
    return commit;
  };
  const pinnedWorld = () => {
    const w = world();
    w.listings.set("acme", [
      ...(w.listings.get("acme") as unknown[]),
      madeListing("acme/dots"),
      madeListing("acme/pack"),
    ]);
    w.addFiles(DOTS_FILES);
    w.addRepository("acme/dots", treeOf("acme/dots", DOTS_FILES));
    w.addFiles(PACK_MAIN);
    w.addRepository("acme/pack", treeOf("acme/pack", PACK_MAIN));
    w.repositoryInfo.set("other/powers", madeInfo("other/powers"));
    return w;
  };
  const only = ["--only", "acme/dots", "--only", "acme/pack", "--max-search-calls", "0"];
  const targets = { targets: [...CONFIG.targets, "acme/dots", "acme/pack", "other/powers"] };

  test("an omp-plugins github: dependency is a unit: its repository at the ref it names, outside the owners too, its package read by Oh My Pi's rule and installed under the dependency's name", async () => {
    const w = pinnedWorld();
    const powers = addAt(w, "other/powers", "v1.2.0", POWERS_FILES);
    addAt(w, "acme/pack", PACK_PIN, PACK_PINNED);
    const r = await run(w, only, targets);
    expect(r.code).toBe(0);
    const read = r.table("repositories.tsv").find((row) => row.repository === "other/powers");
    expect(read).toMatchObject({ branch: "v1.2.0", commit: powers, error: "" });
    expect(installerRows(r, "other/powers", "package.json")).toEqual([
      "ships package:acme-powers power-one other/powers skills/power-one",
    ]);
    expect(installerRows(r, "acme/dots", "omp/plugins/package.json")).toContain(
      "installs package:acme-powers power-one other/powers skills/power-one"
    );
    expect(r.text("prefix-table.tsv")).toContain(
      "**/node_modules/acme-powers/skills/power-one/\tother/powers\tskills/power-one\tpower-one\tpackage:acme-powers (other/powers:package.json)"
    );
    expect(r.text("skill-copies.md")).toContain("- other/powers at ");
  }, 60_000);

  test("an ensure_vendor clone's pool is a unit: the cloned repository at the ref the installer pins, in place of its listed read", async () => {
    const w = pinnedWorld();
    addAt(w, "other/powers", "v1.2.0", POWERS_FILES);
    const pack = addAt(w, "acme/pack", PACK_PIN, PACK_PINNED);
    const r = await run(w, only, targets);
    expect(r.code).toBe(0);
    const read = r.table("repositories.tsv").filter((row) => row.repository === "acme/pack");
    expect(read).toEqual([expect.objectContaining({ branch: PACK_PIN, commit: pack, error: "" })]);
    // Its copies are those of the pinned commit, not of main.
    expect(
      r
        .table("skill-copies.tsv")
        .filter((row) => row.repository === "acme/pack")
        .map((row) => row.path)
    ).toEqual(["skills/pack-one"]);
    expect(installerRows(r, "acme/dots", "skills-sources.json")).toEqual([
      "installs pool:vendor/acme-pack pack-one acme/pack skills/pack-one",
    ]);
    expect(r.text("prefix-table.tsv")).toContain(
      "**/vendor/acme-pack/skills/pack-one/\tacme/pack\tskills/pack-one\tpack-one\tpool:vendor/acme-pack (acme/dots:skills-sources.json)"
    );
    const manifests = r.table("manifests.tsv");
    expect(manifests.find((row) => row.path === "installers/omp.sh")?.says).toContain(
      `clones acme/pack into vendor/acme-pack at ${PACK_PIN}`
    );
    // The helper's own body names no clone, so the library is no installer of its kind.
    expect(manifests.some((row) => row.path === "installers/lib.sh")).toBe(false);
  }, 60_000);

  // Catches: a read at a pin landing as it ended: two listed repositories whose reads at their
  // pins failed were named in failures in the order the reads ended, and the first pin of a
  // repository that two pinned repositories pin was the one whose read ended first. Here the
  // first pin of each pair ends last: its repository answers its tree reads 200 turns later.
  test("the reads at pins land in the pins' order, whichever ends first: two listed repositories' failed reads, and the ref of a repository two pinned repositories clone", async () => {
    const w = world();
    w.listings.set("acme", [
      ...(w.listings.get("acme") as unknown[]),
      ...["acme/dots", "acme/pack", "acme/pack2"].map(madeListing),
    ]);
    const [PACK2_PIN, A_PIN, B_PIN, C_BY_A, C_BY_B] = [
      "acme/pack2",
      "a",
      "b",
      "c by a",
      "c by b",
    ].map((name) => sha1(`pinned ${name}`));
    /** An installer script that clones each of `clones` (repository, name, ref) into vendor/. */
    const installer = (clones: readonly (readonly [string, string, string])[]): MadeFile[] => [
      {
        path: "installers/omp.sh",
        text: [
          OMP_INSTALLER_TEXT.trimEnd(),
          ...clones.map(
            ([repo, name, ref]) => `ensure_vendor https://github.com/${repo}.git ${name} ${ref}`
          ),
          "",
        ].join("\n"),
      },
    ];
    const dots = installer([
      ["acme/pack", "acme-pack", PACK_PIN],
      ["acme/pack2", "acme-pack2", PACK2_PIN],
      ["other/a", "a", A_PIN],
      ["other/b", "b", B_PIN],
    ]);
    w.addFiles(dots);
    w.addRepository("acme/dots", treeOf("acme/dots", dots));
    w.addFiles(PACK_MAIN);
    w.addRepository("acme/pack", treeOf("acme/pack", PACK_MAIN));
    w.addFiles(PACK_PINNED);
    w.addRepository("acme/pack2", treeOf("acme/pack2", PACK_PINNED));
    // Each pack's commit at its pin has no tree: its read there fails.
    for (const [repo, pin] of [
      ["acme/pack", PACK_PIN],
      ["acme/pack2", PACK2_PIN],
    ] as const) {
      const root = sha1(`root ${repo} ${pin}`);
      w.commits.set(`${repo}@${pin}`, [
        { sha: sha1(`commit ${repo}`), commit: { tree: { sha: root } } },
      ]);
    }
    // other/a and other/b each clone other/c, at a ref of its own.
    for (const [repo, pin, cRef] of [
      ["other/a", A_PIN, C_BY_A],
      ["other/b", B_PIN, C_BY_B],
    ] as const) {
      w.repositoryInfo.set(repo, madeInfo(repo));
      addAt(w, repo, pin, installer([["other/c", "c", cRef]]));
    }
    w.repositoryInfo.set("other/c", madeInfo("other/c"));
    for (const ref of [C_BY_A, C_BY_B]) addAt(w, "other/c", ref, POWERS_FILES);
    w.treeTurnsOf.set("acme/pack", 200);
    w.treeTurnsOf.set("other/a", 200);
    const r = await run(
      w,
      [
        "--only",
        "acme/dots",
        "--only",
        "acme/pack",
        "--only",
        "acme/pack2",
        "--max-search-calls",
        "0",
      ],
      { targets: [...CONFIG.targets, "acme/dots", "acme/pack", "acme/pack2", "other/c"] }
    );
    expect(r.code).toBe(0);
    // Each failed read's line up to gh's error, which follows `pins: `.
    const failures = r
      .text("skill-copies.md")
      .split("\n")
      .filter((line) => line.includes(", which acme/dots:") || line.includes("pins other/c"))
      .map((line) => line.replace(/ pins: .*$/, " pins: …"));
    expect(failures).toEqual([
      `- acme/pack at ${PACK_PIN}, which acme/dots:installers/omp.sh pins: …`,
      `- acme/pack2 at ${PACK2_PIN}, which acme/dots:installers/omp.sh pins: …`,
      `- other/b:installers/omp.sh pins other/c at ${C_BY_B}; this list reads it at ${C_BY_A}, as other/a:installers/omp.sh pins it`,
    ]);
    expect(r.table("repositories.tsv").find((row) => row.repository === "other/c")).toMatchObject({
      branch: C_BY_A,
    });
  }, 60_000);
});

// A pool in a clone made by hand, which no installer records, that the configuration's `clones`
// states: acme/dots, in the shape of a dotfiles repository whose skills-sources.json lists the
// pool .private/dots (a clone of acme/private at .private holds it, as such a file's comment
// says), and acme/private, the made-up repository cloned there, whose dots/skills holds the
// pool's skill.
describe("a pool clone made by hand that the configuration's clones states", () => {
  const DOTS_FILES: MadeFile[] = [
    {
      path: "skills-sources.json",
      text: `${JSON.stringify({ sources: [{ root: ".private/dots" }] }, null, 2)}\n`,
    },
    { path: "omp/extensions/skill-pools.ts", text: EXTENSION_TEXT },
    { path: "installers/omp.sh", text: OMP_INSTALLER_TEXT },
  ];
  const PRIVATE_FILES: MadeFile[] = [
    {
      path: "dots/skills/private-one/SKILL.md",
      text: skill("name: private-one\ndescription: Use when the made-up private step is due."),
    },
  ];
  const clonedWorld = () => {
    const w = world();
    w.listings.set("acme", [
      ...(w.listings.get("acme") as unknown[]),
      madeListing("acme/dots"),
      madeListing("acme/private"),
    ]);
    w.addFiles(DOTS_FILES);
    w.addRepository("acme/dots", treeOf("acme/dots", DOTS_FILES));
    w.addFiles(PRIVATE_FILES);
    w.addRepository("acme/private", treeOf("acme/private", PRIVATE_FILES));
    w.repositoryInfo.set("acme/private", madeInfo("acme/private"));
    return w;
  };
  const clone = { repo: "acme/dots", dir: ".private", clone: "acme/private" };
  const targets = [...CONFIG.targets, "acme/dots", "acme/private"];
  const only = ["--only", "acme/dots", "--max-search-calls", "0"];

  // Catches: a pool in a clone no installer makes left as a note, so the profile and the prefix
  // table miss its skills.
  test("its pool is a unit of the cloned repository's skills at its default branch, read as a pin of no ref where --only leaves it out, each row naming the key", async () => {
    const r = await run(clonedWorld(), only, { targets, clones: [clone] });
    expect(r.code).toBe(0);
    expect(r.table("repositories.tsv").find((row) => row.repository === "acme/private")).toEqual(
      expect.objectContaining({ branch: "main", commit: sha1("commit acme/private"), error: "" })
    );
    expect(installerRows(r, "acme/dots", "skills-sources.json")).toEqual([
      "installs pool:.private/dots private-one acme/private dots/skills/private-one",
    ]);
    expect(r.table("installers.tsv").find((row) => row.unit === "pool:.private/dots")?.note).toBe(
      "read by omp/extensions/skill-pools.ts; clones[0] of the configuration says .private is a clone of acme/private made by hand, which no installer records (this list reads its default branch)"
    );
    expect(r.text("prefix-table.tsv")).toContain(
      "**/.private/dots/skills/private-one/\tacme/private\tdots/skills/private-one\tprivate-one\tpool:.private/dots (acme/dots:skills-sources.json)"
    );
  }, 60_000);

  // Catches: a clone stated for a directory that holds no pool, or of a repository the copy
  // lister does not read, taken in silence, so it maps nothing and no one learns why.
  test("a clone whose directory holds no pool of its repository as read, or of a repository the owners do not hold, is refused by name", async () => {
    const misplaced = await run(clonedWorld(), only, {
      targets,
      clones: [clone, { ...clone, dir: ".secret" }],
    });
    expect(misplaced.code).toBe(2);
    expect(misplaced.errors.join("\n")).toContain(
      `is refused:\n  clones[1].dir: acme/dots at ${sha1("commit acme/dots")} holds no skill pool in .secret (the pools of its skills-sources.json files: .private/dots)`
    );
    expect(existsSync(misplaced.out)).toBe(false);
    const unheld = await run(clonedWorld(), only, {
      targets,
      clones: [{ ...clone, clone: "other/private" }],
    });
    expect(unheld.code).toBe(2);
    expect(unheld.errors.join("\n")).toContain(
      "clones[0].clone: other/private is not a repository of an owner in owners"
    );
  }, 60_000);
});

// The prepack script's earlier versions in the count's window: F6's cybermonklabs/legion, whose
// script at the commit read (F6-contents-pi-plugin-prepack.json) stages the two thermonuclear
// skills for @acme/pi-envoy, and a made-up older version, in effect at the window's start, that
// staged thermonuclear-deep-review and ce-simplify-code (a skill the tree read no longer holds) for
// @acme/pi-legion instead, as cybermonklabs/legion's did before 4c2dc60c.
describe("the paths a prepack script staged a skill at during the window and stages no longer", () => {
  const newCache = temporaryDirectories("copies-cache-");
  const SCRIPT = "scripts/pi-plugin-prepack.sh";
  const OLD = sha1("an older prepack script");
  const MOVED = sha1("the commit that moved the thermonuclear skills");
  const REMOVED = sha1("the commit that removed leading-a-work-stream");
  const current = () => {
    const contents = fixtureJson("F6-contents-pi-plugin-prepack.json") as { content: string };
    return Buffer.from(contents.content.replace(/\n/g, ""), "base64").toString("utf8");
  };
  /** The made-up older version of `text`, staging `more` (each after a space) beside
   *  ce-simplify-code for @acme/pi-legion. */
  const older = (text: string, more = "") => {
    const changed = text
      .replace(
        "plain-words thermonuclear-code-quality thermonuclear-deep-review\n",
        "plain-words\n"
      )
      .replace(
        "legion-worker\n      simplify-settled-diff\n",
        `legion-worker\n      ce-simplify-code${more} thermonuclear-deep-review\n`
      );
    if (changed === text) throw new Error("the made-up older script is the fixture's own");
    return changed;
  };
  const only = ["--only", "cybermonklabs/legion", "--max-search-calls", "0"];

  // Catches: the 849 sessions of older worker images that read
  // /opt/legion/pi-legion/dist/skills/thermonuclear-deep-review/ placed at no copy, since the
  // prefix table held only the paths of the script at the commit read.
  test("each is a prefix row of the package's unit mapped to the copy of its name, and a note names the commits that staged it there", async () => {
    const w = world();
    w.histories.set(`cybermonklabs/legion ${SCRIPT}`, [
      { oid: MOVED, date: "2026-11-07T01:36:22Z", text: current() },
      { oid: OLD, date: "2026-10-09T11:26:34Z", text: older(current()) },
    ]);
    const r = await run(w, only);
    expect(r.code).toBe(0);
    const via = "package:@acme/pi-legion (cybermonklabs/legion:packages/pi-legion/package.json)";
    const rows = r
      .table("prefix-table.tsv")
      .filter(
        (row) => row.name === "thermonuclear-deep-review" && row.prefix?.includes("pi-legion")
      );
    expect(rows).toEqual([
      {
        prefix: "**/node_modules/@acme/pi-legion/dist/skills/thermonuclear-deep-review/",
        repository: "cybermonklabs/legion",
        path: "skills/thermonuclear-deep-review",
        name: "thermonuclear-deep-review",
        via,
      },
      {
        prefix: "/opt/legion/pi-legion/dist/skills/thermonuclear-deep-review/",
        repository: "cybermonklabs/legion",
        path: "skills/thermonuclear-deep-review",
        name: "thermonuclear-deep-review",
        via,
      },
    ]);
    const notes = r.text("skill-copies.md");
    expect(notes).toContain(
      `  - staged thermonuclear-deep-review for @acme/pi-legion at dist/skills/thermonuclear-deep-review/ from ${OLD} (2026-10-09T11:26:34Z) until ${MOVED} (2026-11-07T01:36:22Z), and stages it there no longer; the prefix rows **/node_modules/@acme/pi-legion/dist/skills/thermonuclear-deep-review/, /opt/legion/pi-legion/dist/skills/thermonuclear-deep-review/ map it to cybermonklabs/legion skills/thermonuclear-deep-review\n`
    );
    // GitHub names no commit since the window's start that changed its SKILL.md, so no row.
    expect(notes).toContain(
      `  - staged ce-simplify-code for @acme/pi-legion at dist/skills/ce-simplify-code/ from ${OLD} (2026-10-09T11:26:34Z) until ${MOVED} (2026-11-07T01:36:22Z), and stages it there no longer; no prefix row maps that path: the tree read holds no SKILL.md at skills/ce-simplify-code, and no commit since 2026-10-23T00:00:00Z changed skills/ce-simplify-code/SKILL.md\n`
    );
    expect(r.text("prefix-table.tsv")).not.toContain("ce-simplify-code");
    // A run with no earlier version holds none of those rows.
    const plain = await run(world(), only);
    expect(plain.text("prefix-table.tsv")).not.toContain(
      "/opt/legion/pi-legion/dist/skills/thermonuclear-deep-review/"
    );
  }, 60_000);

  // Catches: R1 on R4's list main13, where the 1,084 read sessions of
  // /opt/legion/pi-legion/dist/skills/ce-simplify-code/ (cybermonklabs/legion's prepack staged its
  // skill there until #2896 renamed it simplify-settled-diff at 4c2dc60c) had no prefix row, the
  // tree read holding no copy of that name.
  test("a path of a skill git renamed since gets rows to the renamed copy under its former name, a removed skill's path is a note alone, and a run started again reads neither from GitHub", async () => {
    const cacheDir = newCache();
    const args = [...only, "--cache-dir", cacheDir];
    const histories = (w: World) =>
      w.histories.set(`cybermonklabs/legion ${SCRIPT}`, [
        { oid: MOVED, date: "2026-11-07T01:36:22Z", text: current() },
        {
          oid: OLD,
          date: "2026-10-09T11:26:34Z",
          text: older(current(), " leading-a-work-stream"),
        },
      ]);
    const w = world();
    histories(w);
    w.pathCommits.set("cybermonklabs/legion skills/ce-simplify-code/SKILL.md", [
      { sha: MOVED, date: "2026-11-07T01:36:22Z" },
    ]);
    w.pathCommits.set("cybermonklabs/legion skills/leading-a-work-stream/SKILL.md", [
      { sha: REMOVED, date: "2026-11-10T01:48:50Z" },
    ]);
    // In the shape of GitHub's answer for cybermonklabs/legion's 4c2dc60c and cd4874e0 (gh api
    // repos/cybermonklabs/legion/commits/<sha>), each cut to two of its files.
    w.commitFiles.set(`cybermonklabs/legion ${MOVED}`, [
      { filename: "skills/legion-worker/SKILL.md", status: "modified" },
      {
        filename: "skills/simplify-settled-diff/SKILL.md",
        status: "renamed",
        previous_filename: "skills/ce-simplify-code/SKILL.md",
      },
    ]);
    w.commitFiles.set(`cybermonklabs/legion ${REMOVED}`, [
      { filename: "skills/leading-a-work-stream/SKILL.md", status: "removed" },
      { filename: "skills/leading-a-work-stream/references/daily-plan.md", status: "removed" },
    ]);
    const r = await run(w, args);
    expect(r.code).toBe(0);
    const via = "package:@acme/pi-legion (cybermonklabs/legion:packages/pi-legion/package.json)";
    const locations = ["**/node_modules/@acme/pi-legion/", "/opt/legion/pi-legion/"];
    expect(r.table("prefix-table.tsv").filter((row) => row.name === "ce-simplify-code")).toEqual(
      locations.map((location) => ({
        prefix: `${location}dist/skills/ce-simplify-code/`,
        repository: "cybermonklabs/legion",
        path: "skills/simplify-settled-diff",
        name: "ce-simplify-code",
        via,
      }))
    );
    expect(r.text("prefix-table.tsv")).not.toContain("leading-a-work-stream");
    const renamed = `git renamed skills/ce-simplify-code/SKILL.md to skills/simplify-settled-diff/SKILL.md at ${MOVED} (2026-11-07T01:36:22Z), so the prefix rows ${locations.map((location) => `${location}dist/skills/ce-simplify-code/`).join(", ")} map it to cybermonklabs/legion skills/simplify-settled-diff under its former name ce-simplify-code`;
    const removed = `no prefix row maps that path: the tree read holds no SKILL.md at skills/leading-a-work-stream, and git removed skills/leading-a-work-stream/SKILL.md at ${REMOVED} (2026-11-10T01:48:50Z)`;
    const stage = (skill: string, copy: string, renamedTo: string, note: string) =>
      locations.map((location) => ({
        prefix: `${location}dist/skills/${skill}/`,
        script_repository: "cybermonklabs/legion",
        script_path: SCRIPT,
        package: "@acme/pi-legion",
        skill,
        until: MOVED,
        renamed_to: renamedTo,
        repository: copy === "" ? "" : "cybermonklabs/legion",
        path: copy,
        note,
      }));
    expect(r.text("former-stages.tsv").split("\n")[0]).toBe(FORMER_STAGES_COLUMNS.join("\t"));
    expect(
      r
        .table("former-stages.tsv")
        .filter((row) => row.skill !== "thermonuclear-deep-review")
        .sort((a, b) => (a.skill ?? "").localeCompare(b.skill ?? ""))
    ).toEqual([
      ...stage(
        "ce-simplify-code",
        "skills/simplify-settled-diff",
        "skills/simplify-settled-diff",
        renamed
      ),
      ...stage("leading-a-work-stream", "", "", removed),
    ]);
    const said = (skill: string) =>
      `  - staged ${skill} for @acme/pi-legion at dist/skills/${skill}/ from ${OLD} (2026-10-09T11:26:34Z) until ${MOVED} (2026-11-07T01:36:22Z), and stages it there no longer; `;
    const notes = r.text("skill-copies.md");
    expect(notes).toContain(`${said("ce-simplify-code")}${renamed}\n`);
    expect(notes).toContain(`${said("leading-a-work-stream")}${removed}\n`);
    // Started again on the same cache, with a GitHub that names no such commit: each read comes
    // from the cache, so the tables are the same and neither read is made.
    const again = world();
    histories(again);
    const second = await run(again, args);
    expect(
      again.calls.filter((call) =>
        /\/commits(\?sha=[^&]+&path=|\/[0-9a-f]{40}$)/.test(call[1] ?? "")
      )
    ).toEqual([]);
    for (const name of ["prefix-table.tsv", "former-stages.tsv"])
      expect(second.text(name)).toBe(r.text(name));
  }, 60_000);
});

// Catches: R1 on a list made from the sources at 4eea3c9f, where the dev-box sessions' reads of a
// marketplace repository's plugin cache at its older layout (the skills of running-heavy-jobs at
// `<marketplace>___dev-infra___*/skills/`, 719 paths under `<marketplace>___<plugin>___*` of a
// plugin the repository dropped) had no prefix row, the skills now at other plugins' directories:
// running-heavy-jobs fell from 2,218 read sessions to 494, 1,724 of them unattributed.
describe("the cache locations a marketplace gave a skill of its repository during the window and gives no more", () => {
  const MANIFEST = ".claude-plugin/marketplace.json";
  const BEFORE = sha1("the manifest before the window");
  const MOVED = sha1("the commit that renamed the marketplace and moved the skills");
  const LATER = sha1("the commit that moved alpha again");
  const newCache = temporaryDirectories("copies-cache-");
  const CURRENT = ACME_FILES.find((file) => file.path === MANIFEST)?.text ?? "";
  const KIT = "plugin:acme-kit@acme-old-market";
  const GAMMA = "plugin:plugin-gamma@acme-old-market";
  const via = (unit: string) => `${unit} (acme/skills:${MANIFEST})`;
  /** The two cache locations of a plugin, as Oh My Pi's cache and Claude Code's name them. */
  const locations = (marketplace: string, plugin: string) => [
    `**/plugins/cache/plugins/${marketplace}___${plugin}___*/`,
    `**/plugins/cache/${marketplace}/${plugin}/*/`,
  ];
  const only = ["--only", "acme/skills", "--max-search-calls", "0"];

  /** The world with the manifest's three versions: the one before the window (an older
   *  marketplace name, a plugin acme-kit that held tool-loop-alpha, alpha and gone, and
   *  plugin-gamma), that of the commit that renamed the marketplace, moved tool-loop-alpha,
   *  removed gone and moved alpha to a directory of no plugin, and that of a later commit that
   *  moved alpha again, to where it is now. */
  const moved = () => {
    const w = world();
    const old = JSON.stringify({
      name: "acme-old-market",
      plugins: ["acme-kit", "plugin-gamma"].map((name) => ({ name, source: `./plugins/${name}` })),
    });
    w.histories.set(`acme/skills ${MANIFEST}`, [
      { oid: LATER, date: "2026-11-09T05:00:00Z", text: `${CURRENT}\n` },
      { oid: MOVED, date: "2026-11-07T00:46:53Z", text: CURRENT },
      { oid: BEFORE, date: "2026-10-09T11:26:34Z", text: old },
    ]);
    w.commitFiles.set(`acme/skills ${MOVED}`, [
      { filename: MANIFEST, status: "modified" },
      {
        filename: "plugins/acme-tools/skills/tool-loop-alpha/SKILL.md",
        status: "renamed",
        previous_filename: "plugins/acme-kit/skills/tool-loop-alpha/SKILL.md",
      },
      {
        filename: "plugins/acme-mid/skills/alpha/SKILL.md",
        status: "renamed",
        previous_filename: "plugins/acme-kit/skills/alpha/SKILL.md",
      },
      { filename: "plugins/acme-kit/skills/gone/SKILL.md", status: "removed" },
      // A file of a skill other than its SKILL.md is no skill directory's move.
      {
        filename: "plugins/acme-tools/skills/tool-loop-alpha/references/x.md",
        status: "renamed",
        previous_filename: "plugins/acme-kit/skills/tool-loop-alpha/references/x.md",
      },
    ]);
    w.commitFiles.set(`acme/skills ${LATER}`, [
      { filename: MANIFEST, status: "modified" },
      {
        filename: "plugins/acme-tools/skills/alpha/SKILL.md",
        status: "renamed",
        previous_filename: "plugins/acme-mid/skills/alpha/SKILL.md",
      },
    ]);
    return w;
  };

  test("a skill git moved gets the old plugin's cache locations mapped to the copy it went to, a skill still in a renamed marketplace's plugin directory the same, a removed one a note alone, and each former-stages.tsv row says which", async () => {
    const r = await run(moved(), only);
    expect(r.code).toBe(0);
    const rows = (unit: string) =>
      r
        .table("prefix-table.tsv")
        .filter((row) => row.via === via(unit))
        .map((row) => `${row.prefix} -> ${row.repository} ${row.path} (${row.name})`)
        .sort();
    expect(rows(KIT)).toEqual(
      ["alpha", "tool-loop-alpha"]
        .flatMap((skill) =>
          locations("acme-old-market", "acme-kit").map(
            (location) =>
              `${location}skills/${skill}/ -> acme/skills plugins/acme-tools/skills/${skill} (${skill})`
          )
        )
        .sort()
    );
    expect(rows(GAMMA)).toEqual(
      locations("acme-old-market", "plugin-gamma")
        .map(
          (location) =>
            `${location}skills/gamma-one/ -> acme/skills plugins/plugin-gamma/skills/gamma-one (gamma-one)`
        )
        .sort()
    );
    expect(r.text("prefix-table.tsv")).not.toContain("/skills/gone/");
    const stage = (row: Record<string, string>) =>
      [
        row.prefix,
        row.package,
        row.skill,
        row.until,
        row.renamed_to,
        row.repository,
        row.path,
      ].join(" | ");
    expect(
      r
        .table("former-stages.tsv")
        .filter((row) => row.script_repository === "acme/skills" && row.script_path === MANIFEST)
        .map(stage)
        .sort()
    ).toEqual(
      [
        ...locations("acme-old-market", "acme-kit").flatMap((location) => [
          `${location}skills/alpha/ | ${KIT} | alpha | ${MOVED} | plugins/acme-tools/skills/alpha | acme/skills | plugins/acme-tools/skills/alpha`,
          `${location}skills/gone/ | ${KIT} | gone | ${MOVED} |  |  | `,
          `${location}skills/tool-loop-alpha/ | ${KIT} | tool-loop-alpha | ${MOVED} | plugins/acme-tools/skills/tool-loop-alpha | acme/skills | plugins/acme-tools/skills/tool-loop-alpha`,
        ]),
        ...locations("acme-old-market", "plugin-gamma").map(
          (location) =>
            `${location}skills/gamma-one/ | ${GAMMA} | gamma-one | ${MOVED} |  | acme/skills | plugins/plugin-gamma/skills/gamma-one`
        ),
      ].sort()
    );
    const said = (skill: string) =>
      r
        .table("former-stages.tsv")
        .find((row) => row.package === KIT && row.skill === skill && row.prefix.includes("___"))
        ?.note;
    expect(said("alpha")).toBe(
      `${KIT} (plugins/acme-kit) held plugins/acme-kit/skills/alpha/SKILL.md, and git renamed it to plugins/acme-mid/skills/alpha/SKILL.md at ${MOVED} (2026-11-07T00:46:53Z), and then renamed it to plugins/acme-tools/skills/alpha/SKILL.md at ${LATER} (2026-11-09T05:00:00Z); the prefix rows map it to acme/skills plugins/acme-tools/skills/alpha`
    );
    expect(said("gone")).toBe(
      `${KIT} (plugins/acme-kit) held plugins/acme-kit/skills/gone/SKILL.md, and git removed it at ${MOVED} (2026-11-07T00:46:53Z); no prefix row maps that path`
    );
    expect(r.text("skill-copies.md")).toContain(
      `${KIT} (plugins/acme-kit) is named so from ${BEFORE} (2026-10-09T11:26:34Z) to ${BEFORE}, and no more from ${MOVED} (2026-11-07T00:46:53Z); its former-path prefix rows map 2 skills git moved out of plugins/acme-kit/skills/ to their copies, and 0 skills still there, and 1 skills that git removed or moved to no copy of the tree read have none`
    );
  }, 60_000);

  // Catches: a commit whose file list GitHub cut at one page read as whole, so a SKILL.md it moved
  // past that page got no prefix row, with no note and no failure.
  test("a commit that changed the manifest and lists 300 files, the most GitHub lists on one page, gets a note and a failure that a move may be missed, and the moves it lists still get their rows", async () => {
    const w = moved();
    const listed = w.commitFiles.get(`acme/skills ${MOVED}`);
    if (!Array.isArray(listed)) throw new Error(`moved() lists the files of ${MOVED}`);
    w.commitFiles.set(`acme/skills ${MOVED}`, [
      ...listed,
      ...Array.from({ length: 300 - listed.length }, (_, n) => ({
        filename: `docs/page-${n}.md`,
        status: "modified",
      })),
    ]);
    const r = await run(w, only);
    const cut = `${MOVED} lists 300 files, the most GitHub lists on one page, so its list may be cut`;
    const notes = r.text("skill-copies.md");
    expect(notes).toContain(`\n  - the files of its version at ${MOVED} may miss a move: ${cut}\n`);
    expect(notes.slice(notes.indexOf("\n## Failures\n"))).toContain(
      `\n- the SKILL.md moves of acme/skills:${MANIFEST} at ${MOVED} may be missed: ${cut}\n`
    );
    expect(
      r
        .table("prefix-table.tsv")
        .filter((row) => row.via === via(KIT))
        .map((row) => row.name)
        .sort()
    ).toEqual(["alpha", "alpha", "tool-loop-alpha", "tool-loop-alpha"]);
  }, 60_000);

  test("a run started again on the same cache reads neither the manifest's versions nor a commit's files from GitHub", async () => {
    const cacheDir = newCache();
    const args = [...only, "--cache-dir", cacheDir];
    const first = await run(moved(), args);
    expect(first.table("former-stages.tsv").some((row) => row.script_path === MANIFEST)).toBe(true);
    const again = world();
    const second = await run(again, args);
    expect(
      again.calls.filter(
        (call) =>
          call.join(" ").includes("history(path:") || /\/commits\/[0-9a-f]{40}$/.test(call[1] ?? "")
      )
    ).toEqual([]);
    for (const name of ["prefix-table.tsv", "former-stages.tsv"])
      expect(second.text(name)).toBe(first.text(name));
  }, 60_000);
});

describe("the full run over the owners", () => {
  const w = world();
  let r: Run;
  beforeAll(async () => {
    r = await run(w);
  });

  test("it exits 0 and writes every output", () => {
    expect(r.code).toBe(0);
    expect(readdirSync(r.out).sort()).toEqual(
      [
        "skill-copies.tsv",
        "prefix-table.tsv",
        "repositories.tsv",
        "loaders.tsv",
        "installers.tsv",
        "manifests.tsv",
        "queries.jsonl",
        "skills.jsonl",
        "skill-copies.md",
        "former-stages.tsv",
      ].sort()
    );
    // Each table holds a row below its header, and each other file holds text.
    for (const name of [
      "skill-copies.tsv",
      "prefix-table.tsv",
      "repositories.tsv",
      "loaders.tsv",
      "installers.tsv",
      "manifests.tsv",
    ]) {
      expect({ name, rows: r.table(name).length > 0 }).toEqual({ name, rows: true });
    }
    for (const name of ["queries.jsonl", "skills.jsonl", "skill-copies.md"]) {
      expect({ name, text: r.text(name).length > 0 }).toEqual({ name, text: true });
    }
  });

  test.each(
    MANIFEST_KINDS.map((kind) => kind.kind)
  )("the %s manifest kind is read and shown", (kind) => {
    const example = KIND_EXAMPLES[kind];
    const shown = r
      .table("manifests.tsv")
      .find((row) => row.repository === example.repository && row.path === example.path);
    expect(shown?.kind).toBe(kind);
    expect(shown?.error).toBe("");
    expect(shown?.says ?? "").toContain(example.says);
    if (example.row !== "")
      expect(installerRows(r, example.repository, example.path)).toContain(example.row);
  });

  test("a Legion deployment's role setup, program and worker image are read as manifests: the catalog its roles install from, the plugins every role installs, and the skill the image installs from a release", () => {
    const kinds = new Map(
      r.table("manifests.tsv").map((row) => [`${row.repository} ${row.path}`, row.kind])
    );
    expect(kinds.get("acme/skills legion/acme-setup.sh")).toBe("legion-role-setup");
    expect(kinds.get("acme/skills legion/main.go")).toBe("legion-program");
    expect(kinds.get("acme/skills legion/worker/Dockerfile")).toBe("worker-image");
    expect(installerRows(r, "acme/skills", "legion/worker/Dockerfile")).toContain(
      "installs image:acme-alpha alpha acme/skills skills/alpha"
    );
  });

  test("every tree file of a manifest kind that names skills is shown (the class inventory)", () => {
    const shown = new Set(
      r.table("manifests.tsv").map((row) => `${row.repository} ${row.path} ${row.kind}`)
    );
    const trees: [string, TreeEntry[]][] = [
      ["cybermonklabs/legion", legionTree().entries],
      ["acme/skills", treeOf("acme/skills", [...madeUpFiles(), ...ACME_FILES])],
      ["acme/old-skills", treeOf("acme/old-skills", OLD_FILES)],
    ];
    let checked = 0;
    for (const [repository, entries] of trees) {
      for (const entry of entries) {
        const kind = manifestKind(entry);
        const text = w.blobs.get(entry.sha);
        if (kind === undefined || text === undefined) continue;
        if (!parseManifest(kind, entry.path, text).ofKind) continue;
        checked++;
        expect(shown).toContain(`${repository} ${entry.path} ${kind}`);
      }
    }
    expect(checked).toBeGreaterThanOrEqual(MANIFEST_KINDS.length);
  });

  test("the installers resolve through plugins, packages, links, settings and projects", () => {
    expect(installerRows(r, "cybermonklabs/legion", "scripts/pi-plugin-prepack.sh")).toContain(
      "stages package:@acme/pi-envoy dispatch cybermonklabs/legion skills/dispatch"
    );
    expect(installerRows(r, "cybermonklabs/legion", ".claude-plugin/marketplace.json")).toContain(
      "ships plugin:claude-envoy@legion-plugins"
    );
    expect(
      r.table("installers.tsv").find((row) => row.unit === "plugin:claude-envoy@legion-plugins")
        ?.note
    ).toBe("the npm package @acme/claude-envoy is in no package.json read");
    expect(installerRows(r, "acme/skills", ".claude-plugin/marketplace.json")).toContain(
      "ships plugin:acme-envoy@acme-market beta acme/skills skills/beta"
    );
    // An npm-sourced plugin whose package.json has no omp.skills: the package is still found, its
    // plugin.json is that plugin's, and its skills/ link reaches the copy.
    expect(installerRows(r, "acme/skills", ".claude-plugin/marketplace.json")).toContain(
      "ships plugin:acme-claude@acme-market alpha acme/skills skills/alpha"
    );
    expect(r.text("skill-copies.md")).toMatch(
      /^Read by path and naming no skill, plugin or package, so not listed: (.*, )?1 omp-package(, .*)?\.$/m
    );
    expect(installerRows(r, "acme/skills", ".claude/settings.json")).toContain(
      "disables plugin:beta-pack@acme-market"
    );
    expect(installerRows(r, "acme/skills", "legion/projects/10-ACME.yaml")).toContain(
      "leaves out project:ACME"
    );
    expect(installerRows(r, "acme/skills", "omp/config.yml")).toContain(
      "ignores omp-settings beta acme/skills skills/beta"
    );
    expect(installerRows(r, "acme/skills", "installers/skills.sh")).toEqual(
      expect.arrayContaining([
        "links user-skills:claude-code alpha acme/skills skills/alpha",
        "links user-skills:omp legion-worker acme/skills skills/legion-worker",
        "links config omp-settings",
      ])
    );
    expect(installerRows(r, "acme/skills", "packages/acme-envoy/package.json")).toContain(
      "ships package:@acme/acme-envoy beta acme/skills skills/beta"
    );
    // The plugin.json a marketplace names is read into that marketplace's plugin, not a unit of
    // its own.
    expect(
      r
        .table("installers.tsv")
        .some((row) => row.unit === "plugin:acme-tools" || row.unit === "plugin:acme-claude")
    ).toBe(false);
  });

  test("the copies, their marks, installers and loaders are one row each in skill-copies.tsv", () => {
    expect(r.text("skill-copies.tsv").split("\n")[0]).toBe(SKILL_COPIES_COLUMNS.join("\t"));
    const copies = r.table("skill-copies.tsv");
    const old = copies.find((row) => row.repository === "acme/old-skills");
    expect(old).toMatchObject({
      path: "skills/alpha",
      name: "alpha",
      fork: "yes",
      archived: "yes",
      parent: "acme/skills",
    });
    const alpha = copies.find(
      (row) => row.repository === "acme/skills" && row.path === "skills/alpha"
    );
    expect(alpha?.description_bytes).toBe(
      String(Buffer.byteLength("Use when the alpha steps are needed."))
    );
    expect(alpha?.installers).toContain("links user-skills:omp (acme/skills:installers/skills.sh)");
    expect(alpha?.loaded_by).toContain(
      "acme/skills:tree/init.py (python-loader loader:tree/init.py#roles.proposer)"
    );
    const legion = copies.find(
      (row) => row.repository === "cybermonklabs/legion" && row.path === "skills/legion-worker"
    );
    expect(legion?.installers).toContain(
      "ships plugin:legion (cybermonklabs/legion:packages/claude-envoy/scripts/build.ts)"
    );
    expect(legion?.installers).toContain(
      "stages package:@acme/pi-legion (cybermonklabs/legion:scripts/pi-plugin-prepack.sh)"
    );
    expect(
      copies.find((row) => row.path === "skills/thermonuclear-deep-review")?.loaded_by
    ).toContain(
      "cybermonklabs/legion:packages/pi-legion/agents/thermonuclear-deep-review.md (agent-definition agent:thermonuclear-deep-review)"
    );
    expect(copies.some((row) => row.path.startsWith("pipeline/candidates/"))).toBe(false);
    expect(copies.find((row) => row.path === "plugins/plugin-gamma/skills/gamma-one")?.name).toBe(
      "gamma-one"
    );
    expect(
      copies.find((row) => row.repository === "acme/skills" && row.path === "skills/dispatch")
        ?.description_bytes
    ).toBe(String(Buffer.byteLength("Use when: a made-up task needs a ticket")));
  });

  test("shared names: across repositories and twice in one, forks and archived copies marked", () => {
    const md = r.text("skill-copies.md");
    expect(md).toContain(
      "- `alpha` (in 2 repositories; twice or more in acme/skills): acme/old-skills skills/alpha (fork of acme/skills, archived); acme/skills plugins/acme-tools/skills/alpha; acme/skills skills/alpha"
    );
    expect(md).toContain(
      "- `legion-worker` (in 2 repositories): acme/skills skills/legion-worker; cybermonklabs/legion skills/legion-worker"
    );
    expect(md).toContain("- `dispatch` (in 2 repositories)");
  });

  test("a repository whose tree read fails, an owner whose listing fails and a failed read are each named with the error", () => {
    const omp = r
      .table("repositories.tsv")
      .find((row) => row.repository === "cybermonklabs/oh-my-pi");
    expect(omp).toMatchObject({
      fork: "yes",
      parent: "upstream-owner/oh-my-pi",
      error: "tree read failed: gh: Not Found (HTTP 404)",
    });
    const md = r.text("skill-copies.md");
    expect(md).toContain(
      "- cybermonklabs/oh-my-pi (fork of upstream-owner/oh-my-pi): tree read failed: gh: Not Found (HTTP 404)"
    );
    expect(md).toContain(
      "- owner acme-gone: the listing failed: GraphQL: Could not resolve to a User with the login of 'acme-gone'."
    );
    expect(md).toContain(
      "- cybermonklabs/legion:skills/asd-ste100/SKILL.md: SKILL.md read failed: gh: Not Found (HTTP 404)"
    );
    const legion = r
      .table("repositories.tsv")
      .find((row) => row.repository === "cybermonklabs/legion");
    expect(legion).toMatchObject({
      commit: "63b3a613feb15cb191f87f4239467cdd37b62c82",
      tree_read: "whole",
      tree_entries: "105",
      error: "",
    });
  });

  test("the prefix table maps each installer kind to its repository copy", () => {
    expect(r.text("prefix-table.tsv").split("\n")[0]).toBe(PREFIX_TABLE_COLUMNS.join("\t"));
    const rows = new Set(
      r.table("prefix-table.tsv").map((row) => `${row.prefix} -> ${row.repository} ${row.path}`)
    );
    for (const expected of [
      // An Oh My Pi plugin package the prepack script stages, in the worker image and installed.
      "/opt/legion/pi-legion/dist/skills/legion-worker/ -> cybermonklabs/legion skills/legion-worker",
      "/opt/legion/pi-envoy/dist/skills/dispatch/ -> cybermonklabs/legion skills/dispatch",
      "**/node_modules/@acme/pi-legion/dist/skills/legion-worker/ -> cybermonklabs/legion skills/legion-worker",
      // The plugin the build script generates.
      "/opt/legion/claude-envoy/legion/skills/legion-worker/ -> cybermonklabs/legion skills/legion-worker",
      // A marketplace plugin, in Oh My Pi's cache and Claude Code's.
      "**/plugins/cache/plugins/acme-market___acme-tools___*/skills/alpha/ -> acme/skills plugins/acme-tools/skills/alpha",
      "**/plugins/cache/acme-market/acme-tools/*/skills/tool-loop-alpha/ -> acme/skills plugins/acme-tools/skills/tool-loop-alpha",
      // An npm-sourced marketplace plugin and its package, through a link.
      "**/plugins/cache/acme-market/acme-envoy/*/skills/beta/ -> acme/skills skills/beta",
      // Its Oh My Pi package ships the copy its prepack script stages in dist/skills.
      "**/node_modules/@acme/acme-envoy/dist/skills/beta/ -> acme/skills skills/beta",
      "/opt/legion/acme-envoy/dist/skills/beta/ -> acme/skills skills/beta",
      "**/plugins/cache/acme-market/acme-claude/*/skills/alpha/ -> acme/skills skills/alpha",
      // An installer's links into each host's user skills.
      "**/.omp/agent/skills/beta/ -> acme/skills skills/beta",
      "**/.claude/skills/alpha/ -> acme/skills skills/alpha",
    ]) {
      expect(rows).toContain(expected);
    }
    expect(r.text("skill-copies.md")).not.toContain(
      "rows point at repositories the configuration's targets do not name"
    );
  });

  test("a code-search hit is kept only where it names the skill, and a substring of a longer name is not", () => {
    const alpha = r
      .queries()
      .find((query) => query.q === '"alpha" user:acme') as unknown as QueryRecord;
    expect(alpha.hits).toEqual([
      {
        repository: "acme/skills",
        path: "docs/guide.md",
        verdict: "dropped: only inside a longer name",
        match: "",
      },
      {
        repository: "acme/skills",
        path: "plugins/acme-tools/skills/alpha/SKILL.md",
        verdict: "inside the skill plugins/acme-tools/skills/alpha",
        match: "",
      },
      {
        repository: "acme/skills",
        path: "tree/init.py",
        verdict: "kept",
        match: "quoted-string,code-load",
      },
      {
        repository: "acme/skills",
        path: "docs/gone.md",
        verdict: `not in the tree at ${sha1("commit acme/skills")}`,
        match: "",
      },
    ]);
    const loaders = r.table("loaders.tsv");
    expect(loaders).toContainEqual({
      repository: "acme/skills",
      path: "docs/guide.md",
      skill: "beta",
      match: "skill-uri",
      source: "code search",
    });
    expect(loaders.some((row) => row.path === "docs/guide.md" && row.skill === "alpha")).toBe(
      false
    );
    expect(loaders).toContainEqual({
      repository: "acme/skills",
      path: "docs/guide.md",
      skill: "tool-loop-alpha",
      match: "name",
      source: "code search",
    });
    expect(
      loaders.find((row) => row.path === "tree/init.py" && row.skill === "alpha")
    ).toMatchObject({
      match: "quoted-string,code-load,manifest:python-loader",
      source: "code search, manifest",
    });
  });

  test("each name is searched once per owner listed, as the quoted name, and each query keeps its counts", () => {
    const queries = r.queries().filter((query) => query.type === "search");
    const names = [...new Set(r.table("skill-copies.tsv").map((row) => row.name))].sort();
    const owners = queries.filter((query) => query.reason === "owner");
    // acme-gone's listing failed, so it is not searched.
    expect(owners.map((query) => query.q).sort()).toEqual(
      names.flatMap((name) => [`"${name}" org:cybermonklabs`, `"${name}" user:acme`]).sort()
    );
    expect(queries.every((query) => query.status === "ok" && query.total_count !== null)).toBe(
      true
    );
  });

  test("a common word whose owner-wide search was read whole is not run again, and no query word is one the pod's gh refuses", () => {
    const queries = r.queries().filter((query) => query.type === "search");
    // dispatch and envoy are common words; every owner-wide search here is read whole.
    expect(queries.some((query) => query.name === "dispatch" && query.reason === "owner")).toBe(
      true
    );
    expect(queries.filter((query) => query.reason !== "owner")).toEqual([]);
    for (const query of queries) {
      for (const word of String(query.q).split(/\s+/)) {
        if (word.includes(":")) expect(word).toMatch(/^[A-Za-z][A-Za-z-]*:[^\s"'()\\]+$/);
      }
    }
  });

  test("blobs are read in GraphQL batches routed by their owner field, and only a blob no batch gives is read alone", () => {
    expect(w.batches.length).toBeGreaterThan(0);
    const read = new Set(["cybermonklabs/legion", "acme/skills", "acme/old-skills"]);
    for (const batch of w.batches) {
      expect(read).toContain(batch.repository);
      expect(batch.blobs).toBeLessThanOrEqual(50);
    }
    const alone = w.calls
      .map((call) => /\/git\/blobs\/([0-9a-f]{40})$/.exec(call[1] ?? "")?.[1])
      .filter((sha): sha is string => sha !== undefined);
    const failing = legionTree().entries.find(
      (entry) => entry.path === "skills/asd-ste100/SKILL.md"
    );
    const binary = OLD_FILES.find((file) => file.path === "assets/data.json");
    expect(new Set(alone)).toEqual(new Set([failing?.sha ?? "", blobSha(binary?.text ?? "")]));
  });

  test("a fork that holds a skill is read in the files it changed against its parent, under the size bound", () => {
    const scan = r.queries().find((query) => query.type === "fork-scan");
    expect(scan).toEqual({
      type: "fork-scan",
      repository: "acme/old-skills",
      read: `changed files against acme/skills@${OLD_BASE}`,
      changed_files: 3,
      max_bytes: 100_000,
      files_read: 2,
      skipped_large: 1,
      skipped_binary: 1,
      files_naming_a_skill: 1,
      failures: [],
    });
    expect(r.table("loaders.tsv")).toContainEqual({
      repository: "acme/old-skills",
      path: "README.md",
      skill: "beta",
      match: "skill-uri",
      source: "fork scan",
    });
    expect(w.calls.some((call) => call.join(" ").includes("repo:acme/old-skills"))).toBe(false);
  });

  test("same-job pairs and the sentence are left to the read command's readers, with the skills list beside the copies", () => {
    expect(r.text("skill-copies.md")).toContain(
      "the `read` command's `same-job` and `sentence` readers find them, from an input file of their own shape. skills.jsonl holds each copy with its whole description."
    );
    // skills.jsonl is written by copies.ts from readerSkills; read each line back as that type.
    const skills = r
      .text("skills.jsonl")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line) as ReaderSkill);
    expect(
      skills.find((entry) => entry.repository === "acme/skills" && entry.path === "skills/beta")
        ?.description
    ).toBe("Use when beta's greeting is needed.");
  });
});

// The stand-in's code search answers for the full run above.
function acmeSearch(q: string): SearchReply {
  const item = (repository: string, file: string) => ({
    name: path.posix.basename(file),
    path: file,
    sha: sha1(file),
    repository: { full_name: repository, fork: false },
  });
  if (q === '"alpha" user:acme') {
    return {
      body: {
        total_count: 4,
        incomplete_results: false,
        items: [
          item("acme/skills", "docs/guide.md"),
          item("acme/skills", "plugins/acme-tools/skills/alpha/SKILL.md"),
          item("acme/skills", "tree/init.py"),
          item("acme/skills", "docs/gone.md"),
        ],
      },
    };
  }
  if (q === '"beta" user:acme')
    return {
      body: {
        total_count: 1,
        incomplete_results: false,
        items: [item("acme/skills", "docs/guide.md")],
      },
    };
  if (q === '"tool-loop-alpha" user:acme') {
    return {
      body: {
        total_count: 1,
        incomplete_results: false,
        items: [item("acme/skills", "docs/guide.md")],
      },
    };
  }
  return { body: EMPTY_SEARCH };
}

describe("one repository with --only, on F6's code search answer", () => {
  const w = world();
  // F6-search-code.json as GitHub gave it, with one more hit, made here, on the omp-plugins
  // package.json of the same tree, whose name `legion-worker-omp-plugins` holds the skill's name
  // inside a longer one.
  const f6 = fixtureJson("F6-search-code.json") as {
    total_count: number;
    incomplete_results: boolean;
    items: unknown[];
  };
  const substring = {
    name: "package.json",
    path: "packages/daemon/docker/omp-plugins/package.json",
    sha: "760e942cfe64bd19e53a564b563047f345e06ecb",
    repository: { full_name: "cybermonklabs/legion", fork: false },
  };
  w.search = (q) =>
    q === '"legion-worker" repo:cybermonklabs/legion'
      ? { body: { ...f6, items: [...f6.items, substring] } }
      : { body: EMPTY_SEARCH };
  let r: Run;
  beforeAll(async () => {
    r = await run(w, ["--only", "cybermonklabs/legion"]);
  });

  test("only the named repository is read, and its queries name it", () => {
    expect(r.code).toBe(0);
    expect(r.table("repositories.tsv").map((row) => row.repository)).toEqual([
      "cybermonklabs/legion",
    ]);
    expect(w.calls.some((call) => call[0] === "repo" && call[2] === "acme")).toBe(false);
    expect(r.queries().every((query) => query.scope === "repo:cybermonklabs/legion")).toBe(true);
  });

  test("F6's hits: kept where the name stands whole, dropped inside a longer name, inside a skill or not in the tree", () => {
    // queries.jsonl is written by copies.ts from its QueryRecord values; read one back as that
    // type.
    const query = r
      .queries()
      .find(
        (entry) => entry.q === '"legion-worker" repo:cybermonklabs/legion'
      ) as unknown as QueryRecord;
    expect(query.total_count).toBe(325);
    expect(query.incomplete_results).toBe(false);
    expect(query.items_read).toBe(31);
    // F6's answer is GitHub's first page of 30 (gh's default page size) of 325: the hits it did not
    // give are counted and listed, never dropped.
    expect(query.unread).toBe(294);
    expect(r.text("skill-copies.md")).toContain(
      '- "legion-worker" repo:cybermonklabs/legion: 325 hits, 31 read, 294 not read (GitHub\'s pages held fewer hits than its count)'
    );
    const verdict = (file: string) => query.hits.find((hit) => hit.path === file);
    expect(verdict("scripts/pi-plugin-prepack.sh")).toMatchObject({
      verdict: "kept",
      match: "name",
    });
    expect(verdict("packages/daemon/docker/omp-plugins/package.json")?.verdict).toBe(
      "dropped: only inside a longer name"
    );
    expect(verdict("skills/legion-worker/SKILL.md")?.verdict).toBe(
      "inside the skill skills/legion-worker"
    );
    expect(verdict("README.md")?.verdict).toBe(
      "not in the tree at 63b3a613feb15cb191f87f4239467cdd37b62c82"
    );
    const loaders = r.table("loaders.tsv").filter((row) => row.skill === "legion-worker");
    expect(loaders.map((row) => row.path)).toEqual(["scripts/pi-plugin-prepack.sh"]);
  });

  test("a target the configuration does not name is reported, and its rows are left out of the table", async () => {
    const other = await run(world(), [], { targets: ["cybermonklabs/legion"] });
    const md = other.text("skill-copies.md");
    expect(md).toContain(
      "rows point at repositories the configuration's targets do not name, so they are not in the table: acme/skills."
    );
    expect(md).toContain("- `**/.omp/agent/skills/beta/` -> acme/skills skills/beta");
    expect(
      other.table("prefix-table.tsv").every((row) => row.repository === "cybermonklabs/legion")
    ).toBe(true);
  }, 60_000);
});

describe("a fork read against its parent", () => {
  const commit = sha1("commit acme/old-skills");
  const compareKey = `acme/old-skills acme:main...${commit}`;
  const only = ["--only", "acme/old-skills", "--only", "acme/skills", "--max-search-calls", "0"];
  /** Five more text files of the fork, each naming the skill beta. Its compare lists one changed,
   *  one removed and one renamed (from docs/old-name.md), not docs/untouched.md; the tree holds
   *  the removed file and the old name too, so only the compare keeps them from being read. */
  const EXTRA: MadeFile[] = [
    { path: "docs/changed.md", text: "Load skill://beta: changed.\n" },
    { path: "docs/untouched.md", text: "Load skill://beta: untouched.\n" },
    { path: "docs/removed.md", text: "Load skill://beta: removed.\n" },
    { path: "docs/old-name.md", text: "Load skill://beta: the old name.\n" },
    { path: "docs/new-name.md", text: "Load skill://beta: the new name.\n" },
  ];
  const FILES: CompareAnswer["files"] = [
    { filename: "docs/changed.md", status: "modified" },
    { filename: "docs/removed.md", status: "removed" },
    { filename: "docs/new-name.md", status: "renamed", previous_filename: "docs/old-name.md" },
  ];
  const WHOLE = [
    "README.md",
    "docs/changed.md",
    "docs/new-name.md",
    "docs/old-name.md",
    "docs/removed.md",
    "docs/untouched.md",
  ];
  const forkWorld = () => {
    const w = world();
    const files = [...OLD_FILES, ...EXTRA];
    w.addFiles(files);
    w.addRepository("acme/old-skills", treeOf("acme/old-skills", files));
    w.compares.set(compareKey, { merge_base_commit: { sha: OLD_BASE }, files: FILES });
    return w;
  };
  const loaderPaths = (r: Run) =>
    r
      .table("loaders.tsv")
      .filter((row) => row.repository === "acme/old-skills")
      .map((row) => row.path)
      .sort();
  const forkScan = (r: Run) => r.queries().find((query) => query.type === "fork-scan");

  test("only the files it changed are read, a renamed one under its new name and a removed one not at all", async () => {
    const w = forkWorld();
    const r = await run(w, only);
    expect(r.code).toBe(0);
    expect(
      w.calls.some(
        (call) => call[1] === `repos/acme/old-skills/compare/acme:main...${commit}?per_page=1`
      )
    ).toBe(true);
    expect(loaderPaths(r)).toEqual(["docs/changed.md", "docs/new-name.md"]);
    expect(forkScan(r)).toMatchObject({
      read: `changed files against acme/skills@${OLD_BASE}`,
      changed_files: 2,
      files_read: 2,
      files_naming_a_skill: 2,
    });
    expect(r.text("skill-copies.md")).toContain(
      `- the fork acme/old-skills (changed files against acme/skills@${OLD_BASE}): 2 text files of at most`
    );
    // No blob of a file the compare left out, removed or renamed away is read.
    const unread = ["docs/untouched.md", "docs/removed.md", "docs/old-name.md", "README.md"].map(
      (file) => blobSha([...OLD_FILES, ...EXTRA].find((made) => made.path === file)?.text ?? "")
    );
    expect(w.calls.filter((call) => unread.some((sha) => call.join(" ").includes(sha)))).toEqual(
      []
    );
    // Its skills are still read from its whole tree.
    expect(
      r.table("skill-copies.tsv").filter((row) => row.repository === "acme/old-skills")
    ).toHaveLength(1);
  }, 60_000);

  test("a compare of 299 files is read as changed files", async () => {
    const w = forkWorld();
    const more = Array.from({ length: 299 - FILES.length }, (_, index) => ({
      filename: `src/file-${index}.ts`,
      status: "added",
    }));
    w.compares.set(compareKey, {
      merge_base_commit: { sha: OLD_BASE },
      files: [...FILES, ...more],
    });
    const r = await run(w, only);
    expect(forkScan(r)).toMatchObject({
      read: `changed files against acme/skills@${OLD_BASE}`,
      changed_files: 298,
    });
    expect(loaderPaths(r)).toEqual(["docs/changed.md", "docs/new-name.md"]);
  }, 60_000);

  /** The error of a read GitHub answers 502 on each try: a 502 is sent again 4 times, then the
   *  read fails naming each of its 5 tries. */
  const FIVE_502 = `5 tries: ${[1, 2, 3, 4, 5].map((n) => `try ${n}: gh: Server Error (HTTP 502)`).join("; ")}`;

  test.each<[string, (w: World) => void, string]>([
    [
      "the listing names no parent",
      (w) =>
        w.listings.set(
          "acme",
          (w.listings.get("acme") as Record<string, unknown>[]).map((entry) =>
            entry.nameWithOwner === "acme/old-skills" ? { ...entry, parent: null } : entry
          )
        ),
      "whole tree: the listing names no parent",
    ],
    [
      "its parent is gone or not readable",
      (w) =>
        w.repositoryInfo.set("acme/old-skills", { full_name: "acme/old-skills", parent: null }),
      "whole tree: its parent acme/skills is gone or not readable",
    ],
    [
      "its repository's read fails",
      (w) => w.repositoryInfo.set("acme/old-skills", "gh: Server Error (HTTP 502)"),
      `whole tree: its parent could not be read (${FIVE_502})`,
    ],
    [
      "the compare fails",
      (w) => w.compares.set(compareKey, "gh: Server Error (HTTP 502)"),
      `whole tree: the compare with acme/skills failed (${FIVE_502})`,
    ],
    [
      "the compare lists 300 files, a cut list",
      (w) =>
        w.compares.set(compareKey, {
          merge_base_commit: { sha: OLD_BASE },
          files: Array.from({ length: 300 }, (_, index) => ({
            filename: `src/file-${index}.ts`,
            status: "added",
          })),
        }),
      "whole tree: the compare with acme/skills lists 300 files, the most GitHub lists, so its list may be cut",
    ],
  ])("a fork the compare cannot answer whole is read from its whole tree: %s", async (_why, change, read) => {
    const w = forkWorld();
    change(w);
    const r = await run(w, only);
    expect(r.code).toBe(0);
    expect(forkScan(r)).toMatchObject({ read, changed_files: null });
    expect(loaderPaths(r)).toEqual(WHOLE);
    expect(r.text("skill-copies.md")).toContain(`- the fork acme/old-skills (${read}): `);
  });

  test("a fork cached from a whole-tree read before the compare rule keeps it, says so, and its text files move apart", async () => {
    const cacheDir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
    scratch.push(cacheDir);
    const args = [...only, "--cache-dir", cacheDir];
    expect((await run(forkWorld(), args)).code).toBe(0);
    // Each repository's file as the code before the compare rule wrote it: no forkRead, no
    // changed, and a fork's text files in its blobs, not in lines of their own.
    const texts = Object.fromEntries(
      [...OLD_FILES, ...EXTRA].map((file) => [blobSha(file.text), file.text])
    );
    const dir = path.join(cacheDir, "repository");
    for (const file of readdirSync(dir)) {
      const entry = JSON.parse(readFileSync(path.join(dir, file), "utf8")) as {
        key: string;
        value: { blobs: Record<string, string>; tree: TreeEntry[] };
      };
      if (entry.key.startsWith("acme/old-skills\0")) {
        for (const kept of entry.value.tree) {
          const text = texts[kept.sha];
          if (text !== undefined && text.length <= 100_000) entry.value.blobs[kept.sha] = text;
        }
      }
      writeFileSync(
        path.join(dir, file),
        JSON.stringify(entry, (key, value) =>
          key === "forkRead" || key === "changed" || key === "forkFilesApart" ? undefined : value
        )
      );
    }
    rmSync(path.join(cacheDir, "fork-files"), { recursive: true });
    const w = forkWorld();
    const r = await run(w, args);
    expect(forkScan(r)).toMatchObject({
      read: "whole tree: read before the compare rule",
      changed_files: null,
    });
    expect(loaderPaths(r)).toEqual(WHOLE);
    // Every text file came from the cache: no compare, and no blob read alone but the binary one.
    expect(
      w.calls.filter((call) => /\/compare\/|^repos\/[^/]+\/[^/]+$/.test(call[1] ?? ""))
    ).toEqual([]);
    const binary = blobSha(OLD_FILES.find((file) => file.path === "assets/data.json")?.text ?? "");
    expect(
      w.calls.filter((call) => /\/git\/blobs\//.test(call[1] ?? "") && !call[1]?.endsWith(binary))
    ).toEqual([]);
    // The file moved its fork texts to their own lines, and keeps only the skill's text.
    const moved = cachedEntry(cacheDir, "acme/old-skills");
    expect(moved?.forkFilesApart).toBe(true);
    expect(moved?.forkRead).toBe("whole tree: read before the compare rule");
    expect(Object.values(moved?.blobs ?? {}).some((text) => text.includes("skill://beta"))).toBe(
      false
    );
    expect(readdirSync(path.join(cacheDir, "fork-files"))).toHaveLength(1);
  }, 60_000);

  test("a fork cached after the compare rule keeps its changed files, with no compare made again", async () => {
    const cacheDir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
    scratch.push(cacheDir);
    const args = [...only, "--cache-dir", cacheDir];
    const first = await run(forkWorld(), args);
    const w = forkWorld();
    const second = await run(w, args);
    expect(w.calls).toEqual([]);
    expect(forkScan(second)).toEqual(forkScan(first));
    expect(loaderPaths(second)).toEqual(["docs/changed.md", "docs/new-name.md"]);
  }, 60_000);
});

describe("a truncated tree", () => {
  /** Each directory of F6's tree, by its path, with its tree id: F6's where its tree lists the
   *  directory, else a made-up one. */
  function legionDirs(): Map<string, string> {
    const dirs = new Map<string, string>();
    for (const entry of legionTree().entries) {
      const parts = entry.path.split("/");
      for (let depth = 1; depth < parts.length; depth++) {
        const dir = parts.slice(0, depth).join("/");
        if (!dirs.has(dir)) dirs.set(dir, sha1(`tree legion ${dir}`));
      }
      if (entry.type === "tree") dirs.set(entry.path, entry.sha);
    }
    return dirs;
  }

  /** The F6 tree served the way GitHub serves a truncated one: the root's recursive read and the
   *  skills/ directory's are truncated, so both are read one level down. */
  function truncatedWorld(): World {
    const w = world();
    const { entries } = legionTree();
    const dirs = legionDirs();
    const all: TreeEntry[] = [
      ...entries.filter((entry) => entry.type === "blob"),
      ...[...dirs].map(([dir, sha]) => ({ path: dir, mode: "040000", type: "tree", sha })),
    ];
    const relative = (dir: string, deep: boolean) =>
      all
        .filter((entry) => (dir === "" ? true : entry.path.startsWith(`${dir}/`)))
        .map((entry) => ({
          ...entry,
          path: dir === "" ? entry.path : entry.path.slice(dir.length + 1),
        }))
        .filter((entry) => deep || !entry.path.includes("/"));
    w.trees.set(`cybermonklabs/legion ${LEGION_ROOT} recursive`, {
      sha: LEGION_ROOT,
      tree: relative("", true).slice(0, 10),
      truncated: true,
    });
    w.trees.set(`cybermonklabs/legion ${LEGION_ROOT}`, {
      sha: LEGION_ROOT,
      tree: relative("", false),
      truncated: false,
    });
    for (const [dir, sha] of dirs) {
      const truncated = dir === "skills";
      w.trees.set(`cybermonklabs/legion ${sha} recursive`, {
        sha,
        tree: truncated ? [] : relative(dir, true),
        truncated,
      });
      w.trees.set(`cybermonklabs/legion ${sha}`, {
        sha,
        tree: relative(dir, false),
        truncated: false,
      });
    }
    return w;
  }

  test("it is read by directory, its subtrees in GraphQL batches, into the same tree and copies", async () => {
    const whole = await run(world(), ["--only", "cybermonklabs/legion"]);
    const w = truncatedWorld();
    const byDirectory = await run(w, ["--only", "cybermonklabs/legion"]);
    const legion = byDirectory.table("repositories.tsv")[0];
    expect(legion?.tree_read).toMatch(/^by directory, \d+ reads$/);
    expect(legion?.error).toBe("");
    // One REST read of the whole (truncated) tree, then one batch per level of the walk.
    expect(w.calls.filter((call) => /\/git\/trees\//.test(call[1] ?? "")).length).toBe(1);
    expect(w.treeBatches.length).toBeGreaterThan(1);
    expect(Math.max(...w.treeBatches)).toBeGreaterThan(1);
    expect(byDirectory.text("skill-copies.tsv")).toBe(whole.text("skill-copies.tsv"));
    expect(byDirectory.text("manifests.tsv")).toBe(whole.text("manifests.tsv"));
  }, 30_000);

  test("a tree whose whole read fails is walked by directory into the same copies", async () => {
    const whole = await run(world(), ["--only", "cybermonklabs/legion"]);
    const w = truncatedWorld();
    w.trees.set(`cybermonklabs/legion ${LEGION_ROOT} recursive`, "unexpected end of JSON input");
    const walked = await run(w, ["--only", "cybermonklabs/legion"]);
    const legion = walked.table("repositories.tsv")[0];
    expect(legion?.error).toBe("");
    expect(legion?.tree_read).toMatch(/^by directory, \d+ reads$/);
    expect(walked.text("skill-copies.tsv")).toBe(whole.text("skill-copies.tsv"));
    expect(walked.text("manifests.tsv")).toBe(whole.text("manifests.tsv"));
  }, 30_000);

  test("the batched walk and the REST walk give the same kept entries and counts", async () => {
    const read = async (refuse: boolean) => {
      const w = truncatedWorld();
      w.refuseTreeBatches = refuse;
      w.treeTurns = 5;
      const cacheDir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
      scratch.push(cacheDir);
      const result = await listCopies(
        parseConfig(CONFIG),
        { at: AT, only: ["cybermonklabs/legion"], maxSearchCalls: 0, cacheDir },
        {
          gh: w.gh,
          sleep: w.sleep,
          now: w.now,
          out: () => {},
          err: () => {},
          peakRssBytes: () => 0,
        }
      );
      return {
        w,
        count: result.repositories[0]?.treeEntries,
        kept: cachedEntry(cacheDir, "cybermonklabs/legion")
          ?.tree.map((entry) => JSON.stringify(entry))
          .sort(),
      };
    };
    const batched = await read(false);
    const rest = await read(true);
    expect(batched.count).toBeGreaterThan(0);
    expect(batched.count).toBe(rest.count);
    expect(batched.kept?.length).toBeGreaterThan(0);
    expect(batched.kept).toEqual(rest.kept);
    // Refused batches are halved, then each subtree is read over REST, under the
    // per-repository cap.
    expect(rest.w.treeBatches).toEqual([]);
    expect(rest.w.maxTreeReads).toBe(READ_CALLS_PER_REPOSITORY);
    expect(READ_CALLS).toBeGreaterThan(READ_CALLS_PER_REPOSITORY);
  });

  test("6 repositories read at once beside its walk give the same output, byte for byte, as 1", async () => {
    /** The truncated world, its walk over REST, beside 4 made-up repositories whose tree reads
     *  are slow: with the first by name, 5 slow reads and the walk's 4 would be 9 in flight, so
     *  the walk and they share the run's read slots. */
    const busyWorld = () => {
      const w = truncatedWorld();
      w.refuseTreeBatches = true;
      w.treeTurns = 5;
      const small = [1, 2, 3, 4].map((n) => `cybermonklabs/small-${n}`);
      w.listings.set("cybermonklabs", [
        ...(fixtureJson("F6-repo-list.json") as unknown[]),
        ...small.map((nameWithOwner) => ({
          nameWithOwner,
          isFork: false,
          isArchived: false,
          defaultBranchRef: { name: "main" },
          parent: null,
        })),
      ]);
      for (const repository of small) {
        const files = [{ path: "README.md", text: `# ${repository}, a made-up repository\n` }];
        w.addFiles(files);
        w.addRepository(repository, treeOf(repository, files));
        w.treeTurnsOf.set(repository, 200);
      }
      // The first repository by name answers last, so the repositories end out of name order.
      w.treeTurnsOf.set("acme/old-skills", 400);
      return w;
    };
    const read = async (repositoryJobs: number) => {
      const w = busyWorld();
      const errors: string[] = [];
      const result = await listCopies(
        parseConfig(CONFIG),
        { at: AT, only: [], maxSearchCalls: Number.POSITIVE_INFINITY, repositoryJobs },
        {
          gh: w.gh,
          sleep: w.sleep,
          now: w.now,
          out: () => {},
          err: (line) => errors.push(line),
          peakRssBytes: () => 0,
        }
      );
      const out = mkdtempSync(path.join(tmpdir(), "copies-jobs-"));
      scratch.push(out);
      writeOutputs(out, result);
      return {
        w,
        ended: errors.flatMap((line) => /^copies: tree \d+\/\d+ (\S+):/.exec(line)?.[1] ?? []),
        files: Object.fromEntries(
          readdirSync(out).map((file) => [file, readFileSync(path.join(out, file), "utf8")])
        ),
      };
    };
    const one = await read(1);
    const six = await read(REPOSITORY_JOBS);
    expect(REPOSITORY_JOBS).toBe(6);
    expect(one.w.maxTreeRepositories).toBe(1);
    expect(one.ended).toEqual([...one.ended].sort());
    expect(one.ended).toHaveLength(8);
    // At once: 6 repositories in flight, the walk and the slow trees filling every read slot, and
    // the reads ending out of name order.
    expect(six.w.maxTreeRepositories).toBe(REPOSITORY_JOBS);
    expect(six.w.maxTreeReads).toBe(READ_CALLS);
    // The run's slots are shared, so the walk may hold fewer than its own cap.
    expect(six.w.maxTreeReadsOfOne).toBeLessThanOrEqual(READ_CALLS_PER_REPOSITORY);
    expect(six.ended.at(-1)).toBe("acme/old-skills");
    expect(six.files["repositories.tsv"]).toContain("cybermonklabs/small-4");
    expect(six.files).toEqual(one.files);
  }, 30_000);

  const LEGION_ONLY = ["--only", "cybermonklabs/legion", "--max-search-calls", "0"];

  /** Each subtree's level in the truncated tree's walk: the root's 0, a directory's the count of
   *  its path's parts. */
  function depthsOf(): Map<string, number> {
    const depthOf = new Map([[LEGION_ROOT, 0]]);
    for (const [dir, sha] of legionDirs()) depthOf.set(sha, dir.split("/").length);
    return depthOf;
  }

  /** A run over `cacheDir` that stops (its memory passes the limit) once its walk asks for its
   *  third level; returns the levels it saved there, each its file, key and value. */
  async function stoppedWalk(cacheDir: string) {
    const depthOf = depthsOf();
    const w = truncatedWorld();
    let stop = false;
    w.onTreeBatch = (shas) => {
      if (shas.some((sha) => depthOf.get(sha) === 2)) stop = true;
    };
    const stopped = await run(w, [...LEGION_ONLY, "--cache-dir", cacheDir], {}, () =>
      stop ? MEMORY_LIMIT_BYTES + 1 : PEAK_BYTES
    );
    expect(stopped.code).toBe(3);
    const walkDir = path.join(cacheDir, "walk");
    return readdirSync(walkDir)
      .filter((name) => name.endsWith(".json"))
      .map((name) => {
        const file = path.join(walkDir, name);
        const { key, value } = JSON.parse(readFileSync(file, "utf8")) as {
          key: string;
          value: { kept: unknown[]; subtrees: { sha: string }[]; count: number };
        };
        return { file, key, value };
      });
  }

  /** The kind of GraphQL batch a gh call reads (subtrees, blobs by id, or files by path), or
   *  undefined for a call that is none. */
  function graphqlKind(args: readonly string[]): "tree" | "blob" | "file" | undefined {
    if (args[1] !== "graphql") return undefined;
    const query = args.find((arg) => arg.startsWith("query=")) ?? "";
    if (query.includes("... on Tree")) return "tree";
    return query.includes("object(expression:") ? "file" : "blob";
  }

  test("a walk stopped after a level goes on from the next when run again, into the same output", async () => {
    const whole = await run(truncatedWorld(), LEGION_ONLY);
    const depthOf = depthsOf();
    const cacheDir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
    scratch.push(cacheDir);
    // The levels the first run finished are in the cache, each keyed by the repository, its root
    // tree, the fork flag, the keep rules' version and the level, with the next level's subtrees.
    const saved = new Map<number, { subtrees: { sha: string }[] }>();
    for (const { key, value } of await stoppedWalk(cacheDir)) {
      const [repository, root, fork, rules, level] = key.split("\0");
      expect([repository, root, fork, rules]).toEqual([
        "cybermonklabs/legion",
        LEGION_ROOT,
        "false",
        String(KEEP_RULES_VERSION),
      ]);
      saved.set(Number(level), value);
    }
    const last = Math.max(...saved.keys());
    const levels = [...saved.keys()].sort((a, b) => a - b);
    expect(levels).toEqual(Array.from({ length: last + 1 }, (_, level) => level));
    expect(last).toBeGreaterThanOrEqual(1);
    expect(last).toBeLessThan(Math.max(...depthOf.values()));

    // The second run reads no whole tree and no level the first finished: its first batch is the
    // first subtrees of the level after the last saved.
    const second = truncatedWorld();
    const asked: string[][] = [];
    second.onTreeBatch = (shas) => {
      asked.push([...shas]);
    };
    const resumed = await run(second, [...LEGION_ONLY, "--cache-dir", cacheDir]);
    expect(resumed.code).toBe(0);
    expect(second.calls.filter((call) => /\/git\/trees\//.test(call[1] ?? ""))).toEqual([]);
    const next = saved.get(last)?.subtrees.map((subtree) => subtree.sha) ?? [];
    expect(next.length).toBeGreaterThan(0);
    expect(asked[0]).toEqual(next.slice(0, 50));
    expect(asked.flat().every((sha) => (depthOf.get(sha) ?? -1) > last)).toBe(true);
    // Its output, the walk's "by directory, N reads" with it, is that of a walk never stopped.
    expect(resumed.table("repositories.tsv")[0]?.tree_read).toMatch(/^by directory, \d+ reads$/);
    expect(outputs(resumed)).toEqual(outputs(whole));
  }, 60_000);

  // Catches: a repository, or a level of its walk, that a run under other keep rules saved, read
  // back as this run's: its kept entries are the other rules'.
  test("a repository and its walk's levels saved under other keep rules are read again", async () => {
    const legion = "cybermonklabs/legion";
    const whole = await run(truncatedWorld(), LEGION_ONLY);
    const cacheDir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
    scratch.push(cacheDir);
    expect((await run(truncatedWorld(), [...LEGION_ONLY, "--cache-dir", cacheDir])).code).toBe(0);
    const rules = KEEP_RULES_VERSION + 1;
    /** Moves the cache file of `kind` and `key` to the file a run under the other rules keeps
     *  under `other`, its value as `emptied` leaves it. */
    const moveTo = (
      kind: string,
      key: string,
      other: string,
      emptied: (value: Record<string, unknown>) => unknown
    ) => {
      const file = path.join(cacheDir, kind, `${sha1(key)}.json`);
      const saved = JSON.parse(readFileSync(file, "utf8")) as {
        key: string;
        value: Record<string, unknown>;
      };
      expect(saved.key).toBe(key);
      rmSync(file);
      writeFileSync(
        path.join(cacheDir, kind, `${sha1(other)}.json`),
        JSON.stringify({ key: other, value: emptied(saved.value) })
      );
    };
    // As a run under other keep rules saved them: the repository and each level of its walk under
    // the key that run gives it, holding no entry, so a replay of one would change the output.
    moveTo(
      "repository",
      repositoryKey(legion, "main", AT),
      repositoryKey(legion, "main", AT, rules),
      (value) => ({ ...value, tree: [] })
    );
    const levels = readdirSync(path.join(cacheDir, "walk")).filter((name) =>
      name.endsWith(".json")
    ).length;
    expect(levels).toBeGreaterThanOrEqual(2);
    for (let level = 0; level < levels; level++) {
      moveTo(
        "walk",
        walkLevelKey(legion, LEGION_ROOT, false, level),
        walkLevelKey(legion, LEGION_ROOT, false, level, rules),
        (value) => ({ ...value, kept: [], count: 0 })
      );
    }
    const w = truncatedWorld();
    const asked: string[][] = [];
    w.onTreeBatch = (shas) => {
      asked.push([...shas]);
    };
    const again = await run(w, [...LEGION_ONLY, "--cache-dir", cacheDir]);
    expect(again.code).toBe(0);
    // It reads the whole tree, which is truncated, then walks it from its root.
    expect(w.calls.filter((call) => /\/git\/trees\//.test(call[1] ?? ""))).toHaveLength(1);
    expect(asked[0]).toEqual([LEGION_ROOT]);
    expect(outputs(again)).toEqual(outputs(whole));
  }, 60_000);

  /** Each read of a rate limit's reset `w` took: `gh api rate_limit`, or a read of an
   *  installation's (`gh api -i`, but a code search's), its first five arguments. */
  const resetReads = (w: World) =>
    w.calls
      .filter(
        (call) => call[1] === "rate_limit" || (call[1] === "-i" && !call.includes("search/code"))
      )
      .map((call) => call.slice(0, 5));
  const iso = (ms: number) => new Date(ms).toISOString();

  // Catches: a refused `gh repo list`, a GraphQL call, waited out to the REST limit's reset; and
  // to the reset `gh api rate_limit` gives, which names no owner, so the pod's gh answers it from
  // another installation than the listed owner's.
  test("an owner's listing refused for the GraphQL rate limit waits for the reset of that owner's installation's GraphQL limit", async () => {
    const w = world();
    let left = 1;
    w.refuse = (args) =>
      args[0] === "repo" && args[1] === "list" && left-- > 0
        ? "GraphQL: API rate limit exceeded for user ID 1001."
        : undefined;
    const r = await run(w, LEGION_ONLY);
    expect(r.code).toBe(0);
    // The reset is read with a GraphQL query routed by the listed owner (300 s away), not with
    // `gh api rate_limit` (45 s away).
    expect(resetReads(w)).toEqual([["api", "-i", "graphql", "-f", "owner=cybermonklabs"]]);
    expect(w.sleeps).toEqual([301_000]);
    expect(r.errors).toContain(
      `copies: cybermonklabs's GraphQL rate limit resets at ${iso(CLOCK_START + 300_000)}; waiting 301 s, until ${iso(CLOCK_START + 301_000)}`
    );
    expect(r.table("repositories.tsv").map((row) => row.repository)).toEqual([
      "cybermonklabs/legion",
    ]);
  }, 60_000);

  // Catches: a REST read waited out to the reset `gh api rate_limit` gives, which is another
  // installation's: where it comes sooner, each try is refused again.
  test("a REST read refused for the primary rate limit waits for the reset of its repository's installation, read from gh api -i, not the sooner one gh api rate_limit gives", async () => {
    const calm = await run(world(), LEGION_ONLY);
    const w = world();
    let left = 1;
    w.refuse = (args) =>
      /\/commits\?/.test(args[1] ?? "") && left-- > 0
        ? "gh: API rate limit exceeded for installation ID 1001. (HTTP 403)"
        : undefined;
    const r = await run(w, LEGION_ONLY);
    expect(r.code).toBe(0);
    expect(resetReads(w)).toEqual([["api", "-i", "--method", "GET", "repos/cybermonklabs/legion"]]);
    expect(w.sleeps).toEqual([301_000]);
    expect(r.errors).toContain(
      `copies: cybermonklabs's REST rate limit resets at ${iso(CLOCK_START + 300_000)}; waiting 301 s, until ${iso(CLOCK_START + 301_000)}`
    );
    expect(outputs(r)).toEqual(outputs(calm));
  }, 60_000);

  test("a batch refused for the GraphQL rate limit is sent again after the reset, not halved", async () => {
    const f6 = fixtureJson("F6-search-code.json");
    const read = async (limited: boolean) => {
      const w = truncatedWorld();
      // F6's hits, read by path (`filesAt`) at the commit read.
      w.search = (q) =>
        q === '"legion-worker" repo:cybermonklabs/legion' ? { body: f6 } : { body: EMPTY_SEARCH };
      // The first batch of subtrees, of blobs and of files is refused for the GraphQL limit.
      const left = new Set(limited ? ["tree", "blob", "file"] : []);
      w.refuse = (args) => {
        const kind = graphqlKind(args);
        if (kind === undefined || !left.delete(kind)) return undefined;
        return "GraphQL: API rate limit exceeded for user ID 1001.";
      };
      return { w, r: await run(w, ["--only", "cybermonklabs/legion"]) };
    };
    const calm = await read(false);
    const limited = await read(true);
    expect(limited.r.code).toBe(0);
    // Each was sent again whole: the next call of its kind is the same call.
    for (const kind of ["tree", "blob", "file"]) {
      const [refused, again] = limited.w.calls.filter((call) => graphqlKind(call) === kind);
      expect(refused).toBeDefined();
      expect(again).toEqual(refused);
    }
    // Each waited for the reset of the GraphQL limit of the installation its owner field names.
    expect(
      limited.r.errors.filter((line) =>
        /^copies: cybermonklabs's GraphQL rate limit resets at \S+; waiting 301 s, until \S+$/.test(
          line
        )
      )
    ).toHaveLength(3);
    expect(resetReads(limited.w)).toEqual(
      Array.from({ length: 3 }, () => ["api", "-i", "graphql", "-f", "owner=cybermonklabs"])
    );
    // None was halved or read alone: the batches answered, and the REST reads, are those of a run
    // GitHub refused nothing.
    expect(limited.r.errors.filter((line) => line.includes("each is read alone"))).toEqual([]);
    expect([...limited.w.treeBatches].sort()).toEqual([...calm.w.treeBatches].sort());
    expect(limited.w.batches).toEqual(calm.w.batches);
    const rest = (w: World) => w.calls.filter((call) => /^repos\/.*\/git\//.test(call[1] ?? ""));
    expect(rest(limited.w)).toEqual(rest(calm.w));
    for (const name of ["skill-copies.tsv", "manifests.tsv", "loaders.tsv", "queries.jsonl"]) {
      expect(limited.r.text(name)).toBe(calm.r.text(name));
    }
    // The call refused is a gh call of the walk: it counts one read more.
    const reads = (r: Run) => {
      const treeRead = r.table("repositories.tsv")[0]?.tree_read ?? "";
      return Number(/^by directory, (\d+) reads$/.exec(treeRead)?.[1]);
    };
    expect(reads(calm.r)).toBeGreaterThan(0);
    expect(reads(limited.r)).toBe(reads(calm.r) + 1);
  }, 60_000);

  test("a call refused for the secondary rate limit is sent again after a minute, then two, not at the reset", async () => {
    const secondary = "gh: You have exceeded a secondary rate limit. (HTTP 403)";
    const calm = await run(world(), LEGION_ONLY);
    const w = world();
    // Each limit's reset is an hour away. The commit read (a REST call) is refused once for the
    // secondary limit, and the first batch of blobs (a GraphQL call) twice.
    w.rateLimitResetIn = 3_600;
    let commitRefusals = 1;
    let blobRefusals = 2;
    w.refuse = (args) => {
      if (/\/commits\?/.test(args[1] ?? "") && commitRefusals > 0) {
        commitRefusals--;
        return secondary;
      }
      if (graphqlKind(args) === "blob" && blobRefusals > 0) {
        blobRefusals--;
        return secondary;
      }
      return undefined;
    };
    const r = await run(w, LEGION_ONLY);
    expect(r.code).toBe(0);
    // A minute for each first refusal, two for the second: no wait for the reset, which is not
    // even asked for.
    expect(w.sleeps).toEqual([60_000, 60_000, 120_000]);
    expect(
      r.errors.flatMap(
        (line) =>
          /^copies: a gh call was refused for its rate limit \(.*secondary rate limit.*\); waiting (\d+) s, until /
            .exec(line)
            ?.slice(1, 2) ?? []
      )
    ).toEqual(["60", "60", "120"]);
    expect(resetReads(w)).toEqual([]);
    // The same calls were sent again, and the output is that of a run GitHub refused nothing.
    const commits = w.calls.filter((call) => /\/commits\?/.test(call[1] ?? ""));
    expect(commits).toEqual([commits[0], commits[0]]);
    const blobs = w.calls.filter((call) => graphqlKind(call) === "blob");
    expect(blobs.slice(0, 3)).toEqual([blobs[0], blobs[0], blobs[0]]);
    expect(outputs(r)).toEqual(outputs(calm));
  }, 60_000);

  describe("a read that fails for a transient reason", () => {
    /** The truncated world walked over REST, one directory a call (GitHub refuses each GraphQL
     *  batch), as the run that lost a repository to one TLS handshake timeout walked its tree. */
    const restWorld = () => {
      const w = truncatedWorld();
      w.refuseTreeBatches = true;
      return w;
    };
    /** The walk's REST read of the skills/ directory, one level. */
    const SKILLS_READ = `repos/cybermonklabs/legion/git/trees/${legionDirs().get("skills")}`;
    const SKILLS_URL = `https://api.github.com/${SKILLS_READ}`;
    /** `w`, whose read of skills/ fails with each of `lines` in turn, then is answered. */
    const failing = (w: World, lines: readonly string[]) => {
      const left = [...lines];
      w.refuse = (args) => (args[1] === SKILLS_READ ? left.shift() : undefined);
      return w;
    };
    const skillsReads = (w: World) => w.calls.filter((call) => call[1] === SKILLS_READ).length;
    const SAME = ["skill-copies.tsv", "manifests.tsv"];
    /** The gh calls of the walk repositories.tsv gives (`by directory, N reads`), and its rows
     *  less that column. */
    const walked = (r: Run) => {
      const rows = r.table("repositories.tsv");
      const treeRead = /^by directory, (\d+) reads$/.exec(rows[0]?.tree_read ?? "");
      return {
        reads: Number(treeRead?.[1]),
        rest: rows.map(({ tree_read: _treeRead, ...row }) => row),
      };
    };
    /** Checks that `r`'s repositories are `calm`'s, its walk counting `retries` reads more: each
     *  try of a read sent again is a gh call, over REST as over GraphQL. */
    const sameRepositories = (r: Run, calm: Run, retries: number) => {
      expect(walked(calm).reads).toBeGreaterThan(0);
      expect(walked(r).reads).toBe(walked(calm).reads + retries);
      expect(walked(r).rest).toEqual(walked(calm).rest);
    };

    test("a tree read that fails on a TLS handshake timeout is sent again 5 s later, and its repository keeps its entries", async () => {
      const calm = await run(restWorld(), LEGION_ONLY);
      const timeout = `Get "${SKILLS_URL}": net/http: TLS handshake timeout`;
      const w = failing(restWorld(), [timeout]);
      const r = await run(w, LEGION_ONLY);
      expect(r.code).toBe(0);
      expect(r.table("repositories.tsv")[0]).toMatchObject({ error: "" });
      for (const name of SAME) expect(r.text(name)).toBe(calm.text(name));
      sameRepositories(r, calm, 1);
      expect(w.sleeps).toEqual([5_000]);
      expect(skillsReads(w)).toBe(2);
      expect(r.errors).toContain(
        `copies: a gh call failed (${timeout}); sending it again in 5 s, retry 1 of 4`
      );
      expect(r.text("skill-copies.md")).toContain(
        "1 retry after a transient failure (TLS handshake timeout 1)."
      );
      expect(r.stdout).toContain(
        "code search calls, 1 retry after a transient failure (TLS handshake timeout 1), peak memory"
      );
    }, 30_000);

    test("a tree read GitHub answers 503 twice is sent again 5 s, then 15 s later, and its repository keeps its entries", async () => {
      const calm = await run(restWorld(), LEGION_ONLY);
      const unavailable = `HTTP 503: Service Unavailable (${SKILLS_URL})`;
      const w = failing(restWorld(), [unavailable, unavailable]);
      const r = await run(w, LEGION_ONLY);
      expect(r.code).toBe(0);
      expect(r.table("repositories.tsv")[0]).toMatchObject({ error: "" });
      for (const name of SAME) expect(r.text(name)).toBe(calm.text(name));
      sameRepositories(r, calm, 2);
      expect(w.sleeps).toEqual([5_000, 15_000]);
      expect(skillsReads(w)).toBe(3);
      expect(r.text("skill-copies.md")).toContain(
        "2 retries after a transient failure (HTTP 503 2)."
      );
    }, 30_000);

    test("a tree read GitHub answers 500 is sent again 5 s later, and its repository keeps its entries", async () => {
      const calm = await run(restWorld(), LEGION_ONLY);
      const serverError = `HTTP 500: Internal Server Error (${SKILLS_URL})`;
      const w = failing(restWorld(), [serverError]);
      const r = await run(w, LEGION_ONLY);
      expect(r.code).toBe(0);
      expect(r.table("repositories.tsv")[0]).toMatchObject({ error: "" });
      for (const name of SAME) expect(r.text(name)).toBe(calm.text(name));
      sameRepositories(r, calm, 1);
      expect(w.sleeps).toEqual([5_000]);
      expect(skillsReads(w)).toBe(2);
      expect(r.text("skill-copies.md")).toContain(
        "1 retry after a transient failure (other HTTP 5xx 1)."
      );
    }, 30_000);

    test("a GraphQL batch of more than one item GitHub refuses as a timeout is split at once; a batch of one refused so is sent again", async () => {
      const timedOut =
        "gh: Something went wrong while executing your query. This may be the result of a timeout, or it could be a GitHub bug.";
      /** How many subtrees a GraphQL tree batch asks for. */
      const subtrees = (args: readonly string[]) =>
        (args.find((arg) => arg.startsWith("query=")) ?? "").match(/t\d+: object\(/g)?.length ?? 0;
      const read = async (flaky: boolean) => {
        const w = truncatedWorld();
        // The walk's first batch, its root alone, and its first batch of more than one subtree
        // are each refused once.
        const left = new Set(flaky ? ["root", "wide"] : []);
        w.refuse = (args) => {
          if (graphqlKind(args) !== "tree") return undefined;
          if (subtrees(args) === 1 && left.delete("root")) return timedOut;
          if (subtrees(args) > 1 && left.delete("wide")) return timedOut;
          return undefined;
        };
        return { w, r: await run(w, LEGION_ONLY) };
      };
      const calm = await read(false);
      const flaky = await read(true);
      expect(flaky.r.code).toBe(0);
      expect(flaky.r.table("repositories.tsv")[0]).toMatchObject({ error: "" });
      for (const name of ["skill-copies.tsv", "manifests.tsv"]) {
        expect(flaky.r.text(name)).toBe(calm.r.text(name));
      }
      // The root alone was sent again 5 s after it failed; the wider batch was halved at once.
      expect(flaky.w.sleeps).toEqual([5_000]);
      expect(
        flaky.r.errors.filter((line) => line.startsWith("copies: a gh call failed ("))
      ).toEqual([`copies: a gh call failed (${timedOut}); sending it again in 5 s, retry 1 of 4`]);
      expect(flaky.r.text("skill-copies.md")).toContain(
        "1 retry after a transient failure (timeout 1)."
      );
    }, 30_000);

    test("a tree read that fails five times fails its repository, naming each try's error", async () => {
      const lines = [
        `Get "${SKILLS_URL}": net/http: TLS handshake timeout`,
        `Get "${SKILLS_URL}": read: connection reset by peer`,
        `Get "${SKILLS_URL}": dial tcp: lookup api.github.com: i/o timeout`,
        `Get "${SKILLS_URL}": unexpected EOF`,
        `HTTP 504: Gateway Timeout (${SKILLS_URL})`,
      ];
      const w = failing(restWorld(), lines);
      const r = await run(w, LEGION_ONLY);
      expect(r.code).toBe(0);
      const each = lines.map((line, index) => `try ${index + 1}: ${line}`).join("; ");
      expect(r.table("repositories.tsv")[0]).toMatchObject({
        error: `tree read failed: 5 tries: ${each}`,
      });
      expect(r.text("skill-copies.md")).toContain(
        `- cybermonklabs/legion: tree read failed: 5 tries: ${each}`
      );
      expect(w.sleeps).toEqual([5_000, 15_000, 45_000, 135_000]);
      expect(skillsReads(w)).toBe(5);
      expect(r.text("skill-copies.md")).toContain(
        "4 retries after a transient failure (TLS handshake timeout 1, connection reset 1, unexpected EOF 1, i/o timeout 1)."
      );
    }, 30_000);

    test.each([
      ["a 404", "gh: Not Found (HTTP 404)"],
      ["a 401", "gh: Bad credentials (HTTP 401)"],
      ["a 403 that is no rate limit", "gh: Resource not accessible by integration (HTTP 403)"],
    ])(
      "a tree read refused with %s fails at once, with no retry",
      async (_, line) => {
        const w = failing(restWorld(), [line]);
        const r = await run(w, LEGION_ONLY);
        expect(r.code).toBe(0);
        expect(r.table("repositories.tsv")[0]).toMatchObject({
          error: `tree read failed: ${line}`,
        });
        expect(w.sleeps).toEqual([]);
        expect(skillsReads(w)).toBe(1);
        expect(r.text("skill-copies.md")).toContain("0 retries after a transient failure.");
      },
      30_000
    );

    test("a GraphQL batch of more than one item GitHub answers 502 is split at once; a batch of one, or one whose TLS handshake timed out, is sent again", async () => {
      const badGateway = "HTTP 502: Bad Gateway (https://api.github.com/graphql)";
      const timeout = 'Post "https://api.github.com/graphql": net/http: TLS handshake timeout';
      /** How many subtrees a GraphQL tree batch asks for. */
      const subtrees = (args: readonly string[]) =>
        (args.find((arg) => arg.startsWith("query=")) ?? "").match(/t\d+: object\(/g)?.length ?? 0;
      const read = async (flaky: boolean) => {
        const w = truncatedWorld();
        // The walk's first batch, its root alone, gets a 502; the next, the root's 4 directories,
        // a 502; a half of that, a TLS handshake timeout; the first batch of blobs, a 502.
        const left = new Set(flaky ? ["root", "wide", "half", "blobs"] : []);
        w.refuse = (args) => {
          const kind = graphqlKind(args);
          if (kind === "tree" && subtrees(args) === 1 && left.delete("root")) return badGateway;
          if (kind === "tree" && subtrees(args) > 2 && left.delete("wide")) return badGateway;
          if (kind === "tree" && subtrees(args) === 2 && left.delete("half")) return timeout;
          if (kind === "blob" && left.delete("blobs")) return badGateway;
          return undefined;
        };
        return { w, r: await run(w, LEGION_ONLY) };
      };
      const calm = await read(false);
      const flaky = await read(true);
      expect(flaky.r.code).toBe(0);
      expect(flaky.r.table("repositories.tsv")[0]).toMatchObject({ error: "" });
      for (const name of ["skill-copies.tsv", "manifests.tsv"]) {
        expect(flaky.r.text(name)).toBe(calm.r.text(name));
      }
      /** How many times `w` sent the first call `which` picks. */
      const sent = (w: World, which: (args: readonly string[]) => boolean) => {
        const first = w.calls.find(which)?.join("\0");
        return w.calls.filter((call) => call.join("\0") === first).length;
      };
      const tree = (count: (n: number) => boolean) => (args: readonly string[]) =>
        graphqlKind(args) === "tree" && count(subtrees(args));
      // The root alone and the half were sent again, 5 s after each failed; the batch of 4 was
      // halved at once, and the batch of blobs read blob by blob at once, with no wait.
      expect(
        sent(
          flaky.w,
          tree((n) => n === 1)
        )
      ).toBe(2);
      expect(
        sent(
          flaky.w,
          tree((n) => n === 2)
        )
      ).toBe(2);
      expect(
        sent(
          flaky.w,
          tree((n) => n > 2)
        )
      ).toBe(1);
      expect(sent(flaky.w, (args) => graphqlKind(args) === "blob")).toBe(1);
      expect(flaky.w.sleeps).toEqual([5_000, 5_000]);
      expect(
        flaky.r.errors.filter((line) => line.startsWith("copies: a gh call failed ("))
      ).toEqual([
        `copies: a gh call failed (${badGateway}); sending it again in 5 s, retry 1 of 4`,
        `copies: a gh call failed (${timeout}); sending it again in 5 s, retry 1 of 4`,
      ]);
      const readAlone = ` blobs of cybermonklabs/legion failed (${badGateway}); each is read alone`;
      const [blobsAlone = ""] = flaky.r.errors.filter((line) => line.endsWith(readAlone));
      const blobs = /^copies: a batch of (\d+)$/.exec(blobsAlone.slice(0, -readAlone.length));
      expect(Number(blobs?.[1])).toBeGreaterThan(1);
      expect(flaky.r.text("skill-copies.md")).toContain(
        "2 retries after a transient failure (TLS handshake timeout 1, HTTP 502 1)."
      );
    }, 30_000);

    test("a code search that fails on a refused connection is sent again, not failed", async () => {
      const f6 = fixtureJson("F6-search-code.json");
      const refused =
        'Get "https://api.github.com/search/code": dial tcp: connect: connection refused';
      const read = async (flaky: boolean) => {
        const w = world();
        w.search = (q) =>
          q === '"legion-worker" repo:cybermonklabs/legion' ? { body: f6 } : { body: EMPTY_SEARCH };
        let left = flaky ? 1 : 0;
        w.refuse = (args) => (args.includes("search/code") && left-- > 0 ? refused : undefined);
        return { w, r: await run(w, ["--only", "cybermonklabs/legion"]) };
      };
      const calm = await read(false);
      const flaky = await read(true);
      expect(flaky.r.code).toBe(0);
      expect(
        flaky.r.queries().filter((query) => query.type === "search" && query.status !== "ok")
      ).toEqual([]);
      expect(flaky.r.text("loaders.tsv")).toBe(calm.r.text("loaders.tsv"));
      expect(
        flaky.r.errors.filter((line) => line.startsWith("copies: a gh call failed ("))
      ).toEqual([`copies: a gh call failed (${refused}); sending it again in 5 s, retry 1 of 4`]);
      expect(flaky.r.text("skill-copies.md")).toContain(
        "1 retry after a transient failure (connection refused 1)."
      );
    }, 30_000);

    test("a call waiting to be sent again holds no read slot: another repository's read takes it", async () => {
      // Five small repositories beside the walk: their tree reads and the walk's want more than
      // the run's read slots.
      const w = restWorld();
      const small = [1, 2, 3, 4, 5].map((n) => `cybermonklabs/small-${n}`);
      w.listings.set("cybermonklabs", [
        ...(fixtureJson("F6-repo-list.json") as unknown[]),
        ...small.map((nameWithOwner) => ({
          nameWithOwner,
          isFork: false,
          isArchived: false,
          defaultBranchRef: { name: "main" },
          parent: null,
        })),
      ]);
      for (const repository of small) {
        const files = [{ path: "README.md", text: `# ${repository}, a made-up repository\n` }];
        w.addFiles(files);
        w.addRepository(repository, treeOf(repository, files));
      }
      const [flaky = ""] = small;
      const flakyKey = `${flaky} ${sha1(`root ${flaky}`)} recursive`;
      const answer = w.trees.get(flakyKey);
      /** Microtask turns, no timer, until `done` holds or `most` turns pass: every gh call here is
       *  answered in microtasks, so the reads let start reach their gates within them. */
      const turns = async (done: () => boolean, most = 10_000) => {
        for (let turn = 0; turn < most && !done(); turn++) await Promise.resolve();
      };
      const slotsFull = () => w.treeReadsInFlight() >= READ_CALLS;
      /** The reads in flight while the flaky read waits, and the repositories whose tree reads
       *  started since it failed. */
      let during: { inFlight: number; started: string[] } | undefined;
      // Every tree read but the flaky repository's and the walk's of its root (whole, then one
      // level) waits for `open`, so the walk's next level and the small repositories hold every
      // slot, one read left waiting.
      const open = Promise.withResolvers<void>();
      let failedAt = -1;
      let letGo: Promise<void> | undefined;
      w.treeGate = async (repository, read) => {
        if (repository === flaky) {
          if (failedAt >= 0) {
            w.trees.set(flakyKey, answer);
            return;
          }
          // Its first try ends once every read slot is held: its TLS handshake timed out.
          await turns(slotsFull);
          failedAt = w.calls.length;
          w.trees.set(
            flakyKey,
            `Get "https://api.github.com/repos/${flaky}/git/trees/${sha1(`root ${flaky}`)}?recursive=1": net/http: TLS handshake timeout`
          );
          // A run that does not send it again never waits: its reads go once no wait came in
          // 100,000 turns.
          letGo = turns(() => during !== undefined, 100_000).then(() => open.resolve());
          return;
        }
        if (!read.startsWith(`cybermonklabs/legion ${LEGION_ROOT}`)) await open.promise;
      };
      const result = await listCopies(
        parseConfig(CONFIG),
        { at: AT, only: ["cybermonklabs/legion", ...small], maxSearchCalls: 0 },
        {
          gh: w.gh,
          // The wait: the read its slot let start reaches its gate, and the reads are counted.
          sleep: async (ms) => {
            await turns(slotsFull);
            during = {
              inFlight: w.treeReadsInFlight(),
              started: w.calls
                .slice(failedAt)
                .flatMap(
                  (call) => /^repos\/([^/]+\/[^/]+)\/git\/trees\//.exec(call[1] ?? "")?.[1] ?? []
                ),
            };
            open.resolve();
            await w.sleep(ms);
          },
          now: w.now,
          out: () => {},
          err: () => {},
          peakRssBytes: () => 0,
        }
      );
      await letGo;
      // While it waited, the other reads held every slot, and another repository's read started
      // in the wait: the flaky read itself is sent again only once the wait ends.
      expect(during?.inFlight).toBe(READ_CALLS);
      expect(during?.started.length).toBeGreaterThan(0);
      expect(w.sleeps).toEqual([5_000]);
      expect(result.repositories.map((repo) => [repo.repository, repo.error])).toEqual(
        ["cybermonklabs/legion", ...small].map((repository) => [repository, ""])
      );
      expect(result.retries.get("TLS handshake timeout")).toBe(1);
    }, 30_000);
  });
});

describe("the read slots", () => {
  test("a freed slot goes to the repository holding the fewest, so a small repository passes two large walks", async () => {
    const slots = new ReadSlots(READ_CALLS, READ_CALLS_PER_REPOSITORY);
    const started: string[] = [];
    const open: { repository: string; end: () => void }[] = [];
    const call = (repository: string) =>
      slots.run(repository, () => {
        const gate = Promise.withResolvers<void>();
        started.push(repository);
        open.push({ repository, end: gate.resolve });
        return gate.promise;
      });
    const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
    const end = async (repository: string) => {
      const index = open.findIndex((read) => read.repository === repository);
      open.splice(index, 1)[0]?.end();
      await settle();
    };
    // Two large walks queue a level of ten subtree reads each: four of each start, the rest wait.
    const all = [
      ...Array.from({ length: 10 }, () => call("acme/walk-a")),
      ...Array.from({ length: 10 }, () => call("acme/walk-b")),
    ];
    await settle();
    expect(started).toEqual([...Array(4).fill("acme/walk-a"), ...Array(4).fill("acme/walk-b")]);
    // A small repository's read waits behind them, then takes the first slot a walk frees.
    all.push(call("acme/small"));
    await end("acme/walk-a");
    expect(started.at(-1)).toBe("acme/small");
    // Between the walks, both holding three once walk-b's read ends, the slot goes to the read
    // that waited first: walk-a's.
    await end("acme/walk-b");
    expect(started.at(-1)).toBe("acme/walk-a");
    while (open.length > 0) await end(open[0]?.repository ?? "");
    await Promise.all(all);
    expect(started).toHaveLength(21);
  });
});

describe("a run started again over --cache-dir", () => {
  const only = [
    "--only",
    "cybermonklabs/legion",
    "--only",
    "acme/skills",
    "--only",
    "acme/old-skills",
  ];
  /** The world with no read that fails, so a first run caches every read. */
  const clean = () => {
    const w = world();
    w.failingBlobs.clear();
    return w;
  };

  // Catches: a summary that counts the calls an earlier run made for a cached search as this
  // run's, beside retries that count this run's alone.
  test("a second run makes no gh call and writes the same output, its summary counting no search call and each search it took from the cache", async () => {
    const cacheDir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
    scratch.push(cacheDir);
    const first = await run(clean(), [...only, "--cache-dir", cacheDir]);
    expect(first.code).toBe(0);
    const w = clean();
    const second = await run(w, [...only, "--cache-dir", cacheDir]);
    expect(second.code).toBe(0);
    expect(w.calls).toEqual([]);
    const searches = first.queries().filter((query) => query.type === "search");
    const calls = searches.reduce((sum, query) => sum + Number(query.calls), 0);
    expect(searches.length).toBeGreaterThan(1);
    expect(calls).toBeGreaterThan(0);
    /** The summary's sentence of `made` search calls and `cached` searches. */
    const said = (made: number, cached: number) =>
      `${made} code search calls (budget none); ${cached} searches taken from the cache.`;
    expect(first.text("skill-copies.md")).toContain(said(calls, 0));
    expect(second.text("skill-copies.md")).toContain(said(0, searches.length));
    expect(second.stdout).toContain(
      `${searches.length} searches taken from the cache, 0 code search calls, 0 retries`
    );
    // The second run's files with its summary sentence written as the first run's are the
    // first run's files.
    const again: Record<string, string> = outputs(second);
    again["skill-copies.md"] = (again["skill-copies.md"] ?? "").replace(
      said(0, searches.length),
      said(calls, 0)
    );
    expect(again).toEqual(outputs(first));
    // The fork's tree: its 4 files and the 4 directories that hold them.
    expect(first.errors).toContainEqual(
      expect.stringMatching(
        /^copies: tree \d\/3 acme\/old-skills: 8 entries, 1 tree reads, 0 s, read$/
      )
    );
    expect(second.errors).toContainEqual(
      expect.stringMatching(
        /^copies: tree \d\/3 acme\/old-skills: 8 entries, 0 tree reads, 0 s, cached$/
      )
    );
  }, 60_000);

  // Catches: a cache read that takes the temporary file a killed write left where no final file
  // is, or a final file cut short.
  test("a cache file a killed write left is never read: a temporary file with no final file, or a final file cut short", async () => {
    const cacheDir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
    scratch.push(cacheDir);
    const first = await run(clean(), [...only, "--cache-dir", cacheDir]);
    // Half the repositories keep only the temporary file a kill between the write and the rename
    // leaves, here with a commit no run read; the others keep a final file cut short, which is
    // not of the cache's shape.
    const repositories = path.join(cacheDir, "repository");
    const files = readdirSync(repositories).sort();
    expect(files.length).toBeGreaterThanOrEqual(2);
    for (const [index, file] of files.entries()) {
      const final = path.join(repositories, file);
      const text = readFileSync(final, "utf8");
      if (index % 2 === 0) {
        writeFileSync(`${final}.tmp-1-0`, text.replace(/"commit":"[0-9a-f]+"/, '"commit":"0000"'));
        rmSync(final);
      } else {
        writeFileSync(final, text.slice(0, Math.floor(text.length / 2)));
      }
    }
    const w = clean();
    const second = await run(w, [...only, "--cache-dir", cacheDir]);
    expect(second.code).toBe(0);
    expect(w.calls.filter((call) => /\/commits\?/.test(call[1] ?? "")).length).toBe(3);
    expect(second.text("repositories.tsv")).toBe(first.text("repositories.tsv"));
    expect(second.text("repositories.tsv")).not.toContain("0000");
  }, 60_000);
});

describe("the memory a run holds", () => {
  /** The world with twelve made-up forks of acme/skills, acme/fork-00 to acme/fork-11 (first by
   *  name), each holding a skill and PAGES text pages of about PAGE_BYTES bytes that name it, and
   *  a parent the run cannot read, so each is read from its whole tree. */
  const FORKS = Array.from({ length: 12 }, (_, n) => `acme/fork-${String(n).padStart(2, "0")}`);
  const PAGES = 20;
  const PAGE_BYTES = "text of a made-up page ".length * 4_000;
  const forksWorld = () => {
    const w = world();
    w.listings.set("acme", [
      ...(w.listings.get("acme") as unknown[]),
      ...FORKS.map((nameWithOwner) => ({
        nameWithOwner,
        isFork: true,
        isArchived: false,
        defaultBranchRef: { name: "main" },
        parent: { id: "R_madeup", name: "skills", owner: { id: "U_madeup", login: "acme" } },
      })),
    ]);
    for (const fork of FORKS) {
      const files: MadeFile[] = [
        {
          path: "skills/alpha/SKILL.md",
          text: skill(`name: alpha\ndescription: A copy in ${fork}.`),
        },
        ...Array.from({ length: PAGES }, (_, n) => ({
          path: `docs/page-${n}.md`,
          text: `# ${fork} page ${n}\n\nLoad skill://alpha.\n\n${"text of a made-up page ".repeat(4_000)}\n`,
        })),
      ];
      w.addFiles(files);
      w.addRepository(fork, treeOf(fork, files));
    }
    return w;
  };
  /** Every string `value` holds, however deep. */
  const stringsIn = (value: unknown, seen = new Set<object>()): string[] => {
    if (typeof value === "string") return [value];
    if (value === null || typeof value !== "object" || seen.has(value)) return [];
    seen.add(value);
    const values =
      value instanceof Map ? [...value.keys(), ...value.values()] : Object.values(value);
    return values.flatMap((item) => stringsIn(item, seen));
  };

  test("a tree keeps only the entries the list uses, on disk once its step is done", async () => {
    const w = world();
    const cacheDir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
    scratch.push(cacheDir);
    const result = await listCopies(
      parseConfig(CONFIG),
      { at: AT, only: [], maxSearchCalls: 0, cacheDir },
      { gh: w.gh, sleep: w.sleep, now: w.now, out: () => {}, err: () => {}, peakRssBytes: () => 0 }
    );
    // The run keeps no tree past its repository's step.
    expect(result.repositories.every((repo) => repo.tree.length === 0)).toBe(true);
    const wholeTrees: [string, TreeEntry[]][] = [
      ["cybermonklabs/legion", legionTree().entries],
      ["acme/skills", treeOf("acme/skills", [...madeUpFiles(), ...ACME_FILES])],
      ["acme/old-skills", treeOf("acme/old-skills", OLD_FILES)],
    ];
    for (const [repository, entries] of wholeTrees) {
      const repo = result.repositories.find((candidate) => candidate.repository === repository);
      expect(repo?.treeEntries).toBe(entries.length);
      const kept = entries.filter((entry) => keepsEntry(entry, repo?.fork ?? false));
      expect(
        cachedEntry(cacheDir, repository)
          ?.tree.map((entry) => entry.path)
          .sort()
      ).toEqual(kept.map((entry) => entry.path).sort());
      expect(kept.length).toBeLessThan(entries.length);
    }
    const tree = (repository: string) =>
      cachedEntry(cacheDir, repository)
        ?.tree.map((entry) => entry.path)
        .sort();
    // No directory, no reference file of a skill, no plain file of a repository that is no fork.
    expect(tree("cybermonklabs/legion")?.some((file) => file.includes("/references/"))).toBe(false);
    expect(tree("acme/skills")).not.toContain("docs/guide.md");
    // A fork keeps the text files its scan reads.
    expect(tree("acme/old-skills")).toEqual([
      "README.md",
      "assets/data.json",
      "notes/big.md",
      "skills/alpha/SKILL.md",
    ]);
  });

  test("what a run keeps holds no blob text: its skills parsed, its forks' hits, no page and no body", async () => {
    const w = forksWorld();
    const result = await listCopies(
      parseConfig(CONFIG),
      { at: AT, only: FORKS, maxSearchCalls: 0 },
      { gh: w.gh, sleep: w.sleep, now: w.now, out: () => {}, err: () => {}, peakRssBytes: () => 0 }
    );
    // Each fork's pages were read, and each named alpha, so each is a loader row...
    for (const fork of FORKS) {
      expect(
        result.loaders.filter((row) => row.repository === fork && row.skill === "alpha")
      ).toHaveLength(PAGES);
    }
    // ...but no string the result holds is a page's text or a SKILL.md's body.
    const held = stringsIn(result);
    expect(held.filter((text) => text.includes("text of a made-up page"))).toEqual([]);
    expect(held.filter((text) => text.includes("# A made-up skill"))).toEqual([]);
    expect(held.some((text) => text === "A copy in acme/fork-03.")).toBe(true);
  }, 60_000);

  test("the memory a run holds does not grow with the blobs its repositories read", async () => {
    const w = forksWorld();
    const steps: number[] = [];
    const scans: number[] = [];
    const sample = (line: string) => {
      const into = line.startsWith("copies: tree ")
        ? steps
        : line.startsWith("copies: reading the fork acme/fork-")
          ? scans
          : undefined;
      if (into === undefined) return;
      Bun.gc(true);
      into.push(heapSize());
    };
    await listCopies(
      parseConfig(CONFIG),
      { at: AT, only: FORKS, maxSearchCalls: 0, repositoryJobs: 1 },
      { gh: w.gh, sleep: w.sleep, now: w.now, out: () => {}, err: sample, peakRssBytes: () => 0 }
    );
    const read = FORKS.length * PAGES * PAGE_BYTES;
    expect(read).toBeGreaterThan(20 * 1024 ** 2);
    // One repository at a time, the forks alone: after each fork's step and before each fork's
    // scan, the heap holds about what it held after the first.
    expect(steps).toHaveLength(FORKS.length);
    expect(scans).toHaveLength(FORKS.length);
    const grown = (samples: number[]) => (samples[FORKS.length - 1] ?? 0) - (samples[0] ?? 0);
    expect(grown(steps)).toBeLessThan(4 * 1024 ** 2);
    expect(grown(scans)).toBeLessThan(4 * 1024 ** 2);
  }, 60_000);

  /** acme/data, a made-up repository beside the world's: acme/skills' own files (its skills,
   *  manifests and installer script) and data/, DATA_DIRS directories of DATA_FILES files each, all
   *  one blob (files of the same bytes have one blob id). With `walked`, its root's recursive read
   *  is answered truncated, as GitHub answers a tree that large, and each directory one level at a
   *  time, so the run walks it; else the recursive read gives the whole tree. */
  const DATA = "acme/data";
  const DATA_DIRS = 2_000;
  const DATA_FILES = 100;
  const dataWorld = (walked: boolean): { w: World; own: TreeEntry[]; count: number } => {
    const w = world();
    w.listings.set("acme", [
      ...(w.listings.get("acme") as unknown[]),
      {
        nameWithOwner: DATA,
        isFork: false,
        isArchived: false,
        defaultBranchRef: { name: "main" },
        parent: null,
      },
    ]);
    const rootSha = sha1(`root ${DATA}`);
    w.commits.set(DATA, [{ sha: sha1(`commit ${DATA}`), commit: { tree: { sha: rootSha } } }]);
    const own = treeOf(DATA, ACME_FILES);
    const directory = (dir: string): TreeEntry => ({
      path: dir,
      mode: "040000",
      type: "tree",
      sha: sha1(`tree ${DATA} ${dir}`),
    });
    const dataDir = directory("data");
    const dataDirs = Array.from({ length: DATA_DIRS }, (_, n) =>
      directory(`data/d-${String(n).padStart(4, "0")}`)
    );
    const blob = blobSha("a made-up data file\n");
    const file = (name: string): TreeEntry => ({
      path: name,
      mode: "100644",
      type: "blob",
      sha: blob,
      size: 20,
    });
    const names = Array.from(
      { length: DATA_FILES },
      (_, n) => `f-${String(n).padStart(3, "0")}.bin`
    );
    const count = own.length + 1 + DATA_DIRS * (1 + DATA_FILES);
    if (!walked) {
      const tree = [
        ...own,
        dataDir,
        ...dataDirs.flatMap((dir) => [dir, ...names.map((name) => file(`${dir.path}/${name}`))]),
      ];
      w.trees.set(`${DATA} ${rootSha} recursive`, { sha: rootSha, tree, truncated: false });
      return { w, own, count };
    }
    /** One level of `dir` in `entries`, each named within it. */
    const level = (entries: readonly TreeEntry[], dir: string) =>
      entries
        .filter((entry) => dir === "" || entry.path.startsWith(`${dir}/`))
        .map((entry) => ({
          ...entry,
          path: dir === "" ? entry.path : entry.path.slice(dir.length + 1),
        }))
        .filter((entry) => !entry.path.includes("/"));
    w.trees.set(`${DATA} ${rootSha} recursive`, {
      sha: rootSha,
      tree: own.slice(0, 10),
      truncated: true,
    });
    w.trees.set(`${DATA} ${rootSha}`, {
      sha: rootSha,
      tree: [...level(own, ""), dataDir],
      truncated: false,
    });
    for (const dir of own.filter((entry) => entry.type === "tree")) {
      w.trees.set(`${DATA} ${dir.sha}`, { sha: dir.sha, tree: level(own, dir.path) });
    }
    w.trees.set(`${DATA} ${dataDir.sha}`, { sha: dataDir.sha, tree: level(dataDirs, "data") });
    // Every directory of data/ holds the same files, so one list of them serves each.
    const files = names.map(file);
    for (const dir of dataDirs) w.trees.set(`${DATA} ${dir.sha}`, { sha: dir.sha, tree: files });
    return { w, own, count };
  };

  test("a truncated tree of over 200,000 entries is walked holding only the entries the list uses and a count of the rest", async () => {
    const newCache = () => {
      const dir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
      scratch.push(dir);
      return dir;
    };
    const args = (cacheDir: string) => [
      "--only",
      DATA,
      "--max-search-calls",
      "0",
      "--cache-dir",
      cacheDir,
    ];
    const walked = dataWorld(true);
    expect(walked.count).toBeGreaterThan(200_000);
    const samples: number[] = [];
    walked.w.onTreeBatch = () => {
      Bun.gc(true);
      samples.push(heapSize());
    };
    const walkedCache = newCache();
    Bun.gc(true);
    const before = heapSize();
    const byDirectory = await run(walked.w, args(walkedCache));
    walked.w.onTreeBatch = undefined;
    // At each batch of the walk, data/'s directories among them, the heap held about what it held
    // before the run: no level's entries, only the kept ones and the subtrees still to read.
    expect(samples.length).toBeGreaterThan(DATA_DIRS / 50);
    expect(Math.max(...samples) - before).toBeLessThan(8 * 1024 ** 2);

    const whole = dataWorld(false);
    const wholeCache = newCache();
    const read = await run(whole.w, args(wholeCache));
    expect(byDirectory.code).toBe(0);
    expect(read.code).toBe(0);
    for (const name of [
      "skill-copies.tsv",
      "manifests.tsv",
      "installers.tsv",
      "prefix-table.tsv",
    ]) {
      expect(byDirectory.text(name)).toBe(read.text(name));
    }
    expect(byDirectory.table("skill-copies.tsv").length).toBeGreaterThan(0);
    expect(byDirectory.table("manifests.tsv").length).toBeGreaterThan(0);
    const [walkedRow] = byDirectory.table("repositories.tsv");
    const [wholeRow] = read.table("repositories.tsv");
    expect(walkedRow?.tree_read).toMatch(/^by directory, \d+ reads$/);
    expect(wholeRow?.tree_read).toBe("whole");
    expect(walkedRow?.tree_entries).toBe(String(walked.count));
    expect({ ...walkedRow, tree_read: "" }).toEqual({ ...wholeRow, tree_read: "" });
    const line = `copies: tree 1/1 ${DATA}: ${walked.count} entries, `;
    expect(byDirectory.errors.filter((each) => each.startsWith(line))).toHaveLength(1);
    expect(read.errors.filter((each) => each.startsWith(line))).toHaveLength(1);
    // Either way the cache holds the kept entries and the count of them all.
    const kept = walked.own
      .filter((entry) => keepsEntry(entry, false))
      .map((entry) => entry.path)
      .sort();
    for (const cacheDir of [walkedCache, wholeCache]) {
      const cached = cachedEntry(cacheDir, DATA);
      expect(cached?.tree.map((entry) => entry.path).sort()).toEqual(kept);
      expect(cached?.treeEntries).toBe(walked.count);
    }
  }, 120_000);

  test("a cache entry holding a whole tree loads as the entries the list uses, and is written back so", async () => {
    const cacheDir = mkdtempSync(path.join(tmpdir(), "copies-cache-"));
    scratch.push(cacheDir);
    const args = ["--only", "acme/skills", "--max-search-calls", "0", "--cache-dir", cacheDir];
    const first = await run(world(), args);
    // As a run before trees kept only the entries the list uses wrote it: the whole tree.
    const entries = treeOf("acme/skills", [...madeUpFiles(), ...ACME_FILES]);
    const dir = path.join(cacheDir, "repository");
    const files = readdirSync(dir).filter((file) => file.endsWith(".json"));
    expect(files).toHaveLength(1);
    const file = path.join(dir, files[0] ?? "");
    const written = JSON.parse(readFileSync(file, "utf8")) as { key: string; value: object };
    writeFileSync(file, JSON.stringify({ ...written, value: { ...written.value, tree: entries } }));

    const w = world();
    const second = await run(w, args);
    expect(w.calls.filter((call) => /\/git\/trees\//.test(call[1] ?? ""))).toEqual([]);
    for (const name of [
      "skill-copies.tsv",
      "manifests.tsv",
      "installers.tsv",
      "repositories.tsv",
      "prefix-table.tsv",
    ]) {
      expect(second.text(name)).toBe(first.text(name));
    }
    expect(
      cachedEntry(cacheDir, "acme/skills")
        ?.tree.map((entry) => entry.path)
        .sort()
    ).toEqual(
      entries
        .filter((entry) => keepsEntry(entry, false))
        .map((entry) => entry.path)
        .sort()
    );
  }, 60_000);

  test("the run's peak resident memory is in its summary", async () => {
    const r = await run(world(), ["--only", "cybermonklabs/legion", "--max-search-calls", "0"]);
    expect(r.text("skill-copies.md")).toContain(
      "Peak resident memory 100 MiB (the run stops above 1536 MiB)."
    );
  }, 60_000);

  test("a run whose memory passes 1.5 GiB stops with a clear message and writes nothing", async () => {
    let reads = 0;
    const r = await run(world(), ["--only", "cybermonklabs/legion"], {}, () =>
      ++reads > 5 ? MEMORY_LIMIT_BYTES + 10 * 1024 ** 2 : PEAK_BYTES
    );
    expect(r.code).toBe(3);
    expect(r.errors).toContain(
      "copies: the run stops: its resident memory reached 1546 MiB, above the limit of 1536 MiB; nothing is written"
    );
    expect(existsSync(r.out)).toBe(false);
  }, 60_000);
});

describe("an owner-wide search run again per repository", () => {
  // Each owner-wide answer below is read whole but these: `dispatch` (a common word) marked
  // incomplete in cybermonklabs, `dispatch` left with hits unread in acme (a count of 120 with no
  // item given), and `beta` (no common word) at the 1,000 cap in acme. `envoy`, a common word, is
  // read whole in both owners.
  const w = world();
  w.search = (q) => {
    if (q === '"dispatch" org:cybermonklabs')
      return { body: { total_count: 3, incomplete_results: true, items: [] } };
    if (q === '"dispatch" user:acme')
      return { body: { total_count: 120, incomplete_results: false, items: [] } };
    if (q === '"beta" user:acme')
      return { body: { total_count: 1000, incomplete_results: false, items: [] } };
    return { body: EMPTY_SEARCH };
  };
  let r: Run;
  beforeAll(async () => {
    r = await run(w);
  });

  test("a common word whose owner-wide search was incomplete or left hits unread is run again in each repository holding a skill", () => {
    const reruns = r
      .queries()
      .filter((query) => query.type === "search" && query.reason !== "owner");
    const common =
      "run again per repository: a common word whose owner-wide search was not read whole";
    const capped = "run again per repository: total_count reached 1,000";
    // The owners run at once, so their reruns interleave: compare them in a fixed order.
    expect(reruns.map((query) => `${query.q} | ${query.reason}`).sort()).toEqual(
      [
        `"dispatch" repo:cybermonklabs/legion | ${common}`,
        `"beta" repo:acme/skills | ${capped}`,
        `"dispatch" repo:acme/skills | ${common}`,
      ].sort()
    );
  });

  test("a common word read whole is not run again", () => {
    const envoy = r.queries().filter((query) => query.type === "search" && query.name === "envoy");
    expect(envoy.map((query) => query.reason)).toEqual(["owner", "owner"]);
  });
});

describe("code search pacing and the call budget", () => {
  const w = world();
  const resetIn = (seconds: number) => String(Math.floor(w.clock / 1000) + seconds);
  w.search = (_q, _page, call) => {
    if (call === 1) {
      return {
        status: 403,
        stderr: "gh: API rate limit exceeded for installation. (HTTP 403)\n",
        headers: { "X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": resetIn(30) },
        body: { message: "API rate limit exceeded for installation." },
      };
    }
    if (call === 2)
      return {
        headers: { "X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": resetIn(50) },
        body: EMPTY_SEARCH,
      };
    return { body: EMPTY_SEARCH };
  };
  let r: Run;
  beforeAll(async () => {
    r = await run(w, ["--only", "cybermonklabs/legion", "--max-search-calls", "4"]);
  });

  test("a refused call waits for the reset and runs again; a spent limit waits before the next call", () => {
    expect(r.code).toBe(0);
    expect(w.sleeps).toEqual([31_000, 51_000]);
    const first = r.queries()[0];
    expect(first).toMatchObject({ status: "ok", rate_limit_retries: 1, calls: 2 });
  });

  test("the budget stops the searches, and each query it leaves out is kept as not run", () => {
    const searches = w.calls.filter((call) => call.includes("search/code"));
    expect(searches.length).toBe(4);
    const queries = r.queries().filter((query) => query.type === "search");
    expect(queries.filter((query) => query.status === "ok").length).toBe(3);
    const left = queries.filter((query) => query.status === "not run");
    expect(left.length).toBe(queries.length - 3);
    expect(left.every((query) => query.error === "the search-call budget (4) is spent")).toBe(true);
    expect(r.text("skill-copies.md")).toContain("4 code search calls (budget 4)");
  });
});

// Catches: a refused search whose answer gives no reset waited out to the reset `gh api
// rate_limit` gives, which names no owner, so the pod's gh answers it from another installation.
test("a code search refused with neither a retry-after nor a reset waits the 15-minute cap, with no read of gh api rate_limit", async () => {
  const w = world();
  w.search = (_q, _page, call) =>
    call === 1
      ? {
          status: 403,
          stderr: "gh: API rate limit exceeded for installation. (HTTP 403)\n",
          headers: { "X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": "" },
          body: { message: "API rate limit exceeded for installation." },
        }
      : { body: EMPTY_SEARCH };
  const r = await run(w, ["--only", "cybermonklabs/legion", "--max-search-calls", "2"]);
  expect(r.code).toBe(0);
  expect(w.sleeps).toEqual([900_000]);
  expect(w.calls.some((call) => call[1] === "rate_limit")).toBe(false);
  expect(
    r.errors.filter((line) =>
      /^copies: the refused call names no owner in its endpoint or its owner field, so no installation's reset is read; waiting 900 s, the longest rate wait, until \S+$/.test(
        line
      )
    )
  ).toHaveLength(1);
  expect(r.queries()[0]).toMatchObject({ status: "ok", rate_limit_retries: 1, calls: 2 });
}, 60_000);

describe("the owners' searches at once, each paced by its own limit", () => {
  // Each owner's first search is held until both owners have one in flight: searching the owners
  // one after another would never release it, and the run would not end.
  const w = world();
  w.holdFirstSearches = 2;
  let r: Run;
  beforeAll(async () => {
    r = await run(w);
  });
  const callsOf = (owner: string) => w.searchLog.filter((call) => call.owner === owner);

  test("both owners' searches are in flight at once, and each owner's one at a time", () => {
    expect(r.code).toBe(0);
    expect(Math.max(...w.searchLog.map((call) => call.inFlight))).toBe(2);
    expect(w.searchLog.every((call) => call.ownerInFlight === 1)).toBe(true);
    expect(new Set(w.searchLog.map((call) => call.owner))).toEqual(
      new Set(["cybermonklabs", "acme"])
    );
  });

  test("no owner starts more than its limit of 10 searches in any minute", () => {
    for (const owner of ["cybermonklabs", "acme"]) {
      const starts = callsOf(owner).map((call) => call.at);
      expect(starts.length).toBeGreaterThan(20);
      for (let index = 10; index < starts.length; index++) {
        expect((starts[index] ?? 0) - (starts[index - 10] ?? 0)).toBeGreaterThanOrEqual(60_000);
      }
    }
    expect(
      r.errors.some((line) =>
        /^copies: code search of acme waits \d+ s for its rate limit$/.test(line)
      )
    ).toBe(true);
  });

  test("each owner's minutes overlap the other's: the searches take the busier owner's time, not the sum", () => {
    const minutes = (owner: string) => Math.ceil(callsOf(owner).length / 10) - 1;
    const first = Math.min(...w.searchLog.map((call) => call.at));
    const last = Math.max(...w.searchLog.map((call) => call.at));
    expect(last - first).toBe(Math.max(minutes("cybermonklabs"), minutes("acme")) * 60_000);
    expect(last - first).toBeLessThan((minutes("cybermonklabs") + minutes("acme")) * 60_000);
  });
});

describe("the searches beside the tree reads", () => {
  /** acme/slow, a made-up repository beside the world's, whose tree the "before" run below holds
   *  back until three searches are answered, so that run reads it last: it holds slow-skill, a
   *  name no other repository holds, a copy of alpha, and a guide naming slow-skill. */
  const SLOW = "acme/slow";
  const SLOW_FILES: MadeFile[] = [
    {
      path: "skills/slow-skill/SKILL.md",
      text: skill("name: slow-skill\ndescription: A made-up skill of a repository read last."),
    },
    {
      path: "skills/alpha/SKILL.md",
      text: skill("name: alpha\ndescription: A made-up copy of alpha."),
    },
    { path: "docs/slow.md", text: "# Slow\n\nLoad skill://slow-skill first.\n" },
  ];
  const slowWorld = () => {
    const w = world();
    w.listings.set("acme", [
      ...(w.listings.get("acme") as unknown[]),
      {
        nameWithOwner: SLOW,
        isFork: false,
        isArchived: false,
        defaultBranchRef: { name: "main" },
        parent: null,
      },
    ]);
    w.addFiles(SLOW_FILES);
    w.addRepository(SLOW, treeOf(SLOW, SLOW_FILES));
    // alpha's owner-wide search in acme reaches GitHub's cap, so it is run again in each
    // repository of acme that holds a skill, acme/slow among them; slow-skill's finds the guide.
    w.search = (q) => {
      const reply = acmeSearch(q);
      if (q === '"alpha" user:acme') {
        return { body: { ...(reply.body as object), total_count: 1000 } };
      }
      if (q === '"slow-skill" user:acme') {
        const guide = {
          name: "slow.md",
          path: "docs/slow.md",
          sha: sha1("docs/slow.md"),
          repository: { full_name: SLOW, fork: false },
        };
        return { body: { total_count: 1, incomplete_results: false, items: [guide] } };
      }
      return reply;
    };
    return w;
  };
  /** Every output file of a run over `w`, by name; `err` takes each progress line. */
  const outputsOf = async (w: World, err: (line: string) => void) => {
    const result = await listCopies(
      parseConfig(CONFIG),
      { at: AT, only: [], maxSearchCalls: Number.POSITIVE_INFINITY },
      { gh: w.gh, sleep: w.sleep, now: w.now, out: () => {}, err, peakRssBytes: () => 0 }
    );
    const out = mkdtempSync(path.join(tmpdir(), "copies-searches-"));
    scratch.push(out);
    writeOutputs(out, result);
    return Object.fromEntries(
      readdirSync(out).map((file) => [file, readFileSync(path.join(out, file), "utf8")])
    );
  };

  test("a run whose searches start before its last tree ends writes the same output, byte for byte, as one that searches after", async () => {
    // After: each search is answered only once every tree is read (the run then reads its
    // SKILL.md files).
    const after = slowWorld();
    const treesRead = Promise.withResolvers<void>();
    after.searchGate = treesRead.promise;
    const late = await outputsOf(after, (line) => {
      if (/^copies: reading \d+ SKILL\.md files$/.test(line)) treesRead.resolve();
    });
    // Before: acme/slow's tree is answered only once three searches are.
    const before = slowWorld();
    const searched = Promise.withResolvers<void>();
    let answers = 0;
    before.onAnswer = (what) => {
      if (what.startsWith("search ") && ++answers === 3) searched.resolve();
    };
    before.treeGate = async (repository) => {
      if (repository === SLOW) await searched.promise;
    };
    const early = await outputsOf(before, () => {});

    const isTree = (what: string) => what.startsWith("tree ");
    const isSearch = (what: string) => what.startsWith("search ");
    const firstSearch = before.answered.findIndex(isSearch);
    expect(firstSearch).toBeGreaterThanOrEqual(0);
    expect(firstSearch).toBeLessThan(before.answered.indexOf(`tree ${SLOW}`));
    expect(before.answered.filter(isTree).at(-1)).toBe(`tree ${SLOW}`);
    expect(after.answered.findLastIndex(isTree)).toBeLessThan(after.answered.findIndex(isSearch));
    expect(early).toEqual(late);

    // The records keep one order: each name, sorted, in each scope, then each owner's reruns,
    // acme/slow's among them, since they were queued once every tree was read.
    const queries = (early["queries.jsonl"] ?? "")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line) as Record<string, unknown>)
      .filter((query) => query.type === "search");
    const names = [...new Set(parseTsv(early["skill-copies.tsv"] ?? "").map((row) => row.name))];
    expect(names).toContain("slow-skill");
    expect(queries.map((query) => query.q)).toEqual([
      ...names.sort().flatMap((name) => [`"${name}" org:cybermonklabs`, `"${name}" user:acme`]),
      '"alpha" repo:acme/skills',
      `"alpha" repo:${SLOW}`,
    ]);
    expect(parseTsv(early["loaders.tsv"] ?? "")).toContainEqual({
      repository: SLOW,
      path: "docs/slow.md",
      skill: "slow-skill",
      match: "skill-uri",
      source: "code search",
    });
  }, 60_000);
});

describe("the pure parts", () => {
  test.each([
    [
      "plain",
      "name: alpha\ndescription: Use when alpha is needed.",
      "alpha",
      "Use when alpha is needed.",
    ],
    [
      "double-quoted, escapes",
      'name: "alpha"\ndescription: "A \\"quoted\\" line\\nand a second"',
      "alpha",
      'A "quoted" line\nand a second',
    ],
    ["single-quoted", "name: 'alpha'\ndescription: 'It''s alpha.'", "alpha", "It's alpha."],
    [
      "folded",
      "name: alpha\ndescription: >\n  One line\n  folded.\n\n  A new paragraph.",
      "alpha",
      "One line folded.\nA new paragraph.\n",
    ],
    [
      "folded, stripped",
      "name: alpha\ndescription: >-\n  One line\n  folded.",
      "alpha",
      "One line folded.",
    ],
    [
      "literal",
      "name: alpha\ndescription: |\n  Line one.\n  Line two.",
      "alpha",
      "Line one.\nLine two.\n",
    ],
    [
      // Oh My Pi quotes the plain scalar that holds `: `, which still leaves the block no YAML,
      // and then reads each `key: value` line alone: the continuation line is no field.
      "not valid YAML, read one line at a time as Oh My Pi reads it",
      "name: alpha\ndescription: Use when: the alpha list\n  is needed.",
      "alpha",
      "Use when: the alpha list",
    ],
    [
      "no name: the directory's",
      "description: Only a description.",
      "dir-name",
      "Only a description.",
    ],
  ])("frontmatter %s", (_shape, frontmatter, name, description) => {
    const fields = skillFields(
      "skills/dir-name",
      "acme/skills",
      `---\n${frontmatter}\n---\n# Body\n`
    );
    expect(fields.name).toBe(name);
    expect(fields.description).toBe(description);
    expect(fields.descriptionBytes).toBe(Buffer.byteLength(description));
  });

  test("a SKILL.md with no frontmatter takes its directory's name", () => {
    expect(skillFields("skills/gamma", "acme/skills", "# Gamma\n")).toMatchObject({
      name: "gamma",
      nameFrom: "directory",
      fault: "no frontmatter",
    });
  });

  // Catches: a copy named by a reading of its frontmatter other than Oh My Pi's
  // (packages/utils/src/frontmatter.ts at the pin), which lists it under another name: installed
  // under the copies' name (context.ts), the capture owed a hint Oh My Pi never asked for, and the
  // copies list named it wrongly.
  test.each([
    [
      "an HTML comment before the opening ---, which Oh My Pi drops",
      "<!-- A made-up note. -->\n---\nname: other\ndescription: One.\n---\n# Body\n",
      { name: "other", nameFrom: "frontmatter", description: "One.", fault: "" },
    ],
    [
      "a closing line that starts --- (----)",
      "---\nname: other\ndescription: One.\n----\n# Body\n",
      { name: "other", nameFrom: "frontmatter", description: "One.", fault: "" },
    ],
    [
      "an opening line that starts --- (---x)",
      "---x\nname: other\ndescription: One.\n---\n# Body\n",
      { name: "other", nameFrom: "frontmatter", description: "One.", fault: "" },
    ],
    [
      "a block closed by ..., which Oh My Pi does not close, so it has none",
      "---\nname: other\ndescription: One.\n...\n# Body\n",
      { name: "dir", nameFrom: "directory", description: "", fault: "no frontmatter" },
    ],
  ])("a SKILL.md with %s is named as Oh My Pi names it", (_shape, text, fields) => {
    expect(skillFields("skills/dir", "acme/skills", text)).toMatchObject(fields);
  });

  test.each([
    ["Read skill://alpha now.", ["skill-uri"]],
    ["Run /skill:alpha.", ["slash-skill"]],
    ["See skills/alpha/SKILL.md.", ["skills-path"]],
    ["Load acme-tools:alpha.", ["plugin-qualified"]],
    ['const name = "alpha";', ["quoted-string"]],
    ["The alpha skill.", ["name"]],
    ["The tool-loop-alpha skill, alpha-two and alpha_x.", []],
  ])("mentions in %j", (text, kinds) => {
    expect(mentions(text, "alpha", ["acme-tools"], "notes.md")).toEqual(kinds as never);
  });

  test("a quoted name on a skills line of a code file is a load", () => {
    expect(
      mentions('return join(skillsDirectory, "alpha", "SKILL.md")', "alpha", [], "src/load.ts")
    ).toEqual(["quoted-string", "code-load"]);
    expect(mentions('skills = ["alpha", "beta"]', "alpha", [], "tree/init.py")).toEqual([
      "quoted-string",
      "code-load",
    ]);
    expect(mentions('skills = ["alpha", "beta"]', "alpha", [], "docs/notes.md")).toEqual([
      "quoted-string",
    ]);
  });

  test("a name is searched as its quoted words, with no quoted word holding a colon", () => {
    expect(searchTerms("alpha")).toBe('"alpha"');
    expect(searchTerms("tool-loop-alpha")).toBe('"tool-loop-alpha"');
    expect(searchTerms('"a:b"')).toBe('"a" "b"');
  });

  test.each([
    [
      "/opt/legion/pi-legion/dist/skills/alpha/",
      "/opt/legion/pi-legion/dist/skills/alpha/SKILL.md",
      true,
    ],
    ["**/.omp/agent/skills/alpha/", "/home/someone/.omp/agent/skills/alpha/references/x.md", true],
    ["**/.omp/agent/skills/alpha/", "/home/someone/.omp/agent/skills/alpha-two/SKILL.md", false],
    [
      "**/plugins/cache/plugins/acme-market___acme-tools___*/skills/alpha/",
      "/h/.omp/plugins/cache/plugins/acme-market___acme-tools___0.3.0/skills/alpha/SKILL.md",
      true,
    ],
    [
      "**/plugins/cache/acme-market/acme-tools/*/skills/alpha/",
      "/c/plugins/cache/acme-market/acme-tools/0.1.0/skills/alpha/SKILL.md",
      true,
    ],
    [
      "**/plugins/cache/acme-market/acme-tools/*/skills/alpha/",
      "/c/plugins/cache/acme-market/acme-tools/0.1/extra/skills/alpha/SKILL.md",
      false,
    ],
    [
      "/opt/legion/pi-legion/dist/skills/alpha/",
      "/srv/opt/legion/pi-legion/dist/skills/alpha/SKILL.md",
      false,
    ],
  ])("the prefix %s against %s", (prefix, copyPath, matches) => {
    expect(prefixMatches(prefix, copyPath)).toBe(matches);
  });

  test("a TSV cell keeps a tab, a line break and a backslash", () => {
    const text = tsv(["a", "b"], [["one\ttwo", "x\\y\nz"]]);
    expect(text.split("\n").length).toBe(3);
    expect(parseTsv(text)).toEqual([{ a: "one\ttwo", b: "x\\y\nz" }]);
  });

  test("a manifest of the wrong shape is refused naming the field", () => {
    expect(() =>
      parseManifest(
        "claude-marketplace",
        ".claude-plugin/marketplace.json",
        '{"name": "m", "plugins": [{"source": "./p"}]}'
      )
    ).toThrow("plugins.0.name");
  });

  test("main refuses a bad command line", async () => {
    const errors: string[] = [];
    const io = {
      gh: new World().gh,
      sleep: async () => {},
      now: () => 0,
      out: () => {},
      err: (line: string) => errors.push(line),
    };
    expect(await main(["--config", "x.json", "--at", "yesterday", "--out", "o"], io)).toBe(2);
    expect(
      await main(["--config", "x.json", "--at", AT, "--out", "o", "--only", "no-slash"], io)
    ).toBe(2);
    expect(await main(["--at", AT, "--out", "o"], io)).toBe(2);
    expect(errors).toEqual([
      'copies: --at "yesterday" is not an ISO instant ending in Z',
      'copies: --only "no-slash" is not owner/name',
      expect.stringContaining("copies: --config, --at and --out are required."),
    ]);
  });
});

describe("build.ts's profile reads of a repository", () => {
  /** acme/drift, a made-up repository whose files copies.ts's rules leave unread: a directory
   *  named SKILL.md (a tree entry, so no skill, and the repository holds none), the package.json
   *  of an Oh My Pi plugin package (read only where a SKILL.md is), and an Oh My Pi extension
   *  that names a skill and reads a skills-sources.json the repository does not hold (read only
   *  where one is). */
  const DRIFT = "acme/drift";
  const DRIFT_FILES: MadeFile[] = [
    { path: "docs/SKILL.md/notes.md", text: "# Notes kept in a directory named SKILL.md\n" },
    {
      path: "packages/acme-drift/package.json",
      text: `${JSON.stringify({ name: "@acme/acme-drift", version: "1.0.0", omp: { skills: ["skills"] } }, null, 2)}\n`,
    },
    {
      path: "omp/extensions/skill-pools.ts",
      text: `// It feeds skill://alpha.\n${EXTENSION_TEXT}`,
    },
  ];
  /** The world, with acme/drift among acme's repositories. */
  const driftWorld = (): World => {
    const w = world();
    w.listings.set("acme", [
      ...(w.listings.get("acme") as unknown[]),
      {
        nameWithOwner: DRIFT,
        isFork: false,
        isArchived: false,
        defaultBranchRef: { name: "main" },
        parent: null,
      },
    ]);
    w.addFiles(DRIFT_FILES);
    w.addRepository(DRIFT, treeOf(DRIFT, DRIFT_FILES));
    return w;
  };
  /** Each manifest as `<path> <kind>`, sorted. */
  const shown = (manifests: readonly Manifest[]) =>
    manifests.map((manifest) => `${manifest.path} ${manifest.kind}`).sort();
  /** Each installer row as its JSON text, sorted. */
  const rowTexts = (rows: readonly InstallerRow[]) => rows.map((row) => JSON.stringify(row)).sort();

  test("select the files copies.ts selects, so the installers they resolve are the installer rows copies.ts resolves", async () => {
    const config = parseConfig(CONFIG);
    // copies.ts's rules read none of acme/drift's files.
    expect(repositoryFiles(DRIFT, treeOf(DRIFT, DRIFT_FILES), config.excludePaths)).toEqual({
      skillFiles: [],
      excluded: [],
      manifests: [],
    });
    for (const repository of ["cybermonklabs/legion", "acme/skills", DRIFT]) {
      const listedWorld = driftWorld();
      const listed = await listCopies(
        config,
        { at: AT, only: [repository], maxSearchCalls: 0 },
        {
          gh: listedWorld.gh,
          sleep: listedWorld.sleep,
          now: listedWorld.now,
          out: () => {},
          err: () => {},
          peakRssBytes: () => 0,
        }
      );
      const w = driftWorld();
      const [head] = w.commits.get(repository) as {
        sha: string;
        commit: { tree: { sha: string } };
      }[];
      if (head === undefined) throw new Error(`the world has no commit of ${repository}`);
      // build.ts reads a commit's tree as one JSON answer, with no --jq: the stand-in gives it
      // from the world's recursive tree of that commit.
      const gh: Gh = async (args) => {
        const tree = /^repos\/(.+)\/git\/trees\/([0-9a-f]+)\?recursive=1$/.exec(args[1] ?? "");
        if (args[0] !== "api" || tree === null || args.includes("--jq")) return w.gh(args);
        if (tree[1] !== repository || tree[2] !== head.sha) return w.gh(args);
        const found = w.trees.get(`${repository} ${head.commit.tree.sha} recursive`) as {
          tree: TreeEntry[];
        };
        const answer = { sha: head.commit.tree.sha, tree: found.tree, truncated: false };
        return { code: 0, stdout: JSON.stringify(answer), stderr: "" };
      };
      const retries = new GhRetries({ sleep: Bun.sleep, err: () => {} }, "build-cases");
      const at = await new GithubReads(gh, new Map(), retries).at(
        repository,
        head.sha,
        config.excludePaths
      );
      // The same skill files...
      expect(at.copies.map((copy) => copy.file).sort()).toEqual(
        listed.copies
          .filter((copy) => copy.repository === repository)
          .map((copy) => copy.file)
          .sort()
      );
      // ...the same manifests listed...
      expect(shown(at.manifests.filter(listedManifest))).toEqual(
        shown(
          listed.manifests.filter(
            (manifest) => manifest.repository === repository && manifest.source === "tree"
          )
        )
      );
      // ...and the same installers.
      if (repository !== DRIFT) expect(listed.rows.length).toBeGreaterThan(0);
      expect(rowTexts(resolveInstallers(at.manifests, new CopyIndex(at.copies)).rows)).toEqual(
        rowTexts(listed.rows)
      );
    }
  }, 60_000);
});

test("a plugin list's exact npm versions are its npm pins; a package's release tag is <directory>-v<version>, or v<version> at a repository's root; a tarball's skills are those its manifest's directories hold", () => {
  const file = "omp/plugins/package.json";
  const list = parseManifest(
    "omp-plugins",
    file,
    JSON.stringify({
      name: "omp-plugins",
      dependencies: {
        "@acme/kit": "1.0.0",
        "@acme/range": "^2.0.0",
        "acme-powers": "github:acme/powers#v1.2.0",
        "@acme/pre": "3.0.0-rc.1",
      },
    })
  );
  const manifest = { repository: "acme/dots", path: file, facts: list.facts } as Manifest;
  expect(npmPins([manifest]).map((pin) => `${pin.package} ${pin.version}`)).toEqual([
    "@acme/kit 1.0.0",
    "@acme/pre 3.0.0-rc.1",
  ]);
  expect(releaseTag("packages/pi-envoy/package.json", "15.1.2")).toBe("pi-envoy-v15.1.2");
  expect(releaseTag("package.json", "0.2.9")).toBe("v0.2.9");
  /** A SKILL.md of `name` with a description, which Oh My Pi lists. */
  const described = (name: string) =>
    skill(`name: ${name}\ndescription: Use when a made-up ${name} step is due.`);
  const files = new Map([
    [
      "package.json",
      JSON.stringify({ name: "@acme/kit", omp: { skills: ["dist/skills", "one"] } }),
    ],
    ["dist/skills/a/SKILL.md", described("a")],
    ["dist/skills/b/SKILL.md", described("b")],
    ["dist/skills/b/references/x.md", "x"],
    ["one/SKILL.md", described("one")],
    ["skills/c/SKILL.md", described("c")],
  ]);
  expect(tarballSkills(files)).toEqual(["a", "b", "one"]);
  // With no skills key, Oh My Pi falls back to skills/.
  files.set("package.json", JSON.stringify({ name: "@acme/kit", pi: {} }));
  expect(tarballSkills(files)).toEqual(["c"]);
});

// Catches: a tarball's skills read by rules of their own, so one pin named other skills when
// build.ts read it from the registry's tarball than when it read the release tag's tree: a
// package with no omp or pi manifest shipping skills/, skills/SKILL.md taken as a skill of the
// fallback, a declared directory's own SKILL.md hiding its children, no namespace level, no skip
// of `.` names, a skill with no description listed, and a skill named after its directory.
test("a tarball's skills are read by the rule a package in a tree is read by (CopyIndex.scan)", () => {
  const described = (name: string) =>
    skill(`name: ${name}\ndescription: Use when a made-up ${name} step is due.`);
  const fallback = new Map([
    ["package.json", JSON.stringify({ name: "@acme/kit", pi: {} })],
    ["skills/SKILL.md", described("whole")],
    ["skills/c/SKILL.md", described("c")],
    ["skills/.hidden/SKILL.md", described("hidden")],
    ["skills/group/inner/SKILL.md", described("inner")],
    ["skills/nodesc/SKILL.md", skill("name: nodesc")],
    ["skills/dir-x/SKILL.md", described("real-name")],
  ]);
  expect(tarballSkills(fallback)).toEqual(["c", "inner", "real-name"]);
  // A package with neither an omp nor a pi manifest is no Oh My Pi plugin.
  fallback.set("package.json", JSON.stringify({ name: "@acme/kit" }));
  expect(tarballSkills(fallback)).toEqual([]);
  // A declared directory that holds a SKILL.md and children that do lists all of them.
  const declared = new Map([
    ["package.json", JSON.stringify({ name: "@acme/kit", omp: { skills: ["one"] } })],
    ["one/SKILL.md", described("one")],
    ["one/sub/SKILL.md", described("sub")],
  ]);
  expect(tarballSkills(declared)).toEqual(["one", "sub"]);
});

// Catches: the profile build and the copies listing starting a gh process for every read at once
// (a pod's gh is about 145 MiB a call), so the runner's processes pass the pod's memory.
describe("the runner's one bound on gh processes", () => {
  test("a Limiter of 3 runs every task, never more than 3 at once, the waiting ones in the order they came", async () => {
    const limiter = new Limiter(3);
    let running = 0;
    let most = 0;
    const started: number[] = [];
    const gates = Array.from({ length: 8 }, () => Promise.withResolvers<void>());
    const done = gates.map((gate, n) =>
      limiter.run(async () => {
        started.push(n);
        running += 1;
        most = Math.max(most, running);
        await gate.promise;
        running -= 1;
        return n;
      })
    );
    await Bun.sleep(0);
    expect(started).toEqual([0, 1, 2]);
    for (const gate of gates) {
      gate.resolve();
      await Bun.sleep(0);
    }
    expect(await Promise.all(done)).toEqual([0, 1, 2, 3, 4, 5, 6, 7]);
    expect(started).toEqual([0, 1, 2, 3, 4, 5, 6, 7]);
    expect(most).toBe(3);
    expect(() => new Limiter(0)).toThrow("is not a whole number of 1 or more");
  });

  test("spawnGh, the gh of copies and of build's GitHub reads, runs at most GH_CONCURRENCY (4) gh processes at once, however many calls come at once", async () => {
    const dir = mkdtempSync(path.join(process.env.TMPDIR ?? tmpdir(), "skill-cuts-gh-"));
    const log = path.join(dir, "log");
    // A stand-in gh that logs its start and end and answers its first argument.
    writeFileSync(
      path.join(dir, "gh"),
      `#!/bin/sh\necho "start $(date +%s%N)" >> '${log}'\nsleep 0.3\necho "end $(date +%s%N)" >> '${log}'\necho "$1"\n`,
      { mode: 0o755 }
    );
    const PATH = process.env.PATH;
    process.env.PATH = `${dir}:${PATH ?? ""}`;
    try {
      expect(GH_CONCURRENCY).toBe(4);
      expect(ghLimiter().limit).toBe(GH_CONCURRENCY);
      const calls = Array.from({ length: 9 }, (_, n) => `call-${n}`);
      const results = await Promise.all(calls.map((call) => spawnGh([call])));
      expect(results.map((each) => each.stdout.trim())).toEqual(calls);
      const events = readFileSync(log, "utf8")
        .trim()
        .split("\n")
        .map((line) => {
          const [kind = "", at = "0"] = line.split(" ");
          return { start: kind === "start", at: BigInt(at) };
        })
        .sort((a, b) => (a.at === b.at ? Number(a.start) - Number(b.start) : a.at < b.at ? -1 : 1));
      let running = 0;
      let most = 0;
      for (const event of events) {
        running += event.start ? 1 : -1;
        most = Math.max(most, running);
      }
      expect(events).toHaveLength(18);
      expect(most).toBe(GH_CONCURRENCY);
    } finally {
      process.env.PATH = PATH;
      rmSync(dir, { recursive: true, force: true });
    }
  }, 60_000);
});

// Catches: a gh call that hangs (a connection that never answers) holding a copies listing or a
// profile build for ever: copies.ts's gh had no limit, while context.ts's reads had one.
test("a gh call that runs past its limit is ended: it exits 124, its error names the call and the limit, and GhRetries takes it as a transient failure", async () => {
  const dir = mkdtempSync(path.join(process.env.TMPDIR ?? tmpdir(), "skill-cuts-gh-hung-"));
  // A stand-in gh that waits in a child of its own, which holds its output open.
  writeFileSync(path.join(dir, "gh"), "#!/bin/sh\nsleep 5\n", { mode: 0o755 });
  const PATH = process.env.PATH;
  process.env.PATH = `${dir}:${PATH ?? ""}`;
  try {
    const started = performance.now();
    const result = await spawnGhWithin(200)(["api", "repos/acme/skills"]);
    expect(performance.now() - started).toBeLessThan(3_000);
    expect(result.code).toBe(124);
    expect(result.stderr).toContain("gh api repos/acme/skills timed out after 200 ms");
    expect(transientReason(result.stderr)).toBe("time limit");
  } finally {
    process.env.PATH = PATH;
    rmSync(dir, { recursive: true, force: true });
  }
}, 60_000);

// Catches: a rate wait of up to 15 minutes that writes nothing, so a run that waits looks hung (a
// run stopped three times for 901-902 s with no child process and no line).
test("GhRetries says each wait for a rate limit before it waits: the call, gh's reason, the wait and its end", async () => {
  const lines: string[] = [];
  const waits: number[] = [];
  const retries = new GhRetries(
    { sleep: async (ms) => void waits.push(ms), err: (line) => lines.push(line) },
    "copies"
  );
  const tries = [
    { code: 1, stderr: "gh: API rate limit exceeded for installation ID 1. (HTTP 403)\n" },
    { code: 0, stderr: "" },
  ];
  const before = Date.now();
  const result = await retries.send(async () => tries.shift() ?? { code: 0, stderr: "" }, {
    rateWait: async () => 900_000,
  });
  expect(result.code).toBe(0);
  expect(waits).toEqual([900_000]);
  expect(lines).toHaveLength(1);
  const said =
    /^copies: a gh call was refused for its rate limit \(gh: API rate limit exceeded for installation ID 1\. \(HTTP 403\)\); waiting 900 s, until (\S+), before sending it again, refusal 1 of 5$/.exec(
      lines[0] ?? ""
    );
  expect(said).not.toBeNull();
  const until = Date.parse(said?.[1] ?? "");
  expect(until - before).toBeGreaterThanOrEqual(900_000);
  expect(until - Date.now()).toBeLessThanOrEqual(900_000);
});

// Catches: a refusal for GitHub's secondary rate limit waited out to the core limit's reset, which
// it has none of (a run sat 15 minutes with 13,924 of 15,000 core calls left), in context.ts's reads,
// which had their own rule; copies.ts's had this one.
test("the one rate-wait rule of copies.ts and context.ts: a secondary-limit refusal waits a minute, doubled at each refusal, and asks for no reset; a primary refusal waits to its reset", async () => {
  let asked = 0;
  const untilReset = async () => {
    asked += 1;
    return 600_000;
  };
  const secondary =
    "gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)";
  const waits: number[] = [];
  for (const refused of [0, 1, 2]) waits.push(await rateLimitWait(secondary, refused, untilReset));
  expect(waits).toEqual([SECONDARY_WAIT_MS, 2 * SECONDARY_WAIT_MS, 4 * SECONDARY_WAIT_MS]);
  expect(SECONDARY_WAIT_MS).toBe(60_000);
  expect(asked).toBe(0);
  const primary = "gh: API rate limit exceeded for installation ID 1. (HTTP 403)";
  expect(await rateLimitWait(primary, 0, untilReset)).toBe(600_000);
  expect(await rateLimitWait(primary, 3, untilReset)).toBe(600_000);
  expect(asked).toBe(2);
});
