/**
 * The composer census (INFRA-1503), its walk up: from a mount, or from a place a component above
 * mounts is drawn, up through what draws it in its component to the top of that component's code
 * or to a route, into the body of each component it is handed to in a slot, through each helper
 * it is drawn through, and into each function a page registers for a call it is handed to
 * (`CALL_WRAPPERS`). `readTrees` (`trees.ts`) walks up from each mount and each place once and
 * keeps the walk on it (`Mount.walked`, `Edge.walked`): the trees read its branches, keys, routes
 * and early returns, and the forms (`forms.ts`) read the same items and each element it passes,
 * so the two never read two walks that could differ.
 */
import * as ts from "typescript";

import {
  attribute,
  bindingNamed,
  type Comp,
  calleeName,
  type Dep,
  declOf,
  dependsOn,
  drawnIn,
  helperId,
  helperName,
  isModuleHelper,
  isThis,
  lookup,
  modOf,
  moduleRefs,
  type Program,
  referencesOf,
  resolveLocal,
  spreads,
  tagComp,
  tagOf,
  text,
  where,
} from "./program";

export const CONDITIONAL_OPERATORS = new Set([
  ts.SyntaxKind.AmpersandAmpersandToken,
  ts.SyntaxKind.BarBarToken,
  ts.SyntaxKind.QuestionQuestionToken,
]);

/** The kinds of a tree's item: what moves an address or ends or hides a mount. */
export type TreeKind = "address prop" | "conditional" | "key" | "route" | "early return";

/** One thing a walk up from a mount passes. A slot, a helper or an element is only the way the
 *  walk went; the other kinds are the tree's items. */
export interface Item {
  readonly kind: TreeKind | "slot" | "helper" | "element";
  readonly at: string;
  readonly expression: string;
  readonly deps: readonly Dep[];
  /** The component whose code holds the item. */
  readonly unit: Comp;
  /** Whether the walk reached it inside a component it does not climb out of, whose props its
   *  callers decide: a slot's receiver, or the component registering a function it followed. */
  readonly inside: boolean;
  /** The element the walk ends at, where it reached a route. */
  readonly route?: boolean;
  /** The expression the item turns on, which the forms read for the layout. */
  readonly node?: ts.Node;
}

/** A walk up from one place, as the trees and the forms read it. */
export interface Walked {
  /** What the walk passed, in the order it passed it. */
  readonly items: readonly Item[];
  /** The components a slot or a registered function was followed into, which the place sits in. */
  readonly receivers: ReadonlySet<string>;
  /** What between the place and the routes the walk met and cannot read, each with where it is. */
  readonly unread: readonly string[];
}

/** A walk up as it runs: what it has gathered so far, and the nodes it has started from, so that
 *  it starts from none twice. */
interface Walk {
  readonly program: Program;
  readonly items: Item[];
  readonly receivers: Set<string>;
  readonly unread: string[];
  readonly seen: Set<ts.Node>;
}

/** How many steps a walk follows from where it started: each slot followed into the component it
 *  is handed to, each registered function followed into from the call drawing what it answers,
 *  and each helper, local or parameter followed to where it is drawn, is one step.
 *  A place more steps away is unread, so the census fails closed there. */
const MAX_DEPTH = 6;

export function containsReturn(node: ts.Node): boolean {
  let found = false;
  const visit = (current: ts.Node) => {
    if (found || (current !== node && ts.isFunctionLike(current))) return;
    if (ts.isReturnStatement(current)) {
      found = true;
      return;
    }
    ts.forEachChild(current, visit);
  };
  visit(node);
  return found;
}

/** Walks up from `start`, a mount or a place a component is drawn, in `comp`. */
export function walkUp(program: Program, start: ts.Node, comp: Comp): Walked {
  const walk: Walk = { items: [], program, receivers: new Set(), seen: new Set(), unread: [] };
  walkFrom(walk, start, comp, false, 0);
  return { items: walk.items, receivers: walk.receivers, unread: walk.unread };
}

