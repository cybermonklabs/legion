import { afterAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import {
  type Attribution,
  attribute as attributeByProfile,
  type CountRow,
  checkoutRule,
  copyPrefix,
  countOnce,
  type FormerStagesRow,
  type InstallersRow,
  MODELS_BY_CUT_COLUMNS,
  MODELS_BY_SKILL_COLUMNS,
  ModelCountFile,
  main,
  modelCount,
  modelId,
  notAnthropicShare,
  PROJECT_SKILL_DIRS,
  type PrefixTableRow,
  parseModelOutput,
  parseQueryOutput,
  parseRunLog,
  type QueryOutput,
  queryIssues,
  type R4Tables,
  READ_COUNT_COLUMNS,
  rankChanged,
  readCount,
  readCountTsv,
  type SkillCopiesRow,
  sessionFiles,
  sessionKey,
  sessionModel,
  sessionModels,
} from "./attribute";
import { PROJECT_SKILLS } from "./build";
import { loadConfig, parseConfig } from "./config";
import {
  CopyIndex,
  FORMER_STAGES_COLUMNS,
  INSTALLERS_COLUMNS,
  LOADERS_COLUMNS,
  MANIFESTS_COLUMNS,
  PREFIX_TABLE_COLUMNS,
  parseTsv,
  REPOSITORIES_COLUMNS,
  resolveInstallers,
  SKILL_COPIES_COLUMNS,
  type SkillCopy,
  tsv,
} from "./copies";
import { main as countSqlMain, fillTemplate } from "./countsql";
import { psqlRows } from "./psql-rows";
import { madeManifest } from "./testdata/fixtures";

// attribute.ts on F7, count.sql's and models.sql's own output under the store command's flags
// (testdata/F7-count-first.txt and F7-count-second.txt, whose capture commands psql-rows.test.ts's
// header gives, and F7-models-first.txt, F7-models-second.txt and F7-models-later.txt, whose
// count.test.ts's header gives), and on made-up copies outputs (copies.ts's tables) of made-up
// repositories (acme/skills, acme/tools, acme/dotfiles, acme/cc, acme/kiln, ...). Other query
// outputs are made here in F7's shape: a psql table of rows, read back with the parsers.
//
// F7-count-first.txt holds, for store_a and the day 2026-11-20:
// - H1 (F1's session): a read of alpha at F1_PROFILE/alpha/SKILL.md, and of its references/x.md;
// - H2 (F4's session): a read of alpha at F3/marketplace/plugins/acme-tools/skills/alpha/SKILL.md,
//   and of beta at F3/config/plugins/cache/acme-market/acme-tools/0.1.0/skills/beta/SKILL.md;
// - H3 (F1's subagent): a read of beta at F1_PROFILE/beta/SKILL.md.
// F7-count-second.txt holds H1B (F1b's session) reading alpha at F1_PROFILE/alpha/SKILL.md, and
// of its references/x.md. F1b is F1's session with its alpha read copied a day later, so it holds
// each of H1's reads (the same keys) and one of its own: in one count with F7-count-first.txt,
// H1's reads count in H1B, whose hash sorts first, and H1 is no session.
// F7-models-first.txt gives H1 and H3 their model_change model and H2 its first assistant
// message's (from its second part); F7-models-second.txt gives F1c (F1's subagent without its
// model_change line) its session_init model and F1d (F1b's first two lines) none;
// F7-models-later.txt gives H1B the model_change model of its second part.
//
// The made-up world of those rows (`f7World`): alpha has one copy (acme/skills src/alpha) and
// beta two (acme/skills src/beta, acme/tools plugins/acme-tools/skills/beta). An installer of
// acme/dotfiles links both acme/skills copies into Oh My Pi's user skills, and acme/tools'
// marketplace ships its beta in the plugin acme-tools. The prefix table maps F1_PROFILE/alpha/ to
// the linked alpha and the plugin cache's beta to acme/tools' beta; F1_PROFILE/beta/ and the
// marketplace's alpha are in no prefix row, and no copy's skill directory ends either path, so the
// checkout rule places neither.

const TESTDATA = path.join(import.meta.dir, "testdata");
const F1_PROFILE = "/legion/scratch/tmp/acme-1/t1-runs/f1/home/.omp/profiles/fixture/agent/skills";
const F3 = "/legion/scratch/tmp/acme-1/t1-runs/f3";
const H1 = "83af4b8e0c75a050";
const H2 = "1e93689d7698bd6a";
const H3 = "0da7427e81182998";
const H1B = "2e8fca5c3950db03";
const H1C = "cf863e47a559fcca";
const H1D = "6bf367db484f59de";
const H4 = "4444444444444444";

const CONFIG_FILE = path.join(TESTDATA, "F7-skill-cuts.json");
const CONFIG = loadConfig(CONFIG_FILE);
const TWO_STORES = parseConfig({
  ...(JSON.parse(readFileSync(CONFIG_FILE, "utf8")) as object),
  stores: [
    { name: "store_a", context: "devbox" },
    { name: "store_b", context: "legion:ACME" },
  ],
});

const dir = mkdtempSync(path.join(process.env.TMPDIR ?? tmpdir(), "skill-cuts-attribute-"));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const f7 = (name: string) => readFileSync(path.join(TESTDATA, name), "utf8");
const F7_FIRST = () => parseQueryOutput("store_a", "2026-11-20", f7("F7-count-first.txt"));
const F7_SECOND = () => parseQueryOutput("store_a", "2026-11-21", f7("F7-count-second.txt"));

/** A psql aligned table of `rows`, one JSON value each, as the store command prints it. */
function psqlTable(rows: readonly object[]): string {
  const lines = rows.map((row) => ` ${JSON.stringify(row)}`);
  return [" total", "-".repeat(40), ...lines, `(${rows.length} rows)`, ""].join("\n");
}

/** Older fixtures model a single profile in each store. */
function attribute(config: typeof CONFIG, outputs: readonly QueryOutput[], tables: R4Tables) {
  return attributeByProfile(
    config,
    outputs,
    tables,
    new Map(
      outputs.flatMap(({ store, rows }) =>
        rows.flatMap(({ hashes }) =>
          hashes.map((hash) => [sessionKey(store, hash), "fixture"] as const)
        )
      )
    )
  );
}

const fixtureRoots = (...hashes: string[]) =>
  psqlTable(hashes.map((hash) => ({ hash, profile: "fixture" })));

const summaryRow = (store: string) => ({
  row: "summary",
  store,
  mtime_from: 0,
  mtime_to: 0,
  files: 0,
  parts: 0,
  bytes: 0,
  lines_parsed: 0,
  bad_lines: 0,
  shared_call_ids: 0,
});

/** A count.sql result of `store` for the query `name`, in F7's shape. A file's one key is its own
 *  hash unless the row's `calls` gives its keys, so no two files hold a key. */
function queryText(
  store: string,
  name: string,
  rows: readonly (Pick<CountRow, "skill" | "copy"> & {
    readonly hashes: readonly string[];
    readonly kind?: CountRow["kind"];
    readonly files?: number;
    readonly calls?: CountRow["calls"];
    readonly unpaired?: number;
    readonly unpaired_hashes?: readonly string[];
    readonly unpaired_calls?: CountRow["calls"];
  })[]
): string {
  const own = (hashes: readonly string[]) =>
    Object.fromEntries(hashes.map((hash) => [hash, [hash]]));
  return psqlTable([
    ...rows.map((row) => ({
      row: "count",
      store,
      day: name.slice(0, 10),
      kind: "read",
      files: row.hashes.length,
      calls: own(row.hashes),
      unpaired: 0,
      unpaired_hashes: [],
      unpaired_calls: own(row.unpaired_hashes ?? []),
      ...row,
    })),
    summaryRow(store),
  ]);
}

function query(store: string, name: string, rows: Parameters<typeof queryText>[2]): QueryOutput {
  return parseQueryOutput(store, name, queryText(store, name, rows));
}

/** A models.sql result of `store` for the query `name`, in F7-models' shape: each file's
 *  `[hash, model, source]`. */
function modelsText(
  store: string,
  rows: readonly (readonly [string, string | null, string | null])[]
): string {
  return psqlTable([
    ...rows.map(([hash, model, source]) => ({ row: "model", store, hash, model, source })),
    {
      row: "summary",
      store,
      mtime_from: 0,
      mtime_to: 0,
      files: rows.length,
      with_model: rows.filter(([, model]) => model !== null).length,
      bytes: 0,
      lines_parsed: 0,
      bad_lines: 0,
    },
  ]);
}

function copyRow(
  repository: string,
  dir: string,
  name: string,
  more: Partial<SkillCopiesRow> = {}
): SkillCopiesRow {
  return {
    repository,
    path: dir,
    name,
    description_bytes: "20",
    fork: "no",
    archived: "no",
    parent: "",
    installers: "",
    loaded_by: "",
    ...more,
  };
}

type Unit = Pick<
  InstallersRow,
  "unit" | "manifest_repository" | "manifest_path" | "kind" | "action" | "host"
>;
const USER_SKILLS: Unit = {
  unit: "user-skills:omp",
  manifest_repository: "acme/dotfiles",
  manifest_path: "installers/omp.sh",
  kind: "installer-script",
  action: "links",
  host: "omp",
};
/** A Claude Code plugin of acme/cc's marketplace `m`. */
const plugin = (name: string, repository = "acme/cc"): Unit => ({
  unit: `plugin:${name}@m`,
  manifest_repository: repository,
  manifest_path: ".claude-plugin/marketplace.json",
  kind: "claude-marketplace",
  action: "ships",
  host: "claude-code",
});
const ACME_TOOLS: Unit = {
  ...plugin("acme-tools", "acme/tools"),
  unit: "plugin:acme-tools@acme-market",
};

const installs = (unit: Unit, copy: SkillCopiesRow): InstallersRow => ({
  ...unit,
  skill: copy.name,
  copy_repository: copy.repository,
  copy_path: copy.path,
  note: "",
});
const via = (unit: Unit) => `${unit.unit} (${unit.manifest_repository}:${unit.manifest_path})`;
const prefixRow = (prefix: string, copy: SkillCopiesRow, unit: Unit): PrefixTableRow => ({
  prefix,
  repository: copy.repository,
  path: copy.path,
  name: copy.name,
  via: via(unit),
});
const repositoryRow = (repository: string) => ({
  repository,
  branch: "main",
  commit: "0123456789abcdef0123456789abcdef01234567",
  fork: "no",
  archived: "no",
  parent: "",
  tree_entries: "10",
  tree_read: "whole",
  skills: "2",
  error: "",
});

const ALPHA = copyRow("acme/skills", "src/alpha", "alpha");
const BETA = copyRow("acme/skills", "src/beta", "beta");
const TOOLS_BETA = copyRow("acme/tools", "plugins/acme-tools/skills/beta", "beta");

function f7World(): R4Tables {
  return {
    copies: [ALPHA, BETA, TOOLS_BETA],
    installers: [
      installs(USER_SKILLS, ALPHA),
      installs(USER_SKILLS, BETA),
      installs(ACME_TOOLS, TOOLS_BETA),
    ],
    prefixes: [
      prefixRow("**/profiles/fixture/agent/skills/alpha/", ALPHA, USER_SKILLS),
      prefixRow("**/plugins/cache/acme-market/acme-tools/*/skills/beta/", TOOLS_BETA, ACME_TOOLS),
    ],
    formerStages: [],
    loaders: [],
    repositories: [repositoryRow("acme/skills"), repositoryRow("acme/tools")],
  };
}

const shownCopy = (copy: SkillCopiesRow) => `${copy.repository} ${copy.path}`;
function countOf(attribution: Attribution, copy: SkillCopiesRow) {
  const found = attribution.copies.find(
    (count) => count.repository === copy.repository && count.path === copy.path
  );
  if (found === undefined) throw new Error(`no count for ${shownCopy(copy)}`);
  return found;
}
const outcomesOf = (attribution: Attribution, prefix: string) =>
  attribution.unmapped.find((item) => item.prefix === prefix)?.outcomes;

test("a mapped prefix goes to its copy", () => {
  const attribution = attribute(CONFIG, [F7_FIRST()], f7World());
  expect(attribution.mappedPrefixes.get(`${F1_PROFILE}/alpha/`)).toEqual(
    new Set([shownCopy(ALPHA)])
  );
  expect(
    attribution.mappedPrefixes.get(
      `${F3}/config/plugins/cache/acme-market/acme-tools/0.1.0/skills/beta/`
    )
  ).toEqual(new Set([shownCopy(TOOLS_BETA)]));
  expect(countOf(attribution, TOOLS_BETA)).toMatchObject({ sessions: 1, referenceSessions: 0 });
  // H1's read and its reference read of references/x.md, both under F1_PROFILE/alpha/.
  expect(countOf(attribution, ALPHA).referenceSessions).toBe(1);
});

test("an unmapped read goes to the copy the session's context installs", () => {
  const attribution = attribute(CONFIG, [F7_FIRST()], f7World());
  // store_a's mapped reads come from the linked user skills and the acme-tools plugin, which
  // between them hold one alpha: H2's alpha read from the marketplace goes to it.
  expect(attribution.stores[0]?.units).toEqual([
    { unit: via(ACME_TOOLS), sessions: 1, copies: 1 },
    { unit: via(USER_SKILLS), sessions: 1, copies: 2 },
  ]);
  expect(countOf(attribution, ALPHA)).toMatchObject({ sessions: 2, unattributed: 0 });
  expect(
    attribution.unmapped.find(
      (item) => item.prefix === `${F3}/marketplace/plugins/acme-tools/skills/alpha/`
    )
  ).toEqual({
    prefix: `${F3}/marketplace/plugins/acme-tools/skills/alpha/`,
    skills: ["alpha"],
    sessions: 1,
    referenceSessions: 0,
    outcomes: [
      {
        outcome:
          "store_a: context: to acme/skills src/alpha, the one copy of alpha its context installs",
        sessions: 1,
      },
    ],
  });
});

test("a read that neither the prefix table, a checkout nor the context decides is unattributed", () => {
  // store_b's only read maps by no prefix row, so its context installs no unit.
  const unmappedOnly = query("store_b", "2026-11-20", [
    { skill: "alpha", copy: "/work/acme/skills/alpha/SKILL.md", hashes: [H4] },
  ]);
  const attribution = attribute(TWO_STORES, [F7_FIRST(), unmappedOnly], f7World());
  // H3's beta under F1_PROFILE/beta/: the context holds two copies of beta.
  expect(outcomesOf(attribution, `${F1_PROFILE}/beta/`)).toEqual([
    {
      outcome:
        "store_a: unattributed: no copy's skill directory ends the path, and its context installs 2 copies of beta (acme/skills src/beta, acme/tools plugins/acme-tools/skills/beta)",
      sessions: 1,
    },
  ]);
  expect(countOf(attribution, BETA)).toMatchObject({ sessions: 0, unattributed: 1 });
  expect(countOf(attribution, TOOLS_BETA)).toMatchObject({ sessions: 1, unattributed: 1 });
  expect(attribution.stores[1]?.units).toEqual([]);
  expect(outcomesOf(attribution, "/work/acme/skills/alpha/")).toEqual([
    {
      outcome:
        "store_b: unattributed: no copy's skill directory ends the path, and its context installs no copy of alpha",
      sessions: 1,
    },
  ]);
  expect(countOf(attribution, ALPHA)).toMatchObject({ sessions: 2, unattributed: 1 });
  expect(attribution.unattributedSessions).toBe(2);
});

// The checkout rule: copies at directories that F7's reads and the reads below end with.
const DOT_ALPHA = copyRow("acme/dotfiles", "plugins/acme/skills/alpha", "alpha");
const KILN_GAMMA = copyRow("acme/kiln", ".claude/skills/gamma", "gamma");
const KILN_DELTA = copyRow("acme/kiln", ".claude/skills/delta", "delta");
const OTHER_DELTA = copyRow("acme/other", ".claude/skills/delta", "delta");
const ORG1_OMEGA = copyRow("org1/legion", "skills/omega", "omega");
const ORG2_OMEGA = copyRow("org2/legion", "skills/omega", "omega");
const ROOT_SIGMA = copyRow("acme/rootskills", "sigma", "sigma");
const KILN_ETA = copyRow("acme/kiln", "skills/eta", "eta");
// Two repositories whose names one holds the other's, at one skill directory, as R4's list of the
// three owners holds a proof repository and its `-internal` one, and a cut repository whose name
// ends with the name of another repository.
const PROOFS_ADDING = copyRow("acme/proofs", ".claude/skills/adding-a-proof", "adding-a-proof");
const INTERNAL_ADDING = copyRow(
  "acme/proofs-internal",
  ".claude/skills/adding-a-proof",
  "adding-a-proof"
);
const BINARY_FORGE_WIKI = copyRow("acme/binary-forge", ".claude/skills/forge-wiki", "forge-wiki");
const FORGE_WIKI = copyRow("acme/forge", ".claude/skills/forge-wiki", "forge-wiki");

function checkoutWorld(): R4Tables {
  const world = f7World();
  return {
    ...world,
    copies: [
      ...world.copies,
      DOT_ALPHA,
      KILN_GAMMA,
      KILN_DELTA,
      OTHER_DELTA,
      ORG1_OMEGA,
      ORG2_OMEGA,
      ROOT_SIGMA,
      KILN_ETA,
      PROOFS_ADDING,
      INTERNAL_ADDING,
      BINARY_FORGE_WIKI,
      FORGE_WIKI,
    ],
  };
}

test.each([
  [
    "a dotfiles checkout read goes to dotfiles' copy, not to the copy of that name its context installs",
    "/home/acme/.dotfiles/plugins/acme/skills/alpha/SKILL.md",
    DOT_ALPHA,
    "checkout: to acme/dotfiles plugins/acme/skills/alpha: the path ends with plugins/acme/skills/alpha/, its skill directory",
  ],
  [
    "a Legion workspace checkout read of .claude/skills/<name> goes to that repository",
    "/legion/workspaces/acme/kiln/acme-12/.claude/skills/gamma/SKILL.md",
    KILN_GAMMA,
    "checkout: to acme/kiln .claude/skills/gamma: the path ends with .claude/skills/gamma/, its skill directory",
  ],
  [
    "a tie between repositories is broken by the repository's name in a directory of the path",
    "/legion/workspaces/someone/kiln-acme-7/.claude/skills/delta/SKILL.md",
    KILN_DELTA,
    "checkout: to acme/kiln .claude/skills/delta: the path ends with .claude/skills/delta/, the skill directory of 2 repositories, and the path names kiln in a directory",
  ],
  [
    "a tie between repositories of one name is broken by the owner and name in the path",
    "/legion/workspaces/org2/legion/i-1/skills/omega/SKILL.md",
    ORG2_OMEGA,
    "checkout: to org2/legion skills/omega: the path ends with skills/omega/, the skill directory of 2 repositories, and the path names org2/legion",
  ],
  // Catches: R1 on R4's list of the three owners, where a workspace path naming the owner and the
  // `-internal` repository tied it with the repository whose name it holds (1,434 read sessions
  // of adding-a-proof), and a checkout of a cut repository tied it with the repository whose name
  // its own ends with (75 read sessions of one of the cut's changed skills), each left
  // unattributed.
  [
    "the owner and the exact name in a row win over the owner and a directory that only holds the other's name",
    "/legion/workspaces/acme/proofs-internal/p-526/.claude/skills/adding-a-proof/SKILL.md",
    INTERNAL_ADDING,
    "checkout: to acme/proofs-internal .claude/skills/adding-a-proof: the path ends with .claude/skills/adding-a-proof/, the skill directory of 2 repositories, and the path names acme/proofs-internal",
  ],
  [
    "an exact name as a directory wins over a directory that only holds the other's name",
    "/home/dev/src/binary-forge/.claude/skills/forge-wiki/SKILL.md",
    BINARY_FORGE_WIKI,
    "checkout: to acme/binary-forge .claude/skills/forge-wiki: the path ends with .claude/skills/forge-wiki/, the skill directory of 2 repositories, and the path names binary-forge in a directory",
  ],
  [
    "a tie the path does not break, nor the context, which installs none of the tied copies, is unattributed",
    "/tmp/work/.claude/skills/delta/SKILL.md",
    undefined,
    "unattributed: the path ends with .claude/skills/delta/, the skill directory of 2 repositories (acme/kiln, acme/other), and no directory of the path names just one of them, and its context installs none of them",
  ],
  [
    "a one-segment skill directory counts when the path names its repository",
    "/home/acme/src/rootskills/sigma/SKILL.md",
    ROOT_SIGMA,
    "checkout: to acme/rootskills sigma: the path ends with sigma/, its skill directory, and the path names rootskills in a directory",
  ],
  [
    "a one-segment skill directory does not count when the path does not name its repository",
    "/home/acme/cache/sigma/SKILL.md",
    undefined,
    "unattributed: no copy's skill directory ends the path, and its context installs no copy of sigma",
  ],
  [
    "a read inside a clone an installer makes (<dir>/vendor/<name>/) is no checkout of a repository the clone's directories do not name",
    "/home/acme/.dotfiles/vendor/eta-pack/skills/eta/SKILL.md",
    undefined,
    "unattributed: the path ends with skills/eta/, the skill directory of acme/kiln, inside /home/acme/.dotfiles/vendor/eta-pack/, a copy of another repository that an installer writes, which names none of them; no prefix row maps it, and the context rule claims no copy for a read inside a clone",
  ],
  [
    "a read inside a clone that no prefix row maps is unattributed, though its context installs one copy of its name",
    "/home/acme/.dotfiles/vendor/alpha-pack/skills/alpha/SKILL.md",
    undefined,
    "unattributed: no copy's skill directory ends the path, which is inside /home/acme/.dotfiles/vendor/alpha-pack/, a clone an installer makes; no prefix row maps it, and the context rule claims no copy for a read inside a clone",
  ],
  [
    "a read inside a package a plugins package.json installs (node_modules/<name>/) is no checkout of a repository it does not name",
    "/home/acme/.dotfiles/omp/plugins/node_modules/eta-tools/skills/eta/SKILL.md",
    undefined,
    "unattributed: the path ends with skills/eta/, the skill directory of acme/kiln, inside /home/acme/.dotfiles/omp/plugins/node_modules/eta-tools/, a copy of another repository that an installer writes, which names none of them, and its context installs no copy of eta",
  ],
  [
    "a read inside a clone whose directory names the repository is a checkout of it",
    "/home/acme/.dotfiles/vendor/kiln/skills/eta/SKILL.md",
    KILN_ETA,
    "checkout: to acme/kiln skills/eta: the path ends with skills/eta/, its skill directory, and the path names kiln in a directory of the copy an installer writes at /home/acme/.dotfiles/vendor/kiln/",
  ],
] as const)("%s", (_name, readPath, copy, outcome) => {
  const skill = path.posix.basename(path.posix.dirname(readPath));
  const reads = query("store_a", "2026-11-22", [{ skill, copy: readPath, hashes: [H4] }]);
  const attribution = attribute(CONFIG, [F7_FIRST(), reads], checkoutWorld());
  expect(outcomesOf(attribution, copyPrefix(readPath, skill))).toEqual([
    { outcome: `store_a: ${outcome}`, sessions: 1 },
  ]);
  const placed = attribution.copies.filter((count) =>
    count.readSessions.has(sessionKey("store_a", H4))
  );
  expect(placed.map((count) => `${count.repository} ${count.path}`)).toEqual(
    copy === undefined ? [] : [shownCopy(copy)]
  );
  // The context installs one alpha (acme/skills'), whose sessions stay H1's and H2's.
  expect(countOf(attribution, ALPHA).sessions).toBe(2);
});

// A plugin of another repository holds zeta at plugins/acme-plugs/skills/zeta and theta beside it;
// a session of a Legion workspace of acme/proj reads zeta in the workspace's own .claude/skills.
const PLUGS: Unit = plugin("acme-plugs", "acme/plugs");
const PLUGS_ZETA = copyRow("acme/plugs", "plugins/acme-plugs/skills/zeta", "zeta");
const PLUGS_THETA = copyRow("acme/plugs", "plugins/acme-plugs/skills/theta", "theta");
const PROJECT_ZETA = copyRow("acme/proj", ".claude/skills/zeta", "zeta");
const THETA_READ = "/home/dev/.claude/plugins/cache/m/acme-plugs/1.0.0/skills/theta/SKILL.md";
const PROJECT_READS = {
  workspace: "/legion/workspaces/acme/proj/p-1/.claude/skills/zeta/SKILL.md",
  relative: ".claude/skills/zeta/SKILL.md",
  home: "/home/dev/.claude/skills/zeta/SKILL.md",
};
function projectWorld(copies: readonly SkillCopiesRow[]): R4Tables {
  return {
    copies: [PLUGS_ZETA, PLUGS_THETA, ...copies],
    installers: [installs(PLUGS, PLUGS_ZETA), installs(PLUGS, PLUGS_THETA)],
    prefixes: [prefixRow("**/plugins/cache/m/acme-plugs/*/skills/theta/", PLUGS_THETA, PLUGS)],
    formerStages: [],
    loaders: [],
    repositories: [repositoryRow("acme/plugs"), repositoryRow("acme/proj")],
  };
}
/** The reads of the project skill: a session that read theta from the plugin's cache (so its
 *  context installs the plugin), and one session each of the three reads of zeta. */
function projectReads() {
  return query("store_a", "2026-11-22", [
    { skill: "theta", copy: THETA_READ, hashes: ["1111111111111111"] },
    { skill: "zeta", copy: PROJECT_READS.workspace, hashes: ["2222222222222222"] },
    { skill: "zeta", copy: PROJECT_READS.relative, hashes: ["3333333333333333"] },
    { skill: "zeta", copy: PROJECT_READS.home, hashes: ["4444444444444444"] },
  ]);
}

// Catches: vskills-293's porting-released-proofs, 9 cases whose read of the project's own
// .claude/skills/porting-released-proofs/SKILL.md went to the plugin that holds a skill of that
// name, by the store's context, so the profile installed that plugin whole (20 of the 21 names
// it added).
test("a read inside a checkout's own .claude/skills is that project's skill: no plugin or other unit's copy of its name takes it, and a home directory's .claude/skills is still a user's", () => {
  const unplaced = attribute(CONFIG, [projectReads()], projectWorld([]));
  const why =
    "a project skill (.claude/skills of a checkout, not of a home directory), which only a copy in such a directory places, so no unit's copy of zeta is it";
  expect(outcomesOf(unplaced, copyPrefix(PROJECT_READS.workspace, "zeta"))).toEqual([
    {
      outcome: `store_a: unattributed: no copy in a project skills directory ends the path; the path is of ${why}`,
      sessions: 1,
    },
  ]);
  expect(outcomesOf(unplaced, copyPrefix(PROJECT_READS.relative, "zeta"))).toEqual([
    {
      outcome: `store_a: unattributed: no copy in a project skills directory ends the path; the path is of ${why}`,
      sessions: 1,
    },
  ]);
  // The plugin's zeta has the user-level read alone; the two project reads are nobody's.
  expect(countOf(unplaced, PLUGS_ZETA)).toMatchObject({ sessions: 1, unattributed: 2 });
  expect(
    unplaced.copies.find((count) => count.repository === "acme/proj")?.sessions
  ).toBeUndefined();
  // Where the copies output lists the project's own copy, both project reads are of it (the home
  // directory's read has no project, and the checkout rule, which names no home, takes it there).
  const listed = attribute(CONFIG, [projectReads()], projectWorld([PROJECT_ZETA]));
  const reading = countOf(listed, PROJECT_ZETA).readSessions;
  expect(reading.has(sessionKey("store_a", "2222222222222222"))).toBe(true);
  expect(reading.has(sessionKey("store_a", "3333333333333333"))).toBe(true);
  expect(countOf(listed, PLUGS_ZETA).readSessions.size).toBe(0);
  expect(outcomesOf(listed, copyPrefix(PROJECT_READS.workspace, "zeta"))).toEqual([
    {
      outcome:
        "store_a: checkout: to acme/proj .claude/skills/zeta: the path ends with .claude/skills/zeta/, its skill directory",
      sessions: 1,
    },
  ]);
});

test("the project skills directories attribute.ts places by are the ones build.ts lists a session repository's project skills from", () => {
  expect([...PROJECT_SKILL_DIRS]).toEqual(PROJECT_SKILLS.flatMap((kind) => kind.dirs));
});

// Catches: R1 on r4/main15 at 4eea3c9f, where /home/<user>/.agents/skills/gh/ (3,263 dev-box
// sessions) tied cli/cli and a second repository's gh, and the default profile's context
// installed neither, though the dev box's own installers install cli/cli's gh (`gh skill install`).
test("a default-profile session of a devbox store has the units the dev box's own installers install, so it breaks a checkout tie in their favour, and a session of another profile has none", () => {
  const GH = copyRow("cli/cli", "skills/gh", "gh");
  const OTHER_GH = copyRow("acme/sessions", "skills/gh", "gh");
  const installer: Unit = {
    unit: "gh-skill:gh",
    manifest_repository: "acme/dotfiles",
    manifest_path: "installers/omp.sh",
    kind: "installer-script",
    action: "installs",
    host: "omp",
  };
  const manifest = (file: string, kind: string) => ({
    repository: "acme/dotfiles",
    path: file,
    kind,
    source: "tree",
    rows: "1",
    says: "",
    error: "",
  });
  const world: R4Tables = {
    copies: [GH, OTHER_GH],
    installers: [installs(installer, GH)],
    prefixes: [],
    formerStages: [],
    loaders: [],
    repositories: [repositoryRow("cli/cli"), repositoryRow("acme/sessions")],
    manifests: [
      manifest("installers/omp.sh", "installer-script"),
      manifest(".claude/settings.json", "claude-settings"),
    ],
  };
  const read = "/home/dev/.agents/skills/gh/SKILL.md";
  const reads = query("store_a", "2026-11-22", [
    { skill: "gh", copy: read, hashes: ["1111111111111111", "2222222222222222"] },
  ]);
  const profiles = new Map([
    [sessionKey("store_a", "1111111111111111"), "default"],
    [sessionKey("store_a", "2222222222222222"), "legion"],
  ]);
  const attribution = attributeByProfile(CONFIG, [reads], world, profiles);
  const tie =
    "the path ends with skills/gh/, the skill directory of 2 repositories (cli/cli, acme/sessions), and no directory of the path names just one of them";
  expect(outcomesOf(attribution, copyPrefix(read, "gh"))).toEqual([
    {
      outcome: `store_a: context: to cli/cli skills/gh: ${tie}, and its context installs that one of them`,
      sessions: 1,
    },
    {
      outcome: `store_a: unattributed: ${tie}, and its context installs none of them`,
      sessions: 1,
    },
  ]);
  expect(countOf(attribution, GH)).toMatchObject({ sessions: 1, unattributed: 1 });
  expect(countOf(attribution, OTHER_GH)).toMatchObject({ sessions: 0, unattributed: 1 });
});

// Catches: the dev-box sessions' reads of verification-skills' plugin cache at its older layout
// (`<marketplace>___dev-infra___*/skills/running-heavy-jobs/`, 719 paths under another dropped
// plugin's directories), unattributed for want of a prefix row: running-heavy-jobs fell from
// 2,218 read sessions to 494. copies.ts now writes a former-path row for each such location.
test("a read at a plugin cache location the marketplace held a skill at and holds no more is counted for the copy git moved it to, under its former directory; one of a skill git removed is unattributed, and its outcome names the former location", () => {
  const MOVED_TOOL = copyRow("acme/skills", "plugins/acme-tools/skills/tool-loop", "tool-loop");
  const GAMMA = copyRow("acme/skills", "plugins/plugin-gamma/skills/gamma-one", "gamma-one");
  const KIT: Unit = {
    unit: "plugin:acme-kit@acme-old",
    manifest_repository: "acme/skills",
    manifest_path: ".claude-plugin/marketplace.json",
    kind: "claude-marketplace",
    action: "ships",
    host: "claude-code",
  };
  const UNTIL_MOVE = "3fe9b7821784751173ab41a052b2e320c6c538fc";
  const cache = (plugin: string, skill: string) =>
    `**/plugins/cache/plugins/acme-old___${plugin}___*/skills/${skill}/`;
  const stage = (
    prefix: string,
    skill: string,
    copy: SkillCopiesRow | undefined,
    renamedTo: string,
    note: string
  ): FormerStagesRow => ({
    prefix,
    script_repository: "acme/skills",
    script_path: ".claude-plugin/marketplace.json",
    package: KIT.unit,
    skill,
    until: UNTIL_MOVE,
    renamed_to: renamedTo,
    repository: copy?.repository ?? "",
    path: copy?.path ?? "",
    note,
  });
  const world: R4Tables = {
    copies: [MOVED_TOOL, GAMMA],
    installers: [installs(plugin("acme-tools", "acme/skills"), MOVED_TOOL)],
    prefixes: [
      { ...prefixRow(cache("acme-kit", "tool-loop"), MOVED_TOOL, KIT) },
      { ...prefixRow(cache("plugin-gamma", "gamma-one"), GAMMA, KIT), name: "gamma-one" },
    ],
    formerStages: [
      stage(cache("acme-kit", "tool-loop"), "tool-loop", MOVED_TOOL, MOVED_TOOL.path, "renamed"),
      stage(cache("plugin-gamma", "gamma-one"), "gamma-one", GAMMA, "", "still there"),
      stage(cache("acme-kit", "gone"), "gone", undefined, "", "git removed it"),
    ],
    loaders: [],
    repositories: [repositoryRow("acme/skills")],
  };
  const base = "/home/dev/.dotfiles/omp/plugins/cache/plugins/acme-old___";
  const moved = `${base}acme-kit___0.2.36/skills/tool-loop/SKILL.md`;
  const stayed = `${base}plugin-gamma___0.1.0/skills/gamma-one/SKILL.md`;
  const removed = `${base}acme-kit___0.2.36/skills/gone/SKILL.md`;
  const reads = query("store_a", "2026-11-22", [
    { skill: "tool-loop", copy: moved, hashes: ["1111111111111111"] },
    { skill: "gamma-one", copy: stayed, hashes: ["2222222222222222"] },
    { skill: "gone", copy: removed, hashes: ["3333333333333333"] },
  ]);
  const attribution = attribute(CONFIG, [reads], world);
  expect(countOf(attribution, MOVED_TOOL)).toMatchObject({
    sessions: 1,
    unattributed: 0,
    formerNameSessions: 1,
  });
  expect(countOf(attribution, GAMMA)).toMatchObject({
    sessions: 1,
    unattributed: 0,
    formerNameSessions: 0,
  });
  expect(outcomesOf(attribution, copyPrefix(removed, "gone"))).toEqual([
    {
      outcome: `store_a: unattributed: no copy's skill directory ends the path, and its context installs no copy of gone; acme/skills's .claude-plugin/marketplace.json named ${KIT.unit} as holding gone here until 3fe9b782, and holds it there no longer; git removed it`,
      sessions: 1,
    },
  ]);
});

// A repository and its fork each hold a copy at skills/dispatch, as cybermonklabs/legion and
// theorem-labs/legion do; a workspace path that names `legion` names both (each repository is
// named legion), so the checkout rule ties. The parent's package ships its dispatch and its
// agent-secrets, whose image path a prefix row maps; the fork's marketplace plugin ships the
// fork's dispatch and brainstorm.
const PARENT_SECRETS = copyRow("org/legion", "skills/agent-secrets", "agent-secrets");
const PARENT_DISPATCH = copyRow("org/legion", "skills/dispatch", "dispatch");
const FORK_DISPATCH = copyRow("fork-org/legion", "skills/dispatch", "dispatch", {
  fork: "yes",
  parent: "org/legion",
});
const FORK_BRAINSTORM = copyRow("fork-org/legion", "skills/brainstorm", "brainstorm", {
  fork: "yes",
  parent: "org/legion",
});
const PARENT_ENVOY: Unit = {
  unit: "package:@org/pi-envoy",
  manifest_repository: "org/legion",
  manifest_path: "packages/pi-envoy/package.json",
  kind: "omp-package",
  action: "ships",
  host: "omp",
};
const FORK_PLUGIN = plugin("claude-envoy", "fork-org/legion");

// Catches: R1 on R4's list of the three owners, where a checkout tie of a fork and its parent
// was unattributed at once and the installer rule never ran. (That count's 49,375 read sessions
// of /opt/legion/pi-envoy/dist/skills/ and /opt/legion/pi-legion/dist/skills/ are no checkout at
// all: the next tests.)
test("a checkout tie goes to the one tied copy the store's context installs, and stays unattributed when its context installs none or both of them", () => {
  const stores = parseConfig({
    ...(JSON.parse(readFileSync(CONFIG_FILE, "utf8")) as object),
    stores: [
      { name: "store_a", context: "legion:ACME" },
      { name: "store_b", context: "legion:BETA" },
      { name: "store_c", context: "legion:GAMMA" },
    ],
  });
  const world: R4Tables = {
    copies: [PARENT_SECRETS, PARENT_DISPATCH, FORK_DISPATCH, FORK_BRAINSTORM],
    installers: [
      installs(PARENT_ENVOY, PARENT_SECRETS),
      installs(PARENT_ENVOY, PARENT_DISPATCH),
      installs(FORK_PLUGIN, FORK_DISPATCH),
      installs(FORK_PLUGIN, FORK_BRAINSTORM),
    ],
    prefixes: [
      prefixRow("/opt/legion/pi-envoy/dist/skills/agent-secrets/", PARENT_SECRETS, PARENT_ENVOY),
      prefixRow(
        "**/plugins/cache/m/claude-envoy/*/skills/brainstorm/",
        FORK_BRAINSTORM,
        FORK_PLUGIN
      ),
    ],
    formerStages: [],
    loaders: [],
    repositories: [
      repositoryRow("org/legion"),
      { ...repositoryRow("fork-org/legion"), fork: "yes", parent: "org/legion" },
    ],
  };
  const secrets = "/opt/legion/pi-envoy/dist/skills/agent-secrets/SKILL.md";
  const dispatch = "/legion/workspaces/someone/legion/w-1/skills/dispatch/SKILL.md";
  const outputs = [
    // store_a's context installs the parent's package alone, through its agent-secrets read.
    query("store_a", "2026-11-22", [
      { skill: "agent-secrets", copy: secrets, hashes: ["5555555555555555"] },
      { skill: "dispatch", copy: dispatch, hashes: ["6666666666666666"] },
    ]),
    // store_b has no step-1 read, so its context installs nothing.
    query("store_b", "2026-11-22", [
      { skill: "dispatch", copy: dispatch, hashes: ["7777777777777777"] },
    ]),
    // store_c's context installs the parent's package and the fork's plugin.
    query("store_c", "2026-11-22", [
      { skill: "agent-secrets", copy: secrets, hashes: ["8888888888888888"] },
      {
        skill: "brainstorm",
        copy: "/home/x/.claude/plugins/cache/m/claude-envoy/1.0.0/skills/brainstorm/SKILL.md",
        hashes: ["aaaaaaaaaaaaaaaa"],
      },
      { skill: "dispatch", copy: dispatch, hashes: ["9999999999999999"] },
    ]),
  ];
  const attribution = attribute(stores, outputs, world);
  const tie =
    "the path ends with skills/dispatch/, the skill directory of 2 repositories (org/legion, fork-org/legion), and no directory of the path names just one of them";
  expect(outcomesOf(attribution, copyPrefix(dispatch, "dispatch"))).toEqual([
    {
      outcome: `store_a: context: to org/legion skills/dispatch: ${tie}, and its context installs that one of them`,
      sessions: 1,
    },
    {
      outcome: `store_b: unattributed: ${tie}, and its context installs none of them`,
      sessions: 1,
    },
    {
      outcome: `store_c: unattributed: ${tie}, and its context installs 2 of them (org/legion skills/dispatch, fork-org/legion skills/dispatch)`,
      sessions: 1,
    },
  ]);
  expect(countOf(attribution, PARENT_DISPATCH)).toMatchObject({
    sessions: 1,
    unattributed: 2,
    ties: [shownCopy(FORK_DISPATCH)],
  });
  expect(countOf(attribution, FORK_DISPATCH)).toMatchObject({ sessions: 0, unattributed: 2 });
  expect(attribution.unattributedSessions).toBe(2);
});

// The worker image's layout (/opt/legion/<package directory>/...), as cybermonklabs/legion's
// image carries pi-legion: the parent's package ships legion-worker and simplify-settled-diff
// (both mapped by image rows), and its prepack script staged ce-simplify-code until a commit of
// the window. The fork holds skills/ce-simplify-code and skills/brainstorm, and another repository
// skills/ce-simplify-code, as theorem-labs/legion and sjawhar/compound-engineering-plugin do.
const PARENT_WORKER = copyRow("org/legion", "skills/legion-worker", "legion-worker");
const PARENT_SIMPLIFY = copyRow(
  "org/legion",
  "skills/simplify-settled-diff",
  "simplify-settled-diff"
);
const FORK_SIMPLIFY = copyRow("fork-org/legion", "skills/ce-simplify-code", "ce-simplify-code", {
  fork: "yes",
  parent: "org/legion",
});
const OTHER_SIMPLIFY = copyRow("other/compound", "skills/ce-simplify-code", "ce-simplify-code");
const PARENT_LEGION: Unit = {
  unit: "package:@org/pi-legion",
  manifest_repository: "org/legion",
  manifest_path: "packages/pi-legion/package.json",
  kind: "omp-package",
  action: "ships",
  host: "omp",
};
const UNTIL = "4c2dc60ce8597f90a5e48aac854f20e907cdc384";
const IMAGE_SIMPLIFY = "/opt/legion/pi-legion/dist/skills/ce-simplify-code/";
/** The former-stages.tsv rows copies.ts writes for ce-simplify-code at each location of the
 *  parent's package, mapped to `copy` (none when undefined) with `note`. */
const formerSimplify = (note: string, copy?: SkillCopiesRow): FormerStagesRow[] =>
  ["**/node_modules/@org/pi-legion/dist/skills/ce-simplify-code/", IMAGE_SIMPLIFY].map(
    (prefix) => ({
      prefix,
      script_repository: "org/legion",
      script_path: "scripts/pi-plugin-prepack.sh",
      package: "@org/pi-legion",
      skill: "ce-simplify-code",
      until: UNTIL,
      renamed_to: copy === undefined ? "" : copy.path,
      repository: copy?.repository ?? "",
      path: copy?.path ?? "",
      note,
    })
  );
function imageWorld(formerStages: readonly FormerStagesRow[], more: PrefixTableRow[] = []) {
  return {
    copies: [PARENT_WORKER, PARENT_SIMPLIFY, FORK_SIMPLIFY, OTHER_SIMPLIFY, FORK_BRAINSTORM],
    installers: [installs(PARENT_LEGION, PARENT_WORKER), installs(PARENT_LEGION, PARENT_SIMPLIFY)],
    prefixes: [
      prefixRow("/opt/legion/pi-legion/dist/skills/legion-worker/", PARENT_WORKER, PARENT_LEGION),
      prefixRow(
        "/opt/legion/pi-legion/dist/skills/simplify-settled-diff/",
        PARENT_SIMPLIFY,
        PARENT_LEGION
      ),
      ...more,
    ],
    formerStages,
    loaders: [],
    repositories: [
      repositoryRow("org/legion"),
      { ...repositoryRow("fork-org/legion"), fork: "yes", parent: "org/legion" },
      repositoryRow("other/compound"),
    ],
  } satisfies R4Tables;
}

// Catches: R1 on R4's list main13, where /opt/legion/pi-legion/dist/skills/ce-simplify-code/
// (1,084 read sessions) went to theorem-labs/legion by the checkout rule, the image's `legion`
// directory taken for the repository's name, and /opt/legion/pi-envoy/dist/skills/
// dispatch-brainstorming/ (19) went to it as its one skill directory: the image's layout is an
// install location, not a checkout of any repository.
test("a read under the worker image's install location that no prefix row maps is no checkout: a skill the parent's package staged only formerly goes to neither the fork nor another repository holding it at skills/<name>, and its outcome names that former stage", () => {
  const removed = `no prefix row maps that path: the tree read holds no SKILL.md at skills/ce-simplify-code, and git removed skills/ce-simplify-code/SKILL.md at ${UNTIL} (2026-11-07T01:36:22Z)`;
  const otherPrefix = prefixRow(
    "**/plugins/cache/m/compound/*/skills/ce-simplify-code/",
    OTHER_SIMPLIFY,
    plugin("compound", OTHER_SIMPLIFY.repository)
  );
  const unknown = "/opt/legion/pi-unknown/dist/skills/ce-simplify-code/";
  const reads = query("store_a", "2026-11-22", [
    {
      skill: "legion-worker",
      copy: "/opt/legion/pi-legion/dist/skills/legion-worker/SKILL.md",
      hashes: ["5555555555555555"],
    },
    { skill: "ce-simplify-code", copy: `${IMAGE_SIMPLIFY}SKILL.md`, hashes: ["6666666666666666"] },
    // This mapped read installs another repository's same-name copy in the store's context.
    {
      skill: "ce-simplify-code",
      copy: "/home/x/.claude/plugins/cache/m/compound/1.0.0/skills/ce-simplify-code/SKILL.md",
      hashes: ["8888888888888888"],
    },
    { skill: "ce-simplify-code", copy: `${unknown}SKILL.md`, hashes: ["9999999999999999"] },
    {
      skill: "brainstorm",
      copy: "/opt/legion/pi-envoy/dist/skills/brainstorm/SKILL.md",
      hashes: ["7777777777777777"],
    },
  ]);
  const attribution = attribute(
    CONFIG,
    [reads],
    imageWorld(formerSimplify(removed), [otherPrefix])
  );
  const image =
    "only the prefix table places reads under the worker image's install location /opt/legion/";
  expect(outcomesOf(attribution, IMAGE_SIMPLIFY)).toEqual([
    {
      outcome: `store_a: unattributed: org/legion's scripts/pi-plugin-prepack.sh staged ce-simplify-code for @org/pi-legion here until 4c2dc60c, and stages it there no longer; ${removed}; ${image}`,
      sessions: 1,
    },
  ]);
  // No former stage names this path, so its outcome says no prefix row maps it.
  expect(outcomesOf(attribution, "/opt/legion/pi-envoy/dist/skills/brainstorm/")).toEqual([
    {
      outcome: `store_a: unattributed: no prefix row maps the path; ${image}`,
      sessions: 1,
    },
  ]);
  // Even without a former-stage note, the image read cannot use the same-name copy in context.
  expect(outcomesOf(attribution, unknown)).toEqual([
    {
      outcome: `store_a: unattributed: no prefix row maps the path; ${image}`,
      sessions: 1,
    },
  ]);
  expect(countOf(attribution, OTHER_SIMPLIFY).readSessions).toEqual(
    new Set([sessionKey("store_a", "8888888888888888")])
  );
  for (const copy of [FORK_SIMPLIFY, FORK_BRAINSTORM])
    expect(countOf(attribution, copy).sessions).toBe(0);
  expect(countOf(attribution, PARENT_WORKER).sessions).toBe(1);
});

// Catches: a read of /opt/legion/pi-legion/dist/skills/ce-simplify-code/, the path
// cybermonklabs/legion's prepack staged its skill at until #2896 renamed it simplify-settled-diff
// (4c2dc60c), counted for no copy, or for the renamed copy with nothing to tell those reads from
// the reads of its new name.
test("a unique former-stage image path counts for the renamed copy under its former name, but the store's context cannot resolve a prefix-table tie", () => {
  const renamed = `git renamed skills/ce-simplify-code/SKILL.md to skills/simplify-settled-diff/SKILL.md at ${UNTIL} (2026-11-07T01:36:22Z), so the prefix rows **/node_modules/@org/pi-legion/dist/skills/ce-simplify-code/, ${IMAGE_SIMPLIFY} map it to org/legion skills/simplify-settled-diff under its former name ce-simplify-code`;
  const formerRow = {
    ...prefixRow(IMAGE_SIMPLIFY, PARENT_SIMPLIFY, PARENT_LEGION),
    name: "ce-simplify-code",
  };
  const builtPlugin = "/opt/legion/claude-envoy/legion/skills/ce-simplify-code/";
  const reads = query("store_a", "2026-11-22", [
    { skill: "ce-simplify-code", copy: `${IMAGE_SIMPLIFY}SKILL.md`, hashes: ["5555555555555555"] },
    {
      skill: "simplify-settled-diff",
      copy: "/opt/legion/pi-legion/dist/skills/simplify-settled-diff/SKILL.md",
      hashes: ["6666666666666666"],
    },
    // The plugin the build script generated from the same stages: no prefix row maps it.
    { skill: "ce-simplify-code", copy: `${builtPlugin}SKILL.md`, hashes: ["7777777777777777"] },
  ]);
  const world = imageWorld(formerSimplify(renamed, PARENT_SIMPLIFY), [formerRow]);
  const attribution = attribute(CONFIG, [reads], world);
  expect(attribution.mappedPrefixes.get(IMAGE_SIMPLIFY)).toEqual(
    new Set([shownCopy(PARENT_SIMPLIFY)])
  );
  // The rename is evidence about the matched path, not an alias for every read of its old name.
  expect(outcomesOf(attribution, builtPlugin)?.[0]?.outcome).toContain(
    "unattributed: no prefix row maps the path"
  );
  expect(countOf(attribution, PARENT_SIMPLIFY)).toMatchObject({
    sessions: 2,
    formerNameSessions: 1,
    formerNames: ["ce-simplify-code"],
  });
  for (const copy of [FORK_SIMPLIFY, OTHER_SIMPLIFY])
    expect(countOf(attribution, copy).sessions).toBe(0);
  expect(
    parseTsv(readCountTsv(attribution))
      .filter((row) => row.path === PARENT_SIMPLIFY.path)
      .map((row) => [row.sessions, row.former_name_sessions])
  ).toEqual([["2", "1"]]);
  const ambiguous = attribute(
    CONFIG,
    [reads],
    imageWorld(world.formerStages, [
      formerRow,
      prefixRow(IMAGE_SIMPLIFY, FORK_SIMPLIFY, FORK_PLUGIN),
    ])
  );
  expect(outcomesOf(ambiguous, IMAGE_SIMPLIFY)?.[0]?.outcome).toContain(
    "unattributed: prefix rows match 2 copies"
  );
  expect(countOf(ambiguous, PARENT_SIMPLIFY)).toMatchObject({
    sessions: 1,
    formerNameSessions: 0,
  });
  expect(countOf(ambiguous, FORK_SIMPLIFY).sessions).toBe(0);
  // Without a former-stages.tsv row of a rename, the prefix still maps to the same copy, but
  // does not mark the read as using a former name.
  const unmarked = attribute(CONFIG, [reads], imageWorld([], [formerRow]));
  expect(countOf(unmarked, PARENT_SIMPLIFY)).toMatchObject({
    sessions: 2,
    formerNameSessions: 0,
    formerNames: [],
  });
  expect(outcomesOf(unmarked, builtPlugin)?.map((item) => item.outcome)).toEqual([
    "store_a: unattributed: no prefix row maps the path; only the prefix table places reads under the worker image's install location /opt/legion/",
  ]);
});

// The prefix rows copies.ts gives a `github:` dependency of a plugins package.json and a pool an
// `ensure_vendor` clone holds, from made-up manifests in the shape of a dotfiles repository's
// (acme/dots) over made-up copies: acme/powers' power-one, acme/pack's pack-one, and a copy of
// power-one in acme/skills, which a read of the dependency's must not be counted for.
test("a read under node_modules/<package>/skills/<name>/ of a github: dependency, or under vendor/<pool>/skills/<name>/ of a cloned pool, goes to the pinned copy by the prefix table when its repository is a target", () => {
  const manifests = [
    madeManifest(
      "acme/dots",
      "omp/plugins/package.json",
      "omp-plugins",
      JSON.stringify({
        name: "omp-plugins",
        dependencies: { "acme-powers": "github:acme/powers#v1" },
      })
    ),
    madeManifest(
      "acme/powers",
      "package.json",
      "omp-package",
      JSON.stringify({ name: "powers-of-acme", pi: { skills: ["./skills"] } })
    ),
    madeManifest(
      "acme/dots",
      "skills-sources.json",
      "omp-skill-sources",
      JSON.stringify({ sources: [{ root: "vendor/acme-pack" }] })
    ),
    madeManifest(
      "acme/dots",
      "omp/extensions/skill-pools.ts",
      "omp-extension",
      'export const manifest = readFileSync("skills-sources.json", "utf8");\n'
    ),
    madeManifest(
      "acme/dots",
      "installers/omp.sh",
      "installer-script",
      [
        'ln -sfn "$REPO_DIR/omp/extensions/skill-pools.ts" "$HOME/.omp/agent/extensions/skill-pools.ts"',
        "ensure_vendor https://github.com/acme/pack.git acme-pack v2",
        "",
      ].join("\n")
    ),
  ];
  const copyAt = (repository: string, dir: string): SkillCopy => ({
    repository,
    path: dir,
    file: `${dir}/SKILL.md`,
    blob: "",
    fork: false,
    archived: false,
    parent: "",
    name: path.posix.basename(dir),
    nameFrom: "frontmatter",
    description: "Use when a made-up skill is needed.",
    descriptionBytes: 35,
    fault: "",
  });
  const copies = [
    copyAt("acme/pack", "skills/pack-one"),
    copyAt("acme/powers", "skills/power-one"),
    copyAt("acme/skills", "skills/power-one"),
  ];
  const resolved = resolveInstallers(manifests, new CopyIndex(copies));
  const targets = new Set(["acme/pack", "acme/powers", "acme/skills"]);
  const world: R4Tables = {
    copies: copies.map((copy) => copyRow(copy.repository, copy.path, copy.name)),
    installers: resolved.rows.map((row) => ({
      manifest_repository: row.manifestRepository,
      manifest_path: row.manifestPath,
      kind: row.kind,
      action: row.action,
      unit: row.unit,
      host: row.host,
      skill: row.skill,
      copy_repository: row.copyRepository,
      copy_path: row.copyPath ?? "",
      note: row.note,
    })),
    prefixes: resolved.prefixes
      .filter((row) => targets.has(row.repository))
      .map((row) => ({ ...row })),
    formerStages: [],
    loaders: [],
    repositories: [...targets, "acme/dots"].map(repositoryRow),
  };
  const powersRead =
    "/home/acme/.dotfiles/omp/plugins/node_modules/acme-powers/skills/power-one/SKILL.md";
  const packRead = "/home/acme/.dotfiles/vendor/acme-pack/skills/pack-one/SKILL.md";
  const reads = query("store_a", "2026-11-22", [
    { skill: "power-one", copy: powersRead, hashes: [H4] },
    { skill: "pack-one", copy: packRead, hashes: [H4] },
  ]);
  const attribution = attribute(CONFIG, [reads], world);
  expect(attribution.mappedPrefixes.get(copyPrefix(powersRead, "power-one"))).toEqual(
    new Set(["acme/powers skills/power-one"])
  );
  expect(attribution.mappedPrefixes.get(copyPrefix(packRead, "pack-one"))).toEqual(
    new Set(["acme/pack skills/pack-one"])
  );
  const sessions = (repository: string, dir: string) =>
    attribution.copies.find((count) => count.repository === repository && count.path === dir)
      ?.sessions;
  expect(sessions("acme/powers", "skills/power-one")).toBe(1);
  expect(sessions("acme/pack", "skills/pack-one")).toBe(1);
  // Not counted for the same-named copy of another repository.
  expect(sessions("acme/skills", "skills/power-one")).toBe(0);
});

test("the checkout rule takes the longest skill directory, and names no directory for skill://", () => {
  const decide = checkoutRule([
    { repository: "acme/skills", path: "skills/alpha" },
    { repository: "acme/dotfiles", path: "plugins/acme/skills/alpha" },
  ]);
  expect(decide("/home/acme/.dotfiles/plugins/acme/skills/alpha/")?.copy).toBe(
    "acme/dotfiles\tplugins/acme/skills/alpha"
  );
  expect(decide("/srv/checkout/skills/alpha/")?.copy).toBe("acme/skills\tskills/alpha");
  expect(decide("skills/alpha/")?.copy).toBe("acme/skills\tskills/alpha");
  expect(decide("/srv/checkout/myskills/alpha/")).toBeUndefined();
  expect(decide("skill://alpha/")).toBeUndefined();
});

test.each([
  ["at most the name's unattributed sessions apart is a tie", [], 1, true],
  [
    "one more apart is not",
    [
      {
        skill: "beta",
        copy: `${F3}/config/plugins/cache/acme-market/acme-tools/0.2.0/skills/beta/SKILL.md`,
        hashes: [H4],
      },
    ],
    2,
    false,
  ],
] as const)("two copies of one name %s", (_name, extra, toolsSessions, tie) => {
  const outputs = [F7_FIRST(), query("store_a", "2026-11-21", extra)];
  const attribution = attribute(CONFIG, outputs, f7World());
  // beta: acme/skills' copy 0 sessions, acme/tools' 1 (or 2); H3's read is unattributed.
  expect(countOf(attribution, TOOLS_BETA)).toMatchObject({
    sessions: toolsSessions,
    unattributed: 1,
  });
  expect(countOf(attribution, BETA).ties).toEqual(tie ? [shownCopy(TOOLS_BETA)] : []);
  expect(countOf(attribution, TOOLS_BETA).ties).toEqual(tie ? [shownCopy(BETA)] : []);
  // alpha has one copy, so it ties with none.
  expect(countOf(attribution, ALPHA).ties).toEqual([]);
});

test("a file hash seen in two queries of a store counts once, and in two stores twice", () => {
  // H1 resumed: its file is in the day query of 2026-11-20 and again in the next day's.
  const again = query("store_a", "2026-11-21", [
    { skill: "alpha", copy: `${F1_PROFILE}/alpha/SKILL.md`, hashes: [H1] },
  ]);
  expect(countOf(attribute(CONFIG, [F7_FIRST(), again], f7World()), ALPHA).sessions).toBe(2);
  const otherStore = query("store_b", "2026-11-20", [
    {
      skill: "alpha",
      copy: `/home/acme/.omp/profiles/fixture/agent/skills/alpha/SKILL.md`,
      hashes: [H1],
    },
  ]);
  const attribution = attribute(TWO_STORES, [F7_FIRST(), again, otherStore], f7World());
  expect(countOf(attribution, ALPHA).sessions).toBe(3);
  expect(attribution.stores.map((store) => store.readSessions)).toEqual([3, 1]);
  // F7-count-second's session, F1b, is F1's session with its alpha read copied a day later: it
  // holds each read of F1 (the same call ids and timestamps) and a read of its own. So F1's reads
  // count in F1b, whose hash sorts first, and the alpha sessions are F1b's and F4's, not F1's.
  const second = F7_SECOND();
  expect(second.rows.map((row) => row.hashes)).toEqual([[H1B], [H1B]]);
  expect(H1B < H1).toBe(true);
  expect(countOf(attribute(CONFIG, [F7_FIRST(), second], f7World()), ALPHA).readSessions).toEqual(
    new Set([sessionKey("store_a", H2), sessionKey("store_a", H1B)])
  );
});

// Forks of one session, each holding a copy of its one alpha read (PARENT_KEY), each on its own
// mtime day and so in its own query: H3 the session (its hash sorts first), H1 a fork with the
// copy only, H4 a fork with a read of its own as well (OWN_KEY), and H2 a fork whose copy of the
// call has no result in it.
const PARENT_KEY = "a0a0a0a0a0a0a0a0";
const OWN_KEY = "b1b1b1b1b1b1b1b1";
const ALPHA_PATH = `${F1_PROFILE}/alpha/SKILL.md`;
const forkQueries = () => [
  query("store_a", "2026-11-20", [
    { skill: "alpha", copy: ALPHA_PATH, hashes: [H3], calls: { [H3]: [PARENT_KEY] } },
  ]),
  query("store_a", "2026-11-21", [
    { skill: "alpha", copy: ALPHA_PATH, hashes: [H1], calls: { [H1]: [PARENT_KEY] } },
  ]),
  query("store_a", "2026-11-22", [
    { skill: "alpha", copy: ALPHA_PATH, hashes: [H4], calls: { [H4]: [PARENT_KEY, OWN_KEY] } },
  ]),
  query("store_a", "2026-11-23", [
    {
      skill: "alpha",
      copy: "skill://alpha",
      hashes: [],
      unpaired: 1,
      unpaired_hashes: [H2],
      unpaired_calls: { [H2]: [PARENT_KEY] },
    },
  ]),
];

test("a read that files of two queries hold counts in the file whose hash sorts first: a fork with the copy only, its call answered or not, is no session, and one with a read of its own is", () => {
  const outputs = forkQueries();
  const counted = countOnce(outputs);
  expect(
    counted.outputs.map((output) =>
      output.rows.map((row) => [row.files, row.hashes, row.unpaired, row.unpaired_hashes])
    )
  ).toEqual([[[1, [H3], 0, []]], [[0, [], 0, []]], [[1, [H4], 0, []]], [[0, [], 0, []]]]);
  // The first query's rows lose no file, and come back as they are.
  expect(counted.outputs[0]).toBe(outputs[0]);
  // Each row keeps its calls, so the rule applied again gives the same rows.
  expect(counted.outputs[1]?.rows[0]?.calls).toEqual({ [H1]: [PARENT_KEY] });
  expect(countOnce(counted.outputs)).toEqual(counted);
  // Four files of four queries hold one key: six pairs of them.
  expect(counted.acrossQueries.get("store_a")).toEqual({ keys: 1, pairs: 6 });
  const attribution = attribute(CONFIG, outputs, f7World());
  expect(countOf(attribution, ALPHA).sessions).toBe(2);
  expect(attribution.stores[0]).toMatchObject({
    readSessions: 2,
    unpairedSessions: 0,
    sharedCallIds: 0,
    sharedAcrossQueries: 1,
    filePairsAcrossQueries: 6,
  });
  // The rule holds inside each store: another store's file that lists the key is its session.
  const otherStore = query("store_b", "2026-11-20", [
    { skill: "alpha", copy: ALPHA_PATH, hashes: [H1], calls: { [H1]: [PARENT_KEY] } },
  ]);
  const twoStores = attribute(TWO_STORES, [...outputs, otherStore], f7World());
  expect(
    twoStores.stores.map((store) => [
      store.readSessions,
      store.sharedAcrossQueries,
      store.filePairsAcrossQueries,
    ])
  ).toEqual([
    [2, 1, 6],
    [1, 0, 0],
  ]);
});

test("rows no key of which files of two queries hold come back as they are", () => {
  const outputs = [
    F7_FIRST(),
    query("store_a", "2026-11-21", [{ skill: "alpha", copy: ALPHA_PATH, hashes: [H1, H4] }]),
  ];
  const counted = countOnce(outputs);
  expect(counted.outputs.map((output, at) => output === outputs[at])).toEqual([true, true]);
  expect(counted.acrossQueries.get("store_a")).toEqual({ keys: 0, pairs: 0 });
});

test("a count whose rows name no calls, made by an earlier count.sql, fails the command (exit 1), naming the field", () => {
  const earlier = psqlTable(
    psqlRows(f7("F7-count-first.txt")).map((value) => {
      const { calls: _calls, unpaired_calls: _unpaired, ...row } = value as Record<string, unknown>;
      return row;
    })
  );
  const fault =
    'the count query store_a 2026-11-20: row 1 has no "calls" and no "unpaired_calls" field, so an earlier count.sql made it';
  expect(() => parseQueryOutput("store_a", "2026-11-20", earlier)).toThrow(fault);
  const root = path.join(dir, "earlier-count");
  const countDir = tree(path.join(root, "count"), {
    "store_a/2026-11-20.txt": earlier,
    "store_a/2026-11-21.txt": f7("F7-count-second.txt"),
    "run.log": "",
  });
  expect(() => readCount(countDir, ["store_a"])).toThrow(fault);
  const result = run([
    "--config",
    CONFIG_FILE,
    "--count",
    countDir,
    "--copies",
    r4Dir(path.join(root, "copies"), f7World()),
    "--spans",
    path.join(tree(root, { "spans.jsonl": "" }), "spans.jsonl"),
    "--out",
    path.join(root, "out"),
  ]);
  expect(result.code).toBe(1);
  expect(result.err).toContain(fault);
  expect(existsSync(path.join(root, "out"))).toBe(false);
});

// The hosts of a copy. Oh My Pi installs it when an Oh My Pi session read it, when a store's
// context installs its unit, or when an installers.tsv row of host omp installs it; Claude Code
// when a row of host claude-code does.
const CACHE = "/home/acme/.omp/profiles/default/plugins/cache/plugins";
const DELTA = copyRow("acme/cc", "plugins/cc/skills/delta", "delta");
const EPSILON = copyRow("acme/cc", "plugins/cc2/skills/epsilon", "epsilon");
const ZETA = copyRow("acme/cc", "plugins/cc3/skills/zeta", "zeta");
const ETA = copyRow("acme/cc", "plugins/cc4/skills/eta", "eta");
const THETA = copyRow("acme/cc", "plugins/cc5/skills/theta", "theta");
const IOTA = copyRow("acme/cc", "plugins/cc5/skills/iota", "iota");
const KAPPA = copyRow("acme/skills", "skills/kappa", "kappa");

function hostsWorld(): R4Tables {
  return {
    copies: [DELTA, EPSILON, ZETA, ETA, THETA, IOTA, KAPPA],
    installers: [
      installs(plugin("cc"), DELTA),
      installs(plugin("cc2"), EPSILON),
      installs(plugin("cc3"), ZETA),
      installs(plugin("cc4"), ETA),
      installs(USER_SKILLS, ETA),
      installs(plugin("cc5"), THETA),
      installs(plugin("cc5"), IOTA),
    ],
    prefixes: [
      prefixRow("**/plugins/cache/plugins/m___cc2___*/skills/epsilon/", EPSILON, plugin("cc2")),
      prefixRow("**/plugins/cache/plugins/m___cc3___*/skills/zeta/", ZETA, plugin("cc3")),
      prefixRow("**/plugins/cache/plugins/m___cc5___*/skills/iota/", IOTA, plugin("cc5")),
    ],
    formerStages: [],
    loaders: [],
    repositories: [repositoryRow("acme/cc"), repositoryRow("acme/skills")],
  };
}

test.each([
  [
    "only a Claude Code plugin installs it, and no Oh My Pi session read it",
    DELTA,
    "not countable yet",
  ],
  [
    "only a Claude Code plugin installs it, and an Oh My Pi session read one of its references",
    EPSILON,
    "no Oh My Pi read; Claude Code not counted yet",
  ],
  ["only a Claude Code plugin installs it, and an Oh My Pi session read it", ZETA, "read"],
  [
    "a Claude Code plugin and an Oh My Pi installer install it, and no session read it",
    ETA,
    "no Oh My Pi read; Claude Code not counted yet",
  ],
  [
    "a Claude Code plugin installs it, and Oh My Pi sessions read another skill of that plugin",
    THETA,
    "no Oh My Pi read; Claude Code not counted yet",
  ],
  ["no installer installs it, and no session read it", KAPPA, "no read"],
] as const)("%s: %s", (_name, copy, status) => {
  const reads = query("store_a", "2026-11-20", [
    {
      skill: "epsilon",
      copy: `${CACHE}/m___cc2___1.0.0/skills/epsilon/references/r.md`,
      kind: "reference",
      hashes: [H1],
    },
    { skill: "zeta", copy: `${CACHE}/m___cc3___1.0.0/skills/zeta/SKILL.md`, hashes: [H2] },
    { skill: "iota", copy: `${CACHE}/m___cc5___1.0.0/skills/iota/SKILL.md`, hashes: [H3] },
  ]);
  expect(countOf(attribute(CONFIG, [reads], hostsWorld()), copy).status).toBe(status);
});

test("a skill that an agent definition, a loader list, code or pi-envoy loads is marked loaded", () => {
  const copies = [
    copyRow("acme/skills", "skills/reviewer", "reviewer"),
    copyRow("acme/skills", "skills/tree-tool", "tree-tool"),
    copyRow("acme/skills", "skills/coded", "coded"),
    copyRow("acme/skills", "skills/dispatch-first", "dispatch-first"),
    copyRow("acme/skills", "skills/plain", "plain", {
      installers: "ships package:acme-envoy (acme/skills:packages/acme-envoy/package.json)",
    }),
  ];
  const [reviewer, plain] = [copies[0], copies[4]];
  if (reviewer === undefined || plain === undefined) throw new Error("no copies");
  const world: R4Tables = {
    copies,
    prefixes: [],
    formerStages: [],
    installers: [
      installs(
        {
          unit: "agent:reviewer",
          manifest_repository: "acme/skills",
          manifest_path: "agents/reviewer.md",
          kind: "agent-definition",
          action: "loads",
          host: "omp",
        },
        reviewer
      ),
      {
        unit: "loader:tree/init.py#list",
        manifest_repository: "acme/tree",
        manifest_path: "tree/init.py",
        kind: "python-loader",
        action: "loads",
        host: "any",
        skill: "tree-tool",
        copy_repository: "",
        copy_path: "",
        note: "no copy of this name was read",
      },
      installs(
        {
          unit: "package:acme-envoy",
          manifest_repository: "acme/skills",
          manifest_path: "packages/acme-envoy/package.json",
          kind: "omp-package",
          action: "ships",
          host: "omp",
        },
        plain
      ),
    ],
    loaders: [
      {
        repository: "acme/app",
        path: "src/skills.ts",
        skill: "coded",
        match: "quoted-string,code-load",
        source: "search",
      },
      {
        repository: "acme/app",
        path: "README.md",
        skill: "plain",
        match: "skill-uri",
        source: "search",
      },
    ],
    repositories: [repositoryRow("acme/skills")],
  };
  const attribution = attribute(CONFIG, [], world);
  expect(attribution.copies.map((copy) => [copy.name, copy.loadedBy])).toEqual([
    ["reviewer", ["acme/skills:agents/reviewer.md (agent-definition agent:reviewer)"]],
    ["tree-tool", ["acme/tree:tree/init.py (python-loader loader:tree/init.py#list)"]],
    ["coded", ["acme/app:src/skills.ts (code-load, search)"]],
    [
      "dispatch-first",
      [
        "pi-envoy injects it into each session that has Dispatch (packages/envoy-client/src/dispatch-first.ts)",
      ],
    ],
    ["plain", []],
  ]);
});

test("the prefix table class inventory: each prefix in the totals is mapped or listed with the rule that placed it", () => {
  const outputs = [
    F7_FIRST(),
    F7_SECOND(),
    query("store_a", "2026-11-22", [
      { skill: "alpha", copy: "skill://alpha/SKILL.md:1-40", hashes: [H4] },
      { skill: "alpha", copy: "skill://alpha/references/y.md", kind: "reference", hashes: [H4] },
      { skill: "alpha", copy: "src/alpha/SKILL.md", hashes: [H4] },
      { skill: "beta", copy: "skills/beta/SKILL.md", hashes: [H4] },
      {
        skill: "gamma",
        copy: "https://example.com/acme/skills/main/skills/gamma/SKILL.md",
        hashes: [H4],
      },
      // A call with no result: no read, so in no total.
      {
        skill: "beta",
        copy: "skill://beta",
        files: 0,
        hashes: [],
        unpaired: 1,
        unpaired_hashes: [H3],
      },
    ]),
    query("store_b", "2026-11-20", [
      {
        skill: "beta",
        copy: `${F3}/config/plugins/cache/acme-market/acme-tools/9/skills/beta/SKILL.md`,
        hashes: [H2],
      },
      { skill: "beta", copy: `${F1_PROFILE}/beta/SKILL.md`, hashes: [H2] },
    ]),
  ];
  const attribution = attribute(TWO_STORES, outputs, f7World());
  const listed = new Set(attribution.unmapped.map((item) => item.prefix));
  const inTotals = outputs.flatMap((output) =>
    output.rows.filter((row) => row.hashes.length > 0).map((row) => copyPrefix(row.copy, row.skill))
  );
  expect(
    inTotals.filter((prefix) => !attribution.mappedPrefixes.has(prefix) && !listed.has(prefix))
  ).toEqual([]);
  // And each unmapped prefix's sessions all went somewhere, by a named rule or unattributed.
  for (const item of attribution.unmapped) {
    const went = item.outcomes.reduce((sum, outcome) => sum + outcome.sessions, 0);
    expect(went).toBeGreaterThanOrEqual(Math.max(item.sessions, item.referenceSessions));
    for (const { outcome } of item.outcomes)
      expect(outcome).toMatch(/^store_[ab]: (checkout|context|unattributed): /);
  }
  expect(listed).toEqual(
    new Set([
      `${F3}/marketplace/plugins/acme-tools/skills/alpha/`,
      `${F1_PROFILE}/beta/`,
      "skill://alpha/",
      "src/alpha/",
      "skills/beta/",
      "https://example.com/acme/skills/main/skills/gamma/",
    ])
  );
  expect(outcomesOf(attribution, "src/alpha/")).toEqual([
    {
      outcome:
        "store_a: checkout: to acme/skills src/alpha: the path ends with src/alpha/, its skill directory",
      sessions: 1,
    },
  ]);
  expect(attribution.namesWithoutCopy).toEqual([
    { name: "gamma", sessions: 1, referenceSessions: 0 },
  ]);
  expect(attribution.stores[0]?.unpairedSessions).toBe(1);
});

test.each([
  ["skill://alpha", "alpha", "skill://alpha/"],
  ["skill://alpha/references/x.md:raw", "alpha", "skill://alpha/"],
  ["/opt/acme/skills/alpha/SKILL.md", "alpha", "/opt/acme/skills/alpha/"],
  ["/opt/acme/skills/alpha/SKILL.md:1-40", "alpha", "/opt/acme/skills/alpha/"],
  ["/opt/acme/skills/alpha/references/deep/x.md", "alpha", "/opt/acme/skills/alpha/"],
  ["/opt/alpha/skills/alpha/SKILL.md", "alpha", "/opt/alpha/skills/alpha/"],
  ["skills/alpha/SKILL.md", "alpha", "skills/alpha/"],
  ["/opt/acme/skills/alpha-dir/SKILL.md", "alpha", "/opt/acme/skills/alpha-dir/"],
  ["/opt/acme/skills/alpha-dir/references/x.md", "alpha", "/opt/acme/skills/alpha-dir/"],
])("the copy prefix of %s (skill %s) is %s", (copyPath, skill, prefix) => {
  expect(copyPrefix(copyPath, skill)).toBe(prefix);
});

// The models of the reading sessions.

test("a session's model comes from its file's head: model_change, a subagent's session_init, Claude Code's lines, or none", () => {
  const outputs = [
    parseModelOutput("store_a", "2026-11-20", f7("F7-models-first.txt")),
    parseModelOutput("store_a", "2026-11-20", f7("F7-models-second.txt")),
    parseModelOutput("store_a", "2026-11-21", f7("F7-models-later.txt")),
  ];
  expect(
    outputs.flatMap((output) => output.rows.map((row) => [row.hash, row.model, row.source]))
  ).toEqual([
    [H3, "anthropic/claude-sonnet-5-5", "model_change"],
    [H2, "claude-sonnet-5-5", "assistant"],
    [H1, "anthropic/claude-sonnet-5-5", "model_change"],
    [H1D, null, null],
    [H1C, "anthropic/claude-sonnet-5-5", "session_init"],
    [H1B, "anthropic/claude-sonnet-5-5", "model_change"],
  ]);
  const { models, conflicts } = sessionModels(outputs, CONFIG.servedModels);
  const sonnet = { model: "anthropic/claude-sonnet-5-5", anthropic: true, served: true };
  expect(Object.fromEntries(models)).toEqual({
    [sessionKey("store_a", H1)]: sonnet,
    [sessionKey("store_a", H2)]: sonnet,
    [sessionKey("store_a", H3)]: sonnet,
    [sessionKey("store_a", H1C)]: sonnet,
    [sessionKey("store_a", H1D)]: null,
    [sessionKey("store_a", H1B)]: sonnet,
  });
  expect(conflicts).toBe(0);
});

test.each([
  [
    "a model of Anthropic's family under a gateway's name",
    "acme-gateway/claude-sonnet-5-5",
    true,
    true,
  ],
  [
    "a model of Anthropic's family the gateway does not serve",
    "anthropic/claude-opus-5",
    true,
    false,
  ],
  ["Claude Code's bare model id", "claude-sonnet-5-5", true, true],
  ["another family's model the gateway serves, by its id", "acme-gateway/gpt-6.1-sol", false, true],
  ["another family's model the gateway does not serve", "openai/gpt-6.1:high", false, false],
] as const)("%s: %s is Anthropic's %s, served %s", (_name, model, anthropic, served) => {
  const ids = new Set(CONFIG.servedModels.map(modelId));
  const source = model.includes("/") ? "model_change" : "assistant";
  expect(sessionModel({ model, source }, ids)).toMatchObject({ anthropic, served });
});

test("sessions by model: the thinking suffix dropped, a file in two queries once, Anthropic's family and the gateway's models counted, a model none names and a session no query holds apart", () => {
  const outputs = [
    parseModelOutput(
      "store_a",
      "2026-11-20",
      modelsText("store_a", [
        [H1, "openai/gpt-6.1:high", "model_change"],
        [H2, "anthropic/claude-sonnet-5-5", "session_init"],
        [H3, "openai/gpt-6.1-sol", "model_change"],
        [H1B, null, null],
      ])
    ),
    parseModelOutput(
      "store_a",
      "2026-11-21",
      modelsText("store_a", [
        // H1 resumed and named another model: the first day's stays, and it is counted.
        [H1, "anthropic/claude-sonnet-5-5", "model_change"],
        // H1B's head names a model on its second day: Anthropic's, under a gateway's name.
        [H1B, "acme-gateway/claude-opus-5", "model_change"],
      ])
    ),
  ];
  const models = sessionModels(outputs, CONFIG.servedModels);
  expect(models.conflicts).toBe(1);
  const count = modelCount(
    new Set([H1, H2, H3, H1B, H4].map((hash) => sessionKey("store_a", hash))),
    models
  );
  const model = (name: string, anthropic: boolean, served: boolean) => ({
    model: name,
    anthropic,
    served,
    sessions: 1,
  });
  expect(count).toEqual({
    sessions: 5,
    byModel: [
      model("acme-gateway/claude-opus-5", true, false),
      model("anthropic/claude-sonnet-5-5", true, true),
      model("openai/gpt-6.1", false, false),
      model("openai/gpt-6.1-sol", false, true),
    ],
    noModel: 0,
    notQueried: 1,
    notAnthropic: 2,
    served: 2,
    withModel: 4,
  });
  expect(notAnthropicShare(count)).toBe("0.500");
  expect(notAnthropicShare(modelCount(new Set(), models))).toBe("");
});

test("a models output that is not a whole models.sql result is refused", () => {
  const whole = modelsText("store_a", [[H1, "anthropic/claude-sonnet-5-5", "model_change"]]);
  expect(() => parseModelOutput("store_b", "2026-11-20", whole)).toThrow(
    'row 1 is of the store "store_a"'
  );
  expect(() =>
    parseModelOutput("store_a", "2026-11-20", whole.replace('"with_model":1', '"with_model":0'))
  ).toThrow("the summary counts 1 files, 0 with a model; the rows are 1, 1 with a model");
  expect(() =>
    parseModelOutput(
      "store_a",
      "2026-11-20",
      modelsText("store_a", [[H1, "anthropic/claude-sonnet-5-5", null]])
    )
  ).toThrow("a model needs its source");
  expect(() => parseModelOutput("store_a", "2026-11-20", psqlTable([]))).toThrow("no summary row");
});

/** Writes `files` (path below `root` → text) and returns `root`. */
function tree(root: string, files: Readonly<Record<string, string>>): string {
  for (const [file, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(root, file)), { recursive: true });
    writeFileSync(path.join(root, file), text);
  }
  return root;
}

