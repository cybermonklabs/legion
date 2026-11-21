import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { pagedGroups } from "../../__tests__/paged-groups";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { AccessGroup } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { GroupsSection } from "./GroupsSection";

const crew: AccessGroup = {
  members: ["carol@example.com", "dave@example.com"],
  name: "crew",
  not_signed_in: [],
  owner: "alice",
};
const ops: AccessGroup = {
  members: ["erin@example.com"],
  name: "ops",
  not_signed_in: [],
  owner: "bob",
};

/** Settings → Groups signed in as `login`, through the identity the shell already read. The group
 *  list answers what the writes left, as Dispatch does: a make or a write puts its group in, a
 *  delete takes it out. */
function renderSection({
  groups = [crew, ops],
  login = "alice",
}: {
  groups?: AccessGroup[];
  login?: string;
} = {}) {
  let stored = groups;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login });
  const spies = {
    deleteAccessGroup: spyOn(api, "deleteAccessGroup").mockImplementation(async (name) => {
      stored = stored.filter((group) => group.name !== name);
    }),
    listAccessGroups: spyOn(api, "listAccessGroups").mockImplementation(async () => stored),
    putAccessGroup: spyOn(api, "putAccessGroup").mockImplementation(async (name, input) => {
      const written: AccessGroup = {
        members: [...(input.members ?? [])].sort(),
        not_signed_in: [],
        name,
        owner: stored.find((group) => group.name === name)?.owner ?? login,
      };
      stored = [...stored.filter((group) => group.name !== name), written].sort((a, b) =>
        a.name.localeCompare(b.name)
      );
      return written;
    }),
  };
  const view = render(
    <QueryClientProvider client={queryClient}>
      <GroupsSection />
    </QueryClientProvider>
  );
  return {
    ...spies,
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

/** One group's row of the table, found by its name cell. */
function row(name: string): HTMLElement {
  const cell = screen.getByRole("cell", { name });
  const found = cell.closest("tr");
  if (found === null) throw new Error(`no row holds ${name}`);
  return found;
}

test("lists every group with its owner and its members, and says so when there is none", async () => {
  const listed = renderSection();
  try {
    expect(await screen.findByRole("heading", { name: "Groups" })).toBeInTheDocument();
    await screen.findByRole("cell", { name: "crew" });
    expect(within(row("crew")).getByText("alice (you)")).toBeInTheDocument();
    expect(
      within(row("crew")).getByText("carol@example.com, dave@example.com")
    ).toBeInTheDocument();
    expect(within(row("ops")).getByText("bob")).toBeInTheDocument();
    expect(within(row("ops")).getByText("erin@example.com")).toBeInTheDocument();
  } finally {
    listed.restore();
  }

  const empty = renderSection({ groups: [] });
  try {
    expect(await screen.findByText("No groups yet.")).toBeInTheDocument();
  } finally {
    empty.restore();
  }
});

// The group list answers a page at a time, and the section lists every group of every page.
test("lists the groups of every page the group list answers", async () => {
  const pages = pagedGroups([[crew], [ops]]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <GroupsSection />
    </QueryClientProvider>
  );
  try {
    expect(await screen.findByRole("cell", { name: "ops" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "crew" })).toBeInTheDocument();
    expect(pages.mock.calls).toEqual([[undefined], ["crew"]]);
  } finally {
    view.unmount();
    queryClient.clear();
    pages.mockRestore();
  }
});

// The Access tab links here as `/settings#groups`. A router navigation scrolls to no fragment, so
// the section brings itself into view once its list is drawn, as the link promised.
test("an address naming #groups brings the section into view once its list is drawn", async () => {
  window.history.replaceState(null, "", "/settings#groups");
  const scrolled = spyOn(HTMLElement.prototype, "scrollIntoView").mockImplementation(() => {});
  const section = renderSection();
  try {
    await screen.findByRole("cell", { name: "crew" });
    await waitFor(() => expect(scrolled).toHaveBeenCalledTimes(1));
    expect(scrolled.mock.contexts[0]).toBe(document.getElementById("groups"));
  } finally {
    section.restore();
    scrolled.mockRestore();
    window.history.replaceState(null, "", "/");
  }
});

test("a group the viewer does not own has Edit and Delete disabled, with its owner named as the reason", async () => {
  const user = userEvent.setup();
  const section = renderSection();
  try {
    await screen.findByRole("cell", { name: "ops" });
    const reason = "Only bob, the group's owner, changes it.";
    for (const name of ["Edit ops", "Delete ops"]) {
      const button = screen.getByRole("button", { name });
      expect(button).toHaveAttribute("aria-disabled", "true");
      expect(button).toHaveAttribute("title", reason);
    }
    await user.click(screen.getByRole("button", { name: "Delete ops" }));
    await user.click(screen.getByRole("button", { name: "Edit ops" }));
    expect(screen.queryByLabelText("Members of ops")).not.toBeInTheDocument();
    expect(section.deleteAccessGroup).not.toHaveBeenCalled();
    // The viewer's own group is theirs to change.
    expect(screen.getByRole("button", { name: "Edit crew" })).not.toHaveAttribute("aria-disabled");
    expect(screen.getByRole("button", { name: "Delete crew" })).not.toHaveAttribute("title");
  } finally {
    section.restore();
  }
});

test("the owner edits a group's members, and deletes it once they confirm", async () => {
  const user = userEvent.setup();
  const section = renderSection();
  const confirm = spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValueOnce(true);
  try {
    await user.click(await screen.findByRole("button", { name: "Edit crew" }));
    const members = screen.getByLabelText<HTMLTextAreaElement>("Members of crew");
    expect(members.value).toBe("carol@example.com\ndave@example.com");
    await user.clear(members);
    await user.type(members, "dave@example.com, frank@example.com");
    await user.click(screen.getByRole("button", { name: "Save crew" }));
    await waitFor(() =>
      expect(
        within(row("crew")).getByText("dave@example.com, frank@example.com")
      ).toBeInTheDocument()
    );
    expect(section.putAccessGroup).toHaveBeenCalledWith(
      "crew",
      { members: ["dave@example.com", "frank@example.com"] },
      keyedAttempt
    );
    expect(screen.queryByLabelText("Members of crew")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Delete crew" }));
    expect(confirm).toHaveBeenLastCalledWith(
      "Delete group crew? Every project's access list that names it loses it."
    );
    expect(section.deleteAccessGroup).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Delete crew" }));
    await waitFor(() =>
      expect(screen.queryByRole("cell", { name: "crew" })).not.toBeInTheDocument()
    );
    expect(section.deleteAccessGroup).toHaveBeenCalledWith("crew", keyedAttempt);
  } finally {
    section.restore();
    confirm.mockRestore();
  }
});

test("makes a group from its name and members, and empties the form once Dispatch made it", async () => {
  const user = userEvent.setup();
  const section = renderSection();
  try {
    const name = await screen.findByLabelText<HTMLInputElement>("Group name");
    await user.type(name, "Infra");
    // A name is lowercase: what is typed is written as it will be stored.
    expect(name.value).toBe("infra");
    await user.type(
      screen.getByLabelText("Members"),
      "grace@example.com\nheidi@example.com, grace@example.com"
    );
    await user.click(screen.getByRole("button", { name: "New group" }));
    await screen.findByRole("cell", { name: "infra" });
    expect(section.putAccessGroup).toHaveBeenCalledWith(
      "infra",
      { members: ["grace@example.com", "heidi@example.com"] },
      keyedAttempt
    );
    expect(within(row("infra")).getByText("alice (you)")).toBeInTheDocument();
    expect(name.value).toBe("");
    expect(screen.getByLabelText<HTMLTextAreaElement>("Members").value).toBe("");
  } finally {
    section.restore();
  }
});

test("a refused make shows the server's own words, keeps the form, and Retry sends it again", async () => {
  const user = userEvent.setup();
  const section = renderSection();
  const refusal =
    "a group holds full members only, and this person is a restricted member: ivan@outside.example";
  section.putAccessGroup.mockRejectedValueOnce(
    new ApiError(400, { code: "GROUP_MEMBER_RESTRICTED", error: refusal })
  );
  try {
    const name = await screen.findByLabelText<HTMLInputElement>("Group name");
    await user.type(name, "vendors");
    await user.type(screen.getByLabelText("Members"), "ivan@outside.example");
    await user.click(screen.getByRole("button", { name: "New group" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(refusal);
    expect(name.value).toBe("vendors");

    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await screen.findByRole("cell", { name: "vendors" });
    expect(section.putAccessGroup).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    section.restore();
  }
});

// A make the reader stopped may have made the group: the form says so and what its Retry does,
// not that it failed (#3000's rule for a keyed write the reader stops). Control: without it the
// form says "Couldn't make group vendors."
test("a make the reader stopped says the group may have been made and what Retry does, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const section = renderSection();
  section.putAccessGroup.mockRejectedValue(
    new NetworkError("/api/v1/access/groups/vendors", new TypeError("Failed to fetch"))
  );
  try {
    await user.type(await screen.findByLabelText<HTMLInputElement>("Group name"), "vendors");
    await user.type(screen.getByLabelText("Members"), "grace@example.com");
    await user.click(screen.getByRole("button", { name: "New group" }));
    await stopTheLateWrite(section.queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the new group vendors", "made", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    section.restore();
  }
});

// A row's save the reader stopped may have landed too: the row says so beneath the group, not
// that it failed. Control: without it the row says "Couldn't save the members of crew."
test("a members save the reader stopped says it may have been saved and what Retry does, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const section = renderSection();
  section.putAccessGroup.mockRejectedValue(
    new NetworkError("/api/v1/access/groups/crew", new TypeError("Failed to fetch"))
  );
  try {
    await user.click(await screen.findByRole("button", { name: "Edit crew" }));
    await user.type(
      screen.getByLabelText<HTMLTextAreaElement>("Members of crew"),
      ", frank@example.com"
    );
    await user.click(screen.getByRole("button", { name: "Save crew" }));
    await stopTheLateWrite(section.queryClient);

    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the members of crew", "saved", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    section.restore();
  }
});

// A make is a PUT, which writes a group of that name again when one exists: the form refuses a
// listed name rather than replace that group's members, and says where it is edited.
test("a name the list holds already is refused before anything is sent", async () => {
  const user = userEvent.setup();
  const section = renderSection();
  try {
    await user.type(await screen.findByLabelText("Group name"), "crew");
    await user.type(screen.getByLabelText("Members"), "zoe@example.com");
    await user.click(screen.getByRole("button", { name: "New group" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "A group named crew exists already. Its owner edits it in its row."
    );
    expect(section.putAccessGroup).not.toHaveBeenCalled();
  } finally {
    section.restore();
  }
});
