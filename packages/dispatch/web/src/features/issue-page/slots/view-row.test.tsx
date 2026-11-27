import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../../api/client";
import { issueResultsQuery, referenceTargetsQuery } from "../../../api/queries";
import type { ArtifactBlock, ReferenceTarget } from "../../../api/types";
import { decision, KEY } from "../../decision-card/decision-fixture";
import { DESKTOP_WIDTH, stubViewport } from "../../margin/margin-fixture";
import { buildDispatchReference } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { decisionsRead, issueDetails, SPEC_SLUG } from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import { TWO_THREADS } from "../outline-fixtures";
import { FILTER_SIGNAL_WORDS, PRESET_NAMES, PRESET_ORDER } from "../page-words";
import type { Filter } from "../presets";
import { ProvidersSlot } from "./providers";
import { ViewRowSlot } from "./view-row";

/**
 * The view row's counts. Each is read off the issue's tree, which lacks the nodes of a read
 * still out or failed, so a count read before every read holds its answer is short - often a
 * `0` - and reads as the real one. The row draws no count, unit word or flag until then, and the
 * real counts once it holds them.
 */

/** A `requirement` block of the spec, as the blocks read answers it: a statement, which the Spec
 *  view counts. */
function requirementBlock(id: string): ArtifactBlock {
  return {
    attributes: [],
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type: "requirement",
  };
}

/** The address of the step `blocksWithStep` gives the spec, which the names read names it by. */
const STEP = buildDispatchReference({
  block: { id: "t1" },
  key: KEY,
  kind: "artifact",
  slug: SPEC_SLUG,
});

/** The blocks read's answer for the document `id`: the spec's two statements and a step (`STEP`),
 *  which the Plan view counts and whose words come from the names read alone; none for any other
 *  document. */
function blocksWithStep(id: string): never {
  return Promise.resolve(
    id === "artifact-spec"
      ? [
          requirementBlock("r1"),
          requirementBlock("r2"),
          { ...requirementBlock("t1"), type: "plan_task" },
        ]
      : []
  ) as never;
}

/** The counts once every read has answered: two statements and no decisions, steps or checks.
 *  Conversation draws no count, and neither does Dashboard: what waits on the reader is counted
 *  in the header. */
const ANSWERED_COUNTS: Readonly<Record<Filter, string>> = {
  decisions: "0",
  plan: "0",
  spec: "2",
  verification: "0",
};

/** The row's text with each control's name alone, which is all it draws with no signals. */
const NAMES_ALONE = PRESET_ORDER.map((preset) => PRESET_NAMES[preset]).join("");

/** Answers every read the row reaches: the tree's six reads, the documents' blocks, the reader
 *  and the names. A case changes the answer of the read it holds out or fails. */
function answerReads() {
  return {
    asks: spyOn(api, "listIssueAsks").mockResolvedValue([] as never),
    blocks: spyOn(api, "getArtifactBlocks").mockImplementation(
      (id: string) =>
        Promise.resolve(
          id === "artifact-spec" ? [requirementBlock("r1"), requirementBlock("r2")] : []
        ) as never
    ),
    comments: spyOn(api, "listComments").mockResolvedValue(TWO_THREADS as never),
    coverage: spyOn(api, "getIssueCoverage").mockResolvedValue({ requirements: [] } as never),
    decisions: spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    } as never),
    messages: spyOn(api, "listIssueMessages").mockResolvedValue([] as never),
    names: spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never),
    results: spyOn(api, "getIssueResults").mockResolvedValue({ checks: [] } as never),
    who: spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" } as never),
  };
}

/** The view row of the fixtures' issue at `path`, its own address unless given, under the page's
 *  providers. Each case draws it at 1440 px (`DESKTOP_WIDTH`), from 1280 px up, where the row is
 *  the row of five. */