test("a failed query in the run log is listed, with a split day's missing hours and a day not run", () => {
  const config = parseConfig({
    ...(JSON.parse(readFileSync(CONFIG_FILE, "utf8")) as object),
    windowEnd: "2026-11-23T00:00:00Z",
  });
  const hours = Object.fromEntries(
    Array.from({ length: 24 }, (_, hour) => String(hour).padStart(2, "0"))
      .filter((hour) => hour !== "05")
      .map((hour) => [`store_a/2026-11-20T${hour}.txt`, queryText("store_a", "2026-11-20", [])])
  );
  const at = (minute: number) => `2026-11-22T03:${String(minute).padStart(2, "0")}:00Z`;
  const log = [
    "START 2026-11-22T03:00:00Z runner=/scratch/runner",
    `${at(1)}\tstore_a\t2026-11-20\trc=3\tseconds=900.0\tpsql:<stdin>:1: ERROR:  canceling statement due to statement timeout`,
    ...Array.from({ length: 24 }, (_, hour) =>
      hour === 5
        ? `${at(2)}\tstore_a\t2026-11-20T05\trc=3\tseconds=900.0\tpsql:<stdin>:1: ERROR:  canceling statement due to statement timeout`
        : `${at(2)}\tstore_a\t2026-11-20T${String(hour).padStart(2, "0")}\trc=0\tseconds=1.0\t`
    ),
    "FAILED store_a 2026-11-20 hour 5",
    `${at(3)}\tstore_a\t2026-11-21\trc=2\tseconds=1.0\tpsql: error: connection to server failed`,
    "FAILED store_a 2026-11-21 rc=2",
    "",
  ].join("\n");
  const countDir = tree(path.join(dir, "issues-count"), {
    ...hours,
    "store_a/roots.txt": fixtureRoots(),
    "store_a/2026-11-21.err": "psql: error: connection to server failed\n",
    "store_a/2026-11-22.tmp": "",
    "run.log": log,
  });
  const count = readCount(countDir, ["store_a"]);
  expect(count.outputs).toHaveLength(23);
  expect(queryIssues(config, count)).toEqual([
    {
      store: "store_a",
      name: "2026-11-20",
      what: "split",
      detail:
        "timed out, so it was split into hours: 23 of 24 hour queries have output; hours 05 have none",
    },
    {
      store: "store_a",
      name: "2026-11-20T05",
      what: "failed",
      detail: "rc=3: psql:<stdin>:1: ERROR:  canceling statement due to statement timeout",
    },
    {
      store: "store_a",
      name: "2026-11-21",
      what: "failed",
      detail: "rc=2: psql: error: connection to server failed",
    },
    {
      store: "store_a",
      name: "2026-11-22",
      what: "no output",
      detail: "the run log has no line for it: not run yet",
    },
  ]);
  expect(parseRunLog(log).ended).toBeUndefined();
});

