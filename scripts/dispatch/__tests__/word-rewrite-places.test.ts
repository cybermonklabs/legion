import { describe, expect, test } from "bun:test";
import { daemonAtLeast } from "../word-rewrite/daemons.ts";
import type { Actor, DispatchReads, Version } from "../word-rewrite/dispatch-api.ts";
import {
  type Context,
  type Examined,
  examine,
  type Item,
  issueItems,
  projectItems,
} from "../word-rewrite/inventory.ts";
import { builtInMap } from "../word-rewrite/name-map.ts";
import { holdsWord } from "../word-rewrite/pattern.ts";
import { PLACE_REASONS, type PlaceReason } from "../word-rewrite/places.ts";
import { FakeDispatch, fixture, history, input } from "./word-rewrite-fake.ts";

const CONTRACT = 220;
const ENTRIES = builtInMap(CONTRACT);

interface Listed {
  readonly id: string;
  readonly name: string;
}

function artifactNamed(file: string, name: string): Listed {
  const listed = fixture(file) as Listed[];
  const found = listed.find((artifact) => artifact.name === name);
  if (found === undefined) throw new Error(`${file} lists no ${name}`);
  return found;
}

/** Reads that answer from the captured fixtures: SEED's issues, the main issue's documents,
 *  `mixed.md`'s and `verification.md`'s texts and `mixed.md`'s versions, the main issue's
 *  events, and nothing for anything else. */
function fixtureReads(texts: Record<string, unknown> = {}): DispatchReads {
  const mixed = artifactNamed("issue-artifacts.json", "mixed.md").id;
  const verification = artifactNamed("issue-artifacts.json", "verification.md").id;
  const empty = async () => [];
  return {
    healthz: async () => fixture("dispatch-healthz.json"),
    whoami: async () => fixture("whoami-shared.json"),
    projects: async () => [{ key: "SEED" }],
    issues: async (_project, includeClosed) =>
      fixture(includeClosed ? "issues-all.json" : "issues-open.json"),
    children: empty,
    issue: async () => fixture("issue-details.json"),
    issueArtifacts: async (key) => {
      if (key === "SEED-3") return fixture("issue-artifacts.json");
      if (key === "SEED-6") return fixture("issue-artifacts-requested.json");
      return [];
    },
    projectDocuments: empty,
    artifact: async () => null,
    text: async (id) => {
      if (id === mixed) return fixture("text-blocks.json");
      if (id === verification) return fixture("text-blocks-verification.json");
      const text = texts[id];
      if (text === undefined) return { markdown: "Nothing here.\n", token: "sha256:0", version: 1 };
      return text;
    },
    version: async (id, number) => {
      if (id !== mixed) throw new Error(`no version fixture for ${id}`);
      return fixture(`version-blocks-${number}.json`);
    },
    issueEvents: async (key) => (key === "SEED-3" ? fixture("issue-events.json") : []),
    daemonSetting: async () => null,
    eventLog: empty,
    stateText: async () => null,
  };
}

function context(reads: DispatchReads, overrides: Partial<Context> = {}): Context {
  return {
    reads,
    entries: ENTRIES,
    reachOf: () => ({
      daemonAtLeast: (contract) =>
        daemonAtLeast({ kind: "contract", daemon: CONTRACT, source: "dispatch" }, contract),
      dispatchHoldsMerge: true,
    }),
    scope: null,
    tree: new Set(),
    ...overrides,
  };
}

/** The main issue's items, from the fixtures. */
async function mainItems(): Promise<Item[]> {
  return (await projectItems(fixtureReads(), "SEED", true)).filter(
    (item) => item.issue?.key === "SEED-3"
  );
}

async function mainDocument(name: string, overrides: Partial<Context> = {}): Promise<Examined> {
  const item = (await mainItems()).find((candidate) => candidate.artifact?.name === name);
  if (item === undefined) throw new Error(`no ${name}`);
  return examine(context(fixtureReads(), overrides), item);
}

function mixedDocument(overrides: Partial<Context> = {}): Promise<Examined> {
  return mainDocument("mixed.md", overrides);
}

/** The reason of the occurrence that stands in the text after `marker`. */
function reasonAfter(examined: Examined, marker: string): string | null {
  const at = examined.text.indexOf(marker);
  if (at === -1) throw new Error(`the text holds no ${JSON.stringify(marker)}`);
  const occurrence = examined.occurrences.find(
    (candidate) => !candidate.inName && candidate.start >= at
  );
  if (occurrence === undefined) throw new Error(`no occurrence after ${JSON.stringify(marker)}`);
  return occurrence.reason;
}

