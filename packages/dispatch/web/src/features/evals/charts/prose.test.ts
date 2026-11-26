import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/prose-readings.json";
import type { DeclaredInput, DeclaredSpec } from "./kind";
import { PARAGRAPHS, proseKind } from "./prose";
import { readingsOf } from "./readings";

// The prose kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): a text section whose
// paragraphs are its model's rows in the spec's order, drawn once in body text whether or not the
// compare range is shown, reading none of the eval's readings (`__fixtures__/prose-readings.json`),
// and its table holding each paragraph and no number.

const LEAST_TIME: DeclaredSpec = {
  approved: "dispatch://DEMO-1/ask/1",
  id: "least-time",
  kind: "prose",
  paragraphs: [
    "For each task it is one compile and kernel check of the agent's own files, alone.",
    "Grading waits for nothing else.",
    "Grading waits for nothing else.",
  ],
  title: "The least time grading could take",
};

function input(compare: EvalRange | null = fixture.compare): DeclaredInput {
  return {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "rocq-wait",
    window: fixture.window,
  };
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("a prose section reads no series: its paragraphs are its rows, in order, with no number", () => {
  expect(proseKind.reads(LEAST_TIME)).toEqual([]);
  const drawn = proseKind.model(LEAST_TIME, input());
  expect([drawn.name, drawn.kind, drawn.title, drawn.comparable]).toEqual([
    "rocq-wait/least-time",
    "prose",
    "The least time grading could take",
    true,
  ]);
  expect(drawn.compareRange).toEqual(fixture.compare);
  expect(drawn.series.map((series) => [series.name, series.statistics])).toEqual([
    [PARAGRAPHS, [{ id: "text", label: "Text" }]],
  ]);
  // A paragraph the spec repeats is a row of its own.
  expect(
    drawn.series[0]?.points.map((point) => [
      point.x,
      point.label,
      point.values.text?.label,
      point.values.text?.state,
      point.values.text?.value,
      point.values.text?.note,
    ])
  ).toEqual([
    [
      "1",
      "1",
      "For each task it is one compile and kernel check of the agent's own files, alone.",
      "plain",
      null,
      null,
    ],
    ["2", "2", "Grading waits for nothing else.", "plain", null, null],
    ["3", "3", "Grading waits for nothing else.", "plain", null, null],
  ]);
  // The compare range holds nothing to compare: no compare value and no change, with it or not.
  for (const compare of [fixture.compare, null]) {
    const points = proseKind.model(LEAST_TIME, input(compare)).series[0]?.points ?? [];
    expect(points.every((point) => point.compare === null && point.change === null)).toBe(true);
  }
});

test("its paragraphs draw once in body text, the compare range shown or not, and the table holds each and no number", async () => {
  const user = userEvent.setup();
  render(
    createElement(EvalChart, {
      model: proseKind.model(LEAST_TIME, input()),
      renderer: proseKind.renderer,
    })
  );
  const figure = screen.getByRole("figure", { name: "The least time grading could take" });
  const paragraphs = () =>
    [...figure.querySelectorAll('[data-part="prose"] p')].map((p) => p.textContent);
  const texts = [
    "For each task it is one compile and kernel check of the agent's own files, alone.",
    "Grading waits for nothing else.",
    "Grading waits for nothing else.",
  ];
  const compare = within(figure).getByRole("checkbox", { name: "Compare" });
  expect(compare).toBeChecked();
  expect(paragraphs()).toEqual(texts);
  await user.click(compare);
  expect(paragraphs()).toEqual(texts);
  await user.click(compare);
  // No reading of the eval draws: prose holds no number.
  expect(figure.querySelectorAll("[data-value]")).toHaveLength(0);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const table = within(figure).getByRole("table");
  expect(
    [...table.querySelectorAll('[data-statistic="text"][data-range="window"]')].map(
      (cell) => cell.textContent
    )
  ).toEqual(texts);
  expect(table.querySelectorAll("[data-value]")).toHaveLength(0);
});
