/**
 * The search for every opener of a Postgres connection string in the repository, each judged by
 * what it passes.
 *
 *   bun scripts/checks/db-openers.ts [--all]
 *
 * A Postgres connection string a person configured can carry a user and a password, and a user
 * part written with a raw @ or / is read by pgx in part as the host or the database, which its
 * errors print. So each opener reads its string through the one formatter, `pgsetting` in the Go
 * module of Envoy (`ParseConfig` or `ParsePoolConfig`, whose error names the value by its length,
 * before the call; `Refuse` alone refuses only the shapes pgx reads apart, so it guards nothing),
 * or is handed a value that holds no setting, or this search's list (`db-openers.judged.ts`) says
 * why its value cannot be printed.
 *
 * It reads every Go module of the repository (each `go.mod` git tracks; a module the workspace's
 * go.work does not name is read with GOWORK=off) with its Go half, `scripts/checks/db-openers/go/`,
 * which lists each call of an opener form of database/sql, pgx, pgconn, pgxpool, pgx's stdlib and
 * lib/pq in the module's non-test files, whatever their build constraints (a file only a build tag
 * or another system builds is read in a build of its own), the forms read from those packages' own
 * signatures, and each call of a wrapper of one, and judges each by its argument. It reads every
 * TypeScript and JavaScript file git tracks, tests included, with its TypeScript half
 * (`scripts/checks/db-openers/typescript.ts`), which lists each use of Bun's SQL and of the
 * Postgres client modules. An entry of the list judges a call only when it names the call's file,
 * function, callee and whole argument text; text is cut only where it is printed.
 *
 * It exits 0 when every call it lists is judged by reading or by an entry of the list, and 1 when a
 * call is neither, when an entry names no call it lists, or when it cannot say: when it listed no
 * call at all, read no file of a module, or a planted shape (`db-openers/go/planted.go`, more files
 * of the Dispatch command's package, and `db-openers/planted.ts`) was not listed as marked. A
 * search that cannot see those shapes would say the same about the tree whether it looked or not,
 * so it prints UNKNOWN then.
 */
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { PLANTED, plantedBlind, plantedName } from "./db-openers/planted";
import { folded, type Listed, short, typescriptOpeners } from "./db-openers/typescript";
import { JUDGED, type JudgedOpener } from "./db-openers.judged";

export const root = path.resolve(import.meta.dir, "../..");

/** The module and the package of it the Go half's planted shapes stand in: the Dispatch command,
 *  whose other openers go through the formatter, so only what a call passes can judge it. */
export const GO_PLANT = { module: "packages/envoy", package: "cmd/dispatch" };

/** One Go module's run: its records, its summary and what its planted shapes gave. */
interface GoRun {
  readonly module: string;
  readonly listed: Listed[];
  readonly summary: Record<string, unknown> | undefined;
  readonly blind: string[];
}

const goDirectory = "scripts/checks/db-openers/go";

/** A call as the Go half prints it, read as what it must be, or undefined. */
function goCall(c: unknown): Listed | undefined {
  if (
    typeof c !== "object" ||
    c === null ||
    !("file" in c && typeof c.file === "string") ||
    !("line" in c && typeof c.line === "number") ||
    !("function" in c && typeof c.function === "string") ||
    !("callee" in c && typeof c.callee === "string") ||
    !("argument" in c && typeof c.argument === "string") ||
    !("kind" in c && typeof c.kind === "string") ||
    !("why" in c && typeof c.why === "string") ||
    !("verdict" in c) ||
    (c.verdict !== "judged" && c.verdict !== "unjudged" && c.verdict !== "wrapper")
  )
    return undefined;
  const { file, line, callee, argument, kind, verdict, why } = c;
  return { file, line, function: c.function, callee, argument, kind, verdict, why };
}

