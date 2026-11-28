import { expect, test } from "bun:test";

import { type PanelEvent, type PanelPlace, type PanelTransition, panelNext } from "./panel-states";

// The state table of where the command bar's conversation is (plan.md t15, spec R1): each place -
// closed, over the page, at the side - under each event - Ctrl-K, Ctrl-Shift-K, the button "Keep
// at the side", Esc in a full box, Esc in an empty box while a run works (and while none does), a
// navigation, a click on the page. Each row is a test: the next place, whether the run stops, and
// whether the clicked item becomes the next request's context.

const events: Readonly<Record<string, PanelEvent>> = {
  "Ctrl-K": { kind: "mod_k" },
  "Ctrl-Shift-K": { kind: "mod_shift_k" },
  "a click on the page": { kind: "page_click" },
  "a navigation": { kind: "navigation" },
  "Esc in a full box": { kind: "esc_full" },
  "Esc in an empty box while a run works": { kind: "esc_empty", working: true },
  "Esc in an empty box while no run works": { kind: "esc_empty", working: false },
  "the button Keep at the side": { kind: "keep_at_side" },
};

type Row = readonly [PanelPlace, keyof typeof events, PanelTransition];

const rows: readonly Row[] = [
  ["closed", "Ctrl-K", { place: "over" }],
  ["closed", "Ctrl-Shift-K", { place: "side" }],
  ["closed", "the button Keep at the side", { place: "closed" }],
  ["closed", "Esc in a full box", { place: "closed" }],
  ["closed", "Esc in an empty box while a run works", { place: "closed" }],
  ["closed", "a navigation", { place: "closed" }],
  ["closed", "a click on the page", { place: "closed" }],
  ["over", "Ctrl-K", { place: "closed" }],
  ["over", "Ctrl-Shift-K", { place: "side" }],
  ["over", "the button Keep at the side", { place: "side" }],
  ["over", "Esc in a full box", { place: "closed" }],
  ["over", "Esc in an empty box while a run works", { place: "over", stopRun: true }],
  ["over", "Esc in an empty box while no run works", { place: "closed" }],
  ["over", "a navigation", { place: "closed" }],
  ["over", "a click on the page", { place: "closed" }],
  ["side", "Ctrl-K", { place: "over" }],
  ["side", "Ctrl-Shift-K", { place: "closed" }],
  ["side", "the button Keep at the side", { place: "side" }],
  ["side", "Esc in a full box", { place: "side" }],
  ["side", "Esc in an empty box while a run works", { place: "side", stopRun: true }],
  ["side", "Esc in an empty box while no run works", { place: "side" }],
  ["side", "a navigation", { place: "side" }],
  ["side", "a click on the page", { context: true, place: "side" }],
];

for (const [place, event, next] of rows) {
  test(`the conversation ${place}, on ${event}, is ${next.place}${next.stopRun ? " and its run stops" : ""}${next.context ? " and the click names the next request's context" : ""}`, () => {
    expect(panelNext(place, events[event] as PanelEvent)).toEqual(next);
  });
}

// Firefox keeps Ctrl-Shift-K for its Web Console: where the page never gets that key, Ctrl-K
// pressed while the bar is open keeps the conversation at the side instead of closing it.
test("where the page never gets Ctrl-Shift-K, Ctrl-K in the open bar keeps it at the side", () => {
  expect(panelNext("over", { kind: "mod_k" }, { sideByModK: true })).toEqual({ place: "side" });
  expect(panelNext("closed", { kind: "mod_k" }, { sideByModK: true })).toEqual({ place: "over" });
});
