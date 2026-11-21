import { expect, spyOn, test } from "bun:test";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { settle } from "../../__tests__/fake-clock";
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
    passkeys: [
      { email: ana, enrolled_at: "2026-11-20T10:00:00Z", made_by: "alice", project: "CORE" },
    ],
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

// A person a project of another owner names directly gets no link from this owner (the route
// refuses it, 409 LISTED_BY_ANOTHER_OWNER, and the answer leaves them out of invitable). The row
// says why there is no Invite, so the owner is not left guessing.
test("a person another owner's project names has no Invite, and the row says why", async () => {
  const panel = renderPanel({
    answer: withPasskeys({ invitable: [], listed_by_another_owner: [ana], passkeys: [] }),
  });
  try {
    await screen.findByRole("heading", { name: "People" });
    const anaRow = personRow(ana);
    expect(within(anaRow).queryByRole("button", { name: /^Invite/ })).not.toBeInTheDocument();
    expect(
      within(anaRow).getByText(
        "Another project's owner also names this person, so no link can be made here: a passkey would read that project too."
      )
    ).toBeInTheDocument();
    // Carol is named by no other owner's project: her row says nothing of the kind.
    expect(
      within(personRow("carol@example.com")).queryByText(/Another project's owner/)
    ).not.toBeInTheDocument();
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
      expires_at: "2099-11-21T10:00:00Z",
      made_at: "2099-11-20T10:00:00Z",
      url: `https://dispatch.example/enroll#${"a".repeat(43)}`,
    })
    .mockResolvedValueOnce({
      expires_at: "2099-11-21T11:00:00Z",
      made_at: "2099-11-20T11:00:00Z",
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
    // No second press gets past it, so the row offers no Retry.
    expect(within(personRow(ana)).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});

// Naming a person who holds a passkey another owner's link enrolled is refused until that owner
// resets them (409 PASSKEY_OF_ANOTHER_OWNER), and a Reset of a full member or a Workspace address
// is refused for good: no Retry gets past either, so the panel shows Dispatch's sentence, which
// names no other owner, and no Retry. A refusal a Retry can get past keeps it (the input refusal
// of ProjectAccessPanel.test.tsx is the control).
test("a refusal no Retry can change shows Dispatch's sentence and no Retry", async () => {
  const user = userEvent.setup();
  const sentence =
    "this address holds a passkey that a link of another project's owner enrolled; that owner resets them first";
  const panel = renderPanel({ answer: withPasskeys() });
  const put = spyOn(api, "putProjectAccessPerson").mockRejectedValue(
    new ApiError(409, { code: "PASSKEY_OF_ANOTHER_OWNER", error: sentence })
  );
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const reset = spyOn(api, "resetProjectAccessPerson").mockRejectedValue(
    new ApiError(409, { code: "FULL_MEMBER", error: "a full member signs in with Google" })
  );
  try {
    await user.type(await screen.findByLabelText("Add a person"), "held@other.example");
    await user.click(screen.getByRole("button", { name: "Add person" }));
    const added = await screen.findByRole("alert");
    expect(added).toHaveTextContent(sentence);
    expect(within(added).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: `Reset ${ana}` }));
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent("a full member signs in with Google")
    );
    expect(
      within(screen.getByRole("alert")).queryByRole("button", { name: "Retry" })
    ).not.toBeInTheDocument();
  } finally {
    panel.restore();
    put.mockRestore();
    confirm.mockRestore();
    reset.mockRestore();
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

// The lost answer's words reach a screen reader as the new link's do: through a status region that
// was in the row before the press, since a region that mounts with its words already in it is not
// announced by every reader (the row's own rule for the link, AccessPersonRow's doc comment).
test("a lost answer is announced through a status region there before the press", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const invite = spyOn(api, "createProjectInvitation").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "the write was made" })
  );
  try {
    await screen.findByRole("button", { name: `Invite ${ana}` });
    const before = within(personRow(ana)).getAllByRole("status");
    await user.click(screen.getByRole("button", { name: `Invite ${ana}` }));
    const note = await within(personRow(ana)).findByText(
      "Dispatch made a link, but its answer was lost on the way. Press Invite again: a new link ends that one."
    );
    expect(before).toContain(note.closest('[role="status"]') as HTMLElement);
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
    expires_at: "2099-11-21T10:00:00Z",
    made_at: "2099-11-20T10:00:00Z",
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

// A Retry of a Reset Dispatch refused is a new Reset under a new key, which deletes the person's
// passkeys and signs them out as the first would have: it asks first, as the row's Reset does.
test("Retry of a refused Reset asks first, and sends nothing until the owner confirms", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const confirm = spyOn(window, "confirm")
    .mockReturnValueOnce(true)
    .mockReturnValueOnce(false)
    .mockReturnValueOnce(true);
  const reset = spyOn(api, "resetProjectAccessPerson")
    .mockRejectedValueOnce(
      new ApiError(409, { code: "PERSON_NOT_LISTED", error: "the list no longer names them" })
    )
    .mockImplementation(panel.lands(withPasskeys({ passkeys: [] })));
  try {
    await user.click(await screen.findByRole("button", { name: `Reset ${ana}` }));
    const retry = await within(await screen.findByRole("alert")).findByRole("button", {
      name: "Retry",
    });
    expect(reset).toHaveBeenCalledTimes(1);

    await user.click(retry);
    expect(confirm).toHaveBeenCalledTimes(2);
    expect(confirm).toHaveBeenLastCalledWith(
      `Reset ${ana}? Dispatch deletes their passkeys and their unused links of every project, signs them out, and revokes their personal tokens and the device logins they approved. They sign in again only through a new link.`
    );
    await settle();
    expect(reset).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(within(personRow(ana)).queryByTestId("access-passkey")).not.toBeInTheDocument()
    );
    expect(reset).toHaveBeenCalledTimes(2);
  } finally {
    panel.restore();
    confirm.mockRestore();
    reset.mockRestore();
  }
});

