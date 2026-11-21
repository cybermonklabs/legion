// The programs of this directory a run of rig.sh runs, which rig.sh's services_from records in
// every run's provenance (<run>/services-from), one `<program> shared <dir> <digest>` line each.
// They are read off rig.sh's own text and off each program's imports, never kept in a list: the
// list services_from kept by hand was short by every program rig.sh had newly started to run
// (daemon-pane.ts, score.ts, completion-answers.ts and judge.ts ran on a run's path with no
// digest of theirs recorded anywhere). The set is
//   - every program of the directory rig.sh names as `$here/<name>.ts` or `$here/<name>.go` above
//     its command dispatch, in its functions: `rig.sh health`'s health.ts, which only the
//     dispatch names and no run runs, is not one;
//   - every module of the directory one of those loads, at any depth (`importGraph`), since the
//     digest of a program leaves a change to what it imports unseen (github-standin.ts's
//     github-routes.ts, the gh stand-in's gh-routes.ts and gh-call.ts), the JSON one imports
//     among them (gh-routes.ts's graphql-schema.json, which the gh stand-in's bundle carries);
//   - every Go program of the directory but a test: daemon-pane.ts hands daemon-routes.go,
//     daemon-completion.go and gh-selectors.go to `go run` by their paths, which no import names.
// A program rig.sh names that is not there is in the set all the same, and services_from records
// its digest as `missing`.
import { existsSync, readdirSync, readFileSync, realpathSync } from "node:fs";
import path from "node:path";

/** Every file of this repository that bun loads to run the module `file`, itself among them: each
 * module its source imports, exports from, imports dynamically or requires (`Bun.Transpiler`'s
 * scan, which leaves out an import of types alone, as bun's run does), at any depth, a workspace
 * package's own sources included, and no installed package's; a JSON file one imports is loaded
 * as data and scanned no further. A `#!` first line is left out of the scan, which refuses it, as
 * bun's run skips it. */
export function importGraph(file: string): Set<string> {
  const scanner = new Bun.Transpiler({ loader: "ts" });
  const seen = new Set<string>();
  const visit = (one: string) => {
    if (seen.has(one)) return;
    seen.add(one);
    const source = readFileSync(one, "utf8").replace(/^#!.*/, "");
    for (const { path: specifier } of scanner.scanImports(source)) {
      if (/^(?:node|bun):/.test(specifier)) continue;
      const resolved = realpathSync(Bun.resolveSync(specifier, path.dirname(one)));
      if (resolved.includes(`${path.sep}node_modules${path.sep}`)) continue;
      if (resolved.endsWith(".json")) seen.add(resolved);
      else if (/\.[cm]?[jt]sx?$/.test(resolved)) visit(resolved);
    }
  };
  visit(realpathSync(file));
  return seen;
}

/** rig.sh's command dispatch, the line its commands are told apart at: above it are the script's
 * settings and its functions, and below it each command's one line, `rig.sh health`'s among
 * them. */
const DISPATCH = /^case "\$\{1:-\}" in$/m;
/** A program of rig.sh's own directory, as rig.sh names one. */
const NAMED = /\$here\/([\w.-]+\.(?:ts|go))\b/g;

/** The programs of the directory of the rig.sh at `rig` that a run of it runs (this module's
 * opening comment says which), by name, in byte order. Throws for a rig.sh with no command
 * dispatch, whose functions cannot be told from its commands, and for an import of one of its
 * programs that does not resolve. */
export function runPrograms(rig: string): string[] {
  const dir = realpathSync(path.dirname(rig));
  const text = readFileSync(rig, "utf8");
  const dispatch = text.search(DISPATCH);
  if (dispatch === -1)
    throw new Error(
      `${rig} holds no command dispatch, so which programs its functions name is unknown`
    );
  const programs = new Set<string>();
  for (const [, name = ""] of text.slice(0, dispatch).matchAll(NAMED)) {
    const file = path.join(dir, name);
    if (name.endsWith(".go") || !existsSync(file)) programs.add(name);
    else
      for (const module of importGraph(file))
        if (path.dirname(module) === dir) programs.add(path.basename(module));
  }
  for (const entry of readdirSync(dir, { withFileTypes: true }))
    if (entry.isFile() && entry.name.endsWith(".go") && !entry.name.endsWith("_test.go"))
      programs.add(entry.name);
  return [...programs].sort();
}

if (import.meta.main) {
  const [rig, ...rest] = Bun.argv.slice(2);
  if (rig === undefined || rest.length > 0) {
    console.error("usage: bun run-programs.ts <rig.sh>");
    process.exit(2);
  }
  try {
    for (const program of runPrograms(rig)) console.log(program);
  } catch (error) {
    console.error(`run-programs: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}
