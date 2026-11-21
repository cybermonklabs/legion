import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock, settle } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { pagedGroups } from "../../__tests__/paged-groups";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { ProjectAccess } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { SendExpiredError, SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { ProjectAccessPanel } from "./ProjectAccessPanel";
import { access, crew, named, ops, renderPanel } from "./project-access-fixture";

const notOwner = "Only alice, the project's owner, changes who may see it.";

// The group list is the whole deployment's, which Dispatch refuses a restricted member (403
// DEPLOYMENT_READ_REFUSED): their Access tab never asks for it. The groups it would offer are the
// owner's to add, and an owner is a full member, so a restricted member loses nothing. A full
// member who is not the owner reads the list as always.
test("a restricted member's Access tab never reads the deployment's groups", async () => {
  const restricted = renderPanel({ login: "bob@outside.example", memberKind: "restricted" });
  try {
    expect(await screen.findByTestId("access-visibility")).toHaveTextContent("Public");
    await settle();
    expect(restricted.listAccessGroups).not.toHaveBeenCalled();
    expect(screen.queryByText("Couldn't load the groups.")).not.toBeInTheDocument();
  } finally {
    restricted.restore();
  }

  const full = renderPanel({ login: "carol@example.com" });
  try {
    expect(await screen.findByTestId("access-visibility")).toHaveTextContent("Public");
    await waitFor(() => expect(full.listAccessGroups).toHaveBeenCalled());
  } finally {
    full.restore();
  }
});

// A restricted member's Settings draws no Groups (`SettingsPage`), so their Access tab sends them
// to none: it says what a group admits, with no link to a section they do not have. A full member
// who is not the owner is sent to it as always.
test("a restricted member's Access tab links to no Groups section of Settings", async () => {
  const restricted = renderPanel({ login: "bob@outside.example", memberKind: "restricted" });
  try {
    expect(await screen.findByTestId("access-visibility")).toHaveTextContent("Public");
    expect(screen.getByText(/A group admits only its full members\./)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Settings → Groups" })).not.toBeInTheDocument();
  } finally {
    restricted.restore();
  }

  const full = renderPanel({ login: "carol@example.com" });
  try {
    expect(await screen.findByTestId("access-visibility")).toHaveTextContent("Public");
    expect(screen.getByRole("link", { name: "Settings → Groups" })).toHaveAttribute(
      "href",
      "/settings#groups"
    );
  } finally {
    full.restore();
  }
});

test("shows the owner, the visibility, and the people and groups the access list names", async () => {
  const panel = renderPanel();
  try {
    expect(await screen.findByRole("heading", { name: "Owner" })).toBeInTheDocument();
    expect(screen.getByTestId("access-owner")).toHaveTextContent("alice (you)");
    // The switch reads the visibility the answer holds.
    expect(screen.getByTestId("access-visibility")).toHaveTextContent("Public");
    expect(screen.getByRole("switch", { name: "Private" })).toHaveAttribute(
      "aria-checked",
      "false"
    );
    expect(named("People")).toEqual(["carol@example.com"]);
    expect(named("Groups")).toEqual(["crew"]);
    // The picker offers the groups the list does not name already.
    const picker = screen.getByLabelText<HTMLSelectElement>("Add a group");
    expect(Array.from(picker.options).map((option) => option.value)).toEqual(["", "ops"]);
  } finally {
    panel.restore();
  }
});

// The group list answers a page at a time, and the picker offers the groups of every page.
test("the group picker offers the groups of every page the group list answers", async () => {
  const pages = pagedGroups([[crew], [ops]]);
  const read = spyOn(api, "getProjectAccess").mockResolvedValue(access({ groups: [] }));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ProjectAccessPanel project="CORE" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const picker = await screen.findByLabelText<HTMLSelectElement>("Add a group");
    await waitFor(() =>
      expect(Array.from(picker.options).map((option) => option.value)).toEqual(["", "crew", "ops"])
    );
    expect(pages.mock.calls).toEqual([[undefined], ["crew"]]);
  } finally {
    view.unmount();
    queryClient.clear();
    pages.mockRestore();
    read.mockRestore();
  }
});

test("the owner adds a person and a group and removes each, and the list follows each answer", async () => {
  const user = userEvent.setup();
  const panel = renderPanel();
  const putPerson = spyOn(api, "putProjectAccessPerson").mockImplementation(
    panel.lands(access({ people: ["carol@example.com", "dave@example.com"] }))
  );
  const deletePerson = spyOn(api, "deleteProjectAccessPerson").mockImplementation(
    panel.lands(access({ people: ["dave@example.com"] }))
  );
  const putGroup = spyOn(api, "putProjectAccessGroup").mockImplementation(
    panel.lands(access({ groups: ["crew", "ops"], people: ["dave@example.com"] }))
  );
  const deleteGroup = spyOn(api, "deleteProjectAccessGroup").mockImplementation(
    panel.lands(access({ groups: ["ops"], people: ["dave@example.com"] }))
  );
  try {
    const email = await screen.findByLabelText<HTMLInputElement>("Add a person");
    await user.type(email, " dave@example.com ");
    await user.click(screen.getByRole("button", { name: "Add person" }));
    await waitFor(() => expect(named("People")).toEqual(["carol@example.com", "dave@example.com"]));
    expect(putPerson).toHaveBeenCalledWith("CORE", "dave@example.com", keyedAttempt);
    // A person the list took leaves the field empty for the next.
    expect(email.value).toBe("");

    await user.click(screen.getByRole("button", { name: "Remove carol@example.com" }));
    await waitFor(() => expect(named("People")).toEqual(["dave@example.com"]));
    expect(deletePerson).toHaveBeenCalledWith("CORE", "carol@example.com", keyedAttempt);

    await user.selectOptions(screen.getByLabelText("Add a group"), "ops");
    await user.click(screen.getByRole("button", { name: "Add group" }));
    await waitFor(() => expect(named("Groups")).toEqual(["crew", "ops"]));
    expect(putGroup).toHaveBeenCalledWith("CORE", "ops", keyedAttempt);

    await user.click(screen.getByRole("button", { name: "Remove crew" }));
    await waitFor(() => expect(named("Groups")).toEqual(["ops"]));
    expect(deleteGroup).toHaveBeenCalledWith("CORE", "crew", keyedAttempt);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    panel.restore();
    putPerson.mockRestore();
    deletePerson.mockRestore();
    putGroup.mockRestore();
    deleteGroup.mockRestore();
  }
});

test("a person who is not the owner finds every control disabled, with the reason as its title, and no press writes", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ login: "bob" });
  const deletePerson = spyOn(api, "deleteProjectAccessPerson");
  const deleteGroup = spyOn(api, "deleteProjectAccessGroup");
  try {
    await screen.findByRole("heading", { name: "Owner" });
    expect(screen.getByTestId("access-owner")).toHaveTextContent("alice");
    expect(screen.getByTestId("access-owner")).not.toHaveTextContent("(you)");
    // The reason is on the page for everyone, not in a tooltip alone.
    expect(screen.getByRole("note")).toHaveTextContent(notOwner);
    for (const field of [
      screen.getByLabelText("Give this project to"),
      screen.getByLabelText("Add a person"),
      screen.getByLabelText("Add a group"),
    ]) {
      expect(field).not.toBeEnabled();
      expect(field).toHaveAttribute("title", notOwner);
    }
    for (const name of [
      "Give project",
      "Add person",
      "Add group",
      "Remove carol@example.com",
      "Remove crew",
    ]) {
      const button = screen.getByRole("button", { name });
      expect(button).toHaveAttribute("aria-disabled", "true");
      expect(button).toHaveAttribute("title", notOwner);
    }
    await user.click(screen.getByRole("button", { name: "Remove carol@example.com" }));
    await user.click(screen.getByRole("button", { name: "Remove crew" }));
    expect(deletePerson).not.toHaveBeenCalled();
    expect(deleteGroup).not.toHaveBeenCalled();
  } finally {
    panel.restore();
    deletePerson.mockRestore();
    deleteGroup.mockRestore();
  }
});

