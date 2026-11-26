import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { onFakeClock } from "../../__tests__/fake-clock";
import { useDetailsFocusKeeper } from "../margin/details-focus-keeper";
import { Margin } from "../margin/Margin";
import { MarginProvider } from "../margin/margin-context";
import { DESKTOP_WIDTH, issue, stubViewport } from "../margin/margin-fixture";
import { standsFailed } from "../refs/reference-target";
import { buildIssuePath } from "../refs/routes";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "./details-error";
import { useDrawerPage } from "./drawer-page";
import {
  DETAILS_ATTRIBUTE,
  type MarginDrawer,
  type MarginDrawerPage,
  type MarginFill,
  MarginSlotProvider,
  useFillMarginSlot,
  useMarginDrawer,
} from "./margin-slot";
import { CONTROL_LABELS } from "./page-words";

/** Where the focus is when no element holds it. */
const PAGE = "the page";

/** happy-dom lays nothing out, so each drawn element says where it stands: `data-top`, and
 *  `data-height` (24 where it has none), in pixels, 300 wide. An element with no `data-top`, or
 *  inside a `hidden` one, is not drawn. */
function layoutOf(element: Element): DOMRect | null {
  const top = element.getAttribute("data-top");
  if (top === null || !element.isConnected || element.closest("[hidden]") !== null) return null;
  return new DOMRect(0, Number(top), 300, Number(element.getAttribute("data-height") ?? 24));
}

/** What every case runs under: the layout above, and Chromium's drop of a disabled control's
 *  focus to the page (`emulateChromiumFocusFixup`). Returns what removes both. */
function chromiumLayout(): () => void {
  // Stubbed on Element, the prototype that owns both methods.
  const bounds = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    return layoutOf(this) ?? new DOMRect();
  });
  const rects = spyOn(Element.prototype, "getClientRects").mockImplementation(function (
    this: Element
  ) {
    const rect = layoutOf(this);
    return (rect === null ? [] : [rect]) as unknown as DOMRectList;
  });
  const restoreFocus = emulateChromiumFocusFixup();
  return () => {
    restoreFocus();
    rects.mockRestore();
    bounds.mockRestore();
  };
}

type Draw<S> = (state: S, set: (state: S) => void) => ReactNode;

/** Hands a case the fixture's state setter. */
interface Handle<S> {
  set: (state: S) => void;
}

/** Details as the keeper sees it: a box 560 px tall at the window's top, with its heading and its
 *  Close, that draws the rest from its state; beside it, a control of the page. */
function Fixture<S>({
  active,
  draw,
  handle,
  hidden,
  initial,
}: {
  active: boolean;
  draw: Draw<S>;
  handle: Handle<S>;
  hidden: boolean;
  initial: S;
}): ReactNode {
  const box = useRef<HTMLElement | null>(null);
  const [state, setState] = useState(initial);
  handle.set = setState;
  useDetailsFocusKeeper(box, active);
  return (
    <>
      <button data-top={0} type="button">
        Page control
      </button>
      <aside aria-label="Details" data-height={560} data-top={0} hidden={hidden} ref={box}>
        <h2 data-top={0}>Details</h2>
        <button data-top={24} type="button">
          Close
        </button>
        {draw(state, setState)}
      </aside>
    </>
  );
}

/** What a case does with the box it drew. */
interface Drawn<S> {
  /** Sets the box's state, then waits until every observer and microtask it set off has run. */
  change: (state: S) => Promise<void>;
  /** Draws one animation frame: runs every callback asked for before it. The box's frames are
   *  held until a case draws them. happy-dom draws a frame as an immediate, between any two steps
   *  of a case, while a browser draws the commit a press makes - which a case makes as the change
   *  after its press - before the next frame. */
  frame: () => Promise<void>;
  /** Sets the box's state as a write that lands does (`outsideAct`), then waits until React has
   *  run the commit's passive effects and the keeper has had its task. In a browser React's
   *  scheduler queues the task that runs those effects as the commit ends, before the keeper's
   *  observer queues the keeper's own, so they run first. Under Bun the scheduler's tasks are
   *  immediates, which run after a timer, so this holds the keeper's timer on the fake clock until
   *  React's tasks have run, and then lets it fire. */
  land: (state: S) => Promise<void>;
  /** Draws the box again with the keeper turned on or off, or the box hidden. */
  redraw: (props: { active?: boolean; hidden?: boolean }) => Promise<void>;
  /** Takes the box away, and lets frames run freely again. */
  unmount: () => void;
}