// Reset deletes the person's unused links in its own transaction, so the link an earlier Invite
// showed in the row no longer enrolls anyone: the row stops showing it, and its Copy, once the
// Reset lands, rather than offer a dead link to send.
test("Reset takes away the link an earlier Invite showed, since Dispatch deletes it", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const invite = spyOn(api, "createProjectInvitation").mockResolvedValue({
    expires_at: "2099-11-21T10:00:00Z",
    made_at: "2099-11-20T10:00:00Z",
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

// A Reset Dispatch refuses deletes nothing. The refusal that reaches the page while the link is
// still live is the keyed write's: Dispatch answers the Reset 401 twice in a row while the page's
// sign-in check still names the owner (`writeUnderKey`), which ends the write as refused. Every
// refusal the Reset route itself documents leaves no live link (a delisting deletes it, a full
// member or passkey sign-in turned off refuses it), and a 5xx is retried until it lands. A link's
// use reads nothing of its maker's session, so after those two 401s the link still enrolls the
// person: the row keeps showing it beside the refusal, as it showed it before the press. The mock
// stands in for that refusal with an answer the keyed write does not retry.
test("a Reset refused by two 401s in a row, the sign-in check still naming the owner, leaves the link an earlier Invite showed", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys() });
  const url = `https://dispatch.example/enroll#${"a".repeat(43)}`;
  const invite = spyOn(api, "createProjectInvitation").mockResolvedValue({
    expires_at: "2099-11-21T10:00:00Z",
    made_at: "2099-11-20T10:00:00Z",
    url,
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const reset = spyOn(api, "resetProjectAccessPerson").mockRejectedValue(
    new ApiError(409, { code: "REFUSED", error: "Dispatch refused the Reset" })
  );
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    const link = { name: `Enrollment link for ${ana}` };
    expect(await within(personRow(ana)).findByRole("textbox", link)).toHaveValue(url);

    await user.click(screen.getByRole("button", { name: `Reset ${ana}` }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(reset).toHaveBeenCalledTimes(1);
    expect(within(personRow(ana)).getByRole("textbox", link)).toHaveValue(url);
    expect(within(personRow(ana)).getByTestId("access-invitation-expiry")).toBeInTheDocument();
  } finally {
    panel.restore();
    invite.mockRestore();
    confirm.mockRestore();
    reset.mockRestore();
  }
});

// Once a project of another owner names the person, the link this row showed enrolls nobody
// (the enrollment refuses it, LISTED_BY_ANOTHER_OWNER): the row stops offering it as one that
// works once, and says why no link can be made here.
test("a link the row showed is taken away while another owner's project names the person", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys({ passkeys: [] }) });
  const invite = spyOn(api, "createProjectInvitation").mockResolvedValue({
    expires_at: "2099-11-21T10:00:00Z",
    made_at: "2099-11-20T10:00:00Z",
    url: `https://dispatch.example/enroll#${"a".repeat(43)}`,
  });
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    const link = { name: `Enrollment link for ${ana}` };
    expect(await within(personRow(ana)).findByRole("textbox", link)).toBeInTheDocument();

    await panel.lands(
      withPasskeys({ invitable: [], listed_by_another_owner: [ana], passkeys: [] })
    )();
    await act(() => panel.queryClient.invalidateQueries());
    await waitFor(() =>
      expect(
        within(personRow(ana)).getByText(
          "Another project's owner also names this person, so no link can be made here: a passkey would read that project too."
        )
      ).toBeInTheDocument()
    );
    expect(within(personRow(ana)).queryByRole("textbox", link)).not.toBeInTheDocument();
    expect(
      within(personRow(ana)).queryByTestId("access-invitation-expiry")
    ).not.toBeInTheDocument();
    expect(within(personRow(ana)).getByTestId("access-invitation-announcement")).toHaveTextContent(
      /^$/
    );
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});