function walkFrom(
  walk: Walk,
  start: ts.Node,
  comp: Comp,
  inside: boolean,
  depth: number,
  initialPath: readonly string[] = [],
  // The function a walk from inside a registered function ends at (`followRegistered`): above
  // it is only how that function was registered, and the walk that met the call drawing what
  // the function answers goes on from that call.
  top?: ts.Node
): void {
  if (walk.seen.has(start)) return;
  walk.seen.add(start);
  if (depth > MAX_DEPTH) {
    unreadAt(walk, start, "sits deeper than the census reads");
    return;
  }
  const push = (item: Omit<Item, "inside" | "unit">) =>
    walk.items.push({ ...item, inside, unit: comp });
  // The properties of an object literal the JSX sits in, outermost first, so a slot handed over
  // as `anchoredThread={{ children: <X /> }}` is followed as `anchoredThread.children`.
  let path = [...initialPath];
  let child: ts.Node = start;
  let current: ts.Node | undefined = start.parent;
  while (current !== undefined && current !== top && child !== comp.fn) {
    if (ts.isPropertyAssignment(current) && child === current.initializer) {
      path.unshift(current.name.getText());
    }
    if (ts.isJsxElement(current) && child !== current.openingElement) {
      // The forms read each element the mount sits in for what the layout changes about it: a
      // class, a role, a dialog's ref.
      push({
        at: where(current.openingElement),
        deps: [],
        expression: `<${text(current.openingElement.tagName)}>`,
        kind: "element",
        node: current.openingElement,
      });
      const key = attribute(current, "key");
      if (key?.initializer !== undefined) {
        push({
          at: where(key),
          deps: dependsOn(key.initializer, comp),
          expression: `<${text(current.openingElement.tagName)} ${text(key)}>`,
          kind: "key",
          node: key.initializer,
        });
      }
      const hidden = attribute(current, "hidden");
      if (hidden !== undefined) {
        push({
          at: where(hidden),
          deps: hidden.initializer === undefined ? [] : dependsOn(hidden.initializer, comp),
          expression: `<${text(current.openingElement.tagName)} ${text(hidden)}>`,
          kind: "conditional",
          node: hidden.initializer,
        });
      }
      const tag = current.openingElement.tagName;
      if (!isPlainTag(tag)) {
        const why = followElement(walk, comp, current, "children", [], depth + 1);
        if (why !== undefined) unreadAt(walk, current.openingElement, `<${text(tag)}> ${why}`);
      }
    } else if (ts.isConditionalExpression(current) && child !== current.condition) {
      push({
        at: where(current.condition),
        deps: dependsOn(current.condition, comp),
        expression: `${child === current.whenTrue ? "" : "!"}(${text(current.condition)}) ? …`,
        kind: "conditional",
        node: current.condition,
      });
    } else if (
      ts.isBinaryExpression(current) &&
      CONDITIONAL_OPERATORS.has(current.operatorToken.kind) &&
      child === current.right
    ) {
      push({
        at: where(current.left),
        deps: dependsOn(current.left, comp),
        expression: `${text(current.left)} ${current.operatorToken.getText()} …`,
        kind: "conditional",
        node: current.left,
      });
    } else if (ts.isIfStatement(current) && child !== current.expression) {
      const arm = child === current.thenStatement ? "if" : "else of if";
      push({
        at: where(current.expression),
        deps: dependsOn(current.expression, comp),
        expression: `${arm} (${text(current.expression)})`,
        kind: "conditional",
        node: current.expression,
      });
    } else if (ts.isCaseClause(current)) {
      const switched = current.parent.parent;
      push({
        at: where(current.expression),
        deps: dependsOn(switched.expression, comp),
        expression: `switch (${text(switched.expression)}) case ${text(current.expression)}`,
        kind: "conditional",
        node: switched.expression,
      });
    } else if (ts.isReturnStatement(current)) {
      earlyReturns(current, comp, push);
    } else if (ts.isJsxAttribute(current)) {
      const element = current.parent.parent;
      const tag = text(tagOf(element) as ts.Node);
      const name = current.name.getText();
      if (name === "element" && tag === "Route") {
        const routePath = attribute(element, "path");
        push({
          at: where(element),
          deps: [],
          expression: `<Route ${routePath === undefined ? "(no path)" : text(routePath)}>`,
          kind: "route",
          route: true,
        });
        return;
      }
      push({
        at: where(current),
        deps: [],
        expression: `passed as <${tag} ${name}={…}>`,
        kind: "slot",
      });
      // An attribute of a plain element (an event handler, a value) draws nothing.
      if (isPlainTag(tagOf(element) as ts.JsxTagNameExpression)) return;
      const why = followElement(walk, comp, element, name, path, depth + 1);
      if (why !== undefined) unreadAt(walk, current, `<${tag} ${name}={…}> ${why}`);
      path = [];
      const key = attribute(element, "key");
      if (key?.initializer !== undefined) {
        push({
          at: where(key),
          deps: dependsOn(key.initializer, comp),
          expression: `<${tag} ${text(key)}>`,
          kind: "key",
          node: key.initializer,
        });
      }
      child = ts.isJsxOpeningElement(element) ? element.parent : element;
      current = child.parent;
      continue;
    } else if (
      (ts.isArrowFunction(current) || ts.isFunctionExpression(current)) &&
      current !== comp.fn &&
      ts.isCallExpression(current.parent) &&
      current.parent.arguments.includes(current as ts.Expression)
    ) {
      // Drawn once per item of a list: an item that leaves the list ends its mount.
      const call = current.parent;
      push({
        at: where(call),
        deps: dependsOn(call.expression, comp),
        expression: `drawn by ${text(call.expression)}(…)`,
        kind: "conditional",
        node: call.expression,
      });
    } else {
      const helper = helperAt(walk.program, current, child, comp);
      if (helper !== undefined) {
        push({ at: where(current), deps: [], expression: helper.name, kind: "helper" });
        if (helper.uses.length === 0) {
          unreadAt(walk, current, `${helper.what} nowhere the census can see`);
        }
        for (const use of helper.uses) walkFrom(walk, use, comp, inside, depth + 1, [], top);
        return;
      }
      // Handed to a call the census does not follow as a helper: unread, unless the call draws
      // what it is handed as it is, or where each function registered for it draws it
      // (`CALL_WRAPPERS`).
      const why = handedTo(walk, current, child, comp, depth + 1);
      if (why !== undefined) unreadAt(walk, current, why);
    }
    child = current;
    current = current.parent;
  }
}

