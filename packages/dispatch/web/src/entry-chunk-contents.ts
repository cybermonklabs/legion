import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { entryScripts } from "./zod-in-entry-chunk";

/**
 * What the entry chunk may hold - React, react-dom, the router, the query client and the
 * shell's frame - and which parts of the app evaluate in a main-thread task of their own.
 *
 * The browser downloads a chunk, compiles it and evaluates every static import of it in one
 * main-thread task: a static module graph evaluates in one task whatever it holds, and a task
 * over 500 ms is a freeze a reader feels (dispatch://INFRA-341, R11). So this check reads two
 * things from the build, both of them about which modules share a task.
 *
 * The first is the entry chunk, the task that runs before anything of the page's own. With the
 * app's own source in it that task took 501 to 655 ms on a phone at 4x CPU throttling, so the
 * app is asked for with `import()` instead (`main.tsx`), and so is every part below: each is
 * drawn by a route, by a tab the reader opens or by a key of their own, not by the shell's
 * first frame. One static import from a module the entry reaches would put it back, and
 * nothing in the source says so.
 *
 * The second is `apartFrom`: the route that opens a document used to evaluate its whole page -
 * the Conversation, the Activity list, the role documents and the document surface - in one
 * task of 505 to 579 ms, because its chunk imported all of them statically. A part named there
 * must not be in the static-import closure of the part it is named against, so the browser
 * evaluates the two in tasks of their own.
 *
 * This check reads the build rather than the source, as `check:no-zod` does, and it reads it
 * from the build's own record of what it emitted: `dist/chunk-contents.json`, which names the
 * modules of every chunk and the chunks every chunk statically imports (`chunkContents` in
 * `web/vite.config.ts`). A part is a module path, so the check is about the module the part is
 * rather than about a string of reader-visible copy that happens to be in it, and a module no
 * chunk of the build holds is a fault rather than a rule enforced over nothing. The module
 * script `index.html` loads and every chunk the browser preloads with it - the entry's own
 * static-import closure, the modules of that same first task - must hold none of these parts,
 * and no part may share a task with one it is named apart from.
 *
 * It reads the other half of the split too. Moving a part a document address needs into a chunk
 * asked for only once the entry has evaluated trades one wait for another, so the build writes
 * the chunks of those parts into `index.html` for the page to preload
 * (`documentAddressChunks` in `web/vite.config.ts`): a part a list names in `neededBy` must be
 * in a chunk that list names.
 *
 * The file below is where the build writes the modules and imports of its chunks, and this
 * check's whole reading of what shares a main-thread task with what (`chunkContents` in
 * web/vite.config.ts, the one writer of it).
 */
export const CHUNK_CONTENTS_FILE = "chunk-contents.json";

/**
 * The address shapes `index.html`'s inline script preloads for, as the build writes them into
 * `window.__dispatchAddressChunks` (`documentAddressChunks` in web/vite.config.ts). `app` is
 * the list every address that opens no document reads, and the two document lists begin with
 * everything in it, since the script reads one list and only one.
 */
export type PreloadAddress = "app" | "document" | "issue";

/** One chunk of the build, as the build recorded it. */
export interface ChunkContents {
  /** The modules it holds, each by its path below `web/`, and none of them emptied by
   *  tree-shaking. */
  readonly modules: readonly string[];
  /** The chunks it statically imports, each by its path below `dist`: the chunks the browser
   *  evaluates in one main-thread task with it. */
  readonly imports: readonly string[];
}

/** Every chunk of the build, by path below `dist`. */
export type BuildChunks = Readonly<Record<string, ChunkContents>>;

export interface DeferredPart {
  /** What the part is, in the message a failure prints. */
  readonly name: string;
  /**
   * The modules the part is, each by its path below `web/`, as the build records a chunk's
   * modules. A part is in a chunk when that chunk holds one of them, so a part split over two
   * modules - the surface and the component that mounts it - names both: an entry chunk that
   * reaches either reaches the part.
   */
  readonly modules: readonly string[];
  /** The addresses whose reader needs it before the page draws, and whose preload list must
   *  therefore name its chunk; empty for a part no address draws first. */
  readonly neededBy: readonly PreloadAddress[];
  /** Where the part is loaded from, for the message a failure prints. */
  readonly loadedBy: string;
  /**
   * The parts whose own chunk must not reach this one by static import. A chunk evaluates only
   * once everything it statically imports has, so the two would be one main-thread task; named
   * here, they are two.
   */
  readonly apartFrom?: readonly string[];
}