test("the owner gives the project to another person once they confirm it, and every control is then disabled", async () => {
  const user = userEvent.setup();
  const panel = renderPanel();
  const confirm = spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValueOnce(true);
  const patch = spyOn(api, "patchProjectAccess").mockImplementation(
    panel.lands(access({ owner: "bob" }))
  );
  try {
    const field = await screen.findByLabelText<HTMLInputElement>("Give this project to");
    await user.type(field, "bob");
    // Declined: nothing is sent and the draft stays.
    await user.click(screen.getByRole("button", { name: "Give project" }));
    expect(confirm).toHaveBeenCalledWith(
      "Give CORE to bob? Only its new owner can change who may see it after this."
    );
    expect(patch).not.toHaveBeenCalled();
    expect(field.value).toBe("bob");

    await user.click(screen.getByRole("button", { name: "Give project" }));
    await waitFor(() => expect(screen.getByTestId("access-owner")).toHaveTextContent("bob"));
    expect(patch).toHaveBeenCalledWith("CORE", { owner: "bob" }, keyedAttempt);
    const reason = "Only bob, the project's owner, changes who may see it.";
    expect(screen.getByRole("note")).toHaveTextContent(reason);
    expect(screen.getByLabelText("Add a person")).not.toBeEnabled();
    expect(screen.getByRole("button", { name: "Remove carol@example.com" })).toHaveAttribute(
      "title",
      reason
    );
  } finally {
    panel.restore();
    confirm.mockRestore();
    patch.mockRestore();
  }
});