/** What a composer handed to a call the census does not follow as a helper - from a package, a
 *  value a hook returns - may be drawn inside: `CALL_WRAPPERS` names the calls that draw it as
 *  it is, or where a function registered for the call draws it. */
interface CallWrapper {
  /** Why the census reads the call as drawing what it is handed unchanged, or, with
   *  `registeredBy`, as drawing it where each registered function draws its parameter. */
  readonly reason: string;
  /** The hook (`<module>#<name>`) that code hands, as its first argument, the function the call
   *  calls: the census follows what the call is handed into each function the hook's callers
   *  register, as it follows a slot into the component it is handed to (`followRegistered`). */
  readonly registeredBy?: string;
}

/** Calls that draw what they are handed, by `<module>#<export>` for a function a module exports,
 *  or `<module>#<export>()` for the value such a hook returns, each with why: as it is, or, for a
 *  function code registers through a hook, where each registered function draws it, which the
 *  walk reads. A composer, or an element holding one, handed to any other call the census does
 *  not follow is unread. */
const CALL_WRAPPERS: Readonly<Record<string, CallWrapper>> = {
  "react#createElement": {
    reason:
      "is what JSX compiles to: it makes one element of its type and props, and draws its children - each argument after the props - as they are, in that element, so what it is handed mounts and unmounts with the call's own place (`features/issue-page/useDocumentToolbars.ts` writes its two providers with it, in a file with no JSX)",
  },
  "react-dom#createPortal": {
    reason:
      "draws its first argument as it is, into another DOM node; React keeps the portal in this tree, so what it draws mounts and unmounts with the call's own place",
  },
  "features/inbox/InboxGroups.tsx#groupItems": {
    reason:
      "draws what its renderRow answers for each row it is handed, once per row (rows.map), beside the band's heading and its Show more, each a direct child of the caller's list; which rows it is handed is the caller's (drawn.flatMap, which the walk reads), so a row it draws mounts and unmounts with its place in that list",
  },
  "features/issue-page/margin-slot.tsx#useMarginSlotFill()": {
    reason:
      "the page's own fill of the margin's slot: the function a page last registered through useFillMarginSlot (MarginSlotProvider holds it in state), or undefined while none is registered, when SheetBody draws nothing in its place. Called during SheetBody's render, it answers what SheetBody draws, so what it is handed is drawn where the registered function draws its parameter, and mounts and unmounts with that place under SheetBody's own; the walk reads each function a caller of useFillMarginSlot hands it",
    registeredBy: "features/issue-page/margin-slot.tsx#useFillMarginSlot",
  },
};

