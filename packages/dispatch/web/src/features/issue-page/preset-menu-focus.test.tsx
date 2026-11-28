import { expect, spyOn, test } from "bun:test";
import { render, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { IssuePresets } from "./IssuePresets";
import { presetPath } from "./issue-address";

test("choosing a view returns focus to the control before removing the menu", async () => {
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter initialEntries={[presetPath("CORE-1", "conversation")]}>
      <IssuePresets active="conversation" issueKey="CORE-1" />
    </MemoryRouter>
  );
  const chooser = view.getByRole("button", { name: "Conversation" });
  const focus = chooser.focus.bind(chooser);
  try {
    await user.click(chooser);
    const menu = view.getByRole("menu");
    const returnFocus = spyOn(chooser, "focus").mockImplementation((options) => {
      expect(menu.isConnected, "focus returned after the menu was removed").toBe(true);
      focus(options);
    });
    try {
      await user.click(within(menu).getByRole("menuitemradio", { name: "Spec" }));
      expect(returnFocus).toHaveBeenCalledTimes(1);
      expect(document.activeElement).toBe(chooser);
      expect(view.queryByRole("menu")).toBeNull();
    } finally {
      returnFocus.mockRestore();
    }
  } finally {
    view.unmount();
    restoreViewport();
  }
});
