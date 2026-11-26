import { afterEach, expect, spyOn, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";

import { LinkList, type ListedLink } from "./LinkList";

// The links out to results (dispatch://INFRA-2918/spec R10, plan D12, T16): each drawn with its
// label, its relation and its host, the four relations of R10 each apart from a plain link, and
// nothing read from the far side.

afterEach(cleanup);

/** A link of relation `rel` to a result page of its own on results.example.com. */
function linkOf(rel: string, label = `the ${rel} result`): ListedLink {
  return { href: `https://results.example.com/runs/7/${rel}`, label, rel };
}

/** The list item that draws the link labelled `label`. */
function itemOf(label: string): HTMLElement {
  const item = screen.getByRole("link", { name: label }).closest("li");
  if (item === null) throw new Error(`no list item holds ${label}`);
  return item;
}

test("a link draws its label, its relation and its host", () => {
  render(<LinkList label="Links" links={[linkOf("describedby", "The method page")]} />);
  const link = screen.getByRole("link", { name: "The method page" });
  expect(link).toHaveAttribute("href", "https://results.example.com/runs/7/describedby");
  expect(link).toHaveAttribute("target", "_blank");
  const item = itemOf("The method page");
  expect(item.querySelector("[data-link-relation]")?.textContent).toBe("Described by");
  expect(item.querySelector("[data-link-host]")?.textContent).toBe("results.example.com");
  expect(screen.getByRole("list", { name: "Links" })).toContainElement(link);
});

test("the four relations each draw with an icon and words of their own, apart from a plain one", () => {
  const special = ["via", "describedby", "enclosure", "source-rule"];
  render(<LinkList label="Links" links={[...special, "related"].map((rel) => linkOf(rel))} />);
  const words = special.map((rel) => {
    const link = screen.getByRole("link", { name: `the ${rel} result` });
    expect(link).toHaveAttribute("data-link-kind", rel);
    expect(link.querySelector("svg")).not.toBeNull();
    return itemOf(`the ${rel} result`).querySelector("[data-link-relation]")?.textContent;
  });
  expect(words).toEqual(["Via", "Described by", "Attachment", "Source rule"]);
  // Each icon is its own.
  const icons = special.map(
    (rel) => screen.getByRole("link", { name: `the ${rel} result` }).querySelector("svg")?.innerHTML
  );
  expect(new Set(icons).size).toBe(special.length);

  const plain = screen.getByRole("link", { name: "the related result" });
  expect(plain).toHaveAttribute("data-link-kind", "plain");
  expect(plain.querySelector("svg")).toBeNull();
});

test("a link of a relation no rule names draws as a plain link, its relation as written", () => {
  render(
    <LinkList
      label="Links"
      links={[linkOf("x-dashboard", "The dashboard"), linkOf("VIA", "Loud via")]}
    />
  );
  const unknown = screen.getByRole("link", { name: "The dashboard" });
  expect(unknown).toHaveAttribute("data-link-kind", "plain");
  expect(unknown).toHaveAttribute("data-rel", "x-dashboard");
  expect(itemOf("The dashboard").querySelector("[data-link-relation]")?.textContent).toBe(
    "x-dashboard"
  );
  // A relation is matched without regard to case.
  expect(screen.getByRole("link", { name: "Loud via" })).toHaveAttribute("data-link-kind", "via");
});

test("a link whose target is gone draws as any other", () => {
  const gone = {
    href: "https://gone.example.invalid/deleted-run",
    label: "Deleted run",
    rel: "via",
  };
  render(<LinkList label="Links" links={[gone, linkOf("via", "Kept run")]} />);
  const drawn = (label: string) => {
    const item = itemOf(label);
    return {
      kind: within(item).getByRole("link").getAttribute("data-link-kind"),
      relation: item.querySelector("[data-link-relation]")?.textContent,
    };
  };
  expect(drawn("Deleted run")).toEqual(drawn("Kept run"));
  expect(itemOf("Deleted run").querySelector("[data-link-host]")?.textContent).toBe(
    "gone.example.invalid"
  );
});

test("drawing the links reads nothing of the far side", () => {
  const fetched = spyOn(globalThis, "fetch");
  try {
    render(
      <LinkList
        label="Links"
        links={["via", "describedby", "enclosure", "source-rule", "related"].map((rel) =>
          linkOf(rel)
        )}
      />
    );
    expect(screen.getAllByRole("link")).toHaveLength(5);
    expect(fetched).not.toHaveBeenCalled();
    // No picture of the far side either: an image would be a read of it.
    expect(document.querySelector("img")).toBeNull();
  } finally {
    fetched.mockRestore();
  }
});
