import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { settle } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { pagedGroups } from "../../__tests__/paged-groups";
import { ApiError, api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { AccessGroup, ProjectAccess } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { ProjectAccessPanel } from "./ProjectAccessPanel";

/** CORE's access as the server answers it: owned by alice, naming carol and the group crew. */
function access(overrides: Partial<ProjectAccess> = {}): ProjectAccess {
  return {
    groups: ["crew"],
    owner: "alice",
    not_signed_in: [],
    people: ["carol@example.com"],
    project: "CORE",
    slack_channels: [],
    slack_channels_changed_at: null,
    visibility: "public",
    ...overrides,
  };
}

const crew: AccessGroup = {
  members: ["carol@example.com"],
  name: "crew",
  not_signed_in: [],
  owner: "alice",
};
const ops: AccessGroup = {
  members: ["dave@example.com"],
  name: "ops",
  not_signed_in: [],
  owner: "bob",
};

const notOwner = "Only alice, the project's owner, changes who may see it.";

/** The panel for CORE, signed in as `login` through the identity the shell already read: the
 *  query client holds `/auth/whoami`'s answer fresh, as the shell's leaves it, so a panel that
 *  asked for the identity again would show in `whoAmI`. The access read answers what the last
 *  write left, as Dispatch does: `lands(next)` is a write that leaves `next` and answers it. */
function renderPanel({
  answer = access(),
  groups = [crew, ops],
  login = "alice",
  memberKind,
}: {
  answer?: ProjectAccess;
  groups?: AccessGroup[];
  login?: string;
  memberKind?: "restricted";
} = {}) {
  let stored = answer;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login,
    ...(memberKind === undefined ? {} : { member_kind: memberKind }),
  });
  const spies = {
    getProjectAccess: spyOn(api, "getProjectAccess").mockImplementation(async () => stored),
    listAccessGroups: spyOn(api, "listAccessGroups").mockResolvedValue(groups),
    whoAmI: spyOn(api, "whoAmI"),
  };
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ProjectAccessPanel project="CORE" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    ...spies,
    lands: (next: ProjectAccess | Promise<ProjectAccess>) => async (): Promise<ProjectAccess> => {
      stored = await next;
      return stored;
    },
    queryClient,
    restore: () => {
      view.unmount();
      queryClient.clear();
      for (const spy of Object.values(spies)) {
        spy.mockRestore();
      }
    },
  };
}

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

/** What a list names, one entry per row: each row's first child is its name, and its Remove
 *  button repeats the name for a screen reader only. */
function named(list: string): (string | null | undefined)[] {
  return within(screen.getByRole("list", { name: list }))
    .getAllByRole("listitem")
    .map((item) => item.firstChild?.textContent);
}

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

