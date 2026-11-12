import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";

import {
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeadCell,
  TableRow,
  TableShell,
} from "./TableShell";

/**
 * Tailwind's own definitions of the utilities the sticky head and the two densities are written
 * in. happy-dom applies a stylesheet it is given but loads none of the application's generated
 * CSS, so what the shell draws is read from the computed style with these in the document: a head
 * that stopped sticking, or a density that stopped being the tighter one, computes something else
 * here whatever class it carries.
 */
const SHELL_CSS =
  ".sticky{position:sticky}.py-3{padding-top:0.75rem;padding-bottom:0.75rem}" +
  ".py-1{padding-top:0.25rem;padding-bottom:0.25rem}";

function withStylesheet(body: () => void): void {
  const style = document.createElement("style");
  style.textContent = SHELL_CSS;
  document.head.appendChild(style);
  try {
    body();
  } finally {
    style.remove();
  }
}

/** The box the shell draws the table in. */
function shell(): HTMLElement {
  const found = document.querySelector<HTMLElement>('[data-control="table-shell"]');
  if (found === null) throw new Error("no table shell is drawn");
  return found;
}

function Projects({ rows }: { rows: readonly string[] }) {
  return (
    <TableShell className="mt-6 max-w-4xl" label="Projects">
      <TableHead>
        <TableHeadCell>Key</TableHeadCell>
        <TableHeadCell>
          <span className="sr-only">Actions</span>
        </TableHeadCell>
      </TableHead>
      <TableBody>
        {rows.length === 0 ? (
          <TableEmpty colSpan={2}>No projects yet.</TableEmpty>
        ) : (
          rows.map((row) => (
            <TableRow data-project={row} key={row}>
              <TableCell mono tone="primary">
                {row}
              </TableCell>
              <TableCell align="right">
                <button type="button">Delete</button>
              </TableCell>
            </TableRow>
          ))
        )}
      </TableBody>
    </TableShell>
  );
}

test("the table takes the caller's name, and the box its layout class and the control's name", () => {
  const view = render(<Projects rows={["CORE"]} />);
  try {
    expect(screen.getByRole("table", { name: "Projects" })).toBeDefined();
    expect(shell()).toHaveAttribute("data-control", "table-shell");
    // The caller places the box; everything inside it is the shell's.
    expect(shell().className).toContain("mt-6 max-w-4xl");
    expect(shell().contains(screen.getByRole("table", { name: "Projects" }))).toBe(true);
  } finally {
    view.unmount();
  }
});

test("a header cell is a column header, and a row's data attributes and cells are the caller's", () => {
  const view = render(<Projects rows={["CORE"]} />);
  try {
    const key = screen.getByRole("columnheader", { name: "Key" });
    expect(key.tagName).toBe("TH");
    expect(key).toHaveAttribute("scope", "col");
    // The header a reader hears but does not see is still a column header.
    expect(screen.getByRole("columnheader", { name: "Actions" })).toBeDefined();

    const row = screen.getByRole("row", { name: /CORE/ });
    expect(row).toHaveAttribute("data-project", "CORE");
    expect(within(row).getAllByRole("cell")).toHaveLength(2);
    expect(within(row).getByRole("button", { name: "Delete" })).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("the empty state is the one line in place of the rows, spanning every column", () => {
  const view = render(<Projects rows={[]} />);
  try {
    const empty = screen.getByRole("cell", { name: "No projects yet." });
    expect(empty).toHaveAttribute("colspan", "2");
    expect(screen.getAllByRole("row")).toHaveLength(2);
  } finally {
    view.unmount();
  }
});

test("a head cell carries the caller's own attributes through", () => {
  const view = render(
    <TableShell label="Slowest jobs" size="compact">
      <TableHead>
        <TableHeadCell align="right" colSpan={2} title="Both halves of the week">
          Median
        </TableHeadCell>
      </TableHead>
      <TableBody>
        <TableRow>
          <TableCell colSpan={2}>93m</TableCell>
        </TableRow>
      </TableBody>
    </TableShell>
  );
  try {
    const median = screen.getByRole("columnheader", { name: "Median" });
    expect(median).toHaveAttribute("colspan", "2");
    expect(median).toHaveAttribute("title", "Both halves of the week");
    expect(screen.getByRole("cell", { name: "93m" })).toHaveAttribute("colspan", "2");
  } finally {
    view.unmount();
  }
});

test("the head stays at the top of the box only when the caller asks for it", () => {
  withStylesheet(() => {
    const loose = render(
      <TableShell label="Per day">
        <TableHead>
          <TableHeadCell>Day</TableHeadCell>
        </TableHead>
        <TableBody>
          <TableRow>
            <TableCell>2026-09-05</TableCell>
          </TableRow>
        </TableBody>
      </TableShell>
    );
    try {
      const head = screen.getByRole("table", { name: "Per day" }).querySelector("thead");
      expect(head).not.toBeNull();
      expect(getComputedStyle(head as HTMLElement).position).not.toBe("sticky");
    } finally {
      loose.unmount();
    }

    const stuck = render(
      <TableShell label="Per day" size="compact" stickyHead>
        <TableHead>
          <TableHeadCell>Day</TableHeadCell>
        </TableHead>
        <TableBody>
          <TableRow>
            <TableCell>2026-09-05</TableCell>
          </TableRow>
        </TableBody>
      </TableShell>
    );
    try {
      const head = screen.getByRole("table", { name: "Per day" }).querySelector("thead");
      expect(getComputedStyle(head as HTMLElement).position).toBe("sticky");
    } finally {
      stuck.unmount();
    }
  });
});

test("the compact density draws a shorter cell than the comfortable one", () => {
  withStylesheet(() => {
    // happy-dom reports the computed length in pixels: 0.75rem and 0.25rem at the 16px root.
    for (const [size, padding] of [
      ["comfortable", "12px"],
      ["compact", "4px"],
    ] as const) {
      const view = render(
        <TableShell label={`Jobs ${size}`} size={size}>
          <TableHead>
            <TableHeadCell>Job</TableHeadCell>
          </TableHead>
          <TableBody>
            <TableRow>
              <TableCell>build / unit</TableCell>
            </TableRow>
          </TableBody>
        </TableShell>
      );
      try {
        const cell = screen.getByRole("cell", { name: "build / unit" });
        expect(getComputedStyle(cell).paddingTop).toBe(padding);
      } finally {
        view.unmount();
      }
    }
  });
});
