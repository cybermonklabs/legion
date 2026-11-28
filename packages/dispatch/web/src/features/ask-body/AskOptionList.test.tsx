import { beforeAll, expect, test } from "bun:test";

import { renderWithQueries as render } from "../../__tests__/query-provider";
import type { AskOption } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { AskOptionList } from "./AskOptionList";

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
