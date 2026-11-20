import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import {
  installFakePasskeys,
  passkeySignatureJSON,
  signInOptions,
} from "../../__tests__/fake-passkeys";
import { wholeText } from "../../__tests__/whole-text";
import { ApiError, api } from "../../api/client";
import type { AuthenticatedUser } from "../../api/types";
import { AuthGate } from "../../app";

const ana = "ana@other.example";

/** Dispatch's 401 at its outside origin, where the one sign-in is a passkey. */
const signedOutHere = new ApiError(401, {
  code: "NO_IDENTITY",
  error: "unauthorized",
  sign_in: "passkey",
});

/** The app at `/` with whoami answering `whoAmI`, and the reads the shell makes once signed in. */
function renderGate(whoAmI: () => Promise<AuthenticatedUser>) {
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const spies = [
    spyOn(api, "whoAmI").mockImplementation(whoAmI),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
  ];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return () => {
    view.unmount();
    queryClient.clear();
    for (const spy of spies) spy.mockRestore();
    window.matchMedia = originalMatchMedia;
  };
}

test("where the one sign-in is a passkey, the sign-in page offers it and no Google link", async () => {
  const browser = installFakePasskeys();
  const teardown = renderGate(async () => {
    throw signedOutHere;
  });
  try {
    expect(await screen.findByRole("button", { name: "Sign in with a passkey" })).toBeVisible();
    expect(screen.queryByRole("link", { name: "Sign in with Google" })).not.toBeInTheDocument();
    expect(screen.getByRole("main").textContent).toBe(
      "DispatchMake the next decision clear.Sign in with the passkey you made through the link a project's owner sent you.Sign in with a passkey"
    );
  } finally {
    teardown();
    browser.restore();
  }
});

test("a 401 that names no passkey sign-in shows the Google link as always", async () => {
  const teardown = renderGate(async () => {
    throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
  });
  try {
    expect(await screen.findByRole("link", { name: "Sign in with Google" })).toBeVisible();
    expect(
      screen.queryByRole("button", { name: "Sign in with a passkey" })
    ).not.toBeInTheDocument();
  } finally {
    teardown();
  }
});

test("the passkey signs the person in, and the shell opens without a reload", async () => {
  const user = userEvent.setup();
  const browser = installFakePasskeys();
  let signedIn = false;
  const begin = spyOn(api, "passkeySignInOptions").mockResolvedValue(signInOptions);
  const finish = spyOn(api, "passkeySignIn").mockImplementation(async () => {
    signedIn = true;
    return { login: ana };
  });
  const teardown = renderGate(async () => {
    if (!signedIn) throw signedOutHere;
    return { event_head: "0", kind: "user", login: ana, member_kind: "restricted" };
  });
  try {
    await user.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    expect(await screen.findByText(wholeText(`Signed in as ${ana}`))).toBeInTheDocument();
    const [asked] = browser.get.mock.calls[0] ?? [];
    expect(new Uint8Array(asked?.publicKey?.challenge as ArrayBuffer)).toEqual(
      new Uint8Array([0, 1, 2])
    );
    expect(asked?.publicKey?.userVerification).toBe("required");
    expect(finish).toHaveBeenCalledWith(passkeySignatureJSON);
  } finally {
    teardown();
    browser.restore();
    begin.mockRestore();
    finish.mockRestore();
  }
});

test("a passkey Dispatch refuses is said in plain words beside the button, which stays", async () => {
  const user = userEvent.setup();
  const browser = installFakePasskeys();
  const begin = spyOn(api, "passkeySignInOptions").mockResolvedValue(signInOptions);
  for (const [code, words] of [
    [
      "PASSKEY_SIGN_COUNT",
      "Dispatch refused this passkey: its sign-in count did not go up since its last use, which happens when a passkey has been copied. Ask a project's owner for a new link.",
    ],
    [
      "PASSKEY_UNKNOWN",
      "Dispatch does not know this passkey: it was removed, so it signs in no more. Ask a project's owner for a new link.",
    ],
  ] as const) {
    const finish = spyOn(api, "passkeySignIn").mockRejectedValue(
      new ApiError(401, { code, error: code.toLowerCase() })
    );
    const teardown = renderGate(async () => {
      throw signedOutHere;
    });
    try {
      await user.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
      expect(await screen.findByRole("alert")).toHaveTextContent(words);
      // Another passkey on another device may still sign in, so the button stays.
      expect(screen.getByRole("button", { name: "Sign in with a passkey" })).toBeVisible();
    } finally {
      teardown();
      finish.mockRestore();
    }
  }
  begin.mockRestore();
  browser.restore();
});

test("a browser that cannot use a passkey is told so on the sign-in page", async () => {
  const teardown = renderGate(async () => {
    throw signedOutHere;
  });
  try {
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This browser cannot use a passkey. Open Dispatch in a current version of Chrome, Edge, Firefox or Safari."
    );
    expect(
      screen.queryByRole("button", { name: "Sign in with a passkey" })
    ).not.toBeInTheDocument();
  } finally {
    teardown();
  }
});