// A refusal shows Dispatch's own words beside its control, and Retry where a second try can get
// past it, as this input refusal can once the draft is fixed. A refusal no retry can change - the
// person held by another owner's passkey or project, a full member, a Workspace address - shows
// no Retry (ProjectAccessPanel.passkeys.test.tsx).
test("a refused save a Retry can get past shows the server's own words beside its control, keeps the draft, and Retry sends it again", async () => {
  const user = userEvent.setup();
  const panel = renderPanel();
  const refusal = 'a person is named by their email, and a group by its name: "dave" is no email';
  const putPerson = spyOn(api, "putProjectAccessPerson")
    .mockRejectedValueOnce(new ApiError(400, { code: "PROJECT_ACCESS_INPUT", error: refusal }))
    .mockImplementation(panel.lands(access({ people: ["carol@example.com", "dave"] })));
  try {
    const email = await screen.findByLabelText<HTMLInputElement>("Add a person");
    await user.type(email, "dave");
    await user.click(screen.getByRole("button", { name: "Add person" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(refusal);
    // Shown in the People section, the control it was refused over.
    expect(
      within(screen.getByRole("region", { name: "People" })).getByRole("alert")
    ).toBeInTheDocument();
    expect(email.value).toBe("dave");

    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(putPerson).toHaveBeenCalledTimes(2);
    expect(putPerson).toHaveBeenLastCalledWith("CORE", "dave", keyedAttempt);
  } finally {
    panel.restore();
    putPerson.mockRestore();
  }
});

// The panel reads its refusal from the mutation cache, not from the write's own result, so a save
// the reader stopped reaches it by that road: it may have landed, and the panel says so and what
// its Retry does, not that it failed (#3000's rule for a keyed write the reader stops). Control:
// without it the panel says "Couldn't add dave@example.com."
test("an access save the reader stopped says it may have been made and what Retry does, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const panel = renderPanel();
  const putPerson = spyOn(api, "putProjectAccessPerson").mockRejectedValue(
    new NetworkError(
      "/api/v1/projects/CORE/access/people/dave@example.com",
      new TypeError("Failed to fetch")
    )
  );
  try {
    const email = await screen.findByLabelText<HTMLInputElement>("Add a person");
    await user.type(email, "dave@example.com");
    await user.click(screen.getByRole("button", { name: "Add person" }));
    await stopTheLateWrite(panel.queryClient);

    const alert = within(screen.getByRole("region", { name: "People" })).getByRole("alert");
    // The alert's paragraph, without its Retry button's name.
    expect(alert.querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the addition of dave@example.com", "made", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    panel.restore();
    putPerson.mockRestore();
  }
});

// A PATCH goes under a key, so where the panel that made a stopped save is still open its Retry
// makes that very press again under the key. A panel opened after the save holds no press to make
// again, so its Retry is a new save under a new key, and its note says that instead. Control:
// with one note for both panels, the reopened panel says the Retry goes under the old key.
test("a stopped Slack channel save says Retry asks under its key, and a panel opened after it says Retry is a new save", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const panel = renderPanel();
  const patch = spyOn(api, "patchProjectAccess").mockRejectedValue(
    new NetworkError("/api/v1/projects/CORE/access", new TypeError("Failed to fetch"))
  );
  try {
    await user.type(await screen.findByLabelText<HTMLInputElement>("Channel ids"), "C012345678");
    await user.click(screen.getByRole("button", { name: "Save channels" }));
    await stopTheLateWrite(panel.queryClient);
    const slack = { name: "Slack channels" };
    expect(
      within(screen.getByRole("region", slack)).getByRole("alert").querySelector("p")?.textContent
    ).toBe(
      stoppedWriteNote("CORE's Slack channels", "saved", {
        method: "PATCH",
        stopped: new SendStoppedError(),
      })
    );

    panel.remount();
    await screen.findByLabelText("Channel ids");
    expect(
      within(screen.getByRole("region", slack)).getByRole("alert").querySelector("p")?.textContent
    ).toBe(
      stoppedWriteNote("CORE's Slack channels", "saved", {
        anew: "This panel was opened after that save, so Retry sends it as a new save.",
      })
    );
  } finally {
    realClock();
    panel.restore();
    patch.mockRestore();
  }
});

// A PATCH whose key ran out ends with SendExpiredError, which leaves no press to make again
// (retriesUnderItsKey), even in the panel that made it. That panel still says what its Retry is:
// a new write, which can save the change a second time if the first save landed
// (expiredRetryNote). Control: with madeHere alone deciding, the panel says it was opened after
// that save, and the second-save warning is gone.
test("a Slack channel save whose key ran out warns in its own panel that Retry can save it a second time", async () => {
  const user = userEvent.setup();
  const panel = renderPanel();
  const patch = spyOn(api, "patchProjectAccess").mockRejectedValue(new SendExpiredError());
  try {
    await user.type(await screen.findByLabelText<HTMLInputElement>("Channel ids"), "C012345678");
    await user.click(screen.getByRole("button", { name: "Save channels" }));
    const alert = await within(screen.getByRole("region", { name: "Slack channels" })).findByRole(
      "alert"
    );
    expect(alert.querySelector("p")?.textContent).toBe(
      stoppedWriteNote("CORE's Slack channels", "saved", {
        method: "PATCH",
        stopped: new SendExpiredError(),
      })
    );
  } finally {
    panel.restore();
    patch.mockRestore();
  }
});

test("a project's saves go out one at a time, in the order they were pressed", async () => {
  const user = userEvent.setup();
  const panel = renderPanel();
  const first = Promise.withResolvers<ProjectAccess>();
  const putPerson = spyOn(api, "putProjectAccessPerson").mockImplementationOnce(
    panel.lands(first.promise)
  );
  const deletePerson = spyOn(api, "deleteProjectAccessPerson").mockImplementation(
    panel.lands(access({ people: ["dave@example.com"] }))
  );
  try {
    await user.type(await screen.findByLabelText("Add a person"), "dave@example.com");
    await user.click(screen.getByRole("button", { name: "Add person" }));
    await waitFor(() => expect(putPerson).toHaveBeenCalledTimes(1));
    await user.click(screen.getByRole("button", { name: "Remove carol@example.com" }));
    // The remove waits behind the add, which Dispatch has not answered.
    await settle();
    expect(deletePerson).not.toHaveBeenCalled();

    first.resolve(access({ people: ["carol@example.com", "dave@example.com"] }));
    await waitFor(() => expect(deletePerson).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(named("People")).toEqual(["dave@example.com"]));
  } finally {
    panel.restore();
    putPerson.mockRestore();
    deletePerson.mockRestore();
  }
});

test("says so when the list names nobody and no group, and when no group exists to add", async () => {
  const panel = renderPanel({ answer: access({ groups: [], people: [] }), groups: [] });
  try {
    expect(await screen.findByText("No one is named on this list.")).toBeInTheDocument();
    expect(screen.getByText("No group is on this list.")).toBeInTheDocument();
    const reason = "No group exists yet. Make one in Settings → Groups.";
    expect(screen.getByLabelText("Add a group")).not.toBeEnabled();
    expect(screen.getByLabelText("Add a group")).toHaveAttribute("title", reason);
    expect(screen.getByRole("button", { name: "Add group" })).toHaveAttribute("title", reason);
    expect(screen.getByRole("link", { name: "Settings → Groups" })).toHaveAttribute(
      "href",
      "/settings#groups"
    );
  } finally {
    panel.restore();
  }
});

test("reads the identity the shell already holds, and asks Dispatch for none", async () => {
  const panel = renderPanel();
  try {
    await screen.findByRole("heading", { name: "Owner" });
    await waitFor(() => expect(panel.getProjectAccess).toHaveBeenCalledWith("CORE"));
    expect(panel.whoAmI).not.toHaveBeenCalled();
  } finally {
    panel.restore();
  }
});

// The Slack channels the project's agents may post to are the owner's to name: the field shows
// the stored list, a save sends the whole list, and a refusal shows beside the field.
test("the owner names the project's Slack channels, and the field shows the stored list", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: access({ slack_channels: ["C012345678"] }) });
  const patch = spyOn(api, "patchProjectAccess").mockImplementation(
    panel.lands(access({ slack_channels: ["C012345678", "GABCDEFGH1"] }))
  );
  try {
    const field = await screen.findByLabelText<HTMLInputElement>("Channel ids");
    expect(field.value).toBe("C012345678");
    await user.clear(field);
    await user.type(field, "C012345678, GABCDEFGH1");
    await user.click(screen.getByRole("button", { name: "Save channels" }));
    await waitFor(() =>
      expect(patch).toHaveBeenCalledWith(
        "CORE",
        { slack_channels: ["C012345678", "GABCDEFGH1"] },
        keyedAttempt
      )
    );
    await waitFor(() =>
      expect(screen.getByLabelText<HTMLInputElement>("Channel ids").value).toBe(
        "C012345678 GABCDEFGH1"
      )
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    panel.restore();
    patch.mockRestore();
  }
});