export const DEFERRED_PARTS: readonly DeferredPart[] = [
  {
    loadedBy: "`App` in web/src/main.tsx",
    modules: ["src/app.tsx"],
    name: "the app and its routes",
    neededBy: ["app", "document", "issue"],
  },
  {
    loadedBy: "`App` in web/src/main.tsx, through `app.tsx`",
    modules: ["src/features/sidebar/Sidebar.tsx"],
    name: "the sidebar",
    neededBy: ["app", "document", "issue"],
  },
  {
    loadedBy: "`App` in web/src/main.tsx, through `app.tsx`",
    modules: ["src/features/writes/write-storage.ts"],
    name: "the page's write store",
    neededBy: ["app", "document", "issue"],
  },
  {
    loadedBy: "`importIssuePage` in web/src/app.tsx",
    modules: ["src/features/issue/IssuePage.tsx"],
    name: "the issue page",
    neededBy: ["issue"],
  },
  {
    loadedBy: "`importDocumentPage` in web/src/app.tsx",
    modules: ["src/features/document/DocumentPage.tsx"],
    name: "the project document page",
    neededBy: ["document"],
  },
  {
    apartFrom: ["the issue page", "the project document page"],
    loadedBy:
      "`ArtifactDocument` in web/src/features/issue/IssuePage.tsx and " +
      "web/src/features/document/DocumentPage.tsx",
    // The card the two pages mount and the editor surface under it: either one in the entry
    // chunk is the document surface in the entry chunk.
    modules: ["src/features/artifacts/ArtifactDocument.tsx", "src/features/doc/ProofDocument.tsx"],
    name: "the document surface",
    neededBy: ["document", "issue"],
  },
  {
    apartFrom: ["the issue page"],
    loadedBy: "`ConversationTab` in web/src/features/issue/IssuePage.tsx",
    modules: ["src/features/conversation/ConversationTab.tsx"],
    name: "the issue page's Conversation",
    // Not preloaded: a document address opens the Spec or an artifact, never the Conversation,
    // and the tab's chunk is asked for when the reader opens the tab.
    neededBy: [],
  },
  {
    apartFrom: ["the issue page"],
    loadedBy: "`ActivityTab` in web/src/features/issue/IssuePage.tsx",
    modules: ["src/features/activity/ActivityTab.tsx"],
    name: "the issue page's Activity",
    neededBy: [],
  },
  {
    loadedBy: "web/src/features/doc/DeferredAskCard.tsx",
    // The card itself, not the `lazy()` wrapper the shell provides, which is in the entry
    // chunk and is how this one is asked for.
    modules: ["src/features/doc/AskBlockCard.tsx"],
    name: "the decision card every Markdown body draws",
    // Needed before no address's first draw: no `ask` shell exists until a body has been parsed
    // and painted. The two document preload lists hold its chunk all the same, since
    // `ArtifactDocument` statically imports it (`DOCUMENT_ADDRESS_MODULES` says what is named).
    neededBy: [],
  },
  {
    loadedBy: "`SearchPalette` in web/src/features/shell/deferred-shell.ts",
    modules: ["src/features/search/SearchPalette.tsx"],
    name: "the search palette",
    neededBy: [],
  },
  {
    loadedBy: "`InboxList` in web/src/features/inbox/InboxList.tsx",
    modules: ["src/features/inbox/Inbox.tsx"],
    name: "the Inbox's list",
    neededBy: [],
  },
  {
    loadedBy: "`MarginColumn` in web/src/features/margin/MarginColumn.tsx",
    modules: ["src/features/margin/Margin.tsx"],
    name: "the margin",
    neededBy: [],
  },
];

