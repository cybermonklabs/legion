import { expect, test } from "bun:test";
import { TMUX_LAYOUT_MAX } from "@legion/contracts";

import recorded from "./__fixtures__/tmux38-layouts.json";
import {
  drawWindow,
  parseTmuxLayout,
  type TmuxBox,
  type TmuxLayout,
  type TmuxLayoutPane,
  type TmuxSize,
} from "./tmux-layout";

// Recorded from tmux 3.5a, each the window's own `window_layout`.
/** One pane filling an 80x24 window. */
const ONE_PANE = "b262,80x24,0,0,5";
/** `%0` over `%1` beside `%2`, in an 80x24 window. */
const THREE_PANES_SMALL = "b05b,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13,2}]";
/** The same three panes in a 200x50 window. */
const THREE_PANES_LARGE = "81f0,200x50,0,0[200x25,0,0,0,200x24,0,26{100x24,0,26,1,99x24,101,26,2}]";
/** What that window reports while `%1` is zoomed: the one pane that is on the screen. */
const ZOOMED = "ac9e,200x50,0,0,1";

// Recorded from tmux 3.8, which writes the JSON form, each the window's own `window_layout`: the
// same windows as above, built by the same commands on a fresh server. A fresh server numbers its
// panes from %0, so `ONE_PANE`'s pane (%5, from a server that had made five before it) and
// `JSON_ONE_PANE`'s (%0) differ; the three-pane windows' ids match.
/** One pane filling an 80x24 window. */
const JSON_ONE_PANE = '{"V":2,"L":{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%0"}}';
/** `%0` over `%1` beside `%2`, in an 80x24 window. */
const JSON_THREE_PANES_SMALL =
  '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":12,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"h","w":80,"h":11,"x":0,"y":13,"c":[{"t":"p","w":40,"h":11,"x":0,"y":13,"i":1,"I":"%1"},{"t":"p","w":39,"h":11,"x":41,"y":13,"i":2,"I":"%2"}]}]}}';
/** The same three panes in a 200x50 window. */
const JSON_THREE_PANES_LARGE =
  '{"V":2,"L":{"t":"v","w":200,"h":50,"x":0,"y":0,"c":[{"t":"p","w":200,"h":25,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"h","w":200,"h":24,"x":0,"y":26,"c":[{"t":"p","w":100,"h":24,"x":0,"y":26,"i":1,"I":"%1"},{"t":"p","w":99,"h":24,"x":101,"y":26,"i":2,"I":"%2"}]}]}}';
/** One tiled pane filling an 80x24 window and, over it, the floating pane
 *  `new-pane -X 10 -Y 5 -x 30 -y 10` made. */
const JSON_FLOATING =
  '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":28,"h":8,"x":11,"y":6,"i":1,"z":0,"I":"%1"}]}}';

/**
 * One window as tmux wrote its layout, with what tmux's own `list-panes` said of each pane in the
 * same moment (`pane_index`, `pane_floating_flag`, `pane_left`, `pane_top`, `pane_width`,
 * `pane_height`, `pane_z`) and the window's size (`window_width`, `window_height`): the boxes
 * and the stacking a drawing must reproduce. `z` is given for a floating pane only, since tmux's
 * `pane_z` of a tiled pane counts the floating panes over it and the layout states no such thing.
 */
interface Recording {
  readonly name: string;
  readonly layout: string;
  readonly window: TmuxSize;
  readonly panes: readonly {
    readonly paneId: string;
    readonly index: number;
    readonly box: TmuxBox;
    readonly z?: number;
  }[];
  /** The pane ids from the back to the front, as tmux paints them. */
  readonly paintOrder: readonly string[];
}

function at(x: number, y: number, width: number, height: number): TmuxBox {
  return { height, width, x, y };
}

const WINDOW_80X24: TmuxSize = { height: 24, width: 80 };

/**
 * tmux 3.8's floating panes, each window built by the commands its comment names on an 80x24
 * window of a tmux 3.8 server of its own, with `list-panes` read in the same moment. In each, a
 * floating pane's cell sits right after the pane it was made from, so its place in the string is
 * neither its index nor where tmux paints it.
 */
