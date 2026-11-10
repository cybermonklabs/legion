/** Every committed recording of the stubbed layer, held to the generated contract.
 *
 *  dispatch://INFRA-1406's generated types make a response shape a compiler reads, so a shape
 *  that changes breaks the client in the same commit. A recording is not code: its bodies are
 *  JSON in a file nothing compiles, so a changed shape would leave every recording stale and
 *  every stubbed row green until the nightly re-records. This test is what closes that window -
 *  it runs on every pull request that touches packages/dispatch, and the drift fails on the
 *  commit that causes it rather than hours later.
 *
 *  What it refuses, per entry: a path that matches no route of
 *  `packages/contracts/schemas/dispatch-openapi.json`, a method that route does not take, a
 *  status that operation does not declare, a JSON body that fails that status's schema, and a
 *  200 that answers a stored text without the display tree the contract declares beside it.
 *
 *  That last one is what keeps the stubbed rows reading Go's tree. The dashboard asks for the
 *  trees on every request it makes, and draws one wherever it is given one; where it is given
 *  none it parses the markdown itself, which is the fallback a rolling deploy needs and which
 *  every row recorded against a server that sends no tree would silently be proving instead.
 *  A recording is the whole server those rows see, so a tree missing from one is a row that
 *  reads the browser's parse while its spec says it reads the server's.
 *
 *  The schema is reached by a JSON pointer into the document itself, never copied out of it: a
 *  response schema is almost always a `$ref` into `components`, and a copy would carry the
 *  reference without the components it points at.
 */

import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { Ajv2020 } from "ajv/dist/2020";

import { recordedOrigin } from "../stub/origin";
import type { Har } from "../stub/recording";
import { recordingsDirectory, recordingsUnder } from "../stub/recordings";

interface OpenApiDocument {
  paths: Record<string, Record<string, OpenApiOperation | undefined> | undefined>;
}

interface OpenApiOperation {
  responses?: Record<string, { content?: Record<string, { schema?: unknown } | undefined> }>;
}

const documentPath = fileURLToPath(
  new URL("../../../contracts/schemas/dispatch-openapi.json", import.meta.url)
);
const document = JSON.parse(readFileSync(documentPath, "utf8")) as OpenApiDocument;

const recordings = recordingsDirectory();

/** Every committed recording, by its path relative to that directory. */
const files = recordingsUnder(recordings);

/** The document's routes with their segments, split once. `routeOf` runs for every entry of
 *  every recording - thousands over a hundred and fifty routes - and splitting the table inside
 *  it cost 424 ms of this test's run against 67 ms with the split hoisted. */
const routeTable = Object.keys(document.paths).map((route) => ({
  route,
  segments: route.split("/"),
}));

/** The templated route of the document that `path` is an instance of, or undefined where none
 *  is. A route's `{name}` segment takes one path segment, which is what the Go route table
 *  writes and what the generator copies. The route with the most literal segments wins, so a
 *  path that two routes match is read as the more specific one's. */
function routeOf(path: string): string | undefined {
  const wanted = path.split("/");
  let best: string | undefined;
  let bestLiterals = -1;
  for (const { route, segments } of routeTable) {
    if (segments.length !== wanted.length) continue;
    let literals = 0;
    let matches = true;
    for (const [index, segment] of segments.entries()) {
      if (segment.startsWith("{") && segment.endsWith("}")) continue;
      if (segment !== wanted[index]) {
        matches = false;
        break;
      }
      literals += 1;
    }
    if (matches && literals > bestLiterals) {
      best = route;
      bestLiterals = literals;
    }
  }
  return best;
}

/** One step of a JSON pointer, escaped as RFC 6901 writes it. */
function step(name: string): string {
  return name.replaceAll("~", "~0").replaceAll("/", "~1");
}

/** The pointer at one operation's response schema inside the document registered as `root`. Two
 *  describes below read the same schema under two registrations, so the pointer is written once:
 *  a second spelling of it would fail as "the contract holds no schema" rather than as a typo. */
