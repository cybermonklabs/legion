import { afterEach, beforeAll, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the submit site below is a form submitted with no control pressed, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRequests } from "../../__tests__/network";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type {
  AgentToken,
  CreatedAgentToken,
  DispatchScope,
  DispatchTokenScope,
  Project,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { AgentTokensSection } from "./AgentTokensSection";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const token: AgentToken = {
  created_at: "2026-09-12T00:00:00Z",
  id: "11111111-1111-1111-1111-111111111111",
  last_used_at: null,
  name: "architect",
  prefix: "AbCdEf12",
  revoked_at: null,
  scopes: ["person", "approve"],
};

/** A project Dispatch lists to the reader. */
function project(key: string, name: string): Project {
  return { created_at: "2026-09-12T00:00:00Z", gated_steps: null, key, name, open_asks: 0 };
}

/** The projects the section offers a token, which every row reads: none unless a row lists its
 *  own. */
let listProjects: Mock<typeof api.listProjects>;
beforeEach(() => {
  listProjects = spyOn(api, "listProjects").mockResolvedValue([]);
});
afterEach(() => {
  listProjects.mockRestore();
});

test("Settings exposes a new personal token once, copies it, and revokes an existing token", async () => {
  const user = userEvent.setup();
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([token]);
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reviewer",
    token: "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG",
  });
  const revokeAgentToken = spyOn(api, "revokeAgentToken").mockResolvedValue();
  const originalConfirm = window.confirm;
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  window.confirm = () => true;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    expect(await screen.findByRole("cell", { name: "dsp_AbCdEf12…" })).toBeDefined();
    expect(screen.getByText("Never used")).toBeDefined();

    await user.click(screen.getByLabelText("Token label"));
    await user.paste("reviewer");
    await user.click(screen.getByRole("button", { name: "New token" }));

    const plaintext = "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG";
    const origin = window.location.origin;
    const config = JSON.stringify(
      { dispatch: { enabled: true, serverUrl: origin, token: plaintext } },
      null,
      2
    );
    expect(await screen.findByText(plaintext)).toBeDefined();
    const snippets = Array.from(document.querySelectorAll("pre code"));
    const configSnippet = snippets.find((snippet) => snippet.textContent?.includes('"dispatch"'));
    expect(configSnippet?.textContent).toBe(config);
    const paragraphs = Array.from(document.querySelectorAll("p"), (p) => p.textContent);
    expect(paragraphs).toContain("Put this in ~/.config/legion/envoy.json:");
    expect(paragraphs).toContain(
      "If the file exists, replace its dispatch section with this one and keep the rest. A session under a named omp profile, or with CLAUDE_CONFIG_DIR set, reads the envoy.json in that directory instead; any other session reads the file ENVOY_CONFIG_FILE names when it is set."
    );
    const envSnippet = snippets.find((snippet) => snippet.textContent?.includes("DISPATCH_URL="));
    expect(envSnippet?.textContent).toBe(`DISPATCH_URL=${origin}\nDISPATCH_TOKEN=${plaintext}`);

    await user.click(screen.getByRole("button", { name: "Copy token" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(plaintext));

    await user.click(screen.getByRole("button", { name: "Revoke architect" }));
    await waitFor(() => expect(revokeAgentToken).toHaveBeenCalledWith(token.id, keyedAttempt));
    await waitFor(() => expect(screen.getByText("Revoked")).toBeDefined());
    expect(screen.queryByRole("button", { name: "Revoke architect" })).not.toBeInTheDocument();
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    createAgentToken.mockRestore();
    revokeAgentToken.mockRestore();
    window.confirm = originalConfirm;
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
  }
});

test("Settings copies a new token through the legacy clipboard fallback", async () => {
  const user = userEvent.setup();
  const plaintext = "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG";
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reviewer",
    token: plaintext,
  });
  const originalClipboard = navigator.clipboard;
  const originalExecCommand = document.execCommand;
  const execCommand = spyOn({ execCommand: (_command: string) => true }, "execCommand");
  Object.defineProperty(document, "execCommand", { configurable: true, value: execCommand });
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );
    await user.click(screen.getByLabelText("Token label"));
    await user.paste("reviewer");
    await user.click(screen.getByRole("button", { name: "New token" }));
    await screen.findByText(plaintext);

    await user.click(screen.getByRole("button", { name: "Copy token" }));
    expect(await screen.findByText("Copied", { exact: true })).toBeDefined();
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    createAgentToken.mockRestore();
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
  }
});

/** The box labelled `label` for `subject`, as a checkbox the test can read and click. */
function box(label: string, subject: string): HTMLInputElement {
  return screen.getByRole("checkbox", { name: `${label} (${subject})` }) as HTMLInputElement;
}

// A token's two scopes are what it may do: act as its person, and settle what its person
// settles. The row shows the set the token holds and writes back the whole set, since the PATCH
// replaces it rather than adding to it; no token holds Approve without Act as you, so each box
// carries the other where the server would refuse the pair. A token that writes is offered no Read
// only box, and no PATCH sends `read`: no token that writes becomes read only.
test("a token's row shows the scopes it holds, and each change sends the whole set", async () => {
  const user = userEvent.setup();
  const personOnly: AgentToken = { ...token, scopes: ["person"] };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([personOnly]);
  const updateAgentToken = spyOn(api, "updateAgentToken").mockImplementation(
    async (id: string, input: { scopes: DispatchScope[] }) => ({
      ...personOnly,
      id,
      scopes: input.scopes,
    })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    await screen.findByRole("cell", { name: "dsp_AbCdEf12…" });
    expect(box("Act as you", "architect").checked).toBe(true);
    expect(box("Approve and control credentials", "architect").checked).toBe(false);

    // A scope write names the scopes alone, which leaves the token's projects as they are.
    await user.click(box("Approve and control credentials", "architect"));
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenCalledWith(
        personOnly.id,
        { scopes: ["person", "approve"] },
        keyedAttempt
      )
    );
    await waitFor(() =>
      expect(box("Approve and control credentials", "architect").checked).toBe(true)
    );
    expect(box("Act as you", "architect").checked).toBe(true);
    expect(screen.queryByRole("checkbox", { name: "Read only (architect)" })).toBeNull();

    await user.click(box("Act as you", "architect"));
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenLastCalledWith(personOnly.id, { scopes: [] }, keyedAttempt)
    );
    await waitFor(() => expect(box("Act as you", "architect").checked).toBe(false));
    expect(box("Approve and control credentials", "architect").checked).toBe(false);
    for (const call of updateAgentToken.mock.calls) expect(call[1]?.scopes).not.toContain("read");
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    updateAgentToken.mockRestore();
  }
});

/** The projects picker of `subject`'s token: its trigger, which reads the projects it names. Its
 *  accessible name holds the text it shows, after whose token it is (WCAG 2.5.3, which the
 *  accessibility pass's `label-content-name-mismatch` reads), so the helper checks both. */
function projectsPicker(subject: string): HTMLElement {
  const trigger = screen.getByRole("button", { name: new RegExp(`^Projects \\(${subject}\\): `) });
  expect(trigger.getAttribute("aria-label")).toBe(`Projects (${subject}): ${trigger.textContent}`);
  return trigger;
}

// A token that names no project uses the public projects only, and a private project needs a token
// that names it: the mint form names none until the reader picks some, sends the ones picked, and
// names none again for the next token.
test("the mint form names no project until the reader picks some, and sends the ones picked", async () => {
  const user = userEvent.setup();
  listProjects.mockResolvedValue([project("CORE", "Core"), project("PRIV", "Private work")]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reviewer",
    projects: ["PRIV"],
    token: "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG",
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    expect(projectsPicker("new token").textContent).toBe("Public projects only");
    await user.click(projectsPicker("new token"));
    await user.click(await screen.findByRole("option", { name: "PRIV, Private work" }));
    await user.keyboard("{Escape}");
    expect(projectsPicker("new token").textContent).toBe("PRIV");

    await user.click(screen.getByLabelText("Token label"));
    await user.paste("reviewer");
    await user.click(screen.getByRole("button", { name: "New token" }));
    await waitFor(() =>
      expect(createAgentToken).toHaveBeenCalledWith(
        { name: "reviewer", projects: ["PRIV"], scopes: ["person", "approve"] },
        keyedAttempt
      )
    );
    await screen.findByText("dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG");
    expect(projectsPicker("new token").textContent).toBe("Public projects only");
    expect(projectsPicker("reviewer").textContent).toBe("PRIV");
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    createAgentToken.mockRestore();
  }
});

