import { expect, spyOn, test } from "bun:test";
import { act, render, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useEffect, useLayoutEffect, useRef, useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { KeymapProvider } from "../../shell/KeymapProvider";
import { presetPath } from "../issue-address";
import { CONTROL_LABELS } from "../page-words";
import { TAB_STOP_ROW } from "../tree-focus";
import { useIssueKeymap } from "../useIssueKeymap";
import { FAILED_READ_RETRY, FailedReadLine } from "./failed-read-line";
import { ViewStage } from "./stage";

// A failed read's line, as each view drawn in the stage is to draw it. The views here are
// stand-ins that draw the lines over a tree the way a view is to: each view's own slot is to be
// filled by that view's own code.

/** Stands in for Chromium's focus fixup, which moves the focus of a button it disables to the
 *  page's body where happy-dom keeps it on the button; the returned function ends it. happy-dom
 *  refuses `blur()` on a disabled control, so the body is given the focus instead. */
function blurWhenDisabled(): () => void {
  const fixup = new MutationObserver(() => {
    const focused = document.activeElement;
    if (focused instanceof HTMLButtonElement && focused.disabled) document.body.focus();
  });
  fixup.observe(document.body, { attributeFilter: ["disabled"], attributes: true, subtree: true });
  return () => fixup.disconnect();
}

/** Lets `count` frames pass. */
async function frames(count: number): Promise<void> {
  for (let left = count; left > 0; left -= 1) {
    const { promise, resolve } = Promise.withResolvers<void>();
    requestAnimationFrame(() => resolve());
    await act(() => promise);
  }
}

/** A view with one read refused, which keeps that read's line drawn while its Retry runs the
 *  read again. */
function RunningAgainView({ asked }: { asked: () => void }): ReactNode {
  const [running, setRunning] = useState(false);
  return (
    <>
      <FailedReadLine
        message="Couldn't read the statements."
        onRetry={() => {
          asked();
          setRunning(true);
        }}
        retrying={running}
      />
      <div aria-label={CONTROL_LABELS.tree} role="tree" tabIndex={-1} />
    </>
  );
}

test("a failed read's Retry reads busy and stays enabled while its read runs again, and a press then asks nothing more", async () => {
  const user = userEvent.setup();
  const fixup = blurWhenDisabled();
  let asks = 0;
  const view = render(
    <ViewStage>
      <RunningAgainView
        asked={() => {
          asks += 1;
        }}
      />
    </ViewStage>
  );
  try {
    const retry = view.getByRole("button", { name: "Retry" }) as HTMLButtonElement;
    expect(retry.getAttribute("aria-busy"), "the Retry reads busy before it was pressed").toBe(
      null
    );
    await act(async () => retry.focus());
    await user.keyboard("{Enter}");
    expect(asks, "Enter on Retry did not ask the read again").toBe(1);
    // The fixup's observer runs once React has drawn the read running.
    await act(async () => {});
    expect(document.activeElement, "the read running again took the focus off its Retry").toBe(
      retry
    );
    expect(retry.disabled, "the Retry is disabled while its read runs again").toBe(false);
    expect(
      retry.getAttribute("aria-busy"),
      "the Retry does not read busy while its read runs again"
    ).toBe("true");
    await user.keyboard("{Enter}");
    expect(asks, "a second press while the read runs asked it again").toBe(1);
  } finally {
    fixup();
    view.unmount();
  }
});

test("each failed read's line says its own words, and its Retry asks that read alone", async () => {
  const user = userEvent.setup();
  const asked: string[] = [];
  const view = render(
    <ViewStage>
      <FailedReadLine
        message="Couldn't read the statements."
        onRetry={() => asked.push("statements")}
        retrying={false}
      />
      <FailedReadLine
        message="Couldn't read the results."
        onRetry={() => asked.push("results")}
        retrying={false}
      />
      <div aria-label={CONTROL_LABELS.tree} role="tree" tabIndex={-1} />
    </ViewStage>
  );
  try {
    expect(
      view.getAllByRole("alert").map((alert) => alert.textContent),
      "a line did not say its read's words"
    ).toEqual(["Couldn't read the statements.", "Couldn't read the results."]);
    const [, results] = view.getAllByRole("button", { name: "Retry" });
    if (results === undefined) throw new Error("the second line drew no Retry");
    await user.click(results);
    expect(asked, "the results' Retry asked a read other than its own").toEqual(["results"]);
  } finally {
    view.unmount();
  }
});

/** A view with one read refused that keeps that read's line drawn while the read runs again, as
 *  the case has the read stand: `failure` is the read's own count of its failures
 *  (`errorUpdateCount`), where the view names it. */
function KeptLineView({ failure, retrying }: { failure?: number; retrying: boolean }): ReactNode {
  return (
    <ViewStage>
      <FailedReadLine
        {...(failure === undefined ? {} : { failure })}
        message="Couldn't read the statements."
        onRetry={() => undefined}
        retrying={retrying}
      />
      <div aria-label={CONTROL_LABELS.tree} role="tree" tabIndex={-1} />
    </ViewStage>
  );
}

// A view that keeps the line drawn while its read runs again drew the same alert, in the same
// words, for the failure after: a screen reader said nothing of it. Each failure draws the alert
// afresh, and the Retry the reader pressed stays the same button, with its focus.
test("a read that fails again while its line stays drawn says the failure again, and its Retry keeps the focus", async () => {
  const view = render(<KeptLineView retrying={false} />);
  try {
    const retry = view.getByRole("button", { name: "Retry" });
    const first = view.getByRole("alert");
    await act(async () => retry.focus());
    view.rerender(<KeptLineView retrying />);
    expect(view.getByRole("alert"), "the read running again said its failure again").toBe(first);
    view.rerender(<KeptLineView retrying={false} />);
    expect(
      view.getByRole("alert"),
      "the read failed again and the line kept the alert it drew for the first failure"
    ).not.toBe(first);
    expect(view.getByRole("button", { name: "Retry" }), "the second failure drew a new Retry").toBe(
      retry
    );
    expect(document.activeElement, "the second failure took the focus off the Retry").toBe(retry);
  } finally {
    view.unmount();
  }
});

// The same where the view names each failure by its read's count. A run that started and failed
// between two of the view's renders is one the line never saw run, and the read's count is what
// says it failed again.
test("a read that names its failures says each again while its line stays drawn, and its Retry keeps the focus", async () => {
  const view = render(<KeptLineView failure={1} retrying={false} />);
  try {
    const retry = view.getByRole("button", { name: "Retry" });
    const first = view.getByRole("alert");
    await act(async () => retry.focus());
    view.rerender(<KeptLineView failure={1} retrying />);
    expect(view.getByRole("alert"), "the read running again said its failure again").toBe(first);
    view.rerender(<KeptLineView failure={2} retrying={false} />);
    const second = view.getByRole("alert");
    expect(second, "the read's second failure kept the alert of its first").not.toBe(first);
    view.rerender(<KeptLineView failure={3} retrying={false} />);
    expect(
      view.getByRole("alert"),
      "the read's third failure, from a run the line never saw, kept the alert of its second"
    ).not.toBe(second);
    expect(view.getByRole("button", { name: "Retry" }), "a failure drew a new Retry").toBe(retry);
    expect(document.activeElement, "a failure took the focus off the Retry").toBe(retry);
  } finally {
    view.unmount();
  }
});

/** How a read of `TwoReadsView` stands. */
type Stands = "failed" | "out" | "answered";

/** A view drawn from two reads: a failed read's line for each read that failed, in the order of
 *  the reads, over a tree that holds the view's rows once both have answered, its first row
 *  holding the tab stop. */
function TwoReadsView({ results, statements }: { results: Stands; statements: Stands }): ReactNode {
  const answered = statements === "answered" && results === "answered";
  return (
    <>
      {statements === "failed" ? (
        <FailedReadLine
          message="Couldn't read the statements."
          onRetry={() => undefined}
          retrying={false}
        />
      ) : null}
      {results === "failed" ? (
        <FailedReadLine
          message="Couldn't read the results."
          onRetry={() => undefined}
          retrying={false}
        />
      ) : null}
      <div aria-label={CONTROL_LABELS.tree} role="tree" tabIndex={-1}>
        {answered ? (
          <>
            <div aria-level={1} role="treeitem" tabIndex={0}>
              The first statement.
            </div>
            <div aria-level={1} role="treeitem" tabIndex={-1}>
              The second statement.
            </div>
          </>
        ) : null}
      </div>
    </>
  );
}

test("a failed read's line that leaves with the focus in it gives the focus to what replaces it: the next failed read's Retry, or the view's first row", async () => {
  const drawn = (statements: Stands, results: Stands) => (
    <ViewStage>
      <TwoReadsView results={results} statements={statements} />
    </ViewStage>
  );

  (document.activeElement as HTMLElement | null)?.blur();
  const view = render(drawn("failed", "failed"));
  try {
    const [statements, results] = view.getAllByRole("button", { name: "Retry" });
    await waitFor(() => expect(document.activeElement).toBe(statements));
    view.rerender(drawn("answered", "failed"));
    await waitFor(() =>
      expect(
        document.activeElement,
        "the statements answered and their line left the focus elsewhere than the results' Retry"
      ).toBe(results)
    );
    view.rerender(drawn("answered", "answered"));
    await waitFor(() =>
      expect(
        document.activeElement,
        "the results answered and their line left the focus elsewhere than the view's first row"
      ).toBe(view.container.querySelector(TAB_STOP_ROW))
    );
  } finally {
    view.unmount();
  }

  // A read asked again that holds no answer is out again, so its line leaves before the rows
  // come: the tree itself holds the focus meanwhile, and the first row takes it once drawn.
  (document.activeElement as HTMLElement | null)?.blur();
  const again = render(drawn("answered", "failed"));
  try {
    const retry = again.getByRole("button", { name: "Retry" });
    await waitFor(() => expect(document.activeElement).toBe(retry));
    again.rerender(drawn("answered", "out"));
    const tree = again.getByRole("tree", { name: CONTROL_LABELS.tree });
    await waitFor(() =>
      expect(
        document.activeElement,
        "the results' read out again left the focus elsewhere than the tree"
      ).toBe(tree)
    );
    again.rerender(drawn("answered", "answered"));
    await waitFor(() =>
      expect(
        document.activeElement,
        "the rows that came left the focus elsewhere than the view's first row"
      ).toBe(again.container.querySelector(TAB_STOP_ROW))
    );
  } finally {
    again.unmount();
  }
});

/** A view whose row is drawn from a read that answered, under the lines of two other reads that
 *  failed, the first of them while `first`. */
function RowsUnderLinesView({ first }: { first: boolean }): ReactNode {
  return (
    <ViewStage>
      {first ? (
        <FailedReadLine
          message="Couldn't read the statements."
          onRetry={() => undefined}
          retrying={false}
        />
      ) : null}
      <FailedReadLine
        message="Couldn't read the results."
        onRetry={() => undefined}
        retrying={false}
      />
      <div aria-label={CONTROL_LABELS.tree} role="tree" tabIndex={-1}>
        <div aria-level={1} role="treeitem" tabIndex={0}>
          The first statement.
        </div>
      </div>
    </ViewStage>
  );
}

// A view may draw rows from the reads that answered while the lines of others still stand over
// them. The line that leaves gives the focus to the next failed read's Retry all the same, ahead
// of the tree's tab-stop row: that Retry is the one control there that says what is missing.
test("a failed read's line that leaves over drawn rows gives the focus to the next failed read's Retry, not the first row", async () => {
  (document.activeElement as HTMLElement | null)?.blur();
  const view = render(<RowsUnderLinesView first />);
  try {
    const [statements, results] = view.getAllByRole("button", { name: "Retry" });
    await act(async () => statements?.focus());
    view.rerender(<RowsUnderLinesView first={false} />);
    await waitFor(() =>
      expect(
        document.activeElement,
        "the statements' line left the focus elsewhere than the results' Retry"
      ).toBe(results)
    );
  } finally {
    view.unmount();
  }
});

/** A view with one read refused whose answer draws a control and focuses it in the render that
 *  takes the read's line off the page, as a view of its own may move the focus. */
function FocusingView({ answered }: { answered: boolean }): ReactNode {
  const control = useRef<HTMLButtonElement>(null);
  useLayoutEffect(() => {
    if (answered) control.current?.focus();
  }, [answered]);
  return (
    <ViewStage>
      {answered ? (
        <button ref={control} type="button">
          A control the view focuses
        </button>
      ) : (
        <FailedReadLine
          message="Couldn't read the statements."
          onRetry={() => undefined}
          retrying={false}
        />
      )}
      <div aria-label={CONTROL_LABELS.tree} role="tree" tabIndex={-1}>
        {answered ? (
          <div aria-level={1} role="treeitem" tabIndex={0}>
            The first statement.
          </div>
        ) : null}
      </div>
    </ViewStage>
  );
}

// The stage places the focus a line leaves only where nothing took it in the render the line
// left in: a control the view focused there keeps it.
test("a focus the view puts on a control in the render its failed read's line leaves stays on that control", async () => {
  (document.activeElement as HTMLElement | null)?.blur();
  const view = render(<FocusingView answered={false} />);
  try {
    const retry = view.getByRole("button", { name: "Retry" });
    await waitFor(() => expect(document.activeElement).toBe(retry));
    view.rerender(<FocusingView answered />);
    const control = view.getByRole("button", { name: "A control the view focuses" });
    await frames(2);
    expect(
      document.activeElement,
      "the line that left took the focus off the control the view focused in that render"
    ).toBe(control);
  } finally {
    view.unmount();
  }
});

const ISSUE = "CORE-1";

/** A view whose read is refused from its first load, which draws its tree at once and with no
 *  row, as a view does while a read is out. The refused read's line over it is drawn at once, or
 *  where `late` a moment after the view mounts, once the chord has reached the tree. */
function RefusedTreeView({ late }: { late: boolean }): ReactNode {
  const [refused, setRefused] = useState(!late);
  useEffect(() => {
    if (!late) return;
    const failed = window.setTimeout(() => setRefused(true), 40);
    return () => window.clearTimeout(failed);
  }, [late]);
  return (
    <>
      {refused ? (
        <FailedReadLine
          message="Couldn't read the checks."
          onRetry={() => undefined}
          retrying={false}
        />
      ) : null}
      <div aria-label={CONTROL_LABELS.tree} role="tree" tabIndex={-1} />
    </>
  );
}

/** The issue page's chords (`useIssueKeymap`), a control elsewhere on the page, and a stage that
 *  draws the Verification view at that view's address. */
function ChordPage({ late }: { late: boolean }): ReactNode {
  useIssueKeymap(ISSUE);
  const { pathname } = useLocation();
  return (
    <>
      <button type="button">Elsewhere</button>
      {pathname === presetPath(ISSUE, "verification") ? (
        <ViewStage>
          <RefusedTreeView late={late} />
        </ViewStage>
      ) : null}
    </>
  );
}

test("the chord that opens a view with no row reaches its failed read's Retry from anywhere on the page", async () => {
  const user = userEvent.setup();
  for (const from of ["a control elsewhere", "the page's body"] as const) {
    for (const late of [false, true]) {
      const page = render(
        <MemoryRouter initialEntries={[presetPath(ISSUE, "conversation")]}>
          <KeymapProvider>
            <ChordPage late={late} />
          </KeymapProvider>
        </MemoryRouter>
      );
      try {
        if (from === "the page's body") (document.activeElement as HTMLElement | null)?.blur();
        else await act(async () => page.getByRole("button", { name: "Elsewhere" }).focus());
        await user.keyboard("tk");
        const retry = await page.findByRole("button", { name: "Retry" });
        const said = `t k from ${from}, with the read refused ${late ? "after" : "before"} the chord reached the tree, left the focus elsewhere than the Retry`;
        await waitFor(() => expect(document.activeElement, said).toBe(retry));
        // The chord looks for the tree a frame after it opens the view and focuses it there, so
        // the focus is read again once those frames have passed.
        await frames(3);
        expect(document.activeElement, said).toBe(retry);
      } finally {
        page.unmount();
      }
    }
  }
});

// The chord scrolls the tree to the screen's top once it has focused it (`reachPart`), which left
// the Retry the stage gave the chord's focus to, standing over the tree, above the screen: the
// stage brings that Retry on screen in the next frame. happy-dom lays nothing out, so the Retry's
// box is given, 100 px above the screen, and every other box is empty.
test("the chord that opens a view with no row brings its failed read's Retry on screen in the next frame", async () => {
  const user = userEvent.setup();
  const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return this.matches(FAILED_READ_RETRY) ? new DOMRect(0, -100, 80, 24) : new DOMRect();
  });
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  const page = render(
    <MemoryRouter initialEntries={[presetPath(ISSUE, "conversation")]}>
      <KeymapProvider>
        <ChordPage late={false} />
      </KeymapProvider>
    </MemoryRouter>
  );
  try {
    await act(async () => page.getByRole("button", { name: "Elsewhere" }).focus());
    await user.keyboard("tk");
    const retry = await page.findByRole("button", { name: "Retry" });
    await waitFor(() => expect(document.activeElement).toBe(retry));
    await frames(2);
    // Its top is brought 8 px below the band, which nothing publishes here (`bringOnScreen`).
    expect(scrolled, "the Retry the chord reached was left above the screen").toHaveBeenCalledWith({
      top: -108,
    });
  } finally {
    page.unmount();
    boxes.mockRestore();
    scrolled.mockRestore();
  }
});