const TMUX_38: readonly Recording[] = [
  {
    // `split-window`, then `new-pane` from the top pane. The floating %2 is written before the
    // tiled %1 it overlaps.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":12,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":28,"h":8,"x":11,"y":9,"i":2,"z":0,"I":"%2"},{"t":"p","w":80,"h":11,"x":0,"y":13,"i":1,"I":"%1"}]}}',
    name: "a floating pane made from the top pane of a split",
    paintOrder: ["%0", "%1", "%2"],
    panes: [
      { box: at(0, 0, 80, 12), index: 0, paneId: "%0" },
      { box: at(0, 13, 80, 11), index: 1, paneId: "%1" },
      { box: at(11, 9, 28, 8), index: 2, paneId: "%2", z: 0 },
    ],
    window: WINDOW_80X24,
  },
  {
    // A second floating pane from the same pane, which tmux puts in front.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":12,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":28,"h":8,"x":21,"y":11,"i":3,"z":0,"I":"%3"},{"t":"p","w":28,"h":8,"x":11,"y":9,"i":2,"z":1,"I":"%2"},{"t":"p","w":80,"h":11,"x":0,"y":13,"i":1,"I":"%1"}]}}',
    name: "two floating panes made from the same pane",
    paintOrder: ["%0", "%1", "%2", "%3"],
    panes: [
      { box: at(0, 0, 80, 12), index: 0, paneId: "%0" },
      { box: at(0, 13, 80, 11), index: 1, paneId: "%1" },
      { box: at(11, 9, 28, 8), index: 2, paneId: "%2", z: 1 },
      { box: at(21, 11, 28, 8), index: 3, paneId: "%3", z: 0 },
    ],
    window: WINDOW_80X24,
  },
  {
    // The same window after `select-pane -t %2`, which brings %2 to the front while the string
    // keeps its order.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":12,"x":0,"y":0,"l":0,"i":0,"I":"%0"},{"t":"p","w":28,"h":8,"x":21,"y":11,"i":3,"z":1,"I":"%3"},{"t":"p","w":28,"h":8,"x":11,"y":9,"a":true,"i":2,"z":0,"I":"%2"},{"t":"p","w":80,"h":11,"x":0,"y":13,"i":1,"I":"%1"}]}}',
    name: "the older of two floating panes selected",
    paintOrder: ["%0", "%1", "%3", "%2"],
    panes: [
      { box: at(0, 0, 80, 12), index: 0, paneId: "%0" },
      { box: at(0, 13, 80, 11), index: 1, paneId: "%1" },
      { box: at(11, 9, 28, 8), index: 2, paneId: "%2", z: 0 },
      { box: at(21, 11, 28, 8), index: 3, paneId: "%3", z: 1 },
    ],
    window: WINDOW_80X24,
  },
  {
    // `split-window -f` after a floating pane, so the tiled %9 comes after the floating %8 it lies
    // under.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":12,"x":0,"y":0,"a":true,"i":0,"I":"%7"},{"t":"p","w":28,"h":18,"x":11,"y":3,"i":1,"z":0,"I":"%8"},{"t":"p","w":80,"h":11,"x":0,"y":13,"i":2,"I":"%9"}]}}',
    name: "a tiled pane split off after a floating pane",
    paintOrder: ["%7", "%9", "%8"],
    panes: [
      { box: at(0, 0, 80, 12), index: 0, paneId: "%7" },
      { box: at(11, 3, 28, 18), index: 1, paneId: "%8", z: 0 },
      { box: at(0, 13, 80, 11), index: 2, paneId: "%9" },
    ],
    window: WINDOW_80X24,
  },
  {
    // Two floating panes over one tiled pane, after `select-pane -t .1`: the pane of index 1 in
    // front.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":24,"x":0,"y":0,"l":0,"i":0,"I":"%4"},{"t":"p","w":28,"h":8,"x":21,"y":9,"i":2,"z":1,"I":"%6"},{"t":"p","w":28,"h":8,"x":11,"y":6,"a":true,"i":1,"z":0,"I":"%5"}]}}',
    name: "two floating panes over one tiled pane, the first made selected",
    paintOrder: ["%4", "%6", "%5"],
    panes: [
      { box: at(0, 0, 80, 24), index: 0, paneId: "%4" },
      { box: at(11, 6, 28, 8), index: 1, paneId: "%5", z: 0 },
      { box: at(21, 9, 28, 8), index: 2, paneId: "%6", z: 1 },
    ],
    window: WINDOW_80X24,
  },
  {
    // `resize-pane -x 60` takes the box past the right edge.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":58,"h":8,"x":61,"y":16,"i":1,"z":0,"I":"%1"}]}}',
    name: "a floating pane resized past the right edge",
    paintOrder: ["%0", "%1"],
    panes: [
      { box: at(0, 0, 80, 24), index: 0, paneId: "%0" },
      { box: at(61, 16, 58, 8), index: 1, paneId: "%1", z: 0 },
    ],
    window: WINDOW_80X24,
  },
  {
    // `move-pane -X -5 -Y -3` takes it left of and above the window.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":48,"h":18,"x":-4,"y":-2,"i":1,"z":0,"I":"%1"}]}}',
    name: "a floating pane moved left of and above the window",
    paintOrder: ["%0", "%1"],
    panes: [
      { box: at(0, 0, 80, 24), index: 0, paneId: "%0" },
      { box: at(-4, -2, 48, 18), index: 1, paneId: "%1", z: 0 },
    ],
    window: WINDOW_80X24,
  },
  {
    // `move-pane -X -40 -Y -20`: wholly outside the window.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%10"},{"t":"p","w":28,"h":8,"x":-39,"y":-19,"i":1,"z":0,"I":"%11"}]}}',
    name: "a floating pane moved wholly above and left of the window",
    paintOrder: ["%10", "%11"],
    panes: [
      { box: at(0, 0, 80, 24), index: 0, paneId: "%10" },
      { box: at(-39, -19, 28, 8), index: 1, paneId: "%11", z: 0 },
    ],
    window: WINDOW_80X24,
  },
  {
    // The same window after `move-pane -X 79 -Y 23`: past the bottom right corner.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%10"},{"t":"p","w":28,"h":8,"x":80,"y":24,"i":1,"z":0,"I":"%11"}]}}',
    name: "a floating pane moved wholly past the bottom right corner",
    paintOrder: ["%10", "%11"],
    panes: [
      { box: at(0, 0, 80, 24), index: 0, paneId: "%10" },
      { box: at(80, 24, 28, 8), index: 1, paneId: "%11", z: 0 },
    ],
    window: WINDOW_80X24,
  },
  {
    // The tiled pane killed, so the layout is the floating pane's cell alone and states no window
    // size; tmux lists the window as 80x24.
    layout: '{"V":2,"L":{"t":"p","w":28,"h":8,"x":11,"y":6,"a":true,"i":0,"z":0,"I":"%1"}}',
    name: "a window whose one pane floats",
    paintOrder: ["%1"],
    panes: [{ box: at(11, 6, 28, 8), index: 0, paneId: "%1", z: 0 }],
    window: WINDOW_80X24,
  },
  {
    // Two floating panes and no tiled one: tmux keeps the window's own root then.
    layout:
      '{"V":2,"L":{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[{"t":"p","w":28,"h":8,"x":41,"y":13,"i":1,"z":0,"I":"%2"},{"t":"p","w":28,"h":8,"x":11,"y":6,"a":true,"i":0,"z":1,"I":"%1"}]}}',
    name: "a window whose two panes both float",
    paintOrder: ["%1", "%2"],
    panes: [
      { box: at(11, 6, 28, 8), index: 0, paneId: "%1", z: 1 },
      { box: at(41, 13, 28, 8), index: 1, paneId: "%2", z: 0 },
    ],
    window: WINDOW_80X24,
  },
];