/** The `CALL_WRAPPERS` key of a call's function: `<module>#<export>` for one a module exports,
 *  a package's by the name it exports it under, and `<module>#<export>()` for a local holding
 *  what such a hook returns (`const fill = useMarginSlotFill()`). Undefined for any other. */
function callKey(program: Program, call: ts.CallExpression, comp: Comp): string | undefined {
  const mod = modOf(program, call);
  const exported = (name: string): string | undefined => {
    const binding = mod.imports.get(name);
    if (binding !== undefined && binding.from === undefined)
      return `${binding.spec}#${binding.name}`;
    return resolveLocal(program, mod, name);
  };
  const callee = call.expression;
  if (ts.isPropertyAccessExpression(callee) && ts.isIdentifier(callee.expression)) {
    const binding = mod.imports.get(callee.expression.text);
    return binding !== undefined && binding.from === undefined
      ? `${binding.spec}#${callee.name.text}`
      : undefined;
  }
  if (!ts.isIdentifier(callee)) return undefined;
  const found = lookup(callee, comp);
  if (found.kind === "module") return exported(callee.text);
  if (found.kind !== "local" || found.element !== undefined) return undefined;
  const init = found.decl.initializer;
  if (init === undefined || !ts.isCallExpression(init) || !ts.isIdentifier(init.expression)) {
    return undefined;
  }
  const hook = exported(init.expression.text);
  return hook === undefined ? undefined : `${hook}()`;
}

/** What `current` does with `child`, where the walk, coming up from `child`, finds it handed to a
 *  call the census does not follow as a helper. A call of `CALL_WRAPPERS` draws it as it is, or
 *  where each function registered for the call draws it, which the walk is taken into here
 *  (`followRegistered`, `depth` steps from where it started). Answers why the census cannot read
 *  any other call: it draws what it is handed somewhere the census does not follow. Undefined
 *  where `current` is no such call (no call, `child` no argument of it, `child` a function, which
 *  the walk reads as drawn by the call) or a call of `CALL_WRAPPERS`. */
function handedTo(
  walk: Walk,
  current: ts.Node,
  child: ts.Node,
  comp: Comp,
  depth: number
): string | undefined {
  if (!ts.isCallExpression(current) || !current.arguments.includes(child as ts.Expression)) {
    return undefined;
  }
  if (ts.isArrowFunction(child) || ts.isFunctionExpression(child)) return undefined;
  const callee = text(current.expression);
  const key = callKey(walk.program, current, comp);
  const wrapper = key === undefined ? undefined : CALL_WRAPPERS[key];
  if (wrapper !== undefined) {
    if (wrapper.registeredBy !== undefined) {
      const index = current.arguments.indexOf(child as ts.Expression);
      followRegistered(walk, wrapper.registeredBy, index, depth);
    }
    return undefined;
  }
  const allow =
    key === undefined
      ? ""
      : `, or add ${key} to CALL_WRAPPERS in composer-census/walk.ts with why it draws what it is handed unchanged`;
  return `${callee}(…) is handed what holds the composer, and is no function the census reads (draw it through a component or a helper the census reads${allow})`;
}

/** Follows what a call of a registered function is handed, its argument `index`, into each
 *  function the callers of `hook` (`<module>#<name>`) hand it, as a slot is followed into the
 *  component it is handed to: from each place the function draws that parameter, up to the
 *  function and no further, since the walk that met the call goes on from the call. A caller the
 *  census cannot read - the hook read but not called, called in no component the census reads,
 *  handed no function it reads, or one drawing the parameter nowhere it can see - is unread
 *  where it reads or calls the hook. */
