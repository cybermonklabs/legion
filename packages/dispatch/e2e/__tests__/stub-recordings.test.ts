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
 *  200 that answers a stored text without the display tree the contract declares beside it, or
 *  with a tree that holds nothing to draw. An event carries its trees in a map of its own, keyed
 *  by the path of the text each was read from rather than in a property beside that text, so a
 *  200 that answers a list of events is read a second way: every text an event's payload holds
 *  is demanded under the path that map keys it by.
 *
 *  Those last ones are what keep the stubbed rows reading Go's tree. The dashboard asks for the
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
  components?: { schemas?: Record<string, unknown> };
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

/** One stored text the document declares a display tree beside: the schema that declares the
 *  pair, and the text's own field. The two together name the tree a body has to answer, and a
 *  field name alone does not: half a dozen schemas declare a `text`, and a tree taken off one
 *  of them leaves the name standing in the others. */
interface TreePair {
  schema: string;
  field: string;
}

/** The document again, with one demand added at every schema that declares such a pair - a
 *  property whose name is another property's with `_tree` on the end: a body that answers the
 *  text must answer its tree beside it, and that tree must hold content. A text the server
 *  stored as nothing has no tree to read and the server sends none, so the demand is made of a
 *  non-empty string alone, and a field that is absent or null demands nothing. The content is
 *  demanded because the contract's tree schema takes `{"type":"doc"}`, the empty document a row
 *  draws as nothing: a tree that holds no node answers a text that holds one with nothing, which
 *  is the row reading its own parse under another name.
 *
 *  Every pair demanded is appended to `demanded`, named by the node that declared it, which for
 *  every pair this contract holds today is a schema of `components.schemas`. The canary below
 *  asserts that list, so what the rows validate against and what a reader is told they cover are
 *  the one derivation and cannot drift apart.
 *
 *  The demand is an `allOf` branch and never a rewritten `required`, so everything else the
 *  schema says is left as the generator wrote it and every `$ref` still resolves: ajv reads this
 *  document as it reads the first, under the pointer the first is read by. */
function demandingTrees(node: unknown, name: string, demanded: TreePair[]): unknown {
  if (node === null || typeof node !== "object") return node;
  if (Array.isArray(node)) return node.map((held) => demandingTrees(held, name, demanded));
  const copied: Record<string, unknown> = {};
  for (const [key, held] of Object.entries(node)) copied[key] = demandingTrees(held, key, demanded);
  const properties = copied.properties;
  if (properties === null || typeof properties !== "object" || Array.isArray(properties)) {
    return copied;
  }
  const demands: unknown[] = [];
  for (const field of Object.keys(properties)) {
    if (!Object.hasOwn(properties, `${field}_tree`)) continue;
    demanded.push({ schema: name, field });
    demands.push({
      if: { properties: { [field]: { type: "string", minLength: 1 } }, required: [field] },
      // `then` here is JSON Schema's own keyword beside `if`, and this object is handed to ajv
      // rather than awaited: the rule guards against an object a `await` would mistake for a
      // promise, which nothing here does.
      // biome-ignore lint/suspicious/noThenProperty: the JSON Schema keyword, never a thenable
      then: {
        properties: {
          [`${field}_tree`]: {
            properties: { content: { type: "array", minItems: 1 } },
            required: ["content"],
          },
        },
        required: [`${field}_tree`],
      },
    });
  }
  if (demands.length === 0) return copied;
  const held = Array.isArray(copied.allOf) ? copied.allOf : [];
  return { ...copied, allOf: [...held, ...demands] };
}

/** Every pair the walk above demanded a tree of, filled as it builds the document below. */
const demandedPairs: TreePair[] = [];

// `demandingTrees` walks every node of the document and so answers `unknown`; the document it
// was given is an object, so its clone is one. The root itself stands under no name and declares
// no property, so the empty name it is walked with is recorded against nothing.
const demandedDocument = demandingTrees(document, "", demandedPairs) as Record<string, unknown>;
const withTrees = new Ajv2020({ allErrors: false, strict: false, validateFormats: false });
withTrees.addSchema({ $id: "trees", ...demandedDocument });

const ajv = new Ajv2020({ allErrors: false, strict: false, validateFormats: false });
ajv.addSchema({ $id: "dispatch", ...document });

/** Every member of a payload that can hold a stored text: the field of every pair the document
 *  declares. An event's trees stand in a map of their own, so the demand above reaches none of
 *  them and the rule below reads an event's payload with these names instead.
 *
 *  The server reads a payload only for the fields the event's own type can hold - a body for the
 *  comment, message and suggestion families, an ask's own texts for the ask family, and nothing
 *  at all for any other type (`packages/envoy/internal/dispatch/api/event_trees.go`'s
 *  `eventPayloadTexts`). A recording is read for every field instead, because a payload that
 *  gains a text the server answers no tree for is the drift this is here to refuse. Run over the
 *  132 recordings committed today, that wider reading demands a tree in six places - the body of
 *  a `comment.created`, a `message.created` and a `message.answered`, and an `ask.opened`'s
 *  question and its two option labels - and every one of them is answered, so it is kept as it
 *  stands rather than narrowed to the families that file names. */
const storedTextFields = new Set(demandedPairs.map(({ field }) => field));

/** Whether this schema is an event's: one that declares a `payload` beside the `trees` map
 *  holding the display tree of every text that payload carries (model.Event's `Trees`). */