test("an output that is not a whole count.sql result is listed, and one of another store refused", () => {
  const countDir = tree(path.join(dir, "unreadable-count"), {
    "store_a/2026-11-20.txt": f7("F7-count-first.txt"),
    "store_a/2026-11-21.txt": psqlTable([]),
    "store_a/roots.txt": fixtureRoots(H1, H2, H3),
    "run.log": [
      "2026-11-22T03:00:00Z\tstore_a\t2026-11-20\trc=0\tseconds=1.0\t",
      "2026-11-22T03:01:00Z\tstore_a\t2026-11-21\trc=0\tseconds=1.0\t",
      "END 2026-11-21T23:00:00Z",
    ].join("\n"),
  });
  const count = readCount(countDir, ["store_a"]);
  expect(queryIssues(CONFIG, count)).toEqual([
    {
      store: "store_a",
      name: "2026-11-21",
      what: "unreadable",
      detail: "its output is not a whole result: no summary row: the query did not end",
    },
  ]);
  expect(() => parseQueryOutput("store_b", "2026-11-20", f7("F7-count-first.txt"))).toThrow(
    'row 1 is of the store "store_a"'
  );
});

test("a cut's ranking is its changed skills by the sessions of its repository's copy, a tie by name", () => {
  const attribution = attribute(CONFIG, [F7_FIRST(), F7_SECOND()], f7World());
  expect(
    rankChanged(attribution, "skills-1", "acme/skills", [
      "src/beta",
      "skills/gone/",
      "src/alpha",
      "",
    ])
  ).toEqual({
    cut: "skills-1",
    repository: "acme/skills",
    rows: [
      // H2 and H1B: H1's alpha read counts in H1B, which holds it as well.
      { skill: "alpha", path: "src/alpha", sessions: 2 },
      { skill: "beta", path: "src/beta", sessions: 0 },
      { skill: "gone", path: "skills/gone", sessions: 0 },
    ],
    missing: ["skills/gone"],
  });
});

