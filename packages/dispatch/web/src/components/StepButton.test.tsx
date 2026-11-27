import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { MOUSE_CSS, SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import type { ControlSize } from "./control-names";
import { StepButton } from "./StepButton";

test("a step takes the page's size: the comfortable padding and 44 px by default, the compact padding with 44 px given up from md", () => {
  const step = (size?: ControlSize) => (
    <StepButton ariaDisabled={false} onClick={() => undefined} size={size}>
      Next ›
    </StepButton>
  );
  const view = render(step());
  // What the step draws under `css`: a window below `md` (`SIZE_CSS` alone), or from it.
  const drawn = (css: string) => sizeUnder(screen.getByRole("button", { name: "Next ›" }), css);
  try {
    const comfortable = { minHeight: "44px", padding: "8px 12px 8px 12px" };
    expect(drawn(SIZE_CSS)).toMatchObject(comfortable);
    expect(drawn(`${SIZE_CSS}${MOUSE_CSS}`)).toMatchObject(comfortable);
    view.rerender(step("comfortable"));
    expect(drawn(SIZE_CSS)).toMatchObject(comfortable);
    expect(drawn(`${SIZE_CSS}${MOUSE_CSS}`)).toMatchObject(comfortable);
    view.rerender(step("compact"));
    expect(drawn(SIZE_CSS)).toMatchObject({ minHeight: "44px", padding: "4px 8px 4px 8px" });
    expect(drawn(`${SIZE_CSS}${MOUSE_CSS}`)).toMatchObject({
      minHeight: "32px",
      padding: "4px 8px 4px 8px",
    });
  } finally {
    view.unmount();
  }
});
