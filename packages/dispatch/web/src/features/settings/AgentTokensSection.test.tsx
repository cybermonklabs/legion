import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the submit site below is a form submitted with no control pressed, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { AgentToken, CreatedAgentToken, DispatchScope } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { AgentTokensSection } from "./AgentTokensSection";

const token: AgentToken = {
  created_at: "2026-09-12T00:00:00Z",
  id: "11111111-1111-1111-1111-111111111111",
  last_used_at: null,
  name: "architect",
  prefix: "AbCdEf12",
  revoked_at: null,
  scopes: ["person", "approve"],
};

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
// carries the other where the server would refuse the pair.
test("a token's row shows the scopes it holds, and each change sends the whole set", async () => {
  const user = userEvent.setup();
  const personOnly: AgentToken = { ...token, scopes: ["person"] };
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([personOnly]);
  const setAgentTokenScopes = spyOn(api, "setAgentTokenScopes").mockImplementation(
    async (id: string, scopes: DispatchScope[]) => ({ ...personOnly, id, scopes })
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

    await user.click(box("Approve and control credentials", "architect"));
    await waitFor(() =>
      expect(setAgentTokenScopes).toHaveBeenCalledWith(
        personOnly.id,
        ["person", "approve"],
        keyedAttempt
      )
    );
    await waitFor(() =>
      expect(box("Approve and control credentials", "architect").checked).toBe(true)
    );
    expect(box("Act as you", "architect").checked).toBe(true);

    await user.click(box("Act as you", "architect"));
    await waitFor(() =>
      expect(setAgentTokenScopes).toHaveBeenLastCalledWith(personOnly.id, [], keyedAttempt)
    );
    await waitFor(() => expect(box("Act as you", "architect").checked).toBe(false));
    expect(box("Approve and control credentials", "architect").checked).toBe(false);
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    setAgentTokenScopes.mockRestore();
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

/** A live token labelled `name`, made `offsetMs` from now: after a press when positive, before
 *  it when negative. */
function madeAt(id: string, name: string, offsetMs: number): AgentToken {
  return { ...token, created_at: new Date(Date.now() + offsetMs).toISOString(), id, name };
}

/** The Settings token section, its mint answered `409 KEY_COMMITTED` and its list then read as
 *  `listed`; the list read before the press holds those of `listed` made before it, which the page
 *  tells apart from the lost mint's by id. `confirmAnswers` answers each `window.confirm` in
 *  turn. */
async function mintLostWith(user: UserEvent, listed: AgentToken[], confirmAnswers: boolean[] = []) {
  const now = Date.now();
  const listAgentTokens = spyOn(api, "listAgentTokens")
    .mockResolvedValueOnce(listed.filter((made) => Date.parse(made.created_at) <= now))
    .mockResolvedValue(listed);
  // Held, and given inside `act` after the press, as packages/dispatch/AGENTS.md's act rule says.
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
  const lost = madeAt("lost", "reviewer", 60_000);
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
    // The replacement holds what the lost token held.
    expect(page.createAgentToken.mock.calls[1]?.[0]).toEqual({
      name: "reviewer",
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
  const setAgentTokenScopes = spyOn(api, "setAgentTokenScopes").mockReturnValue(changed.promise);

  try {
    await user.click(box("Approve and control credentials", "architect"));
    await waitFor(() =>
      expect(setAgentTokenScopes).toHaveBeenCalledWith(
        personOnly.id,
        ["person", "approve"],
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
    setAgentTokenScopes.mockRestore();
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
