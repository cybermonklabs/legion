import { expect, mock, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { RewriteBadge } from "./RewriteBadge";

// The one mark a rewrite leaves (INFRA-3231's R7): Rewriting… while it runs, Rewritten once it is
// in place, Original while the original wording shows, and Rewrite failed with its reason only
// in the tooltip.

test("a rewrite that runs reads Rewriting…, which is no control", () => {
  const view = render(
    <RewriteBadge
      onToggle={() => {}}
      rewrite={{ state: "rewriting" }}
      showingOriginal={false}
      toggleKey="w"
    />
  );
  try {
    expect(screen.getByRole("status").textContent).toBe("Rewriting…");
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("the reader's own press reads Rewriting… before the read says so", () => {
  const view = render(
    <RewriteBadge onToggle={() => {}} rewrite={undefined} running showingOriginal={false} />
  );
  try {
    expect(screen.getByRole("status").textContent).toBe("Rewriting…");
  } finally {
    view.unmount();
  }
});

test("a rewrite in place reads Rewritten, and the badge or w shows the original as Original", async () => {
  const user = userEvent.setup();
  const onToggle = mock(() => {});
  const view = render(
    <RewriteBadge
      onToggle={onToggle}
      rewrite={{ state: "rewritten" }}
      showingOriginal={false}
      toggleKey="w"
    />
  );
  try {
    const badge = screen.getByRole("button", { name: "Rewritten" });
    expect(badge).toHaveAttribute("aria-pressed", "false");
    expect(badge).toHaveAttribute("title", "Show the original wording (w)");
    await user.click(badge);
    expect(onToggle).toHaveBeenCalledTimes(1);

    view.rerender(
      <RewriteBadge
        onToggle={onToggle}
        rewrite={{ state: "rewritten" }}
        showingOriginal
        toggleKey="w"
      />
    );
    const original = screen.getByRole("button", { name: "Original" });
    expect(original).toHaveAttribute("aria-pressed", "true");
    expect(original).toHaveAttribute("title", "Show the rewrite (w)");
  } finally {
    view.unmount();
  }
});

test("a surface with no key for the original names none in the tooltip", () => {
  const view = render(
    <RewriteBadge onToggle={() => {}} rewrite={{ state: "rewritten" }} showingOriginal={false} />
  );
  try {
    expect(screen.getByRole("button", { name: "Rewritten" })).toHaveAttribute(
      "title",
      "Show the original wording"
    );
  } finally {
    view.unmount();
  }
});

test("a failed rewrite reads Rewrite failed, says why only in its tooltip, and tries again", async () => {
  const user = userEvent.setup();
  const onRetry = mock(() => {});
  const reason = "The rewrite changed an option's label.";
  const view = render(
    <RewriteBadge
      onRetry={onRetry}
      onToggle={() => {}}
      rewrite={{ reason, state: "failed" }}
      showingOriginal={false}
      toggleKey="w"
    />
  );
  try {
    const badge = screen.getByRole("button", { name: "Rewrite failed" });
    expect(badge.textContent).toBe("Rewrite failed");
    expect(view.container.textContent).not.toContain(reason);
    expect(badge).toHaveAttribute("title", `${reason} Try again (r)`);
    await user.click(badge);
    expect(onRetry).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("no rewrite, and a state this release does not know, draw nothing", () => {
  for (const rewrite of [undefined, { state: "queued" }]) {
    const view = render(
      <RewriteBadge onToggle={() => {}} rewrite={rewrite} showingOriginal={false} />
    );
    try {
      expect(view.container.textContent).toBe("");
    } finally {
      view.unmount();
    }
  }
});