function drawBox<S>(active: boolean, initial: S, draw: Draw<S>): Drawn<S> {
  const handle: Handle<S> = { set: () => undefined };
  let props = { active, hidden: false };
  let asked = new Map<number, FrameRequestCallback>();
  let made = 0;
  const request = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    made += 1;
    asked.set(made, callback);
    return made;
  });
  const cancel = spyOn(window, "cancelAnimationFrame").mockImplementation((id) => {
    asked.delete(id);
  });
  const fixture = () => (
    <Fixture
      active={props.active}
      draw={draw}
      handle={handle}
      hidden={props.hidden}
      initial={initial}
    />
  );
  const view = render(fixture());
  return {
    change: async (state) => {
      await act(async () => handle.set(state));
      await settled();
    },
    frame: async () => {
      await act(async () => {
        const due = asked;
        asked = new Map();
        for (const callback of due.values()) callback(performance.now());
      });
    },
    land: async (state) => {
      await onFakeClock(async () => {
        outsideAct(() => handle.set(state));
        await reactTasks();
      });
    },
    redraw: async (next) => {
      props = { ...props, ...next };
      view.rerender(fixture());
      await settled();
    },
    unmount: () => {
      view.unmount();
      cancel.mockRestore();
      request.mockRestore();
    },
  };
}

/** Waits a task, by which every observer callback and microtask the last change set off has run. */
async function settled(): Promise<void> {
  await act(async () => {
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, 0);
    await promise;
  });
}

/** Runs `update` as code outside any event does - a write that lands - outside `act`, so React
 *  renders it on its own scheduler and runs its passive effects in a task after the commit. React
 *  is told it is not in a test while it runs, so it does not warn of an update outside `act`. */
function outsideAct(update: () => void): void {
  const environment = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean };
  const inTest = environment.IS_REACT_ACT_ENVIRONMENT;
  environment.IS_REACT_ACT_ENVIRONMENT = false;
  try {
    update();
  } finally {
    environment.IS_REACT_ACT_ENVIRONMENT = inTest;
  }
}

/** Lets React's scheduler run what an update outside `act` queued: the render and its commit in
 *  one task, the commit's passive effects in the next. Under Bun the scheduler's tasks are
 *  immediates, which the fake clock does not hold. */
async function reactTasks(): Promise<void> {
  for (let turn = 0; turn < 10; turn += 1) {
    const { promise, resolve } = Promise.withResolvers<void>();
    setImmediate(resolve);
    await promise;
  }
}

/** Where the focus is: its control's name, and the history entry the control stands in. */
function focusName(): string {
  const focus = document.activeElement;
  if (!(focus instanceof HTMLElement) || focus === document.body) return PAGE;
  const name = focus.getAttribute("aria-label") ?? focus.textContent?.trim() ?? "";
  const entry = focus.closest("[data-history-entry]")?.getAttribute("data-history-entry") ?? null;
  return entry === null ? name : `${name} in ${entry}`;
}

/** How the reader acts on the control a case gives the focus before it changes the box: a key
 *  pressed on it, a click of the pointer, which gives it the focus as it presses it, or nothing. */
type Press = "key" | "none" | "pointer";

/** Gives `control` the focus with the reader's `press` of it; a press by `key` is made with
 *  `key`, Enter where the case names none. */
async function take(control: HTMLElement, press: Press, key = "{Enter}"): Promise<void> {
  if (press === "pointer") {
    await userEvent.click(control);
    return;
  }
  control.focus();
  if (press === "key") await userEvent.keyboard(key);
}

/** Save, which turns disabled while its write is out. Discard, below it, stands nearer, but under
 *  the box's lower edge, out of sight. */
function saveForm(writing: boolean): ReactNode {
  return (
    <>
      <button data-top={420} type="button">
        Edit title
      </button>
      <button data-top={500} disabled={writing} type="button">
        Save
      </button>
      <button data-top={570} type="button">
        Discard
      </button>
    </>
  );
}

/** Save given the focus with the reader's `press`, and then disabled as its write goes out. */
async function saveWhileWriting(active: boolean, press: Press): Promise<string> {
  const box = drawBox<boolean>(active, false, saveForm);
  try {
    await take(screen.getByRole("button", { name: "Save" }), press);
    await box.change(true);
    return focusName();
  } finally {
    box.unmount();
  }
}