// A row's picker reads the projects its token names and writes the whole list back with the
// scopes the token holds, since the edit names both. A token that also names a project its reader
// no longer sees arrives cut to the projects the reader sees and marked projects_hidden, as the
// list answers it (`GET /api/v1/me/agent-tokens`): the hidden project is never offered, the row
// says a change drops it, and the edit sends the list the reader sees, whose answer hides nothing.
// Taking every project off sends the empty list, which makes the token one of the public projects
// only.
test("a token's row edits the projects it names, sent with the scopes it holds", async () => {
  const user = userEvent.setup();
  listProjects.mockResolvedValue([project("CORE", "Core"), project("PRIV", "Private work")]);
  // As the list answers a token listing GONE and PRIV to a reader who no longer sees GONE.
  const naming: AgentToken = { ...token, projects: ["PRIV"], projects_hidden: true };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([naming]);
  const updateAgentToken = spyOn(api, "updateAgentToken").mockImplementation(
    async (id: string, input: { projects?: string[] | null; scopes: DispatchScope[] }) => {
      if (input.projects == null) {
        return { ...naming, id, scopes: input.scopes };
      }
      // The edit names only projects the reader sees, so its answer hides none.
      const { projects: _listed, projects_hidden: _hidden, ...rest } = naming;
      const projects = [...input.projects].sort();
      return projects.length === 0 ? { ...rest, id } : { ...rest, id, projects };
    }
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    await screen.findByRole("cell", { name: "dsp_AbCdEf12…" });
    expect(projectsPicker("architect").textContent).toBe("PRIV, and projects you cannot see");
    expect(screen.getByText("A change here drops the projects you cannot see.")).toBeDefined();
    await user.click(projectsPicker("architect"));
    expect(screen.queryByRole("option", { name: "GONE" })).toBeNull();
    expect(
      screen.getByRole("option", { name: "PRIV, Private work" }).getAttribute("aria-selected")
    ).toBe("true");
    await user.click(screen.getByRole("option", { name: "CORE, Core" }));
    // Nothing is written while the picker is open.
    expect(updateAgentToken).not.toHaveBeenCalled();
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenCalledWith(
        naming.id,
        { projects: ["PRIV", "CORE"], scopes: ["person", "approve"] },
        keyedAttempt
      )
    );
    await waitFor(() => expect(projectsPicker("architect").textContent).toBe("CORE, PRIV"));
    expect(screen.queryByText("A change here drops the projects you cannot see.")).toBeNull();

    // A picker closed with the list it opened with writes nothing.
    await user.click(projectsPicker("architect"));
    await user.keyboard("{Escape}");
    expect(updateAgentToken).toHaveBeenCalledTimes(1);

    await user.click(projectsPicker("architect"));
    await user.click(screen.getByRole("button", { name: "Clear projects" }));
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenLastCalledWith(
        naming.id,
        { projects: [], scopes: ["person", "approve"] },
        keyedAttempt
      )
    );
    await waitFor(() =>
      expect(projectsPicker("architect").textContent).toBe("Public projects only")
    );
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    updateAgentToken.mockRestore();
  }
});

// Dispatch cuts each token's list to the projects its reader sees, so a token whose every
// project is hidden from the reader arrives with no list, the same as one that lists none. An
// edit the reader made only to its scopes, and a picker opened and closed unchanged, must leave
// the stored list alone: a body that named `projects` would replace it with the empty list, and
// so widen the token to every public project.
test("a token's scope edit and an unchanged picker never send its projects", async () => {
  const user = userEvent.setup();
  listProjects.mockResolvedValue([project("CORE", "Core")]);
  // As the list answers a token listing PRIV to a reader who no longer sees PRIV.
  const cut: AgentToken = { ...token, projects_hidden: true, scopes: ["person"] };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([cut]);
  const updateAgentToken = spyOn(api, "updateAgentToken").mockImplementation(
    async (id: string, input: { scopes: DispatchScope[] }) => ({ ...cut, id, scopes: input.scopes })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    await screen.findByRole("cell", { name: "dsp_AbCdEf12…" });
    await user.click(box("Approve and control credentials", "architect"));
    await waitFor(() => expect(updateAgentToken).toHaveBeenCalledTimes(1));
    const [, body] = updateAgentToken.mock.calls[0] ?? [];
    expect(Object.keys(body ?? {})).toEqual(["scopes"]);

    await user.click(projectsPicker("architect"));
    await user.keyboard("{Escape}");
    await settle();
    expect(updateAgentToken).toHaveBeenCalledTimes(1);
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    updateAgentToken.mockRestore();
  }
});

// Dispatch tells the person's own browser when it cut a token's list (`projects_hidden`): the
// row then says the token lists projects the reader cannot see, never "Public projects only",
// and warns that a change of the list drops them.
test("a token listing projects its reader cannot see says so, and never reads as public only", async () => {
  listProjects.mockResolvedValue([project("CORE", "Core")]);
  const allHidden: AgentToken = { ...token, id: "hidden", name: "hidden", projects_hidden: true };
  const partHidden: AgentToken = {
    ...token,
    id: "part",
    name: "part",
    projects: ["CORE"],
    projects_hidden: true,
  };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([allHidden, partHidden]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    await screen.findAllByRole("cell", { name: "dsp_AbCdEf12…" });
    expect(projectsPicker("hidden").textContent).toBe("No project you can see");
    expect(projectsPicker("part").textContent).toBe("CORE, and projects you cannot see");
    expect(screen.getAllByText("A change here drops the projects you cannot see.")).toHaveLength(2);
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
  }
});

/** The row's button that saves the token's list without the projects its reader cannot see. */
function dropHidden(subject: string): HTMLElement {
  return screen.getByRole("button", { name: `Drop the projects you cannot see (${subject})` });
}

// A token whose every listed project is hidden from its reader can read nothing, and its
// picker opens with nothing ticked, so no change of it sends anything. The row's button does
// what the note says a change does: once the reader confirms it, it saves the list they see,
// here none, which makes the token one of the public projects only. A part-hidden token keeps
// what the reader sees. A declined confirmation sends nothing, and a token whose list is whole
// carries no such button.
test("a token listing projects its reader cannot see can drop them in one confirmed step", async () => {
  const user = userEvent.setup();
  listProjects.mockResolvedValue([project("CORE", "Core")]);
  const allHidden: AgentToken = { ...token, id: "hidden", name: "hidden", projects_hidden: true };
  const partHidden: AgentToken = {
    ...token,
    id: "part",
    name: "part",
    projects: ["CORE"],
    projects_hidden: true,
  };
  const whole: AgentToken = { ...token, id: "whole", name: "whole", projects: ["CORE"] };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([
    allHidden,
    partHidden,
    whole,
  ]);
  const updateAgentToken = spyOn(api, "updateAgentToken").mockImplementation(
    async (id: string, input: { projects?: string[] | null; scopes: DispatchScope[] }) => {
      const named = [allHidden, partHidden, whole].find((row) => row.id === id) ?? token;
      const { projects: _listed, projects_hidden: _hidden, ...rest } = named;
      const kept = input.projects ?? [];
      return kept.length === 0 ? { ...rest, scopes: input.scopes } : { ...rest, projects: kept };
    }
  );
  const asked: string[] = [];
  const answers = [false, true, true];
  const originalConfirm = window.confirm;
  window.confirm = (message?: string) => {
    asked.push(message ?? "");
    return answers.shift() ?? false;
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    await screen.findAllByRole("cell", { name: "dsp_AbCdEf12…" });
    expect(
      screen.queryByRole("button", { name: "Drop the projects you cannot see (whole)" })
    ).toBeNull();

    // Declined: nothing is sent.
    await user.click(dropHidden("hidden"));
    expect(asked[0]).toBe(
      "Drop the projects you cannot see from hidden? It then uses the public projects only, and a project you cannot see cannot be added back."
    );
    await settle();
    expect(updateAgentToken).not.toHaveBeenCalled();

    await user.click(dropHidden("hidden"));
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenCalledWith(
        "hidden",
        { projects: [], scopes: ["person", "approve"] },
        keyedAttempt
      )
    );
    await waitFor(() => expect(projectsPicker("hidden").textContent).toBe("Public projects only"));
    expect(
      screen.queryByRole("button", { name: "Drop the projects you cannot see (hidden)" })
    ).toBeNull();

    await user.click(dropHidden("part"));
    expect(asked[2]).toBe(
      "Drop the projects you cannot see from part? It then names CORE alone, and a project you cannot see cannot be added back."
    );
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenLastCalledWith(
        "part",
        { projects: ["CORE"], scopes: ["person", "approve"] },
        keyedAttempt
      )
    );
    await waitFor(() => expect(projectsPicker("part").textContent).toBe("CORE"));
    expect(updateAgentToken).toHaveBeenCalledTimes(2);
  } finally {
    cleanup();
    queryClient.clear();
    window.confirm = originalConfirm;
    listAgentTokens.mockRestore();
    updateAgentToken.mockRestore();
  }
});

