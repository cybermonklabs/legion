import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent } from "../../api/types";
import recorded from "./__fixtures__/tmux38-layouts.json";
import type { MachinePane, MachineWindow } from "./machine-model";
import { type PaneCounts, WindowDrawing } from "./WindowDrawing";

/** A viewer who is owed nothing by any session: the drawing takes the counts, it never reads
 *  them, so a case that is not about the first state hands it two empty maps. */
const NOTHING_OWED: PaneCounts = { asksForYou: new Map(), unreadReplies: new Map() };

/** The three-pane window this design recorded from tmux 3.5a: `%0` over `%1` beside `%2`. */
const THREE_PANES = "b05b,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13,2}]";
/** The same window as tmux 3.8 writes its layout, in the JSON form. */
const THREE_PANES_JSON =
  '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":12,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"h","w":80,"h":11,"x":0,"y":13,"c":[{"t":"p","w":40,"h":11,"x":0,"y":13,"i":1,"I":"%1"},{"t":"p","w":39,"h":11,"x":41,"y":13,"i":2,"I":"%2"}]}]}}';

// Windows with floating panes, as tmux 3.8 and tmux 3.7c write them, each recorded on a server of
// its own (`tmux-layout.test.ts` holds these windows with all that `list-panes` said of them).
/** tmux 3.8: `split-window`, then `new-pane` from the top pane, which tmux writes right after
 *  that pane: the floating %2 (index 2) before the tiled %1 (index 1) it lies over. */
const SPLIT_THEN_FLOATING_38 =
  '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":12,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":28,"h":8,"x":11,"y":9,"i":2,"z":0,"I":"%2"},{"t":"p","w":80,"h":11,"x":0,"y":13,"i":1,"I":"%1"}]}}';
/** tmux 3.8: a second floating pane from the same pane, %3, in front of %2 and written first. */
const TWO_FLOATING_38 =
  '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":12,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":28,"h":8,"x":21,"y":11,"i":3,"z":0,"I":"%3"},{"t":"p","w":28,"h":8,"x":11,"y":9,"i":2,"z":1,"I":"%2"},{"t":"p","w":80,"h":11,"x":0,"y":13,"i":1,"I":"%1"}]}}';
/** tmux 3.7c: the same window, its floating panes among the tiled cells and then named again
 *  after the root, front first. */
const TWO_FLOATING_37C =
  "e84f,80x24,0,0[80x12,0,0,0,80x11,0,13,1,30x10,10,8,2,30x10,20,10,3]<30x10,20,10,3,30x10,10,8,2>";
/** tmux 3.8: a window split left (%0) and right (%1), then `new-pane -t %0`, a floating pane
 *  reaching over the right pane, which the string names after it. */
const FLOATING_OVER_LATER_TILED_38 =
  '{"V":2,"L":{"t":"h","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":40,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":28,"h":8,"x":31,"y":6,"i":2,"z":0,"I":"%2"},{"t":"p","w":39,"h":24,"x":41,"y":0,"i":1,"I":"%1"}]}}';
/** tmux 3.8: the tiled pane killed, so the layout is the one floating pane's cell, at 11,6 in
 *  the 80x24 window tmux lists, and states no window size. */
const FLOATING_ONLY_38 =
  '{"V":2,"L":{"t":"p","w":28,"h":8,"x":11,"y":6,"a":true,"i":0,"z":0,"I":"%1"}}';
/** tmux 3.7c: the same window. */
const FLOATING_ONLY_37C = "4cfd,30x10,10,5,1<30x10,10,5,1>";
/** tmux 3.8: `resize-pane -x 60` takes the floating pane past the right edge (61 + 58 > 80). */
const PAST_RIGHT_38 =
  '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":58,"h":8,"x":61,"y":16,"i":1,"z":0,"I":"%1"}]}}';
/** tmux 3.8: `move-pane -X -5 -Y -3` takes it left of and above the window. */
const LEFT_ABOVE_38 =
  '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":48,"h":18,"x":-4,"y":-2,"i":1,"z":0,"I":"%1"}]}}';
/** tmux 3.8: `move-pane -X -40 -Y -20` takes it wholly outside the window. */
const OFF_WINDOW_38 =
  '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%10"},{"t":"p","w":28,"h":8,"x":-39,"y":-19,"i":1,"z":0,"I":"%11"}]}}';

const NOW = 1_762_000_000_000;

