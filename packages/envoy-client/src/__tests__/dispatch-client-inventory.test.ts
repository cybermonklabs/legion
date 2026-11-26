// Every production request to Dispatch goes through the private-project check
// (src/private-check.ts): every construction of the client through `checkedDispatchClient`, and
// every request a file sends around the client through `checkedDispatchFetch`. The scan lists
// every tracked file of the repository with `git ls-files`, so a new file is read the day it is
// added, and it fails UNKNOWN when it read nothing it could judge.
import { describe, expect, test } from "bun:test";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { dirname, join, normalize } from "node:path";
import ts from "typescript";

/** The suffixes of the files the scan reads. */
const SOURCE = /\.(?:ts|tsx|mts|cts|js|jsx|mjs|cjs)$/;

/** A path segment that holds tests and stand-ins, which build the client as they please. */
const TEST_SEGMENTS: Readonly<Record<string, true>> = {
  __tests__: true,
  test: true,
  tests: true,
  e2e: true,
};

/**
 * The files under a test segment that dial production Dispatch, and so are held to both rules:
 * the production check of Dispatch's keyed writes (scripts/e2e/README.md, "lib/dispatch-keyed-
 * comment.ts"), which posts a comment on production with an operator's bearer, and the live
 * event probe (lib/dispatch-live-events.ts), which a stage proof's audit runs on production's
 * `/ws/live` with the operator's bearer. Every other file of the `e2e` trees dials a stack of its
 * own run (packages/dispatch/e2e truncates the database of the server it names).
 */
const PRODUCTION_UNDER_TEST_SEGMENTS: Readonly<Record<string, true>> = {
  "scripts/e2e/lib/dispatch-keyed-comment.ts": true,
  "scripts/e2e/lib/dispatch-live-events.ts": true,
};

/** The trees rule 2 leaves out, each for its reason. */
const OUTSIDE_RULE_2: readonly { readonly tree: string; readonly why: string }[] = [
  { tree: "packages/envoy-client/src/", why: "the client and its check themselves" },
  { tree: "packages/contracts/", why: "declares the routes and sends no request" },
  {
    tree: "packages/dispatch/web/",
    why: "runs in a person's browser under the person's own sign-in, which is operator access",
  },
  { tree: "docs/", why: "sends no request" },
];

/**
 * The files rule 2 leaves out by name, each with its reason, which the test reads: a file named
 * here must still be in the tree and still import no envoy-client module, or the test fails.
 */
const OUTSIDE_RULE_2_FILES: Readonly<Record<string, string>> = {
  "packages/envoy/scripts/approve/scan.ts":
    "approve-check's scan presents only broker tokens (`Authorization: DPoP`), which the check admits without asking anything, and loads no part of envoy-client on purpose, so the gate that runs it keeps its narrow read (packages/envoy/scripts/approve/dispatch-env.ts)",
};

/** The one Dispatch reader of each of their trees, whose importers it reads for. */
const READER_MODULES: Readonly<Record<string, true>> = {
  "scripts/approval-eval/dispatch-read.ts": true,
  "packages/pi-legion/scripts/skill-scenarios/production-dispatch.ts": true,
};

/** The file and the function that alone may construct the client unchecked. */
const FACTORY_FILE = "packages/envoy-client/src/dispatch-http.ts";
const FACTORY = "checkedDispatchClient";