// An edit that landed with its answer lost (`409 KEY_COMMITTED`) writes the row as the press did,
// and the page reads every query again (`Writes`); until the list answers, the row is the one the
// press wrote. An edit that names the projects names only ones the reader sees, so that row lists
// none they cannot see: it reads as the list it saved, and offers no drop. A scope edit leaves the
// list alone, and so its note and its button. The re-read here never answers, so the row read is
// the press's.
test("a token edit whose answer was lost reads as the list it saved", async () => {
  const user = userEvent.setup();
  listProjects.mockResolvedValue([project("CORE", "Core")]);
  const allHidden: AgentToken = { ...token, id: "hidden", name: "hidden", projects_hidden: true };
  const partHidden: AgentToken = {
    ...token,
    id: "part",
    name: "part",
    projects: ["CORE"],
    projects_hidden: true,
  };
  const listAgentTokens = spyOn(api, "listAgentTokens")
    .mockResolvedValueOnce([allHidden, partHidden])
    .mockImplementation(() => Promise.withResolvers<AgentToken[]>().promise);
  const updateAgentToken = spyOn(api, "updateAgentToken").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  const originalConfirm = window.confirm;
  window.confirm = () => true;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    await screen.findAllByRole("cell", { name: "dsp_AbCdEf12…" });
    await user.click(box("Approve and control credentials", "part"));
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenCalledWith("part", { scopes: ["person"] }, keyedAttempt)
    );
    await waitFor(() => expect(box("Approve and control credentials", "part").checked).toBe(false));
    await waitFor(() => expect(listAgentTokens.mock.calls.length).toBeGreaterThan(1));
    expect(projectsPicker("part").textContent).toBe("CORE, and projects you cannot see");
    expect(dropHidden("part")).toBeDefined();

    await user.click(dropHidden("hidden"));
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenLastCalledWith(
        "hidden",
        { projects: [], scopes: ["person", "approve"] },
        keyedAttempt
      )
    );
    await waitFor(() => expect(projectsPicker("hidden").textContent).toBe("Public projects only"));
    expect(
      screen.queryByRole("button", { name: "Drop the projects you cannot see (hidden)" })
    ).toBeNull();
    await waitFor(() => expect(listAgentTokens.mock.calls.length).toBeGreaterThan(2));
  } finally {
    cleanup();
    queryClient.clear();
    window.confirm = originalConfirm;
    listAgentTokens.mockRestore();
    updateAgentToken.mockRestore();
  }
});

// Dispatch holds a token to at most 16 projects (400 TOKEN_PROJECTS_INPUT): the picker takes no
// seventeenth, and says why.
test("a token's picker takes at most 16 projects", async () => {
  const user = userEvent.setup();
  const keys = Array.from({ length: 17 }, (_, index) => `P${String(index).padStart(2, "0")}`);
  listProjects.mockResolvedValue(keys.map((key) => project(key, key)));
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    await user.click(projectsPicker("new token"));
    for (const key of keys) {
      await user.click(await screen.findByRole("option", { name: `${key}, ${key}` }));
    }
    expect(screen.getByText("A token names at most 16 projects.")).toBeDefined();
    expect(screen.getByRole("option", { name: "P16, P16" }).getAttribute("aria-selected")).toBe(
      "false"
    );
    await user.keyboard("{Escape}");
    expect(projectsPicker("new token").textContent).toBe(keys.slice(0, 16).join(", "));
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
  }
});

// A new token holds both scopes unless the reader clears one, and the mint sends the set the
// boxes hold.
test("the mint form starts with both scopes and sends the set the reader chose", async () => {
  const user = userEvent.setup();
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reviewer",
    scopes: ["person"],
    token: "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG",
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    expect(box("Act as you", "new token").checked).toBe(true);
    expect(box("Approve and control credentials", "new token").checked).toBe(true);

    await user.click(box("Approve and control credentials", "new token"));
    expect(box("Approve and control credentials", "new token").checked).toBe(false);
    expect(box("Act as you", "new token").checked).toBe(true);

    await user.click(screen.getByLabelText("Token label"));
    await user.paste("reviewer");
    await user.click(screen.getByRole("button", { name: "New token" }));
    await waitFor(() =>
      expect(createAgentToken).toHaveBeenCalledWith(
        { name: "reviewer", scopes: ["person"] },
        keyedAttempt
      )
    );
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    createAgentToken.mockRestore();
  }
});

// A restricted member's token is their agent's: Dispatch gives it no scope and holds it to exactly
// one project they see (403 RESTRICTED_MEMBER for a mint or an edit asking otherwise). Their
// Settings offers no scope box, says what such a token may do, mints and edits sending no scope,
// and a second project picked replaces the first rather than adding to it.
//
// Control: draw the full member's form for them (useRestrictedMember answering false), and the
// row finds the scope boxes and the mint sends both scopes.
test("a restricted member's form and rows offer no scope, send none, and name one project", async () => {
  const user = userEvent.setup();
  listProjects.mockResolvedValue([project("ACC", "Access"), project("OTH", "Other")]);
  const theirs: AgentToken = { ...token, projects: ["ACC"], scopes: [] };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([theirs]);
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...theirs,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reviewer",
    projects: ["OTH"],
    token: "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG",
  });
  const updateAgentToken = spyOn(api, "updateAgentToken").mockImplementation(
    async (id: string, input: { projects?: string[] | null; scopes: DispatchScope[] }) => ({
      ...theirs,
      id,
      projects: input.projects ?? theirs.projects,
      scopes: input.scopes,
    })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "erin@outside.example",
    member_kind: "restricted",
  });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    await screen.findByRole("cell", { name: "dsp_AbCdEf12…" });
    // Their browser holds every scope, so whoami names p0 for them too; still no box is drawn.
    await waitFor(() =>
      expect(queryClient.getQueryData<string[]>(["whoami-scopes"])).toContain("p0")
    );
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(
      screen.getAllByText(
        "Works as you in the one project it names, and approves nothing: no login, no token, no credential request."
      )
    ).toHaveLength(2);
    expect(projectsPicker("new token").textContent).toBe("Your one project");

    await user.click(projectsPicker("new token"));
    await user.click(await screen.findByRole("option", { name: "ACC, Access" }));
    await user.click(screen.getByRole("option", { name: "OTH, Other" }));
    await user.keyboard("{Escape}");
    expect(projectsPicker("new token").textContent).toBe("OTH");
    await user.click(screen.getByLabelText("Token label"));
    await user.paste("reviewer");
    await user.click(screen.getByRole("button", { name: "New token" }));
    await waitFor(() =>
      expect(createAgentToken).toHaveBeenCalledWith(
        { name: "reviewer", projects: ["OTH"], scopes: [] },
        keyedAttempt
      )
    );

    await user.click(projectsPicker("architect"));
    await user.click(await screen.findByRole("option", { name: "OTH, Other" }));
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenCalledWith(
        theirs.id,
        { projects: ["OTH"], scopes: [] },
        keyedAttempt
      )
    );
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    createAgentToken.mockRestore();
    updateAgentToken.mockRestore();
  }
});