function session(sessionId: string, title: string, lastSeen = NOW - 30_000): Agent {
  return {
    capabilities: [],
    dir: "/w",
    host: "sand-devbox",
    last_activity: null,
    last_seen: lastSeen,
    machine_id: "sand-devbox",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    // The server always writes a session's state (model.Agent), so the document requires it.
    state: "live",
    title,
  };
}

function pane(paneId: string, paneIndex: number, agent: Agent, extra?: Partial<MachinePane>) {
  return {
    otherSessions: [],
    paneId,
    paneIndex,
    session: agent,
    zoomed: false,
    ...extra,
  } satisfies MachinePane;
}

function window(
  panes: readonly MachinePane[],
  layout?: string,
  size?: MachineWindow["size"]
): MachineWindow {
  return { layout, panes, size, windowId: "@1", windowIndex: 2, windowName: "agents" };
}

/** happy-dom lays nothing out and has no `ResizeObserver`, so every case drives the component
 *  through its `width` seam. */
function draw(node: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
}

function boxes(): HTMLElement[] {
  return screen.getAllByTestId("window-pane");
}

/** Each drawn box's pane id, in document order: the order a browser paints sibling boxes with no
 *  z-index in, back first. */
function paneIds(): (string | null)[] {
  return boxes().map((box) => box.getAttribute("data-pane"));
}

/** A box's place and size, as the drawing set them. */
function placed(box: HTMLElement): string[] {
  return [box.style.left, box.style.top, box.style.width, box.style.height];
}

/** `part` of `whole` as a CSS percentage, worked out the way the drawing writes one. */
function percent(part: number, whole: number): string {
  return `${(part / whole) * 100}%`;
}

test("floating panes are drawn over the tiled ones, the front of their stack on top", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [pane("%0", 0, session("a".repeat(16), "Planner"))];
  try {
    for (const [layout, order] of [
      // The floating %2 over the tiled %1, which the string names after it.
      [SPLIT_THEN_FLOATING_38, ["%0", "%1", "%2"]],
      [FLOATING_OVER_LATER_TILED_38, ["%0", "%1", "%2"]],
      // %3 is the front of the two floating panes: tmux wrote it first, and it is drawn last.
      [TWO_FLOATING_38, ["%0", "%1", "%2", "%3"]],
      [TWO_FLOATING_37C, ["%0", "%1", "%2", "%3"]],
      // The control: three tiled panes keep the layout's order.
      [THREE_PANES_JSON, ["%0", "%1", "%2"]],
    ] as const) {
      const view = draw(
        <WindowDrawing
          counts={NOTHING_OWED}
          now={NOW}
          width={1440}
          window={window(panes, layout)}
        />
      );
      try {
        expect({ layout, order: paneIds() }).toEqual({ layout, order: [...order] });
        // No box carries a z-index of its own, so the document order is the paint order.
        expect(boxes().map((box) => box.style.zIndex)).toEqual(order.map(() => ""));
      } finally {
        view.unmount();
      }
    }
  } finally {
    agents.mockRestore();
  }
});

test("a pane no session names is labelled with the index tmux gives it, wherever it is drawn", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [pane("%0", 0, session("a".repeat(16), "Planner"))];
  try {
    // tmux's `list-panes` gives %1 index 1, %2 index 2 and %3 index 3 in both: tmux 3.8 writes
    // each index, and tmux 3.7c writes the panes in index order.
    for (const layout of [TWO_FLOATING_38, TWO_FLOATING_37C]) {
      const view = draw(
        <WindowDrawing
          counts={NOTHING_OWED}
          now={NOW}
          width={1440}
          window={window(panes, layout)}
        />
      );
      try {
        expect(
          Object.fromEntries(
            boxes().map((box) => [
              box.getAttribute("data-pane"),
              box.getAttribute("data-pane-index"),
            ])
          )
        ).toEqual({ "%0": "0", "%1": "1", "%2": "2", "%3": "3" });
        expect(within(boxes()[3]).getByText("Pane 3")).toBeDefined();
      } finally {
        view.unmount();
      }
    }
  } finally {
    agents.mockRestore();
  }
});