test("each ranked skill's sessions file holds the copy's reading sessions after every rule, sorted by store then hash, and no path", () => {
  const storeB = query("store_b", "2026-11-20", [
    // A checkout read and a prefix-table read of acme/skills' alpha.
    { skill: "alpha", copy: "/srv/checkouts/skills/src/alpha/SKILL.md", hashes: [H4] },
    {
      skill: "alpha",
      copy: "/home/acme/.omp/profiles/fixture/agent/skills/alpha/SKILL.md",
      hashes: [H2],
    },
  ]);
  const attribution = attribute(TWO_STORES, [F7_FIRST(), F7_SECOND(), storeB], f7World());
  const ranking = rankChanged(attribution, "skills-1", "acme/skills", ["src/alpha", "src/beta"]);
  expect(sessionFiles(attribution, ranking)).toEqual({
    files: [
      {
        name: "sessions/alpha.jsonl",
        text: [
          // store_a: H2 by its context, H1B by the prefix table; H1's alpha read counts in H1B,
          // which holds it as well.
          { store: "store_a", hash: H2 },
          { store: "store_a", hash: H1B },
          { store: "store_b", hash: H2 },
          { store: "store_b", hash: H4 },
        ]
          .map((line) => `${JSON.stringify(line)}\n`)
          .join(""),
      },
      { name: "sessions/beta.jsonl", text: "" },
    ],
    skipped: [],
  });
  // Two ranked directories of one name get no file.
  const shared = rankChanged(attribution, "skills-1", "acme/skills", ["src/beta", "old/beta"]);
  expect(sessionFiles(attribution, shared)).toEqual({
    files: [],
    skipped: ["beta (src/beta)", "beta (old/beta)"],
  });
});