// A read-only token a restricted member made while they were a full member lists no project, and
// Dispatch takes no edit of it, so it acts for nobody until they revoke it and make a token naming
// their one project. The token list answers it holding `read` alone. Its row says that, with no
// project picker, while the row of their other token keeps its note and its picker.
//
// Control: draw the restricted row's usual note whatever the token holds (drop the readOnly branch
// of its scopes cell), and the row shows no word of what to do.
test("a restricted member's read-only token's row says to replace it, and offers no picker", async () => {
  listProjects.mockResolvedValue([project("ACC", "Access")]);
  const reader: AgentToken = {
    ...token,
    id: "33333333-3333-3333-3333-333333333333",
    name: "reader",
    scopes: ["read"],
  };
  const theirs: AgentToken = { ...token, projects: ["ACC"], scopes: [] };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([reader, theirs]);
  const updateAgentToken = spyOn(api, "updateAgentToken");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "erin@outside.example",
    member_kind: "restricted",
  });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    const name = await screen.findByRole("cell", { name: "reader" });
    const row = name.closest("tr") as HTMLElement;
    expect(
      within(row).getByText(
        "Read only, made before your access changed: it works nowhere now. Revoke it, then make a token that names your one project."
      )
    ).toBeInTheDocument();
    expect(within(row).getByText("No project")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Projects \(reader\)/ })).toBeNull();
    expect(within(row).getByRole("button", { name: "Revoke reader" })).toBeEnabled();
    expect(projectsPicker("architect").textContent).toBe("ACC");
    expect(updateAgentToken).not.toHaveBeenCalled();
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    updateAgentToken.mockRestore();
  }
});

/** The label of each word's box on the mint form. */
const LABELS: Readonly<Record<DispatchTokenScope, string>> = {
  person: "Act as you",
  approve: "Approve and control credentials",
  p0: "Set P0",
  read: "Read only",
};

/** Each word the mint form offers, by the label of its box, in the form's order. */
const MINT_BOXES: readonly (readonly [DispatchTokenScope, string])[] = (
  ["person", "approve", "p0", "read"] as const
).map((scope) => [scope, LABELS[scope]] as const);

/** The Set P0 box for `subject`, or null where none is drawn: the box is drawn only once whoami
 *  names p0, so a row reads its absence here rather than through `box`, which throws. */
function setP0Box(subject: string): HTMLInputElement | null {
  return screen.queryByRole<HTMLInputElement>("checkbox", { name: `${LABELS.p0} (${subject})` });
}

/** The Set P0 box for `subject`, once it is drawn. */
function drawnSetP0Box(subject: string): Promise<HTMLInputElement> {
  return screen.findByRole<HTMLInputElement>("checkbox", { name: `${LABELS.p0} (${subject})` });
}

/** The mint form's boxes drawn now, each with its word, in the form's order. Every box but Set
 *  P0 is drawn from the start, and `box` fails the row where one is missing; Set P0 is drawn
 *  only once whoami names p0, and a form that draws none holds no p0. */
function drawnMintBoxes(): (readonly [DispatchTokenScope, HTMLInputElement])[] {
  return MINT_BOXES.flatMap(([id, label]) => {
    const drawn = id === "p0" ? setP0Box("new token") : box(label, "new token");
    return drawn === null ? [] : [[id, drawn] as const];
  });
}

/** The set the mint form's boxes hold now, in the form's order. */
function formHolds(): DispatchTokenScope[] {
  return drawnMintBoxes()
    .filter(([, drawn]) => drawn.checked)
    .map(([id]) => id);
}

/** The boxes to press to take the mint form from its start (person and approve on) to the set
 *  each key names. Each test checks the form holds that set before its own press, so a setup that
 *  went wrong fails as a setup and not as the row. */
const SETUP: Record<string, readonly DispatchTokenScope[]> = {
  "person,approve": [],
  "person,approve,p0": ["p0"],
  p0: ["p0", "person"],
  read: ["person", "read"],
  "person,read": ["approve", "read"],
};

/** The Settings token section listing `listed`, none unless given, its mint form at its start. */
function renderMintForm(listed: AgentToken[] = []): () => void {
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue(listed);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <AgentTokensSection />
    </QueryClientProvider>
  );
  return () => {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
  };
}

/** What `GET /api/v1/whoami` answers a row: the scopes Dispatch names for the person at the
 *  browser, or a failure. */
type WhoamiAnswer = { readonly scopes: readonly string[] } | "failed";

/** A person at a browser, as a Dispatch with the p0 scope answers one: every scope of a person's
 *  own credential, p0 among them. */
const NAMES_P0: WhoamiAnswer = { scopes: ["person", "approve", "p0"] };

/** The same person, as a build before p0 answers one: the two scopes it knew. */
const NAMES_NO_P0: WhoamiAnswer = { scopes: ["person", "approve"] };

/**
 * Answers the row's `GET /api/v1/whoami` with `answer` at the test network layer, and counts the
 * reads that reached it. A `held` answer is given only once `release` is called, so the page reads
 * it as a read still out until then; a row that holds it releases it in its `finally`, given or
 * not.
 */
function answerWhoami(
  answer: WhoamiAnswer,
  { held = false }: { held?: boolean } = {}
): { asked: () => number; release: () => void } {
  let asked = 0;
  const gate = Promise.withResolvers<void>();
  if (!held) gate.resolve();
  answerRequests(
    http.get("/api/v1/whoami", async () => {
      asked += 1;
      await gate.promise;
      return answer === "failed"
        ? Response.json({ code: "INTERNAL", error: "the server failed" }, { status: 500 })
        : Response.json({ kind: "user", login: "alice", scopes: [...answer.scopes] });
    })
  );
  return { asked: () => asked, release: () => gate.resolve() };
}

// The mint form's state table, one test a row: from the state the row names, the one press it
// names leaves the boxes holding the result it names. A read-only token holds read alone or beside
// person and never beside approve or p0, so ticking Read only clears Approve and Set P0, ticking
// Approve clears Read only and ticks Act as you, and ticking Set P0 clears Read only; clearing Act
// as you clears Approve and leaves Set P0. No box is ever disabled for these rules: each press is
// taken, and the other boxes follow it. Whoami names p0 in every row, and a row that holds or
// presses Set P0 starts once the box is drawn. The rows that submit the form are the tests after
// this loop.
const STATE_TABLE: readonly {
  state: string;
  holds: readonly DispatchTokenScope[];
  press: DispatchTokenScope;
  result: readonly DispatchTokenScope[];
}[] = [
  {
    state: "at its start (person and approve on)",
    holds: ["person", "approve"],
    press: "read",
    result: ["person", "read"],
  },
  { state: "with read on alone", holds: ["read"], press: "approve", result: ["person", "approve"] },
  {
    state: "with read and person on",
    holds: ["person", "read"],
    press: "approve",
    result: ["person", "approve"],
  },
  { state: "with read on alone", holds: ["read"], press: "person", result: ["person", "read"] },
  {
    state: "with read and person on",
    holds: ["person", "read"],
    press: "person",
    result: ["read"],
  },
  {
    state: "with approve and person on",
    holds: ["person", "approve"],
    press: "person",
    result: [],
  },
  { state: "with read on alone", holds: ["read"], press: "read", result: [] },
  {
    state: "at its start (person and approve on)",
    holds: ["person", "approve"],
    press: "p0",
    result: ["person", "approve", "p0"],
  },
  {
    state: "with person, approve and p0 on",
    holds: ["person", "approve", "p0"],
    press: "read",
    result: ["person", "read"],
  },
  { state: "with read on alone", holds: ["read"], press: "p0", result: ["p0"] },
  {
    state: "with read and person on",
    holds: ["person", "read"],
    press: "p0",
    result: ["person", "p0"],
  },
  {
    state: "with person, approve and p0 on",
    holds: ["person", "approve", "p0"],
    press: "person",
    result: ["p0"],
  },
  { state: "with p0 on alone", holds: ["p0"], press: "p0", result: [] },
];

