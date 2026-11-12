import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { Chip } from "./Chip";

test("a chip presses, says whether it is selected, and names itself to a check", async () => {
  const user = userEvent.setup();
  const presses: number[] = [];
  const view = render(
    <Chip onClick={() => presses.push(1)} selected>
      docs
    </Chip>
  );
  try {
    const chip = screen.getByRole("button", { name: "docs" });
    expect(chip).toHaveAttribute("data-control", "chip");
    expect(chip).toHaveAttribute("aria-pressed", "true");
    await user.click(chip);
    expect(presses).toEqual([1]);
  } finally {
    view.unmount();
  }
});

test("the compact size is the same chip: it presses and names itself as ever", async () => {
  const user = userEvent.setup();
  const presses: number[] = [];
  const view = render(
    <Chip
      aria-label="Remove Label: docs filter"
      onClick={() => presses.push(1)}
      removable
      size="compact"
    >
      docs
    </Chip>
  );
  try {
    const chip = screen.getByRole("button", { name: "Remove Label: docs filter" });
    expect(chip).toHaveAttribute("data-control", "chip");
    await user.click(chip);
    expect(presses).toEqual([1]);
  } finally {
    view.unmount();
  }
});
