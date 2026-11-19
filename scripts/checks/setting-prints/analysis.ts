/**
 * The search's analysis: the propagation of each value read from a setting through the tree, to a
 * fixed point of its two readings, and the lines that write a setting into text that a line prints.
 */
import path from "node:path";
import ts from "typescript";
import { guardsOf } from "./guards";
import { labelsOf } from "./labels";
import { EMPTY, type Finding, type Labels, union } from "./model";
import { compile, root } from "./program";
import {
  childCommand,
  commandLine,
  environment,
  FILE_READS,
  type FunctionWithBody,
  KEEPING_CLASSES,
  KEEPS,
  KEEPS_ARGUMENTS,
  KEEPS_RECEIVER,
  keyOf,
  LOG_NAMES,
  MAPPERS,
  MUTATORS,
  nameOf,
  RENDERS,
  SANITIZERS,
  strip,
  URL_TAKERS,
  VISITORS,
  withBody,
} from "./rules";

/** The search over `files` (absolute paths), with `extra` files of text that is not on disk. */
export function search(files: readonly string[], extra: ReadonlyMap<string, string>) {
  const { program, checker, ours } = compile(files, extra);
  const ourNode = (node: ts.Node) => ours.has(node.getSourceFile());

  const {
    labels,
    labelIds,
    where,
    one,
    setting,
    variable,
    calledAt,
    derive,
    fnLabel,
    paramLabel,
    settingsIn,
    carries,
    leadingIn,
    write,
    writtenAt,
  } = labelsOf(() => symbolic);

  // Two readings of the tree, grown together to a fixed point. The symbolic one reads each
  // function with its parameters as themselves, so a function's answer is a summary that each call
  // fills in with its own arguments; the concrete one hands each parameter what every call hands
  // it, and is what the lines that print read.
  let symbolic = false;
  const concreteCarried = new Map<ts.Node, Set<number>>();
  const symbolicCarried = new Map<ts.Node, Set<number>>();
  const returns = new Map<ts.Node, Set<number>>();
  const thrown = new Map<ts.Node, Set<number>>();
  const caught = new Map<ts.Node, Set<number>>();
  const urls = new Set<number>();
  let changed = false;
  const grow = (map: Map<ts.Node, Set<number>>, key: ts.Node, value: Labels) => {
    if (value.size === 0) return;
    let held = map.get(key);
    if (held === undefined) {
      held = new Set();
      map.set(key, held);
    }
    for (const id of value) {
      if (!held.has(id)) {
        held.add(id);
        changed = true;
      }
    }
  };
  const carry = (key: ts.Node, value: Labels) => {
    if (ourNode(key)) grow(symbolic ? symbolicCarried : concreteCarried, key, value);
  };

  const declarationsOf = (symbol: ts.Symbol | undefined): readonly ts.Node[] => {
    if (symbol === undefined) return [];
    let s = symbol;
    if (s.flags & ts.SymbolFlags.Alias) {
      try {
        s = checker.getAliasedSymbol(s);
      } catch {
        return [];
      }
    }
    return s.declarations ?? [];
  };
  const symbolAt = (node: ts.Node): ts.Symbol | undefined => {
    try {
      return checker.getSymbolAtLocation(node);
    } catch {
      return undefined;
    }
  };
  const carriedBy = (declarations: readonly ts.Node[]): Labels => {
    const map = symbolic ? symbolicCarried : concreteCarried;
    return union(...declarations.map((d) => map.get(d) ?? EMPTY));
  };

  const bodies = new Map<ts.Node, FunctionWithBody | undefined>();
  /** The function of this tree, with a body, that `call` runs. */
  const bodyOf = (call: ts.CallLikeExpression): FunctionWithBody | undefined => {
    if (bodies.has(call)) return bodies.get(call);
    let found: FunctionWithBody | undefined;
    try {
      const declaration = checker.getResolvedSignature(call)?.declaration;
      if (declaration !== undefined && !ts.isJSDocSignature(declaration)) {
        if (withBody(declaration)) found = declaration;
        else {
          const named = ts.getNameOfDeclaration(declaration);
          found = declarationsOf(named === undefined ? undefined : symbolAt(named)).find(withBody);
        }
      }
    } catch {
      found = undefined;
    }
    if (found !== undefined && !ourNode(found)) found = undefined;
    const foundName = found === undefined ? undefined : ts.getNameOfDeclaration(found);
    if (foundName !== undefined && ts.isIdentifier(foundName) && SANITIZERS.has(foundName.text)) {
      found = undefined;
    }
    bodies.set(call, found);
    return found;
  };
  /** The function a value names: an inline one, or one a name or a member is bound to. */
  const functionAt = (expression: ts.Expression | undefined): FunctionWithBody | undefined => {
    if (expression === undefined) return undefined;
    const e = strip(expression);
    if (withBody(e)) return e;
    if (!ts.isIdentifier(e) && !ts.isPropertyAccessExpression(e)) return undefined;
    for (const d of declarationsOf(symbolAt(ts.isIdentifier(e) ? e : e.name))) {
      if (withBody(d) && ourNode(d)) return d;
      if (ts.isVariableDeclaration(d) && d.initializer !== undefined) {
        const init = strip(d.initializer);
        if (withBody(init) && ourNode(init)) return init;
      }
    }
    return undefined;
  };

  const inertTypes = new Map<ts.Node, boolean>();
  /** Whether a value of this expression's type cannot hold the text of a setting. */
  const inert = (node: ts.Expression): boolean => {
    const known = inertTypes.get(node);
    if (known !== undefined) return known;
    let answer = false;
    try {
      const holds = (type: ts.Type): boolean => {
        if (type.flags & (ts.TypeFlags.Any | ts.TypeFlags.Unknown)) return true;
        if (type.isUnion()) return type.types.some(holds);
        if (type.isIntersection()) return type.types.every(holds);
        const none =
          ts.TypeFlags.NumberLike |
          ts.TypeFlags.BooleanLike |
          ts.TypeFlags.BigIntLike |
          ts.TypeFlags.Void |
          ts.TypeFlags.Undefined |
          ts.TypeFlags.Null |
          ts.TypeFlags.Never |
          ts.TypeFlags.ESSymbolLike |
          ts.TypeFlags.EnumLike |
          ts.TypeFlags.StringLiteral;
        if (type.flags & none) return false;
        return !(type.getCallSignatures().length > 0 && type.getProperties().length === 0);
      };
      answer = !holds(checker.getTypeAtLocation(node));
    } catch {
      answer = false;
    }
    inertTypes.set(node, answer);
    return answer;
  };

  const { admitted } = guardsOf({
    checker,
    symbolAt,
    declarationsOf,
    carries,
    ev: (expression) => ev(expression),
  });

  let concreteMemo = new Map<ts.Node, Labels>();
  let symbolicMemo = new Map<ts.Node, Labels>();
  const ev = (expression: ts.Expression): Labels => {
    const memo = symbolic ? symbolicMemo : concreteMemo;
    const known = memo.get(expression);
    if (known !== undefined) return known;
    memo.set(expression, EMPTY);
    let value = evaluate(expression);
    if (value.size > 0 && inert(expression)) {
      value = new Set([...value].filter((id) => labels[id]?.kind === "function"));
    }
    memo.set(expression, value);
    return value;
  };
  /** What the call `call` of `fn` answers: each of its parameters filled in by `argument(index)`,
   *  and each other setting of its summary named for this call. */
  const answer = (
    fn: ts.Node | undefined,
    argument: (index: number) => Labels,
    call: ts.Node
  ): Labels => {
    if (fn === undefined) return EMPTY;
    const summary = returns.get(fn);
    if (summary === undefined) return EMPTY;
    const out = new Set<number>();
    for (const id of summary) {
      const label = labels[id];
      if (label?.kind === "parameter" && label.fn === fn) {
        const value = argument(label.index);
        for (const each of label.rendered
          ? writtenAt(label.writer, call, value, label.leading)
          : value)
          out.add(each);
      } else out.add(calledAt(id, call));
    }
    return out;
  };
  const argumentsOf =
    (fn: FunctionWithBody, args: readonly ts.Expression[]) =>
    (index: number): Labels => {
      const parameter = fn.parameters[index];
      if (parameter === undefined) return EMPTY;
      if (parameter.dotDotDotToken !== undefined) return union(...args.slice(index).map(ev));
      const arg = args[index];
      return arg === undefined ? EMPTY : ev(arg);
    };
  const fnsIn = (value: Labels): FunctionWithBody[] =>
    [...value].flatMap((id) => {
      const label = labels[id];
      return label?.kind === "function" ? [label.node] : [];
    });

  const evaluate = (expression: ts.Expression): Labels => {
    const e = strip(expression);
    if (e !== expression) return ev(e);
    if (ts.isIdentifier(e)) {
      if (e.text === "undefined") return EMPTY;
      const declarations = declarationsOf(symbolAt(e));
      const fn = declarations.find((d): d is FunctionWithBody => withBody(d) && ourNode(d));
      return union(carriedBy(declarations), fn === undefined ? EMPTY : one(fnLabel(fn)));
    }
    if (ts.isPropertyAccessExpression(e) || ts.isElementAccessExpression(e)) {
      if (environment(e)) return one(setting(e, "environment", "the environment"));
      if (commandLine(e)) return one(setting(e, "command line", e.getText()));
      const key = keyOf(e);
      if (environment(e.expression)) {
        return key === undefined
          ? one(setting(e, "environment", `${e.expression.getText()}[${e.getText()}]`))
          : one(variable(key, e));
      }
      const declarations = declarationsOf(symbolAt(ts.isPropertyAccessExpression(e) ? e.name : e));
      const fn = declarations.find((d): d is FunctionWithBody => withBody(d) && ourNode(d));
      const fnValue = fn === undefined ? EMPTY : one(fnLabel(fn));
      const base = ev(e.expression);
      const derived = derive(base, key, e);
      // A property the tree declares holds what its writes put there, which the concrete reading
      // reads in every function: not what the rest of the value holding it holds. Of that value,
      // only a member read of a setting as a whole names more.
      if (declarations.some((d) => ourNode(d) && !withBody(d))) {
        const map = concreteCarried;
        const written = union(...declarations.map((d) => map.get(d) ?? EMPTY));
        return union(written, new Set([...derived].filter((id) => !base.has(id))), fnValue);
      }
      return union(carriedBy(declarations), derived, fnValue);
    }
    if (ts.isCallExpression(e)) return evaluateCall(e);
    if (ts.isNewExpression(e)) {
      const name = nameOf(e.expression);
      const args = e.arguments ?? [];
      const body = bodyOf(e);
      if (body !== undefined)
        return union(answer(body, argumentsOf(body, args), e), ...args.map(ev));
      if (KEEPING_CLASSES.has(name) || /Error$/.test(name)) return union(...args.map(ev));
      return EMPTY;
    }
    if (ts.isTemplateExpression(e)) {
      return union(
        ...e.templateSpans.map((span, index) =>
          write(
            span.expression,
            "a template",
            ev(span.expression),
            index === 0 && e.head.text === ""
          )
        )
      );
    }
    if (ts.isTaggedTemplateExpression(e)) {
      if (nameOf(e.tag) === "$" || !ts.isTemplateExpression(e.template)) return EMPTY;
      return union(
        ...e.template.templateSpans.map((span) =>
          write(span.expression, "a template", ev(span.expression))
        )
      );
    }
    if (ts.isBinaryExpression(e)) {
      const op = e.operatorToken.kind;
      if (op === ts.SyntaxKind.PlusToken || op === ts.SyntaxKind.PlusEqualsToken) {
        return union(
          write(e.left, "a string +", ev(e.left), true),
          write(e.right, "a string +", ev(e.right))
        );
      }
      if (
        op === ts.SyntaxKind.QuestionQuestionToken ||
        op === ts.SyntaxKind.BarBarToken ||
        op === ts.SyntaxKind.AmpersandAmpersandToken
      ) {
        return union(ev(e.left), ev(e.right));
      }
      if (
        op === ts.SyntaxKind.CommaToken ||
        (op >= ts.SyntaxKind.FirstAssignment && op <= ts.SyntaxKind.LastAssignment)
      ) {
        return ev(e.right);
      }
      return EMPTY;
    }
    if (ts.isConditionalExpression(e)) return union(ev(e.whenTrue), ev(e.whenFalse));
    if (ts.isArrayLiteralExpression(e)) {
      return union(
        ...e.elements.map((element) => (ts.isOmittedExpression(element) ? EMPTY : ev(element)))
      );
    }
    if (ts.isSpreadElement(e)) return ev(e.expression);
    if (ts.isObjectLiteralExpression(e)) {
      return union(
        ...e.properties.map((property) => {
          if (ts.isPropertyAssignment(property)) return ev(property.initializer);
          if (ts.isShorthandPropertyAssignment(property)) {
            return carriedBy(declarationsOf(checker.getShorthandAssignmentValueSymbol(property)));
          }
          if (ts.isSpreadAssignment(property)) return ev(property.expression);
          return EMPTY;
        })
      );
    }
    return EMPTY;
  };

  const evaluateCall = (call: ts.CallExpression): Labels => {
    const callee = strip(call.expression);
    const name = nameOf(callee);
    if (SANITIZERS.has(name)) return EMPTY;
    const isFileRead =
      FILE_READS.has(name) ||
      (name === "file" &&
        ts.isPropertyAccessExpression(callee) &&
        nameOf(callee.expression) === "Bun");
    if (isFileRead) return one(setting(call, "file", `the text of a file (${callee.getText()})`));
    const body = bodyOf(call);
    if (body !== undefined) return answer(body, argumentsOf(body, call.arguments), call);
    const through = fnsIn(ev(callee));
    if (through.length > 0)
      return union(...through.map((fn) => answer(fn, argumentsOf(fn, call.arguments), call)));
    const args = call.arguments.map((arg) => (withBody(strip(arg)) ? EMPTY : ev(arg)));
    if (ts.isPropertyAccessExpression(callee)) {
      const receiver = callee.expression;
      if (MAPPERS.has(name)) {
        const fn = functionAt(call.arguments[0]);
        return fn === undefined
          ? ev(receiver)
          : answer(fn, (index) => (index === 0 ? ev(receiver) : EMPTY), call);
      }
      if (name === "reduce" || name === "reduceRight") {
        const fn = functionAt(call.arguments[0]);
        const start = args[1] ?? EMPTY;
        return union(
          start,
          answer(
            fn,
            (index) =>
              index === 0 ? union(start, ev(receiver)) : index === 1 ? ev(receiver) : EMPTY,
            call
          )
        );
      }
      if (KEEPS_RECEIVER.has(name)) {
        const value = union(ev(receiver), KEEPS_ARGUMENTS.has(name) ? union(...args) : EMPTY);
        return RENDERS.has(name) ? write(callee.name, `${name}()`, value) : value;
      }
    }
    if (KEEPS.has(name)) {
      let value = union(...args);
      if (name === "from" && call.arguments[1] !== undefined) {
        const fn = functionAt(call.arguments[1]);
        value = union(
          value,
          answer(fn, (index) => (index === 0 ? (args[0] ?? EMPTY) : EMPTY), call)
        );
      }
      return RENDERS.has(name) ? write(call, `${name}()`, value) : value;
    }
    return EMPTY;
  };

  /** `value` into each name `name` binds: a member's key names the setting it reads more closely. */
  const bind = (name: ts.BindingName, value: Labels) => {
    if (ts.isIdentifier(name)) {
      carry(name.parent, value);
      return;
    }
    name.elements.forEach((element, index) => {
      if (ts.isOmittedExpression(element)) return;
      const key = ts.isArrayBindingPattern(name)
        ? String(index)
        : element.propertyName === undefined
          ? ts.isIdentifier(element.name)
            ? element.name.text
            : undefined
          : ts.isIdentifier(element.propertyName) || ts.isStringLiteral(element.propertyName)
            ? element.propertyName.text
            : undefined;
      let each = element.dotDotDotToken === undefined ? derive(value, key, element) : value;
      if (element.initializer !== undefined) each = union(each, ev(element.initializer));
      if (ts.isIdentifier(element.name)) carry(element, each);
      else bind(element.name, each);
    });
  };
  /** `value` into the place an assignment writes. */
  const assign = (target: ts.Expression, value: Labels) => {
    const t = strip(target);
    if (ts.isIdentifier(t)) {
      for (const d of declarationsOf(symbolAt(t))) carry(d, value);
    } else if (ts.isPropertyAccessExpression(t)) {
      for (const d of declarationsOf(symbolAt(t.name))) carry(d, value);
      assign(t.expression, value);
    } else if (ts.isElementAccessExpression(t)) {
      assign(t.expression, value);
    } else if (ts.isArrayLiteralExpression(t)) {
      for (const element of t.elements)
        assign(ts.isSpreadElement(element) ? element.expression : element, value);
    } else if (ts.isObjectLiteralExpression(t)) {
      for (const property of t.properties) {
        if (ts.isPropertyAssignment(property)) assign(property.initializer, value);
        else if (ts.isShorthandPropertyAssignment(property)) {
          for (const d of declarationsOf(checker.getShorthandAssignmentValueSymbol(property)))
            carry(d, value);
        } else if (ts.isSpreadAssignment(property)) assign(property.expression, value);
      }
    } else if (ts.isBinaryExpression(t) && t.operatorToken.kind === ts.SyntaxKind.EqualsToken) {
      assign(t.left, value);
    }
  };
  /** The declarations of the property `property` of the type its object literal is written as. */
  const contextual = (property: ts.ObjectLiteralElementLike): readonly ts.Node[] => {
    const object = property.parent;
    if (!ts.isObjectLiteralExpression(object) || property.name === undefined) return [];
    const key =
      ts.isIdentifier(property.name) || ts.isStringLiteral(property.name)
        ? property.name.text
        : undefined;
    if (key === undefined) return [];
    let type: ts.Type | undefined;
    try {
      type = checker.getContextualType(object);
    } catch {
      return [];
    }
    if (type === undefined) return [];
    return (type.isUnion() ? type.types : [type]).flatMap((t) =>
      declarationsOf(t.getProperty(key))
    );
  };

  const enclosingFunction = (node: ts.Node): ts.Node => {
    for (let p = node.parent; p !== undefined; p = p.parent) {
      if (withBody(p) || ts.isSourceFile(p)) return p;
    }
    return node.getSourceFile();
  };
  const enclosingTry = (node: ts.Node): ts.TryStatement | undefined => {
    let child: ts.Node = node;
    for (let p = node.parent; p !== undefined; child = p, p = p.parent) {
      if (withBody(p) || ts.isSourceFile(p)) return undefined;
      if (ts.isTryStatement(p) && p.tryBlock === child && p.catchClause !== undefined) return p;
    }
    return undefined;
  };
  const handledByCatch = (call: ts.Node): boolean => {
    const parent = call.parent;
    return (
      parent !== undefined &&
      ts.isPropertyAccessExpression(parent) &&
      parent.expression === call &&
      (parent.name.text === "catch" || parent.name.text === "then")
    );
  };
  /** A parse error that carries `value`, out of `site`: to the catch of the try around it, or out
   *  of its function. */
  const raise = (site: ts.Node, value: Labels) => {
    if (symbolic || value.size === 0) return;
    const handler = enclosingTry(site);
    if (handler !== undefined) grow(caught, handler, value);
    else grow(thrown, enclosingFunction(site), value);
  };

  const calls = new Map<ts.Node, number>();
  let collecting = false;
  /** A place that prints `value`: a line for it, and an end for each line that wrote it. */
  const end = (argument: ts.Expression, kind: string, site: ts.Node) => {
    const value = write(argument, kind, ev(argument));
    for (const id of value) {
      const label = labels[id];
      if (label?.kind === "written" && label.sources.size > 0)
        label.ends.add(`${kind} at ${where(site)}`);
    }
  };
  const sinkOf = (
    call: ts.CallExpression | ts.NewExpression
  ): { kind: string; args: readonly ts.Expression[] } | undefined => {
    const callee = strip(call.expression);
    const name = nameOf(callee);
    const args = call.arguments ?? [];
    const text = callee.getText();
    if (/Error$/.test(name)) return { kind: `the message of ${name}`, args };
    if (ts.isNewExpression(call)) return undefined;
    if (/^console\.\w+$/.test(text)) return { kind: text, args };
    if (
      (name === "write" || name === "writeSync") &&
      /(^|\.)(stdout|stderr)$/.test(text.replace(/\.write(Sync)?$/, ""))
    ) {
      return { kind: text, args };
    }
    if (
      text === "Bun.write" &&
      args[0] !== undefined &&
      /^Bun\.(stdout|stderr)$/.test(args[0].getText())
    ) {
      return { kind: `Bun.write(${args[0].getText()})`, args: args.slice(1) };
    }
    if (
      name === "writeSync" &&
      args[0] !== undefined &&
      /^(1|2|process\.(stdout|stderr)\.fd)$/.test(args[0].getText())
    ) {
      return { kind: "writeSync to standard output or error", args: args.slice(1) };
    }
    if (childCommand(callee) || text === "Bun.spawn" || text === "Bun.spawnSync") {
      return {
        kind: `the command line of ${text}`,
        args: args.slice(0, ts.isArrayLiteralExpression(strip(args[0] ?? callee)) ? 1 : 2),
      };
    }
    if (text === "Promise.reject") return { kind: "a rejected promise", args };
    if (bodyOf(call) === undefined && LOG_NAMES.has(name) && fnsIn(ev(callee)).length === 0) {
      return { kind: `a log call (${text})`, args };
    }
    return undefined;
  };

  const flowCall = (call: ts.CallExpression | ts.NewExpression) => {
    const args = call.arguments ?? [];
    const callee = strip(call.expression);
    const name = nameOf(callee);
    const owner = ts.isPropertyAccessExpression(callee) ? nameOf(callee.expression) : "";
    const body = bodyOf(call);
    const targets =
      body !== undefined ? [body] : ts.isCallExpression(call) ? fnsIn(ev(callee)) : [];
    for (const target of targets) {
      if (!symbolic) {
        target.parameters.forEach((parameter, index) => {
          bind(parameter.name, argumentsOf(target, args)(index));
        });
      }
      if (!handledByCatch(call)) raise(call, thrown.get(target) ?? EMPTY);
    }
    if (ts.isCallExpression(call) && ts.isPropertyAccessExpression(callee) && !symbolic) {
      const receiver = callee.expression;
      if (VISITORS.has(name)) {
        const fn = functionAt(args[0]);
        const first = fn?.parameters[0];
        if (first !== undefined) bind(first.name, ev(receiver));
        const second = fn?.parameters[1];
        if (second !== undefined && (name === "sort" || name === "toSorted"))
          bind(second.name, ev(receiver));
      }
      if (name === "reduce" || name === "reduceRight") {
        const fn = functionAt(args[0]);
        const [accumulator, element] = fn?.parameters ?? [];
        if (accumulator !== undefined)
          bind(accumulator.name, union(ev(receiver), args[1] === undefined ? EMPTY : ev(args[1])));
        if (element !== undefined) bind(element.name, ev(receiver));
      }
      if (name === "catch" || name === "then") {
        const fn = functionAt(name === "catch" ? args[0] : args[1]);
        const inner = strip(receiver);
        const failing = ts.isCallExpression(inner)
          ? union(
              ...[bodyOf(inner), ...fnsIn(ev(inner.expression))].map((t) =>
                t === undefined ? EMPTY : (thrown.get(t) ?? EMPTY)
              )
            )
          : EMPTY;
        const first = fn?.parameters[0];
        if (first !== undefined) bind(first.name, failing);
      }
    }
    if (
      ts.isCallExpression(call) &&
      ts.isPropertyAccessExpression(callee) &&
      MUTATORS.has(name) &&
      body === undefined
    ) {
      assign(callee.expression, union(...args.map(ev)));
    }
    if (
      ts.isCallExpression(call) &&
      name === "assign" &&
      owner === "Object" &&
      args[0] !== undefined
    ) {
      assign(args[0], union(...args.slice(1).map(ev)));
    }
    if (name === "from" && args[1] !== undefined && !symbolic) {
      const fn = functionAt(args[1]);
      const first = fn?.parameters[0];
      if (first !== undefined && args[0] !== undefined) bind(first.name, ev(args[0]));
    }
    if (ts.isNewExpression(call) && name === "URL") {
      // Bun's error holds each argument, the base too, and a parse against an admitted base still
      // fails when its input does: a parse that hands a setting is admitted only when each of its
      // arguments is the very value a guard admitted.
      const carrying = args.filter((arg) => carries(ev(arg)));
      if (carrying.length > 0 && !args.every((arg) => admitted(arg))) {
        raise(
          call,
          write(
            call,
            "the error of new URL, which holds what it could not parse",
            union(...carrying.map(ev))
          )
        );
      }
    }
    // A URL taker reads its first argument as a URL (`new URL` and `URL.canParse` their base too);
    // a child is handed its command line and its environment, whose use the search cannot read.
    const spawns = childCommand(callee) || /^Bun\.spawn(Sync)?$/.test(callee.getText());
    const takesUrl = URL_TAKERS.has(name) || /^ky\b/.test(callee.getText());
    if (!symbolic && body === undefined && (takesUrl || spawns)) {
      const handed = spawns
        ? args.flatMap((arg) => {
            const value = strip(arg);
            if (!ts.isObjectLiteralExpression(value)) return [arg];
            return value.properties.flatMap((property) =>
              ts.isPropertyAssignment(property) && /^(env|cmd)$/.test(property.name.getText())
                ? [property.initializer]
                : []
            );
          })
        : name === "URL" || name === "canParse"
          ? args
          : args.slice(0, 1);
      // A setting is read as a URL when it is the URL handed, or the head of its text (the base a
      // template or a `+` puts a path after); one written after the head is a part of a path.
      for (const arg of handed) {
        const value = ev(arg);
        for (const id of spawns ? settingsIn(value) : leadingIn(value)) urls.add(id);
      }
    }
    if (collecting) {
      const sink = sinkOf(call);
      if (sink !== undefined) {
        for (const arg of sink.args) {
          const value = strip(arg);
          if (ts.isArrayLiteralExpression(value)) {
            for (const element of value.elements)
              end(ts.isSpreadElement(element) ? element.expression : element, sink.kind, call);
          } else end(arg, sink.kind, call);
        }
      }
    }
  };
  const flow = (node: ts.Node) => {
    if (ts.isVariableDeclaration(node)) {
      const loop = node.parent.parent;
      if (
        node.initializer !== undefined &&
        !ts.isForOfStatement(loop) &&
        !ts.isForInStatement(loop)
      ) {
        bind(node.name, ev(node.initializer));
      }
    } else if (ts.isParameter(node)) {
      if (node.initializer !== undefined) bind(node.name, ev(node.initializer));
    } else if (ts.isBinaryExpression(node)) {
      const op = node.operatorToken.kind;
      if (op >= ts.SyntaxKind.FirstAssignment && op <= ts.SyntaxKind.LastAssignment) {
        assign(node.left, op === ts.SyntaxKind.PlusEqualsToken ? ev(node) : ev(node.right));
      }
    } else if (ts.isPropertyAssignment(node) && ts.isObjectLiteralExpression(node.parent)) {
      const value = ev(node.initializer);
      carry(node, value);
      for (const d of contextual(node)) carry(d, value);
    } else if (ts.isShorthandPropertyAssignment(node)) {
      const value = carriedBy(declarationsOf(checker.getShorthandAssignmentValueSymbol(node)));
      carry(node, value);
      for (const d of contextual(node)) carry(d, value);
    } else if (ts.isPropertyDeclaration(node)) {
      if (node.initializer !== undefined) carry(node, ev(node.initializer));
    } else if (ts.isReturnStatement(node)) {
      // Both readings read the answer: the symbolic one is the summary, and the concrete one hands
      // each line that writes a parameter into text what the calls hand it.
      if (node.expression !== undefined) {
        const value = ev(node.expression);
        if (symbolic) grow(returns, enclosingFunction(node), value);
      }
    } else if (ts.isArrowFunction(node)) {
      if (!ts.isBlock(node.body)) {
        const value = ev(node.body);
        if (symbolic) grow(returns, node, value);
      }
    } else if (ts.isForOfStatement(node)) {
      const value = ev(node.expression);
      const init = node.initializer;
      if (ts.isVariableDeclarationList(init))
        for (const d of init.declarations) bind(d.name, value);
      else assign(init, value);
    } else if (ts.isCatchClause(node)) {
      const declaration = node.variableDeclaration;
      if (declaration !== undefined) bind(declaration.name, caught.get(node.parent) ?? EMPTY);
    } else if (ts.isCallExpression(node) || ts.isNewExpression(node)) {
      flowCall(node);
    } else if (ts.isThrowStatement(node)) {
      if (collecting) end(node.expression, "a throw", node);
    } else if (
      ts.isTaggedTemplateExpression(node) &&
      nameOf(node.tag) === "$" &&
      ts.isTemplateExpression(node.template)
    ) {
      for (const span of node.template.templateSpans) {
        if (!symbolic) for (const id of settingsIn(ev(span.expression))) urls.add(id);
        if (collecting) end(span.expression, "a Bun shell command", node);
      }
    }
    ts.forEachChild(node, flow);
  };

  // How many calls of the tree reach each function, and each parameter as itself for the symbolic
  // reading.
  const prepare = (node: ts.Node) => {
    if (ts.isCallExpression(node) || ts.isNewExpression(node)) {
      const body = bodyOf(node);
      if (body !== undefined) calls.set(body, (calls.get(body) ?? 0) + 1);
    }
    if (withBody(node)) {
      symbolic = true;
      node.parameters.forEach((parameter, index) => {
        bind(parameter.name, one(paramLabel(node, index)));
      });
      symbolic = false;
    }
    ts.forEachChild(node, prepare);
  };
  for (const file of ours) prepare(file);

  let rounds = 0;
  do {
    changed = false;
    concreteMemo = new Map();
    symbolicMemo = new Map();
    symbolic = true;
    for (const file of ours) flow(file);
    symbolic = false;
    for (const file of ours) flow(file);
    rounds += 1;
  } while (changed && rounds < 100);
  if (changed) throw new Error(`the search reached no fixed point in ${rounds} rounds`);
  collecting = true;
  concreteMemo = new Map();
  for (const file of ours) flow(file);
  // A parse error that leaves a file's top level, or a function no call of the tree reaches, is
  // printed by the runtime, or by a caller the search does not read.
  for (const [fn, value] of thrown) {
    if (!ts.isSourceFile(fn) && (calls.get(fn) ?? 0) > 0) continue;
    for (const id of value) {
      const label = labels[id];
      if (label?.kind === "written") {
        label.ends.add(
          ts.isSourceFile(fn)
            ? `an error out of the top level of ${path.relative(root, fn.fileName)}`
            : `an error out of ${where(fn)}, which no call of the tree reaches`
        );
      }
    }
  }

  /** Whether a setting can be a URL: one that reaches a URL or another program, or a read of the
   *  environment by a key the search cannot name. */
  const url = (id: number): boolean => {
    const label = labels[id];
    if (label?.kind !== "source") return false;
    if (urls.has(id) || label.keyed === "environment") return true;
    // A setting named for a call is one when the setting it names inside the function is, and so
    // is a member read of it when the same read of that setting is.
    if (label.origin !== undefined && url(label.origin)) return true;
    const parent = label.parent === undefined ? undefined : labels[label.parent];
    if (parent?.kind === "source" && parent.origin !== undefined) {
      const counterpart = labelIds.get(`d ${parent.origin} ${label.key}`);
      return counterpart !== undefined && url(counterpart);
    }
    return false;
  };
  const lines = new Map<
    string,
    Finding & {
      hows: string[];
      expressions: string[];
      sources: string[];
      ends: string[];
      urlish: boolean;
    }
  >();
  for (const label of labels) {
    if (label.kind !== "written" || label.ends.size === 0 || label.sources.size === 0) continue;
    const key = `${label.file}:${label.line}`;
    let finding = lines.get(key);
    if (finding === undefined) {
      const file = program.getSourceFile(path.join(root, label.file));
      const text = file === undefined ? "" : lineText(file.text.split("\n"), label.line - 1);
      finding = {
        file: label.file,
        line: label.line,
        text,
        hows: [],
        expressions: [],
        sources: [],
        ends: [],
        urlish: false,
      };
      lines.set(key, finding);
    }
    if (!finding.hows.includes(label.how)) finding.hows.push(label.how);
    if (!finding.expressions.includes(label.expression)) finding.expressions.push(label.expression);
    for (const id of label.sources) {
      const s = labels[id];
      if (s?.kind !== "source") continue;
      if (url(id)) finding.urlish = true;
      const named = `${s.what} (${s.where})${url(id) ? "" : ", which reaches no URL"}`;
      if (!finding.sources.includes(named)) finding.sources.push(named);
    }
    for (const each of label.ends) if (!finding.ends.includes(each)) finding.ends.push(each);
  }
  const all = [...lines.values()].sort((a, b) => a.file.localeCompare(b.file) || a.line - b.line);
  let printing = 0;
  const countEnds = (node: ts.Node) => {
    if ((ts.isCallExpression(node) || ts.isNewExpression(node)) && sinkOf(node) !== undefined)
      printing += 1;
    ts.forEachChild(node, countEnds);
  };
  for (const file of ours) countEnds(file);
  return {
    findings: all.filter((f) => f.urlish),
    quiet: all.filter((f) => !f.urlish),
    files: ours.size,
    rounds,
    printing,
  };
}

/** The text a finding names its line by: the line, trimmed, and where it holds no word (a bare `{`
 *  or `(`), the lines after it up to the first that does, so a judgement names it apart from every
 *  other line of its file. */
function lineText(lines: readonly string[], index: number): string {
  let text = (lines[index] ?? "").trim();
  for (let next = index + 1; !/\w/.test(text) && next < lines.length; next += 1) {
    text = `${text} ${(lines[next] ?? "").trim()}`;
  }
  return text;
}