// When the list last changed shows beside it, so the owner can read when the secrets broker's
// settle time for these channels started; a list never written shows no time.
test("the time the Slack channels last changed shows beside them, and nothing while it is null", async () => {
  const panel = renderPanel({
    answer: access({
      slack_channels: ["C012345678"],
      slack_channels_changed_at: "2026-11-17T10:00:00Z",
    }),
  });
  try {
    const changed = await screen.findByTestId("access-slack-channels-changed");
    expect(changed.textContent).toStartWith("Changed ");
    expect(changed.querySelector("time")).toHaveAttribute("datetime", "2026-11-17T10:00:00Z");
  } finally {
    panel.restore();
  }
  const never = renderPanel();
  try {
    await screen.findByLabelText("Channel ids");
    expect(screen.queryByTestId("access-slack-channels-changed")).not.toBeInTheDocument();
  } finally {
    never.restore();
  }
});

// A list the owner emptied changed too: the secrets broker stops releasing the token for the
// channels taken off from that time, so the tab shows it beside the empty field, as it does beside
// a list. The break it catches: the time shown only beside a list that names a channel.
test("the time the Slack channels last changed shows beside a list the owner emptied", async () => {
  const panel = renderPanel({
    answer: access({ slack_channels: [], slack_channels_changed_at: "2026-11-17T10:00:00Z" }),
  });
  try {
    const changed = await screen.findByTestId("access-slack-channels-changed");
    expect(changed.textContent).toStartWith("Changed ");
    expect(changed.querySelector("time")).toHaveAttribute("datetime", "2026-11-17T10:00:00Z");
  } finally {
    panel.restore();
  }
});