/**
 * The same windows as tmux 3.7c writes them, in version 1 (tmux 3.7c built from its release
 * tarball, checked against its published checksum). Each floating pane's cell is among
 * the tiled cells, and the `<…>` group after the root names the floating panes again, front
 * first. tmux 3.7c moves and resizes a floating pane only with the mouse, so its windows with a
 * box left of or above the window could not be made by a command.
 */
const TMUX_37C: readonly Recording[] = [
  {
    layout: "2b8e,80x24,0,0[80x12,0,0,0,80x11,0,13,1,30x10,10,8,2]<30x10,10,8,2>",
    name: "a floating pane made from the top pane of a split",
    paintOrder: ["%0", "%1", "%2"],
    panes: [
      { box: at(0, 0, 80, 12), index: 0, paneId: "%0" },
      { box: at(0, 13, 80, 11), index: 1, paneId: "%1" },
      { box: at(10, 8, 30, 10), index: 2, paneId: "%2", z: 0 },
    ],
    window: WINDOW_80X24,
  },
  {
    layout:
      "e84f,80x24,0,0[80x12,0,0,0,80x11,0,13,1,30x10,10,8,2,30x10,20,10,3]<30x10,20,10,3,30x10,10,8,2>",
    name: "two floating panes made from the same pane",
    paintOrder: ["%0", "%1", "%2", "%3"],
    panes: [
      { box: at(0, 0, 80, 12), index: 0, paneId: "%0" },
      { box: at(0, 13, 80, 11), index: 1, paneId: "%1" },
      { box: at(10, 8, 30, 10), index: 2, paneId: "%2", z: 1 },
      { box: at(20, 10, 30, 10), index: 3, paneId: "%3", z: 0 },
    ],
    window: WINDOW_80X24,
  },
  {
    layout:
      "0307,80x24,0,0[80x12,0,0,0,80x11,0,13,1,30x10,10,8,2,30x10,20,10,3]<30x10,10,8,2,30x10,20,10,3>",
    name: "the older of two floating panes selected",
    paintOrder: ["%0", "%1", "%3", "%2"],
    panes: [
      { box: at(0, 0, 80, 12), index: 0, paneId: "%0" },
      { box: at(0, 13, 80, 11), index: 1, paneId: "%1" },
      { box: at(10, 8, 30, 10), index: 2, paneId: "%2", z: 0 },
      { box: at(20, 10, 30, 10), index: 3, paneId: "%3", z: 1 },
    ],
    window: WINDOW_80X24,
  },
  {
    layout: "8741,80x24,0,0[80x12,0,0,0,30x20,10,2,1,80x11,0,13,2]<30x20,10,2,1>",
    name: "a tiled pane split off after a floating pane",
    paintOrder: ["%0", "%2", "%1"],
    panes: [
      { box: at(0, 0, 80, 12), index: 0, paneId: "%0" },
      { box: at(10, 2, 30, 20), index: 1, paneId: "%1", z: 0 },
      { box: at(0, 13, 80, 11), index: 2, paneId: "%2" },
    ],
    window: WINDOW_80X24,
  },
  {
    // `resize-pane -x 60` does not resize a floating pane in 3.7c; `new-pane -X 60` alone puts
    // the box past the right and bottom edges.
    layout: "7209,80x24,0,0[80x24,0,0,0,30x10,60,15,1]<30x10,60,15,1>",
    name: "a floating pane past the right and bottom edges",
    paintOrder: ["%0", "%1"],
    panes: [
      { box: at(0, 0, 80, 24), index: 0, paneId: "%0" },
      { box: at(60, 15, 30, 10), index: 1, paneId: "%1", z: 0 },
    ],
    window: WINDOW_80X24,
  },
  {
    layout: "4cfd,30x10,10,5,1<30x10,10,5,1>",
    name: "a window whose one pane floats",
    paintOrder: ["%1"],
    panes: [{ box: at(10, 5, 30, 10), index: 0, paneId: "%1", z: 0 }],
    window: WINDOW_80X24,
  },
  {
    layout: "b357,80x24,0,0[30x10,10,5,1,30x10,40,12,2]<30x10,40,12,2,30x10,10,5,1>",
    name: "a window whose two panes both float",
    paintOrder: ["%1", "%2"],
    panes: [
      { box: at(10, 5, 30, 10), index: 0, paneId: "%1", z: 1 },
      { box: at(40, 12, 30, 10), index: 1, paneId: "%2", z: 0 },
    ],
    window: WINDOW_80X24,
  },
];