for (const row of STATE_TABLE) {
  const ticks = !row.holds.includes(row.press);
  const event = `${ticks ? "ticking" : "clearing"} ${LABELS[row.press]}`;
  const result = row.result.length === 0 ? "nothing" : row.result.join(" and ");
  test(`the mint form ${row.state}: ${event} leaves ${result} on`, async () => {
    const user = userEvent.setup();
    answerWhoami(NAMES_P0);
    const restore = renderMintForm();
    try {
      if ([...row.holds, row.press, ...row.result].includes("p0")) {
        await drawnSetP0Box("new token");
      }
      for (const scope of SETUP[row.holds.join(",")] ?? []) {
        await user.click(box(LABELS[scope], "new token"));
      }
      expect(formHolds()).toEqual([...row.holds]);

      await user.click(box(LABELS[row.press], "new token"));
      expect(formHolds()).toEqual([...row.result]);
      for (const [, drawn] of drawnMintBoxes()) expect(drawn.disabled).toBe(false);
    } finally {
      restore();
    }
  });
}

// A read-only token's mint sends the set its boxes hold, read among them.
test("the mint form sends read and person for a token that reads as its person", async () => {
  const user = userEvent.setup();
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reader",
    scopes: ["person", "read"],
    token: "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG",
  });
  const restore = renderMintForm();

  try {
    await user.click(box("Approve and control credentials", "new token"));
    await user.click(box("Read only", "new token"));
    expect(formHolds()).toEqual(["person", "read"]);
    await user.click(screen.getByLabelText("Token label"));
    await user.paste("reader");
    await user.click(screen.getByRole("button", { name: "New token" }));
    await waitFor(() =>
      expect(createAgentToken).toHaveBeenCalledWith(
        { name: "reader", scopes: ["person", "read"] },
        keyedAttempt
      )
    );
    // The form is back at its start for the next token.
    await waitFor(() => expect(formHolds()).toEqual(["person", "approve"]));
  } finally {
    restore();
    createAgentToken.mockRestore();
  }
});

// A token that only reads and acts for nobody: the mint sends read alone.
test("the mint form sends read alone for a token that only reads", async () => {
  const user = userEvent.setup();
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...token,
    id: "44444444-4444-4444-4444-444444444444",
    name: "plain-reader",
    scopes: ["read"],
    token: "dsp_abcdefghijklmnopqrstuvwxyz0123456789-HIJKLMN",
  });
  const restore = renderMintForm();

  try {
    for (const scope of SETUP.read ?? []) await user.click(box(LABELS[scope], "new token"));
    expect(formHolds()).toEqual(["read"]);
    await user.click(screen.getByLabelText("Token label"));
    await user.paste("plain-reader");
    await user.click(screen.getByRole("button", { name: "New token" }));
    await waitFor(() =>
      expect(createAgentToken).toHaveBeenCalledWith(
        { name: "plain-reader", scopes: ["read"] },
        keyedAttempt
      )
    );
  } finally {
    restore();
    createAgentToken.mockRestore();
  }
});

// A read-only token lists no project (400 TOKEN_PROJECTS_INPUT for a mint naming one beside
// read): while Read only is ticked, the projects picker is disabled and shows none, and the mint
// names no project, whatever was picked before. The pick is named again once Read only is cleared.
test("while Read only is ticked, the mint form's projects picker is disabled and the mint names no project", async () => {
  const user = userEvent.setup();
  listProjects.mockResolvedValue([project("CORE", "Core"), project("PRIV", "Private work")]);
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reader",
    scopes: ["person", "read"],
    token: "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG",
  });
  const restore = renderMintForm();

  try {
    await user.click(projectsPicker("new token"));
    await user.click(await screen.findByRole("option", { name: "PRIV, Private work" }));
    await user.keyboard("{Escape}");
    expect(projectsPicker("new token").textContent).toBe("PRIV");

    await user.click(box("Read only", "new token"));
    expect(formHolds()).toEqual(["person", "read"]);
    expect(projectsPicker("new token")).toBeDisabled();
    expect(projectsPicker("new token").textContent).toBe("Public projects only");
    await user.click(box("Read only", "new token"));
    expect(projectsPicker("new token")).toBeEnabled();
    expect(projectsPicker("new token").textContent).toBe("PRIV");
    await user.click(box("Read only", "new token"));

    await user.click(screen.getByLabelText("Token label"));
    await user.paste("reader");
    await user.click(screen.getByRole("button", { name: "New token" }));
    await waitFor(() =>
      expect(createAgentToken).toHaveBeenCalledWith(
        { name: "reader", scopes: ["person", "read"] },
        keyedAttempt
      )
    );
  } finally {
    restore();
    createAgentToken.mockRestore();
  }
});

// A read-only token's scopes never change, so its row shows them as words and offers no box; the
// row of a token that writes beside it keeps its Act as you and Approve boxes and offers no Read
// only.
test("a read-only token's row shows its scopes and offers no box to change them", async () => {
  const reader: AgentToken = {
    ...token,
    id: "33333333-3333-3333-3333-333333333333",
    name: "reader",
    scopes: ["read", "person"],
  };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([reader, token]);
  const updateAgentToken = spyOn(api, "updateAgentToken");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    const shown = await screen.findByRole("list", { name: "What reader may do" });
    expect(Array.from(shown.querySelectorAll("li"), (item) => item.textContent)).toEqual([
      "Act as you",
      "Read only",
    ]);
    for (const [, label] of MINT_BOXES) {
      expect(screen.queryByRole("checkbox", { name: `${label} (reader)` })).toBeNull();
    }
    expect(box("Act as you", "architect").checked).toBe(true);
    expect(box("Approve and control credentials", "architect").checked).toBe(true);
    expect(screen.queryByRole("checkbox", { name: "Read only (architect)" })).toBeNull();
    expect(updateAgentToken).not.toHaveBeenCalled();
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    updateAgentToken.mockRestore();
  }
});

// A read-only token lists no project, and no edit of it is taken (400 TOKEN_SCOPES_INPUT), so its
// row shows the projects it reads as words, as it shows its scopes, and offers no picker; the row
// of a token that writes beside it keeps its picker.
test("a read-only token's row shows its projects and offers no picker to change them", async () => {
  listProjects.mockResolvedValue([project("CORE", "Core")]);
  const reader: AgentToken = {
    ...token,
    id: "33333333-3333-3333-3333-333333333333",
    name: "reader",
    scopes: ["read", "person"],
  };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([reader, token]);
  const updateAgentToken = spyOn(api, "updateAgentToken");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    const shown = await screen.findByRole("list", { name: "What reader may do" });
    const row = shown.closest("tr") as HTMLElement;
    expect(within(row).getByText("Public projects only")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Projects \(reader\)/ })).toBeNull();
    expect(projectsPicker("architect")).toBeEnabled();
    expect(updateAgentToken).not.toHaveBeenCalled();
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    updateAgentToken.mockRestore();
  }
});

// Settings offers Set P0, the scope a priority of 0 needs, only where Dispatch says the person at
// the browser holds it: the section reads `GET /api/v1/whoami` and draws the box, on the mint form
// and on the row of each token that writes, once its scopes name p0. While that read has not
// answered, has failed, or was answered by a build before p0, no box is drawn and the form still
// mints. Each test below answers that read itself, in the state its name says.

/** The line under the Set P0 box: what a token holding p0 may do. */
const SET_P0_ALLOWS = "Set an issue's priority to P0.";

/** The plaintext Dispatch answers each mint of these tests with. */
const MINTED = "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG";

/** A token holding person, approve and p0: what the mint form makes with Set P0 ticked. */
const holdsP0: AgentToken = {
  ...token,
  id: "55555555-5555-5555-5555-555555555555",
  name: "planner",
  prefix: "PlNr5678",
  scopes: ["person", "approve", "p0"],
};

/** The mint, answered as Dispatch answers one: a new token holding the scopes the mint named. */
function answerMints(): Mock<typeof api.createAgentToken> {
  return spyOn(api, "createAgentToken").mockImplementation(async (input) => ({
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: input.name,
    scopes: input.scopes ?? null,
    token: MINTED,
  }));
}

/** Writes `label` in the mint form and presses New token. */
async function pressNewToken(user: UserEvent, label: string): Promise<void> {
  await user.click(screen.getByLabelText("Token label"));
  await user.paste(label);
  await user.click(screen.getByRole("button", { name: "New token" }));
}