test("a floating pane past the window's edges is drawn as the part of it inside the window", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [
    pane("%0", 0, session("a".repeat(16), "Planner")),
    pane("%1", 1, session("b".repeat(16), "Builder")),
  ];
  try {
    for (const [layout, inside] of [
      // 58 wide from column 61 of 80: the 19 columns up to the right edge.
      [PAST_RIGHT_38, [percent(61, 80), percent(16, 24), percent(19, 80), percent(8, 24)]],
      // 48 by 18 from -4, -2: the 44 by 16 from the window's top left corner.
      [LEFT_ABOVE_38, ["0%", "0%", percent(44, 80), percent(16, 24)]],
    ] as const) {
      const view = draw(
        <WindowDrawing
          counts={NOTHING_OWED}
          now={NOW}
          width={1440}
          window={window(panes, layout)}
        />
      );
      try {
        expect(screen.getByTestId("window-drawing").getAttribute("data-drawn")).toBe("layout");
        const floating = boxes()[1];
        expect(floating.getAttribute("data-floating")).toBe("true");
        expect(placed(floating)).toEqual([...inside]);
        // The session's name and state are in the part that is drawn.
        expect(within(floating).getByText("Builder")).toBeDefined();
        expect(screen.queryByTestId("window-off-window-panes")).toBeNull();
      } finally {
        view.unmount();
      }
    }
  } finally {
    agents.mockRestore();
  }
});

test("a floating pane wholly outside the window is named under the drawing, with its session", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const hidden = session("b".repeat(16), "Hidden");
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [pane("%10", 0, session("a".repeat(16), "Planner")), pane("%11", 1, hidden)],
        OFF_WINDOW_38
      )}
    />
  );
  try {
    // tmux shows nothing of %11, and neither does the drawing.
    expect(paneIds()).toEqual(["%10"]);
    const block = screen.getByTestId("window-off-window-panes");
    expect(within(block).getByTestId("window-off-window-panes-reason").textContent).toBe(
      "This window holds a floating pane that lies wholly outside the window, so tmux shows nothing of it, and the session in that pane is named here rather than above."
    );
    const rows = within(block).getAllByTestId("window-off-window-pane-row");
    expect(rows.map((row) => row.getAttribute("data-pane"))).toEqual(["%11"]);
    expect(rows[0].getAttribute("data-agent")).toBe(hidden.session_id);
    expect(rows[0].getAttribute("data-pane-index")).toBe("1");
    expect(within(rows[0]).getByRole("link", { name: "Hidden" })).toBeDefined();
    // It is a pane the layout names, so it is not among the panes the layout leaves out.
    expect(screen.queryByTestId("window-unnamed-panes")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a window whose one pane floats is drawn at its session's window size, or listed", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [pane("%1", 0, session("b".repeat(16), "Builder"))];
  try {
    for (const [layout, box] of [
      [FLOATING_ONLY_38, [percent(11, 80), percent(6, 24), percent(28, 80), percent(8, 24)]],
      [FLOATING_ONLY_37C, [percent(10, 80), percent(5, 24), percent(30, 80), percent(10, 24)]],
    ] as const) {
      const sized = draw(
        <WindowDrawing
          counts={NOTHING_OWED}
          now={NOW}
          width={1440}
          window={window(panes, layout, { height: 24, width: 80 })}
        />
      );
      try {
        // The window tmux lists, 80x24, with the pane at its own place in it.
        const drawn = screen.getByTestId("window-layout");
        expect(drawn.style.height).toBe("864px");
        expect([drawn.getAttribute("data-columns"), drawn.getAttribute("data-rows")]).toEqual([
          "80",
          "24",
        ]);
        expect(placed(boxes()[0])).toEqual([...box]);
      } finally {
        sized.unmount();
      }
      // A session that reported no size: the layout states none, so the panes are listed.
      const unsized = draw(
        <WindowDrawing
          counts={NOTHING_OWED}
          now={NOW}
          width={1440}
          window={window(panes, layout)}
        />
      );
      try {
        expect(screen.queryByTestId("window-pane")).toBeNull();
        const list = screen.getByTestId("window-pane-list");
        expect(list.getAttribute("data-reason")).toBe("no-window-size");
        expect(within(list).getByTestId("window-pane-list-reason").textContent).toBe(
          "This window's one pane floats, so its layout gives no size for the window, and its session reported none, so its panes are listed in layout order instead."
        );
        expect(
          within(list)
            .getAllByTestId("window-pane-row")
            .map((row) => row.getAttribute("data-agent"))
        ).toEqual(["b".repeat(16)]);
      } finally {
        unsized.unmount();
      }
    }
  } finally {
    agents.mockRestore();
  }
});