function carriesEventTrees(schema: unknown): boolean {
  if (schema === null || typeof schema !== "object" || !("properties" in schema)) return false;
  const properties = schema.properties;
  if (properties === null || typeof properties !== "object") return false;
  return Object.hasOwn(properties, "payload") && Object.hasOwn(properties, "trees");
}

/** The `$ref` of every such schema, so a response that answers a list of events is found by what
 *  the document declares and not by the routes that answer one today. */
const eventRefs = new Set(
  Object.entries(document.components?.schemas ?? {})
    .filter(([, schema]) => carriesEventTrees(schema))
    .map(([name]) => `#/components/schemas/${name}`)
);

/** Whether this response schema answers a list of events. The three routes that answer one - an
 *  issue's feed, an artifact's and the project log - each declare it as an array of `$ref`s into
 *  `components`, which is how the generator writes a slice of a named type. */
function isEventList(schema: unknown): boolean {
  if (schema === null || typeof schema !== "object" || !("items" in schema)) return false;
  const items = schema.items;
  if (items === null || typeof items !== "object" || !("$ref" in items)) return false;
  return typeof items.$ref === "string" && eventRefs.has(items.$ref);
}

/** One event of such a list, as far as the rule below reads it. */
interface RecordedEvent {
  id: number;
  type: string;
  payload: unknown;
  trees?: Record<string, unknown>;
}

/** Every stored text an event's payload holds, by the path its tree is keyed under: the field's
 *  own path rooted at the envelope, which is what the server keys the map by - `payload.body` for
 *  a comment's, `payload.options.0.label` for an ask option's. */
function eventTextPaths(node: unknown, at: string, found: string[]): string[] {
  if (node === null || typeof node !== "object") return found;
  if (Array.isArray(node)) {
    for (const [index, held] of node.entries()) eventTextPaths(held, `${at}.${index}`, found);
    return found;
  }
  for (const [key, held] of Object.entries(node)) {
    if (storedTextFields.has(key) && typeof held === "string" && held !== "") {
      found.push(`${at}.${key}`);
    }
    eventTextPaths(held, `${at}.${key}`, found);
  }
  return found;
}

/** Whether a display tree holds anything a row can draw: a `content` with a node in it. The
 *  empty document is what a text of no length reads as, and a row handed one for a text that has
 *  length draws nothing where its spec says it draws what the server read. */
function holdsContent(tree: unknown): boolean {
  if (tree === null || typeof tree !== "object" || !("content" in tree)) return false;
  return Array.isArray(tree.content) && tree.content.length > 0;
}

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
  // found none would make every row below pass over every recording. These are the pairs it
  // demanded, each named by the schema that declares the text and the text's own field, read
  // out of the walk itself rather than derived a second way so that what the rows validate
  // against is what this list says. A commit that adds a tree adds its pair here, and one that
  // takes a tree away has to say so here rather than leave the rows guarding nothing.
  test("the contract declares which stored texts carry a tree", () => {
    expect(demandedPairs.map(({ schema, field }) => `${schema}.${field}`).sort()).toEqual([
      "AcceptSuggestionResponse.body",
      "ArchitectureComponentRead.prose",
      "ArtifactApprovalWithTrees.reason",
      "ArtifactReviewRead.reason",
      "ArtifactText.markdown",
      "ArtifactVersionText.markdown",
      "AskAnswerWithTrees.text",
      "AskEditPreviousWithTrees.question",
      "AskOptionWithTrees.description",
      "AskOptionWithTrees.label",
      "AskResolutionWithTrees.reason",
      "AskWithTrees.question",
      "BlockPinRead.text",
      "BroadcastCreated.body",
      "BroadcastRead.body",
      "BroadcastSummary.body",
      "CommentWithTrees.body",
      "CommentWriteResponse.body",
      "Decision.text",
      "GraphNodeRead.text",
      "InboxRow.question",
      "IssueAskRow.question",
      "IssueOpenAsk.question",
      "MessageReplyResult.body",
      "MessageWithTrees.body",
      "MyAnswerReply.body",
      "MyAnswerRow.question",
      "ReplyBlockWithTrees.text",
      "SearchResult.snippet",
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
        const schema = declared[code]?.content?.["application/json"]?.schema;
        const body = entry.response.content.text;
        if (
          schema === undefined ||
          body === undefined ||
          body.trim() === "" ||
          !(entry.response.content.mimeType ?? "").includes("json")
        ) {
          continue;
        }
        const validate = withTrees.getSchema(schemaPointer("trees", route, method, code));
        if (validate === undefined) continue;
        const where = `${entry.request.method} ${new URL(entry.request.url).pathname} in ${file}`;
        const answered: unknown = JSON.parse(body);
        expect(
          validate(answered),
          `${where}: a stored text is answered without a display tree that draws it, so the row ` +
            `that reads it reads the browser's own parse - ${withTrees.errorsText(validate.errors)}`
        ).toBe(true);
        if (!isEventList(schema)) continue;
        // The list answered the schema above, so each of its entries holds the members an event
        // declares; what it answers beside them is whatever the recorded server sent.
        for (const event of answered as RecordedEvent[]) {
          for (const at of eventTextPaths(event.payload, "payload", [])) {
            expect(
              holdsContent(event.trees?.[at]),
              `${where}: event ${event.id} (${event.type}) answers ${at} without a display tree ` +
                `to draw, so the row that reads it reads the browser's own parse`
            ).toBe(true);
          }
        }
      }
    });
  }
});
