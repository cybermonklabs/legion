import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { onFakeClock, settle, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRoute } from "../../__tests__/send-answers";
import { ApiError, api, type WriteBounds } from "../../api/client";
import type { GoingSettings } from "../../api/types";
import { CommandBarSection, SAVED_SHOWS_MS } from "./CommandBarSection";

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

test("the approval mode is Ask by default, and choosing Auto-review writes it to Dispatch and says Saved beside it", async () => {
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
    // The setting is named as spec v61 names it, inside a card like every other section's.
    const group = screen.getByRole("group", { name: "Approval mode" });
    expect(group.className).toContain("rounded-xl");
    expect(review).toHaveAccessibleDescription(
      "A review of the plan lets through the plain changes your words ask for."
    );
    expect(screen.getByTestId("going-settings-saved").textContent).toBe("");
    expect(screen.queryByTestId("going-settings-saved-mark")).toBeNull();
    // The section says what the bar does in one sentence; each radio's line says the rest.
    expect(screen.getByText("The bar plans its changes before it makes any.")).toBeInTheDocument();

    await user.click(review);

    await waitFor(() => expect(put).toHaveBeenCalledWith({ mode: "auto_review" }, keyedAttempt));
    await waitFor(() => expect(review).toBeChecked());
    expect(ask).not.toBeChecked();
    await waitFor(() =>
      expect(screen.getByTestId("going-settings-saved")).toHaveTextContent("Saved: Auto-review.")
    );
    // The choice saves as it is made, so "Saved" stands beside the mode it saved (review: Settings).
    const mark = screen.getByTestId("going-settings-saved-mark");
    expect(mark).toHaveTextContent("✓ Saved");
    expect(mark.parentElement).toContainElement(screen.getByText("Auto-review"));
  } finally {
    cleanup();
    read.mockRestore();
  }
});

// The choice saves in the press itself and the radio already shows it, so "Saved" stands beside
// the mode for a moment and then goes (review: Settings): a mark that stayed would read as how
// the page stands, not as what the press just did. The next choice says it again.
test("Saved stands beside the mode a choice saved for a moment, then goes, and the next choice says it again", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "myGoingSettings").mockResolvedValue({ mode: "ask" });
  const put = answerRoute<(input: GoingSettings, bounds: WriteBounds) => Promise<GoingSettings>>(
    "PUT",
    "/api/v1/me/going/settings"
  );
  const answer = Promise.withResolvers<GoingSettings>();
  put.mockImplementationOnce(() => answer.promise);
  put.mockImplementation(async (input) => input);
  try {
    renderSection();
    const review = await screen.findByRole("radio", { name: "Auto-review" });
    await user.click(review);
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(screen.queryByTestId("going-settings-saved-mark")).toBeNull();

    // Dispatch answers the save on a held clock, so the moment "Saved" stands for is the row's
    // to cross.
    await onFakeClock(async () => {
      await act(async () => {
        answer.resolve({ mode: "auto_review" });
        await settle();
      });
      expect(screen.getByTestId("going-settings-saved-mark")).toHaveTextContent("✓ Saved");
      expect(screen.getByTestId("going-settings-saved")).toHaveTextContent("Saved: Auto-review.");
      await tick(SAVED_SHOWS_MS - 200);
      expect(screen.getByTestId("going-settings-saved-mark")).toBeInTheDocument();
      await tick(400);
      expect(screen.queryByTestId("going-settings-saved-mark")).toBeNull();
      expect(screen.getByTestId("going-settings-saved").textContent).toBe("");
    });
    expect(review).toBeChecked();

    await user.click(screen.getByRole("radio", { name: "Ask" }));
    await waitFor(() =>
      expect(screen.getByTestId("going-settings-saved")).toHaveTextContent("Saved: Ask.")
    );
    expect(screen.getByTestId("going-settings-saved-mark").parentElement).toContainElement(
      screen.getByText("Ask")
    );
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