function schemaPointer(root: string, route: string, method: string, code: string): string {
  return (
    `${root}#/paths/${step(route)}/${method}/responses/${step(code)}` +
    `/content/${step("application/json")}/schema`
  );
}

/** Every stored text the server answers a display tree beside, named by the text's own field:
 *  a property whose name is another property's with `_tree` on the end, in the schema that
 *  declares both. Read out of the document rather than written down here, so a text that gains
 *  a tree is covered by the commit that declares it and never by a list a reader has to keep. */
function storedTextsWithTrees(node: unknown, found: Set<string>): Set<string> {
  if (node === null || typeof node !== "object") return found;
  if (Array.isArray(node)) {
    for (const held of node) storedTextsWithTrees(held, found);
    return found;
  }
  if ("properties" in node) {
    const properties = node.properties;
    if (properties !== null && typeof properties === "object") {
      for (const name of Object.keys(properties)) {
        if (name.endsWith("_tree") && Object.hasOwn(properties, name.slice(0, -"_tree".length))) {
          found.add(name.slice(0, -"_tree".length));
        }
      }
    }
  }
  for (const held of Object.values(node)) storedTextsWithTrees(held, found);
  return found;
}

const storedTexts = storedTextsWithTrees(document, new Set<string>());

/** The document again, with one demand added at every schema that declares such a pair: a body
 *  that answers the text must answer its tree beside it. A text the server stored as nothing has
 *  no tree to read and the server sends none, so the demand is made of a non-empty string alone,
 *  and a field that is absent or null demands nothing.
 *
 *  The demand is an `allOf` branch and never a rewritten `required`, so everything else the
 *  schema says is left as the generator wrote it and every `$ref` still resolves: ajv reads this
 *  document as it reads the first, under the pointer the first is read by. */
function demandingTrees(node: unknown): unknown {
  if (node === null || typeof node !== "object") return node;
  if (Array.isArray(node)) return node.map(demandingTrees);
  const copied: Record<string, unknown> = {};
  for (const [key, held] of Object.entries(node)) copied[key] = demandingTrees(held);
  const properties = copied.properties;
  if (properties === null || typeof properties !== "object" || Array.isArray(properties)) {
    return copied;
  }
  const demanded = Object.keys(properties)
    .filter((name) => Object.hasOwn(properties, `${name}_tree`))
    .map((name) => ({
      if: { properties: { [name]: { type: "string", minLength: 1 } }, required: [name] },
      // `then` here is JSON Schema's own keyword beside `if`, and this object is handed to ajv
      // rather than awaited: the rule guards against an object a `await` would mistake for a
      // promise, which nothing here does.
      // biome-ignore lint/suspicious/noThenProperty: the JSON Schema keyword, never a thenable
      then: { required: [`${name}_tree`] },
    }));
  if (demanded.length === 0) return copied;
  const held = Array.isArray(copied.allOf) ? copied.allOf : [];
  return { ...copied, allOf: [...held, ...demanded] };
}

// `demandingTrees` walks every node of the document and so answers `unknown`; the document it
// was given is an object, so its clone is one.
const demandedDocument = demandingTrees(document) as Record<string, unknown>;
const withTrees = new Ajv2020({ allErrors: false, strict: false, validateFormats: false });
withTrees.addSchema({ $id: "trees", ...demandedDocument });

const ajv = new Ajv2020({ allErrors: false, strict: false, validateFormats: false });
ajv.addSchema({ $id: "dispatch", ...document });

