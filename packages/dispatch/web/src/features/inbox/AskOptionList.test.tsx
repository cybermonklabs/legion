import { beforeAll, expect, test } from "bun:test";
import { screen } from "@testing-library/react";

import { renderWithQueries as render } from "../../__tests__/query-provider";
import type { AskOption } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { AskOptionList } from "./AskOptionList";
import { AskChoiceRow } from "./AskOptionRow";
import { optionKey, ROW_IN_HAND_CLASS } from "./option-digits";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

test("an option label with backticks renders inline code with no wrapping <p> or literal backticks", async () => {
  const options: AskOption[] = [{ label: "Ship it `now`" }];
  const view = render(<AskOptionList options={options} selected={[]} />);

  try {
    const code = await view.findByText("now", { selector: "code" });
    expect(code).toBeTruthy();
    expect(code.closest("p")).toBeNull();
    expect(view.getByRole("listitem").textContent).toBe("Ship it now");
  } finally {
    view.unmount();
  }
});

test("a list-only option label flattens to inline text with no nested list item", async () => {
  const options: AskOption[] = [{ label: "- a" }];
  const view = render(<AskOptionList options={options} selected={[]} />);

  try {
    await view.findByText("a");
    const items = view.getAllByRole("listitem");
    expect(items).toHaveLength(1);
    expect(items[0]?.textContent).toBe("a");
  } finally {
    view.unmount();
  }
});

// The digit is the Inbox's key for the option, drawn for the eye: the option's name is still its
// label, which every screen reader and every test that finds an option by its name reads.
test("a choice row draws its digit beside its text and keeps its name its label", async () => {
  const view = render(
    <AskChoiceRow
      checked={false}
      digit={{ key: "4", whileRowInHand: false }}
      hotkey
      multiple={false}
      name="ask-a"
      onChange={() => {}}
      option={{ label: "Other" }}
    />
  );

  try {
    // "Other" is a Markdown label (`MarkdownBody`), unnamed until the lazy Markdown engine loads.
    const radio = await screen.findByRole("radio", { name: "Other" });
    expect(radio).toHaveAttribute("data-ask-option", "");
    const digit = view.container.querySelector("kbd");
    expect(digit?.textContent).toBe("4");
    expect(digit?.parentElement).toHaveAttribute("aria-hidden", "true");
    // Drawn wherever the card is in hand, not only while its row is.
    expect(digit?.parentElement?.className).not.toContain("invisible");
  } finally {
    view.unmount();
  }
});

test("a choice row of a card in a row draws its digit for the row in hand alone, and none without one", async () => {
  const inRow = render(
    <AskChoiceRow
      checked={false}
      digit={{ key: "1", whileRowInHand: true }}
      hotkey
      multiple
      name="ask-a"
      onChange={() => {}}
      option={{ label: "Ship" }}
    />
  );
  try {
    expect(inRow.container.querySelector("kbd")?.parentElement?.className).toContain(
      ROW_IN_HAND_CLASS
    );
  } finally {
    inRow.unmount();
  }

  const elsewhere = render(
    <AskChoiceRow
      checked={false}
      multiple={false}
      name="ask-a"
      onChange={() => {}}
      option={{ label: "Ship" }}
    />
  );
  try {
    expect(elsewhere.container.querySelector("kbd")).toBeNull();
    // "Ship" is a Markdown label, named once the lazy Markdown engine has loaded.
    expect(await screen.findByRole("radio", { name: "Ship" })).not.toHaveAttribute(
      "data-ask-option"
    );
  } finally {
    elsewhere.unmount();
  }
});

test("the digits run 1 to 9, then 0 for the tenth option, and stop there", () => {
  expect(Array.from({ length: 11 }, (_, index) => optionKey(index))).toEqual([
    "1",
    "2",
    "3",
    "4",
    "5",
    "6",
    "7",
    "8",
    "9",
    "0",
    undefined,
  ]);
});