function followRegistered(walk: Walk, hook: string, index: number, depth: number): void {
  const { program } = walk;
  const name = hook.slice(hook.lastIndexOf("#") + 1);
  for (const ref of moduleRefs(program, hook)) {
    const call = ref.parent;
    if (!ts.isCallExpression(call) || call.expression !== ref) {
      unreadAt(walk, ref, `${name} is read as a value, not called, so what it registers is unread`);
      continue;
    }
    const owners = drawnIn(program, call);
    if (owners.length === 0) {
      unreadAt(walk, call, `${name}(…) is called in no component the census reads`);
    }
    for (const owner of owners) {
      const fn = registeredFunction(call.arguments[0], owner);
      const uses = fn === undefined ? [] : paramUses(fn, index, owner);
      if (fn === undefined || uses.length === 0) {
        const why =
          fn === undefined
            ? `is handed no function the census reads (hand it a function written there, or a local of ${owner.name} holding one or its useCallback)`
            : "registers a function that draws what it is handed nowhere the census can see";
        unreadAt(walk, call, `${name}(…) ${why}, so the call drawing what it registers is unread`);
        continue;
      }
      walk.receivers.add(owner.id);
      for (const use of uses) walkFrom(walk, use, owner, true, depth, [], fn);
    }
  }
}

/** The function a hook in `comp` is handed to register (`value`): one written there, a function
 *  of `comp`'s own, or a local of `comp`'s holding one, as it is or through `useCallback`. */
function registeredFunction(
  value: ts.Expression | undefined,
  comp: Comp
): ts.FunctionLikeDeclaration | undefined {
  let node = value;
  if (node !== undefined && ts.isIdentifier(node)) {
    const found = lookup(node, comp);
    if (found.kind === "function") return found.node as ts.FunctionDeclaration;
    node =
      found.kind === "local" && found.element === undefined ? found.decl.initializer : undefined;
  }
  if (node !== undefined && ts.isCallExpression(node) && calleeName(node) === "useCallback") {
    node = node.arguments[0];
  }
  return node !== undefined && (ts.isArrowFunction(node) || ts.isFunctionExpression(node))
    ? node
    : undefined;
}

/** The places `fn`, a function inside `comp`, draws its parameter `index` whole, where that
 *  parameter is one name. */
function paramUses(fn: ts.FunctionLikeDeclaration, index: number, comp: Comp): ts.Identifier[] {
  const param = fn.parameters[index];
  if (param === undefined || !ts.isIdentifier(param.name)) return [];
  const uses: ts.Identifier[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isIdentifier(node) && node !== param.name) {
      const found = lookup(node, comp);
      if (found.kind === "param" && found.fn === fn && found.index === index) {
        if (drawsWhole(node)) uses.push(node);
      }
    }
    ts.forEachChild(node, visit);
  };
  if (fn.body !== undefined) visit(fn.body);
  return uses;
}

/** A helper `current` is, which the walk, coming up from `child`, follows to each place `comp`
 *  draws what the helper holds: a helper of its module, a local holding the JSX or a function
 *  returning it, or a helper of `comp`'s own that `child` is handed to, by its parameter. Answers
 *  its name, those places, and what it is, for the line saying the census can see none of them. */