describe("every committed recording answers what the generated contract declares", () => {
  // An empty recordings directory is the state of this tree before the first family moved, and a
  // suite that passed over it silently would also pass over a directory a rename emptied.
  test("there is at least one recording to check", () => {
    expect(files.length).toBeGreaterThan(0);
  });

  for (const file of files) {
    test(file, () => {
      const har = JSON.parse(readFileSync(join(recordings, file), "utf8")) as Har;
      for (const entry of har.log.entries) {
        const url = new URL(entry.request.url);
        const where = `${entry.request.method} ${url.pathname} in ${file}`;
        expect(url.origin, `${where}: a committed recording names the placeholder origin`).toBe(
          recordedOrigin
        );
        // The generator covers the `/api/v1` route table and nothing else, so the sign-in
        // route it leaves out is named here rather than passed over: a recording that reached
        // some other unguarded route would otherwise be checked by nothing at all.
        if (!url.pathname.startsWith("/api/v1/")) {
          expect(url.pathname, `${where}: no contract covers this route`).toBe("/auth/whoami");
          continue;
        }
        const route = routeOf(url.pathname);
        expect(route, `${where}: no route of the generated contract takes this path`).toBeDefined();
        if (route === undefined) continue;
        const method = entry.request.method.toLowerCase();
        const operation = document.paths[route]?.[method];
        expect(operation, `${where}: the route declares no such method`).toBeDefined();
        const status = String(entry.response.status);
        const declared = operation?.responses ?? {};
        const code = declared[status] === undefined ? "default" : status;
        expect(
          declared[code],
          `${where}: the operation declares no ${status} and no default`
        ).toBeDefined();
        const body = entry.response.content.text;
        const mime = entry.response.content.mimeType ?? "";
        if (
          declared[code]?.content?.["application/json"]?.schema === undefined ||
          body === undefined ||
          body.trim() === "" ||
          !mime.includes("json")
        ) {
          continue;
        }
        const pointer = schemaPointer("dispatch", route, method, code);
        const validate = ajv.getSchema(pointer);
        expect(validate, `${where}: the contract holds no schema at ${pointer}`).toBeDefined();
        if (validate === undefined) continue;
        expect(
          validate(JSON.parse(body)),
          `${where}: the body fails the schema - ${ajv.errorsText(validate.errors)}`
        ).toBe(true);
      }
    });
  }
});

describe("every committed recording answers the display tree of every stored text", () => {
  // The demand above is only as wide as the pairs the document declares, and a derivation that
  // found none would make every row below pass over every recording. These are the texts this
  // contract answers a tree beside today; a commit that adds one adds it here, and a commit that
  // takes a tree away has to say so here rather than leave the rows guarding nothing.
  test("the contract declares which stored texts carry a tree", () => {
    expect([...storedTexts].sort()).toEqual([
      "body",
      "description",
      "label",
      "markdown",
      "prose",
      "question",
      "reason",
      "snippet",
      "text",
    ]);
  });

  for (const file of files) {
    test(file, () => {
      const har = JSON.parse(readFileSync(join(recordings, file), "utf8")) as Har;
      for (const entry of har.log.entries) {
        // A 200 alone: an error body carries no stored text of the issue the row reads, and the
        // rows above are what hold every other status to its own schema. An entry this passes
        // over - a route the contract does not cover, an answer that is no JSON - is one they
        // have already refused, so nothing is left unguarded by the quiet `continue`s here.
        if (entry.response.status !== 200) continue;
        const route = routeOf(new URL(entry.request.url).pathname);
        if (route === undefined) continue;
        const method = entry.request.method.toLowerCase();
        const declared = document.paths[route]?.[method]?.responses ?? {};
        const code = declared["200"] === undefined ? "default" : "200";
        const body = entry.response.content.text;
        if (
          declared[code]?.content?.["application/json"]?.schema === undefined ||
          body === undefined ||
          body.trim() === "" ||
          !(entry.response.content.mimeType ?? "").includes("json")
        ) {
          continue;
        }
        const validate = withTrees.getSchema(schemaPointer("trees", route, method, code));
        if (validate === undefined) continue;
        const where = `${entry.request.method} ${new URL(entry.request.url).pathname} in ${file}`;
        expect(
          validate(JSON.parse(body)),
          `${where}: a stored text is answered without its display tree, so the row that reads ` +
            `it reads the browser's own parse - ${withTrees.errorsText(validate.errors)}`
        ).toBe(true);
      }
    });
  }
});
