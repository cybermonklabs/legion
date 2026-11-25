import { describe, expect, test } from "bun:test";
import { Authorship, titleAuthor, versionClass } from "../word-rewrite/authorship.ts";
import { type Actor, Artifact, Event, Text, type Version } from "../word-rewrite/dispatch-api.ts";
import { fixture, history } from "./word-rewrite-fake.ts";

const session: Actor = { kind: "session", id: "s1" };
const person: Actor = { kind: "user", id: "alice" };
/** An agent's write through alice's personal agent token, as Dispatch records it. */
const act: Actor = { kind: "session", id: "s1", owner: "alice" };

/** The captured versions of a document of `issue-artifacts.json`. */
function capturedVersions(name: string): Version[] {
  const found = Artifact.array()
    .parse(fixture("issue-artifacts.json"))
    .find((artifact) => artifact.name === name);
  if (found?.versions === undefined || found.versions === null) {
    throw new Error(`issue-artifacts.json lists no versions of ${name}`);
  }
  return found.versions;
}

function block(text: string, sentence = text) {
  return { id: "p1", text, sentence };
}

/** Who wrote a block does not hang on what it says, so these texts need no occurrence. */
const MADE = "The session wrote that the issue waits for the release.";
const OTHER = "Another paragraph.";

describe("who can have written a block", () => {
  test("a block a person made: version 1 by kind user", async () => {
    const { listed, reads } = history([{ authors: [person], markdown: `${MADE}\n` }]);
    expect(await new Authorship("d", listed, reads).authorOf(block(MADE))).toBe("person");
  });

  test("a block a session made and a person changed later", async () => {
    const changed = "The session wrote that the issue waits for the release, and a person agreed.";
    const { listed, reads } = history([
      { authors: [session], markdown: `${MADE}\n\n${OTHER}\n` },
      { authors: [person], markdown: `${changed}\n\n${OTHER}\n` },
    ]);
    expect(await new Authorship("d", listed, reads).authorOf(block(changed))).toBe("person");
  });

  test("a block whose only person-authored version changed another block is the tool's", async () => {
    const { listed, reads } = history([
      { authors: [session], markdown: `${MADE}\n\n${OTHER}\n` },
      { authors: [person], markdown: `${MADE}\n\nA person rewrote this one.\n` },
    ]);
    expect(await new Authorship("d", listed, reads).authorOf(block(MADE))).toBe("session");
  });

  test("an act write: an agent through a person's token is a session whatever its owner, so its text is changed (spec version 44)", async () => {
    // act.md's second version, written through alice's personal agent token.
    const [, written] = capturedVersions("act.md");
    expect(written?.authors?.map((author) => author.owner)).toEqual(["alice"]);
    expect(versionClass(written?.authors ?? null)).toBe("session");
    const { listed, reads } = history([
      { authors: [session], markdown: `${OTHER}\n` },
      { authors: [act], markdown: `${OTHER}\n\n${MADE}\n` },
    ]);
    expect(versionClass([act])).toBe("session");
    expect(versionClass([act, session])).toBe("session");
    expect(await new Authorship("d", listed, reads).authorOf(block(MADE))).toBe("session");
  });

  test("unknown-author: a version with no author, a system author, or a person beside a session made the block", async () => {
    for (const authors of [
      null,
      [{ kind: "system", id: "document-settlement" }],
      [person, session],
    ] satisfies (Actor[] | null)[]) {
      const { listed, reads } = history([{ authors, markdown: `${MADE}\n` }]);
      expect(await new Authorship("d", listed, reads).authorOf(block(MADE))).toBe("unknown");
    }
    expect(versionClass([person, session])).toBe("unknown");
    expect(versionClass([{ kind: "system", id: "document-settlement" }])).toBe("unknown");
    expect(versionClass([])).toBe("unknown");
  });

  test("a document whose versions are all a session's reads no version", async () => {
    const { listed, reads } = history([
      { authors: [session], markdown: `${MADE}\n` },
      { authors: [session], markdown: `${MADE}\n\n${OTHER}\n` },
    ]);
    const authorship = new Authorship("d", listed, reads);
    expect(authorship.suspect).toBe(false);
    expect(await authorship.authorOf(block(MADE))).toBe("session");
    expect(reads.read).toEqual([]);
  });

  test("from the captured versions: the ask block a session made, and the paragraph a person added", async () => {
    // mixed.md: version 1 a session's upload, version 2 alice's paragraph. The ask block's
    // settlement writes no version, and a person's answer writes one of that person, so no seed
    // step gives a version of a system author.
    const text = Text.parse(fixture("text-blocks.json"));
    const bytes = Buffer.from(text.markdown);
    const versions = capturedVersions("mixed.md");
    expect(versions.map((version) => versionClass(version.authors))).toEqual(["session", "person"]);
    const reads = {
      version: async (_id: string, number: number) => fixture(`version-blocks-${number}.json`),
    };
    const authorOf = (type: string, marker: string) => {
      const found = text.blocks?.find(
        (candidate) =>
          candidate.type === type &&
          bytes.subarray(candidate.from, candidate.to).toString("utf8").includes(marker)
      );
      if (found === undefined) throw new Error(`text-blocks.json holds no ${type} with ${marker}`);
      const markdown = bytes.subarray(found.from, found.to).toString("utf8");
      return new Authorship("d", versions, reads).authorOf({
        id: found.id,
        text: markdown,
        sentence: markdown,
      });
    };
    expect(await authorOf("ask", "Should the")).toBe("session");
    expect(await authorOf("paragraph", "Alice added this")).toBe("person");
  });
});

describe("who can have written a title", () => {
  test("the creator, when no event set the title since: issue-events.json", () => {
    const events = Event.array().parse(fixture("issue-events.json"));
    expect(titleAuthor(events, null)).toBe("session");
  });

  test("the actor of the last event that set the title", () => {
    const events = Event.array().parse(fixture("issue-events.json"));
    const retitled = {
      ...events[0],
      id: 9_999,
      seq: 99,
      type: "issue.updated",
      actor: person,
      payload: { title: "A person's title", changed: ["title"] },
    };
    expect(titleAuthor([...events, Event.parse(retitled)], session)).toBe("person");
  });

  test("no event and no creator is unknown", () => {
    expect(titleAuthor([], null)).toBe("unknown");
  });
});