function helperAt(
  program: Program,
  current: ts.Node,
  child: ts.Node,
  comp: Comp
): { readonly name: string; readonly uses: readonly ts.Node[]; readonly what: string } | undefined {
  if (isModuleHelper(current) && current !== comp.fn) {
    // A helper of its module, outside every component: follow each place a component it is
    // drawn in reads it, `comp` here, directly or through another helper of a module.
    const name = helperName(current);
    return {
      name,
      uses: moduleRefs(program, helperId(program, current)).filter(
        (ref) => drawsWhole(ref) && drawnIn(program, ref).includes(comp)
      ),
      what: `${name}, a helper of its module, is drawn in ${comp.name}`,
    };
  }
  if (ts.isVariableDeclaration(current) && ts.isIdentifier(current.name)) {
    // A local holding the JSX, or a helper returning it: follow each place it is used.
    const name = current.name.text;
    return {
      name,
      uses: referencesOf(name, current, comp).filter(drawsWhole),
      what: `${name}, a local of ${comp.name}, is drawn`,
    };
  }
  if (ts.isFunctionDeclaration(current) && current !== comp.fn) {
    const name = current.name?.text ?? "(anonymous)";
    return {
      name,
      uses: referencesOf(name, current, comp).filter(drawsWhole),
      what: `${name}, a helper of ${comp.name}, is drawn`,
    };
  }
  if (
    !ts.isCallExpression(current) ||
    !ts.isIdentifier(current.expression) ||
    !current.arguments.includes(child as ts.Expression)
  ) {
    return undefined;
  }
  // Handed to a helper of the component's own (`page(list, row)`): follow its parameter to
  // where the helper draws it, and from there to where the helper is called.
  const callee = lookup(current.expression, comp);
  const init = callee.kind === "local" ? callee.decl.initializer : undefined;
  const fn =
    init !== undefined && (ts.isArrowFunction(init) || ts.isFunctionExpression(init))
      ? init
      : callee.kind === "function"
        ? (callee.node as ts.FunctionDeclaration)
        : undefined;
  const index = current.arguments.indexOf(child as ts.Expression);
  const param = fn?.parameters[index];
  if (fn === undefined || param === undefined || !ts.isIdentifier(param.name)) return undefined;
  return {
    name: param.name.text,
    uses: paramUses(fn, index, comp),
    what: `${current.expression.text}, a helper of ${comp.name}, draws its ${param.name.text}`,
  };
}

/** The `if (…) return` statements before `statement` in its blocks, each of which ends the mount
 *  while its condition holds. */
function earlyReturns(
  statement: ts.Node,
  comp: Comp,
  push: (item: Omit<Item, "inside" | "unit">) => void
): void {
  let child: ts.Node = statement;
  let current: ts.Node | undefined = statement.parent;
  while (current !== undefined && child !== comp.fn) {
    if (ts.isBlock(current)) {
      for (const sibling of current.statements) {
        if (sibling === child) break;
        if (ts.isIfStatement(sibling) && containsReturn(sibling.thenStatement)) {
          push({
            at: where(sibling.expression),
            deps: dependsOn(sibling.expression, comp),
            expression: `if (${text(sibling.expression)}) return …`,
            kind: "early return",
            node: sibling.expression,
          });
        }
      }
    }
    if (ts.isFunctionLike(current)) break;
    child = current;
    current = current.parent;
  }
}

/** Whether `use` places the slot or element it names whole (`placed`). */
const drawsWhole = (use: ts.Node): boolean => placed(use, []) !== undefined;

/** Whether a use of a slot prop places the slot itself: drawn as a child, passed on whole to
 *  another element, portalled, or returned. Answers the parts of `subpath` still to reach, or
 *  undefined where the use only reads something out of the prop (a field, a test). */
function placed(use: ts.Node, subpath: readonly string[]): string[] | undefined {
  let node: ts.Node = use;
  const rest = [...subpath];
  for (;;) {
    const parent: ts.Node = node.parent;
    if (
      ts.isCallExpression(parent) &&
      (parent.expression === node ||
        (ts.isPropertyAccessExpression(parent.expression) &&
          /^(map|flatMap)$/.test(parent.expression.name.text) &&
          parent.arguments.includes(node as ts.Expression)))
    ) {
      // A helper called, or handed to `map`, draws what it returns where the call's value goes.
      node = parent;
      continue;
    }
    if (ts.isPropertyAccessExpression(parent) && parent.expression === node) {
      if (rest.length === 0 || parent.name.text !== rest[0]) return undefined;
      rest.shift();
    } else if (
      ts.isConditionalExpression(parent)
        ? parent.condition === node
        : ts.isBinaryExpression(parent)
          ? !CONDITIONAL_OPERATORS.has(parent.operatorToken.kind) || parent.right !== node
          : !(
              ts.isParenthesizedExpression(parent) ||
              ts.isNonNullExpression(parent) ||
              ts.isAsExpression(parent) ||
              ts.isArrayLiteralExpression(parent) ||
              ts.isSpreadElement(parent)
            )
    ) {
      if (ts.isJsxExpression(parent)) {
        return rest.length === 0 || ts.isJsxAttribute(parent.parent) ? rest : undefined;
      }
      if (ts.isJsxSpreadAttribute(parent)) return rest;
      if (ts.isVariableDeclaration(parent) && parent.initializer === node) return rest;
      if (
        ts.isCallExpression(parent) &&
        ts.isIdentifier(parent.expression) &&
        parent.arguments.includes(node as ts.Expression) &&
        rest.length === 0
      ) {
        // Handed to a helper; the walk up follows the helper's parameter, where the helper is
        // the component's own or its module's.
        return rest;
      }
      if (ts.isReturnStatement(parent) || ts.isArrowFunction(parent)) {
        return rest.length === 0 ? rest : undefined;
      }
      if (
        ts.isCallExpression(parent) &&
        parent.arguments.includes(node as ts.Expression) &&
        /createPortal$/.test(parent.expression.getText()) &&
        rest.length === 0
      ) {
        node = parent;
        continue;
      }
      return undefined;
    }
    node = parent;
  }
}