test("a JSON layout with a cell below the root it cannot read is listed, never drawn short", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [
    pane("%0", 0, session("a".repeat(16), "Planner")),
    pane("%1", 1, session("b".repeat(16), "Builder")),
    pane("%2", 2, session("c".repeat(16), "Reviewer")),
  ];
  // %2's id without its `%`: a cell tmux did not write, inside the root's second child.
  const broken = THREE_PANES_JSON.replace('"I":"%2"', '"I":"2"');
  expect(broken).not.toBe(THREE_PANES_JSON);
  const view = draw(
    <>
      <WindowDrawing counts={NOTHING_OWED} now={NOW} width={1440} window={window(panes, broken)} />
      {/* The control: the unbroken layout draws all three. */}
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={1440}
        window={window(panes, THREE_PANES_JSON)}
      />
    </>
  );
  try {
    const [refused, drawn] = screen.getAllByTestId("window-drawing");
    expect(refused.getAttribute("data-drawn")).toBe("list");
    expect(within(refused).getByTestId("window-pane-list").getAttribute("data-reason")).toBe(
      "unreadable-layout"
    );
    expect(
      within(refused)
        .getAllByTestId("window-pane-row")
        .map((row) => row.getAttribute("data-pane"))
    ).toEqual(["%0", "%1", "%2"]);
    expect(drawn.getAttribute("data-drawn")).toBe("layout");
    expect(within(drawn).getAllByTestId("window-pane")).toHaveLength(3);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a window of 60 panes whose tmux writes the JSON layout is drawn", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  // Recorded from tmux 3.8: 60 tiled panes in a 400x200 window, 3,756 characters.
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [pane("%0", 0, session("a".repeat(16), "Planner"))],
        recorded.tiledSixtyPanes400x200
      )}
    />
  );
  try {
    expect(screen.getByTestId("window-drawing").getAttribute("data-drawn")).toBe("layout");
    expect(boxes()).toHaveLength(60);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a window is drawn at its own aspect ratio, each pane at the layout's percentages", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner")),
          pane("%1", 1, session("b".repeat(16), "Builder")),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    expect(screen.getByTestId("window-drawing").getAttribute("data-drawn")).toBe("layout");
    // 80 columns by 24 rows, in cells one wide and two tall: 1440 × 2 × 24 / 80.
    const layout = screen.getByTestId("window-layout");
    expect(layout.style.height).toBe("864px");
    expect([layout.getAttribute("data-columns"), layout.getAttribute("data-rows")]).toEqual([
      "80",
      "24",
    ]);

    const [top, left, right] = boxes();
    expect(boxes().map((box) => box.getAttribute("data-pane"))).toEqual(["%0", "%1", "%2"]);
    expect([top.style.left, top.style.top, top.style.width, top.style.height]).toEqual([
      "0%",
      "0%",
      "100%",
      `${(12 / 24) * 100}%`,
    ]);
    expect([left.style.left, left.style.top, left.style.width, left.style.height]).toEqual([
      "0%",
      `${(13 / 24) * 100}%`,
      "50%",
      `${(11 / 24) * 100}%`,
    ]);
    expect([right.style.left, right.style.width]).toEqual([
      `${(41 / 80) * 100}%`,
      `${(39 / 80) * 100}%`,
    ]);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a window whose tmux writes the JSON layout is drawn as the same boxes", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [
    pane("%0", 0, session("a".repeat(16), "Planner")),
    pane("%1", 1, session("b".repeat(16), "Builder")),
  ];
  /** Each drawn pane's id and box, as the drawing placed it. */
  const drawn = (layout: string) => {
    const view = draw(
      <WindowDrawing counts={NOTHING_OWED} now={NOW} width={1440} window={window(panes, layout)} />
    );
    try {
      expect(screen.getByTestId("window-drawing").getAttribute("data-drawn")).toBe("layout");
      return boxes().map((box) => [
        box.getAttribute("data-pane"),
        box.style.left,
        box.style.top,
        box.style.width,
        box.style.height,
      ]);
    } finally {
      view.unmount();
    }
  };
  try {
    const fromJson = drawn(THREE_PANES_JSON);
    expect(fromJson.map(([paneId]) => paneId)).toEqual(["%0", "%1", "%2"]);
    expect(fromJson).toEqual(drawn(THREE_PANES));
  } finally {
    agents.mockRestore();
  }
});