/** The copies output directory for `world`, in copies.ts's columns. */
function r4Dir(root: string, world: R4Tables): string {
  const rows = <C extends readonly string[]>(
    columns: C,
    items: readonly Record<C[number], string>[]
  ) =>
    tsv(
      columns,
      items.map((item) => columns.map((column: C[number]) => item[column]))
    );
  return tree(root, {
    "skill-copies.tsv": rows(SKILL_COPIES_COLUMNS, world.copies),
    "prefix-table.tsv": rows(PREFIX_TABLE_COLUMNS, world.prefixes),
    "former-stages.tsv": rows(FORMER_STAGES_COLUMNS, world.formerStages),
    "installers.tsv": rows(INSTALLERS_COLUMNS, world.installers),
    "loaders.tsv": rows(LOADERS_COLUMNS, world.loaders),
    "manifests.tsv": rows(MANIFESTS_COLUMNS, []),
    "repositories.tsv": rows(REPOSITORIES_COLUMNS, world.repositories),
  });
}

function run(argv: readonly string[]) {
  const out: string[] = [];
  const err: string[] = [];
  const code = main(argv, { out: (text) => out.push(text), err: (text) => err.push(text) });
  return { code, out: out.join(""), err: err.join("\n") };
}

test("the command writes the count, its report, each cut's ranking and the models of its reading sessions under --out", () => {
  const root = path.join(dir, "command");
  const runLog = [
    "START 2026-11-21T20:00:00Z runner=/scratch/runner",
    "2026-11-21T20:00:01Z\tstore_a\t2026-11-20\trc=0\tseconds=1.0\t",
    "2026-11-21T20:00:02Z\tstore_a\t2026-11-21\trc=0\tseconds=1.0\t",
    "END 2026-11-21T23:00:00Z",
    "",
  ].join("\n");
  const countDir = tree(path.join(root, "count"), {
    "store_a/2026-11-20.txt": f7("F7-count-first.txt"),
    "store_a/2026-11-20.err": "",
    "store_a/2026-11-21.txt": f7("F7-count-second.txt"),
    "store_a/roots.txt": fixtureRoots(H1, H1B, H2, H3),
    "store_a/2026-11-21.err": "",
    "run.log": runLog,
  });
  const modelsDir = tree(path.join(root, "models"), {
    "store_a/2026-11-20.txt": f7("F7-models-first.txt"),
    // H1B ran on another family's model, under a gateway's name, that the gateway does not serve.
    "store_a/2026-11-21.txt": modelsText("store_a", [
      [H1B, "acme-gateway/gpt-6.1", "model_change"],
    ]),
    "run.log": runLog,
  });
  const copiesDir = r4Dir(path.join(root, "copies"), f7World());
  const files = tree(root, {
    "spans.jsonl": `${JSON.stringify({ store: "store_a", earliest_mtime_ms: Date.parse("2026-11-20T02:00:00Z"), files: 3, measured_at: "2026-11-21T23:30:00Z" })}\n`,
    "changed.txt": "src/alpha\nsrc/beta\nskills/gone\n",
  });
  const out = path.join(root, "out");
  const result = run([
    "--config",
    CONFIG_FILE,
    "--count",
    countDir,
    "--copies",
    copiesDir,
    "--spans",
    path.join(files, "spans.jsonl"),
    "--out",
    out,
    "--models",
    modelsDir,
    "--changed",
    `skills-1=${path.join(files, "changed.txt")}`,
  ]);
  expect(result.err).toBe("");
  expect(result.code).toBe(0);
  // F1b (H1B) holds each of F1's (H1's) reads, so H1's reads count in H1B and H1 is no session.
  expect(result.out).toContain(
    "2 query outputs, 3 copies (2 read), 3 read sessions (1 unattributed), 2 unmapped prefixes, 0 queries failed, split or without output; 2 models query outputs, 0 failed, split or without output"
  );

  const tsvText = readFileSync(path.join(out, "skill-read-count.tsv"), "utf8");
  expect(tsvText.split("\n")[0]).toBe(READ_COUNT_COLUMNS.join("\t"));
  expect(parseTsv(tsvText)).toEqual([
    {
      repository: "acme/skills",
      path: "src/alpha",
      name: "alpha",
      sessions: "2",
      former_name_sessions: "0",
      reference_sessions: "1",
      unattributed_sessions: "0",
      tie: "",
      loaded: "no",
      status: "read",
      transcript_store_sessions: "0",
      partial_coverage_hosts: "",
    },
    {
      repository: "acme/skills",
      path: "src/beta",
      name: "beta",
      sessions: "0",
      former_name_sessions: "0",
      reference_sessions: "0",
      unattributed_sessions: "1",
      tie: "acme/tools plugins/acme-tools/skills/beta",
      loaded: "no",
      status: "no read",
      transcript_store_sessions: "0",
      partial_coverage_hosts: "",
    },
    {
      repository: "acme/tools",
      path: "plugins/acme-tools/skills/beta",
      name: "beta",
      sessions: "1",
      former_name_sessions: "0",
      reference_sessions: "0",
      unattributed_sessions: "1",
      tie: "acme/skills src/beta",
      loaded: "no",
      status: "read",
      transcript_store_sessions: "0",
      partial_coverage_hosts: "",
    },
  ]);
  expect(JSON.parse(readFileSync(path.join(out, "skills-1", "ranking.json"), "utf8"))).toEqual([
    { skill: "alpha", sessions: 2 },
    { skill: "beta", sessions: 0 },
    { skill: "gone", sessions: 0 },
  ]);
  const sessionsOf = (skill: string) =>
    readFileSync(path.join(out, "skills-1", "sessions", `${skill}.jsonl`), "utf8");
  expect(sessionsOf("alpha")).toBe(
    [H2, H1B].map((hash) => `${JSON.stringify({ store: "store_a", hash })}\n`).join("")
  );
  expect([sessionsOf("beta"), sessionsOf("gone")]).toEqual(["", ""]);
  expect(result.out).toContain(`${path.join(out, "skills-1", "sessions")}/ (3 files)`);

  // H2 (Claude Code's first assistant message) ran on a served Anthropic model; H1B on another
  // family's model the gateway does not serve.
  const sonnet = "anthropic/claude-sonnet-5-5";
  const other = "acme-gateway/gpt-6.1";
  const bySkill = readFileSync(path.join(out, "models-by-skill.tsv"), "utf8");
  expect(bySkill.split("\n")[0]).toBe(MODELS_BY_SKILL_COLUMNS.join("\t"));
  const none = { no_model_sessions: "0", not_queried_sessions: "0" };
  expect(parseTsv(bySkill)).toEqual([
    {
      repository: "acme/skills",
      path: "src/alpha",
      name: "alpha",
      sessions: "2",
      by_model: `${other}=1; ${sonnet}=1`,
      ...none,
      not_anthropic_sessions: "1",
      not_anthropic_share: "0.500",
      served_sessions: "1",
      not_served_sessions: "1",
      not_served_models: `${other}=1`,
    },
    {
      repository: "acme/skills",
      path: "src/beta",
      name: "beta",
      sessions: "0",
      by_model: "",
      ...none,
      not_anthropic_sessions: "0",
      not_anthropic_share: "",
      served_sessions: "0",
      not_served_sessions: "0",
      not_served_models: "",
    },
    {
      repository: "acme/tools",
      path: "plugins/acme-tools/skills/beta",
      name: "beta",
      sessions: "1",
      by_model: `${sonnet}=1`,
      ...none,
      not_anthropic_sessions: "0",
      not_anthropic_share: "0.000",
      served_sessions: "1",
      not_served_sessions: "0",
      not_served_models: "",
    },
  ]);
  const byCut = readFileSync(path.join(out, "models-by-cut.tsv"), "utf8");
  expect(byCut.split("\n")[0]).toBe(MODELS_BY_CUT_COLUMNS.join("\t"));
  expect(
    parseTsv(byCut).map((row) => [
      row.cut,
      row.rank,
      row.skill,
      row.sessions,
      row.by_model,
      row.not_anthropic_share,
      row.served_sessions,
    ])
  ).toEqual([
    ["skills-1", "1", "alpha", "2", `${other}=1; ${sonnet}=1`, "0.500", "1"],
    ["skills-1", "2", "beta", "0", "", "", "0"],
    ["skills-1", "3", "gone", "0", "", "", "0"],
  ]);
  // Each ranked skill's models file: the counts of the sessions its sessions file holds, in the
  // shape behaviour --reading-models reads.
  const modelsOf = (skill: string) =>
    ModelCountFile.parse(
      JSON.parse(readFileSync(path.join(out, "skills-1", "models", `${skill}.json`), "utf8"))
    );
  expect(modelsOf("alpha")).toEqual({
    sessions: 2,
    byModel: [
      { model: other, anthropic: false, served: false, sessions: 1 },
      { model: sonnet, anthropic: true, served: true, sessions: 1 },
    ],
    noModel: 0,
    notQueried: 0,
    notAnthropic: 1,
    served: 1,
    withModel: 2,
  });
  const empty = {
    sessions: 0,
    byModel: [],
    noModel: 0,
    notQueried: 0,
    notAnthropic: 0,
    served: 0,
    withModel: 0,
  };
  expect([modelsOf("beta"), modelsOf("gone")]).toEqual([empty, empty]);
  expect(result.out).toContain(`${path.join(out, "skills-1", "models")}/ (3 files)`);
  // No transcriptStore is configured: no store-only file (INFRA-4676, 2026-11-25).
  expect(existsSync(path.join(out, "skills-1", "store-only"))).toBe(false);

  const report = readFileSync(path.join(out, "skill-read-count.md"), "utf8");
  for (const line of [
    "End time 2026-11-22T00:00:00Z. The window is [2026-11-20T00:00:00Z, 2026-11-22T00:00:00Z), 2 days.",
    "| store_a | devbox | 2026-11-20T02:00:00Z | 3 | 2026-11-20 | 3 | 2 | 4 | 205503 | 0 | 0 | 2 | 1 |",
    "- store_a starts after the window's start: its records span 2026-11-20T02:00:00Z to the end time, 1.9 of the window's 2 days.",
    "- The transcript store is not read: no transcriptStore is configured.",
    "None: each query of the window has its output, and the run log lists no failure.",
    "- store_a (devbox):",
    `  - ${via(USER_SKILLS)}: 1 session read from it; it holds 2 copies`,
    `| \`${F3}/marketplace/plugins/acme-tools/skills/alpha/\` | alpha | 1 | 0 | store_a: context: to acme/skills src/alpha, the one copy of alpha its context installs (1 session) |`,
    // The context's copies come in the order their units first give a session: the acme-tools
    // plugin's (H2, day 2026-11-20) before the user skills' (H1B, day 2026-11-21).
    `| \`${F1_PROFILE}/beta/\` | beta | 1 | 0 | store_a: unattributed: no copy's skill directory ends the path, and its context installs 2 copies of beta (acme/tools plugins/acme-tools/skills/beta, acme/skills src/beta) (1 session) |`,
    `| \`${F1_PROFILE}/alpha/\` | acme/skills src/alpha |`,
    "- beta: acme/skills src/beta (0 sessions) ties with acme/tools plugins/acme-tools/skills/beta; the name has 1 unattributed session.",
    "- beta (src/beta): no read; 1 session read beta unattributed",
    "| acme/skills | 0123456789abcdef0123456789abcdef01234567 | 2 | 9 | 40 | 49 |",
    "- skills-1 (acme/skills): 3 changed skills, in `skills-1/ranking.json`, with each one's reading sessions in `skills-1/sessions/<skill>.jsonl`; first: alpha (2), beta (0), gone (0). The copies output holds no copy at `skills/gone`: ranked with 0 sessions.",
    "### Skills read that no copy in the copies output has",
    "A skill that code, an agent definition or an installer loads into a session: its text reaches the session with no read, so its read count leaves out each session that loads it so.",
    "The models queries read 4 files; the head of 4 of them names a model. The 2 read sessions of the copies: with a model 2; whose file's head names none 0; in no models query's rows 0. Sessions two queries give two models for (the first day's is kept): 0.",
    "The gateway serves a model when its id is the id of one of the configuration's `servedModels`: anthropic/claude-sonnet-5-5, openai/gpt-6.1-sol.",
    `| ${sonnet} | yes | yes | 1 |`,
    `| ${other} | no | no | 1 |`,
    "#### skills-1 (acme/skills)",
    `| 1 | alpha | 2 | 2 | 1 | 0.500 | 1 | 1 | 0 | 0 | ${other} (1, not served), ${sonnet} (1) |`,
    "| 2 | beta | 0 | 0 | 0 | none | 0 | 0 | 0 | 0 | none |",
  ]) {
    expect(report).toContain(line);
  }
});