/**
 * What the build recorded of its own chunks, read from `dist`.
 *
 * A build that wrote nothing there is no build to check rather than a build that passes: every
 * rule below is about which modules share a task, and this file is where that is written.
 */
export function readChunkContents(dist: string): BuildChunks {
  const path = join(dist, CHUNK_CONTENTS_FILE);
  if (!existsSync(path)) {
    throw new Error(
      `${path} is not there: this build recorded none of its chunks, so none of the rules ` +
        "below reads anything (`chunkContents` in web/vite.config.ts)."
    );
  }
  const parsed: unknown = JSON.parse(readFileSync(path, "utf8"));
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new Error(`${path} is not an object of chunks: ${JSON.stringify(parsed).slice(0, 200)}`);
  }
  const chunks = Object.entries(parsed as Record<string, unknown>);
  if (chunks.length === 0) {
    throw new Error(`${path} holds no chunk: this build emitted none, or recorded none of them.`);
  }
  for (const [file, contents] of chunks) {
    const recorded =
      typeof contents === "object" &&
      contents !== null &&
      "imports" in contents &&
      Array.isArray(contents.imports) &&
      "modules" in contents &&
      Array.isArray(contents.modules);
    if (!recorded) {
      throw new Error(
        `${path} records ${file} with no modules and imports of its own: ${JSON.stringify(contents)}`
      );
    }
  }
  return parsed as BuildChunks;
}

/** Every chunk of the build holding one of `modules`, by path below `dist`. */
export function chunksHolding(chunks: BuildChunks, modules: readonly string[]): string[] {
  return Object.entries(chunks)
    .filter(([, contents]) => modules.some((module) => contents.modules.includes(module)))
    .map(([file]) => file)
    .sort();
}

/**
 * The chunks the browser evaluates in one main-thread task with `file`, `file` included: the
 * chunks it statically imports, theirs, and so on, since a module graph is evaluated only once
 * all of it has arrived.
 *
 * Every name here is one the build wrote, on both sides - the chunk and what it imports - so a
 * name this record does not hold is a record that cannot be read rather than a branch that
 * ends.
 */
export function oneTaskWith(chunks: BuildChunks, file: string): Set<string> {
  const reached = new Set<string>();
  const walk = (name: string): void => {
    if (reached.has(name)) return;
    reached.add(name);
    const contents = chunks[name];
    if (contents === undefined) {
      throw new Error(
        `${file} reaches ${name}, which this build recorded no contents for: the record of ` +
          `this build is incomplete (${CHUNK_CONTENTS_FILE})`
      );
    }
    for (const imported of contents.imports) walk(imported);
  };
  walk(file);
  return reached;
}

/**
 * The chunks `index.html` preloads for each address shape, by path below `dist`: what
 * `documentAddressChunks` wrote into the page for its inline script to fetch beside the entry.
 */
