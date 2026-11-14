import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  addressChunks,
  type BuildChunks,
  CHUNK_CONTENTS_FILE,
  chunksHolding,
  DEFERRED_PARTS,
  faultsInBuild,
  oneTaskWith,
  type PreloadAddress,
} from "../entry-chunk-contents";

const dirs: string[] = [];

afterEach(() => {
  for (const dir of dirs.splice(0)) {
    rmSync(dir, { recursive: true, force: true });
  }
});

/** A chunk as a row writes it: what it holds and what it statically imports, both optional,
 *  since most rows are about one of the two. */
interface FixtureChunk {
  readonly modules?: readonly string[];
  readonly imports?: readonly string[];
}

/**
 * A built page: `index.html`, the chunks it names as files, and the record of what each chunk
 * holds and imports, written where the build writes it (`chunkContents` in web/vite.config.ts).
 */
function build({
  entry = "assets/entry.js",
  preloads = [],
  addresses,
  chunks,
  recorded = true,
}: {
  entry?: string;
  preloads?: string[];
  addresses: Record<string, string[]>;
  chunks: Record<string, FixtureChunk>;
  /** False for a build that recorded none of its chunks, which is no build to check. */
  recorded?: boolean;
}): string {
  const dir = mkdtempSync(join(tmpdir(), "dispatch-entry-"));
  dirs.push(dir);
  mkdirSync(join(dir, "assets"));
  for (const name of Object.keys(chunks)) {
    writeFileSync(join(dir, name), "");
  }
  if (recorded) {
    writeFileSync(
      join(dir, CHUNK_CONTENTS_FILE),
      JSON.stringify(
        Object.fromEntries(
          Object.entries(chunks).map(([name, chunk]) => [
            name,
            { imports: chunk.imports ?? [], modules: chunk.modules ?? [] },
          ])
        )
      )
    );
  }
  const links = preloads
    .map((file) => `    <link rel="modulepreload" crossorigin href="/${file}">\n`)
    .join("");
  writeFileSync(
    join(dir, "index.html"),
    `<!doctype html>
<html lang="en">
  <head>
${links}    <script>
      window.__dispatchAddressChunks = ${JSON.stringify(addresses)};
    </script>
    <script type="module" crossorigin src="/${entry}"></script>
  </head>
  <body><div id="root"></div></body>
</html>
`
  );
  return dir;
}

/** Every part in its own lazy chunk, which is what the build this check guards looks like. */
function partChunks(): Record<string, FixtureChunk> {
  return Object.fromEntries(
    DEFERRED_PARTS.map((part, index) => [`assets/part-${index}.js`, { modules: part.modules }])
  );
}

/** Where each part's chunk ends up, by name, for an address list to preload. */
function chunkOf(part: string): string {
  return `assets/part-${DEFERRED_PARTS.findIndex((candidate) => candidate.name === part)}.js`;
}

/** The chunks a build has to preload at `address`: one per part that address's reader needs. */
function neededAt(address: PreloadAddress): string[] {
  return DEFERRED_PARTS.filter((part) => part.neededBy.includes(address)).map((part) =>
    chunkOf(part.name)
  );
}

/** Every list of a build that preloads everything it owes, which is what this check passes. */
const preloadsEverything = {
  app: neededAt("app"),
  document: neededAt("document"),
  issue: neededAt("issue"),
};

/** The one module of `part`, for a row that moves it or puts it somewhere else. */
function moduleOf(part: string): string {
  const [module] = DEFERRED_PARTS.find((candidate) => candidate.name === part)?.modules ?? [];
  if (module === undefined) throw new Error(`${part} is no part of DEFERRED_PARTS`);
  return module;
}

test("a build with every part in a lazy chunk, and the document address's chunks preloaded, passes", () => {
  const dir = build({
    addresses: preloadsEverything,
    chunks: { "assets/entry.js": {}, ...partChunks() },
  });
  expect(faultsInBuild(dir)).toEqual([]);
});

test("a part the entry chunk holds is named with the module that found it", () => {
  const dir = build({
    addresses: preloadsEverything,
    chunks: {
      "assets/entry.js": { modules: [moduleOf("the search palette")] },
      ...partChunks(),
    },
  });
  const faults = faultsInBuild(dir);
  expect(faults.map((fault) => fault.part?.name)).toEqual(["the search palette"]);
  expect(faults[0].fault).toContain("assets/entry.js is evaluated in the page's first task");
  expect(faults[0].fault).toContain(moduleOf("the search palette"));
});

test("a part in a chunk the entry statically imports is caught too: one task evaluates both", () => {
  const dir = build({
    entry: "assets/entry.js",
    preloads: ["assets/shared.js"],
    addresses: preloadsEverything,
    chunks: {
      "assets/entry.js": { imports: ["assets/shared.js"] },
      "assets/shared.js": { modules: [moduleOf("the margin")] },
      ...partChunks(),
    },
  });
  expect(faultsInBuild(dir).map((fault) => fault.fault)).toEqual([
    expect.stringContaining("assets/shared.js is evaluated in the page's first task") as string,
  ]);
});

test("a part whose module no chunk of this build holds is a fault, not a passing build", () => {
  const chunks = partChunks();
  chunks[chunkOf("the issue page")] = { modules: ["src/features/issue/IssuePane.tsx"] };
  const dir = build({
    addresses: preloadsEverything,
    chunks: { "assets/entry.js": {}, ...chunks },
  });
  const faults = faultsInBuild(dir);
  expect(faults.map((fault) => fault.part?.name)).toEqual(["the issue page"]);
  expect(faults[0].fault).toContain("so this check reads nothing");
  expect(faults[0].fault).toContain("enforced over nothing");
});

