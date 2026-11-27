import { afterEach, beforeEach, describe, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import {
  type IntersectionControl,
  installIntersectionObserver,
} from "../../__tests__/intersection-observer";
import { api } from "../../api/client";
import type { DisplayTree, HandoffSummary, IssueDetails } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import capturedBodies from "./fixtures/handoff-bodies.json";
import { useHandoffFold } from "./HistoryHandoff";
import { IssueHistory } from "./IssueHistory";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

/**
 * A handoff's fields in the issue's history (`HistoryHandoff.tsx`, the plan's D11 and D12): a
 * list is one numbered entry for each item, an object each key with its value, each value inside
 * them by the same rule, an empty one None, and only the fold control folds them.
 */

const KEY = "CORE-1";

/** A handoff of `KEY` as its list gives it, version 2 of its implement phase. */
const LISTED: HandoffSummary = {
  author: { id: "fedcba9876543210", kind: "session", origin: { session_title: "Implementer" } },
  created_at: "2026-11-19T11:30:00Z",
  id: "handoff-1",
  issue_key: KEY,
  phase: "implement",
  version: 2,
};

// The reads a row spies on, each put back after it.
let getHandoff: Mock<typeof api.getHandoff> | undefined;
let summaries: Mock<typeof api.getIssueSummary> | undefined;
beforeEach(() => {
  getHandoff = undefined;
  summaries = undefined;
});
afterEach(() => {
  getHandoff?.mockRestore();
  summaries?.mockRestore();
});

/** Brings the place of every handoff the history watches within one screen of Details' view, as
 *  the browser's observer reports it, and lets the read that sends land. */
async function showHandoffs(near: IntersectionControl): Promise<void> {
  await act(async () => {
    for (const element of near.observed()) near.show(element);
  });
}

/** Draws the history of `KEY` with the one handoff `LISTED`, whose body is `body` as the bodies
 *  read answers it, with `trees` beside it where given; brings its entry within one screen of
 *  the view, and waits until its fields are drawn. */
async function drawBody(
  body: unknown,
  trees?: Record<string, DisplayTree>
): Promise<{
  bodies: Mock<typeof api.getHandoffBodies>;
  fields: HTMLElement;
  near: IntersectionControl;
  unmount: () => void;
}> {
  const near = installIntersectionObserver();
  const bodies = spyOn(api, "getHandoffBodies").mockResolvedValue({
    bodies: [
      {
        body: JSON.stringify(body),
        id: LISTED.id,
        ...(trees === undefined ? {} : { field_trees: trees }),
      },
    ],
    left: [],
  });
  const restore = drawHistoryWithHandoff();
  const unmount = () => {
    restore();
    near.restore();
    bodies.mockRestore();
  };
  try {
    const entry = await handoffEntry();
    await showHandoffs(near);
    const toggle = within(entry).getByRole("button", { name: CONTROL_LABELS.hide_fields });
    const fields = await waitFor(() => {
      const found = document.getElementById(toggle.getAttribute("aria-controls") ?? "");
      if (found?.tagName !== "DL") throw new Error("the handoff's fields are not drawn");
      return found;
    });
    return { bodies, fields, near, unmount };
  } catch (error) {
    unmount();
    throw error;
  }
}

/** The value of the top-level field `key` of the fields drawn. */
function fieldValue(fields: HTMLElement, key: string): HTMLElement {
  const field = [...fields.querySelectorAll<HTMLElement>("[data-handoff-field]")].find(
    (element) => element.getAttribute("data-handoff-field") === key
  );
  const value = field?.querySelector<HTMLElement>(":scope > dd");
  if (value == null) throw new Error(`the fields draw no ${key}`);
  return value;
}

/** The value `value` of a list or an object holds under `key`, drawn inside `within`. */
function memberValue(scope: HTMLElement, key: string): HTMLElement {
  const member = [...scope.querySelectorAll<HTMLElement>("[data-handoff-key]")].find(
    (element) => element.getAttribute("data-handoff-key") === key
  );
  const value = member?.querySelector<HTMLElement>(":scope > dd");
  if (value == null) throw new Error(`no member ${key} is drawn`);
  return value;
}

/** The items of the list drawn first inside `scope`, each as the reader reads it. */
function items(scope: HTMLElement): string[] {
  const list = scope.querySelector("ol");
  if (list === null) throw new Error("no list is drawn");
  return [...list.querySelectorAll(":scope > li")].map((item) => item.textContent ?? "");
}

describe("a handoff's values are drawn as lists and keys", () => {
  // A list is an ordered list with one entry for each item, in the box that bounds an entry's
  // open height, and never the JSON it was written as.
  test("H1: a list is one numbered entry for each item", async () => {
    const drawn = await drawBody({
      filesChanged: ["web/src/a.ts", "web/src/b.ts", "web/src/a.ts"],
    });
    try {
      const value = fieldValue(drawn.fields, "filesChanged");
      expect(items(value)).toEqual(["web/src/a.ts", "web/src/b.ts", "web/src/a.ts"]);
      expect(value.querySelector("ol")?.className).toContain("list-decimal");
      expect(value.querySelector("pre"), "the list is drawn as JSON").toBeNull();
      expect(value.textContent).not.toContain("[");
      // The box of a list at the top: at most 16 rem high, and scrolling inside itself.
      const box = value.querySelector("ol")?.parentElement;
      expect(box?.className).toContain("max-h-64");
      expect(box?.className).toContain("overflow-auto");
    } finally {
      drawn.unmount();
    }
  });

  // An object is each of its keys, in the words the key spells, with its value.
  test("H2: an object is each key with its value", async () => {
    const drawn = await drawBody({
      implementerProof: { verdict: "verified", how: "Each check was run again, and passed." },
    });
    try {
      const value = fieldValue(drawn.fields, "implementerProof");
      expect(value.querySelector("pre"), "the object is drawn as JSON").toBeNull();
      expect(
        [...value.querySelectorAll("[data-handoff-key] > dt")].map((label) => label.textContent)
      ).toEqual(["Verdict", "How"]);
      expect(memberValue(value, "verdict").textContent).toBe("verified");
      expect(memberValue(value, "how").textContent).toBe("Each check was run again, and passed.");
      expect(value.textContent).not.toContain("{");
    } finally {
      drawn.unmount();
    }
  });

  // A value inside a list or an object is drawn by the same rule: an object in a list is its keys,
  // an object in an object its keys, a number its JSON text, and a string plain text, however
  // much markdown it holds.
  test("H3: a list of objects, and an object in an object", async () => {
    const drawn = await drawBody({
      keyFindings: [
        { severity: "minor", file: "store/read.go", description: "Run **bun test** again." },
        { severity: "important", file: "store/write.go", description: "A key is missing." },
      ],
      plan: { document: "the plan", version: 6, outcome: { kind: "unchanged" } },
    });
    try {
      const findings = fieldValue(drawn.fields, "keyFindings");
      const entries = [...(findings.querySelector("ol")?.querySelectorAll(":scope > li") ?? [])];
      expect(entries).toHaveLength(2);
      const [first, second] = entries as HTMLElement[];
      if (first === undefined || second === undefined) throw new Error("no finding is drawn");
      expect(memberValue(first, "severity").textContent).toBe("minor");
      expect(memberValue(first, "file").textContent).toBe("store/read.go");
      expect(memberValue(first, "description").textContent).toBe("Run **bun test** again.");
      expect(
        first.querySelector("strong"),
        "a string inside a list is drawn as markdown"
      ).toBeNull();
      expect(memberValue(second, "severity").textContent).toBe("important");

      const plan = fieldValue(drawn.fields, "plan");
      expect(memberValue(plan, "document").textContent).toBe("the plan");
      expect(memberValue(plan, "version").textContent).toBe("6");
      expect(memberValue(memberValue(plan, "outcome"), "kind").textContent).toBe("unchanged");
    } finally {
      drawn.unmount();
    }
  });

  // An empty list and an empty object read None, as every row of Details reads a value it does
  // not hold, at the top and inside.
  test("H4: an empty list and an empty object read None", async () => {
    const drawn = await drawBody({
      failures: [],
      notes: {},
      review: { findings: [], outcome: {} },
    });
    try {
      expect(fieldValue(drawn.fields, "failures").textContent).toBe(CONTROL_LABELS.none);
      expect(fieldValue(drawn.fields, "notes").textContent).toBe(CONTROL_LABELS.none);
      const review = fieldValue(drawn.fields, "review");
      expect(memberValue(review, "findings").textContent).toBe(CONTROL_LABELS.none);
      expect(memberValue(review, "outcome").textContent).toBe(CONTROL_LABELS.none);
    } finally {
      drawn.unmount();
    }
  });

  // Each shape the bodies of two real issues held (`fixtures/handoff-bodies.json`): every key at
  // the top is a field, every key of an object inside it is drawn, every item of a list inside it
  // is one entry, and every string inside them is drawn as written.
  test("H5: each shape of the captured bodies draws every key", async () => {
    await warmMarkdownRenderer();
    expect(capturedBodies.length, "the fixture holds the captured shapes").toBeGreaterThan(5);
    // The bodies cite issues by their addresses, which the renderer names by each one's title.
    summaries = spyOn(api, "getIssueSummary").mockImplementation(async (key) => ({
      documents: [],
      key,
      project: "ACME",
      status: "in_progress",
      title: "An issue",
    }));
    for (const captured of capturedBodies) {
      const body: Record<string, unknown> = JSON.parse(captured.body);
      const drawn = await drawBody(body);
      try {
        expect(
          [...drawn.fields.querySelectorAll(":scope > [data-handoff-field]")].map((field) =>
            field.getAttribute("data-handoff-field")
          ),
          `the top-level keys of handoff ${captured.id}`
        ).toEqual(Object.keys(body));
        for (const [key, value] of Object.entries(body)) {
          if (value === null || typeof value !== "object") continue;
          const shown = fieldValue(drawn.fields, key);
          const inner = { keys: [] as string[], items: 0, strings: [] as string[] };
          const walk = (part: unknown): void => {
            if (Array.isArray(part)) {
              inner.items += part.length;
              for (const item of part) walk(item);
            } else if (part !== null && typeof part === "object") {
              for (const [member, held] of Object.entries(part)) {
                inner.keys.push(member);
                walk(held);
              }
            } else if (typeof part === "string") {
              inner.strings.push(part);
            }
          };
          walk(value);
          expect(
            [...shown.querySelectorAll("[data-handoff-key]")].map((member) =>
              member.getAttribute("data-handoff-key")
            ),
            `the keys inside ${key} of handoff ${captured.id}`
          ).toEqual(inner.keys);
          expect(
            shown.querySelectorAll("[data-handoff-list] > li"),
            `the items inside ${key} of handoff ${captured.id}`
          ).toHaveLength(inner.items);
          for (const words of inner.strings) {
            expect(shown.textContent, `a string inside ${key} of handoff ${captured.id}`).toContain(
              words
            );
          }
        }
      } finally {
        drawn.unmount();
      }
    }
  });

  // A string field is drawn from the reading the server sent beside the body in the bodies
  // answer (`field_trees`), not read again on the page: where the two differ, the tree's words
  // are drawn. An entry that carries no trees - the first of an answer, whose trees alone would
  // pass the answer's limit - is read by the renderer itself, from the words as stored.
  test("H8: a string field is drawn from the answer's tree", async () => {
    await warmMarkdownRenderer();
    const tree = displayTree("The **server's** reading of the summary.") as DisplayTree;
    const drawn = await drawBody({ summary: "The words as stored." }, { "/summary": tree });
    try {
      const summary = fieldValue(drawn.fields, "summary");
      await waitFor(() => expect(summary.textContent).toBe("The server's reading of the summary."));
      expect(summary.querySelector("strong")?.textContent).toBe("server's");
    } finally {
      drawn.unmount();
    }

    const untreed = await drawBody({ summary: "The **stored** words, read on the page." });
    try {
      const summary = fieldValue(untreed.fields, "summary");
      await waitFor(() => expect(summary.textContent).toBe("The stored words, read on the page."));
      expect(summary.querySelector("strong")?.textContent).toBe("stored");
    } finally {
      untreed.unmount();
    }
  });

  // The fold control folds the fields and opens them again, from the body read once, though its
  // entry is in view again as it opens; a press anywhere else on the handoff - its fields, a
  // value, its box - folds nothing.
  test("H9: the fold control folds and opens, and is the only thing that folds", async () => {
    const drawn = await drawBody({
      summary: "First pass",
      filesChanged: ["web/src/a.ts"],
      implementerProof: { verdict: "verified" },
    });
    try {
      const toggle = screen.getByRole("button", { name: CONTROL_LABELS.hide_fields });
      for (const pressed of [
        drawn.fields,
        fieldValue(drawn.fields, "filesChanged"),
        fieldValue(drawn.fields, "implementerProof"),
        fieldValue(drawn.fields, "summary"),
      ]) {
        await userEvent.click(pressed);
        expect(toggle, "a press off the fold control folded the handoff").toHaveAttribute(
          "aria-expanded",
          "true"
        );
      }

      await userEvent.click(toggle);
      expect(toggle).toHaveAttribute("aria-expanded", "false");
      expect(toggle).toHaveTextContent(CONTROL_LABELS.show_fields);
      expect(document.querySelector("[data-handoff-field]")).toBeNull();

      act(() => toggle.focus());
      await userEvent.keyboard("{Enter}");
      expect(toggle).toHaveAttribute("aria-expanded", "true");
      expect(toggle).toHaveTextContent(CONTROL_LABELS.hide_fields);
      await screen.findByText("web/src/a.ts");
      await showHandoffs(drawn.near);
      expect(drawn.bodies, "opening the handoff again read its body again").toHaveBeenCalledTimes(
        1
      );
    } finally {
      drawn.unmount();
    }
  });
});

// ---------------------------------------------------------------------------------------------
// The loader's states (t7): an entry whose body is not read yet, and a read of bodies that
// failed. The body comes from the bodies read of the history (`GET .../handoffs/bodies`), never
// from a read of one handoff.
// ---------------------------------------------------------------------------------------------

/** The history of `KEY` with the one handoff `LISTED` and nothing else, drawn with its fold. */
function HistoryWithHandoff(): ReactNode {
  return (
    <IssueHistory
      activityFollowing={undefined}
      fold={useHandoffFold(KEY)}
      issue={{ artifacts: [], key: KEY } as unknown as IssueDetails}
      item={undefined}
      state={{ [KEY]: { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 } }}
      visitMark={0}
    />
  );
}

/** Draws `HistoryWithHandoff`, every read but the bodies' answered, and returns what restores
 *  the reads and unmounts it. */
function drawHistoryWithHandoff(): () => void {
  const reads = [
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listSteps").mockResolvedValue({ issue: KEY, tasks: [] }),
    spyOn(api, "listHandoffs").mockResolvedValue([LISTED]),
  ];
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["agents"], []);
  const { unmount } = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <HistoryWithHandoff />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return () => {
    unmount();
    queryClient.clear();
    for (const read of reads) read.mockRestore();
  };
}

