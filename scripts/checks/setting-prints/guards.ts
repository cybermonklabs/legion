/**
 * Which parses of a setting a guard admits. A `new URL(…)` of a setting writes no line only where
 * each value it is handed is one a guard admitted, whole: an `if` before it, in a block around it,
 * that ends the function, the loop or the program when `shownSettingUrl` does not show that same
 * value (or `URL.canParse` refuses it), or a branch taken only when the judge admits it. A value
 * built from an admitted one is not admitted.
 */
import ts from "typescript";
import type { Labels } from "./model";
import { JUDGES, nameOf, strip, withBody } from "./rules";

/** The guard reading of one run, over its type checker and its evaluation of a value. */
export function guardsOf(deps: {
  readonly checker: ts.TypeChecker;
  readonly symbolAt: (node: ts.Node) => ts.Symbol | undefined;
  readonly declarationsOf: (symbol: ts.Symbol | undefined) => readonly ts.Node[];
  readonly carries: (set: Labels) => boolean;
  readonly ev: (expression: ts.Expression) => Labels;
}) {
  const { checker, symbolAt, declarationsOf, carries, ev } = deps;
  /** The symbol of the name at the root of `value` (a name, or a chain of members of one) when no
   *  line assigns it again: a `const`, or a parameter that no line of its function assigns. A chain
   *  is one only when no line of its file assigns a member written the same way. */
  const steady = (value: ts.Expression): ts.Symbol | undefined => {
    const whole = strip(value);
    let e = whole;
    while (ts.isPropertyAccessExpression(e)) e = strip(e.expression);
    if (!ts.isIdentifier(e)) return undefined;
    const symbol = symbolAt(e);
    const declarations = declarationsOf(symbol);
    const declaration = declarations[0];
    if (symbol === undefined || declaration === undefined || declarations.length !== 1)
      return undefined;
    let fixed = false;
    if (ts.isVariableDeclaration(declaration) || ts.isBindingElement(declaration)) {
      fixed = (ts.getCombinedNodeFlags(declaration) & ts.NodeFlags.Const) !== 0;
    } else if (ts.isParameter(declaration)) {
      fixed = !assignedIn(declaration.parent, (target) => symbolAt(target) === symbol);
    }
    if (!fixed) return undefined;
    if (whole === e) return symbol;
    const text = whole.getText();
    return assignedIn(whole.getSourceFile(), (target) => target.getText() === text)
      ? undefined
      : symbol;
  };
  /** Whether a line in `node` assigns a place `matches` (after `strip`). */
  const assignedIn = (node: ts.Node, matches: (target: ts.Expression) => boolean): boolean => {
    let found = false;
    const visit = (n: ts.Node) => {
      if (found) return;
      if (
        ts.isBinaryExpression(n) &&
        n.operatorToken.kind >= ts.SyntaxKind.FirstAssignment &&
        n.operatorToken.kind <= ts.SyntaxKind.LastAssignment &&
        matches(strip(n.left))
      ) {
        found = true;
        return;
      }
      if (
        (ts.isPrefixUnaryExpression(n) || ts.isPostfixUnaryExpression(n)) &&
        (n.operator === ts.SyntaxKind.PlusPlusToken ||
          n.operator === ts.SyntaxKind.MinusMinusToken) &&
        matches(strip(n.operand))
      ) {
        found = true;
        return;
      }
      ts.forEachChild(n, visit);
    };
    visit(node);
    return found;
  };
  /** Whether `a` and `b` are the same steady value, written the same way. */
  const sameValue = (a: ts.Expression, b: ts.Expression): boolean => {
    const symbol = steady(a);
    return (
      symbol !== undefined &&
      steady(b) === symbol &&
      strip(a).getText().replace(/\s+/g, "") === strip(b).getText().replace(/\s+/g, "")
    );
  };
  /** What `expression` is of `value`'s judgement: the answer of `shownSettingUrl(value)`
   *  ("shown"), or of `URL.canParse(value)` with no base ("parses"), called here or held by a
   *  `const` that was given it. */
  const judgement = (
    expression: ts.Expression,
    value: ts.Expression
  ): "shown" | "parses" | undefined => {
    const e = strip(expression);
    if (ts.isCallExpression(e)) {
      const name = nameOf(e.expression);
      const first = e.arguments[0];
      if (!JUDGES.has(name) || first === undefined || !sameValue(first, value)) return undefined;
      if (name === "shownSettingUrl") return "shown";
      const callee = strip(e.expression);
      return e.arguments.length === 1 &&
        ts.isPropertyAccessExpression(callee) &&
        nameOf(callee.expression) === "URL"
        ? "parses"
        : undefined;
    }
    if (ts.isIdentifier(e) && steady(e) !== undefined) {
      const declaration = declarationsOf(symbolAt(e))[0];
      if (declaration !== undefined && ts.isVariableDeclaration(declaration)) {
        const init = declaration.initializer;
        if (init !== undefined && ts.isIdentifier(declaration.name)) return judgement(init, value);
      }
    }
    return undefined;
  };
  /** Whether `c` compares `value`'s judgement with the answer the judge gives a value it refuses:
   *  `undefined` from `shownSettingUrl` (`null` too, compared loosely), `false` from `canParse`. */
  const comparesWithRefusal = (c: ts.BinaryExpression, value: ts.Expression): boolean => {
    const op = c.operatorToken.kind;
    const loose =
      op === ts.SyntaxKind.EqualsEqualsToken || op === ts.SyntaxKind.ExclamationEqualsToken;
    for (const [side, other] of [
      [c.left, c.right],
      [c.right, c.left],
    ] as const) {
      const kind = judgement(side, value);
      const answer = strip(other);
      if (kind === "parses" && answer.kind === ts.SyntaxKind.FalseKeyword) return true;
      if (
        kind === "shown" &&
        ((ts.isIdentifier(answer) && answer.text === "undefined") ||
          ts.isVoidExpression(answer) ||
          (loose && answer.kind === ts.SyntaxKind.NullKeyword))
      )
        return true;
    }
    return false;
  };
  /** Whether `condition` holds whenever the judge does not admit `value`. */
  const refusing = (condition: ts.Expression, value: ts.Expression): boolean => {
    const c = strip(condition);
    if (ts.isPrefixUnaryExpression(c) && c.operator === ts.SyntaxKind.ExclamationToken)
      return admitting(c.operand, value);
    if (!ts.isBinaryExpression(c)) return false;
    const op = c.operatorToken.kind;
    if (op === ts.SyntaxKind.BarBarToken)
      return refusing(c.left, value) || refusing(c.right, value);
    return (
      (op === ts.SyntaxKind.EqualsEqualsEqualsToken || op === ts.SyntaxKind.EqualsEqualsToken) &&
      comparesWithRefusal(c, value)
    );
  };
  /** Whether `condition` holds only when the judge admits `value`. */
  const admitting = (condition: ts.Expression, value: ts.Expression): boolean => {
    const c = strip(condition);
    if (judgement(c, value) !== undefined) return true;
    if (ts.isPrefixUnaryExpression(c) && c.operator === ts.SyntaxKind.ExclamationToken)
      return refusing(c.operand, value);
    if (!ts.isBinaryExpression(c)) return false;
    const op = c.operatorToken.kind;
    if (op === ts.SyntaxKind.AmpersandAmpersandToken)
      return admitting(c.left, value) || admitting(c.right, value);
    return (
      (op === ts.SyntaxKind.ExclamationEqualsEqualsToken ||
        op === ts.SyntaxKind.ExclamationEqualsToken) &&
      comparesWithRefusal(c, value)
    );
  };
  /** Whether no line after `statement` runs once it has run: it throws, returns, breaks, goes on
   *  to the next turn of its loop, or calls a function that never returns (`process.exit`). */
  const exits = (statement: ts.Statement): boolean => {
    if (
      ts.isThrowStatement(statement) ||
      ts.isReturnStatement(statement) ||
      ts.isBreakStatement(statement) ||
      ts.isContinueStatement(statement)
    )
      return true;
    if (ts.isBlock(statement)) return statement.statements.some(exits);
    if (ts.isIfStatement(statement))
      return (
        statement.elseStatement !== undefined &&
        exits(statement.thenStatement) &&
        exits(statement.elseStatement)
      );
    if (ts.isExpressionStatement(statement)) {
      const call = strip(statement.expression);
      if (!ts.isCallExpression(call)) return false;
      try {
        const signature = checker.getResolvedSignature(call);
        return (
          signature !== undefined &&
          (checker.getReturnTypeOfSignature(signature).flags & ts.TypeFlags.Never) !== 0
        );
      } catch {
        return false;
      }
    }
    return false;
  };
  /** Whether a guard admits `value` before it is read at its own place on every path: an `if`
   *  before it, in a block around it, that exits when the judge refuses the value, or a branch
   *  around it taken only when the judge admits it. It looks out through an arrow or function
   *  expression, made after any guard before it, but not through a declared function or method,
   *  which a line before the guard can call. */
  const guarded = (value: ts.Expression): boolean => {
    if (steady(value) === undefined) return false;
    let child: ts.Node = value;
    for (let p = value.parent; p !== undefined; child = p, p = p.parent) {
      if (
        ts.isBlock(p) ||
        ts.isSourceFile(p) ||
        ts.isCaseClause(p) ||
        ts.isDefaultClause(p) ||
        ts.isModuleBlock(p)
      ) {
        const before = p.statements.slice(
          0,
          Math.max(p.statements.indexOf(child as ts.Statement), 0)
        );
        for (const statement of before) {
          if (
            ts.isIfStatement(statement) &&
            refusing(statement.expression, value) &&
            exits(statement.thenStatement)
          )
            return true;
        }
      } else if (ts.isIfStatement(p)) {
        if (child === p.thenStatement && admitting(p.expression, value)) return true;
        if (child === p.elseStatement && refusing(p.expression, value)) return true;
      } else if (ts.isConditionalExpression(p)) {
        if (child === p.whenTrue && admitting(p.condition, value)) return true;
        if (child === p.whenFalse && refusing(p.condition, value)) return true;
      } else if (ts.isBinaryExpression(p) && child === p.right) {
        const op = p.operatorToken.kind;
        if (op === ts.SyntaxKind.AmpersandAmpersandToken && admitting(p.left, value)) return true;
        if (op === ts.SyntaxKind.BarBarToken && refusing(p.left, value)) return true;
      } else if (withBody(p) && !ts.isArrowFunction(p) && !ts.isFunctionExpression(p)) {
        return false;
      }
    }
    return false;
  };
  /** Whether the value `expression` hands a parse is one a guard admits whole: a steady value that
   *  is guarded where it is read, or a choice (`?:`, `??`) each of whose values that carries a
   *  setting is. A value built from an admitted one (a template, a `+`) is not admitted: the guard
   *  judged another value, and a text added to it can make the parse fail and print it. */
  const admitted = (expression: ts.Expression): boolean => {
    const e = strip(expression);
    if (ts.isIdentifier(e) || ts.isPropertyAccessExpression(e)) return guarded(e);
    const either = (part: ts.Expression) => !carries(ev(part)) || admitted(part);
    if (ts.isConditionalExpression(e)) return either(e.whenTrue) && either(e.whenFalse);
    if (ts.isBinaryExpression(e) && e.operatorToken.kind === ts.SyntaxKind.QuestionQuestionToken) {
      return either(e.left) && either(e.right);
    }
    return false;
  };
  return { admitted };
}
