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
 *  status that operation does not declare, and a JSON body that fails that status's schema.
 *
 *  The schema is reached by a JSON pointer into the document itself, never copied out of it: a
 *  response schema is almost always a `$ref` into `components`, and a copy would carry the
 *  reference without the components it points at.
 */

import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { Ajv2020 } from "ajv/dist/2020";

import { recordedOrigin } from "../stub/origin";
import type { Har } from "../stub/recording";

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

const recordings = fileURLToPath(new URL("../__recordings__/", import.meta.url));

/** Every `<family>/<row>.har` under `e2e/__recordings__/`. */
function committedRecordings(): string[] {
  let families: string[];
  try {
    families = readdirSync(recordings);
  } catch {
    return [];
  }
  const found: string[] = [];
  for (const family of families) {
    const directory = join(recordings, family);
    if (!statSync(directory).isDirectory()) continue;
    for (const file of readdirSync(directory)) {
      if (file.endsWith(".har")) found.push(join(directory, file));
    }
  }
  return found.sort();
}

/** The templated route of the document that `path` is an instance of, or undefined where none
 *  is. A route's `{name}` segment takes one path segment, which is what the Go route table
 *  writes and what the generator copies. The route with the most literal segments wins, so a
 *  path that two routes match is read as the more specific one's. */
function routeOf(path: string): string | undefined {
  const wanted = path.split("/");
  let best: string | undefined;
  let bestLiterals = -1;
  for (const route of Object.keys(document.paths)) {
    const segments = route.split("/");
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

const ajv = new Ajv2020({ allErrors: false, strict: false, validateFormats: false });
ajv.addSchema({ $id: "dispatch", ...document });

const files = committedRecordings();

describe("every committed recording answers what the generated contract declares", () => {
  // An empty recordings directory is the state of this tree before the first family moved, and a
  // suite that passed over it silently would also pass over a directory a rename emptied.
  test("there is at least one recording to check", () => {
    expect(files.length).toBeGreaterThan(0);
  });

  for (const file of files) {
    const name = file.slice(recordings.length);
    test(name, () => {
      const har = JSON.parse(readFileSync(file, "utf8")) as Har;
      for (const entry of har.log.entries) {
        const url = new URL(entry.request.url);
        const where = `${entry.request.method} ${url.pathname} in ${name}`;
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
        const pointer =
          `dispatch#/paths/${step(route)}/${method}/responses/${step(code)}` +
          `/content/${step("application/json")}/schema`;
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
