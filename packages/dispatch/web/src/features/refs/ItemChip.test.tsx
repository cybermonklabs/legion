import { afterEach, expect, spyOn, test } from "bun:test";
import { cleanup, screen, waitFor, within } from "@testing-library/react";

import { renderWithQueries } from "../../__tests__/query-provider";
import { ApiError, api } from "../../api/client";
import { pageStatusProgressText } from "../../theme/classes";
import { ItemChip } from "./ItemChip";

// An issue the command bar names (design v3, state 4) is a chip: its key, a dot in its status's
// colour and its title on one line, read from the issue's summary; the key alone until that read
// answers, and for an issue the reader cannot read. `MarkdownBody.test.tsx` draws one in an
// answer's body.

afterEach(cleanup);

test("a chip reads its key, a dot in its status's colour, and its title from the issue's summary", async () => {
  const summary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "DEMO-7",
    project: "DEMO",
    status: "in_progress",
    title: "Map the lunar craters",
  });
  try {
    renderWithQueries(<ItemChip issue="DEMO-7" />);
    const chip = screen.getByTestId("item-chip");
    expect(chip.textContent).toBe("DEMO-7");
    await waitFor(() => expect(chip.textContent).toBe("DEMO-7In progressMap the lunar craters"));
    const dot = within(chip).getByTestId("item-chip-status");
    expect(dot.className).toContain(pageStatusProgressText);
    expect(dot).toHaveAttribute("title", "In progress");
    expect(dot).toHaveAttribute("aria-hidden", "true");
    expect(summary).toHaveBeenCalledWith("DEMO-7");
  } finally {
    summary.mockRestore();
  }
});

// A chip is one box on one line (design v3, state 4): it never breaks inside itself, so it goes to
// the next line whole, and a title longer than the line is cut with "…", the whole title on hover.
// As its own box it takes no underline from the link it sits in.
test("a chip is one box that never wraps: a long title is cut, and shows whole on hover", async () => {
  const summary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "DEMO-15",
    project: "DEMO",
    status: "in_progress",
    title: "Board: drag handle hidden on narrow screens",
  });
  try {
    renderWithQueries(<ItemChip issue="DEMO-15" />);
    const chip = screen.getByTestId("item-chip");
    const title = await within(chip).findByTestId("item-chip-title");
    const classes = chip.className.split(" ");
    expect(classes).toContain("inline-flex");
    expect(classes).toContain("whitespace-nowrap");
    expect(classes).toContain("max-w-full");
    expect(classes).toContain("no-underline");
    expect(classes).not.toContain("box-decoration-clone");
    expect(title.className.split(" ")).toContain("truncate");
    expect(title.textContent).toBe("Board: drag handle hidden on narrow screens");
    expect(chip).toHaveAttribute("title", "Board: drag handle hidden on narrow screens");
  } finally {
    summary.mockRestore();
  }
});

// A chip drawn for a time link's words (`MarkdownBody`) shows the time on hover, not the title.
test("a chip given what it shows on hover shows that in place of its title", async () => {
  const summary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "DEMO-17",
    project: "DEMO",
    status: "backlog",
    title: "PDF export: fonts missing on mobile",
  });
  try {
    renderWithQueries(<ItemChip hover="10/10/2026, 9:00:00 AM" issue="DEMO-17" />);
    const chip = screen.getByTestId("item-chip");
    await waitFor(() =>
      expect(chip.textContent).toBe("DEMO-17BacklogPDF export: fonts missing on mobile")
    );
    expect(chip).toHaveAttribute("title", "10/10/2026, 9:00:00 AM");
  } finally {
    summary.mockRestore();
  }
});

test("a chip of an issue the reader cannot read is its key alone", async () => {
  const summary = spyOn(api, "getIssueSummary").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no such issue" })
  );
  try {
    renderWithQueries(<ItemChip issue="DEMO-404" />);
    await waitFor(() => expect(summary).toHaveBeenCalled());
    expect(screen.getByTestId("item-chip").textContent).toBe("DEMO-404");
    expect(screen.queryByTestId("item-chip-status")).toBeNull();
  } finally {
    summary.mockRestore();
  }
});
