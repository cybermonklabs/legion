import { spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { AccessGroup, InvitationAnswer, ProjectAccess } from "../../api/types";
import { ProjectAccessPanel } from "./ProjectAccessPanel";

/** CORE's access as the server answers it: owned by alice, naming carol and the group crew. */
export function access(overrides: Partial<ProjectAccess> = {}): ProjectAccess {
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

export const crew: AccessGroup = {
  members: ["carol@example.com"],
  name: "crew",
  not_signed_in: [],
  owner: "alice",
};
export const ops: AccessGroup = {
  members: ["dave@example.com"],
  name: "ops",
  not_signed_in: [],
  owner: "bob",
};

/** The panel for CORE, signed in as `login` through the identity the shell already read: the
 *  query client holds `/auth/whoami`'s answer fresh, as the shell's leaves it, so a panel that
 *  asked for the identity again would show in `whoAmI`. The client is the app's own
 *  (`DispatchQueryClient`, `main.tsx`), whose `refetchQueries` waits for a running read rather
 *  than cancelling it; `client: "tanstack"` renders under TanStack's, which cancels it. The
 *  access read answers what the last write left, as Dispatch does: `lands(next)` is a write that
 *  leaves `next` and answers it, and `invites(...links)` is the owner's Invite, each press
 *  answering the next of `links` and leaving its link in the answer's `open_invitations` in place
 *  of the person's earlier one. */
export function renderPanel({
  answer = access(),
  client = "dispatch",
  groups = [crew, ops],
  login = "alice",
  memberKind,
}: {
  answer?: ProjectAccess;
  client?: "dispatch" | "tanstack";
  groups?: AccessGroup[];
  login?: string;
  memberKind?: "restricted";
} = {}) {
  let stored = answer;
  const options = { defaultOptions: { queries: { retry: false, staleTime: 30_000 } } };
  const queryClient =
    client === "dispatch" ? new DispatchQueryClient(options) : new QueryClient(options);
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
    invites: (...links: InvitationAnswer[]) => {
      const invite = spyOn(api, "createProjectInvitation");
      const answer = (link: InvitationAnswer) => async (_project: string, email: string) => {
        if (stored.open_invitations !== undefined) {
          stored = {
            ...stored,
            open_invitations: [
              ...stored.open_invitations.filter((open) => open.email !== email),
              { email, expires_at: link.expires_at, made_at: link.made_at },
            ],
          };
        }
        return link;
      };
      for (const link of links) {
        invite.mockImplementationOnce(answer(link));
      }
      const last = links.at(-1);
      if (last !== undefined) {
        invite.mockImplementation(answer(last));
      }
      return invite;
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
export function named(list: string): (string | null | undefined)[] {
  return within(screen.getByRole("list", { name: list }))
    .getAllByRole("listitem")
    .map((item) => item.firstChild?.textContent);
}
