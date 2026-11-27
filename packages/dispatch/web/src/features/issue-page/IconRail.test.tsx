import { expect, test } from "bun:test";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { AuthenticatedUser } from "../../api/types";
import { IconRail } from "./IconRail";

function renderRail(user: AuthenticatedUser): void {
  render(
    <MemoryRouter>
      <IconRail
        inboxCount={0}
        navigationOpen={false}
        onOpenNavigation={() => {}}
        project="CORE"
        user={user}
      />
    </MemoryRouter>
  );
}

// The Settings page's sections are a person's own (their tokens, their sign-ins, their grants), and
// so is the Inbox (the asks, threads and credential requests waiting on them), so the rail offers
// both to a person and neither to an agent viewer, as the sidebar does; every other entry is the
// same for both.
test("the rail offers Settings and the Inbox to a person and neither to an agent viewer", () => {
  try {
    renderRail({ event_head: "0", kind: "user", login: "alice" });
    expect(screen.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings");
    expect(screen.getByRole("link", { name: "Inbox" })).toHaveAttribute("href", "/");
    expect(screen.getByRole("link", { name: "Delivery" })).toBeInTheDocument();
    cleanup();

    renderRail({ event_head: "0", kind: "agent", login: "agent", session: null });
    expect(screen.queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /^Inbox/ })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Delivery" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "CORE" })).toHaveAttribute("href", "/projects/CORE");
  } finally {
    cleanup();
  }
});
