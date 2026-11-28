import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { api } from "../../api/client";
import { SignInsSection } from "./SignInsSection";

test("Sign-ins names the agent-secrets login command as code in its description, and in quotes in its revoke question, never in backticks", async () => {
  const user = userEvent.setup();
  const list = spyOn(api, "getSignIns").mockResolvedValue({
    signins: [
      {
        created_at: "2026-10-03T12:00:00Z",
        last_used_at: "2026-10-03T12:30:00Z",
        machine: "devbox.example.com",
        signin_id: "signin-1",
        thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs",
      },
    ],
  });
  // The reader answers no, so the question is read and no revoke is sent.
  const confirm = spyOn(window, "confirm").mockReturnValue(false);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <SignInsSection />
      </QueryClientProvider>
    );
    const region = within(await screen.findByRole("region", { name: "Sign-ins" }));
    const description = region.getByText(/Every machine you have signed in on with/);
    expect(description.textContent).not.toContain("`");
    expect(within(description).getByText("agent-secrets login").tagName).toBe("CODE");

    await user.click(await region.findByRole("button", { name: "Revoke" }));
    const question = String(confirm.mock.calls[0]?.[0]);
    expect(question).not.toContain("`");
    expect(question).toContain("“agent-secrets login”");
  } finally {
    cleanup();
    queryClient.clear();
    confirm.mockRestore();
    list.mockRestore();
  }
});
