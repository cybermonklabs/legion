import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { dangerText, successText } from "../theme/classes";
import { TargetCell } from "./TargetCell";

const percent = (value: number) => `${(value * 100).toFixed(0)}%`;

// The caller that asked for it: the evals tab of dispatch://INFRA-2918, whose target cards and
// eval rows draw a reading against the target it is held to, with its verdict.
test("draws a reading over its floor with the difference between them", () => {
  const view = render(<TargetCell format={percent} target={0.9} value={0.94} />);
  try {
    expect(screen.getByText("94%")).toBeInTheDocument();
    expect(screen.getByText("of 90%")).toBeInTheDocument();
    expect(screen.getByText("+4%")).toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// A measure held to a ceiling — a cost, a failure rate — meets its target by staying under it,
// so the verdict is the caller's rule rather than the sign of the difference.
test("a reading under its ceiling has met its target, and one over it has not", () => {
  const under = render(
    <TargetCell direction="atMost" format={percent} target={0.05} value={0.02} />
  );
  try {
    expect(screen.getByText("2%")).toHaveClass(...successText.split(" "));
    expect(screen.getByText("-3%")).toHaveClass(...successText.split(" "));
  } finally {
    under.unmount();
  }
  const over = render(
    <TargetCell direction="atMost" format={percent} target={0.05} value={0.09} />
  );
  try {
    expect(screen.getByText("9%")).toHaveClass(...dangerText.split(" "));
    expect(screen.getByText("+4%")).toHaveClass(...dangerText.split(" "));
  } finally {
    over.unmount();
  }
});

// A row whose measure has not been read yet says so, and never counts as zero against its
// target (dispatch://INFRA-2918's "no data", not 0).
test("a row with no reading says so rather than reading as zero", () => {
  const view = render(<TargetCell format={percent} target={0.9} value={undefined} />);
  try {
    expect(screen.getByText("no data")).toBeInTheDocument();
    expect(screen.queryByText("of 90%")).toBeNull();
  } finally {
    view.unmount();
  }
});
