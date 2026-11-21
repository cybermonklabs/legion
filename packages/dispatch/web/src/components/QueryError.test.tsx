import { expect, mock, test } from "bun:test";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { runFocusFixup } from "../__tests__/focus-fixup";
import { QueryError } from "./QueryError";

// A keyboard reader who presses Retry keeps their place while the retry is out, so their next key
// reaches Retry rather than the page's body. While it is out Retry says it cannot be pressed, and
// a press of it does nothing; once it ends, a press retries again.
test("a keyboard press of Retry keeps the focus while the retry is out, and Enter retries once it ends", async () => {
  const user = userEvent.setup();
  const onRetry = mock(() => {});
  const failed = (retrying: boolean) => (
    <QueryError message="Couldn't load." onRetry={onRetry} retrying={retrying} />
  );
  const view = render(failed(false));
  const stopFocusFixup = runFocusFixup();
  try {
    const retry = screen.getByRole("button", { name: "Retry" });
    act(() => retry.focus());
    await user.keyboard(" ");
    expect(onRetry).toHaveBeenCalledTimes(1);

    // The retry is out: Retry says it cannot be pressed, either way a control can say so, and
    // keeps the focus.
    view.rerender(failed(true));
    await waitFor(() => expect(retry.matches(":disabled, [aria-disabled='true']")).toBe(true));
    expect(retry).toHaveFocus();
    await user.keyboard(" ");
    expect(onRetry).toHaveBeenCalledTimes(1);

    view.rerender(failed(false));
    expect(retry).toHaveFocus();
    expect(retry).not.toHaveAttribute("aria-disabled");
    await user.keyboard("{Enter}");
    expect(onRetry).toHaveBeenCalledTimes(2);
  } finally {
    stopFocusFixup();
    view.unmount();
  }
});