test("a part a document address needs, in a chunk that address does not preload, is a second wait", () => {
  const dropped = chunkOf("the project document page");
  const dir = build({
    // The project document page is what a project document's address draws, and its own chunk
    // is the one its list leaves out.
    addresses: {
      ...preloadsEverything,
      document: neededAt("document").filter((file) => file !== dropped),
    },
    chunks: { "assets/entry.js": {}, ...partChunks() },
  });
  const faults = faultsInBuild(dir);
  expect(faults.map((fault) => fault.part?.name)).toEqual(["the project document page"]);
  expect(faults[0].fault).toContain("which the document address needs");
  expect(faults[0].fault).toContain("index.html preloads none of them there");
});

test("a part the part it is named apart from statically imports is one task, not two", () => {
  const apart = DEFERRED_PARTS.find((part) => (part.apartFrom ?? []).length > 0);
  if (apart === undefined) throw new Error("no part is named apart from another");
  const [other] = apart.apartFrom ?? [];
  const chunks = partChunks();
  // The other part's chunk reaches this one by a static import, as a page whose route imported
  // it statically would: one module graph, one evaluation, one main-thread task.
  chunks[chunkOf(other)] = { ...chunks[chunkOf(other)], imports: [chunkOf(apart.name)] };
  const dir = build({
    addresses: preloadsEverything,
    chunks: { "assets/entry.js": {}, ...chunks },
  });
  const faults = faultsInBuild(dir);
  expect(faults.map((fault) => fault.part?.name)).toEqual([apart.name]);
  expect(faults[0].fault).toContain("in one main-thread task");
});

test("a part the other part only imports dynamically is a task of its own", () => {
  const apart = DEFERRED_PARTS.find((part) => (part.apartFrom ?? []).length > 0);
  if (apart === undefined) throw new Error("no part is named apart from another");
  const [other] = apart.apartFrom ?? [];
  const chunks = partChunks();
  // The build records a chunk's static imports alone - a dynamic one is a chunk of a later
  // task, and `chunk.imports` does not hold it - so this chunk imports nothing here.
  chunks[chunkOf(other)] = { ...chunks[chunkOf(other)], imports: [] };
  const dir = build({
    addresses: preloadsEverything,
    chunks: { "assets/entry.js": {}, ...chunks },
  });
  expect(faultsInBuild(dir)).toEqual([]);
});

test("an address preloading a chunk the build does not hold is a fault of its own", () => {
  const dir = build({
    addresses: { ...preloadsEverything, issue: [...neededAt("issue"), "assets/gone.js"] },
    chunks: { "assets/entry.js": {}, ...partChunks() },
  });
  expect(faultsInBuild(dir).map((fault) => fault.fault)).toEqual([
    "the issue address preloads assets/gone.js, which this build does not hold",
  ]);
});

test("a build that recorded none of its chunks is no build to check", () => {
  const dir = build({
    addresses: preloadsEverything,
    chunks: { "assets/entry.js": {}, ...partChunks() },
    recorded: false,
  });
  expect(() => faultsInBuild(dir)).toThrow("recorded none of its chunks");
});

test("a chunk importing one the record does not hold is a record that cannot be read", () => {
  const apart = DEFERRED_PARTS.find((part) => (part.apartFrom ?? []).length > 0);
  if (apart === undefined) throw new Error("no part is named apart from another");
  const [other] = apart.apartFrom ?? [];
  const chunks = partChunks();
  chunks[chunkOf(other)] = { ...chunks[chunkOf(other)], imports: ["assets/unrecorded.js"] };
  const dir = build({
    addresses: preloadsEverything,
    chunks: { "assets/entry.js": {}, ...chunks },
  });
  expect(() => faultsInBuild(dir)).toThrow("the record of this build is incomplete");
});

test("a page the build wrote no address chunks into is no build to check", () => {
  const dir = mkdtempSync(join(tmpdir(), "dispatch-entry-"));
  dirs.push(dir);
  mkdirSync(join(dir, "assets"));
  writeFileSync(
    join(dir, CHUNK_CONTENTS_FILE),
    JSON.stringify({ "assets/entry.js": { imports: [], modules: [] } })
  );
  writeFileSync(
    join(dir, "index.html"),
    '<!doctype html><html><head><script type="module" src="/assets/entry.js"></script></head></html>'
  );
  expect(() => faultsInBuild(dir)).toThrow("names no __dispatchAddressChunks");
});

test("the chunks holding a module are every chunk that holds it, in path order", () => {
  const chunks: BuildChunks = {
    "assets/entry.js": { imports: [], modules: [] },
    "assets/one.js": { imports: [], modules: ["src/features/margin/Margin.tsx"] },
    "assets/two.js": { imports: [], modules: ["src/features/margin/Margin.tsx"] },
  };
  expect(chunksHolding(chunks, ["src/features/margin/Margin.tsx"])).toEqual([
    "assets/one.js",
    "assets/two.js",
  ]);
});

test("one task is a chunk's whole static-import closure, cycles included", () => {
  const chunks: BuildChunks = {
    "assets/a.js": { imports: ["assets/b.js"], modules: [] },
    "assets/b.js": { imports: ["assets/c.js", "assets/a.js"], modules: [] },
    "assets/c.js": { imports: [], modules: [] },
    "assets/apart.js": { imports: [], modules: [] },
  };
  expect([...oneTaskWith(chunks, "assets/a.js")].sort()).toEqual([
    "assets/a.js",
    "assets/b.js",
    "assets/c.js",
  ]);
});

test("the address lists are read with their leading slash stripped, as the build writes them", () => {
  expect(
    addressChunks('<script>window.__dispatchAddressChunks = {"issue":["/assets/a.js"]};</script>')
  ).toEqual({ issue: ["assets/a.js"] });
});
