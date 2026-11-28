import { decodeBlockId, encodeBlockId, itemFromSearch } from "@legion/contracts";
import { isCleanUrl } from "@legion/contracts/log-text";

/**
 * The dispatch:// form of a dashboard URL on the configured server, or undefined when the
 * value is not such a URL. Accepts the issue, spec, artifact, ask, comment, log and activity
 * pages, the Decisions page (`dispatch://KEY/decisions`, the issue's), project document pages (with
 * their ?ask= / ?comment= deep links, which name the item whatever the fragment), the
 * `#b-<block id>` of a live document page or of an issue ask's, comment's or message's page,
 * which names that block, and the page of an upload an agent's conversation owns
 * (`/agents/<session id>/artifacts/<slug>`).
 */
export function dispatchRefFromUrl(value: string, serverUrl: string): string | undefined {
  let url: URL;
  let origin: string;
  try {
    url = new URL(value);
    origin = new URL(serverUrl).origin;
  } catch {
    return undefined;
  }
  if (url.origin !== origin) return undefined;
  // The SPA's version selector: `?v=N` on issue pages, `?version=N` on project document pages.
  const versionOf = (name: string): string => {
    const value = url.searchParams.get(name);
    return value !== null && /^[1-9][0-9]*$/.test(value) ? `@v${value}` : "";
  };
  // The block a page lands on (`#b-<block id>`), written as `encodeBlockId` writes it, a pin being
  // written only as a dispatch:// reference. A document's version page (`versionName` set) names
  // the document whatever its fragment: a version's stored markdown carries no block ids to land
  // on.
  const blockOf = (versionName?: string): string => {
    const id = url.hash.startsWith("#b-") ? decodeBlockId(url.hash.slice(3)) : undefined;
    return id === undefined || (versionName !== undefined && url.searchParams.get(versionName))
      ? ""
      : `#b-${encodeBlockId(id)}`;
  };
  const issuePage = url.pathname.match(
    /^\/issues\/([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?:\/(spec|log|conversation|activity|children|decisions)|\/artifacts\/([^/]+)|\/asks\/([^/]+)|\/comments\/([^/]+)|\/messages\/([^/]+))?\/?$/
  );
  if (issuePage) {
    const [, key = "", page, artifact, ask, comment, message] = issuePage;
    const version = versionOf("v");
    // A document page carrying `?comment=`/`?ask=` names that item, not the document, whatever
    // its fragment (the dashboard writes a document block's fragment beside `?comment=`): that is
    // the href `dispatch_search` returns for an anchored comment or ask on an issue's document.
    if (page === "spec" || artifact !== undefined) {
      const item = itemFromSearch(url.search);
      if (item === null) return undefined;
      if (item !== undefined) return `dispatch://${key}/${item.kind}/${item.id}`;
    }
    if (page === "spec" && version !== "") return `dispatch://${key}/artifact/spec${version}`;
    if (page === "spec") return `dispatch://${key}/spec${blockOf("v")}`;
    // `/conversation` keeps resolving to the reference it always had, `KEY/log`; `/activity`,
    // the tab that now holds the event list, is spelled as its own path is, and so is
    // `/decisions`, the issue's Decisions page, whatever its fragment.
    if (page !== undefined) return `dispatch://${key}/${page === "conversation" ? "log" : page}`;
    if (artifact !== undefined) {
      const slug = decodeURIComponent(artifact);
      return `dispatch://${key}/artifact/${slug}${version}${blockOf("v")}`;
    }
    if (ask !== undefined) return `dispatch://${key}/ask/${ask}${blockOf()}`;
    if (comment !== undefined) return `dispatch://${key}/comment/${comment}${blockOf()}`;
    if (message !== undefined) return `dispatch://${key}/message/${message}${blockOf()}`;
    return `dispatch://${key}`;
  }
  const agentArtifactPage = url.pathname.match(/^\/agents\/([^/]+)\/artifacts\/([^/]+)\/?$/);
  if (agentArtifactPage) {
    const [, session, slug] = agentArtifactPage;
    try {
      return `dispatch://agent/${decodeURIComponent(session ?? "")}/artifact/${decodeURIComponent(slug ?? "")}${versionOf("v")}`;
    } catch {
      return undefined;
    }
  }
  const documentPage = url.pathname.match(
    /^\/projects\/([A-Z][A-Z0-9]{1,9})\/documents\/([^/]+)\/?$/
  );
  if (!documentPage) return undefined;
  const [, project, slug] = documentPage;
  const decodedSlug = decodeURIComponent(slug ?? "");
  const document = `dispatch://${project}/artifact/${decodedSlug}`;
  const item = itemFromSearch(url.search);
  if (item === null) return undefined;
  // An ask or comment has no version; the page's version selector names the document's.
  return item === undefined
    ? `${document}${versionOf("version")}${blockOf("version")}`
    : `${document}/${item.kind}/${item.id}`;
}

/**
 * The base every link a command prints is built on: `base` (DISPATCH_PUBLIC_URL, else the
 * configured Dispatch URL) where it is a plain http(s) URL (`isCleanUrl`), and undefined where it
 * is not. A base that carries user info, a query, a fragment or an escape cannot be printed in any
 * part (`urlForText`), so no link is built on it; each link prints as its `dispatch://` reference
 * instead (`linkFor`), and the session's start notice names the setting (`linkNotice`,
 * dispatch-config.ts).
 */
export function linkBaseOf(base: string): string | undefined {
  return isCleanUrl(base) ? base : undefined;
}

/** A stand-in origin that turns a dashboard path into a URL `dispatchRefFromUrl` reads. It is
 *  never printed: a path it does not read prints as the server sent it. */
const STAND_IN = "http://dispatch.invalid";

/**
 * `href`, a dashboard path or a URL Dispatch answered, as a printed link: absolute against
 * `linkBase` where there is one (`linkBaseOf`), and otherwise the `dispatch://` reference of the
 * page it names (`dispatch://DSP-1`, `dispatch://DSP-1/ask/<id>`), which `dispatch read` reads
 * as the link would have opened it. A href no reference names prints as the server sent it.
 */
export function linkFor(href: string, linkBase: string | undefined): string {
  if (linkBase !== undefined) {
    try {
      return new URL(href, linkBase).toString();
    } catch {
      return href;
    }
  }
  let page: string;
  try {
    page = new URL(href, STAND_IN).toString();
  } catch {
    return href;
  }
  return dispatchRefFromUrl(page, STAND_IN) ?? href;
}
