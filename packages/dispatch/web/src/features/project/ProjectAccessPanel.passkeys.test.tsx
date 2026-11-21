import { expect, spyOn, test } from "bun:test";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import type { ProjectAccess } from "../../api/types";
import { access, named, renderPanel } from "./project-access-fixture";

// The Access tab's passkey controls: what the owner reads of each person's passkeys where
// passkey sign-in is on, and Invite and Reset. The panel's other controls are
// ProjectAccessPanel.test.tsx's, over the same fixture.

const ana = "ana@other.example";

/** CORE's access as its owner reads it where passkey sign-in is on: ana from outside, whom the
 *  invitation route takes, and carol of the Workspace, whom it does not. */
function withPasskeys(overrides: Partial<ProjectAccess> = {}): ProjectAccess {
  return access({
    invitable: [ana],
    passkeys: [{ email: ana, enrolled_at: "2026-11-20T10:00:00Z" }],
    people: [ana, "carol@example.com"],
    ...overrides,
  });
}

/** The row of `email` on the People list. */
function personRow(email: string): HTMLElement {
  const row = within(screen.getByRole("list", { name: "People" }))
    .getAllByRole("listitem")
    .find((item) => item.firstChild?.textContent?.startsWith(email) === true);
  if (row === undefined) throw new Error(`no row of the People list names ${email}`);
  return row;
}

test("the owner's rows show each passkey's enrollment time, and Invite and Reset only where they can act", async () => {
  const panel = renderPanel({ answer: withPasskeys() });
  try {
    await screen.findByRole("heading", { name: "People" });
    expect(named("People")).toEqual([ana, "carol@example.com"]);
    const anaRow = personRow(ana);
    expect(
      within(anaRow)
        .getAllByTestId("access-passkey")
        .map((line) => line.textContent)
    ).toEqual([expect.stringMatching(/^Passkey enrolled /)]);
    expect(within(anaRow).getByRole("button", { name: `Invite ${ana}` })).toBeInTheDocument();
    expect(within(anaRow).getByRole("button", { name: `Reset ${ana}` })).toBeInTheDocument();
    // A person of the Workspace signs in with Google: no link is made for them, and no passkey
    // of theirs exists to reset.
    const carolRow = personRow("carol@example.com");
    expect(within(carolRow).queryByRole("button", { name: /^Invite/ })).not.toBeInTheDocument();
    expect(within(carolRow).queryByRole("button", { name: /^Reset/ })).not.toBeInTheDocument();
    expect(within(carolRow).queryByTestId("access-passkey")).not.toBeInTheDocument();
  } finally {
    panel.restore();
  }
});

test("a person Dispatch would make a link for, with no passkey yet, is told apart from one who has one", async () => {
  const panel = renderPanel({ answer: withPasskeys({ passkeys: [] }) });
  try {
    await screen.findByRole("heading", { name: "People" });
    const anaRow = personRow(ana);
    expect(within(anaRow).getByText("No passkey yet")).toBeInTheDocument();
    expect(within(anaRow).queryByRole("button", { name: `Reset ${ana}` })).not.toBeInTheDocument();
  } finally {
    panel.restore();
  }
});

test("without passkey sign-in, or for anyone but the owner, no row offers Invite or Reset", async () => {
  for (const options of [
    { answer: access({ people: [ana] }) },
    { answer: access({ people: [ana] }), login: "bob" },
  ]) {
    const panel = renderPanel(options);
    try {
      await screen.findByRole("heading", { name: "People" });
      expect(screen.queryByRole("button", { name: /^Invite/ })).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: /^Reset/ })).not.toBeInTheDocument();
      expect(screen.queryByText("No passkey yet")).not.toBeInTheDocument();
    } finally {
      panel.restore();
    }
  }
});

test("Invite shows the link to send and when it stops, and a second press makes a new one", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const invite = spyOn(api, "createProjectInvitation")
    .mockResolvedValueOnce({
      expires_at: "2026-11-21T10:00:00Z",
      url: `https://dispatch.example/enroll#${"a".repeat(43)}`,
    })
    .mockResolvedValueOnce({
      expires_at: "2026-11-21T11:00:00Z",
      url: `https://dispatch.example/enroll#${"b".repeat(43)}`,
    });
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    const link = await within(personRow(ana)).findByRole("textbox", {
      name: `Enrollment link for ${ana}`,
    });
    expect(link).toHaveValue(`https://dispatch.example/enroll#${"a".repeat(43)}`);
    expect(link).toHaveAttribute("readonly");
    expect(invite).toHaveBeenCalledWith("CORE", ana, keyedAttempt);
    expect(within(personRow(ana)).getByTestId("access-invitation-expiry")).toHaveTextContent(
      /^Works once, until .+\. Dispatch shows it only here, once: send it to ana@other\.example yourself\.$/
    );

    await user.click(screen.getByRole("button", { name: `Invite ${ana}` }));
    await waitFor(() =>
      expect(
        within(personRow(ana)).getByRole("textbox", { name: `Enrollment link for ${ana}` })
      ).toHaveValue(`https://dispatch.example/enroll#${"b".repeat(43)}`)
    );
    expect(invite).toHaveBeenCalledTimes(2);
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});