/** The entry of the handoff `LISTED`, once the history draws it. */
async function handoffEntry(): Promise<HTMLElement> {
  return waitFor(() => {
    const found = document.querySelector<HTMLElement>(
      `[data-history-entry="handoff:${LISTED.id}"]`
    );
    if (found === null) throw new Error("the history draws no entry of the handoff");
    return found;
  });
}

// While the bodies read of an entry is out, the entry draws its handoff's phase, version, author
// and time, which the list gives, and the word for a body being read; it sends one bodies read
// for it and no read of the handoff alone.
test("H6: an entry whose body is not read draws the phase, the version, the author, the time and the loading word", async () => {
  const near = installIntersectionObserver();
  const bodies = spyOn(api, "getHandoffBodies").mockImplementation(() => new Promise(() => {}));
  getHandoff = spyOn(api, "getHandoff").mockImplementation(() => new Promise(() => {}));
  const restore = drawHistoryWithHandoff();
  try {
    const entry = await handoffEntry();
    await showHandoffs(near);
    expect(entry.textContent).toContain("recorded the implement handoff, version 2");
    expect(within(entry).getByText("Implementer")).toBeDefined();
    expect(entry.querySelector(`time[datetime="${LISTED.created_at}"]`)).not.toBeNull();
    await waitFor(() =>
      expect(within(entry).getByText(ANSWER_LINES.loading_handoff)).toBeDefined()
    );
    expect(
      getHandoff,
      "the entry read its body by a read of the handoff alone"
    ).not.toHaveBeenCalled();
    expect(bodies).toHaveBeenCalledWith(KEY, [LISTED.id]);
  } finally {
    restore();
    near.restore();
    bodies.mockRestore();
  }
});