test("a control the reader pressed, disabled while its write is out, hands the focus to the nearest control in sight", async () => {
  const restore = chromiumLayout();
  try {
    expect(
      await saveWhileWriting(false, "pointer"),
      "with no keeper the focus falls to the page"
    ).toBe(PAGE);
    expect(await saveWhileWriting(true, "pointer"), "Discard is nearer, but out of sight").toBe(
      "Edit title"
    );
    expect(await saveWhileWriting(true, "key"), "Save pressed by keyboard").toBe("Edit title");
  } finally {
    restore();
  }
});

test("a control disabled with no press of the reader's leaves the focus on the page's body", async () => {
  const restore = chromiumLayout();
  try {
    expect(await saveWhileWriting(true, "none")).toBe(PAGE);
  } finally {
    restore();
  }
});

test("a press the reader made on a control is forgotten once the focus moves to another control", async () => {
  const restore = chromiumLayout();
  const box = drawBox<boolean>(true, false, saveForm);
  try {
    const save = screen.getByRole("button", { name: "Save" });
    await take(save, "key");
    screen.getByRole("button", { name: "Edit title" }).focus();
    save.focus();
    await box.change(true);
    expect(focusName(), "Save, back in the focus with no press, turned disabled").toBe(PAGE);
  } finally {
    box.unmount();
    restore();
  }
});

/** A turn's controls: Collapse; Show more, which gives way to a loading line while the rest of the
 *  turn is read; a line that counts the turns left; Copy link, further down. With `handsOn`, Copy
 *  link takes the focus itself as the loading line is drawn, in a layout effect of that commit
 *  (`CopyLinkTakesFocus`) or in a passive one (`CopyLinkTakesFocusAfter`). */
function showMore(loading: boolean, handsOn?: "layout" | "passive"): ReactNode {
  return (
    <>
      <button data-top={100} type="button">
        Collapse
      </button>
      {loading ? (
        <p data-top={140}>Loading…</p>
      ) : (
        <button data-top={140} type="button">
          Show more
        </button>
      )}
      <p data-top={170}>3 more turns</p>
      {handsOn === "layout" ? (
        <CopyLinkTakesFocus loading={loading} />
      ) : handsOn === "passive" ? (
        <CopyLinkTakesFocusAfter loading={loading} />
      ) : (
        <button data-top={300} type="button">
          Copy link
        </button>
      )}
    </>
  );
}

/** A site's own hand-on: as Show more gives way to its loading line, a layout effect of that same
 *  commit gives Copy link the focus. */
function CopyLinkTakesFocus({ loading }: { loading: boolean }): ReactNode {
  const button = useRef<HTMLButtonElement>(null);
  useLayoutEffect(() => {
    if (loading) button.current?.focus();
  }, [loading]);
  return (
    <button data-top={300} ref={button} type="button">
      Copy link
    </button>
  );
}

/** A site's own hand-on made in a passive effect, as the blocked-by editor's row makes it when
 *  its editor goes: only where the focus fell to the page, Copy link takes it as Show more gives
 *  way to its loading line. */
function CopyLinkTakesFocusAfter({ loading }: { loading: boolean }): ReactNode {
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    const focus = document.activeElement;
    if (loading && (focus === null || focus === document.body)) button.current?.focus();
  }, [loading]);
  return (
    <button data-top={300} ref={button} type="button">
      Copy link
    </button>
  );
}

/** Show more given the focus with the reader's `press`, made with `key` where it is a key's, and
 *  then replaced by its loading line. */
async function showMoreLoading(active: boolean, press: Press, key = "{Enter}"): Promise<string> {
  const box = drawBox(active, false, (loading) => showMore(loading));
  try {
    await take(screen.getByRole("button", { name: "Show more" }), press, key);
    await box.change(true);
    return focusName();
  } finally {
    box.unmount();
  }
}

test("a control the reader pressed, replaced by a loading line, hands the focus to the nearest control that stays", async () => {
  const restore = chromiumLayout();
  try {
    expect(await showMoreLoading(false, "key"), "with no keeper the focus falls to the page").toBe(
      PAGE
    );
    expect(await showMoreLoading(true, "key")).toBe("Collapse");
  } finally {
    restore();
  }
});

test("a control removed with no press of the reader's leaves the focus on the page's body", async () => {
  const restore = chromiumLayout();
  try {
    expect(await showMoreLoading(true, "none"), "Show more replaced by its loading line").toBe(
      PAGE
    );
    expect(await loadOlderGone(true, "none"), "Load older gone at the log's end").toBe(PAGE);
    const box = drawBox(true, false, (loading) => showMore(loading));
    try {
      screen.getByRole("button", { name: "Show more" }).focus();
      await box.land(true);
      expect(focusName(), "Show more taken away by a change from outside an event").toBe(PAGE);
    } finally {
      box.unmount();
    }
  } finally {
    restore();
  }
});