/** A control elsewhere on the page and, once `drawn`, the Verification view with its read
 *  refused: a case puts the focus where it wants it before the stage mounts. */
function RefusedViewPage({ drawn, late }: { drawn: boolean; late: boolean }): ReactNode {
  return (
    <>
      <button type="button">Elsewhere</button>
      {drawn ? (
        <ViewStage>
          <RefusedTreeView late={late} />
        </ViewStage>
      ) : null}
    </>
  );
}

/** The ways a reader moves the focus off the Retry onto the view's tree: a press on the tree, or
 *  a screen reader's cursor, which focuses what it reaches as a script does. */
const MOVES = ["a press on the tree", "a screen reader's cursor"] as const;

/** Moves the focus onto `tree` the way `move` names. */
async function moveOnto(
  tree: HTMLElement,
  move: (typeof MOVES)[number],
  user: UserEvent
): Promise<void> {
  if (move === "a press on the tree") await user.click(tree);
  else await act(async () => tree.focus());
}

// A focus that moves onto the view's tree from inside the stage is the reader's, and rests there:
// the stage gives a focus on the tree to the Retry only where it comes from outside the view, as
// the chord's does.
test("a focus the reader moves off a failed read's Retry onto the view's tree stays on the tree", async () => {
  const user = userEvent.setup();
  for (const move of MOVES) {
    const page = render(<RefusedViewPage drawn={false} late={false} />);
    try {
      const elsewhere = page.getByRole("button", { name: "Elsewhere" });
      await act(async () => elsewhere.focus());
      // The control elsewhere holds the focus, so the stage takes none, and the reader Tabs from
      // it to the Retry.
      page.rerender(<RefusedViewPage drawn late={false} />);
      const retry = page.getByRole("button", { name: "Retry" });
      const tree = page.getByRole("tree", { name: CONTROL_LABELS.tree });
      await user.tab();
      expect(document.activeElement, "Tab from the control elsewhere left the Retry").toBe(retry);
      await moveOnto(tree, move, user);
      await frames(3);
      expect(
        document.activeElement,
        `${move} moved the focus off the Retry onto the tree, and the stage took it back`
      ).toBe(tree);
      // A focus on the tree from a control outside the view is still taken as the chord's.
      await act(async () => elsewhere.focus());
      await act(async () => tree.focus());
      await waitFor(() =>
        expect(
          document.activeElement,
          "a focus on the tree from the control elsewhere left the focus elsewhere than the Retry"
        ).toBe(retry)
      );
    } finally {
      page.unmount();
    }
  }
});