test("without --models the report says so and writes no models table and no models file", () => {
  const root = path.join(dir, "no-models");
  const countDir = tree(path.join(root, "count"), {
    "store_a/2026-11-20.txt": f7("F7-count-first.txt"),
    "store_a/roots.txt": fixtureRoots(H1, H2, H3),
    "run.log": "2026-11-21T20:00:01Z\tstore_a\t2026-11-20\trc=0\tseconds=1.0\t\n",
  });
  const copiesDir = r4Dir(path.join(root, "copies"), f7World());
  const files = tree(root, {
    "spans.jsonl": "",
    "changed.txt": "src/alpha\n",
  });
  const spans = path.join(files, "spans.jsonl");
  const out = path.join(root, "out");
  const result = run([
    "--config",
    CONFIG_FILE,
    "--count",
    countDir,
    "--copies",
    copiesDir,
    "--spans",
    spans,
    "--out",
    out,
    "--changed",
    `skills-1=${path.join(files, "changed.txt")}`,
  ]);
  expect(result.code).toBe(0);
  expect(readFileSync(path.join(out, "skill-read-count.md"), "utf8")).toContain(
    "Not counted: no --models directory was given."
  );
  expect(existsSync(path.join(out, "models-by-skill.tsv"))).toBe(false);
  expect(existsSync(path.join(out, "skills-1", "sessions", "alpha.jsonl"))).toBe(true);
  expect(existsSync(path.join(out, "skills-1", "models"))).toBe(false);
});

