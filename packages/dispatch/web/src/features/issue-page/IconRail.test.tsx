import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { AuthenticatedUser } from "../../api/types";
import { IconRail } from "./IconRail";

const USER: AuthenticatedUser = { event_head: "7", kind: "user", login: "alice" };

// The rail's Inbox carries the count in its name as the sidebar's does: none while a read it adds
// up is still to answer (`useInboxCount` answers undefined until then), none at zero, and the
// number after the word above zero.
test("the rail's Inbox is named Inbox until its count has answered and at 0, and Inbox 3 at 3", () => {
  const cases: readonly (readonly [number | undefined, string])[] = [
    [undefined, "Inbox"],
    [0, "Inbox"],
    [3, "Inbox 3"],
  ];
  for (const [inboxCount, name] of cases) {
    const view = render(
      <MemoryRouter>
        <IconRail
          inboxCount={inboxCount}
          navigationOpen={false}
          onOpenNavigation={() => {}}
          project="CORE"
          user={USER}
        />
      </MemoryRouter>
    );
    const inbox = screen.getByRole("link", { name: /^Inbox/ });
    expect(inbox, `count ${inboxCount}`).toHaveAccessibleName(name);
    view.unmount();
  }
});