test("a person who is not the owner cannot name the Slack channels", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: access({ slack_channels: ["C012345678"] }), login: "bob" });
  const patch = spyOn(api, "patchProjectAccess");
  try {
    const field = await screen.findByLabelText("Channel ids");
    expect(field).not.toBeEnabled();
    expect(field).toHaveAttribute("title", notOwner);
    const save = screen.getByRole("button", { name: "Save channels" });
    expect(save).toHaveAttribute("title", notOwner);
    await user.click(save);
    expect(patch).not.toHaveBeenCalled();
  } finally {
    panel.restore();
    patch.mockRestore();
  }
});

test("a refused Slack channel shows its reason beside the field, and the draft stays", async () => {
  const user = userEvent.setup();
  const panel = renderPanel();
  const patch = spyOn(api, "patchProjectAccess").mockRejectedValue(
    new ApiError(400, {
      code: "INVALID_SLACK_CHANNEL",
      error: 'a Slack channel is named by its id, such as C0123456789: "#general"',
    })
  );
  try {
    const field = await screen.findByLabelText<HTMLInputElement>("Channel ids");
    await user.type(field, "#general");
    await user.click(screen.getByRole("button", { name: "Save channels" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("#general"));
    expect(field.value).toBe("#general");
  } finally {
    panel.restore();
    patch.mockRestore();
  }
});

// A person the list names whom nobody has signed in as yet is shown so: the rule admits them once
// they sign in, and until then the owner should know the name has not been used.
test("a person nobody has signed in as yet is shown as not signed in yet", async () => {
  const panel = renderPanel({
    answer: access({
      not_signed_in: ["newcomer@example.com"],
      people: ["carol@example.com", "newcomer@example.com"],
    }),
  });
  try {
    const people = await screen.findByRole("list", { name: "People" });
    const rows = within(people).getAllByRole("listitem");
    expect(rows[1]).toHaveTextContent("newcomer@example.comnot signed in yet");
    expect(rows[0]).not.toHaveTextContent("not signed in yet");
  } finally {
    panel.restore();
  }
});

// The owner sets the project private with the switch, and the switch and the label beside it
// follow Dispatch's answer rather than the press.
test("the owner makes the project private with the switch, and the switch reads the answer", async () => {
  const user = userEvent.setup();
  const panel = renderPanel();
  const patch = spyOn(api, "patchProjectAccess").mockImplementation(
    panel.lands(access({ visibility: "private" }))
  );
  try {
    const toggle = await screen.findByRole("switch", { name: "Private" });
    await user.click(toggle);
    await waitFor(() =>
      expect(patch).toHaveBeenCalledWith("CORE", { visibility: "private" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Private" })).toHaveAttribute(
        "aria-checked",
        "true"
      )
    );
    expect(screen.getByTestId("access-visibility")).toHaveTextContent("Private");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    panel.restore();
    patch.mockRestore();
  }
});

// A switch to public shows the project and everything in it to every full member, so it is sent
// only once the owner confirms it, as giving the project away is: a declined confirmation sends
// nothing and the switch stays private. The switch to private shows nobody more and asks nothing.
test("the owner makes a private project public only once they confirm it", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ answer: access({ visibility: "private" }) });
  const confirm = spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValueOnce(true);
  const patch = spyOn(api, "patchProjectAccess")
    .mockImplementationOnce(panel.lands(access({ visibility: "public" })))
    .mockImplementationOnce(panel.lands(access({ visibility: "private" })));
  try {
    const toggle = await screen.findByRole("switch", { name: "Private" });
    expect(toggle).toHaveAttribute("aria-checked", "true");
    // Declined: nothing is sent, and the project stays private.
    await user.click(toggle);
    expect(confirm).toHaveBeenCalledWith(
      "Make CORE public? Every full member can then see it and everything in it."
    );
    await settle();
    expect(patch).not.toHaveBeenCalled();
    expect(toggle).toHaveAttribute("aria-checked", "true");
    expect(screen.getByTestId("access-visibility")).toHaveTextContent("Private");

    await user.click(toggle);
    await waitFor(() =>
      expect(patch).toHaveBeenCalledWith("CORE", { visibility: "public" }, keyedAttempt)
    );
    await waitFor(() => expect(toggle).toHaveAttribute("aria-checked", "false"));
    expect(screen.getByTestId("access-visibility")).toHaveTextContent("Public");

    // Back to private: no confirmation is asked.
    await user.click(toggle);
    await waitFor(() =>
      expect(patch).toHaveBeenLastCalledWith("CORE", { visibility: "private" }, keyedAttempt)
    );
    expect(confirm).toHaveBeenCalledTimes(2);
  } finally {
    panel.restore();
    confirm.mockRestore();
    patch.mockRestore();
  }
});