function renderViewRow(client: QueryClient, path = presetPath(KEY, "conversation")) {
  const issue = issueDetails();
  const route = { key: issue.key, kind: "conversation" } as const;
  return render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <ProvidersSlot issue={issue} route={route}>
            <ViewRowSlot issue={issue} route={route} />
          </ProvidersSlot>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Each count the row draws, by the view it is drawn for (its `data-preset-count`). */
function drawnCounts(container: HTMLElement): Record<string, string> {
  return Object.fromEntries(
    [...container.querySelectorAll<HTMLElement>("[data-preset-count]")].map((count) => [
      count.dataset.presetCount ?? "",
      count.textContent ?? "",
    ])
  );
}

/** A client as the page's tests take one: no read tried again, and none read again by its age. */
function newClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

test("the view row draws no count, unit word or flag while a read of the tree is out, and the real counts once every read has answered", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // The asks' read, held out until the case answers it.
  const asks = Promise.withResolvers<never[]>();
  reads.asks.mockReturnValue(asks.promise as never);
  const view = renderViewRow(client);
  try {
    await waitFor(() =>
      expect(client.isFetching(), "a read other than the asks' is still out").toBe(1)
    );
    expect(client.isFetching({ queryKey: ["asks", KEY] }), "the asks' read is not out").toBe(1);
    expect(drawnCounts(view.container), "the row drew a count with the asks' read out").toEqual({});
    expect(
      view.container.querySelector("[data-preset-flag]"),
      "the row drew a flag with the asks' read out"
    ).toBeNull();
    expect(
      view.getByRole("toolbar").textContent,
      "a control drew more than its name with the asks' read out"
    ).toBe(NAMES_ALONE);

    await act(async () => asks.resolve([]));
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw the real counts once every read had answered"
      ).toEqual(ANSWERED_COUNTS)
    );
    expect(
      view.getByRole("toolbar").textContent,
      "the Plan control drew no unit word once every read had answered"
    ).toContain(FILTER_SIGNAL_WORDS.steps);
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

test("the view row draws no count while a read of the tree stands failed", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  reads.results.mockRejectedValue(new Error("the results' read failed"));
  const view = renderViewRow(client);
  try {
    await waitFor(() =>
      expect(
        client.getQueryState(issueResultsQuery(KEY).queryKey)?.status,
        "the results' read did not fail"
      ).toBe("error")
    );
    await waitFor(() => expect(client.isFetching(), "a read is still out").toBe(0));
    expect(
      drawnCounts(view.container),
      "the row drew a count with the results' read failed"
    ).toEqual({});
    expect(
      view.getByRole("toolbar").textContent,
      "a control drew more than its name with the results' read failed"
    ).toBe(NAMES_ALONE);
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// Decisions' flag counts decisions marked `waiting`, which the tree does not mark until the
// reader's login has answered. The tree's other reads holding their answers is not enough.
test("the view row draws no count while the reader's login is out, and the real counts once it has answered", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // The reader's login, held out until the case answers it.
  const who = Promise.withResolvers<never>();
  reads.who.mockReturnValue(who.promise as never);
  const view = renderViewRow(client);
  try {
    await waitFor(() =>
      expect(client.isFetching(), "a read other than the login's is still out").toBe(1)
    );
    expect(client.isFetching({ queryKey: ["whoami"] }), "the login's read is not out").toBe(1);
    expect(drawnCounts(view.container), "the row drew a count with the reader's login out").toEqual(
      {}
    );
    expect(
      view.getByRole("toolbar").textContent,
      "a control drew more than its name with the reader's login out"
    ).toBe(NAMES_ALONE);

    await act(async () => who.resolve({ kind: "user", login: "alice" } as never));
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw the real counts once the reader's login had answered"
      ).toEqual(ANSWERED_COUNTS)
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// Under a words filter a plan task's or a callout's words come from the names read alone, so
// until that read answered Plan matched none of the issue's steps and drew 0 as its count, which a
// names read that failed left for good. Under one the row draws no count, unit word or flag until
// the names are held, and the real counts once they are.
test("under a words filter the view row draws no count while the names read is out, and the real counts once it has answered", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // The spec holds a step too.
  reads.blocks.mockImplementation(blocksWithStep);
  // The names read, held out until the case answers it with the step's words.
  const names = Promise.withResolvers<{ targets: ReferenceTarget[] }>();
  reads.names.mockReturnValue(names.promise as never);
  const view = renderViewRow(client, `${presetPath(KEY, "conversation")}?q=lanterns`);
  try {
    // The names are read once every other read of the tree has answered.
    await waitFor(() => expect(reads.names, "the names were never read").toHaveBeenCalled());
    expect(client.isFetching(), "a read other than the names' is still out").toBe(1);
    expect(drawnCounts(view.container), "the row drew a count with the names read out").toEqual({});
    expect(
      view.getByRole("toolbar").textContent,
      "a control drew more than its name with the names read out"
    ).toBe(NAMES_ALONE);

    await act(async () =>
      names.resolve({
        targets: [{ gone: false, kind: "block", name: "Hang the lanterns first.", ref: STEP }],
      })
    );
    await waitFor(() =>
      expect(
        drawnCounts(view.container).plan,
        "Plan did not count the step its words match once the names read had answered"
      ).toBe("1")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// The names read is read under the set of addresses the tree holds, so a read that brings a node
// whose words cite a new address reads the names again, of a new set. The names in hand stand
// while that read is out, as the last answer the names read gave, so under a words filter the
// row keeps drawing its counts meanwhile, and counts the node that read brought.
test("under a words filter the view row keeps drawing its counts while the names read of a new set of addresses is out", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  reads.blocks.mockImplementation(blocksWithStep);
  const cited = "dispatch://CORE-7";
  // The names read of the first set answers with the step's words; the one of a set that holds
  // the address a new decision cites is held out for the rest of the case.
  const held = Promise.withResolvers<never>();
  reads.names.mockImplementation((refs: readonly string[]) =>
    refs.includes(cited)
      ? held.promise
      : (Promise.resolve({
          targets: [{ gone: false, kind: "block", name: "Hang the lanterns first.", ref: STEP }],
        }) as never)
  );
  const view = renderViewRow(client, `${presetPath(KEY, "conversation")}?q=lanterns`);
  try {
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw its counts once the names read of the first set had answered"
      ).toEqual({ decisions: "0", plan: "1", spec: "0", verification: "0" })
    );

    // The decisions are read again, and answer a decision too, whose words hold the filter's
    // word and cite an issue no other node names: Decisions counts it.
    reads.decisions.mockResolvedValue(
      decisionsRead([decision("d1", { text: `Hang the lanterns as ${cited} asks.` })])
    );
    await act(async () => client.invalidateQueries({ queryKey: ["decisions", KEY] }));
    await waitFor(() =>
      expect(
        reads.names.mock.calls.some(([refs]) => refs.includes(cited)),
        "the names were never read for the set that holds the new decision's address"
      ).toBe(true)
    );
    expect(client.isFetching(), "a read other than the names' is still out").toBe(1);
    expect(
      drawnCounts(view.container),
      "the row did not draw its counts with the names read of the new set out"
    ).toEqual({ decisions: "1", plan: "1", spec: "0", verification: "0" });
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// The one more names read names only what a block's words cite, and gives no node its words, so
// a step's words are held once the first names read has answered. A step whose words matched
// `?q=` and cited an address nothing else names left the row drawing no count until the one more
// read, of that address, answered, and none for good while it stood failed.
test("under a words filter the view row draws Plan's count while the one more names read stands failed", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  reads.blocks.mockImplementation(blocksWithStep);
  const cited = "dispatch://CORE-7";
  // The first names read names the step, whose words cite an issue nothing else names; the one
  // more read, of that issue alone, fails.
  reads.names.mockImplementation(async (refs: readonly string[]) => {
    if (!refs.includes(STEP)) throw new Error("the one more names read failed");
    return {
      targets: [
        {
          gone: false,
          kind: "block",
          name: `Hang the lanterns first, as ${cited} asks.`,
          ref: STEP,
        },
      ],
    } as never;
  });
  const view = renderViewRow(client, `${presetPath(KEY, "conversation")}?q=lanterns`);
  try {
    await waitFor(() =>
      expect(
        client.getQueryState(referenceTargetsQuery([cited]).queryKey)?.status,
        "the one more names read, of the issue the step's words cite, did not fail"
      ).toBe("error")
    );
    await waitFor(() =>
      expect(
        drawnCounts(view.container).plan,
        "Plan did not count the step its words match with the one more names read failed"
      ).toBe("1")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// The names read is always made: the root's words write the issue's own key, which is an address
// it names. So under a words filter an issue with no block, comment or other node draws no count
// while that read is out, and its counts once it has answered.
test("under a words filter the view row of an issue that holds no node draws no count until the names read of its own key has answered", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // No document holds a block and nobody has commented: the tree is the issue alone.
  reads.blocks.mockResolvedValue([] as never);
  reads.comments.mockResolvedValue([] as never);
  const own = buildDispatchReference({ key: KEY, kind: "issue" });
  // The names read, held out until the case answers it with the issue's own name.
  const names = Promise.withResolvers<{ targets: ReferenceTarget[] }>();
  reads.names.mockReturnValue(names.promise as never);
  const view = renderViewRow(client, `${presetPath(KEY, "conversation")}?q=lanterns`);
  try {
    await waitFor(() => expect(reads.names, "the names were never read").toHaveBeenCalled());
    expect(reads.names.mock.calls, "the names read named more than the issue's own key").toEqual([
      [[own]],
    ]);
    expect(client.isFetching(), "a read other than the names' is still out").toBe(1);
    expect(drawnCounts(view.container), "the row drew a count with the names read out").toEqual({});

    await act(async () =>
      names.resolve({
        targets: [{ gone: false, kind: "issue", name: issueDetails().title, ref: own }],
      })
    );
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw its counts once the names read of the issue's own key had answered"
      ).toEqual({ decisions: "0", plan: "0", spec: "0", verification: "0" })
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// With no filter on a count reads no words, so nothing waits on the names: the row draws the real
// counts while the names read is still out.
test("with no filter the view row draws the real counts while the names read is out", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // The names read, held out for the whole case.
  reads.names.mockReturnValue(Promise.withResolvers<never>().promise);
  const view = renderViewRow(client);
  try {
    await waitFor(() => expect(reads.names, "the names were never read").toHaveBeenCalled());
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw the real counts with no filter on and the names read out"
      ).toEqual(ANSWERED_COUNTS)
    );
    expect(
      client.isFetching({ queryKey: ["reference-targets"] }),
      "the names read is not out"
    ).toBe(1);
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});