/** A pane's box alone. */
function boxOf({ height, width, x, y }: TmuxBox): TmuxBox {
  return { height, width, x, y };
}

/** `box` cut to a window of `size`, or undefined where none of it is inside: worked out here
 *  from the window and tmux's own box, apart from the parser's `clip`. */
function inside(box: TmuxBox, size: TmuxSize): TmuxBox | undefined {
  const x = Math.max(box.x, 0);
  const y = Math.max(box.y, 0);
  const width = Math.min(box.x + box.width, size.width) - x;
  const height = Math.min(box.y + box.height, size.height) - y;
  return width > 0 && height > 0 ? { height, width, x, y } : undefined;
}

/** The recording of `list` called `name`. */
function recordingNamed(list: readonly Recording[], name: string): Recording {
  const found = list.find((recording) => recording.name === name);
  if (found === undefined) {
    throw new Error(`no recording called ${name}`);
  }
  return found;
}

function parsed(layout: string): TmuxLayout {
  const read = parseTmuxLayout(layout);
  if (read === undefined) {
    throw new Error(`layout did not parse: ${layout}`);
  }
  return read;
}

/** `layout` with every pane's `index` taken away: what version 1, which states none, reads as. */
function withoutIndexes(layout: TmuxLayout | undefined): unknown {
  return JSON.parse(JSON.stringify(layout, (key, value) => (key === "index" ? undefined : value)));
}