/** Whether `text` is drawn anywhere on the page: in its text, or in an attribute a reader meets,
 *  such as a title's tooltip or an accessible name. */
function drawn(text: string): boolean {
  if (document.body.textContent?.includes(text)) {
    return true;
  }
  return Array.from(document.body.querySelectorAll("*")).some((element) =>
    Array.from(element.attributes).some((attribute) => attribute.value.includes(text))
  );
}

// Every refusal the switch's route answers, with the body Dispatch answers it with. Dispatch's
// words are written for the deployment's operator: a table, an envoy-dispatch command, a setting,
// the hub's bucket. They stay in the server's log and record. The owner who pressed the switch
// reads one sentence of the dashboard's per code instead, which says what they can do, and none
// of Dispatch's text is drawn anywhere on the page. None offers Retry: every answer stays the same
// until someone changes the deployment, the store or the project. That holds for
// PRIVATE_PROJECTS_UNAVAILABLE too, which this release answers every switch to private that passes
// the other checks, since it has no writer of the hub's private-projects record. A code the
// dashboard does not know gets a sentence that names nothing of Dispatch's. The switch stays
// public through each.
for (const { code, status, server, shown } of [
  {
    code: "PRIVATE_PROJECTS_OFF",
    status: 409,
    server: "this deployment cannot make a project private yet: nothing was changed",
    shown:
      "This deployment cannot make a project private yet: ask its operator to set up private projects.",
  },
  {
    code: "PRIVATE_PROJECT_NOT_PROVISIONED",
    status: 409,
    server:
      "CORE is not one of the projects DISPATCH_PRIVATE_PROJECTS names; a deployment names a project there once it has provisioned that project's private namespace, stores and grants (the infrastructure repository's privateprojects.Projects())",
    shown: "This deployment has not set up CORE to be private: ask its operator to set it up.",
  },
  {
    code: "PRIVATE_PROJECTS_INCOMPLETE",
    status: 409,
    server:
      "the project stays public until the store can hold it private: access_events_scrubbed holds no row, so project.access_changed events stored before this release may still carry a project's access list: run envoy-dispatch scrub-access-events",
    shown:
      "This deployment is not ready to keep CORE private yet: ask its operator to finish setting up private projects.",
  },
  {
    code: "PRIVATE_PROJECTS_UNAVAILABLE",
    status: 409,
    server:
      "the project stays public: every listener holds a private project's messages back by its key in the hub's envoy_private_projects bucket, and that key could not be written: this deployment has no bus to write it to",
    shown:
      "This deployment cannot keep CORE's messages from other people yet, so CORE stays public: ask its operator to set up private projects.",
  },
  {
    code: "PROJECT_OWNER_REQUIRED",
    status: 403,
    server: "only the project's owner changes who may see it: ask its owner, bob",
    shown: "Only CORE's owner can change who may see it.",
  },
  {
    code: "PROJECT_NOT_FOUND",
    status: 404,
    server: "no project has this key: CORE",
    shown: "You can no longer see CORE, so you cannot change who may see it.",
  },
  {
    code: "PROJECT_ACCESS_INPUT",
    status: 400,
    server: 'a project is public or private, not "secret"',
    shown: "Dispatch did not take this change: reload the page and press the switch again.",
  },
  {
    code: "A_REFUSAL_THIS_DASHBOARD_DOES_NOT_KNOW",
    status: 409,
    server: "the owner lock dispatch-owner:project:CORE was not taken within 60s",
    shown:
      "Dispatch did not make CORE private: if this keeps happening, ask the deployment's operator.",
  },
] as const) {
  test(`a switch refused ${code} tells the owner what to do, offers no Retry, draws none of Dispatch's words, and stays public`, async () => {
    const user = userEvent.setup();
    const panel = renderPanel();
    const patch = spyOn(api, "patchProjectAccess").mockRejectedValue(
      new ApiError(status, { code, error: server })
    );
    try {
      await user.click(await screen.findByRole("switch", { name: "Private" }));
      const alert = await within(screen.getByRole("region", { name: "Visibility" })).findByRole(
        "alert"
      );
      expect(within(alert).getByText(shown)).toBeInTheDocument();
      expect(drawn(server)).toBe(false);
      expect(screen.getByRole("switch", { name: "Private" })).toHaveAttribute(
        "aria-checked",
        "false"
      );
      expect(screen.getByTestId("access-visibility")).toHaveTextContent("Public");
      expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
      expect(patch).toHaveBeenCalledTimes(1);
    } finally {
      panel.restore();
      patch.mockRestore();
    }
  });
}