const CONSTRUCTION = /\bnew\s+(?:[A-Za-z_$][\w$]*\.)?DispatchClient\s*\(/g;
const API_PATH = /\/api\/v1\/|\/ws\/live|\/ws\/doc\//;
const CHECKED_NAMES: Readonly<Record<string, true>> = {
  checkedDispatchClient: true,
  checkedDispatchFetch: true,
  checkDispatchSocket: true,
};

const ROOT = execFileSync("git", ["rev-parse", "--show-toplevel"], {
  cwd: import.meta.dir,
  encoding: "utf8",
}).trim();

/** Every production file of `listed`: a source suffix, and no test segment or test file name,
 *  but for the files under a test segment that dial production. */
function productionFiles(listed: readonly string[]): string[] {
  return listed.filter((path) => {
    if (!SOURCE.test(path)) return false;
    if (PRODUCTION_UNDER_TEST_SEGMENTS[path] === true) return true;
    const segments = path.split("/");
    const name = segments.at(-1) ?? "";
    return (
      !segments.slice(0, -1).some((segment) => TEST_SEGMENTS[segment] === true) &&
      !name.includes(".test.") &&
      !name.includes(".e2e.")
    );
  });
}

function scriptKind(path: string): ts.ScriptKind {
  if (path.endsWith(".tsx")) return ts.ScriptKind.TSX;
  if (path.endsWith(".jsx")) return ts.ScriptKind.JSX;
  if (/\.[mc]?js$/.test(path)) return ts.ScriptKind.JS;
  return ts.ScriptKind.TS;
}

/** One file read for the two rules: its text with every comment blanked (line breaks kept, so a
 *  match's line is the file's), the span of the factory where it declares it, the checked names
 *  it imports, whether it sends a request itself (a call of `fetch`, or of `checkedDispatchFetch`)
 *  or opens a socket itself (a `new …WebSocket(…)`), and every module it imports. */
interface ReadFile {
  readonly code: string;
  readonly factory: { readonly start: number; readonly end: number } | undefined;
  readonly importsChecked: boolean;
  readonly importsCheckedFetch: boolean;
  readonly importsCheckedSocket: boolean;
  readonly sendsItself: boolean;
  readonly opensSocket: boolean;
  readonly imported: readonly string[];
}

function readSource(path: string, text: string): ReadFile {
  const source = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true, scriptKind(path));
  const comments = new Map<number, number>();
  const note = (ranges: readonly ts.CommentRange[] | undefined): void => {
    for (const range of ranges ?? []) comments.set(range.pos, range.end);
  };
  let factory: ReadFile["factory"];
  let importsChecked = false;
  let importsCheckedFetch = false;
  let importsCheckedSocket = false;
  let sendsItself = false;
  let opensSocket = false;
  const imported: string[] = [];
  const visit = (node: ts.Node): void => {
    note(ts.getLeadingCommentRanges(text, node.getFullStart()));
    note(ts.getTrailingCommentRanges(text, node.getEnd()));
    if (ts.isFunctionDeclaration(node) && node.name?.text === FACTORY) {
      factory = { start: node.getStart(source), end: node.getEnd() };
    }
    if (ts.isCallExpression(node)) {
      // `fetch(…)`, `globalThis.fetch(…)`, `Bun.fetch(…)`, or `checkedDispatchFetch(…)(…)`.
      const callee = ts.isPropertyAccessExpression(node.expression)
        ? node.expression.name.text
        : ts.isIdentifier(node.expression)
          ? node.expression.text
          : undefined;
      if (callee === "fetch" || callee === "checkedDispatchFetch") sendsItself = true;
    }
    if (
      ts.isNewExpression(node) &&
      ts.isIdentifier(node.expression) &&
      node.expression.text.endsWith("WebSocket")
    ) {
      opensSocket = true;
    }
    if (ts.isImportDeclaration(node) && ts.isStringLiteral(node.moduleSpecifier)) {
      imported.push(node.moduleSpecifier.text);
      const clause = node.importClause;
      const named = clause?.namedBindings;
      if (clause !== undefined && !clause.isTypeOnly && named && ts.isNamedImports(named)) {
        for (const element of named.elements) {
          const name = (element.propertyName ?? element.name).text;
          if (element.isTypeOnly || CHECKED_NAMES[name] !== true) continue;
          importsChecked = true;
          if (name === "checkedDispatchFetch") importsCheckedFetch = true;
          if (name === "checkDispatchSocket") importsCheckedSocket = true;
        }
      }
    }
    for (const child of node.getChildren(source)) visit(child);
  };
  visit(source);
  // Each comment blanked once, in order, so a long file is copied once rather than per comment.
  let code = "";
  let at = 0;
  for (const [start, end] of [...comments].sort(([a], [b]) => a - b)) {
    if (start < at) continue;
    code += text.slice(at, start) + text.slice(start, end).replace(/[^\n]/g, " ");
    at = end;
  }
  code += text.slice(at);
  return {
    code,
    factory,
    importsChecked,
    importsCheckedFetch,
    importsCheckedSocket,
    sendsItself,
    opensSocket,
    imported,
  };
}

/** The repository path an import of `specifier` from `from` names, for a relative import: with
 *  its own suffix, or the `.ts` the import leaves out. */
function importedPath(from: string, specifier: string): readonly string[] {
  if (!specifier.startsWith(".")) return [];
  const base = normalize(join(dirname(from), specifier));
  return SOURCE.test(base) ? [base] : [`${base}.ts`, `${base}/index.ts`];
}