interface Proportion {
  readonly paneId: string;
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

/** Each pane's box as a fraction of the window, which is what a drawing scales to its own box. */
function proportions(layout: string): Proportion[] {
  const { panes, root } = parsed(layout);
  return panes.map((pane) => ({
    height: pane.height / root.height,
    paneId: pane.paneId,
    width: pane.width / root.width,
    x: pane.x / root.width,
    y: pane.y / root.height,
  }));
}

/**
 * Two windows of different sizes cannot hold one shape exactly: the divider is one cell whatever
 * the window's size, so it is 1/24 of the small window's height and 1/50 of the large one's, and
 * every difference between the two layouts below is exactly that share — 1/24 − 1/50 = 0.0217 of
 * the height, 1/80 − 1/200 = 0.0075 of the width. The check is therefore one percent of the
 * window on top of the divider's own share.
 */
const SAME_SHAPE = 0.01 + (1 / 24 - 1 / 50);

test("a window of one pane reads as that pane filling the window", () => {
  const pane: TmuxLayoutPane = { height: 24, kind: "pane", paneId: "%5", width: 80, x: 0, y: 0 };

  expect(parseTmuxLayout(ONE_PANE)).toEqual({
    panes: [pane],
    root: pane,
    window: { height: 24, width: 80 },
  });
});

test("a split window reads as its tree of boxes, each pane keeping tmux's `%` on its id", () => {
  const layout = parseTmuxLayout(THREE_PANES_SMALL);

  expect(layout?.root).toEqual({
    children: [
      { height: 12, kind: "pane", paneId: "%0", width: 80, x: 0, y: 0 },
      {
        children: [
          { height: 11, kind: "pane", paneId: "%1", width: 40, x: 0, y: 13 },
          { height: 11, kind: "pane", paneId: "%2", width: 39, x: 41, y: 13 },
        ],
        height: 11,
        into: "columns",
        kind: "split",
        width: 80,
        x: 0,
        y: 13,
      },
    ],
    height: 24,
    into: "rows",
    kind: "split",
    width: 80,
    x: 0,
    y: 0,
  });
  expect(layout?.panes.map((pane) => pane.paneId)).toEqual(["%0", "%1", "%2"]);
  expect(layout?.window).toEqual({ height: 24, width: 80 });
});

test("the divider tmux counts between two panes is left as tmux counted it", () => {
  const panes = parseTmuxLayout(THREE_PANES_SMALL)?.panes ?? [];
  const [top, left, right] = panes;

  // 40 + 1 + 39 is the window's 80, and 12 + 1 + 11 its 24: the missing cell is the divider, and
  // the parser neither adds it to a pane nor reports a gap of its own.
  expect(left.width + 1 + right.width).toBe(80);
  expect(top.height + 1 + left.height).toBe(24);
  expect(right.x).toBe(left.x + left.width + 1);
});

test("the same window at another size reads as the same shape", () => {
  const small = proportions(THREE_PANES_SMALL);
  const large = proportions(THREE_PANES_LARGE);

  expect(large.map((pane) => pane.paneId)).toEqual(small.map((pane) => pane.paneId));
  for (const [index, pane] of small.entries()) {
    const other = large[index];
    expect(Math.abs(pane.x - other.x)).toBeLessThanOrEqual(SAME_SHAPE);
    expect(Math.abs(pane.y - other.y)).toBeLessThanOrEqual(SAME_SHAPE);
    expect(Math.abs(pane.width - other.width)).toBeLessThanOrEqual(SAME_SHAPE);
    expect(Math.abs(pane.height - other.height)).toBeLessThanOrEqual(SAME_SHAPE);
  }
  // Where the window is split is the same in both, to the cell: half the height, half the width.
  expect([small[0].height, large[0].height]).toEqual([0.5, 0.5]);
  expect([small[1].width, large[1].width]).toEqual([0.5, 0.5]);
});

test("a zoomed window reads as the one pane that is on the screen", () => {
  expect(parseTmuxLayout(ZOOMED)?.panes).toEqual([
    { height: 50, kind: "pane", paneId: "%1", width: 200, x: 0, y: 0 },
  ]);
});

test("an unbalanced bracket is no layout", () => {
  expect(parseTmuxLayout("b05b,80x24,0,0[80x12,0,0,0,80x11,0,13")).toBeUndefined();
  expect(parseTmuxLayout("b05b,80x24,0,0[80x12,0,0,0,80x11,0,13}")).toBeUndefined();
});

test("a cell that is neither a pane nor a split is no layout", () => {
  expect(parseTmuxLayout("b262,80x24,0,0")).toBeUndefined();
  expect(parseTmuxLayout("b262,80x24,0,0,")).toBeUndefined();
});

test("a size that is not a number is no layout", () => {
  expect(parseTmuxLayout("b262,80xtall,0,0,5")).toBeUndefined();
  expect(parseTmuxLayout("b262,eightyx24,0,0,5")).toBeUndefined();
  // An offset may be negative, as tmux writes it with `%d`; a size may not.
  expect(parseTmuxLayout("b262,80x-24,0,0,5")).toBeUndefined();
  expect(parseTmuxLayout("b262,80x24,-,0,5")).toBeUndefined();
});

test("the empty string is no layout", () => {
  expect(parseTmuxLayout("")).toBeUndefined();
});

test("a string without tmux's checksum, or with anything after the root cell, is no layout", () => {
  expect(parseTmuxLayout("80x24,0,0,5")).toBeUndefined();
  expect(parseTmuxLayout("b262,80x24,0,0,5]")).toBeUndefined();
});

test("a layout that names one pane twice is no layout, in either form", () => {
  // The unchanged windows read, so a broken fixture cannot pass the refusals below.
  expect(parsed(THREE_PANES_SMALL).panes).toHaveLength(3);
  expect(parsed(JSON_THREE_PANES_SMALL).panes).toHaveLength(3);
  expect(parseTmuxLayout(THREE_PANES_SMALL.replace("41,13,2}", "41,13,1}"))).toBeUndefined();
  expect(parseTmuxLayout(JSON_THREE_PANES_SMALL.replace('"I":"%2"', '"I":"%1"'))).toBeUndefined();
});

/** A layout of `levels` splits, each holding the next, with one pane at the bottom: eleven
 *  characters a level, which is how cheaply a sender can buy a level of recursion in the
 *  parser. */
function nested(levels: number): string {
  return `b262,${"80x24,0,0[".repeat(levels)}80x24,0,0,0${"]".repeat(levels)}`;
}

/** The most levels `nest` can hold within the cap. */
function deepestInside(nest: (levels: number) => string): number {
  const level = nest(1).length - nest(0).length;
  return Math.floor((TMUX_LAYOUT_MAX - nest(0).length) / level);
}

test("a layout past the cap the sender's own client holds to is no layout, however it nests", () => {
  const levels = deepestInside(nested);
  const inside = nested(levels);
  const past = nested(levels + 1);
  expect(inside.length).toBeLessThanOrEqual(TMUX_LAYOUT_MAX);
  expect(past.length).toBeGreaterThan(TMUX_LAYOUT_MAX);

  // The two strings differ by one split, so what refuses the longer one is its length and not
  // its shape: a page draws a window from whatever a session registered, and nothing between
  // the sender and the page applies this cap on the parser's behalf. The deepest string the cap
  // lets through reads, so its recursion fits the stack.
  expect(parseTmuxLayout(inside)?.panes).toEqual([
    { height: 24, kind: "pane", paneId: "%0", width: 80, x: 0, y: 0 },
  ]);
  expect(parseTmuxLayout(past)).toBeUndefined();
});

test("tmux 3.8's JSON form of a window reads as the same boxes as version 1's form of it", () => {
  for (const [json, string] of [
    [JSON_THREE_PANES_SMALL, THREE_PANES_SMALL],
    [JSON_THREE_PANES_LARGE, THREE_PANES_LARGE],
  ]) {
    const layout = parseTmuxLayout(json);
    expect(layout).toBeDefined();
    // The JSON form also states each pane's index, which version 1 does not.
    expect(layout?.panes.map((pane) => pane.index)).toEqual([0, 1, 2]);
    expect(withoutIndexes(layout)).toEqual(withoutIndexes(parseTmuxLayout(string)));
  }
});

test("a window of one pane in the JSON form reads as that pane, its id written with its `%`", () => {
  const pane: TmuxLayoutPane = {
    height: 24,
    index: 0,
    kind: "pane",
    paneId: "%0",
    width: 80,
    x: 0,
    y: 0,
  };

  expect(parseTmuxLayout(JSON_ONE_PANE)).toEqual({
    panes: [pane],
    root: pane,
    window: { height: 24, width: 80 },
  });
});

test("a floating pane reads as a pane at its own box, with its index and its place in the stack", () => {
  const layout = parseTmuxLayout(JSON_FLOATING);

  expect(layout?.panes).toEqual([
    { height: 24, index: 0, kind: "pane", paneId: "%0", width: 80, x: 0, y: 0 },
    { height: 8, index: 1, kind: "pane", paneId: "%1", width: 28, x: 11, y: 6, z: 0 },
  ]);
  // tmux holds a floating pane among the children of a split, beside the tiled cells.
  expect(layout?.root).toMatchObject({ height: 24, into: "rows", kind: "split", width: 80 });
});

for (const [version, recordings] of [
  ["tmux 3.8", TMUX_38],
  ["tmux 3.7c", TMUX_37C],
] as const) {
  test(`${version}: each pane reads at the box, index and stacking place tmux lists it at`, () => {
    for (const recording of recordings) {
      const layout = parsed(recording.layout);
      const read = [...layout.panes]
        .sort((left, right) => left.paneId.localeCompare(right.paneId, "en", { numeric: true }))
        .map((pane) => ({
          box: boxOf(pane),
          // Version 1 states no index: the drawing takes it from the pane's place instead.
          index: pane.index ?? recording.panes.find((one) => one.paneId === pane.paneId)?.index,
          paneId: pane.paneId,
          ...(pane.z === undefined ? {} : { z: pane.z }),
        }));
      expect({ name: recording.name, read }).toEqual({
        name: recording.name,
        read: [...recording.panes],
      });
      if (version === "tmux 3.8") {
        expect(layout.panes.every((pane) => pane.index !== undefined)).toBe(true);
      } else {
        expect(layout.panes.some((pane) => pane.index !== undefined)).toBe(false);
      }
    }
  });

  test(`${version}: each window draws its panes clipped to the window, in tmux's paint order`, () => {
    for (const recording of recordings) {
      const layout = parsed(recording.layout);
      const drawing = drawWindow(layout, recording.window);
      const expected = recording.paintOrder.flatMap((paneId) => {
        const listed = recording.panes.find((pane) => pane.paneId === paneId);
        const box = listed === undefined ? undefined : inside(listed.box, recording.window);
        return box === undefined ? [] : [{ box, paneId }];
      });
      const offWindow = recording.panes
        .filter((pane) => inside(pane.box, recording.window) === undefined)
        .map((pane) => pane.paneId);
      expect({
        name: recording.name,
        outside: drawing?.outside.map((pane) => pane.paneId),
        shown: drawing?.shown.map(({ box, cell }) => ({ box, paneId: cell.paneId })),
        window: drawing?.window,
      }).toEqual({
        name: recording.name,
        outside: offWindow,
        shown: expected,
        window: recording.window,
      });
    }
  });
}

test("a floating box past the window's edges is drawn as the part of it inside the window", () => {
  const pastRight = recordingNamed(TMUX_38, "a floating pane resized past the right edge");
  const leftAbove = recordingNamed(TMUX_38, "a floating pane moved left of and above the window");
  const offAbove = recordingNamed(
    TMUX_38,
    "a floating pane moved wholly above and left of the window"
  );
  const offBelow = recordingNamed(
    TMUX_38,
    "a floating pane moved wholly past the bottom right corner"
  );
  // 58 wide from column 61 of 80 leaves 19; 8 high from row 16 of 24 is all of it.
  expect(drawWindow(parsed(pastRight.layout), undefined)?.shown.at(-1)?.box).toEqual(
    at(61, 16, 19, 8)
  );
  // Case F: from -4, -2, so 4 columns and 2 rows are cut from the left and the top.
  expect(drawWindow(parsed(leftAbove.layout), undefined)?.shown.at(-1)?.box).toEqual(
    at(0, 0, 44, 16)
  );
  // Wholly outside, on either side: tmux shows none of it, and it is named apart.
  for (const recording of [offAbove, offBelow]) {
    const drawing = drawWindow(parsed(recording.layout), undefined);
    expect(drawing?.shown.map(({ cell }) => cell.paneId)).toEqual(["%10"]);
    expect(drawing?.outside.map((pane) => pane.paneId)).toEqual(["%11"]);
  }
});

test("a window whose one pane floats states no size, and is drawn at the size its session reported", () => {
  for (const recording of [TMUX_38, TMUX_37C].map((list) =>
    recordingNamed(list, "a window whose one pane floats")
  )) {
    const layout = parsed(recording.layout);
    expect(layout.window).toBeUndefined();
    expect(drawWindow(layout, undefined)).toBeUndefined();
    expect(drawWindow(layout, { height: 0, width: 0 })).toBeUndefined();
    // At the window's own size the pane is at its own place in it, not stretched over it.
    const drawing = drawWindow(layout, WINDOW_80X24);
    expect(drawing?.window).toEqual(WINDOW_80X24);
    expect(drawing?.shown.map(({ box }) => box)).toEqual([recording.panes[0].box]);
  }
  // A window whose root is tiled is drawn at the root's size whatever its session reported.
  expect(drawWindow(parsed(JSON_FLOATING), { height: 50, width: 200 })?.window).toEqual(
    WINDOW_80X24
  );
});

test("version 1's floating group must name, once each and at the same box, panes the tree holds", () => {
  const twoFloating = recordingNamed(TMUX_37C, "two floating panes made from the same pane").layout;
  // The unchanged recording reads, so a broken fixture cannot pass the refusals below.
  expect(parsed(twoFloating).panes).toHaveLength(4);
  for (const broken of [
    // A pane the tree does not hold.
    twoFloating.replace("<30x10,20,10,3,", "<30x10,20,10,9,"),
    // A pane named twice in the group.
    twoFloating.replace("<30x10,20,10,3,30x10,10,8,2>", "<30x10,20,10,3,30x10,20,10,3>"),
    // A pane at another box than the tree's.
    twoFloating.replace("<30x10,20,10,3,", "<31x10,20,10,3,"),
    // A split in the group, an empty group, an unclosed one, and anything after it.
    twoFloating.replace("<30x10,20,10,3,", "<30x10,20,10[30x10,20,10,3],"),
    twoFloating.replace(/<.*>$/, "<>"),
    twoFloating.slice(0, -1),
    `${twoFloating}>`,
  ]) {
    expect(broken).not.toBe(twoFloating);
    expect(parseTmuxLayout(broken)).toBeUndefined();
  }
});

test("version 1's offsets read with their sign, as tmux writes them", () => {
  // Hand-made from the recorded 3.7c window: tmux writes an offset with `%d`
  // (`layout-custom.c`), and a floating pane the mouse drags off the window's top left has a
  // negative one; tmux 3.7c moves a floating pane by no command, so none was recorded.
  const dragged = "0000,80x24,0,0[80x24,0,0,0,30x10,-6,-3,1]<30x10,-6,-3,1>";
  const layout = parsed(dragged);
  expect(layout.panes.at(-1)).toEqual({
    height: 10,
    kind: "pane",
    paneId: "%1",
    width: 30,
    x: -6,
    y: -3,
    z: 0,
  });
  expect(drawWindow(layout, undefined)?.shown.at(-1)?.box).toEqual(at(0, 0, 24, 7));
});

/** `JSON_ONE_PANE`'s cell with its `member` set to `value`; undefined leaves the member out, as
 *  `JSON.stringify` writes no member whose value is undefined. */
function onePaneWith(member: string, value: unknown): string {
  const cell = { a: true, h: 24, i: 0, I: "%0", t: "p", w: 80, x: 0, y: 0 };
  return JSON.stringify({ L: { ...cell, [member]: value }, V: 2 });
}

test("a JSON form of another version, or that is not whole JSON, is no layout", () => {
  expect(parseTmuxLayout(JSON_ONE_PANE.replace('"V":2', '"V":3'))).toBeUndefined();
  expect(parseTmuxLayout(JSON_ONE_PANE.replace('"V":2,', ""))).toBeUndefined();
  expect(parseTmuxLayout(JSON_ONE_PANE.slice(0, -1))).toBeUndefined();
  expect(parseTmuxLayout(`${JSON_ONE_PANE}}`)).toBeUndefined();
  expect(parseTmuxLayout('{"V":2}')).toBeUndefined();
  expect(parseTmuxLayout(`[${JSON_ONE_PANE}]`)).toBeUndefined();
});

test("a JSON cell that is neither a pane nor a split, or misses what its type needs, is no layout", () => {
  // The cell every case below changes one member of reads as a pane when nothing is changed.
  expect(parseTmuxLayout(onePaneWith("i", 0))?.panes).toHaveLength(1);
  expect(parseTmuxLayout(onePaneWith("t", "x"))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("t", undefined))).toBeUndefined();
  // A pane names its id, written with its `%`.
  expect(parseTmuxLayout(onePaneWith("I", undefined))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("I", "0"))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("I", "%zero"))).toBeUndefined();
  // A pane holds no cells, and a split holds at least one.
  expect(parseTmuxLayout(onePaneWith("c", []))).toBeUndefined();
  const emptySplit = JSON_THREE_PANES_SMALL.replace(/"c":\[.*\]\}\}$/, '"c":[]}}');
  expect(emptySplit).toEndWith('"y":0,"c":[]}}');
  expect(parseTmuxLayout(emptySplit)).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("t", "v"))).toBeUndefined();
  // A pane's index and its place in the stack are counts from 0, and only a pane floats.
  expect(parseTmuxLayout(onePaneWith("i", -1))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("i", "0"))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("z", 0))?.panes[0]?.z).toBe(0);
  expect(parseTmuxLayout(onePaneWith("z", 0.5))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("z", -1))).toBeUndefined();
  expect(
    parseTmuxLayout(JSON_THREE_PANES_SMALL.replace('"t":"h","w":80', '"t":"h","z":0,"w":80'))
  ).toBeUndefined();
});