test("once whoami names p0, the mint form draws Set P0, clear, with the line saying what it allows", async () => {
  answerWhoami(NAMES_P0);
  const restore = renderMintForm();

  try {
    const setP0 = await drawnSetP0Box("new token");
    expect(setP0.checked).toBe(false);
    expect(formHolds()).toEqual(["person", "approve"]);
    expect(
      within(setP0.closest("form") as HTMLFormElement).getByText(SET_P0_ALLOWS)
    ).toBeInTheDocument();
  } finally {
    restore();
  }
});

test("while whoami has not answered, the mint form draws no Set P0 and mints person and approve; once it names p0, the box is drawn, clear", async () => {
  const user = userEvent.setup();
  const whoami = answerWhoami(NAMES_P0, { held: true });
  const createAgentToken = answerMints();
  const restore = renderMintForm();

  try {
    expect(setP0Box("new token")).toBeNull();
    await pressNewToken(user, "reviewer");
    await waitFor(() =>
      expect(createAgentToken).toHaveBeenCalledWith(
        { name: "reviewer", scopes: ["person", "approve"] },
        keyedAttempt
      )
    );
    await screen.findByText(MINTED);
    expect(setP0Box("new token")).toBeNull();

    whoami.release();
    expect((await drawnSetP0Box("new token")).checked).toBe(false);
    expect(formHolds()).toEqual(["person", "approve"]);
  } finally {
    restore();
    whoami.release();
    createAgentToken.mockRestore();
  }
});

// A whoami that names no p0 is a Dispatch of the build before, which mints no p0; one that failed
// says nothing of it. Neither draws the box anywhere, and the form mints as it did before p0. The
// read is asserted to have been made, since a section that never asked would draw no box either.
for (const [state, answer] of [
  ["was answered by a build before p0", NAMES_NO_P0],
  ["failed", "failed"],
] as const) {
  test(`a whoami that ${state} draws no Set P0 on the mint form or on a token's row, and the form still mints person and approve with nothing else on the page broken`, async () => {
    const user = userEvent.setup();
    const whoami = answerWhoami(answer);
    const createAgentToken = answerMints();
    const restore = renderMintForm([token]);

    try {
      await screen.findByRole("cell", { name: "dsp_AbCdEf12…" });
      await waitFor(() =>
        expect(whoami.asked(), "the reads of GET /api/v1/whoami").toBeGreaterThan(0)
      );
      expect(setP0Box("new token")).toBeNull();
      expect(setP0Box("architect")).toBeNull();
      expect(box("Act as you", "architect").checked).toBe(true);
      expect(box("Approve and control credentials", "architect").checked).toBe(true);

      await pressNewToken(user, "reviewer");
      await waitFor(() =>
        expect(createAgentToken).toHaveBeenCalledWith(
          { name: "reviewer", scopes: ["person", "approve"] },
          keyedAttempt
        )
      );
      await screen.findByText(MINTED);
      expect(setP0Box("new token")).toBeNull();
      expect(setP0Box("architect")).toBeNull();
      expect(setP0Box("reviewer")).toBeNull();
      expect(formHolds()).toEqual(["person", "approve"]);
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    } finally {
      restore();
      createAgentToken.mockRestore();
    }
  });
}

test("with whoami naming p0, ticking Set P0 and pressing New token sends person, approve and p0", async () => {
  const user = userEvent.setup();
  answerWhoami(NAMES_P0);
  const createAgentToken = answerMints();
  const restore = renderMintForm();

  try {
    await user.click(await drawnSetP0Box("new token"));
    expect(formHolds()).toEqual(["person", "approve", "p0"]);
    await pressNewToken(user, "planner");
    await waitFor(() =>
      expect(createAgentToken).toHaveBeenCalledWith(
        { name: "planner", scopes: ["person", "approve", "p0"] },
        keyedAttempt
      )
    );
    await screen.findByText(MINTED);
  } finally {
    restore();
    createAgentToken.mockRestore();
  }
});

// The row shows Set P0 ticked for a token that holds p0 and clear for one that does not, and a
// change sends the whole set, since the PATCH replaces it.
test("with whoami naming p0, a writing token's row shows Set P0 as the token holds it, and ticking it sends the token's whole set with p0", async () => {
  const user = userEvent.setup();
  answerWhoami(NAMES_P0);
  const updateAgentToken = spyOn(api, "updateAgentToken").mockImplementation(
    async (id: string, input: { scopes: DispatchScope[] }) => ({
      ...token,
      id,
      scopes: input.scopes,
    })
  );
  const restore = renderMintForm([token, holdsP0]);

  try {
    const architect = await drawnSetP0Box("architect");
    expect(architect.checked).toBe(false);
    expect(setP0Box("planner")?.checked).toBe(true);

    await user.click(architect);
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenCalledWith(
        token.id,
        { scopes: ["person", "approve", "p0"] },
        keyedAttempt
      )
    );
    await waitFor(() => expect(setP0Box("architect")?.checked).toBe(true));
    expect(box("Act as you", "architect").checked).toBe(true);
    expect(box("Approve and control credentials", "architect").checked).toBe(true);
    for (const call of updateAgentToken.mock.calls) expect(call[1]?.scopes).not.toContain("read");
  } finally {
    restore();
    updateAgentToken.mockRestore();
  }
});

// Until whoami names p0, the row of a token that holds it draws no Set P0 box, and a change made on
// that row keeps p0 all the same: the change sends the token's whole set, and that set keeps each
// word the token holds that the row does not draw, so no change drops p0 in silence.
for (const [state, answer, held] of [
  ["has not answered", NAMES_P0, true],
  ["was answered by a build before p0", NAMES_NO_P0, false],
  ["failed", "failed", false],
] as const) {
  test(`while whoami ${state}, the row of a token holding p0 draws no Set P0, and clearing Approve sends person and p0`, async () => {
    const user = userEvent.setup();
    const whoami = answerWhoami(answer, { held });
    const updateAgentToken = spyOn(api, "updateAgentToken").mockImplementation(
      async (id: string, input: { scopes: DispatchScope[] }) => ({
        ...holdsP0,
        id,
        scopes: input.scopes,
      })
    );
    const restore = renderMintForm([holdsP0]);

    try {
      await screen.findByRole("cell", { name: "dsp_PlNr5678…" });
      // The read is in the state the test names: asked, and held, answered or failed.
      await waitFor(() =>
        expect(whoami.asked(), "the reads of GET /api/v1/whoami").toBeGreaterThan(0)
      );
      expect(setP0Box("planner")).toBeNull();
      expect(box("Approve and control credentials", "planner").checked).toBe(true);

      await user.click(box("Approve and control credentials", "planner"));
      await waitFor(() =>
        expect(updateAgentToken).toHaveBeenCalledWith(
          holdsP0.id,
          { scopes: ["person", "p0"] },
          keyedAttempt
        )
      );
      await waitFor(() =>
        expect(box("Approve and control credentials", "planner").checked).toBe(false)
      );
      expect(box("Act as you", "planner").checked).toBe(true);
      expect(setP0Box("planner")).toBeNull();
    } finally {
      restore();
      whoami.release();
      updateAgentToken.mockRestore();
    }
  });
}

// A read-only token's scopes never change, so its row offers no Set P0 either, while the row of a
// token that writes beside it does.
test("with whoami naming p0, a read-only token's row offers no Set P0 box", async () => {
  answerWhoami(NAMES_P0);
  const reader: AgentToken = {
    ...token,
    id: "33333333-3333-3333-3333-333333333333",
    name: "reader",
    scopes: ["read", "person"],
  };
  const restore = renderMintForm([reader, token]);

  try {
    expect((await drawnSetP0Box("architect")).checked).toBe(false);
    expect(setP0Box("reader")).toBeNull();
    const shown = screen.getByRole("list", { name: "What reader may do" });
    expect(Array.from(shown.querySelectorAll("li"), (item) => item.textContent)).toEqual([
      "Act as you",
      "Read only",
    ]);
  } finally {
    restore();
  }
});