test("a pane names its session and its state; one with no session says so and shows its index", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner")),
          pane("%1", 1, session("b".repeat(16), "Builder", NOW - 30 * 60_000)),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    const [top, left, right] = boxes();
    expect(top.getAttribute("data-agent")).toBe("a".repeat(16));
    expect(within(top).getByText("Planner")).toBeDefined();
    expect(within(top).getByTestId("window-pane-state").textContent).toBe("Idle");
    // Unseen for half an hour: the registry's own silence, which is Quiet.
    expect(within(left).getByTestId("window-pane-state").textContent).toBe("Quiet");
    expect(right.getAttribute("data-agent")).toBeNull();
    expect(right.textContent).toBe("Pane 2No agent");
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane with no session takes its index from the layout's order and the window's base", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 1, session("a".repeat(16), "Planner")),
          pane("%1", 2, session("b".repeat(16), "Builder")),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    expect(boxes().map((box) => box.getAttribute("data-pane-index"))).toEqual(["1", "2", "3"]);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a zoomed pane is marked, and a pane two sessions name says how many", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner"), { zoomed: true }),
          pane("%1", 1, session("b".repeat(16), "Builder"), {
            otherSessions: [session("c".repeat(16), "Stray")],
          }),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    const [top, left] = boxes();
    expect(top.getAttribute("data-zoomed")).toBe("true");
    expect(within(top).getByTestId("window-pane-zoomed").textContent).toBe("Zoomed");
    expect(left.getAttribute("data-zoomed")).toBe("false");
    expect(within(left).getByTestId("window-pane-others").textContent).toBe(
      "1 other session names this pane"
    );
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane box too narrow or too short to read draws the window as a list, and says why", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [pane("%0", 0, session("a".repeat(16), "Planner"))];
  const view = draw(
    <>
      {/* 40 of 80 columns at 150 px is 75 px, under the 96 px a name and a state dot need. */}
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={150}
        window={window(panes, THREE_PANES)}
      />
      {/* One row of 50 at 390 px is 9.75 px, under the 42 px an Agents row is given. */}
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={390}
        window={window(panes, "b000,80x50,0,0[80x1,0,0,0,80x48,0,2,1]")}
      />
    </>
  );
  try {
    expect(screen.queryByTestId("window-pane")).toBeNull();
    const [narrow, short] = screen.getAllByTestId("window-pane-list");
    for (const list of [narrow, short]) {
      expect(list.getAttribute("data-reason")).toBe("panes-too-small");
      expect(within(list).getByTestId("window-pane-list-reason").textContent).toBe(
        "A pane of this window would be too small to read at this width, so its panes are listed in layout order instead."
      );
    }
    // The list is the layout's own order, which is the order the drawing holds the panes in.
    expect(
      within(narrow)
        .getAllByTestId("window-pane-row")
        .map((row) => row.getAttribute("data-pane"))
    ).toEqual(["%0", "%1", "%2"]);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a window with no layout, and one whose layout will not read, are lists with their reasons", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const panes = [
    pane("%7", 7, session("a".repeat(16), "Planner")),
    pane("%4", 4, session("b".repeat(16), "Builder")),
  ];
  const view = draw(
    <>
      <WindowDrawing counts={NOTHING_OWED} now={NOW} width={1440} window={window(panes)} />
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={1440}
        window={window(panes, "b05b,80x24,0,0[80x12,0,0,0")}
      />
    </>
  );
  try {
    const [absent, refused] = screen.getAllByTestId("window-pane-list");
    expect(absent.getAttribute("data-reason")).toBe("no-layout");
    expect(within(absent).getByTestId("window-pane-list-reason").textContent).toBe(
      "This window reported no layout, so its panes are listed by pane index instead."
    );
    // With no layout to order them, the model's own order - by pane index - is the list's.
    expect(
      within(absent)
        .getAllByTestId("window-pane-row")
        .map((row) => row.getAttribute("data-pane"))
    ).toEqual(["%7", "%4"]);
    expect(refused.getAttribute("data-reason")).toBe("unreadable-layout");
    expect(within(refused).getByTestId("window-pane-list-reason").textContent).toBe(
      "This window's layout could not be read, so its panes are listed by pane index instead."
    );
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane whose session holds an open ask waiting on the viewer needs them", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={{ asksForYou: new Map([["a".repeat(16), 2]]), unreadReplies: new Map() }}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner")),
          pane("%1", 1, session("b".repeat(16), "Builder")),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    const [top, left] = boxes();

    expect(within(top).getByTestId("window-pane-state").getAttribute("data-state")).toBe(
      "needs-you"
    );
    expect(within(top).getByTestId("window-pane-state").textContent).toBe("Needs you");
    // The count is this session's alone: the pane beside it reads its own activity.
    expect(within(left).getByTestId("window-pane-state").getAttribute("data-state")).toBe("idle");
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane whose session has replies the viewer has not read needs them too", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = draw(
    <WindowDrawing
      counts={{ asksForYou: new Map(), unreadReplies: new Map([["a".repeat(16), 1]]) }}
      now={NOW}
      width={1440}
      // Silent for half an hour, which is Quiet on the session's own activity: what the viewer
      // is owed wins over it, as `agentGroup`'s first-match order says.
      window={window(
        [pane("%0", 0, session("a".repeat(16), "Planner", NOW - 30 * 60_000))],
        THREE_PANES
      )}
    />
  );
  try {
    expect(within(boxes()[0]).getByTestId("window-pane-state").getAttribute("data-state")).toBe(
      "needs-you"
    );
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a pane the drawn layout does not name is named under the window, drawn or listed", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const split = session("c".repeat(16), "Just split");
  const panes = [
    pane("%0", 0, session("a".repeat(16), "Planner")),
    pane("%1", 1, session("b".repeat(16), "Builder")),
    pane("%2", 2, session("d".repeat(16), "Tester")),
    pane("%7", 3, split),
  ];
  const view = draw(
    <>
      {/* Drawn: the layout names three cells and the window holds four panes, which is what a
          pane split off after that layout was read looks like. */}
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={1440}
        window={window(panes, THREE_PANES)}
      />
      {/* The same window too narrow to draw: the list walks the layout's cells as well. */}
      <WindowDrawing
        counts={NOTHING_OWED}
        now={NOW}
        width={150}
        window={window(panes, THREE_PANES)}
      />
      {/* No layout at all: the list is the model's own panes, so none of them is left out. */}
      <WindowDrawing counts={NOTHING_OWED} now={NOW} width={1440} window={window(panes)} />
    </>
  );
  try {
    const blocks = screen.getAllByTestId("window-unnamed-panes");
    expect(blocks).toHaveLength(2);
    for (const block of blocks) {
      expect(within(block).getByTestId("window-unnamed-panes-reason").textContent).toBe(
        "This window holds a pane the layout it was drawn from does not name, so the session in that pane is named here rather than above. Each session reports the layout it last read, and a pane split off after that reading is in no cell of it."
      );
      const rows = within(block).getAllByTestId("window-unnamed-pane-row");
      expect(rows.map((row) => row.getAttribute("data-pane"))).toEqual(["%7"]);
      expect(rows[0].getAttribute("data-agent")).toBe(split.session_id);
      expect(rows[0].getAttribute("data-pane-index")).toBe("3");
      expect(within(rows[0]).getByRole("link", { name: "Just split" })).toBeDefined();
    }
    // Neither the drawing nor the list holds `%7` itself: both walk the layout's own cells.
    expect(boxes().map((box) => box.getAttribute("data-pane"))).toEqual(["%0", "%1", "%2"]);
    const [tooSmall, noLayout] = screen.getAllByTestId("window-pane-list");
    expect(
      within(tooSmall)
        .getAllByTestId("window-pane-row")
        .map((row) => row.getAttribute("data-pane"))
    ).toEqual(["%0", "%1", "%2"]);
    expect(
      within(noLayout)
        .getAllByTestId("window-pane-row")
        .map((row) => row.getAttribute("data-pane"))
    ).toEqual(["%0", "%1", "%2", "%7"]);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a session whose pane a fresher session took is named in that pane's box", () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const stray = session("c".repeat(16), "Stray");
  const moved = session("d".repeat(16), "Moved");
  const view = draw(
    <WindowDrawing
      counts={NOTHING_OWED}
      now={NOW}
      width={1440}
      window={window(
        [
          pane("%0", 0, session("a".repeat(16), "Planner")),
          pane("%1", 1, session("b".repeat(16), "Builder"), { otherSessions: [stray, moved] }),
        ],
        THREE_PANES
      )}
    />
  );
  try {
    const others = within(boxes()[1]).getByTestId("window-pane-others");
    expect(others.textContent).toBe("2 other sessions name this pane");
    expect(others.getAttribute("title")).toBe("Other sessions naming this pane: Stray, Moved");
    expect(
      within(others)
        .getAllByTestId("window-pane-other")
        .map((mark) => mark.getAttribute("data-agent"))
    ).toEqual([stray.session_id, moved.session_id]);
    // One pane draws one session: the others are carried, never drawn as a second name in the
    // box, which would read as two sessions drawn in one pane.
    expect(within(boxes()[1]).queryByText("Stray")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});
