/** The specs whose rows need a NATS, held to the one list that puts them in `chromium-nats`
 *  (`natsSpecNames`, e2e/configurations.ts, which says why those rows are a project of their own).
 *
 *  A row needs the bus where it calls `requireNats` (e2e/legion-state-feed.ts), or a function of
 *  that module that calls it first (`openStateFeed`). A spec that does so and is missing from the
 *  list stays in `chromium`, whose CI legs start no NATS, so its rows fail there by
 *  `requireNats`'s message; a name the list holds that no such spec answers to is a project
 *  selecting nothing. Both fail here, and so does a project other than `chromium-nats` that would
 *  select one of those files, and a CI that runs the project on no leg.
 *
 *  It reads the specs' source, the configuration it imports and two files of .github, and starts
 *  nothing. */
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import * as ts from "typescript";

import { natsProjectName, natsSpecNames, natsSpecPattern } from "../configurations";
import { harnessProcessCount } from "../harness-processes";
import { specFiles } from "../no-browser-rows";
import realConfig from "../playwright.config";

const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));
const feedModule = "./legion-state-feed";
const feedPath = join(e2eDirectory, "legion-state-feed.ts");
const browserWorkflowPath = fileURLToPath(
  new URL("../../../../.github/workflows/dispatch-browser.yaml", import.meta.url)
);
const shardSuitePath = fileURLToPath(
  new URL("../../../../.github/scripts/dispatch-browser-shard-suite.sh", import.meta.url)
);

/** Whether `node` or anything below it calls one of `callees` by name, or one of `members` as a
 *  member of one of `namespaces`. */
function calls(
  node: ts.Node,
  callees: ReadonlySet<string>,
  namespaces: ReadonlySet<string>,
  members: ReadonlySet<string>
): boolean {
  let found = false;
  const visit = (child: ts.Node): void => {
    if (found) return;
    if (ts.isCallExpression(child)) {
      const callee = child.expression;
      if (ts.isIdentifier(callee) && callees.has(callee.text)) found = true;
      if (
        ts.isPropertyAccessExpression(callee) &&
        ts.isIdentifier(callee.expression) &&
        namespaces.has(callee.expression.text) &&
        members.has(callee.name.text)
      ) {
        found = true;
      }
    }
    ts.forEachChild(child, visit);
  };
  visit(node);
  return found;
}

/** `requireNats` and every exported function of e2e/legion-state-feed.ts that calls it: the names
 *  whose call needs the bus. */
const busFunctions: ReadonlySet<string> = (() => {
  const names = new Set(["requireNats"]);
  const source = ts.createSourceFile(
    feedPath,
    readFileSync(feedPath, "utf8"),
    ts.ScriptTarget.Latest,
    true
  );
  for (const statement of source.statements) {
    if (
      ts.isFunctionDeclaration(statement) &&
      statement.name !== undefined &&
      statement.modifiers?.some((modifier) => modifier.kind === ts.SyntaxKind.ExportKeyword) &&
      statement.body !== undefined &&
      calls(statement.body, names, new Set(), new Set())
    ) {
      names.add(statement.name.text);
    }
  }
  return names;
})();

/** Whether the spec at `path` calls one of `busFunctions`, under whatever local name it imports it
 *  by from e2e/legion-state-feed.ts, or through a namespace import of that module. */
function needsTheBus(path: string): boolean {
  const source = ts.createSourceFile(
    path,
    readFileSync(path, "utf8"),
    ts.ScriptTarget.Latest,
    true
  );
  const locals = new Set<string>();
  const namespaces = new Set<string>();
  for (const statement of source.statements) {
    if (
      !ts.isImportDeclaration(statement) ||
      !ts.isStringLiteral(statement.moduleSpecifier) ||
      statement.moduleSpecifier.text.replace(/\.ts$/, "") !== feedModule
    ) {
      continue;
    }
    const bindings = statement.importClause?.namedBindings;
    if (bindings === undefined) continue;
    if (ts.isNamespaceImport(bindings)) {
      namespaces.add(bindings.name.text);
      continue;
    }
    for (const element of bindings.elements) {
      if (busFunctions.has((element.propertyName ?? element.name).text)) {
        locals.add(element.name.text);
      }
    }
  }
  return calls(source, locals, namespaces, busFunctions);
}

