import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";

import {
  createdPasskeyJSON,
  enrollmentOptions,
  installFakePasskeys,
} from "../../__tests__/fake-passkeys";
import { wholeText } from "../../__tests__/whole-text";
import { ApiError, api } from "../../api/client";
import { App } from "../../app";

const token = "q".repeat(43);
const ana = "ana@other.example";

/** Where the page is, for a test to read after a navigation. */
function Address() {
  const location = useLocation();
  return <output aria-label="address">{`${location.pathname}${location.hash}`}</output>;
}

/** The app at `address`, signed in to nobody: the enrollment page needs no session. */
function renderApp(address: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const whoAmI = spyOn(api, "whoAmI").mockRejectedValue(
    new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized", sign_in: "passkey" })
  );
  const view = render(
    <MemoryRouter initialEntries={[address]}>
      <QueryClientProvider client={queryClient}>
        <App />
        <Address />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    queryClient,
    restore: () => {
      view.unmount();
      queryClient.clear();
      whoAmI.mockRestore();
    },
    whoAmI,
  };
}

test("the link's page names the address, makes the passkey with Dispatch's options, and signs the person in", async () => {
  const user = userEvent.setup();
  const browser = installFakePasskeys();
  const begin = spyOn(api, "passkeyEnrollmentOptions").mockResolvedValue(enrollmentOptions(ana));
  const finish = spyOn(api, "passkeyEnroll").mockResolvedValue({ login: ana });
  const app = renderApp(`/enroll#${token}`);
  try {
    expect(await screen.findByRole("heading", { name: "Enroll a passkey" })).toBeInTheDocument();
    // Who invited the person, and to what, before anything is made.
    expect(
      await screen.findByText(wholeText("alice@example.com invited you to the project PRIV."))
    ).toBeInTheDocument();
    expect(
      await screen.findByText(wholeText(`The passkey signs you in to Dispatch as ${ana}.`))
    ).toBeInTheDocument();
    expect(begin).toHaveBeenCalledWith(token);

    await user.click(screen.getByRole("button", { name: "Create a passkey" }));
    await waitFor(() => expect(screen.getByLabelText("address")).toHaveTextContent(/^\/$/));
    const [created] = browser.create.mock.calls[0] ?? [];
    const publicKey = created?.publicKey;
    expect(new Uint8Array(publicKey?.challenge as ArrayBuffer)).toEqual(new Uint8Array([0, 1, 2]));
    expect(publicKey?.user.name).toBe(ana);
    expect(publicKey?.authenticatorSelection).toEqual({
      residentKey: "required",
      userVerification: "required",
    });
    expect(finish).toHaveBeenCalledWith(createdPasskeyJSON);
    // A fresh ceremony for the press, since the one the page opened with may be past its five
    // minutes by then.
    expect(begin).toHaveBeenCalledTimes(2);
  } finally {
    app.restore();
    browser.restore();
    begin.mockRestore();
    finish.mockRestore();
  }
});

test("a link Dispatch refuses says why, and offers nothing to press", async () => {
  const browser = installFakePasskeys();
  try {
    for (const [code, status, words] of [
      [
        "INVITATION_USED",
        410,
        "This link has been used already. If you made a passkey with it, sign in with that passkey. If not, ask the project's owner for a new one.",
      ],
      ["INVITATION_EXPIRED", 410, "This link has expired. Ask the project's owner for a new one."],
      [
        "INVITATION_UNKNOWN",
        404,
        "Dispatch does not hold this link: the project's owner made a newer one, reset the address, or took it off the access list. Ask the project's owner for a new one.",
      ],
      [
        "INVITATION_NOT_LISTED",
        403,
        "The project's access list no longer names this address, so this link cannot enroll a passkey. Ask the project's owner.",
      ],
      [
        "FULL_MEMBER",
        409,
        "This address belongs to a member of the company's Google Workspace, who signs in with Google and never with a passkey.",
      ],
    ] as const) {
      const begin = spyOn(api, "passkeyEnrollmentOptions").mockRejectedValue(
        new ApiError(status, { code, error: code.toLowerCase() })
      );
      const app = renderApp(`/enroll#${token}`);
      try {
        expect(await screen.findByRole("alert")).toHaveTextContent(words);
        expect(screen.getByRole("heading", { name: "Enroll a passkey" })).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Create a passkey" })).not.toBeInTheDocument();
        expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
        // Only a used link can have made the person a passkey, so only it offers the sign-in.
        expect(screen.queryAllByRole("link", { name: "Sign in with your passkey" })).toHaveLength(
          code === "INVITATION_USED" ? 1 : 0
        );
        // No refused link's code stays in the address either.
        expect(screen.getByLabelText("address")).toHaveTextContent(/^\/enroll$/);
      } finally {
        app.restore();
        begin.mockRestore();
      }
    }
  } finally {
    browser.restore();
  }
});

test("a link with no code after its # asks Dispatch nothing", async () => {
  const browser = installFakePasskeys();
  const begin = spyOn(api, "passkeyEnrollmentOptions");
  const app = renderApp("/enroll");
  try {
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This link is incomplete: nothing follows its #. Open the link exactly as the project's owner sent it."
    );
    expect(begin).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Create a passkey" })).not.toBeInTheDocument();
  } finally {
    app.restore();
    browser.restore();
    begin.mockRestore();
  }
});

test("a browser that cannot make a passkey is told so before anything is asked", async () => {
  const begin = spyOn(api, "passkeyEnrollmentOptions");
  const app = renderApp(`/enroll#${token}`);
  try {
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This browser cannot make a passkey. Open the link in a current version of Chrome, Edge, Firefox or Safari."
    );
    expect(begin).not.toHaveBeenCalled();
  } finally {
    app.restore();
    begin.mockRestore();
  }
});