test("an Invite Dispatch refuses says why in the person's row", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const invite = spyOn(api, "createProjectInvitation").mockRejectedValue(
    new ApiError(409, { code: "FULL_MEMBER", error: "a full member signs in with Google" })
  );
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    expect(await within(personRow(ana)).findByRole("alert")).toHaveTextContent(
      `No link can be made for ${ana}: they sign in with Google as a member of the company's Google Workspace.`
    );
    expect(
      within(personRow(ana)).queryByRole("textbox", { name: `Enrollment link for ${ana}` })
    ).not.toBeInTheDocument();
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});

test("a link Dispatch made whose answer was lost says so, and Invite makes a new one", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const invite = spyOn(api, "createProjectInvitation").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "the write was made" })
  );
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    // A status, so a screen reader announces it; the row's empty announcement region is another.
    expect(
      await within(personRow(ana)).findByText(
        "Dispatch made a link, but its answer was lost on the way. Press Invite again: a new link ends that one."
      )
    ).toHaveAttribute("role", "status");
    expect(within(personRow(ana)).getByTestId("access-invitation-announcement")).toHaveTextContent(
      /^$/
    );
    expect(within(personRow(ana)).queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});

// The link appears beside the Invite button the owner just pressed, where the focus stays, so a
// screen reader hears of it through the row's status region: the region is there before the press
// and empty, and the press fills it with the address and when the link stops, not the link itself.
test("a new link and when it stops are announced to a screen reader, through a region there before the press", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const invite = spyOn(api, "createProjectInvitation").mockResolvedValue({
    expires_at: "2026-11-21T10:00:00Z",
    url: `https://dispatch.example/enroll#${"a".repeat(43)}`,
  });
  try {
    await screen.findByRole("button", { name: `Invite ${ana}` });
    const region = within(personRow(ana)).getByTestId("access-invitation-announcement");
    expect(region).toHaveAttribute("role", "status");
    expect(region).toHaveTextContent(/^$/);

    await user.click(screen.getByRole("button", { name: `Invite ${ana}` }));
    await waitFor(() =>
      expect(region).toHaveTextContent(
        /^Made a link for ana@other\.example\. It works once, until .+\.$/
      )
    );
    // The same node, so the reader announces the change rather than meeting a new region.
    expect(within(personRow(ana)).getByTestId("access-invitation-announcement")).toBe(region);
    expect(region.textContent).not.toContain("enroll#");
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});

test("Reset asks first, then the person's passkeys are gone from the row", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const confirm = spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValueOnce(true);
  const reset = spyOn(api, "resetProjectAccessPerson").mockImplementation(
    panel.lands(withPasskeys({ passkeys: [] }))
  );
  try {
    await user.click(await screen.findByRole("button", { name: `Reset ${ana}` }));
    expect(confirm).toHaveBeenCalledWith(
      `Reset ${ana}? Dispatch deletes their passkeys and their unused links of every project, signs them out, and revokes their personal tokens and the device logins they approved. They sign in again only through a new link.`
    );
    expect(reset).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: `Reset ${ana}` }));
    await waitFor(() =>
      expect(within(personRow(ana)).queryByTestId("access-passkey")).not.toBeInTheDocument()
    );
    expect(reset).toHaveBeenCalledWith("CORE", ana, keyedAttempt);
    expect(within(personRow(ana)).getByText("No passkey yet")).toBeInTheDocument();
    expect(
      within(personRow(ana)).queryByRole("button", { name: `Reset ${ana}` })
    ).not.toBeInTheDocument();
  } finally {
    panel.restore();
    confirm.mockRestore();
    reset.mockRestore();
  }
});

// Reset deletes the person's unused links in its own transaction, so the link an earlier Invite
// showed in the row no longer enrolls anyone: the row stops showing it, and its Copy, the moment
// the owner confirms, rather than offer a dead link to send.
test("Reset takes away the link an earlier Invite showed, since Dispatch deletes it", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const invite = spyOn(api, "createProjectInvitation").mockResolvedValue({
    expires_at: "2026-11-21T10:00:00Z",
    url: `https://dispatch.example/enroll#${"a".repeat(43)}`,
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const reset = spyOn(api, "resetProjectAccessPerson").mockImplementation(
    panel.lands(withPasskeys({ passkeys: [] }))
  );
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    const link = { name: `Enrollment link for ${ana}` };
    expect(await within(personRow(ana)).findByRole("textbox", link)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: `Reset ${ana}` }));
    await waitFor(() =>
      expect(within(personRow(ana)).queryByTestId("access-passkey")).not.toBeInTheDocument()
    );
    expect(reset).toHaveBeenCalledWith("CORE", ana, keyedAttempt);
    expect(within(personRow(ana)).queryByRole("textbox", link)).not.toBeInTheDocument();
    expect(
      within(personRow(ana)).queryByRole("button", { name: `Copy the link for ${ana}` })
    ).not.toBeInTheDocument();
    expect(
      within(personRow(ana)).queryByTestId("access-invitation-expiry")
    ).not.toBeInTheDocument();
    expect(within(personRow(ana)).getByTestId("access-invitation-announcement")).toHaveTextContent(
      /^$/
    );
    // Invite still makes a new link.
    expect(
      within(personRow(ana)).getByRole("button", { name: `Invite ${ana}` })
    ).toBeInTheDocument();
  } finally {
    panel.restore();
    invite.mockRestore();
    confirm.mockRestore();
    reset.mockRestore();
  }
});