test("the command refuses missing flags, an --out inside an input, an unknown cut, and copies tables of other columns", () => {
  const root = path.join(dir, "refusals");
  const countDir = tree(path.join(root, "count"), { "run.log": "" });
  const modelsDir = tree(path.join(root, "models"), { "run.log": "" });
  const copiesDir = r4Dir(path.join(root, "copies"), f7World());
  const spans = path.join(tree(root, { "spans.jsonl": "" }), "spans.jsonl");
  const flags = (out: string, ...more: string[]) => [
    "--config",
    CONFIG_FILE,
    "--count",
    countDir,
    "--copies",
    copiesDir,
    "--spans",
    spans,
    "--out",
    out,
    ...more,
  ];
  expect(run(["--config", CONFIG_FILE]).code).toBe(2);
  const insideCount = run(flags(path.join(countDir, "out")));
  expect(insideCount.code).toBe(2);
  expect(insideCount.err).toContain("is inside --count");
  expect(existsSync(path.join(countDir, "out"))).toBe(false);
  const insideModels = run(flags(path.join(modelsDir, "out"), "--models", modelsDir));
  expect(insideModels.code).toBe(2);
  expect(insideModels.err).toContain("is inside --models");
  const unknownCut = run(flags(path.join(root, "out"), "--changed", "no-such-cut=/dev/null"));
  expect(unknownCut.code).toBe(2);
  expect(unknownCut.err).toContain('the cut "no-such-cut"');
  writeFileSync(path.join(copiesDir, "loaders.tsv"), "repository\tpath\tskill\n");
  const wrongColumns = run(flags(path.join(root, "out")));
  expect(wrongColumns.code).toBe(1);
  expect(wrongColumns.err).toContain(
    "loaders.tsv: its columns are repository, path, skill, not copies.ts's"
  );
});