/** One document of a fake Dispatch, examined. */
async function fakeDocument(
  build: (dispatch: FakeDispatch) => void,
  overrides: Partial<Context> = {}
): Promise<Examined[]> {
  const dispatch = new FakeDispatch();
  build(dispatch);
  const reads = dispatch.reads();
  const examined: Examined[] = [];
  for (const project of dispatch.projects) {
    for (const item of await projectItems(reads, project, true)) {
      examined.push(await examine(context(reads, overrides), item));
    }
  }
  return examined.filter((one) => one.occurrences.length > 0);
}

/** A document of the main issue, SEED-3, open, with the versions its listing gives. */
function mainIssueDocument(id: string, name: string, versions: Version[] | null): Item {
  return {
    kind: "issue-document",
    project: "SEED",
    id,
    owner: { kind: "issue", key: "SEED-3" },
    issue: {
      key: "SEED-3",
      project: "SEED",
      title: "t",
      status: "todo",
      closed: false,
      createdBy: null,
    },
    artifact: {
      id,
      issue_key: "SEED-3",
      project: "SEED",
      slug: name.replaceAll(".", "-"),
      name,
      kind: "doc",
      versions,
    },
  };
}

/** One row per reason: an occurrence the tool leaves with that reason. */
const ROWS: Record<PlaceReason, () => Promise<string | null>> = {
  "credential-scope": async () => reasonAfter(await mixedDocument({ scope: "OTHER" }), "Café"),
  "this-tree": async () => reasonAfter(await mixedDocument({ tree: new Set(["SEED-3"]) }), "Café"),
  "closed-issue": async () => {
    const items = await projectItems(fixtureReads(), "SEED", true);
    const title = items.find((item) => item.kind === "title" && item.issue?.key === "SEED-8");
    if (title === undefined) throw new Error("issues-all.json lists no SEED-8");
    const examined = await examine(context(fixtureReads()), title);
    return examined.occurrences[0]?.reason ?? null;
  },
  "approval-request": async () => {
    const spec = artifactNamed("issue-artifacts-requested.json", "spec.md");
    const reads = fixtureReads({
      [spec.id]: { markdown: input("requested"), token: "sha256:1", version: 1 },
    });
    const items = await projectItems(reads, "SEED", false);
    const item = items.find((candidate) => candidate.id === spec.id);
    if (item === undefined) throw new Error("no requested spec");
    return (await examine(context(reads), item)).occurrences[0]?.reason ?? null;
  },
  "name-or-slug": async () => {
    // The main issue's document whose name holds the word.
    const item = (await mainItems()).find((candidate) => holdsWord(candidate.artifact?.name ?? ""));
    if (item === undefined) throw new Error("issue-artifacts.json lists no such name");
    const examined = await examine(context(fixtureReads()), item);
    return examined.occurrences.find((occurrence) => occurrence.inName)?.reason ?? null;
  },
  "ask-block": async () => reasonAfter(await mixedDocument(), "Should the"),
  // verification.md is the issue's Verification document (`role: "verification"`).
  "check-block": async () =>
    reasonAfter(await mainDocument("verification.md"), "The parked tree resumes"),
  "block-attribute": async () => {
    const [examined] = await fakeDocument((dispatch) => {
      const issue = dispatch.issue("SEED", "A requirement");
      dispatch.document({ issue: issue.key }, "spec.md", input("requirement"));
    });
    return examined?.occurrences[0]?.reason ?? null;
  },
  person: async () => reasonAfter(await mixedDocument(), "Alice added this"),
  "unknown-author": async () => {
    const id = "00000000-0000-4000-8000-00000000abcd";
    const markdown = input("no-authors");
    const { listed, reads: versionReads } = history([{ authors: null, markdown }]);
    const reads: DispatchReads = {
      ...fixtureReads({ [id]: { markdown, token: "sha256:2", version: 1 } }),
      version: versionReads.version,
    };
    const item = mainIssueDocument(id, "legacy.md", listed);
    return (await examine(context(reads), item)).occurrences[0]?.reason ?? null;
  },
  "code-name-not-yet": async () => {
    const [examined] = await fakeDocument(
      (dispatch) => {
        dispatch.document({ project: "BELOW" }, "notice.md", input("notice"));
      },
      {
        reachOf: () => ({
          daemonAtLeast: (contract) =>
            daemonAtLeast({ kind: "contract", daemon: CONTRACT - 1, source: "dispatch" }, contract),
          dispatchHoldsMerge: false,
        }),
      }
    );
    return examined?.occurrences[0]?.reason ?? null;
  },
};

