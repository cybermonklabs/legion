import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, within } from "@testing-library/react";

import { api } from "../../api/client";
import type { ExternalLink } from "../../api/types";
import { GitHubLink } from "./GitHubLink";

function renderLink(link: ExternalLink) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <GitHubLink link={link} />
    </QueryClientProvider>
  );
  return { ...view, q: within(view.container) };
}

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });

// A checks read that failed is said where its result would stand, so a reader tells it from one
// that has not answered yet; the title and the state already read stay (dispatch://INFRA-5200,
// c-review F5).
test("a pull request whose checks could not be read says so beside its title and state", async () => {
  const githubRest = spyOn(api, "githubRest").mockImplementation(async (path) => {
    if (path === "repos/owner/repository/pulls/7") {
      return json({ head: { sha: "abc123" }, state: "open", title: "Ship the header" });
    }
    throw new Error(`GitHub refused ${path}`);
  });
  const view = renderLink({ kind: "github_pr", url: "https://github.com/owner/repository/pull/7" });
  try {
    const link = await view.q.findByRole("link");
    expect(await within(link).findByText("checks: unavailable")).toBeDefined();
    expect(within(link).getByText("Ship the header")).toBeDefined();
    expect(within(link).getByText("open")).toBeDefined();
  } finally {
    view.unmount();
    githubRest.mockRestore();
  }
});

// An address that is no safe link is text no click opens, cut to its cell; its title carries the
// whole address and why it is not a link (dispatch://INFRA-5200, c-review F4).
test("an unsafe address is no link, and its title holds the whole address", () => {
  const url = `javascript:alert(${"1".repeat(80)})`;
  const view = renderLink({ kind: "url", url });
  try {
    expect(view.q.queryByRole("link")).not.toBeInTheDocument();
    const text = view.q.getByText(url);
    expect(text).toHaveAttribute("title", `Unsafe external link, not opened: ${url}`);
  } finally {
    view.unmount();
  }
});
