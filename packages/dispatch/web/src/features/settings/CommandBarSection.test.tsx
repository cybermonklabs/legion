import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRoute } from "../../__tests__/send-answers";
import { api, type WriteBounds } from "../../api/client";
import type { GoingSettings } from "../../api/types";
import { CommandBarSection } from "./CommandBarSection";

// The person's own command-bar setting, as the Settings page shows it: read from Dispatch, off
// for a person who never set it, and written back by the toggle.

function renderSection(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <CommandBarSection />
    </QueryClientProvider>
  );
}

test("the setting is off by default, and turning it on writes it to Dispatch", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "myGoingSettings").mockResolvedValue({ confirm_writes: false });
  const put = answerRoute<(input: GoingSettings, bounds: WriteBounds) => Promise<GoingSettings>>(
    "PUT",
    "/api/v1/me/going/settings"
  );
  put.mockImplementation(async (input) => input);
  try {
    renderSection();
    const toggle = await screen.findByRole("checkbox", { name: "Ask before every write" });
    expect(toggle).not.toBeChecked();

    await user.click(toggle);

    await waitFor(() => expect(put).toHaveBeenCalledWith({ confirm_writes: true }, keyedAttempt));
    await waitFor(() => expect(toggle).toBeChecked());
  } finally {
    cleanup();
    read.mockRestore();
  }
});

test("a setting Dispatch holds on is read back on", async () => {
  const read = spyOn(api, "myGoingSettings").mockResolvedValue({ confirm_writes: true });
  try {
    renderSection();
    expect(await screen.findByRole("checkbox", { name: "Ask before every write" })).toBeChecked();
  } finally {
    cleanup();
    read.mockRestore();
  }
});
