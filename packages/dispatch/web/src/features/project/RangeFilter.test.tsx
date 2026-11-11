import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { RangeFilter } from "./RangeFilter";

function RangeFilterHarness() {
  const [value, setValue] = useState<string | undefined>();
  return (
    <>
      <RangeFilter onChange={setValue} value={value} />
      <output>{value}</output>
    </>
  );
}

test("a closed-date preset writes its range value", async () => {
  const user = userEvent.setup();
  const view = render(<RangeFilterHarness />);

  try {
    await user.click(screen.getByRole("button", { name: "7 days" }));
    expect(screen.getByRole("status").textContent).toBe("7d");
  } finally {
    view.unmount();
  }
});

test("a from and to date write one closed window", async () => {
  const user = userEvent.setup();
  const view = render(<RangeFilterHarness />);

  try {
    await user.type(screen.getByLabelText("From"), "2026-11-03");
    await user.type(screen.getByLabelText("To"), "2026-11-04");
    expect(screen.getByRole("status").textContent).toBe("2026-11-03..2026-11-04");
  } finally {
    view.unmount();
  }
});