interface Scan {
  /** The production files read. */
  readonly files: number;
  /** Rule 1: each construction outside the factory, `file:line`. */
  readonly constructions: readonly string[];
  /** How many constructions the factory itself holds. */
  readonly inFactory: number;
  /** Rule 2: each file that holds a Dispatch API path and reaches no check: one that sends a
   *  request itself without importing `checkedDispatchFetch`, one that opens a socket itself
   *  without importing `checkDispatchSocket`, or any other that imports no checked name nor a
   *  reader module that does. */
  readonly unchecked: readonly string[];
  /** Whether rule 2 found a path in the client itself. */
  readonly clientHoldsAPath: boolean;
}

/** The two rules over `listed`, each file read through `read`. */
function scan(listed: readonly string[], read: (path: string) => string): Scan {
  const files = productionFiles(listed);
  const constructions: string[] = [];
  let inFactory = 0;
  let clientHoldsAPath = false;
  const holdingPaths: { readonly path: string; readonly file: ReadFile }[] = [];
  const reachesCheck = new Map<string, boolean>();
  for (const path of files) {
    const file = readSource(path, read(path));
    for (const match of file.code.matchAll(CONSTRUCTION)) {
      const at = match.index;
      if (
        path === FACTORY_FILE &&
        file.factory !== undefined &&
        at >= file.factory.start &&
        at < file.factory.end
      ) {
        inFactory += 1;
        continue;
      }
      constructions.push(`${path}:${file.code.slice(0, at).split("\n").length}`);
    }
    reachesCheck.set(path, file.importsChecked);
    if (!API_PATH.test(file.code)) continue;
    if (path === FACTORY_FILE) clientHoldsAPath = true;
    if (OUTSIDE_RULE_2.some(({ tree }) => path.startsWith(tree))) continue;
    if (Object.hasOwn(OUTSIDE_RULE_2_FILES, path)) continue;
    holdingPaths.push({ path, file });
  }
  const unchecked: string[] = [];
  for (const { path, file } of holdingPaths) {
    // A request or a socket the file opens itself is checked only by the check it calls: the
    // factory or a reader it imports checks only the requests they send.
    if (file.opensSocket && !file.importsCheckedSocket) {
      unchecked.push(`${path} opens a socket itself without checkDispatchSocket`);
    }
    if (file.sendsItself && !file.importsCheckedFetch) {
      unchecked.push(`${path} sends a request itself without checkedDispatchFetch`);
    }
    if (file.sendsItself || file.opensSocket) continue;
    if (file.importsChecked) continue;
    const readers = file.imported.flatMap((specifier) => importedPath(path, specifier));
    if (
      readers.some((reader) => READER_MODULES[reader] === true && reachesCheck.get(reader) === true)
    )
      continue;
    unchecked.push(
      `${path} holds a Dispatch API path and imports neither checkedDispatchClient, checkedDispatchFetch nor a reader module that does`
    );
  }
  return { files: files.length, constructions, inFactory, unchecked, clientHoldsAPath };
}

/** What the scan proves, or the UNKNOWN that a scan which read nothing it could judge is. */
function verdict(result: Scan): string[] {
  const unknown: string[] = [];
  if (result.files === 0) unknown.push("UNKNOWN: git ls-files listed no production file");
  if (result.inFactory === 0) {
    unknown.push(`UNKNOWN: rule 1 found no construction inside ${FACTORY} in ${FACTORY_FILE}`);
  }
  if (!result.clientHoldsAPath) {
    unknown.push(`UNKNOWN: rule 2 found no Dispatch API path in ${FACTORY_FILE}`);
  }
  if (unknown.length > 0) return unknown;
  return [
    ...result.constructions.map(
      (at) => `rule 1: ${at} constructs DispatchClient outside ${FACTORY}`
    ),
    ...result.unchecked.map((why) => `rule 2: ${why}`),
  ];
}

// Every tracked path, some 18,000 of them: more than execFileSync's 1 MiB default buffer holds.
const LISTED = execFileSync("git", ["ls-files", "-z"], {
  cwd: ROOT,
  encoding: "utf8",
  maxBuffer: 64 << 20,
})
  .split("\0")
  .filter((path) => path !== "");
const fromDisk = (path: string): string => readFileSync(join(ROOT, path), "utf8");

/** The scan of the tree with `planted` read in place of the files it names, or added. */
function scanWith(planted: Readonly<Record<string, string>>): string[] {
  const listed = [...new Set([...LISTED, ...Object.keys(planted)])];
  return verdict(scan(listed, (path) => planted[path] ?? fromDisk(path)));
}

const SCAN_MS = 120_000;