/** A project's `testMatch` or `testIgnore` as the RegExps it holds. Every one of the config is a
 *  RegExp at this head; a glob cannot be read from its text, so it is refused by name, as
 *  e2e/__tests__/row-configurations.test.ts refuses one. */
function patterns(
  given: string | RegExp | (string | RegExp)[] | undefined,
  what: string
): RegExp[] {
  if (given === undefined) return [];
  return (Array.isArray(given) ? given : [given]).map((pattern: string | RegExp) => {
    if (typeof pattern === "string") {
      throw new Error(
        `${what} is the glob ${JSON.stringify(pattern)}, which this file cannot read; write it ` +
          "as a RegExp, as every pattern of e2e/playwright.config.ts is."
      );
    }
    return pattern;
  });
}

test("the specs that call requireNats are natsSpecNames, each once", () => {
  expect(busFunctions.has("openStateFeed")).toBe(true);
  const needing = specFiles(e2eDirectory)
    .filter((file) => needsTheBus(join(e2eDirectory, file)))
    .map((file) => file.replace(/\.e2e\.ts$/, ""));
  expect(
    needing,
    "These specs call requireNats, or a function of e2e/legion-state-feed.ts that calls it, and " +
      "`natsSpecNames` (e2e/configurations.ts) does not list each of them, or lists a name no such " +
      "spec has. A spec missing there stays in `chromium`, whose CI legs start no NATS, and its " +
      "rows fail by requireNats's message; write each such spec's name there, and only those."
  ).toEqual([...natsSpecNames].sort());
  expect(new Set(natsSpecNames).size).toBe(natsSpecNames.length);
});

test("the pattern selects each listed spec and no other spec", () => {
  const selected = specFiles(e2eDirectory).filter((file) =>
    natsSpecPattern.test(join(e2eDirectory, file))
  );
  expect(selected).toEqual(natsSpecNames.map((name) => `${name}.e2e.ts`).sort());
});

// A project's `testMatch` and `testIgnore` replace the config's own, as Playwright reads them, and
// each is matched against the file's absolute path. Two-process mode declares the multiprocess
// project alone, which selects none of these files.
test("chromium-nats alone selects the NATS specs", () => {
  for (const name of natsSpecNames) {
    const path = join(e2eDirectory, `${name}.e2e.ts`);
    const selecting = (realConfig.projects ?? []).flatMap((project) => {
      const named = project.name ?? "a project with no name";
      const match = patterns(project.testMatch ?? realConfig.testMatch, `${named}'s testMatch`);
      const ignore = patterns(project.testIgnore ?? realConfig.testIgnore, `${named}'s testIgnore`);
      return match.some((pattern) => pattern.test(path)) &&
        !ignore.some((pattern) => pattern.test(path))
        ? [named]
        : [];
    });
    expect(
      selecting,
      `the projects of e2e/playwright.config.ts that select ${name}.e2e.ts: every project but ` +
        `${natsProjectName} that would see the file names natsSpecPattern in its testIgnore`
    ).toEqual(harnessProcessCount === 1 ? [natsProjectName] : []);
  }
});

// The alone job of .github/workflows/dispatch-browser.yaml runs a leg for each `project` of its
// matrix's `include`, and the shard script names each lane's projects as `--project=<name>`.
test("CI runs chromium-nats on a leg of the alone job and on no shard leg", () => {
  const workflow = Bun.YAML.parse(readFileSync(browserWorkflowPath, "utf8")) as {
    jobs?: { alone?: { strategy?: { matrix?: { include?: { project?: unknown }[] } } } };
  } | null;
  const aloneLegs = (workflow?.jobs?.alone?.strategy?.matrix?.include ?? []).map((leg) =>
    String(leg.project)
  );
  expect(aloneLegs).toContain(natsProjectName);
  const laneProjects = [
    ...readFileSync(shardSuitePath, "utf8").matchAll(/--project=([\w-]+)/g),
  ].map((found) => found[1]);
  expect(laneProjects.length).toBeGreaterThan(0);
  expect(laneProjects).not.toContain(natsProjectName);
});
