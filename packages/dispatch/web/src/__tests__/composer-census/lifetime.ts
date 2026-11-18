/**
 * The composer census (INFRA-1503), its lifetimes: the tree ends and starts again. A page reload
 * does that to every host at once, whoever starts it: the browser's own reload, and the app's,
 * which `features/shell/DeploymentResilience.tsx` starts after a deploy (`reloadForChunkFailure`
 * for a chunk the new build no longer serves, and the Reload of its new-version notice), and
 * every other `location.reload()` the app makes. So each mount is one lifetime item, whatever
 * ends its tree: what a reload means for a draft (the held sends and the drafts kept in
 * `sessionStorage`) is the same whichever reload it was, and the reloads are named in the item's
 * words only.
 */
import * as ts from "typescript";

import { type Program, text, where } from "./program";
import type { Trees } from "./trees";

export interface LifetimeItem {
  readonly host: string;
  readonly mount: string;
  readonly expression: string;
  readonly at: string;
  readonly mounts: readonly string[];
  /** The reloads that end the tree, the browser's first. */
  readonly reloads: readonly string[];
}

/** The name of the top-level declaration a node is part of. */
function declarationOf(node: ts.Node): string {
  let current: ts.Node = node;
  while (!ts.isSourceFile(current.parent)) current = current.parent;
  if (
    (ts.isFunctionDeclaration(current) || ts.isClassDeclaration(current)) &&
    current.name !== undefined
  ) {
    return current.name.text;
  }
  if (ts.isVariableStatement(current)) {
    const first = current.declarationList.declarations[0];
    if (first !== undefined && ts.isIdentifier(first.name)) return first.name.text;
  }
  return "(the module)";
}

/** A reload as its reader meets it: the control whose press starts it, with the words on that
 *  control, or the function that starts it on its own. */
function reloadName(call: ts.CallExpression): string {
  const owner = declarationOf(call);
  for (let current: ts.Node = call; !ts.isSourceFile(current); current = current.parent) {
    if (ts.isJsxAttribute(current)) {
      const element = current.parent.parent;
      const tag = ts.isJsxOpeningElement(element) ? text(element.tagName) : "an element";
      const words = ts.isJsxOpeningElement(element)
        ? element.parent.children
            .filter(ts.isJsxText)
            .map((child) => child.text.trim())
            .join(" ")
            .trim()
        : "";
      const label = words === "" ? "" : ` "${words}"`;
      return `${owner}'s <${tag}>${label} (${current.name.getText()})`;
    }
    if (ts.isFunctionDeclaration(current) && current.name !== undefined) {
      return `${current.name.text}()`;
    }
  }
  return owner;
}

/** Every `location.reload()` of the app, named, with where it is. */
function appReloads(program: Program): string[] {
  const found: string[] = [];
  for (const mod of program.mods.values()) {
    const visit = (node: ts.Node) => {
      if (
        ts.isCallExpression(node) &&
        ts.isPropertyAccessExpression(node.expression) &&
        node.expression.name.text === "reload" &&
        /(^|\.)location$/.test(node.expression.expression.getText())
      ) {
        found.push(`${reloadName(node)} at ${where(node)}`);
      }
      ts.forEachChild(node, visit);
    };
    visit(mod.sf);
  }
  return found;
}

/** One lifetime item per mount. */
export function readLifetimes(trees: Trees): LifetimeItem[] {
  const reloads = ["the browser's own reload", ...appReloads(trees.program)];
  return trees.mounts.map((mount) => ({
    at: where(mount.element),
    expression: mount.shown,
    host: mount.host.name,
    mount: "MentionComposer",
    mounts: [mount.name],
    reloads,
  }));
}
