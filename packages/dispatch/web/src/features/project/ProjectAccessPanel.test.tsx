import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock, settle } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { AccessGroup, ProjectAccess } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
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
}: {
  answer?: ProjectAccess;
  groups?: AccessGroup[];
  login?: string;
} = {}) {
  let stored = answer;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login });
  const spies = {
    getProjectAccess: spyOn(api, "getProjectAccess").mockImplementation(async () => stored),
    listAccessGroups: spyOn(api, "listAccessGroups").mockResolvedValue(groups),
    whoAmI: spyOn(api, "whoAmI"),
  };
  const mount = () =>
    render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <ProjectAccessPanel project="CORE" />
        </QueryClientProvider>
      </MemoryRouter>
    );
  let view = mount();
  return {
    ...spies,
    lands: (next: ProjectAccess | Promise<ProjectAccess>) => async (): Promise<ProjectAccess> => {
      stored = await next;
      return stored;
    },
    queryClient,
    /** Closes the panel and opens it again on the same query client, as leaving the tab and
     *  coming back does: the mutation cache keeps its saves, and the new panel's hook holds none
     *  of the presses the old one made. */
    remount: () => {
      view.unmount();
      view = mount();
    },
    restore: () => {
      view.unmount();
      queryClient.clear();
      for (const spy of Object.values(spies)) {
        spy.mockRestore();
      }
    },
  };
}

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
    // Visibility is read-only in this release: it says what it is and offers no switch.
    expect(screen.getByTestId("access-visibility")).toHaveTextContent("Public");
    expect(screen.queryByRole("button", { name: /private/i })).not.toBeInTheDocument();
    expect(named("People")).toEqual(["carol@example.com"]);
    expect(named("Groups")).toEqual(["crew"]);
    // The picker offers the groups the list does not name already.
    const picker = screen.getByLabelText<HTMLSelectElement>("Add a group");
    expect(Array.from(picker.options).map((option) => option.value)).toEqual(["", "ops"]);
  } finally {
    panel.restore();
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