// The chord's focus on the tree comes in the frame after the stage gave the Retry the focus
// (`reachPart`), and is taken again; a reader's move comes later, and a focus moved off that same
// Retry onto the tree once a frame has passed rests there.
test("a focus the reader moves onto the view's tree, a frame after the stage gave its Retry the focus, stays on the tree", async () => {
  const user = userEvent.setup();
  for (const move of MOVES) {
    (document.activeElement as HTMLElement | null)?.blur();
    const page = render(<RefusedViewPage drawn late={false} />);
    try {
      const retry = page.getByRole("button", { name: "Retry" });
      await waitFor(() => expect(document.activeElement).toBe(retry));
      await frames(1);
      const tree = page.getByRole("tree", { name: CONTROL_LABELS.tree });
      await moveOnto(tree, move, user);
      await frames(3);
      expect(
        document.activeElement,
        `${move} a frame after the stage gave the Retry the focus moved it onto the tree, and the stage took it back`
      ).toBe(tree);
    } finally {
      page.unmount();
    }
  }
});

// The frame after the stage placed the focus places it again where nothing holds it then. A view
// turned off before that frame takes the frame with it, and nothing of a view no longer drawn is
// focused in it.
test("a view turned off before the frame after it placed the focus focuses nothing in that frame", async () => {
  (document.activeElement as HTMLElement | null)?.blur();
  const page = render(<RefusedViewPage drawn late={false} />);
  const retry = page.getByRole("button", { name: "Retry" });
  const focused = spyOn(retry, "focus");
  try {
    expect(document.activeElement, "the view gave its Retry no focus").toBe(retry);
    page.rerender(<RefusedViewPage drawn={false} late={false} />);
    await frames(1);
    expect(
      focused,
      "the frame after the stage placed the focus focused the Retry of a view no longer drawn"
    ).not.toHaveBeenCalled();
  } finally {
    focused.mockRestore();
    page.unmount();
  }
});

