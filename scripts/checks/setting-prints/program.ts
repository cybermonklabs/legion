/**
 * How the search loads what it reads: the tree's root, which of git's files it reads, the
 * TypeScript program of them, and the files this pull request changes, which set its scope.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import ts from "typescript";
import type { Finding } from "./model";

export const root = path.resolve(import.meta.dir, "../../..");

/** Whether git's path is one this search reads. */
export function readPath(file: string): boolean {
  if (!/\.[cm]?tsx?$/.test(file) || /\.d\.[cm]?ts$/.test(file)) return false;
  if (/(^|\/)(__tests__|tests?|fixtures?)\//.test(file)) return false;
  return !/[._](test|spec)\.[cm]?tsx?$/.test(file) && !/\.e2e\.tsx?$/.test(file);
}

/** The program of `files` (absolute paths) and of `extra` files of text that is not on disk, its
 *  type checker, and the source files of those files. */
export function compile(files: readonly string[], extra: ReadonlyMap<string, string>) {
  const options: ts.CompilerOptions = {
    target: ts.ScriptTarget.ESNext,
    module: ts.ModuleKind.ESNext,
    moduleResolution: ts.ModuleResolutionKind.Bundler,
    jsx: ts.JsxEmit.ReactJSX,
    allowImportingTsExtensions: true,
    resolveJsonModule: true,
    skipLibCheck: true,
    noEmit: true,
    strict: true,
    types: ["bun"],
  };
  const host = ts.createCompilerHost(options, true);
  const readFile = host.readFile.bind(host);
  const fileExists = host.fileExists.bind(host);
  const getSourceFile = host.getSourceFile.bind(host);
  host.readFile = (file) => extra.get(file) ?? readFile(file);
  host.fileExists = (file) => extra.has(file) || fileExists(file);
  host.getSourceFile = (file, language, onError, create) => {
    const text = extra.get(file);
    return text === undefined
      ? getSourceFile(file, language, onError, create)
      : ts.createSourceFile(file, text, language, true);
  };
  const program = ts.createProgram([...files, ...extra.keys()], options, host);
  const checker = program.getTypeChecker();
  const ours = new Set(
    [...files, ...extra.keys()]
      .map((file) => program.getSourceFile(file))
      .filter((file): file is ts.SourceFile => file !== undefined)
  );
  return { program, checker, ours };
}

/** The value of the option `name` on the command line, or undefined. */
function option(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index === -1 ? undefined : process.argv[index + 1];
}

/** The files this pull request changes: those `--scope <file>` lists, one a line; else every file
 *  that differs from where the branch left `--base` (origin/main unless named), the working tree's
 *  changes and its new files among them. Undefined when git cannot say. */
export function changedFiles(): Set<string> | undefined {
  const list = option("--scope");
  if (list !== undefined)
    return new Set(
      readFileSync(list, "utf8")
        .split("\n")
        .filter((line) => line !== "")
    );
  const git = (...args: string[]) =>
    Bun.spawnSync(["git", ...args], { cwd: root, stdout: "pipe", stderr: "pipe" });
  const base = git("merge-base", option("--base") ?? "origin/main", "HEAD");
  if (base.exitCode !== 0) return undefined;
  const changed = git("diff", "--name-only", base.stdout.toString().trim());
  const added = git("ls-files", "--others", "--exclude-standard");
  if (changed.exitCode !== 0 || added.exitCode !== 0) return undefined;
  return new Set(
    [...changed.stdout.toString().split("\n"), ...added.stdout.toString().split("\n")].filter(
      (line) => line !== ""
    )
  );
}

/** Whether a finding is in the scope: its line is in a file the pull request changes, or a line of
 *  such a file prints the text it wrote. */
export function inScope(finding: Finding, scope: ReadonlySet<string>): boolean {
  if (scope.has(finding.file)) return true;
  return finding.ends.some((end) =>
    [...end.matchAll(/((?:[\w@.-]+\/)*[\w@.-]+\.(?:[cm]?tsx?|go))\b/g)].some((match) =>
      scope.has(match[1] ?? "")
    )
  );
}