export function addressChunks(html: string): Record<string, string[]> {
  const written = /window\.__dispatchAddressChunks = (\{.*?\});/s.exec(html);
  if (written === null) {
    throw new Error(
      "index.html names no __dispatchAddressChunks: the build wrote no chunks for a document " +
        "address to preload (`documentAddressChunks` in web/vite.config.ts)"
    );
  }
  const parsed: unknown = JSON.parse(written[1]);
  if (typeof parsed !== "object" || parsed === null) {
    throw new Error(`index.html's __dispatchAddressChunks is not an object: ${written[1]}`);
  }
  return Object.fromEntries(
    Object.entries(parsed as Record<string, unknown>).map(([address, files]) => [
      address,
      (files as string[]).map((file) => file.replace(/^\//, "")),
    ])
  );
}

export interface BuildFault {
  /** The part the fault is about, or `undefined` for a fault of the page's own preload list. */
  readonly part: DeferredPart | undefined;
  /** What is wrong, as the failure prints it. */
  readonly fault: string;
}

/** Every rule of this check that the build in `dist` breaks: the page's own preload lists
 *  first, then each part in the order they are listed. */
export function faultsInBuild(dist: string): BuildFault[] {
  const html = readFileSync(join(dist, "index.html"), "utf8");
  const entry = new Set(entryScripts(html));
  if (entry.size === 0) {
    throw new Error(`${join(dist, "index.html")} names no module script: there is no entry chunk.`);
  }
  const chunks = readChunkContents(dist);
  const preloaded = addressChunks(html);
  const faults: BuildFault[] = [];
  for (const [address, files] of Object.entries(preloaded)) {
    for (const file of files) {
      if (!existsSync(join(dist, file))) {
        faults.push({
          fault: `the ${address} address preloads ${file}, which this build does not hold`,
          part: undefined,
        });
      }
    }
  }
  const holdersOf = new Map<string, string[]>(
    DEFERRED_PARTS.map((part) => [part.name, chunksHolding(chunks, part.modules)])
  );
  for (const part of DEFERRED_PARTS) {
    const holders = holdersOf.get(part.name) ?? [];
    if (holders.length === 0) {
      faults.push({
        fault:
          `no chunk of this build holds ${part.modules.join(" or ")}, so this check reads ` +
          `nothing: the module has moved or is no longer built, and the rule for ${part.name} ` +
          "is enforced over nothing",
        part,
      });
      continue;
    }
    const inEntry = holders.filter((file) => entry.has(file));
    if (inEntry.length > 0) {
      faults.push({
        fault:
          `${inEntry.join(", ")} is evaluated in the page's first task and holds ` +
          `${part.modules.join(" or ")}: a module the entry chunk reaches imports ${part.name} ` +
          `statically, rather than leaving it to ${part.loadedBy}`,
        part,
      });
    }
    for (const other of part.apartFrom ?? []) {
      const otherHolders = holdersOf.get(other);
      if (otherHolders === undefined) {
        throw new Error(
          `${part.name} is named apart from "${other}", which is no part of DEFERRED_PARTS`
        );
      }
      for (const otherHolder of otherHolders) {
        const oneTask = oneTaskWith(chunks, otherHolder);
        const shared = holders.filter((file) => oneTask.has(file));
        if (shared.length === 0) {
          continue;
        }
        faults.push({
          fault:
            `${otherHolder} reaches ${shared.join(", ")} by static import, so the browser ` +
            `evaluates ${other} and ${part.name} in one main-thread task: ${other} imports it ` +
            `statically, rather than leaving it to ${part.loadedBy}`,
          part,
        });
      }
    }
    for (const address of part.neededBy) {
      const unpreloaded = holders.filter((file) => preloaded[address]?.includes(file) !== true);
      if (unpreloaded.length === 0) {
        continue;
      }
      faults.push({
        fault:
          `${unpreloaded.join(", ")} holds ${part.name}, which the ${address} address needs, ` +
          "and index.html preloads none of them there: its bytes would be asked for only once " +
          "the entry chunk had evaluated (`DOCUMENT_ADDRESS_MODULES` in web/vite.config.ts)",
        part,
      });
    }
  }
  return faults;
}

function main(): void {
  // Read here rather than as a constant of this module: `web/vite.config.ts` imports the file
  // name above, and Vite bundles its config with esbuild and runs it on Node, where Bun's
  // `import.meta.dir` is not there.
  const dist = join(import.meta.dir, "..", "dist");
  if (!existsSync(join(dist, "index.html"))) {
    throw new Error(
      `No build found at ${dist}. Run "bun run build:web" before "bun run check:entry-chunk".`
    );
  }
  const faults = faultsInBuild(dist);
  for (const fault of faults) {
    console.error(`${fault.part?.name ?? "index.html"}: ${fault.fault}.`);
  }
  if (faults.length > 0) {
    process.exit(1);
  }
  const scripts = entryScripts(readFileSync(join(dist, "index.html"), "utf8"));
  console.log(
    `None of the ${DEFERRED_PARTS.length} deferred parts is in the ${scripts.length} script(s) ` +
      `the page evaluates first (${scripts.join(", ")}), each part named apart from another ` +
      "evaluates in a task of its own, and an address preloads the chunks of the ones it needs."
  );
}

if (import.meta.main) {
  main();
}