test("a refused save shows the server's own words beside its control, keeps the draft, and Retry sends it again", async () => {
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
// a hub or listener replica the switch's proof could not confirm, a lock. They stay in the
// server's log and record. The owner who pressed the switch reads one sentence of the dashboard's
// per code instead, which says what they can do, and none of Dispatch's text is drawn anywhere on
// the page. Retry is offered where pressing again can land with nothing else changed: the proof
// that every hub and listener holds the project's messages back failed just now
// (PRIVATE_PROJECTS_UNAVAILABLE), or another change of who may see a project held a lock the
// switch waited for (LOCK_TIMEOUT). Every other answer stays the same until someone changes the
// deployment, the store or the project. A code the dashboard does not know gets a sentence that
// names nothing of Dispatch's. The switch stays public through each.
for (const { code, status, server, shown, retry } of [
  {
    code: "PRIVATE_PROJECTS_OFF",
    status: 409,
    server: "this deployment cannot make a project private yet: nothing was changed",
    shown:
      "This deployment cannot make a project private yet: ask its operator to set up private projects.",
    retry: false,
  },
  {
    code: "PRIVATE_PROJECT_NOT_PROVISIONED",
    status: 409,
    server:
      "CORE is not one of the projects DISPATCH_PRIVATE_PROJECTS names; a deployment names a project there once it has provisioned that project's private namespace, stores and grants (the infrastructure repository's privateprojects.Projects())",
    shown: "This deployment has not set up CORE to be private: ask its operator to set it up.",
    retry: false,
  },
  {
    code: "PRIVATE_PROJECTS_INCOMPLETE",
    status: 409,
    server:
      "the project stays public until the store can hold it private: access_events_scrubbed holds no row, so project.access_changed events stored before this release may still carry a project's access list: run envoy-dispatch scrub-access-events",
    shown:
      "This deployment is not ready to keep CORE private yet: ask its operator to finish setting up private projects.",
    retry: false,
  },
  {
    code: "PRIVATE_PROJECTS_UNAVAILABLE",
    status: 409,
    server:
      "the project stays public: a switch to private first proves that every hub serving private projects admits Dispatch through its auth service and that every listener replica reads the private projects, and that proof failed: this deployment names no hub to prove (DISPATCH_PRIVATE_HUBS)",
    shown:
      "Dispatch could not confirm just now that CORE's messages would reach only the people who may see it, so CORE stays public: try again in a moment, and if this keeps happening, ask the deployment's operator.",
    retry: true,
  },
  {
    code: "LOCK_TIMEOUT",
    status: 409,
    server:
      "the owner-row lock was not taken in time: another write holds it; nothing was changed; try again once the write that holds it ends",
    shown:
      "Another change to who may see a project was still being saved, so Dispatch did not make CORE private: try again in a moment.",
    retry: true,
  },
  {
    code: "PROJECT_OWNER_REQUIRED",
    status: 403,
    server: "only the project's owner changes who may see it: ask its owner, bob",
    shown: "Only CORE's owner can change who may see it.",
    retry: false,
  },
  {
    code: "PROJECT_NOT_FOUND",
    status: 404,
    server: "no project has this key: CORE",
    shown: "You can no longer see CORE, so you cannot change who may see it.",
    retry: false,
  },
  {
    code: "PROJECT_ACCESS_INPUT",
    status: 400,
    server: 'a project is public or private, not "secret"',
    shown: "Dispatch did not take this change: reload the page and press the switch again.",
    retry: false,
  },
  {
    code: "A_REFUSAL_THIS_DASHBOARD_DOES_NOT_KNOW",
    status: 409,
    server: "the owner lock dispatch-owner:project:CORE was not taken within 60s",
    shown:
      "Dispatch did not make CORE private: if this keeps happening, ask the deployment's operator.",
    retry: false,
  },
] as const) {
  test(`a switch refused ${code} tells the owner what to do, offers ${retry ? "Retry" : "no Retry"}, draws none of Dispatch's words, and stays public`, async () => {
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
      if (retry) {
        expect(within(alert).getByRole("button", { name: "Retry" })).toBeInTheDocument();
      } else {
        expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
      }
      expect(patch).toHaveBeenCalledTimes(1);
    } finally {
      panel.restore();
      patch.mockRestore();
    }
  });
}

// The admission proof can fail for a moment - a hub restarting, a listener replica not answering
// yet - and pass on the next press with nothing changed, so its refusal's Retry sends the same
// switch again, and the project turns private once Dispatch takes it.
test("Retry after a switch refused PRIVATE_PROJECTS_UNAVAILABLE sends the switch again, and the project turns private", async () => {
  const user = userEvent.setup();
  const panel = renderPanel();
  const patch = spyOn(api, "patchProjectAccess")
    .mockRejectedValueOnce(
      new ApiError(409, {
        code: "PRIVATE_PROJECTS_UNAVAILABLE",
        error:
          "the project stays public: the listener replica dispatch-listener-1 did not answer a fresh read",
      })
    )
    .mockImplementationOnce(panel.lands(access({ visibility: "private" })));
  try {
    await user.click(await screen.findByRole("switch", { name: "Private" }));
    const alert = await within(screen.getByRole("region", { name: "Visibility" })).findByRole(
      "alert"
    );
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patch).toHaveBeenCalledTimes(2));
    expect(patch).toHaveBeenLastCalledWith("CORE", { visibility: "private" }, keyedAttempt);
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Private" })).toHaveAttribute(
        "aria-checked",
        "true"
      )
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    panel.restore();
    patch.mockRestore();
  }
});

// A Slack channel belongs to one private project, so a switch to private is refused while another
// private project lists one of this project's channels (409 SLACK_CHANNEL_SHARED). Dispatch's words
// for it are the owner's: they name the channel, which the owner listed, and the other project
// only to an owner who sees it. So the switch quotes them, and says what the owner can do, with no
// Retry: the answer stays the same until a project's channels change.
for (const { form, server } of [
  {
    form: "names the other project to an owner who sees it",
    server:
      "C0SHARED001 is listed by PRIV, a private project; a Slack channel belongs to one private project",
  },
  {
    form: "names no project to an owner who does not see it",
    server: "C0SHARED001 is listed by a private project you cannot see",
  },
] as const) {
  test(`a switch refused SLACK_CHANNEL_SHARED quotes Dispatch's words, which ${form}, offers no Retry, and stays public`, async () => {
    const user = userEvent.setup();
    const panel = renderPanel({ answer: access({ slack_channels: ["C0SHARED001"] }) });
    const patch = spyOn(api, "patchProjectAccess").mockRejectedValue(
      new ApiError(409, { code: "SLACK_CHANNEL_SHARED", error: server })
    );
    try {
      await user.click(await screen.findByRole("switch", { name: "Private" }));
      const alert = await within(screen.getByRole("region", { name: "Visibility" })).findByRole(
        "alert"
      );
      expect(
        within(alert).getByText(
          `CORE stays public: ${server}. Take that channel off CORE's Slack channels below, then make CORE private.`
        )
      ).toBeInTheDocument();
      expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
      expect(screen.getByRole("switch", { name: "Private" })).toHaveAttribute(
        "aria-checked",
        "false"
      );
      expect(screen.getByTestId("access-visibility")).toHaveTextContent("Public");
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
