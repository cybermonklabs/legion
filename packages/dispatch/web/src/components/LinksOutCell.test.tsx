import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { LinksOutCell } from "./LinksOutCell";

// The caller that asked for it: the evals tab of dispatch://INFRA-2918, whose rows behind a
// number draw each row's links out to the results they were read from, each with its label, its
// relation and its host, and each opening away from the application.
test("draws each of a row's links out with its relation and its host, opening elsewhere", () => {
  const view = render(
    <LinksOutCell
      links={[
        { href: "https://ci.example.com/runs/41", label: "Run 41", relation: "source-rule" },
        { href: "https://docs.example.org/method", label: "Method", relation: "describedby" },
      ]}
    />
  );
  try {
    const run = screen.getByRole("link", { name: "Run 41" });
    expect(run).toHaveAttribute("href", "https://ci.example.com/runs/41");
    expect(run).toHaveAttribute("target", "_blank");
    expect(run).toHaveAttribute("rel", "noreferrer");
    expect(screen.getByText("source-rule · ci.example.com")).toBeInTheDocument();

    const method = screen.getByRole("link", { name: "Method" });
    expect(method).toHaveAttribute("href", "https://docs.example.org/method");
    expect(screen.getByText("describedby · docs.example.org")).toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// A reading can carry no link at all, and the cell says so rather than drawing an empty line.
test("says the caller's own words where the row has no link", () => {
  const view = render(<LinksOutCell empty="no results" links={[]} />);
  try {
    expect(screen.getByText("no results")).toBeInTheDocument();
    expect(screen.queryByRole("link")).toBeNull();
  } finally {
    view.unmount();
  }
});