test("a JSON split holding one cell it cannot read is no layout, however deep the cell sits", () => {
  // The unchanged layout reads, so a broken fixture cannot pass the refusals below.
  expect(parsed(JSON_THREE_PANES_SMALL).panes.map((pane) => pane.paneId)).toEqual([
    "%0",
    "%1",
    "%2",
  ]);
  // Each break is in a cell under the root's second child, a split itself: the window is
  // refused whole, never read with the pane it could not read left out.
  for (const broken of [
    JSON_THREE_PANES_SMALL.replace('"I":"%2"', '"I":"2"'),
    JSON_THREE_PANES_SMALL.replace('"w":39,', ""),
    JSON_THREE_PANES_SMALL.replace('{"t":"p","w":40', '{"t":"q","w":40'),
    JSON_THREE_PANES_SMALL.replace('"i":1,', '"i":"1",'),
  ]) {
    expect(broken).not.toBe(JSON_THREE_PANES_SMALL);
    expect(parseTmuxLayout(broken)).toBeUndefined();
  }
});

test("a JSON size or offset that is not a whole number, or a size below zero, is no layout", () => {
  // Whole numbers read, a size of zero and an offset below zero included.
  expect(parseTmuxLayout(onePaneWith("w", 0))?.panes[0]?.width).toBe(0);
  expect(parseTmuxLayout(onePaneWith("x", -1))?.panes[0]?.x).toBe(-1);
  expect(parseTmuxLayout(onePaneWith("w", "80"))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("w", 80.5))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("w", -80))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("h", undefined))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("x", "0"))).toBeUndefined();
  expect(parseTmuxLayout(onePaneWith("y", 0.5))).toBeUndefined();
});