// A keyboard reader who presses the switch keeps their place through its write and the refusal
// that answers it, so their next key reaches the switch rather than the page's body. While the
// write is out the switch says it cannot be pressed, and a press of it sends nothing.
test("a keyboard press of the switch keeps the focus through its write and refusal, and Enter then presses it again", async () => {
  const user = userEvent.setup();
  const panel = renderPanel();
  const refused = Promise.withResolvers<ProjectAccess>();
  const patch = spyOn(api, "patchProjectAccess")
    .mockReturnValueOnce(refused.promise)
    .mockImplementationOnce(panel.lands(access({ visibility: "private" })));
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const toggle = await screen.findByRole("switch", { name: "Private" });
    act(() => toggle.focus());
    await user.keyboard(" ");
    await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
    // The write is out: the switch says it cannot be pressed, either way a control can say so,
    // and keeps the focus.
    await waitFor(() => expect(toggle.matches(":disabled, [aria-disabled='true']")).toBe(true));
    expect(toggle).toHaveFocus();
    // A press now is swallowed, not queued behind the write that is out: once that write is
    // answered, it is still the only one sent.
    await user.keyboard(" ");

    refused.reject(
      new ApiError(409, {
        code: "PRIVATE_PROJECTS_OFF",
        error: "this deployment cannot make a project private yet: nothing was changed",
      })
    );
    await waitFor(() => expect(toggle).not.toHaveAttribute("aria-disabled"));
    expect(patch).toHaveBeenCalledTimes(1);
    await within(screen.getByRole("region", { name: "Visibility" })).findByRole("alert");
    expect(toggle).toHaveFocus();

    await user.keyboard("{Enter}");
    await waitFor(() => expect(patch).toHaveBeenCalledTimes(2));
    expect(patch).toHaveBeenLastCalledWith("CORE", { visibility: "private" }, keyedAttempt);
    await waitFor(() => expect(toggle).toHaveAttribute("aria-checked", "true"));
  } finally {
    stopFocusFixup();
    panel.restore();
    patch.mockRestore();
  }
});

test("a person who is not the owner cannot press the switch", async () => {
  const user = userEvent.setup();
  const panel = renderPanel({ login: "bob" });
  const patch = spyOn(api, "patchProjectAccess");
  try {
    const toggle = await screen.findByRole("switch", { name: "Private" });
    expect(toggle).toHaveAttribute("aria-disabled", "true");
    expect(toggle).toHaveAttribute("title", notOwner);
    await user.click(toggle);
    expect(patch).not.toHaveBeenCalled();
  } finally {
    panel.restore();
    patch.mockRestore();
  }
});