test("with whoami naming p0, the mint form after a mint with Set P0 ticked is back at person and approve, with Set P0 clear", async () => {
  const user = userEvent.setup();
  answerWhoami(NAMES_P0);
  const createAgentToken = answerMints();
  const restore = renderMintForm();

  try {
    await user.click(await drawnSetP0Box("new token"));
    expect(formHolds()).toEqual(["person", "approve", "p0"]);
    await pressNewToken(user, "planner");
    await waitFor(() => expect(createAgentToken).toHaveBeenCalledTimes(1));
    await screen.findByText(MINTED);

    await waitFor(() => expect(formHolds()).toEqual(["person", "approve"]));
    expect(setP0Box("new token")?.checked).toBe(false);
  } finally {
    restore();
    createAgentToken.mockRestore();
  }
});

/** A live token labelled `name`, made `offsetMs` from now: after a press when positive, before
 *  it when negative. */
function madeAt(id: string, name: string, offsetMs: number): AgentToken {
  return { ...token, created_at: new Date(Date.now() + offsetMs).toISOString(), id, name };
}

/** The Settings token section, its mint answered `409 KEY_COMMITTED` and its list then read as
 *  `listed`; the list read before the press holds those of `listed` made before it, which the page
 *  tells apart from the lost mint's by id. `confirmAnswers` answers each `window.confirm` in
 *  turn, and `boxes` are the mint form's boxes pressed before New token. */
async function mintLostWith(
  user: UserEvent,
  listed: AgentToken[],
  confirmAnswers: boolean[] = [],
  boxes: readonly DispatchTokenScope[] = []
) {
  const now = Date.now();
  const listAgentTokens = spyOn(api, "listAgentTokens")
    .mockResolvedValueOnce(listed.filter((made) => Date.parse(made.created_at) <= now))
    .mockResolvedValue(listed);
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const lostMint = Promise.withResolvers<never>();
  const createAgentToken = spyOn(api, "createAgentToken")
    .mockReturnValueOnce(lostMint.promise)
    .mockResolvedValue({ ...token, id: "replacement", name: "reviewer", token: "dsp_replacement" });
  const revokeAgentToken = spyOn(api, "revokeAgentToken").mockResolvedValue();
  const asked: string[] = [];
  const originalConfirm = window.confirm;
  window.confirm = (message?: string) => {
    asked.push(message ?? "");
    return confirmAnswers.shift() ?? false;
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  render(
    <QueryClientProvider client={queryClient}>
      <AgentTokensSection />
    </QueryClientProvider>
  );
  // The tokens made before the press share the fixture's prefix.
  await screen.findAllByRole("cell", { name: "dsp_AbCdEf12…" });
  const label = screen.getByLabelText<HTMLInputElement>("Token label");
  await user.click(label);
  await user.paste("reviewer");
  for (const scope of boxes) await user.click(box(LABELS[scope], "new token"));
  await user.click(screen.getByRole("button", { name: "New token" }));
  await act(async () => {
    lostMint.reject(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }));
  });
  await screen.findByTestId("landed-without-answer");
  return {
    asked,
    createAgentToken,
    label,
    restore: () => {
      cleanup();
      queryClient.clear();
      window.confirm = originalConfirm;
      listAgentTokens.mockRestore();
      createAgentToken.mockRestore();
      revokeAgentToken.mockRestore();
    },
    revokeAgentToken,
    user,
  };
}

/** What the replace offer asks before it revokes `made`. */
function confirmFor(made: AgentToken): string {
  return `Revoke “${made.name}”, created ${new Date(made.created_at).toLocaleString()}? Agents using it stop working at once, and a new token “${made.name}” is minted in its place.`;
}

// A mint Dispatch made and lost the answer to (`409 KEY_COMMITTED`) made a token whose value,
// shown only once, is gone. The form lets go of its label, so a second press mints nothing, and
// says the value was lost. Once the list is read again, the one live token of that label made
// since the press - never an older one of the same label - is offered for revoking, and the
// reader confirms it by its label and when it was made before it is revoked and another minted.
test("a mint answered KEY_COMMITTED offers the one token made since the press, confirmed first", async () => {
  jest.useFakeTimers();
  const older = madeAt("older", "reviewer", -86_400_000);
  const lost: AgentToken = { ...madeAt("lost", "reviewer", 60_000), projects: ["PRIV"] };
  const page = await mintLostWith(userOnFakeClock(), [lost, older, token], [false, true]);

  try {
    expect(screen.getByTestId("landed-without-answer").textContent).toBe(
      "Token “reviewer” was made, but Dispatch lost its answer, so it may take a moment to appear. Its value is shown only once and was lost with the answer, so revoke it and mint another."
    );
    expect(page.label.value).toBe("");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    // The form submitted with no control pressed: user-event has no form for submit.
    fireEvent.submit(page.label.closest("form") as HTMLFormElement);
    await tick(20);
    expect(page.createAgentToken).toHaveBeenCalledTimes(1);

    const replace = await screen.findByRole("button", { name: "Revoke it and mint another" });
    // Cancelled: nothing revoked.
    await page.user.click(replace);
    expect(page.asked).toEqual([confirmFor(lost)]);
    await tick(20);
    expect(page.revokeAgentToken).not.toHaveBeenCalled();

    // The revoke's answer is held as the lost mint's was, and given inside `act` once it is out.
    const revoked = Promise.withResolvers<void>();
    page.revokeAgentToken.mockReturnValueOnce(revoked.promise);
    await page.user.click(replace);
    await waitFor(() => expect(page.revokeAgentToken).toHaveBeenCalledWith(lost.id, keyedAttempt));
    await act(async () => {
      revoked.resolve();
    });
    await waitFor(() => expect(page.createAgentToken).toHaveBeenCalledTimes(2));
    // The replacement holds what the lost token held: its scopes and its projects.
    expect(page.createAgentToken.mock.calls[1]?.[0]).toEqual({
      name: "reviewer",
      projects: ["PRIV"],
      scopes: ["person", "approve"],
    });
    expect(await screen.findByText("dsp_replacement")).toBeDefined();
    expect(screen.queryByTestId("landed-without-answer")).not.toBeInTheDocument();
    expect(page.revokeAgentToken).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    page.restore();
  }
});

// No live token of that label made since the press is listed - only an older one of the same
// label, and a revoked new one: the section says so and revokes nothing, and offers no button.
test("a lost mint with no token made since the press says so and revokes nothing", async () => {
  const older = madeAt("older", "reviewer", -86_400_000);
  const revoked = { ...madeAt("revoked", "reviewer", 60_000), revoked_at: "2099-01-01T00:00:00Z" };
  const page = await mintLostWith(userEvent.setup(), [older, revoked, token]);

  try {
    expect(
      await screen.findByText(
        "No live token “reviewer” made since you pressed New token is listed, so none was revoked."
      )
    ).toBeDefined();
    expect(
      screen.queryByRole("button", { name: "Revoke it and mint another" })
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /^Revoke the one created/ })
    ).not.toBeInTheDocument();
    expect(page.asked).toEqual([]);
    expect(page.revokeAgentToken).not.toHaveBeenCalled();
  } finally {
    page.restore();
  }
});

// More than one live token of that label was made since the press (another tab minted one
// too): the section lists them, newest first, and the reader picks the one to revoke, confirming
// it by when it was made; only that one is revoked.
test("a lost mint with several tokens made since the press lists them and revokes the one picked", async () => {
  const first = madeAt("first", "reviewer", 60_000);
  const second = madeAt("second", "reviewer", 120_000);
  const page = await mintLostWith(userEvent.setup(), [first, second, token], [true]);

  try {
    expect(
      await screen.findByText(
        "2 live tokens “reviewer” were made since you pressed New token. Pick the one to revoke:"
      )
    ).toBeDefined();
    const picks = screen.getAllByRole("button", { name: /^Revoke the one created/ });
    expect(picks.map((pick) => pick.textContent)).toEqual([
      `Revoke the one created ${new Date(second.created_at).toLocaleString()}`,
      `Revoke the one created ${new Date(first.created_at).toLocaleString()}`,
    ]);
    expect(
      screen.queryByRole("button", { name: "Revoke it and mint another" })
    ).not.toBeInTheDocument();

    await page.user.click(picks[1] as HTMLElement);
    expect(page.asked).toEqual([confirmFor(first)]);
    await waitFor(() => expect(page.revokeAgentToken).toHaveBeenCalledWith(first.id, keyedAttempt));
    await waitFor(() => expect(page.createAgentToken).toHaveBeenCalledTimes(2));
    expect(page.revokeAgentToken).toHaveBeenCalledTimes(1);
  } finally {
    page.restore();
  }
});