/** The JSON form of `levels` splits, each holding the next, with one pane at the bottom. */
function nestedJson(levels: number): string {
  const split = '{"t":"v","w":80,"h":24,"x":0,"y":0,"c":[';
  const pane = '{"t":"p","w":80,"h":24,"x":0,"y":0,"i":0,"I":"%0"}';
  return `{"V":2,"L":${split.repeat(levels)}${pane}${"]}".repeat(levels)}}`;
}

test("a JSON form past the same cap is no layout, however it nests", () => {
  const levels = deepestInside(nestedJson);
  const inside = nestedJson(levels);
  const past = nestedJson(levels + 1);
  expect(inside.length).toBeLessThanOrEqual(TMUX_LAYOUT_MAX);
  expect(past.length).toBeGreaterThan(TMUX_LAYOUT_MAX);

  expect(parseTmuxLayout(inside)?.panes).toEqual([
    { height: 24, index: 0, kind: "pane", paneId: "%0", width: 80, x: 0, y: 0 },
  ]);
  expect(parseTmuxLayout(past)).toBeUndefined();
});

test("the cap holds a window of 60 panes in tmux 3.8's JSON form, as it held one in version 1", () => {
  // Recorded from tmux 3.8: 60 panes in a 400x200 window, `select-layout tiled`, 3,756
  // characters. Its version 1 form is 1,071 characters, inside the cap version 1 always had.
  const sixty = recorded.tiledSixtyPanes400x200;
  expect(sixty.length).toBe(3756);
  const layout = parsed(sixty);
  expect(layout.panes).toHaveLength(60);
  expect(new Set(layout.panes.map((pane) => pane.index))).toEqual(
    new Set(Array.from({ length: 60 }, (_, index) => index))
  );
  expect(drawWindow(layout, undefined)?.shown).toHaveLength(60);
});