describe("every production request to Dispatch goes through the private-project check", () => {
  test(
    "no file constructs the client outside the factory, and no file sends its own request around the check",
    () => {
      const result = scan(LISTED, fromDisk);
      expect(verdict(result)).toEqual([]);
      // The scan read the tree: today's count is printed for the run's record.
      console.log(
        `dispatch-client inventory: ${result.files} production files, ${result.inFactory} construction in ${FACTORY}`
      );
    },
    SCAN_MS
  );

  test("each file left out of rule 2 by name is still there, and still loads no envoy-client module", () => {
    for (const [path, why] of Object.entries(OUTSIDE_RULE_2_FILES)) {
      expect(LISTED, `${path} is left out because ${why}`).toContain(path);
      const { imported } = readSource(path, fromDisk(path));
      expect(
        imported.filter((specifier) => specifier.includes("envoy-client")),
        `${path} is left out because ${why}`
      ).toEqual([]);
    }
  });

  test(
    "a socket opened without checkDispatchSocket is named",
    () => {
      const probe = "scripts/e2e/lib/dispatch-live-events.ts";
      const unchecked = fromDisk(probe).replace(/\n {2}checkDispatchSocket,/, "");
      expect(unchecked).not.toContain("\n  checkDispatchSocket,");
      expect(scanWith({ [probe]: unchecked })).toEqual([
        `rule 2: ${probe} opens a socket itself without checkDispatchSocket`,
      ]);
    },
    SCAN_MS
  );

  // The controls, run in every run: each planted fault is named, so the scan can fail for the
  // reason it claims.
  test(
    "a construction written back into a production file is named by file and line",
    () => {
      const path = "packages/pi-shared/src/handoff-record.ts";
      const text = fromDisk(path);
      const line = text.split("\n").length + 1;
      expect(
        scanWith({
          [path]: `${text}\nconst planted = new DispatchClient("", undefined as never);\n`,
        })
      ).toEqual([`rule 1: ${path}:${line} constructs DispatchClient outside ${FACTORY}`]);
    },
    SCAN_MS
  );

  test(
    "a production file whose name begins with test, and one that is not .ts, are read",
    () => {
      const call = `import { DispatchClient } from "@legion/envoy-client/dispatch-http";\nexport const c = new DispatchClient("", undefined);\n`;
      expect(
        scanWith({
          "packages/pi-shared/src/testimony.ts": call,
          "packages/pi-shared/src/probe.mjs": call,
        })
      ).toEqual([
        `rule 1: packages/pi-shared/src/testimony.ts:2 constructs DispatchClient outside ${FACTORY}`,
        `rule 1: packages/pi-shared/src/probe.mjs:2 constructs DispatchClient outside ${FACTORY}`,
      ]);
    },
    SCAN_MS
  );

  test(
    "a file that sends its own request without the check is named, and a comment is no request",
    () => {
      const census = "scripts/dispatch/ask-census.ts";
      const unchecked = fromDisk(census).replace(
        /import \{ checkedDispatchFetch \} from "[^"]+";\n/,
        ""
      );
      expect(unchecked).not.toContain("import { checkedDispatchFetch }");
      const commentOnly = "packages/pi-shared/src/comment-only.ts";
      expect(
        scanWith({
          [census]: unchecked,
          [commentOnly]: "// GET /api/v1/issues is read elsewhere\nexport const x = 1;\n",
        })
      ).toEqual([`rule 2: ${census} sends a request itself without checkedDispatchFetch`]);
    },
    SCAN_MS
  );

  test(
    "a file that names a path and reaches no check, through no reader either, is named",
    () => {
      const path = "packages/pi-shared/src/reads-dispatch.ts";
      expect(
        scanWith({
          [path]: `import { DispatchClient } from "@legion/envoy-client/dispatch-http";\nexport const route = "/api/v1/issues";\nexport type Client = DispatchClient;\n`,
        })
      ).toEqual([
        `rule 2: ${path} holds a Dispatch API path and imports neither checkedDispatchClient, checkedDispatchFetch nor a reader module that does`,
      ]);
    },
    SCAN_MS
  );

  test("a scan that lists no file is UNKNOWN, never a pass", () => {
    expect(verdict(scan([], fromDisk))).toEqual([
      "UNKNOWN: git ls-files listed no production file",
      `UNKNOWN: rule 1 found no construction inside ${FACTORY} in ${FACTORY_FILE}`,
      `UNKNOWN: rule 2 found no Dispatch API path in ${FACTORY_FILE}`,
    ]);
  });
});
