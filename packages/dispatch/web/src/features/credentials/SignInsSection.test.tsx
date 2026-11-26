import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { api } from "../../api/client";
import { SignInsSection } from "./SignInsSection";

test("Sign-ins names the agent-secrets login command as code in its description and in its revoke dialog, never in backticks", async () => {
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
    const dialog = await screen.findByRole("dialog", {
      name: "Revoke your sign-in on devbox.example.com?",
    });
    expect(dialog.textContent).not.toContain("`");
    expect(within(dialog).getByText("agent-secrets login").tagName).toBe("CODE");
  } finally {
    cleanup();
    queryClient.clear();
    list.mockRestore();
  }
});