/** The Go half's run over the module in `module` (a directory relative to the root). */
function goRun(module: string, workspace: ReadonlySet<string> | undefined): GoRun {
  const sources = readdirSync(path.join(root, goDirectory))
    .filter((file) => file.endsWith(".go"))
    .sort()
    .map((file) => `${goDirectory}/${file}`);
  const plant = module === GO_PLANT.module ? [GO_PLANT.package] : [];
  const env = { ...process.env };
  if (workspace !== undefined && !workspace.has(module)) env.GOWORK = "off";
  const run = Bun.spawnSync(["go", "run", ...sources, module, ...plant], {
    cwd: root,
    env,
    stdout: "pipe",
    stderr: "pipe",
  });
  const listed: Listed[] = [];
  const blind: string[] = [];
  let summary: Record<string, unknown> | undefined;
  for (const line of run.stdout.toString().split("\n")) {
    if (line === "") continue;
    const record: unknown = JSON.parse(line);
    if (typeof record !== "object" || record === null) continue;
    if ("call" in record) {
      const call = goCall(record.call);
      if (call === undefined)
        blind.push(`the Go half printed a call this command cannot read: ${line}`);
      else listed.push(call);
    }
    if ("blind" in record && Array.isArray(record.blind)) blind.push(...record.blind.map(String));
    if ("summary" in record && typeof record.summary === "object" && record.summary !== null)
      summary = Object.fromEntries(Object.entries(record.summary));
  }
  if (run.exitCode !== 0 || summary === undefined)
    blind.push(
      `the Go half exited ${run.exitCode} on ${module}: ${run.stderr.toString().trim().slice(0, 2000)}`
    );
  return { module, listed, summary, blind };
}

/** The files git tracks that are on disk, and the new ones it does not ignore, so that a run
 *  before a commit reads both, relative to the root. */
function gitFiles(): string[] {
  const listed = Bun.spawnSync(
    ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
    {
      cwd: root,
      stdout: "pipe",
    }
  );
  return listed.stdout
    .toString()
    .split("\0")
    .filter((file) => file !== "" && existsSync(path.join(root, file)));
}

/** The Go modules git lists, by directory, and the ones the workspace names (undefined when the
 *  repository has no go.work, so each module is read on its own). */
function goModules(): { modules: string[]; workspace: ReadonlySet<string> | undefined } {
  const modules = gitFiles()
    .filter((file) => file === "go.mod" || file.endsWith("/go.mod"))
    .map((file) => path.dirname(file))
    .sort();
  const work = Bun.spawnSync(["go", "work", "edit", "-json"], { cwd: root, stdout: "pipe" });
  if (work.exitCode !== 0) return { modules, workspace: undefined };
  const parsed: unknown = JSON.parse(work.stdout.toString());
  const uses =
    typeof parsed === "object" && parsed !== null && "Use" in parsed && Array.isArray(parsed.Use)
      ? parsed.Use
      : [];
  const workspace = new Set<string>(
    uses.flatMap((use: unknown) =>
      typeof use === "object" &&
      use !== null &&
      "DiskPath" in use &&
      typeof use.DiskPath === "string"
        ? [path.normalize(use.DiskPath)]
        : []
    )
  );
  return { modules, workspace };
}

/** The TypeScript and JavaScript files git lists, declarations apart, with their text. */
function typescriptFiles(): Map<string, string> {
  const files = new Map<string, string>();
  for (const file of gitFiles()) {
    if (!/\.[cm]?[jt]sx?$/.test(file) || /\.d\.[cm]?ts$/.test(file)) continue;
    files.set(file, readFileSync(path.join(root, file), "utf8"));
  }
  return files;
}

/** What one run read: the Go modules' runs, and the TypeScript files with the planted one among
 *  them when the planted shapes are read. */
export interface Read {
  readonly go: readonly GoRun[];
  readonly typescript: ReadonlyMap<string, string>;
}

/** The entry of `judged` that judges `call`, if one does: the same file, function, callee and
 *  whole argument text, each run of white space read as one space. */
function entryFor(call: Listed, judged: readonly JudgedOpener[]): JudgedOpener | undefined {
  return judged.find(
    (j) =>
      j.file === call.file &&
      j.function === call.function &&
      j.callee === call.callee &&
      folded(j.argument) === folded(call.argument)
  );
}

/** The search's verdict over what it read, printed line by line through `print`: 0 when every
 *  call is judged, 1 otherwise. `requirePlants` is false only for a run whose reads hold no
 *  planted shape by design (the check's own tests). */
