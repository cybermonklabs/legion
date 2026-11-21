import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRoute } from "../../__tests__/send-answers";
import { ApiError, api, type WriteBounds } from "../../api/client";
import type { GoingSettings } from "../../api/types";
import { CommandBarSection } from "./CommandBarSection";

// The person's own command-bar mode, as the Settings page shows it: read from Dispatch, Ask for a
// person who never chose one, and written back by the choice.

function renderSection(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <CommandBarSection />
    </QueryClientProvider>
  );
}

test("the mode is Ask by default, and choosing Auto-review writes it to Dispatch", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "myGoingSettings").mockResolvedValue({ mode: "ask" });
  const put = answerRoute<(input: GoingSettings, bounds: WriteBounds) => Promise<GoingSettings>>(
    "PUT",
    "/api/v1/me/going/settings"
  );
  put.mockImplementation(async (input) => input);
  try {
    renderSection();
    const ask = await screen.findByRole("radio", { name: "Ask" });
    const review = screen.getByRole("radio", { name: "Auto-review" });
    expect(ask).toBeChecked();
    expect(review).not.toBeChecked();

    await user.click(review);

    await waitFor(() => expect(put).toHaveBeenCalledWith({ mode: "auto_review" }, keyedAttempt));
    await waitFor(() => expect(review).toBeChecked());
    expect(ask).not.toBeChecked();
  } finally {
    cleanup();
    read.mockRestore();
  }
});

test("a mode Dispatch holds is read back", async () => {
  const read = spyOn(api, "myGoingSettings").mockResolvedValue({ mode: "auto_review" });
  try {
    renderSection();
    expect(await screen.findByRole("radio", { name: "Auto-review" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "Ask" })).not.toBeChecked();
  } finally {
    cleanup();
    read.mockRestore();
  }
});

test("a click shows the choice at once, and a save Dispatch refuses puts the mode back and says why", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "myGoingSettings").mockResolvedValue({ mode: "ask" });
  const answer = Promise.withResolvers<GoingSettings>();
  const put = answerRoute<(input: GoingSettings, bounds: WriteBounds) => Promise<GoingSettings>>(
    "PUT",
    "/api/v1/me/going/settings"
  );
  put.mockReturnValue(answer.promise);
  try {
    renderSection();
    const review = await screen.findByRole("radio", { name: "Auto-review" });

    await user.click(review);

    // Dispatch has not answered, and the choice already shows what the person chose.
    expect(review).toBeChecked();
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    answer.reject(
      new ApiError(400, { code: "INVALID_STATE", error: "mode is required: ask or auto_review" })
    );

    await waitFor(() => expect(review).not.toBeChecked());
    expect(screen.getByRole("radio", { name: "Ask" })).toBeChecked();
    expect(await screen.findByText("mode is required: ask or auto_review")).toBeInTheDocument();
  } finally {
    cleanup();
    read.mockRestore();
  }
});