// A bodies read that failed says so in the entry it was for, with a Retry that sends the read
// again; the handoff's phase, version, author and time stay.
test("H7: a failed read draws the failure line with Retry, and the phase, version, author and time stay", async () => {
  const near = installIntersectionObserver();
  const bodies = spyOn(api, "getHandoffBodies").mockRejectedValue(new Error("the bodies are down"));
  getHandoff = spyOn(api, "getHandoff").mockRejectedValue(new Error("the handoff is down"));
  const restore = drawHistoryWithHandoff();
  try {
    const entry = await handoffEntry();
    await showHandoffs(near);
    await waitFor(() =>
      expect(within(entry).getByRole("alert").textContent).toBe(ANSWER_LINES.handoff_unreadable)
    );
    expect(entry.textContent).toContain("recorded the implement handoff, version 2");
    expect(within(entry).getByText("Implementer")).toBeDefined();
    expect(entry.querySelector(`time[datetime="${LISTED.created_at}"]`)).not.toBeNull();
    expect(
      getHandoff,
      "the entry read its body by a read of the handoff alone"
    ).not.toHaveBeenCalled();
    const sent = bodies.mock.calls.length;
    await userEvent.click(within(entry).getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(bodies.mock.calls.length).toBe(sent + 1));
    expect(bodies.mock.calls.at(-1)).toEqual([KEY, [LISTED.id]]);
  } finally {
    restore();
    near.restore();
    bodies.mockRestore();
  }
});