// Where the tree draws rows a focus on the tree itself is the reader's wherever it comes from: a
// press on what the tree draws outside its rows focuses the tree, and moves no row's focus.
test("a focus on a view's tree that draws rows stays on the tree, from outside the view too", async () => {
  const user = userEvent.setup();
  const page = render(
    <>
      <button type="button">Elsewhere</button>
      <ViewStage>
        <TwoReadsView results="answered" statements="answered" />
      </ViewStage>
    </>
  );
  try {
    const tree = page.getByRole("tree", { name: CONTROL_LABELS.tree });
    await act(async () => page.getByRole("button", { name: "Elsewhere" }).focus());
    await user.click(tree);
    await frames(3);
    expect(
      document.activeElement,
      "a press on the tree from a control outside the view moved the focus off the tree"
    ).toBe(tree);
  } finally {
    page.unmount();
  }
});

// The app focuses its main element at each page change but the first (`app.tsx`), so a view's
// address opened from another page, where the view draws its Retry in a later commit than the
// page change, starts with the focus on what holds the stage. Nobody put it there, as nobody
// puts it on the page itself, and a refused read's Retry takes it. The case's container stands
// for the app's main element.
test("a view opened from another page gives its failed read's Retry the focus the app put on what holds the view", async () => {
  for (const late of [false, true]) {
    const page = render(<RefusedViewPage drawn={false} late={late} />);
    try {
      const { container } = page;
      container.tabIndex = -1;
      await act(async () => container.focus());
      expect(document.activeElement).toBe(container);
      page.rerender(<RefusedViewPage drawn late={late} />);
      const retry = await page.findByRole("button", { name: "Retry" });
      await waitFor(() =>
        expect(
          document.activeElement,
          `the view, with its read refused ${late ? "after" : "as"} it mounted, left the focus on what holds it`
        ).toBe(retry)
      );
    } finally {
      page.unmount();
    }
  }
});

