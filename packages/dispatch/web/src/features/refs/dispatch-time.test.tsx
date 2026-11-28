import { afterEach, expect, spyOn, test } from "bun:test";
import { cleanup, render, waitFor } from "@testing-library/react";

import { renderWithQueries } from "../../__tests__/query-provider";
import { api } from "../../api/client";
import { TimedWords, timeWords } from "./dispatch-time";
import { instantOf, isTimeAddress } from "./time-link";

// A time the command bar's agent writes is a Markdown link to its instant under `dispatch-time:`.
// Words the page draws as plain text - a plan's summary, the line beside the agent's calls - draw
// each such link as a `<time>` of the instant showing its words, the full date on hover; an
// address that names no instant draws the words alone; and Copy takes the words a reader sees.

afterEach(cleanup);

const WRITTEN =
  "Closed [5 days ago](dispatch-time:2026-11-18T11:35:00Z), reopened [later](dispatch-time:yesterday).";

test("a time's address names its instant only where it is one RFC 3339 writes", () => {
  expect(isTimeAddress("dispatch-time:2026-11-18T11:35:00Z")).toBe(true);
  expect(isTimeAddress(" Dispatch-Time:2026-11-18T11:35Z")).toBe(true);
  expect(isTimeAddress("https://example.com")).toBe(false);
  expect(isTimeAddress(undefined)).toBe(false);
  expect(instantOf("dispatch-time:2026-11-18T11:35:00Z")).toBe("2026-11-18T11:35:00Z");
  expect(instantOf("dispatch-time:2026-11-18T11:35:00.250+01:00")).toBe(
    "2026-11-18T11:35:00.250+01:00"
  );
  expect(instantOf("dispatch-time:yesterday")).toBeUndefined();
  expect(instantOf("dispatch-time:2026-11-18")).toBeUndefined();
  expect(instantOf("dispatch-time:2026-13-45T99:99:00Z")).toBeUndefined();
});

test("Copy takes each time as the words it shows, never its address", () => {
  expect(timeWords(WRITTEN)).toBe("Closed 5 days ago, reopened later.");
  expect(timeWords("No time here: [DEMO-7](dispatch://DEMO-7).")).toBe(
    "No time here: [DEMO-7](dispatch://DEMO-7)."
  );
});

test("plain words draw each time link as a time with the full date on hover, and a bad one as its words", () => {
  const view = render(
    <p>
      <TimedWords text={WRITTEN} />
    </p>
  );
  const times = view.container.querySelectorAll("time");
  expect(times).toHaveLength(1);
  expect(times[0]?.textContent).toBe("5 days ago");
  expect(times[0]?.getAttribute("datetime")).toBe("2026-11-18T11:35:00Z");
  expect(times[0]?.getAttribute("title")).toBe(new Date("2026-11-18T11:35:00Z").toLocaleString());
  expect(view.container.textContent).toBe("Closed 5 days ago, reopened later.");
});

test("words with no time link draw as they are", () => {
  const view = render(
    <p>
      <TimedWords text="Set DEMO-10 to P2." />
    </p>
  );
  expect(view.container.textContent).toBe("Set DEMO-10 to P2.");
  expect(view.container.querySelector("time")).toBeNull();
});

// A time link whose words are an issue's key names that issue, so plain words draw that issue's
// chip there, a link to it, with the time on the chip's hover after its title, as an answer's
// body does (`MarkdownBody`); a time link of any other words draws as its time.
test("plain words draw a time link whose words are an issue's key as that issue's chip, and one of other words as its time", async () => {
  const summary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "DEMO-17",
    project: "DEMO",
    status: "backlog",
    title: "PDF export: fonts missing on mobile",
  });
  const instant = "2026-10-10T09:00:00Z";
  const chipWords = "DEMO-17BacklogPDF export: fonts missing on mobile";
  try {
    const view = renderWithQueries(
      <p>
        <TimedWords
          text={`Last moved: [DEMO-17](dispatch-time:${instant}); reopened [5 days ago](dispatch-time:2026-11-18T11:35:00Z).`}
        />
      </p>
    );
    const chip = await waitFor(() => {
      const drawn = view.getByTestId("item-chip");
      expect(drawn.textContent).toBe(chipWords);
      return drawn;
    });
    expect(view.getAllByTestId("item-chip")).toHaveLength(1);
    expect(chip.closest("a")).toHaveAttribute("href", "/issues/DEMO-17");
    expect(chip.closest("time")).toHaveAttribute("datetime", instant);
    expect(chip).toHaveAttribute(
      "title",
      `PDF export: fonts missing on mobile · ${new Date(instant).toLocaleString()}`
    );
    const times = view.container.querySelectorAll("time");
    expect(times).toHaveLength(2);
    expect(times[1]?.textContent).toBe("5 days ago");
    expect(times[1]?.querySelector("a")).toBeNull();
    expect(view.container.textContent).toBe(`Last moved: ${chipWords}; reopened 5 days ago.`);
  } finally {
    summary.mockRestore();
  }
});