test("a key that acts on nothing is no press: the control taken away after it leaves the focus on the page's body", async () => {
  const restore = chromiumLayout();
  try {
    expect(
      await showMoreLoading(true, "key", "{ArrowDown}"),
      "ArrowDown pressed on Show more"
    ).toBe(PAGE);
    expect(await showMoreLoading(true, "key", "{PageDown}"), "PageDown pressed on Show more").toBe(
      PAGE
    );
  } finally {
    restore();
  }
});

/** A row of `rowList`: its name, and where it stands. */
interface ListRow {
  readonly name: string;
  readonly top: number;
}

const ROWS: readonly ListRow[] = [
  { name: "Row one", top: 60 },
  { name: "Row two", top: 100 },
  { name: "Row three", top: 140 },
];

/** A list whose rows each hold a control that takes Delete as its act: it prevents the key's
 *  default and takes its row away. It leaves every other key alone. */
function rowList(rows: readonly ListRow[], set: (rows: readonly ListRow[]) => void): ReactNode {
  return (
    <ul data-height={120} data-top={60}>
      {rows.map((row) => (
        <li data-height={40} data-top={row.top} key={row.name}>
          <button
            data-top={row.top + 4}
            onKeyDown={(event) => {
              if (event.key !== "Delete") return;
              event.preventDefault();
              set(rows.filter((other) => other !== row));
            }}
            type="button"
          >
            {row.name}
          </button>
        </li>
      ))}
    </ul>
  );
}

test("a key the control takes as its act is a press that hands the focus on, and a key it leaves alone is none", async () => {
  const restore = chromiumLayout();
  try {
    const taken = drawBox<readonly ListRow[]>(true, ROWS, rowList);
    try {
      screen.getByRole("button", { name: "Row two" }).focus();
      await userEvent.keyboard("{Delete}");
      await settled();
      expect(screen.queryByRole("button", { name: "Row two" }), "Delete took no row away").toBe(
        null
      );
      expect(focusName(), "Row two took Delete and went").toBe("Row three");
    } finally {
      taken.unmount();
    }
    const left = drawBox<readonly ListRow[]>(true, ROWS, rowList);
    try {
      screen.getByRole("button", { name: "Row two" }).focus();
      await userEvent.keyboard("{Backspace}");
      await left.change(ROWS.filter((row) => row.name !== "Row two"));
      expect(focusName(), "Row two, left Backspace, then taken away by another change").toBe(PAGE);
    } finally {
      left.unmount();
    }
  } finally {
    restore();
  }
});

/** How a line with Retry stands: whether the read Retry sent is out, which Retry reads as busy,
 *  and whether a read has answered, which takes the line and its Retry away. */
interface RetryDrawn {
  readonly answered: boolean;
  readonly busy: boolean;
}

/** Collapse; a line that says a read failed, with its Retry, or the read's answer; Copy link,
 *  further down. */
function retryLine({ answered, busy }: RetryDrawn): ReactNode {
  return (
    <>
      <button data-top={100} type="button">
        Collapse
      </button>
      {answered ? (
        <p data-top={140}>The row</p>
      ) : (
        <div data-top={140}>
          <p data-top={140}>The row could not be read.</p>
          <button aria-busy={busy || undefined} data-top={164} type="button">
            Retry
          </button>
        </div>
      )}
      <button data-top={300} type="button">
        Copy link
      </button>
    </>
  );
}

/** Retry pressed by the reader's Enter, and then, step by step, the line drawn as a step says,
 *  or two frames drawn: where the focus is after the last step. */
async function retryThrough(steps: readonly (RetryDrawn | "two frames")[]): Promise<string> {
  const box = drawBox<RetryDrawn>(true, { answered: false, busy: false }, retryLine);
  try {
    await take(screen.getByRole("button", { name: "Retry" }), "key");
    for (const step of steps) {
      if (step !== "two frames") {
        await box.change(step);
        continue;
      }
      await box.frame();
      await box.frame();
    }
    return focusName();
  } finally {
    box.unmount();
  }
}