describe("the places a program can tell", () => {
  test("every reason places.ts lists has a row here, and no row names another", () => {
    const missing = PLACE_REASONS.filter((reason) => !Object.hasOwn(ROWS, reason));
    expect(missing).toEqual([]);
    expect(Object.keys(ROWS).sort()).toEqual([...PLACE_REASONS].sort());
  });

  for (const reason of PLACE_REASONS) {
    test(`an occurrence the tool leaves with ${reason}`, async () => {
      expect(await ROWS[reason]()).toBe(reason);
    });
  }

  test("unknown-author: a block a session made and a version with no author, a system author, or a person beside a session changed is left, and the block it did not change is judged", async () => {
    // Version 1, a session's, made both paragraphs; version 2 changed only the first. The text
    // has no blocks, so each paragraph is its line.
    const id = "00000000-0000-4000-8000-00000000abce";
    const kept = "The second paragraph says the plan was parked on Tuesday.";
    const made = `The first paragraph says the notes were parked.\n\n${kept}\n`;
    const changed = `The first paragraph says the notes were parked on Monday.\n\n${kept}\n`;
    const fixtures = fixtureReads({ [id]: { markdown: changed, token: "sha256:4", version: 2 } });
    for (const authors of [
      null,
      [{ kind: "system", id: "document-settlement" }],
      [
        { kind: "user", id: "alice" },
        { kind: "session", id: "s1" },
      ],
    ] satisfies (Actor[] | null)[]) {
      const { listed, reads: versionReads } = history([
        { authors: [{ kind: "session", id: "s1" }], markdown: made },
        { authors, markdown: changed },
      ]);
      const reads: DispatchReads = { ...fixtures, version: versionReads.version };
      const examined = await examine(context(reads), mainIssueDocument(id, "notes.md", listed));
      expect(reasonAfter(examined, "The first paragraph")).toBe("unknown-author");
      expect(reasonAfter(examined, "The second paragraph")).toBeNull();
    }
  });
});

describe("the occurrences the tool judges", () => {
  test("an agent's block in an open issue's document", async () => {
    // mixed.md's first paragraph: version 2 (a person) added only the person's own paragraph.
    expect(reasonAfter(await mixedDocument(), "Café")).toBeNull();
  });

  test("a check block of a document that is not the Verification document is judged as its text is", async () => {
    // Dispatch takes a check block in any document and gives it `when`, but records results
    // against the Verification document's checks alone. The opening line stays as it is.
    const mixed = await mixedDocument();
    expect(reasonAfter(mixed, ":::check")).toBe("block-attribute");
    expect(reasonAfter(mixed, "The parked tree resumes")).toBeNull();
  });

  test("an agent's title of an open issue", async () => {
    const title = (await mainItems()).find((item) => item.kind === "title");
    if (title === undefined) throw new Error("no title");
    const examined = await examine(context(fixtureReads()), title);
    expect(examined.occurrences.length).toBeGreaterThan(0);
    expect(examined.occurrences.map((occurrence) => occurrence.reason)).toEqual(
      examined.occurrences.map(() => null)
    );
  });

  test("a project document", async () => {
    const [examined] = await fakeDocument((dispatch) => {
      dispatch.document({ project: "SEED" }, "notes.md", input("project-notes"));
    });
    expect(examined?.item.kind).toBe("project-document");
    expect(examined?.occurrences[0]?.reason).toBeNull();
  });

  test("a document a design gate names, with no approval, is judged: a gate alone leaves nothing", async () => {
    const details = fixture("issue-details.json") as { design_gates: { artifact_id: string }[] };
    const gated = details.design_gates[0]?.artifact_id ?? "";
    const reads = fixtureReads({
      [gated]: { markdown: input("gated"), token: "sha256:3", version: 1 },
    });
    const item = (await issueItems(reads, "SEED-7")).find((candidate) => candidate.id === gated);
    if (item === undefined) throw new Error("issue-details.json names no gated document");
    expect((await examine(context(reads), item)).occurrences[0]?.reason).toBeNull();
  });

  test("an agent's typed block, neither ask nor check, with the word in an attribute of its opening line is left under block-attribute", async () => {
    // A callout's `title` is free text; Dispatch refuses a `requirement` with any attribute
    // (`pmdoc/schema/blocks.json`), so the input is the callout as Dispatch stores it.
    const [examined] = await fakeDocument((dispatch) => {
      const issue = dispatch.issue("SEED", "A callout");
      dispatch.document({ issue: issue.key }, "spec.md", input("callout-attribute"));
    });
    expect(examined?.blocks.find((block) => block.type === "callout")?.attributes).toEqual([
      expect.objectContaining({ name: "kind", value: "note" }),
      expect.objectContaining({ name: "title" }),
    ]);
    expect(examined?.occurrences.map((occurrence) => occurrence.reason)).toEqual([
      "block-attribute",
    ]);
  });

  test("the slug of a document link leaves its occurrence, the prose beside it is judged", async () => {
    const [examined] = await fakeDocument((dispatch) => {
      dispatch.document({ project: "SEED" }, "links.md", input("link"));
    });
    expect(examined?.occurrences.map((occurrence) => occurrence.reason)).toEqual([
      "name-or-slug",
      null,
    ]);
  });
});