test("the command refuses a copies output made before copies.ts wrote former-stages.tsv, naming the file", () => {
  const root = path.join(dir, "no-former-stages");
  const countDir = tree(path.join(root, "count"), { "run.log": "" });
  const copiesDir = r4Dir(path.join(root, "copies"), f7World());
  rmSync(path.join(copiesDir, "former-stages.tsv"));
  const spans = path.join(tree(root, { "spans.jsonl": "" }), "spans.jsonl");
  const result = run([
    "--config",
    CONFIG_FILE,
    "--count",
    countDir,
    "--copies",
    copiesDir,
    "--spans",
    spans,
    "--out",
    path.join(root, "out"),
  ]);
  expect(result.code).toBe(1);
  expect(result.err).toContain(
    `${path.join(copiesDir, "former-stages.tsv")} does not exist: the copies output was made before copies.ts wrote former-stages.tsv, so run copies again`
  );
  expect(existsSync(path.join(root, "out", "skill-read-count.tsv"))).toBe(false);
});

test("the command writes a renamed copy's reads under its former name beside its sessions, in skill-read-count.tsv and in its report", () => {
  const root = path.join(dir, "former-name");
  const countDir = tree(path.join(root, "count"), {
    "store_a/2026-11-20.txt": queryText("store_a", "2026-11-20", [
      { skill: "ce-simplify-code", copy: `${IMAGE_SIMPLIFY}SKILL.md`, hashes: [H4] },
      {
        skill: "simplify-settled-diff",
        copy: "/opt/legion/pi-legion/dist/skills/simplify-settled-diff/SKILL.md",
        hashes: ["5555555555555555"],
      },
    ]),
    "store_a/roots.txt": fixtureRoots(H4, "5555555555555555"),
    "run.log": "2026-11-21T20:00:01Z\tstore_a\t2026-11-20\trc=0\tseconds=1.0\t\n",
  });
  const formerRow = {
    ...prefixRow(IMAGE_SIMPLIFY, PARENT_SIMPLIFY, PARENT_LEGION),
    name: "ce-simplify-code",
  };
  const copiesDir = r4Dir(
    path.join(root, "copies"),
    imageWorld(formerSimplify("renamed", PARENT_SIMPLIFY), [formerRow])
  );
  const spans = path.join(tree(root, { "spans.jsonl": "" }), "spans.jsonl");
  const out = path.join(root, "out");
  const result = run([
    "--config",
    CONFIG_FILE,
    "--count",
    countDir,
    "--copies",
    copiesDir,
    "--spans",
    spans,
    "--out",
    out,
  ]);
  expect(result.code).toBe(0);
  expect(
    parseTsv(readFileSync(path.join(out, "skill-read-count.tsv"), "utf8"))
      .filter((row) => row.path === PARENT_SIMPLIFY.path)
      .map((row) => [row.name, row.sessions, row.former_name_sessions])
  ).toEqual([["simplify-settled-diff", "2", "1"]]);
  expect(readFileSync(path.join(out, "skill-read-count.md"), "utf8")).toContain(
    "| simplify-settled-diff | org/legion skills/simplify-settled-diff | 2 | 1 | ce-simplify-code |"
  );
});

test("one count row can read different copies in two profiles of one store", () => {
  const root = path.join(dir, "profile-roots");
  const countDir = tree(path.join(root, "count"), {
    "store_a/2026-11-20.txt": queryText("store_a", "2026-11-20", [
      {
        skill: "alpha",
        copy: "/home/fixture/.omp/profiles/fixture/agent/skills/alpha/SKILL.md",
        hashes: [H1],
      },
      {
        skill: "beta",
        copy: "/home/fixture/plugins/cache/acme-market/acme-tools/1/skills/beta/SKILL.md",
        hashes: [H2],
      },
      { skill: "beta", copy: "skill://beta", hashes: [H3, H4] },
    ]),
    "store_a/roots.txt": psqlTable([
      { hash: H1, profile: "default" },
      { hash: H2, profile: "legion" },
      { hash: H3, profile: "legion" },
      { hash: H4, profile: "default" },
    ]),
  });
  const copiesDir = r4Dir(path.join(root, "copies"), f7World());
  const spans = path.join(tree(root, { "spans.jsonl": "" }), "spans.jsonl");
  const out = path.join(root, "out");
  const result = run([
    "--config",
    CONFIG_FILE,
    "--count",
    countDir,
    "--copies",
    copiesDir,
    "--spans",
    spans,
    "--out",
    out,
  ]);
  expect(result.code).toBe(0);
  expect(
    parseTsv(readFileSync(path.join(out, "skill-read-count.tsv"), "utf8"))
      .filter((row) => row.name === "beta")
      .map((row) => [row.repository, row.sessions, row.unattributed_sessions])
  ).toEqual([
    ["acme/skills", "1", "0"],
    ["acme/tools", "2", "0"],
  ]);
});

const TRANSCRIPT_CONFIG = {
  ...CONFIG,
  windowStart: "2026-11-07T00:00:00Z",
  windowEnd: "2026-11-12T00:00:00Z",
  transcriptStore: {
    command: "/home/dev/bin/query-transcripts",
    hosts: [{ host: "host-a", context: "devbox" }],
  },
};
const transcriptDay = "2026-11-10";
const transcriptHost = (host: string) => `transcripts@${host}`;
const jsonLines = (rows: readonly object[]) =>
  rows.map((row) => `${JSON.stringify(row)}\n`).join("");
const transcriptEnd = { row: "end", store: "transcripts" };

interface FixtureCoverage {
  row: string;
  store: string;
  format: string;
  profile: string;
  first: number;
  last: number;
  days: string[];
  hashes: string[];
}

function transcriptCoverage(
  host: string,
  format: string,
  hashes: string[],
  days = [transcriptDay]
): FixtureCoverage {
  return {
    row: "coverage",
    store: transcriptHost(host),
    format,
    profile: format === "omp" ? "default" : "unknown",
    first: Date.parse(`${days[0]}T00:00:00Z`),
    last: Date.parse(`${days.at(-1)}T23:59:59.999Z`),
    days,
    hashes,
  };
}

/** attribute over the transcript-store count of `coverage` and `reads` and the Postgres files
 *  `pg`, with TRANSCRIPT_CONFIG; with `changed` (a `--changed` file's text), it ranks the cut
 *  skills-1's changed skills too. Returns the count's rows, its report and `--out`. */
function transcriptReport(
  name: string,
  coverage: readonly FixtureCoverage[],
  reads: readonly QueryOutput[],
  world: R4Tables,
  pg: readonly string[] = [],
  changed?: string
) {
  const root = path.join(dir, `transcript-${name}`);
  const [firstPg] = pg;
  const countDir = tree(path.join(root, "count"), {
    "store_a/hashes.txt": psqlTable(pg.map((hash) => ({ hash }))),
    "store_a/roots.txt": fixtureRoots(...pg),
    "store_a/2026-11-10.txt": queryText(
      "store_a",
      transcriptDay,
      firstPg === undefined
        ? []
        : [{ skill: "alpha", copy: `${F1_PROFILE}/alpha/SKILL.md`, hashes: [firstPg] }]
    ),
    "transcripts/coverage.txt": jsonLines([...coverage, transcriptEnd]),
    "transcripts/2026-11-10.txt": jsonLines([
      ...reads.flatMap(({ rows, summary }) => [...rows, summary]),
      transcriptEnd,
    ]),
    "run.log": "",
  });
  const out = path.join(root, "out");
  const result = run([
    "--config",
    path.join(tree(root, { "config.json": JSON.stringify(TRANSCRIPT_CONFIG) }), "config.json"),
    "--count",
    countDir,
    "--copies",
    r4Dir(path.join(root, "copies"), world),
    "--spans",
    path.join(tree(root, { "spans.jsonl": "" }), "spans.jsonl"),
    "--out",
    out,
    ...(changed === undefined
      ? []
      : [
          "--changed",
          `skills-1=${path.join(tree(root, { "changed.txt": changed }), "changed.txt")}`,
        ]),
  ]);
  expect(result.err).toBe("");
  expect(result.code).toBe(0);
  return {
    rows: parseTsv(readFileSync(path.join(out, "skill-read-count.tsv"), "utf8")),
    report: readFileSync(path.join(out, "skill-read-count.md"), "utf8"),
    out,
  };
}

test("R1 v3 row 1: a Postgres file is counted there, even if a second overlap had no Postgres read", () => {
  const store = transcriptHost("host-a");
  const { rows, report } = transcriptReport(
    "dedupe",
    [transcriptCoverage("host-a", "omp", [H1, H2, H3])],
    [
      query(store, transcriptDay, [
        { skill: "alpha", copy: `${F1_PROFILE}/alpha/SKILL.md`, hashes: [H1, H2, H3] },
      ]),
    ],
    f7World(),
    [H1, H2]
  );
  expect(rows.find((row) => row.name === "alpha")).toMatchObject({
    sessions: "2",
    transcript_store_sessions: "1",
  });
  expect(report).toContain("| 3 | 1 | 1 |");
});

test("R1 v3 row 2: the copy resolved by a Claude Code Skill call has a count", () => {
  const { rows, report } = transcriptReport(
    "claude",
    [transcriptCoverage("host-a", "claude", [H1]), transcriptCoverage("host-a", "codex", [H2])],
    [
      query(transcriptHost("host-a"), transcriptDay, [
        { skill: "delta", copy: `/home/dev/cc/${DELTA.path}/SKILL.md`, hashes: [H1] },
      ]),
    ],
    hostsWorld()
  );
  expect(rows.find((row) => row.name === "delta")).toMatchObject({
    sessions: "1",
    status: "read",
  });
  expect(rows.find((row) => row.name === "epsilon")?.status).toBe("no read");
  expect(report).toContain("no read, Read or Skill calls");
});

test("R1 v3 row 3: an unconfigured host's path can place a read, but its context cannot", () => {
  const { rows, report } = transcriptReport(
    "context",
    [transcriptCoverage("host-b", "omp", [H1, H2, H3])],
    [
      query(transcriptHost("host-b"), transcriptDay, [
        { skill: "alpha", copy: `${F1_PROFILE}/alpha/SKILL.md`, hashes: [H1] },
        { skill: "beta", copy: "skill://beta", hashes: [H2] },
        { skill: "alpha", copy: "/home/dev/skills/src/alpha/SKILL.md", hashes: [H3] },
      ]),
    ],
    f7World()
  );
  expect(rows.find((row) => row.name === "alpha")?.sessions).toBe("2");
  expect(rows.find((row) => row.name === "beta")?.unattributed_sessions).toBe("1");
  expect(report).toContain("the host's context is not known");
});

test("R1 v3 row 4: the mark lists the gap host and the late host", () => {
  const { rows, report } = transcriptReport(
    "coverage",
    [
      transcriptCoverage(
        "host-a",
        "omp",
        [H1],
        ["2026-11-07", "2026-11-09", "2026-11-10", "2026-11-11"]
      ),
      transcriptCoverage("host-b", "omp", [H2], ["2026-11-09", "2026-11-10", "2026-11-11"]),
    ],
    ["host-a", "host-b"].map((host, index) =>
      query(transcriptHost(host), transcriptDay, [
        { skill: "alpha", copy: `${F1_PROFILE}/alpha/SKILL.md`, hashes: [index === 0 ? H1 : H2] },
      ])
    ),
    f7World()
  );
  expect(rows.find((row) => row.name === "alpha")?.partial_coverage_hosts).toBe("host-a; host-b");
  expect(report).toContain("gap days: 2026-11-08");
  expect(report).toContain("starts after the window's start");
});

test("R1 v3 row 5: the transcript SQL fills the UTC day and window and rejects an unbound placeholder", () => {
  const root = tree(path.join(dir, "transcript-template"), {
    "config.json": JSON.stringify(TRANSCRIPT_CONFIG),
  });
  const out: string[] = [];
  const err: string[] = [];
  const status = countSqlMain(
    ["--config", path.join(root, "config.json"), "--query", "transcript", "--day", transcriptDay],
    { out: (text) => out.push(text), err: (text) => err.push(text) }
  );
  expect(err).toEqual([]);
  expect(status).toBe(0);
  const sql = out.join("");
  expect(sql).toContain(String(Date.parse("2026-11-10T00:00:00Z")));
  expect(sql).toContain(String(Date.parse("2026-11-11T00:00:00Z")));
  expect(sql).toContain("2026-11-07T00:00:00.000Z");
  expect(sql).toContain("2026-11-12T00:00:00.000Z");
  expect(sql).not.toContain("{{");
  expect(() => fillTemplate(`${sql}\n{{missing}}`, {})).toThrow(
    "no value for the placeholder missing"
  );
});

// INFRA-4676's architect (2026-11-25T18:54:22Z and 19:02:42Z): cases come from Postgres sessions
// alone, so each behaviour report says how many of its skill's counted sessions only the
// transcript store holds, from attribute's store-only file of the skill.
test("with a transcript store, each ranked skill's store-only file gives its counted sessions, those only the transcript store holds and each host's, which its sessions file leaves out", () => {
  const alpha = { skill: "alpha", copy: `${F1_PROFILE}/alpha/SKILL.md` };
  const { rows, out } = transcriptReport(
    "store-only",
    [transcriptCoverage("host-a", "omp", [H1, H2]), transcriptCoverage("host-b", "omp", [H3])],
    [
      query(transcriptHost("host-a"), transcriptDay, [{ ...alpha, hashes: [H1, H2] }]),
      query(transcriptHost("host-b"), transcriptDay, [{ ...alpha, hashes: [H3] }]),
    ],
    f7World(),
    [H4],
    "src/alpha\nsrc/beta\n"
  );
  // Read as plain JSON: toEqual checks each field, and that there is no other.
  const storeOnlyOf = (skill: string): unknown =>
    JSON.parse(readFileSync(path.join(out, "skills-1", "store-only", `${skill}.json`), "utf8"));
  // H4 is store_a's; H1, H2 and H3 only the transcript store holds.
  expect(storeOnlyOf("alpha")).toEqual({
    skill: "alpha",
    counted: 4,
    storeOnly: 3,
    hosts: [
      { host: "host-a", sessions: 2 },
      { host: "host-b", sessions: 1 },
    ],
  });
  expect(rows.find((row) => row.name === "alpha")).toMatchObject({
    sessions: "4",
    transcript_store_sessions: "3",
  });
  expect(readFileSync(path.join(out, "skills-1", "sessions", "alpha.jsonl"), "utf8")).toBe(
    `${JSON.stringify({ store: "store_a", hash: H4 })}\n`
  );
  expect(storeOnlyOf("beta")).toEqual({ skill: "beta", counted: 0, storeOnly: 0, hosts: [] });
});