test("a press stands while its act is out, and is forgotten once the site answers it with the control standing", async () => {
  const restore = chromiumLayout();
  const out: RetryDrawn = { answered: false, busy: true };
  try {
    expect(
      await retryThrough([out, { answered: false, busy: false }, { answered: true, busy: false }]),
      "Retry's read failed again, and then a read that answered on its own took the line away"
    ).toBe(PAGE);
    expect(
      await retryThrough([out, "two frames", { answered: true, busy: true }]),
      "another change took Retry away while its read was out, frames after the press"
    ).toBe("Collapse");
    expect(
      await retryThrough([out, { answered: true, busy: false }]),
      "the read Retry sent answered and took the line away"
    ).toBe("Collapse");
  } finally {
    restore();
  }
});

test("a press whose act marks the control busy in no way is answered once two frames have drawn after it", async () => {
  const restore = chromiumLayout();
  try {
    expect(
      await retryThrough(["two frames", { answered: true, busy: false }]),
      "Retry, never busy, taken away by another change once two frames had drawn"
    ).toBe(PAGE);
  } finally {
    restore();
  }
});

/** A row of Details whose read failed: its line says so with Retry (`DetailsErrorLine`) while
 *  the read stands failed (`standsFailed`), Retry reads busy while the read it sent is out, and it
 *  hands its focus on to Collapse, above it, where the reader's press of Retry took it away
 *  (`useRetryHandOff`). Each read the row makes is answered by the case (`read`). */
function FailedRow({ read }: { read: () => Promise<string> }): ReactNode {
  const row = useQuery({ queryFn: read, queryKey: ["row"] });
  const collapse = useRef<HTMLButtonElement>(null);
  const handOff = useRetryHandOff(() => collapse.current);
  return (
    <>
      <button data-top={100} ref={collapse} type="button">
        Collapse
      </button>
      <div data-top={140}>
        {standsFailed(row) ? (
          <DetailsErrorLine
            error={row.error}
            failure={row.errorUpdateCount}
            message="The row could not be read."
            onRetry={retryRead(row)}
            {...handOff}
            retrying={row.isFetching}
          />
        ) : (
          <p data-top={140}>{row.data ?? "Loading…"}</p>
        )}
      </div>
      <button data-top={300} type="button">
        Copy link
      </button>
    </>
  );
}

