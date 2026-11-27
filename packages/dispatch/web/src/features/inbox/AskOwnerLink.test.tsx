import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { plannerAgentRow } from "../threads/replies-fixture";
import { AskOwnerLink, ThreadOwnerLine } from "./AskOwnerLink";

const LONG_TITLE =
  "The sandbox tree checkpoint fences and the node release that follows them on the cluster";

// An owner that opens the owner's hover card shows its whole name there, so it carries no native
// title: a title would draw a second tooltip over that card. An owner that opens no card - an
// Agent row's session - carries its whole name as its title instead.

test("an issue owner opens the issue's hover card, carries no title, and cuts only its title; the key stays whole", () => {
  const view = render(
    <MemoryRouter>
      <AskOwnerLink owner={{ issue: { key: "CORE-12", title: LONG_TITLE } }} />
    </MemoryRouter>
  );
  try {
    const link = view.getByRole("link");
    expect(link).toHaveAttribute("data-dispatch-ref", expect.stringContaining("CORE-12"));
    expect(link).not.toHaveAttribute("title");
    expect(link.querySelector("[title]")).toBeNull();
    expect(link.querySelector<HTMLElement>(".truncate")?.textContent).toBe(LONG_TITLE);
    // The key is its own element, never the one that shortens.
    const key = link.querySelector<HTMLElement>(".shrink-0");
    expect(key?.textContent).toBe("CORE-12");
    expect(key?.classList.contains("truncate")).toBe(false);
  } finally {
    view.unmount();
  }
});

test("a document owner opens the document's hover card and carries no title", () => {
  const name = "Runbook for the nightly ledger replay and the audit export that follows it";
  const view = render(
    <MemoryRouter>
      <AskOwnerLink owner={{ document: { name, project: "CORE", slug: "runbook" } }} />
    </MemoryRouter>
  );
  try {
    const link = view.getByRole("link");
    expect(link).toHaveAttribute("data-dispatch-ref", expect.stringContaining("runbook"));
    expect(link).not.toHaveAttribute("title");
    expect(link.querySelector("[title]")).toBeNull();
    expect(link.querySelector<HTMLElement>("span.truncate")?.textContent).toBe(`CORE · ${name}`);
  } finally {
    view.unmount();
  }
});

test("an Agent row's owner opens no hover card and carries its whole session name as a title", () => {
  const sessionName = "Reviewing PR #1489 production-check screen fixes at 390px";
  const view = render(
    <MemoryRouter>
      <ThreadOwnerLine
        className="truncate text-sm"
        thread={plannerAgentRow({ session: { id: "planner", name: sessionName } })}
      />
    </MemoryRouter>
  );
  try {
    const owner = view.getByText(sessionName);
    expect(owner).toHaveAttribute("title", sessionName);
    expect(owner.closest("[data-dispatch-ref]")).toBeNull();
  } finally {
    view.unmount();
  }
});