/** The app's main element, which focuses itself in an effect as it mounts, as the app focuses it
 *  at a page change (`app.tsx`), around a stage mounted in that same commit: React runs the
 *  stage's effect, which places the focus, before this one. */
function PageChangeMain({ children }: { children: ReactNode }): ReactNode {
  const main = useRef<HTMLElement>(null);
  useEffect(() => {
    main.current?.focus();
  }, []);
  return (
    <main ref={main} tabIndex={-1}>
      {children}
    </main>
  );
}

// A view whose reads are cached - Back to its address from another page - draws its Retry in
// the commit the page mounts in. The stage gives the Retry the focus there, and the app then
// moves it to its main element, where it stayed: the stage takes it back in the next frame.
test("a view that draws its failed read's Retry in the commit its page mounts in takes back the focus the app puts on its main element", async () => {
  (document.activeElement as HTMLElement | null)?.blur();
  const page = render(
    <PageChangeMain>
      <ViewStage>
        <RefusedTreeView late={false} />
      </ViewStage>
    </PageChangeMain>
  );
  try {
    expect(
      document.activeElement,
      "the app's main element did not take the focus after the stage placed it"
    ).toBe(page.getByRole("main"));
    await frames(1);
    expect(
      document.activeElement,
      "the view drew its Retry as its page mounted, and left the focus on the app's main element"
    ).toBe(page.getByRole("button", { name: "Retry" }));
  } finally {
    page.unmount();
  }
});

// The same for a view whose reads are cached and answered, which draws its rows in that commit.
test("a view that draws its rows in the commit its page mounts in takes back the focus the app puts on its main element", async () => {
  (document.activeElement as HTMLElement | null)?.blur();
  const page = render(
    <PageChangeMain>
      <ViewStage>
        <TwoReadsView results="answered" statements="answered" />
      </ViewStage>
    </PageChangeMain>
  );
  try {
    expect(
      document.activeElement,
      "the app's main element did not take the focus after the stage placed it"
    ).toBe(page.getByRole("main"));
    await frames(1);
    expect(
      document.activeElement,
      "the view drew its rows as its page mounted, and left the focus on the app's main element"
    ).toBe(page.container.querySelector(TAB_STOP_ROW));
  } finally {
    page.unmount();
  }
});