/** Records what between the place a walk started from and the routes the walk cannot read. */
function unreadAt(walk: Walk, node: ts.Node, what: string): void {
  walk.unread.push(`${where(node)}: ${what}`);
}

/** A tag of the platform's own (`div`), which draws what it holds as it is. */
function isPlainTag(tag: ts.JsxTagNameExpression): boolean {
  return ts.isJsxNamespacedName(tag) || (ts.isIdentifier(tag) && /^[a-z]/.test(tag.text));
}

/** Library elements that draw what they wrap as it is, by `<module>#<export>`, each with why the
 *  census reads it as a plain element. Any other library element between a mount and the routes
 *  is unread. */
const LIBRARY_WRAPPERS: Readonly<Record<string, string>> = {
  "react#Fragment": "draws its children and nothing else",
  "react#Suspense":
    "draws its children, or its fallback until they first load; a tree it has drawn that suspends again is hidden and kept, not unmounted",
};

/** The binding element of a component's props that `name` is read out of. */
function propBinding(comp: Comp, name: string): ts.BindingElement | undefined {
  const patterns: ts.BindingName[] = [];
  if (comp.cls === undefined) {
    const first = comp.fn.parameters[0];
    if (first !== undefined) patterns.push(first.name);
  } else {
    const visit = (node: ts.Node) => {
      if (ts.isVariableDeclaration(node) && node.initializer && isThis(node.initializer, "props")) {
        patterns.push(node.name);
      }
      ts.forEachChild(node, visit);
    };
    if (comp.fn.body !== undefined) visit(comp.fn.body);
  }
  for (const pattern of patterns) {
    const hit = bindingNamed(pattern, name);
    if (hit !== undefined && hit !== true) return hit;
  }
  return undefined;
}

/** A tag a component takes as a prop (`as: Tag = "div"`), which draws a plain element where its
 *  default and every value the component's users pass are strings. */
function plainTagProp(program: Program, comp: Comp, tag: ts.Identifier): boolean {
  const found = lookup(tag, comp);
  if (found.kind !== "prop") return false;
  const fallback = propBinding(comp, tag.text)?.initializer;
  if (fallback !== undefined && !ts.isStringLiteralLike(fallback)) return false;
  return (program.usesById.get(comp.id) ?? []).every(({ element }) => {
    if (spreads(element).length > 0) return false;
    const passed = attribute(element, found.prop);
    if (passed === undefined) return fallback !== undefined;
    const value = passed.initializer;
    return (
      value !== undefined &&
      (ts.isStringLiteral(value) ||
        (ts.isJsxExpression(value) &&
          value.expression !== undefined &&
          ts.isStringLiteralLike(value.expression)))
    );
  });
}

/** The `LIBRARY_WRAPPERS` key of the element's tag: an identifier imported from a package, the
 *  one tag that table is read for. Undefined for any other tag - a namespaced one such as
 *  `<ThreadPrimitive.Root>`, or one from a path the census does not hold. */
function libraryKey(program: Program, element: ts.Node): string | undefined {
  const tag = tagOf(element) as ts.JsxTagNameExpression;
  if (!ts.isIdentifier(tag)) return undefined;
  const binding = modOf(program, element).imports.get(tag.text);
  return binding !== undefined && binding.from === undefined
    ? `${binding.spec}#${binding.name}`
    : undefined;
}