test("a cancelled passkey keeps the button, says what happened, and a second press enrolls", async () => {
  const user = userEvent.setup();
  const browser = installFakePasskeys();
  browser.create.mockRejectedValueOnce(new DOMException("cancelled", "NotAllowedError"));
  const begin = spyOn(api, "passkeyEnrollmentOptions").mockResolvedValue(enrollmentOptions(ana));
  const finish = spyOn(api, "passkeyEnroll").mockResolvedValue({ login: ana });
  const app = renderApp(`/enroll#${token}`);
  try {
    await user.click(await screen.findByRole("button", { name: "Create a passkey" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "No passkey was made: the request was cancelled or timed out. Press the button to try again."
    );
    expect(finish).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Create a passkey" }));
    await waitFor(() => expect(screen.getByLabelText("address")).toHaveTextContent(/^\/$/));
    expect(finish).toHaveBeenCalledTimes(1);
  } finally {
    app.restore();
    browser.restore();
    begin.mockRestore();
    finish.mockRestore();
  }
});

test("a link used in another tab between the check and the press says so, and takes the button away", async () => {
  const user = userEvent.setup();
  const browser = installFakePasskeys();
  const begin = spyOn(api, "passkeyEnrollmentOptions").mockResolvedValue(enrollmentOptions(ana));
  const finish = spyOn(api, "passkeyEnroll").mockRejectedValue(
    new ApiError(410, { code: "INVITATION_USED", error: "used" })
  );
  const app = renderApp(`/enroll#${token}`);
  try {
    await user.click(await screen.findByRole("button", { name: "Create a passkey" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This link has been used already. If you made a passkey with it, sign in with that passkey. If not, ask the project's owner for a new one."
    );
    expect(screen.queryByRole("button", { name: "Create a passkey" })).not.toBeInTheDocument();
    expect(screen.getByLabelText("address")).toHaveTextContent(/^\/enroll$/);
  } finally {
    app.restore();
    browser.restore();
    begin.mockRestore();
    finish.mockRestore();
  }
});

// Another enrollment of the same address finished first, or the finish was replayed: each begin's
// challenge is single-use, so Dispatch answers CEREMONY_EXPIRED, and a second press begins anew.
test("an enrollment another one raced says so, and the next press starts over from the link's check", async () => {
  const user = userEvent.setup();
  const browser = installFakePasskeys();
  const begin = spyOn(api, "passkeyEnrollmentOptions").mockResolvedValue(enrollmentOptions(ana));
  const finish = spyOn(api, "passkeyEnroll")
    .mockRejectedValueOnce(new ApiError(400, { code: "CEREMONY_EXPIRED", error: "expired" }))
    .mockResolvedValueOnce({ login: ana });
  const app = renderApp(`/enroll#${token}`);
  try {
    await user.click(await screen.findByRole("button", { name: "Create a passkey" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This request ran out, or another one finished first. Press the button to try again."
    );
    const begun = begin.mock.calls.length;
    await user.click(screen.getByRole("button", { name: "Create a passkey" }));
    await waitFor(() => expect(screen.getByLabelText("address")).toHaveTextContent(/^\/$/));
    expect(begin.mock.calls.length).toBe(begun + 1);
    expect(finish).toHaveBeenCalledTimes(2);
  } finally {
    app.restore();
    browser.restore();
    begin.mockRestore();
    finish.mockRestore();
  }
});

// The link's code is a live way in until it is used: the page reads it once, then replaces the
// tab's history entry with one without it, so the address bar and a copy of the address carry
// none. The page keeps the code it read, and checks and enrolls with it.
test("the page takes the link's code out of the address once it reads it, and enrolls with the code", async () => {
  const user = userEvent.setup();
  const browser = installFakePasskeys();
  const begin = spyOn(api, "passkeyEnrollmentOptions").mockResolvedValue(enrollmentOptions(ana));
  const finish = spyOn(api, "passkeyEnroll").mockResolvedValue({ login: ana });
  const app = renderApp(`/enroll#${token}`);
  try {
    await waitFor(() => expect(screen.getByLabelText("address")).toHaveTextContent(/^\/enroll$/));
    await user.click(await screen.findByRole("button", { name: "Create a passkey" }));
    await waitFor(() => expect(screen.getByLabelText("address")).toHaveTextContent(/^\/$/));
    expect(begin.mock.calls).toEqual([[token], [token]]);
    expect(finish).toHaveBeenCalledWith(createdPasskeyJSON);
  } finally {
    app.restore();
    browser.restore();
    begin.mockRestore();
    finish.mockRestore();
  }
});

// A used link may be the person's own: they enrolled with it, and opened it again, or their finish
// went through and its answer was lost. Their passkey signs them in, so the page sends them there.
test("a used link links to the sign-in, where the passkey it made signs the person in", async () => {
  const user = userEvent.setup();
  const browser = installFakePasskeys();
  const begin = spyOn(api, "passkeyEnrollmentOptions").mockRejectedValue(
    new ApiError(410, { code: "INVITATION_USED", error: "used" })
  );
  const app = renderApp(`/enroll#${token}`);
  try {
    await user.click(await screen.findByRole("link", { name: "Sign in with your passkey" }));
    await waitFor(() => expect(screen.getByLabelText("address")).toHaveTextContent(/^\/$/));
    expect(
      await screen.findByRole("button", { name: "Sign in with a passkey" })
    ).toBeInTheDocument();
  } finally {
    app.restore();
    browser.restore();
    begin.mockRestore();
  }
});
