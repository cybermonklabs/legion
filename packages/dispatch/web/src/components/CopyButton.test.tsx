import { expect, jest, spyOn, test } from "bun:test";
import { render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock } from "../__tests__/fake-clock";
import { userOnFakeClock } from "../__tests__/user";
import { CopyButton } from "./CopyButton";

function withClipboard(
  clipboard: { writeText: (text: string) => Promise<void> } | undefined,
  run: () => Promise<void>
): Promise<void> {
  const original = navigator.clipboard;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: clipboard });
  return run().finally(() => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: original });
  });
}

test("CopyButton writes its value, confirms briefly, and returns to idle", async () => {
  // The confirmation clears itself on the product's own 1.5 s timer (`CopyButton.tsx`), which the
  // wait below crosses on the fake clock rather than in real time.
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  await withClipboard({ writeText }, async () => {
    const view = render(<CopyButton value="LEGION-2" what="issue key" />);
    try {
      const button = view.getByRole("button", { name: "Copy issue key LEGION-2" });
      expect(button).toHaveAttribute("title", "Copy issue key LEGION-2");
      await user.click(button);
      await waitFor(() => expect(writeText).toHaveBeenCalledWith("LEGION-2"));
      expect(await view.findByText("Copied", { exact: true })).toBeTruthy();
      await waitFor(() =>
        expect(view.queryByText("Copied", { exact: true })).not.toBeInTheDocument()
      );
    } finally {
      realClock();
      view.unmount();
    }
  });
});

test("CopyButton reports a failure when neither clipboard path can copy", async () => {
  const user = userEvent.setup();
  const originalExecCommand = document.execCommand;
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: (_command: string) => false,
  });
  try {
    await withClipboard(undefined, async () => {
      const view = render(
        <CopyButton value="dev:4.7" what="tmux target">
          dev:4.7
        </CopyButton>
      );
      try {
        await user.click(view.getByRole("button", { name: "Copy tmux target dev:4.7" }));
        expect(
          await view.findByText("Copy failed - select the text", { exact: true })
        ).toBeTruthy();
        expect(view.getByText("dev:4.7", { exact: true })).toBeTruthy();
      } finally {
        view.unmount();
      }

      // A labelled or icon-only button does not show the value, so the hint prints it.
      const labelled = render(
        <CopyButton value="planner-session" what="session ID">
          ID
        </CopyButton>
      );
      try {
        await user.click(labelled.getByRole("button", { name: "Copy session ID planner-session" }));
        const shownValue = await labelled.findByText("planner-session", { selector: "code" });
        expect(shownValue.parentElement?.textContent).toBe("Copy failed - planner-session");
        expect(
          labelled.queryByText("Copy failed - select the text", { exact: true })
        ).not.toBeInTheDocument();
      } finally {
        labelled.unmount();
      }
    });
  } finally {
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
  }
});