test("a Retry whose read failed again is answered: a read that answers on its own and takes the line away leaves the focus on the page's body", async () => {
  const restore = chromiumLayout();
  const reads: ReturnType<typeof Promise.withResolvers<string>>[] = [];
  const read = () => {
    const answer = Promise.withResolvers<string>();
    reads.push(answer);
    return answer.promise;
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const box = drawBox(true, null, () => (
    <QueryClientProvider client={client}>
      <FailedRow read={read} />
    </QueryClientProvider>
  ));
  try {
    await act(async () => reads[0]?.reject(new Error("the row could not be read")));
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    await take(retry, "key");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    await act(async () => reads[1]?.reject(new Error("the row could not be read")));
    expect(retry.isConnected, "the line went with a read that failed again").toBe(true);
    expect(retry).not.toHaveAttribute("aria-busy");
    expect(focusName(), "Retry lost the focus as its read failed again").toBe(CONTROL_LABELS.retry);

    act(() => void client.invalidateQueries({ queryKey: ["row"] }));
    await waitFor(() => expect(reads).toHaveLength(3));
    await act(async () => reads[2]?.resolve("The row"));
    await settled();
    expect(retry.isConnected, "the read that answered left the line").toBe(false);
    expect(focusName(), "a read the reader did not press for took Retry away").toBe(PAGE);
  } finally {
    box.unmount();
    client.clear();
    restore();
  }
});

/** The history's three turns, each with its Reply and then what `pinOf` draws for it: `drawing`
 *  keys the middle turn, so a new drawing remounts it as new elements under the same
 *  `data-history-entry`. */
function historyTurns(drawing: number, pinOf: (turn: number) => ReactNode): ReactNode {
  return (
    <ol data-height={180} data-top={60}>
      {[0, 1, 2].map((turn) => (
        <li
          data-height={60}
          data-history-entry={`turn:${turn}`}
          data-top={60 + turn * 60}
          key={turn === 1 ? `turn-1-${drawing}` : `turn-${turn}`}
        >
          <button data-top={70 + turn * 60} type="button">
            Reply
          </button>
          {pinOf(turn)}
        </li>
      ))}
    </ol>
  );
}

/** The middle turn's control named Pin. */
function middlePin(): HTMLElement {
  return within(screen.getAllByRole("listitem")[1]).getByRole("button", { name: "Pin" });
}

/** What `turns` draws of the history's three turns: `drawing` keys the middle one
 *  (`historyTurns`), and `pin` draws that turn's Pin. */
interface TurnsDrawn {
  readonly drawing: number;
  readonly pin: boolean;
}

/** Three turns of the history, each with Reply and Pin, but the middle one's Pin only while `pin`
 *  holds. */
function turns({ drawing, pin }: TurnsDrawn): ReactNode {
  return historyTurns(drawing, (turn) =>
    turn === 1 && !pin ? null : (
      <button data-top={94 + turn * 60} type="button">
        Pin
      </button>
    )
  );
}

/** The middle turn's Pin, given the focus with the reader's `press`, is remounted with its entry
 *  under a new key, and then taken away: where the focus is after each. */
async function entryRemounted(active: boolean, press: Press): Promise<readonly string[]> {
  const box = drawBox<TurnsDrawn>(active, { drawing: 0, pin: true }, turns);
  try {
    await take(middlePin(), press);
    await box.change({ drawing: 1, pin: true });
    const remounted = focusName();
    await box.change({ drawing: 1, pin: false });
    return [remounted, focusName()];
  } finally {
    box.unmount();
  }
}

test("an entry remounted under a new key gives the same control in the new entry the focus, pressed or not, and keeps the reader's press", async () => {
  const restore = chromiumLayout();
  try {
    expect(
      (await entryRemounted(false, "none"))[0],
      "with no keeper the focus falls to the page"
    ).toBe(PAGE);
    // The new entry's Reply stands as near to where the lost Pin stood, and comes first.
    expect(
      await entryRemounted(true, "none"),
      "with no press, Pin then taken away leaves the focus on the page"
    ).toEqual(["Pin in turn:1", PAGE]);
    expect(
      await entryRemounted(true, "key"),
      "Pin pressed, then taken away, hands the focus to the nearest control"
    ).toEqual(["Pin in turn:1", "Reply in turn:1"]);
  } finally {
    restore();
  }
});

/** How the history's three turns stand in `pinTurns`: `drawing` keys the middle one
 *  (`historyTurns`), and `pinned` says whether that turn is pinned. */
interface PinTurnsDrawn {
  readonly drawing: number;
  readonly pinned: boolean;
}

/** Three turns of the history, each with Reply and Pin, as in `turns`, but the middle turn's Pin
 *  is a toggle, as `PinButton` is: a press pins the turn at once, and the same button is named
 *  Unpin while the turn is pinned. It shows no busy state, so the reader's press of it is answered
 *  two frames later (`watchPresses`). */
function pinTurns(state: PinTurnsDrawn, set: (state: PinTurnsDrawn) => void): ReactNode {
  return historyTurns(state.drawing, (turn) => (
    <button
      aria-label={turn === 1 && state.pinned ? "Unpin" : "Pin"}
      data-top={94 + turn * 60}
      onClick={turn === 1 ? () => set({ ...state, pinned: !state.pinned }) : undefined}
      type="button"
    />
  ));
}

/** The middle turn's Pin in `pinTurns`, given the focus, is relabelled Unpin in place: by the
 *  reader's Enter on it where `press` is "key", by another session's change where it is "none".
 *  The reader's press is answered two frames after it, as in `pinTurns`, and the Pin's entry is
 *  drawn anew under a new key a frame after that, once the press stands answered: where the focus
 *  is then. */
async function pinRelabelledThenRemounted(press: "key" | "none"): Promise<string> {
  const box = drawBox<PinTurnsDrawn>(true, { drawing: 0, pinned: false }, pinTurns);
  try {
    const pin = middlePin();
    await take(pin, press);
    if (press === "none") await box.change({ drawing: 0, pinned: true });
    expect(pin, "the Pin was not relabelled Unpin in place").toHaveAttribute("aria-label", "Unpin");
    await box.frame();
    await box.frame();
    await box.frame();
    await box.change({ drawing: 1, pinned: true });
    expect(pin.isConnected, "the turn was not drawn anew under a new key").toBe(false);
    return focusName();
  } finally {
    box.unmount();
  }
}

test("a Pin the reader's Enter relabelled Unpin is the same control when its entry is drawn anew under a new key", async () => {
  const restore = chromiumLayout();
  try {
    expect(
      await pinRelabelledThenRemounted("key"),
      "the focus did not go to the Unpin drawn anew, though the reader's own press renamed it"
    ).toBe("Unpin in turn:1");
  } finally {
    restore();
  }
});

test("a Pin relabelled Unpin by a live change, with no press of the reader's, is the same control when its entry is drawn anew under a new key", async () => {
  const restore = chromiumLayout();
  try {
    expect(
      await pinRelabelledThenRemounted("none"),
      "the focus did not go to the Unpin drawn anew, though a live change only renamed it"
    ).toBe("Unpin in turn:1");
  } finally {
    restore();
  }
});

/** Two turns of the log and, at its end, Load older, which goes away once the log's last page is
 *  read, with no control drawn in its place. */
function logEnd(more: boolean): ReactNode {
  return (
    <>
      <ol data-height={80} data-top={60}>
        <li data-height={40} data-history-entry="turn:1" data-top={60}>
          <button data-top={64} type="button">
            Pin
          </button>
        </li>
        <li data-height={40} data-history-entry="turn:2" data-top={100}>
          <button data-top={104} type="button">
            Pin
          </button>
        </li>
      </ol>
      {more ? (
        <button data-top={148} type="button">
          Load older
        </button>
      ) : null}
    </>
  );
}

/** Load older given the focus with the reader's `press`, and then gone as the log's last page is
 *  read. */
async function loadOlderGone(active: boolean, press: Press): Promise<string> {
  const box = drawBox(active, true, (more) => logEnd(more));
  try {
    await take(screen.getByRole("button", { name: "Load older" }), press);
    await box.change(false);
    return focusName();
  } finally {
    box.unmount();
  }
}

test("Load older the reader pressed, removed at the log's end, hands the focus to the control just above it, not the heading", async () => {
  const restore = chromiumLayout();
  try {
    expect(await loadOlderGone(false, "key"), "with no keeper the focus falls to the page").toBe(
      PAGE
    );
    expect(await loadOlderGone(true, "key")).toBe("Pin in turn:2");
  } finally {
    restore();
  }
});

/** Details' resize handle, fixed to the screen and as tall as it, and under the box's head its
 *  scrolling body, where Retry, pressed by the reader, goes once its read answers. With `others`,
 *  Collapse and Copy link stand in the body around Retry; without, Retry is the body's one
 *  control. */
async function retryAnsweredBesideHandle(others: boolean): Promise<string> {
  const box = drawBox(true, false, (answered) => (
    <>
      <button
        aria-label="Resize Details"
        data-height={560}
        data-top={0}
        style={{ position: "fixed" }}
        type="button"
      />
      <div data-height={500} data-top={60} style={{ overflowY: "auto" }}>
        {others ? (
          <button data-top={260} type="button">
            Collapse
          </button>
        ) : null}
        {answered ? null : (
          <button data-top={300} type="button">
            Retry
          </button>
        )}
        {others ? (
          <button data-top={420} type="button">
            Copy link
          </button>
        ) : null}
      </div>
    </>
  ));
  try {
    await take(screen.getByRole("button", { name: "Retry" }), "key");
    await box.change(true);
    return focusName();
  } finally {
    box.unmount();
  }
}

test("a control the reader pressed, gone from the box's scrolling body, hands the focus to a control of that body, never to a fixed one", async () => {
  const restore = chromiumLayout();
  try {
    expect(
      await retryAnsweredBesideHandle(true),
      "the handle stands at no distance from Retry's place, and comes first"
    ).toBe("Collapse");
    expect(
      await retryAnsweredBesideHandle(false),
      "with no other control in the body, the nearest in the rest of the box"
    ).toBe("Close");
  } finally {
    restore();
  }
});

/** Show more, pressed by the reader, holds the focus, the reader moves it with `move`, and then
 *  Show more gives way to its loading line. */
async function movedThenLoading(move: () => void): Promise<string> {
  const box = drawBox(true, false, (loading) => showMore(loading));
  try {
    await take(screen.getByRole("button", { name: "Show more" }), "key");
    move();
    await settled();
    await box.change(true);
    return focusName();
  } finally {
    box.unmount();
  }
}

test("a focus the reader moved elsewhere stays where they put it", async () => {
  const restore = chromiumLayout();
  try {
    expect(
      await movedThenLoading(() => screen.getByRole("button", { name: "Copy link" }).focus()),
      "to another control of Details"
    ).toBe("Copy link");
    expect(
      await movedThenLoading(() => screen.getByRole("button", { name: "Page control" }).focus()),
      "to a control of the page beside Details"
    ).toBe("Page control");
    expect(
      await movedThenLoading(() => screen.getByRole("button", { name: "Show more" }).blur()),
      "to the page, by a press on no control"
    ).toBe(PAGE);
  } finally {
    restore();
  }
});

test("a site's own hand-on, made in the commit that changed the box, keeps the focus it gave", async () => {
  const restore = chromiumLayout();
  const box = drawBox(true, false, (loading) => showMore(loading, "layout"));
  try {
    await take(screen.getByRole("button", { name: "Show more" }), "key");
    await box.change(true);
    expect(focusName(), "the keeper's own choice, Collapse, is nearer").toBe("Copy link");
  } finally {
    box.unmount();
    restore();
  }
});

test("a site's own hand-on, made in a passive effect after a change from outside an event, keeps the focus it gave", async () => {
  const restore = chromiumLayout();
  const box = drawBox(true, false, (loading) => showMore(loading, "passive"));
  try {
    await take(screen.getByRole("button", { name: "Show more" }), "key");
    await box.land(true);
    expect(focusName(), "the keeper's own choice, Collapse, is nearer").toBe("Copy link");
  } finally {
    box.unmount();
    restore();
  }
});

test("the keeper does nothing while it is turned off or the box is hidden", async () => {
  const restore = chromiumLayout();
  try {
    const off = drawBox(true, false, (loading) => showMore(loading));
    try {
      await take(screen.getByRole("button", { name: "Show more" }), "key");
      await off.redraw({ active: false });
      await off.change(true);
      expect(focusName(), "turned off after it noted Show more").toBe(PAGE);
    } finally {
      off.unmount();
    }
    const hidden = drawBox(true, false, (loading) => showMore(loading));
    try {
      await take(screen.getByRole("button", { name: "Show more" }), "key");
      await hidden.redraw({ hidden: true });
      await hidden.change(true);
      expect(focusName(), "Show more removed while the box is hidden").toBe(PAGE);
    } finally {
      hidden.unmount();
    }
  } finally {
    restore();
  }
});

/** The new issue page's panel in Details, as its root fills the margin's slot: above the margin's
 *  own content, Copy link, and a Retry that goes once it is pressed, as a Retry does once its read
 *  answers. */
function DetailsPanel(): ReactNode {
  const [failed, setFailed] = useState(true);
  return (
    <>
      <button data-top={300} type="button">
        Copy link
      </button>
      {failed ? (
        <button data-top={364} onClick={() => setFailed(false)} type="button">
          Retry
        </button>
      ) : null}
    </>
  );
}

const fillDetails: MarginFill = (content) => (
  <>
    <DetailsPanel />
    {content}
  </>
);

/** What the new issue page's root does for Details: it sets the drawer's words and fills the
 *  margin's slot with its panel; and the page's own control that opens the drawer. */
function NewPageRoot(): ReactNode {
  useDrawerPage(issue);
  useFillMarginSlot(fillDetails);
  const drawer = useMarginDrawer();
  return (
    <button onClick={() => drawer?.setOpen(true)} type="button">
      Open Details
    </button>
  );
}

/** The shell as it draws the new issue page's frame (`app.tsx`'s `ShellProviders`): the margin's
 *  providers, with Details as the margin's drawer, closed on the load, and the margin itself. */
function NewPageShell(): ReactNode {
  const [open, setOpen] = useState(false);
  const [page, setPage] = useState<MarginDrawerPage | undefined>(undefined);
  const drawer = useMemo<MarginDrawer>(() => ({ open, page, setOpen, setPage }), [open, page]);
  return (
    <MarginProvider>
      <MarginSlotProvider drawer={drawer} drawsSlot>
        <NewPageRoot />
        <Margin />
      </MarginSlotProvider>
    </MarginProvider>
  );
}

/** What the margin reads of the issue, answered before it asks. */
function issueClient(): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  return queryClient;
}

test("Details' own drawer keeps the focus inside it when the control that held it goes", async () => {
  const restore = chromiumLayout();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={issueClient()}>
        <NewPageShell />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Open Details" }));
    // `Margin` gives the drawer no `data-top` (`layoutOf`): it stands where the fixture's box does.
    const drawer = screen.getByTestId("desktop-margin-shell");
    drawer.setAttribute("data-top", "0");
    drawer.setAttribute("data-height", "560");
    screen.getByRole("button", { name: "Retry" }).focus();
    await user.keyboard("{Enter}");
    await settled();
    expect(
      document.activeElement?.closest(`[${DETAILS_ATTRIBUTE}]`) ?? null,
      `the focus left Details for ${focusName()}`
    ).not.toBeNull();
  } finally {
    view.unmount();
    restoreMatchMedia();
    restore();
  }
});
