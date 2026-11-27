import { expect, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type KeyboardEvent, type Ref, useRef, useState } from "react";

import { SearchBox } from "./SearchBox";

function Host({
  changes,
  onKeyDown,
  ref,
}: {
  changes?: string[];
  onKeyDown?: (event: KeyboardEvent<HTMLInputElement>) => void;
  ref?: Ref<HTMLInputElement>;
}) {
  const [value, setValue] = useState("");
  return (
    <SearchBox
      label="Search issues"
      onChange={(next) => {
        changes?.push(next);
        setValue(next);
      }}
      onKeyDown={onKeyDown}
      placeholder="Key, title or label"
      ref={ref}
      value={value}
    />
  );
}

test("typing reaches onChange as the box's text, under the label's own accessible name", async () => {
  const user = userEvent.setup();
  const changes: string[] = [];
  const view = render(<Host changes={changes} />);
  try {
    const box = screen.getByRole("searchbox", { name: "Search issues" });
    expect(box).toHaveAttribute("data-control", "search-box");
    await user.type(box, "api");
    expect(changes).toEqual(["a", "ap", "api"]);
    expect(box).toHaveValue("api");
  } finally {
    view.unmount();
  }
});

test("the box takes at most as many characters as the server reads", () => {
  const view = render(<Host />);
  try {
    expect(screen.getByRole("searchbox", { name: "Search issues" })).toHaveAttribute(
      "maxlength",
      String(SEARCH_QUERY_MAX)
    );
  } finally {
    view.unmount();
  }
});

test("a host's maxLength replaces that default", () => {
  const view = render(
    <SearchBox label="Search labels" maxLength={8} onChange={() => undefined} value="" />
  );
  try {
    expect(screen.getByRole("searchbox", { name: "Search labels" })).toHaveAttribute(
      "maxlength",
      "8"
    );
  } finally {
    view.unmount();
  }
});

// The command bar is the one box a reader may overrun: it names the limit under the list
// rather than cutting the words they typed, so it takes a query the server would refuse.
test("a null maxLength caps the box at nothing", () => {
  const view = render(
    <SearchBox label="Search" maxLength={null} onChange={() => undefined} value="" />
  );
  try {
    expect(screen.getByRole("searchbox", { name: "Search" })).not.toHaveAttribute("maxlength");
  } finally {
    view.unmount();
  }
});

// The one box that drives a list of its own: focus stays here while the arrows move the
// highlight, so the box is the combobox and names the highlighted row.
test("a box given a list is that list's combobox, and says which row is highlighted", () => {
  const view = render(
    <SearchBox
      combobox={{ activeId: "search-option-2", controls: "search-results", expanded: true }}
      label="Search"
      onChange={() => undefined}
      value=""
    />
  );
  try {
    const box = screen.getByRole("combobox", { name: "Search" });
    expect(box).toHaveAttribute("aria-controls", "search-results");
    expect(box).toHaveAttribute("aria-expanded", "true");
    expect(box).toHaveAttribute("aria-activedescendant", "search-option-2");
  } finally {
    view.unmount();
  }
});

// Every other page's box claims no list, so a reader meets a plain search field there.
test("a box with no list is a plain search box, claiming no rows", () => {
  const view = render(<Host />);
  try {
    const box = screen.getByRole("searchbox", { name: "Search issues" });
    expect(box).not.toHaveAttribute("aria-controls");
    expect(box).not.toHaveAttribute("aria-expanded");
  } finally {
    view.unmount();
  }
});

test("the ref is the input, so a page's shortcut puts the reader in the box", async () => {
  const user = userEvent.setup();
  function ShortcutHost() {
    const box = useRef<HTMLInputElement>(null);
    return (
      <>
        <Host ref={box} />
        <button onClick={() => box.current?.focus()} type="button">
          Focus search
        </button>
      </>
    );
  }
  const view = render(<ShortcutHost />);
  try {
    await user.click(screen.getByRole("button", { name: "Focus search" }));
    expect(screen.getByRole("searchbox", { name: "Search issues" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("onKeyDown sees Enter, which is how a picker takes the first hit", async () => {
  const user = userEvent.setup();
  const keys: string[] = [];
  const view = render(<Host onKeyDown={(event) => keys.push(event.key)} />);
  try {
    await user.click(screen.getByRole("searchbox", { name: "Search issues" }));
    await user.keyboard("{Enter}");
    expect(keys).toEqual(["Enter"]);
  } finally {
    view.unmount();
  }
});

// The command bar's author filter takes what was typed when the reader walks off the box, so
// a login they finished but never pressed Enter on is still the one they chose.
test("onBlur fires when the reader leaves the box", async () => {
  const user = userEvent.setup();
  let left = 0;
  const view = render(
    <>
      <SearchBox label="Author" onBlur={() => (left += 1)} onChange={() => undefined} value="" />
      <button type="button">Elsewhere</button>
    </>
  );
  try {
    await user.click(screen.getByRole("searchbox", { name: "Author" }));
    expect(left).toBe(0);
    await user.click(screen.getByRole("button", { name: "Elsewhere" }));
    expect(left).toBe(1);
  } finally {
    view.unmount();
  }
});

test("a host's test id rides along, so a check that names one keeps it", () => {
  const view = render(
    <SearchBox
      data-testid="agents-filter-search"
      label="Search agents"
      onChange={() => undefined}
      value=""
    />
  );
  try {
    expect(screen.getByTestId("agents-filter-search")).toHaveAttribute(
      "data-control",
      "search-box"
    );
  } finally {
    view.unmount();
  }
});
