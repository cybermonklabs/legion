import { afterAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import {
  type Attribution,
  attribute,
  type CountRow,
  checkoutRule,
  copyPrefix,
  type InstallersRow,
  MODELS_BY_CUT_COLUMNS,
  MODELS_BY_SKILL_COLUMNS,
  ModelCountFile,
  main,
  modelCount,
  modelId,
  notAnthropicShare,
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
  type SkillCopiesRow,
  sessionFiles,
  sessionKey,
  sessionModel,
  sessionModels,
} from "./attribute";
import { loadConfig, parseConfig } from "./config";
import {
  INSTALLERS_COLUMNS,
  LOADERS_COLUMNS,
  MANIFESTS_COLUMNS,
  PREFIX_TABLE_COLUMNS,
  parseTsv,
  REPOSITORIES_COLUMNS,
  SKILL_COPIES_COLUMNS,
  tsv,
} from "./copies";

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
// F7-count-second.txt holds H1B (F1b's session) reading alpha at F1_PROFILE/alpha/SKILL.md.
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

/** A count.sql result of `store` for the query `name`, in F7's shape. */
function queryText(
  store: string,
  name: string,
  rows: readonly (Pick<CountRow, "skill" | "copy"> & {
    readonly hashes: readonly string[];
    readonly kind?: CountRow["kind"];
    readonly files?: number;
    readonly unpaired?: number;
    readonly unpaired_hashes?: readonly string[];
  })[]
): string {
  return psqlTable([
    ...rows.map((row) => ({
      row: "count",
      store,
      day: name.slice(0, 10),
      kind: "read",
      files: row.hashes.length,
      unpaired: 0,
      unpaired_hashes: [],
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
  [
    "a tie the path does not break is unattributed",
    "/tmp/work/.claude/skills/delta/SKILL.md",
    undefined,
    "unattributed: the path ends with .claude/skills/delta/, the skill directory of 2 repositories (acme/kiln, acme/other), and no directory of the path names just one of them",
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
  // F7-count-second's session (F1b) is another file, so another session.
  const second = F7_SECOND();
  expect(second.rows.map((row) => row.hashes)).toEqual([[H1B], [H1B]]);
  expect(countOf(attribute(CONFIG, [F7_FIRST(), second], f7World()), ALPHA).sessions).toBe(3);
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
      { skill: "alpha", path: "src/alpha", sessions: 3 },
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
          // store_a: H1 by the prefix table, H2 by its context, H1B by the prefix table.
          { store: "store_a", hash: H2 },
          { store: "store_a", hash: H1B },
          { store: "store_a", hash: H1 },
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
  expect(result.out).toContain(
    "2 query outputs, 3 copies (2 read), 4 read sessions (1 unattributed), 2 unmapped prefixes, 0 queries failed, split or without output; 2 models query outputs, 0 failed, split or without output"
  );

  const tsvText = readFileSync(path.join(out, "skill-read-count.tsv"), "utf8");
  expect(tsvText.split("\n")[0]).toBe(READ_COUNT_COLUMNS.join("\t"));
  expect(parseTsv(tsvText)).toEqual([
    {
      repository: "acme/skills",
      path: "src/alpha",
      name: "alpha",
      sessions: "3",
      reference_sessions: "2",
      unattributed_sessions: "0",
      tie: "",
      loaded: "no",
      status: "read",
    },
    {
      repository: "acme/skills",
      path: "src/beta",
      name: "beta",
      sessions: "0",
      reference_sessions: "0",
      unattributed_sessions: "1",
      tie: "acme/tools plugins/acme-tools/skills/beta",
      loaded: "no",
      status: "no read",
    },
    {
      repository: "acme/tools",
      path: "plugins/acme-tools/skills/beta",
      name: "beta",
      sessions: "1",
      reference_sessions: "0",
      unattributed_sessions: "1",
      tie: "acme/skills src/beta",
      loaded: "no",
      status: "read",
    },
  ]);
  expect(JSON.parse(readFileSync(path.join(out, "skills-1", "ranking.json"), "utf8"))).toEqual([
    { skill: "alpha", sessions: 3 },
    { skill: "beta", sessions: 0 },
    { skill: "gone", sessions: 0 },
  ]);
  const sessionsOf = (skill: string) =>
    readFileSync(path.join(out, "skills-1", "sessions", `${skill}.jsonl`), "utf8");
  expect(sessionsOf("alpha")).toBe(
    [H2, H1B, H1].map((hash) => `${JSON.stringify({ store: "store_a", hash })}\n`).join("")
  );
  expect([sessionsOf("beta"), sessionsOf("gone")]).toEqual(["", ""]);
  expect(result.out).toContain(`${path.join(out, "skills-1", "sessions")}/ (3 files)`);

  // H1 (model_change) and H2 (Claude Code's first assistant message) ran on a served Anthropic
  // model; H1B on another family's model the gateway does not serve.
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
      sessions: "3",
      by_model: `${sonnet}=2; ${other}=1`,
      ...none,
      not_anthropic_sessions: "1",
      not_anthropic_share: "0.333",
      served_sessions: "2",
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
    ["skills-1", "1", "alpha", "3", `${sonnet}=2; ${other}=1`, "0.333", "2"],
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
    sessions: 3,
    byModel: [
      { model: sonnet, anthropic: true, served: true, sessions: 2 },
      { model: other, anthropic: false, served: false, sessions: 1 },
    ],
    noModel: 0,
    notQueried: 0,
    notAnthropic: 1,
    served: 2,
    withModel: 3,
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

  const report = readFileSync(path.join(out, "skill-read-count.md"), "utf8");
  for (const line of [
    "End time 2026-11-22T00:00:00Z. The window is [2026-11-20T00:00:00Z, 2026-11-22T00:00:00Z), 2 days.",
    "| store_a | devbox | 2026-11-20T02:00:00Z | 3 | 2026-11-20 | 4 | 2 | 4 | 205506 | 0 | 0 |",
    "- store_a starts after the window's start: its records span 2026-11-20T02:00:00Z to the end time, 1.9 of the window's 2 days.",
    '- The transcript store is not read: the cluster store that a pod reads holds no Claude Code transcript until the Claude Code transcripts are moved into it, so this count holds no Claude Code session. "Claude Code" below lists the skills that leaves out.',
    "None: each query of the window has its output, and the run log lists no failure.",
    "- store_a (devbox):",
    `  - ${via(USER_SKILLS)}: 2 sessions read from it; it holds 2 copies`,
    `| \`${F3}/marketplace/plugins/acme-tools/skills/alpha/\` | alpha | 1 | 0 | store_a: context: to acme/skills src/alpha, the one copy of alpha its context installs (1 session) |`,
    `| \`${F1_PROFILE}/beta/\` | beta | 1 | 0 | store_a: unattributed: no copy's skill directory ends the path, and its context installs 2 copies of beta (acme/skills src/beta, acme/tools plugins/acme-tools/skills/beta) (1 session) |`,
    `| \`${F1_PROFILE}/alpha/\` | acme/skills src/alpha |`,
    "- beta: acme/skills src/beta (0 sessions) ties with acme/tools plugins/acme-tools/skills/beta; the name has 1 unattributed session.",
    "- beta (src/beta): no read; 1 session read beta unattributed",
    "| acme/skills | 0123456789abcdef0123456789abcdef01234567 | 2 | 9 | 40 | 49 |",
    "- skills-1 (acme/skills): 3 changed skills, in `skills-1/ranking.json`, with each one's reading sessions in `skills-1/sessions/<skill>.jsonl`; first: alpha (3), beta (0), gone (0). The copies output holds no copy at `skills/gone`: ranked with 0 sessions.",
    "### Skills read that no copy in the copies output has",
    "A skill that code, an agent definition or an installer loads into a session. The diagnosis of unread skills leaves these out.",
    "The models queries read 4 files; the head of 4 of them names a model. The 3 read sessions of the copies: with a model 3; whose file's head names none 0; in no models query's rows 0. Sessions two queries give two models for (the first day's is kept): 0.",
    "The gateway serves a model when its id is the id of one of the configuration's `servedModels`: anthropic/claude-sonnet-5-5, openai/gpt-6.1-sol.",
    `| ${sonnet} | yes | yes | 2 |`,
    `| ${other} | no | no | 1 |`,
    "#### skills-1 (acme/skills)",
    `| 1 | alpha | 3 | 3 | 1 | 0.333 | 2 | 1 | 0 | 0 | ${sonnet} (2), ${other} (1, not served) |`,
    "| 2 | beta | 0 | 0 | 0 | none | 0 | 0 | 0 | 0 | none |",
  ]) {
    expect(report).toContain(line);
  }
});

test("without --models the report says so and writes no models table and no models file", () => {
  const root = path.join(dir, "no-models");
  const countDir = tree(path.join(root, "count"), {
    "store_a/2026-11-20.txt": f7("F7-count-first.txt"),
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