/** Whether an element the census reads no component for draws what it wraps as it is: a
 *  context's provider, a library element known to, or a plain tag its component takes as a
 *  string. */
function drawsAsItIs(program: Program, comp: Comp, element: ts.Node): boolean {
  const mod = modOf(program, element);
  const tag = tagOf(element) as ts.JsxTagNameExpression;
  const context =
    ts.isPropertyAccessExpression(tag) && tag.name.text === "Provider" ? tag.expression : tag;
  if (ts.isIdentifier(context)) {
    const decl = declOf(program, resolveLocal(program, mod, context.text));
    const init =
      decl !== undefined && ts.isVariableDeclaration(decl) ? decl.initializer : undefined;
    if (init !== undefined && calleeName(init) === "createContext") return true;
  }
  if (!ts.isIdentifier(tag)) return false;
  const key = libraryKey(program, element);
  if (key !== undefined && LIBRARY_WRAPPERS[key] !== undefined) return true;
  return plainTagProp(program, comp, tag);
}

/** Follows a slot handed to the component an element draws, its `prop` (`children`, for what
 *  the element wraps), into that component's body. Answers why the census cannot, where it
 *  cannot: the element names no component it reads, or the component draws the slot nowhere it
 *  can see; for an element it cannot read, with the ways out. */
function followElement(
  walk: Walk,
  comp: Comp,
  element: ts.Node,
  prop: string,
  subpath: readonly string[],
  depth: number
): string | undefined {
  const { program } = walk;
  const id = tagComp(program, modOf(program, element), element);
  if (id !== undefined && program.comps.has(id)) {
    return followSlot(walk, id, prop, subpath, depth);
  }
  if (prop === "children" && drawsAsItIs(program, comp, element)) return undefined;
  // A library element is read as a plain one (`LIBRARY_WRAPPERS`) only where it wraps the slot,
  // and only for the tags `drawsAsItIs` reads that table for (`libraryKey`).
  const wrapper =
    prop === "children" && libraryKey(program, element) !== undefined
      ? ", or add it to LIBRARY_WRAPPERS in composer-census/walk.ts with why it draws its children unchanged"
      : "";
  return id === undefined || declOf(program, id) === undefined
    ? `is no component of the code the census reads (draw it through a component the census reads${wrapper})`
    : "is no function or class component the census reads (draw it through a component the census reads)";
}

/** Where `receiver` draws what it was handed as `prop` (at `subpath` inside it, for an object
 *  handed over), and what decides whether it does: a class component's through `this.props`.
 *  Answers why the census cannot follow it, where it cannot. */
function followSlot(
  walk: Walk,
  receiverId: string,
  prop: string,
  subpath: readonly string[],
  depth: number
): string | undefined {
  const receiver = walk.program.comps.get(receiverId);
  if (receiver?.fn.body === undefined) return "is no component with a body the census reads";
  if (depth > MAX_DEPTH) return "sits deeper than the census reads";
  let drawn = false;
  const visit = (node: ts.Node) => {
    let start: ts.Node | undefined;
    if (ts.isIdentifier(node)) {
      const parent = node.parent;
      const isName =
        (ts.isPropertyAccessExpression(parent) && parent.name === node) ||
        (ts.isBindingElement(parent) && (parent.name === node || parent.propertyName === node));
      if (!isName) {
        const found = lookup(node, receiver);
        if (found.kind === "prop" && found.prop === prop) start = node;
        if (
          found.kind === "props-object" &&
          ts.isPropertyAccessExpression(parent) &&
          parent.name.text === prop
        ) {
          start = parent;
        }
      }
    } else if (
      receiver.cls !== undefined &&
      ts.isPropertyAccessExpression(node) &&
      isThis(node.expression, "props") &&
      node.name.text === prop
    ) {
      start = node;
    }
    const rest = start === undefined ? undefined : placed(start, subpath);
    if (start !== undefined && rest !== undefined) {
      drawn = true;
      walk.receivers.add(receiverId);
      walkFrom(walk, start, receiver, true, depth, rest);
    }
    ts.forEachChild(node, visit);
  };
  visit(receiver.fn.body);
  return drawn ? undefined : `draws its ${prop} nowhere the census can see`;
}