// A lost mint's replacement holds what the lost token held, read included: a read-only token's
// replacement is read only too, never a token that writes.
test("a lost read-only mint is replaced by a read-only token", async () => {
  const lost: AgentToken = { ...madeAt("lost", "reviewer", 60_000), scopes: ["read", "person"] };
  const page = await mintLostWith(userEvent.setup(), [lost, token], [true], ["approve", "read"]);

  try {
    // The lost mint asked for a read-only token.
    expect(page.createAgentToken.mock.calls[0]?.[0]).toEqual({
      name: "reviewer",
      scopes: ["person", "read"],
    });
    await page.user.click(
      await screen.findByRole("button", { name: "Revoke it and mint another" })
    );
    await waitFor(() => expect(page.revokeAgentToken).toHaveBeenCalledWith(lost.id, keyedAttempt));
    await waitFor(() => expect(page.createAgentToken).toHaveBeenCalledTimes(2));
    expect(page.createAgentToken.mock.calls[1]?.[0]).toEqual({
      name: "reviewer",
      scopes: ["person", "read"],
    });
  } finally {
    page.restore();
  }
});

/** The token section with `listed` read, and the list then asked again - as another tab's
 *  change or a refresh would ask it - with that read's answer held on `staleRead`, so it is still
 *  out when a write the row makes lands. */
async function renderWithListReadOut(listed: AgentToken[]): Promise<{
  listAgentTokens: Mock<typeof api.listAgentTokens>;
  restore: () => void;
  staleRead: ReturnType<typeof Promise.withResolvers<AgentToken[]>>;
}> {
  const staleRead = Promise.withResolvers<AgentToken[]>();
  const listAgentTokens = spyOn(api, "listAgentTokens")
    .mockResolvedValueOnce(listed)
    .mockReturnValueOnce(staleRead.promise);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <AgentTokensSection />
    </QueryClientProvider>
  );
  await screen.findByRole("cell", { name: "dsp_AbCdEf12…" });
  await act(async () => {
    void queryClient.invalidateQueries({ queryKey: ["agent-tokens"] });
  });
  expect(listAgentTokens).toHaveBeenCalledTimes(2);
  return {
    listAgentTokens,
    restore: () => {
      cleanup();
      queryClient.clear();
      listAgentTokens.mockRestore();
    },
    staleRead,
  };
}

/** The prefix cell of every token the section lists, top to bottom. */
function listedPrefixes(): (string | null)[] {
  return screen.getAllByRole("cell", { name: /^dsp_/ }).map((cell) => cell.textContent);
}

// A list read still out when a mint's answer lands was asked before the token existed: landing
// after the answer, it would drop the new token from the list the reader just saw it join.
test("a list read asked before a mint answered does not drop the token the mint made", async () => {
  const user = userEvent.setup();
  const page = await renderWithListReadOut([token]);
  const reviewer: AgentToken = {
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reviewer",
    prefix: "ZyXw9876",
  };
  const minted = Promise.withResolvers<CreatedAgentToken>();
  const createAgentToken = spyOn(api, "createAgentToken").mockReturnValue(minted.promise);

  try {
    await user.click(screen.getByLabelText("Token label"));
    await user.paste("reviewer");
    await user.click(screen.getByRole("button", { name: "New token" }));
    await waitFor(() => expect(createAgentToken).toHaveBeenCalledTimes(1));
    // The answer gives up the read still out and asks for the list again, after the mint: Dispatch
    // lists the new token then.
    page.listAgentTokens.mockResolvedValueOnce([reviewer, token]);
    await act(async () => {
      minted.resolve({ ...reviewer, token: "dsp_ZyXw9876abcdefghijklmnopqrstuvwxyz0123456789" });
    });
    await waitFor(() => expect(listedPrefixes()).toEqual(["dsp_ZyXw9876…", "dsp_AbCdEf12…"]));

    // The read asked before the mint answers the list without the new token.
    await act(async () => {
      page.staleRead.resolve([token]);
      await settle();
    });
    expect(listedPrefixes()).toEqual(["dsp_ZyXw9876…", "dsp_AbCdEf12…"]);
    expect(page.listAgentTokens).toHaveBeenCalledTimes(3);
  } finally {
    page.restore();
    createAgentToken.mockRestore();
  }
});

// A list read still out when a revoke's answer lands was asked while the token was live: landing
// after the answer, it would show the revoked token live again, with its Revoke offered.
test("a list read asked before a revoke answered does not show the revoked token live again", async () => {
  const user = userEvent.setup();
  const page = await renderWithListReadOut([token]);
  const revoked = Promise.withResolvers<void>();
  const revokeAgentToken = spyOn(api, "revokeAgentToken").mockReturnValue(revoked.promise);
  const originalConfirm = window.confirm;
  window.confirm = () => true;

  try {
    await user.click(screen.getByRole("button", { name: "Revoke architect" }));
    await waitFor(() => expect(revokeAgentToken).toHaveBeenCalledWith(token.id, keyedAttempt));
    // The answer gives up the read still out and asks for the list again, after the revoke:
    // Dispatch lists the token revoked then.
    page.listAgentTokens.mockResolvedValueOnce([{ ...token, revoked_at: "2026-09-13T00:00:00Z" }]);
    await act(async () => {
      revoked.resolve();
    });
    await waitFor(() => expect(screen.queryByText("Revoked")).not.toBeNull());

    // The read asked before the revoke answers the token live.
    await act(async () => {
      page.staleRead.resolve([token]);
      await settle();
    });
    expect(screen.queryByText("Revoked")).not.toBeNull();
    expect(screen.queryByRole("button", { name: "Revoke architect" })).toBeNull();
    expect(page.listAgentTokens).toHaveBeenCalledTimes(3);
  } finally {
    window.confirm = originalConfirm;
    page.restore();
    revokeAgentToken.mockRestore();
  }
});

// A list read still out when a scope change's answer lands was asked before the change: landing
// after the answer, it would clear the box the reader just saw the change tick.
test("a list read asked before a scope change answered does not put back the scopes it replaced", async () => {
  const user = userEvent.setup();
  const personOnly: AgentToken = { ...token, scopes: ["person"] };
  const both: AgentToken = { ...personOnly, scopes: ["person", "approve"] };
  const page = await renderWithListReadOut([personOnly]);
  const changed = Promise.withResolvers<AgentToken>();
  const updateAgentToken = spyOn(api, "updateAgentToken").mockReturnValue(changed.promise);

  try {
    await user.click(box("Approve and control credentials", "architect"));
    await waitFor(() =>
      expect(updateAgentToken).toHaveBeenCalledWith(
        personOnly.id,
        { scopes: ["person", "approve"] },
        keyedAttempt
      )
    );
    // The answer gives up the read still out and asks for the list again, after the change:
    // Dispatch lists the token with both scopes then.
    page.listAgentTokens.mockResolvedValueOnce([both]);
    await act(async () => {
      changed.resolve(both);
    });
    await waitFor(() =>
      expect(box("Approve and control credentials", "architect").checked).toBe(true)
    );

    // The read asked before the change answers the token with Act as you alone.
    await act(async () => {
      page.staleRead.resolve([personOnly]);
      await settle();
    });
    expect(box("Approve and control credentials", "architect").checked).toBe(true);
    expect(box("Act as you", "architect").checked).toBe(true);
    expect(page.listAgentTokens).toHaveBeenCalledTimes(3);
  } finally {
    page.restore();
    updateAgentToken.mockRestore();
  }
});

// A mint the reader stopped may have been made: the form says so, and that its Retry asks again
// under the mint's key, rather than a failure it cannot know of.
test("a mint the reader stopped says Retry asks again under its key", async () => {
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const createAgentToken = spyOn(api, "createAgentToken").mockRejectedValue(
    new NetworkError("/api/v1/me/agent-tokens", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(screen.getByLabelText("Token label"));
    await user.paste("reviewer");
    await user.click(screen.getByRole("button", { name: "New token" }));
    await stopTheLateWrite(queryClient);
    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the new agent token", "made", {
        method: "POST",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    createAgentToken.mockRestore();
  }
});