// The person used the link while the owner's row showed it: the link stops at its first use, so
// once the access answer lists the passkey it made (enrolled through this project's link after the
// link's made_at), the row stops offering it as a link that works once.
test("a link the person used is taken out of the row once the answer lists the passkey it made", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys({ passkeys: [] }) });
  const invite = spyOn(api, "createProjectInvitation").mockResolvedValue({
    expires_at: "2099-11-21T10:00:00Z",
    made_at: "2099-11-20T10:00:00Z",
    url: `https://dispatch.example/enroll#${"a".repeat(43)}`,
  });
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    const link = { name: `Enrollment link for ${ana}` };
    expect(await within(personRow(ana)).findByRole("textbox", link)).toBeInTheDocument();

    await panel.lands(
      withPasskeys({
        passkeys: [
          { email: ana, enrolled_at: "2099-11-20T10:05:00Z", made_by: "alice", project: "CORE" },
        ],
      })
    )();
    await act(() => panel.queryClient.invalidateQueries());
    await waitFor(() =>
      expect(within(personRow(ana)).getByTestId("access-passkey")).toBeInTheDocument()
    );
    expect(within(personRow(ana)).queryByRole("textbox", link)).not.toBeInTheDocument();
    expect(
      within(personRow(ana)).queryByTestId("access-invitation-expiry")
    ).not.toBeInTheDocument();
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});

// One owner may list the person on two projects, each with a link of its own. A passkey the
// person enrolled with the other project's link, after this project's link was made, did not use
// this link: the row keeps offering it.
test("a passkey enrolled with another project's link leaves this project's link in the row", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys({ passkeys: [] }) });
  const invite = spyOn(api, "createProjectInvitation").mockResolvedValue({
    expires_at: "2099-11-21T10:00:00Z",
    made_at: "2099-11-20T10:00:00Z",
    url: `https://dispatch.example/enroll#${"a".repeat(43)}`,
  });
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    const link = { name: `Enrollment link for ${ana}` };
    expect(await within(personRow(ana)).findByRole("textbox", link)).toBeInTheDocument();

    await panel.lands(
      withPasskeys({
        passkeys: [
          { email: ana, enrolled_at: "2099-11-20T10:05:00Z", made_by: "alice", project: "OTHER" },
        ],
      })
    )();
    await act(() => panel.queryClient.invalidateQueries());
    await waitFor(() =>
      expect(within(personRow(ana)).getByTestId("access-passkey")).toBeInTheDocument()
    );
    expect(within(personRow(ana)).getByRole("textbox", link)).toBeInTheDocument();
    expect(within(personRow(ana)).getByTestId("access-invitation-expiry")).toBeInTheDocument();
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});

// While a row shows a link, it reads the access list again every few seconds. A read that fails
// keeps the list it already read: the panel does not swap itself for its error view, so the row,
// and the link Dispatch showed only once, stay.
test("a failed read of the access list while a link shows keeps the link in the row", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys({ passkeys: [] }) });
  const invite = spyOn(api, "createProjectInvitation").mockResolvedValue({
    expires_at: "2099-11-21T10:00:00Z",
    made_at: "2099-11-20T10:00:00Z",
    url: `https://dispatch.example/enroll#${"a".repeat(43)}`,
  });
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    const link = { name: `Enrollment link for ${ana}` };
    expect(await within(personRow(ana)).findByRole("textbox", link)).toBeInTheDocument();

    panel.getProjectAccess.mockRejectedValueOnce(
      new ApiError(503, { code: "DATABASE_BUSY", error: "the database is busy" })
    );
    await act(() => panel.queryClient.refetchQueries({ queryKey: ["project-access", "CORE"] }));
    await waitFor(() =>
      expect(panel.queryClient.getQueryState(["project-access", "CORE"])?.status).toBe("error")
    );
    expect(within(personRow(ana)).getByRole("textbox", link)).toHaveValue(
      `https://dispatch.example/enroll#${"a".repeat(43)}`
    );
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});

// A link stops working at its expires_at, so the row stops offering it then: it shows no dead
// link as one that works once, and its reads of the access list end with it.
test("a link leaves the row when it lapses", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: withPasskeys({ passkeys: [] }) });
  const invite = spyOn(api, "createProjectInvitation").mockResolvedValue({
    expires_at: new Date(Date.now() + 400).toISOString(),
    made_at: new Date(Date.now() - 1000).toISOString(),
    url: `https://dispatch.example/enroll#${"a".repeat(43)}`,
  });
  try {
    await user.click(await screen.findByRole("button", { name: `Invite ${ana}` }));
    const link = { name: `Enrollment link for ${ana}` };
    expect(await within(personRow(ana)).findByRole("textbox", link)).toBeInTheDocument();
    await waitFor(() =>
      expect(within(personRow(ana)).queryByRole("textbox", link)).not.toBeInTheDocument()
    );
    expect(
      within(personRow(ana)).queryByTestId("access-invitation-expiry")
    ).not.toBeInTheDocument();
    expect(within(personRow(ana)).getByTestId("access-invitation-announcement")).toHaveTextContent(
      /^$/
    );
  } finally {
    panel.restore();
    invite.mockRestore();
  }
});
