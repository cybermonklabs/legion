import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the Enter that ends an IME composition is a key marked as the input method's, which user-event has no form for.
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { INPUT_METHOD_MARKS } from "../../__tests__/input-method-marks";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { NameVersionDialog } from "./NameVersionDialog";

test("renders nothing when closed", () => {
  const view = render(
    <NameVersionDialog
      error={null}
      onClose={() => {}}
      onSave={() => {}}
      open={false}
      saving={false}
    />
  );
  try {
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});
test("Save is disabled until the input is non-empty", async () => {
  const user = userEvent.setup();
  const view = render(
    <NameVersionDialog error={null} onClose={() => {}} onSave={() => {}} open saving={false} />
  );
  try {
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    const input = screen.getByLabelText("What changed?");
    expect(save.disabled).toBe(true);

    await user.click(input);
    await user.paste("   ");
    expect(save.disabled).toBe(true);

    await user.clear(input);
    await user.click(input);
    await user.paste("Tightened the intro");
    expect(save.disabled).toBe(false);
  } finally {
    view.unmount();
  }
});

test("Enter submits the trimmed summary", async () => {
  const user = userEvent.setup();
  const saved: string[] = [];
  const view = render(
    <NameVersionDialog
      error={null}
      onClose={() => {}}
      onSave={(summary) => saved.push(summary)}
      open
      saving={false}
    />
  );
  try {
    const input = screen.getByLabelText("What changed?");
    await user.click(input);
    await user.paste("  Tightened the intro  ");
    await user.keyboard("{Enter}");
    expect(saved).toEqual(["Tightened the intro"]);
  } finally {
    view.unmount();
  }
});

// The Enter that ends an input method's composition - of Japanese, Chinese or Korean - takes the
// word being composed, and is no Enter of the reader's: it saves nothing. Chromium and Firefox
// mark it `isComposing`; Safari sends it after the composition's end, unmarked, with keyCode 229
// (`inputMethodKey`). A plain Enter after it saves the summary.
test.each([
  ["isComposing", { isComposing: true, key: "Enter" }],
  ["keyCode 229", { isComposing: false, key: "Enter", keyCode: 229 }],
] as const)("the Enter that ends a composition, marked by %s, saves nothing, and a plain one saves", async (_mark, composing) => {
  const user = userEvent.setup();
  const saved: string[] = [];
  const view = render(
    <NameVersionDialog
      error={null}
      onClose={() => {}}
      onSave={(summary) => saved.push(summary)}
      open
      saving={false}
    />
  );
  try {
    const input = screen.getByLabelText("What changed?");
    await user.click(input);
    await user.paste("Tightened the intro");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(input, composing);
    expect(saved).toEqual([]);
    await user.keyboard("{Enter}");
    expect(saved).toEqual(["Tightened the intro"]);
  } finally {
    view.unmount();
  }
});

test("Enter does nothing while the input is empty", async () => {
  const user = userEvent.setup();
  const saved: string[] = [];
  const view = render(
    <NameVersionDialog
      error={null}
      onClose={() => {}}
      onSave={(summary) => saved.push(summary)}
      open
      saving={false}
    />
  );
  try {
    const input = screen.getByLabelText("What changed?");
    await user.click(input);
    await user.keyboard("{Enter}");
    expect(saved).toEqual([]);
  } finally {
    view.unmount();
  }
});

// The Enter that ends an input method's composition commits what the reader composed into the
// field and is the input method's (`INPUT_METHOD_MARKS`): it names no version. The plain Enter
// after it saves the summary.
test.each(
  INPUT_METHOD_MARKS
)("an Enter that ends an IME composition, marked by %s, names no version", async (_mark, composing) => {
  const user = userEvent.setup();
  const saved: string[] = [];
  const view = render(
    <NameVersionDialog
      error={null}
      onClose={() => {}}
      onSave={(summary) => saved.push(summary)}
      open
      saving={false}
    />
  );
  try {
    const input = screen.getByLabelText("What changed?");
    await user.click(input);
    await user.paste("Tightened the intro");
    // The key that ends a composition: user-event has no form for `isComposing` or keyCode 229.
    fireEvent.keyDown(input, { ...composing, key: "Enter" });
    expect(saved, "the Enter that ended a composition named a version").toEqual([]);

    await user.keyboard("{Enter}");
    expect(saved).toEqual(["Tightened the intro"]);
  } finally {
    view.unmount();
  }
});

test("Cancel calls onClose", async () => {
  const user = userEvent.setup();
  let closed = 0;
  const view = render(
    <NameVersionDialog
      error={null}
      onClose={() => (closed += 1)}
      onSave={() => {}}
      open
      saving={false}
    />
  );
  try {
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(closed).toBe(1);
  } finally {
    view.unmount();
  }
});

test("a failed save keeps the dialog and the typed summary and shows the error inside it", () => {
  const view = render(
    <NameVersionDialog
      error={new Error("Failed to fetch")}
      onClose={() => {}}
      onSave={() => {}}
      open
      saving={false}
    />
  );
  try {
    expect(within(view.container).getByRole("alert")).toHaveTextContent(
      "Could not name this version"
    );
    expect(
      (within(view.container).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled
    ).toBe(true);
  } finally {
    view.unmount();
  }
});

// A version the reader stopped may have been named: the dialog says so, and that Save is a press
// of its own, rather than a failure it cannot know of.
test("a version the reader stopped says it may have been named, not that it failed", () => {
  const view = render(
    <NameVersionDialog
      error={new SendStoppedError()}
      onClose={() => {}}
      onSave={() => {}}
      open
      saving={false}
    />
  );
  try {
    expect(within(view.container).getByRole("alert").textContent).toBe(
      stoppedWriteNote("the new version", "made", {
        anew: "Saving it again names a new version.",
      })
    );
  } finally {
    view.unmount();
  }
});
