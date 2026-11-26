import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ComponentProps, ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { EvalsHeader } from "./EvalsHeader";
import { useEvalsUrlState } from "./evals-url";

afterEach(() => {
  cleanup();
});

/** The address the header wrote, as its query string. */
function Search(): ReactNode {
  return <output data-testid="search">{useLocation().search}</output>;
}

function currentSearch(): URLSearchParams {
  return new URLSearchParams(screen.getByTestId("search").textContent ?? "");
}

/** The header as the tab draws it, over the URL's state and writing it back. */
function Header(props: Omit<ComponentProps<typeof EvalsHeader>, "state" | "onChange">): ReactNode {
  const [state, setState] = useEvalsUrlState();
  return <EvalsHeader {...props} onChange={setState} state={state} />;
}

function renderHeader(
  search = "",
  props: Omit<ComponentProps<typeof EvalsHeader>, "state" | "onChange"> = {}
) {
  render(
    <MemoryRouter initialEntries={[`/projects/ACME/evals${search}`]}>
      <QueryClientProvider client={new QueryClient()}>
        <Header {...props} />
        <Search />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("each control writes the URL: the window, the compare range and the repositories", async () => {
  const user = userEvent.setup();
  renderHeader();
  const header = screen.getByRole("group", { name: "Window and filters" });

  await user.selectOptions(within(header).getByLabelText("Window"), "84d");
  expect(currentSearch().get("window")).toBe("84d");

  await user.selectOptions(within(header).getByLabelText("Compare with"), "off");
  expect(currentSearch().get("compare")).toBe("off");

  await user.selectOptions(within(header).getByLabelText("Compare with"), "custom");
  await user.click(within(header).getByLabelText("Compare from"));
  await user.paste("2026-01-01");
  await user.click(within(header).getByLabelText("Compare to"));
  await user.paste("2026-01-28");
  await user.click(within(header).getAllByRole("button", { name: "Apply" })[0] as HTMLElement);
  expect(currentSearch().get("compare")).toBeNull();
  expect(currentSearch().get("compare_from")).toBe("2026-01-01T00:00:00.000Z");
  expect(currentSearch().get("compare_to")).toBe("2026-01-29T00:00:00.000Z");

  await user.selectOptions(within(header).getByLabelText("Compare with"), "previous");
  expect(currentSearch().has("compare_from")).toBe(false);
  expect(currentSearch().has("compare")).toBe(false);

  await user.type(within(header).getByLabelText("Repository"), "acme/widgets{Enter}");
  await user.type(within(header).getByLabelText("Repository"), "acme/gadgets{Enter}");
  expect(currentSearch().getAll("repo")).toEqual(["acme/widgets", "acme/gadgets"]);
  await user.click(within(header).getByRole("button", { name: "Remove acme/widgets" }));
  expect(currentSearch().getAll("repo")).toEqual(["acme/gadgets"]);
  // The window kept through every other control's write.
  expect(currentSearch().get("window")).toBe("84d");
});

test("the default compare range is the days before the window, which the URL names by naming nothing", () => {
  renderHeader("?window=14d");
  const compare = screen.getByLabelText("Compare with") as HTMLSelectElement;
  expect(compare.value).toBe("previous");
  expect(compare.selectedOptions[0]?.textContent).toBe("The days before the window");
  expect(currentSearch().toString()).toBe("window=14d");
});

test("a malformed compare range in the URL is dropped, leaving the default", () => {
  renderHeader("?compare_from=not-a-date&compare_to=2026-01-01T00%3A00%3A00Z&window=7d");
  expect((screen.getByLabelText("Compare with") as HTMLSelectElement).value).toBe("previous");
  expect(currentSearch().toString()).toBe("window=7d");
});

test("a preset shows the days Dispatch counts, the answer's from and to, and the compare range's", () => {
  renderHeader("?window=28d", {
    compare: { days: 29, from: "2026-09-27T00:00:00Z", to: "2026-10-26T00:00:00Z" },
    window: { days: 29, from: "2026-10-26T00:00:00Z", to: "2026-11-24T00:00:00Z" },
  });
  expect(screen.getByTestId("evals-days").textContent).toBe(
    "Window 2026-10-26 to 2026-11-23 (UTC), 29 days; compared with 2026-09-27 to 2026-10-25 (UTC), 29 days"
  );
  cleanup();

  renderHeader("?window=7d&compare=off", {
    compare: null,
    window: { days: 8, from: "2026-11-16T00:00:00Z", to: "2026-11-24T00:00:00Z" },
  });
  expect(screen.getByTestId("evals-days").textContent).toBe(
    "Window 2026-11-16 to 2026-11-23 (UTC), 8 days; compared with nothing"
  );
});

test("no days are named before an answer lands, and no freshness line where Dispatch sent none", () => {
  renderHeader();
  expect(screen.queryByTestId("evals-days")).toBeNull();
  expect(screen.queryByRole("region", { name: "Source freshness" })).toBeNull();
  cleanup();

  renderHeader("", {
    freshness: {
      freshness: {
        last_error: null,
        last_event_at: "2026-11-23T10:00:00Z",
        last_reconcile_at: "2026-11-23T10:00:00Z",
        unfetchable_count: 0,
      },
      readAtMs: Date.parse("2026-11-23T10:00:00Z"),
    },
  });
  expect(screen.getByRole("region", { name: "Source freshness" })).toBeTruthy();
  // Unmounted in the row: the line ticks every second on a real timer, which must not fire
  // between the row and its afterEach.
  cleanup();
});
