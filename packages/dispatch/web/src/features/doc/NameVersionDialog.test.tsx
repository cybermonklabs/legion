import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: user-event cannot mark a key as an input method's (`isComposing`, keyCode 229), which the Enter row below reads
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

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

// A reader typing Chinese, Japanese or Korean confirms a word with Enter: that Enter is the input
// method's, marked `isComposing`, or sent unmarked with keyCode 229 after the composition's end, as
// Safari sends it (WebKit bug 165004). It saves nothing; a plain Enter after it saves.
test("the Enter that confirms an input method's word saves no version, and a plain Enter saves", async () => {
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
    // user-event cannot mark a key as an input method's, so the two such Enters are fired.
    expect(fireEvent.keyDown(input, { isComposing: true, key: "Enter" })).toBe(true);
    expect(fireEvent.keyDown(input, { isComposing: false, key: "Enter", keyCode: 229 })).toBe(true);
    expect(saved).toEqual([]);

    // The plain Enter goes the same way as the two above, so only the composition mark differs.
    expect(fireEvent.keyDown(input, { key: "Enter" })).toBe(false);
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
