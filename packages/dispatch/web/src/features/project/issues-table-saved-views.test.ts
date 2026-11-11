import { expect, test } from "bun:test";

import {
  activeSavedView,
  deleteView,
  readSavedViews,
  type SavedView,
  saveView,
  writeSavedViews,
} from "./issues-table-saved-views";

const triage: SavedView = { name: "Triage", query: "view=triage&status=triage" };

test("corrupt saved-view storage reads as no views", () => {
  expect(readSavedViews("not JSON")).toEqual({});
  expect(readSavedViews('{"ACME":"not an array"}')).toEqual({});
});

test("saved views serialize by project", () => {
  const views = { ACME: [triage] };

  expect(readSavedViews(writeSavedViews(views))).toEqual(views);
});

test("saving a view with an existing name replaces it", () => {
  expect(
    saveView([triage, { name: "Queue", query: "sort=queue" }], {
      name: "Triage",
      query: "view=triage&sort=-created",
    })
  ).toEqual([
    { name: "Triage", query: "view=triage&sort=-created" },
    { name: "Queue", query: "sort=queue" },
  ]);
});

test("deleting a saved view leaves the other views", () => {
  expect(deleteView([triage, { name: "Queue", query: "sort=queue" }], "Triage")).toEqual([
    { name: "Queue", query: "sort=queue" },
  ]);
});

test("the active saved view ignores the page parameter", () => {
  expect(
    activeSavedView([triage], new URLSearchParams("view=triage&status=triage&page=2"))
  ).toEqual(triage);
});