export function run(
  read: Read,
  judged: readonly JudgedOpener[],
  print: (line: string) => void,
  options: { all?: boolean; requirePlants?: boolean } = {}
): number {
  const blind: string[] = read.go.flatMap((g) => g.blind);
  const typescript = typescriptOpeners(read.typescript);
  if (read.typescript.has(plantedName)) {
    const planted = new Map<number, string>();
    // A line listed twice reads as both verdicts, which matches no mark.
    for (const t of typescript)
      if (t.file === plantedName) {
        const before = planted.get(t.line);
        planted.set(t.line, before === undefined ? t.verdict : `${before} and ${t.verdict}`);
      }
    blind.push(...plantedBlind(planted));
  } else if (options.requirePlants !== false) {
    blind.push(`the TypeScript half did not read its planted shapes (${plantedName})`);
  }
  if (options.requirePlants !== false && !read.go.some((g) => g.module === GO_PLANT.module))
    blind.push(
      `no Go module read is ${GO_PLANT.module}, which holds the Go half's planted shapes, so they were not read`
    );
  // The planted shapes' files all start so: the TypeScript half's (plantedName), and the Go half's
  // (`plantedFiles` in `db-openers/go/planted.go`).
  const plantedPrefix = "scripts/checks/db-openers.planted";
  const calls = [...read.go.flatMap((g) => g.listed), ...typescript].filter(
    (c) => !c.file.startsWith(plantedPrefix)
  );
  const used = new Set<JudgedOpener>();
  let byReading = 0;
  let inWrappers = 0;
  let byList = 0;
  let open = 0;
  for (const call of calls) {
    const where = `${call.file}:${call.line} ${call.function}: ${call.callee}(${short(call.argument)})`;
    if (call.verdict === "judged") {
      byReading += 1;
      if (options.all) print(`judged ${where}: ${call.why}`);
      continue;
    }
    if (call.verdict === "wrapper") {
      inWrappers += 1;
      if (options.all) print(`wrapper ${where}: judged at each call of ${call.function}`);
      continue;
    }
    const entry = entryFor(call, judged);
    if (entry !== undefined) {
      used.add(entry);
      byList += 1;
      if (options.all) print(`listed ${where}: ${entry.why}`);
      continue;
    }
    open += 1;
    print(`UNJUDGED ${where}: ${call.why}`);
  }
  const stale = judged.filter((j) => !used.has(j));
  for (const j of stale)
    print(
      `STALE ${j.file} ${j.function}: ${j.callee}(${short(j.argument)}): the list judges a call the search no longer lists`
    );
  for (const g of read.go) {
    const s = g.summary ?? {};
    const builds = Array.isArray(s.builds) ? s.builds.length : 0;
    print(
      `${g.module}: ${s.files ?? 0} Go files read (${s.constrained ?? 0} that only a build tag or another system builds), in ${builds} builds, ${s.forms ?? 0} opener forms, ${g.listed.filter((c) => !c.file.startsWith(plantedPrefix)).length} calls`
    );
  }
  print(
    `TypeScript: ${[...read.typescript.keys()].filter((f) => f !== plantedName).length} files read, ${typescript.filter((c) => c.file !== plantedName).length} uses`
  );
  if (blind.length > 0) {
    for (const b of blind) print(`UNKNOWN ${b}`);
    print(`UNKNOWN the search cannot say: ${blind.length} of its reads failed or were blind`);
    return 1;
  }
  if (calls.length === 0) {
    print(
      `UNKNOWN the search listed no opener in ${read.go.length} Go modules and ${read.typescript.size} TypeScript files, so nothing was checked`
    );
    return 1;
  }
  const judgedCount = `${byReading} judged by what they pass, ${inWrappers} in wrappers judged at each call, ${byList} by the list`;
  if (open > 0 || stale.length > 0) {
    print(
      `FAIL ${calls.length} opener calls: ${judgedCount}; ${open} unjudged, ${stale.length} stale entries`
    );
    return 1;
  }
  print(
    `PASS ${calls.length} opener calls, each judged: ${judgedCount}; the planted shapes were found`
  );
  return 0;
}

if (import.meta.main) {
  const { modules, workspace } = goModules();
  const go = modules.map((module) => goRun(module, workspace));
  const typescript = typescriptFiles();
  typescript.set(plantedName, PLANTED);
  const status = run({ go, typescript }, JUDGED, (line) => console.log(line), {
    all: process.argv.includes("--all"),
  });
  process.exit(status);
}
