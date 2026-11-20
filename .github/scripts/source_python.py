"""Python read through its ast module, for check-go-offline.sh: a file's syntax tree, the scope a
node is in, the value a name is bound to, the text of a string expression, and the environment a
dict, dict(...) or os.environ builds.

Imported with PYTHONPATH naming the directory this file lives in. Its own test is
source_scan.test.sh.
"""
import ast

from source_scan import INHERIT, UNKNOWN


def python_tree(text):
    """The file's syntax tree, each node with .parent, or None when Python cannot parse it."""
    try:
        tree = ast.parse(text)
    except SyntaxError:
        return None
    for node in ast.walk(tree):
        for child in ast.iter_child_nodes(node):
            child.parent = node
    return tree


def python_scope(node):
    """The function or module that holds a node."""
    while hasattr(node, "parent") and not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Module)):
        node = node.parent
    return node


def python_bound(name, node):
    """The value of the nearest plain assignment of name before node in its function or module;
    when that function neither assigns name nor takes it as a parameter, the nearest assignment of
    it in the scope around the function, made in that scope itself (a module's name a function
    reads), before node or else after it."""
    scope = python_scope(node)
    best = None
    for stmt in _scope_assigns(scope).get(name, ()):
        if stmt.lineno <= node.lineno and stmt is not node and (best is None or stmt.lineno >= best.lineno):
            best = stmt
    if best is not None:
        return best.value
    if isinstance(scope, ast.Module) or not hasattr(scope, "parent") or name in _parameters(scope):
        return None
    outer = python_scope(scope.parent)
    own = [s for s in _scope_assigns(outer).get(name, ()) if python_scope(s) is outer]
    before = [s for s in own if s.lineno <= node.lineno]
    chosen = max(before, key=lambda s: s.lineno) if before else min(own, key=lambda s: s.lineno, default=None)
    return chosen.value if chosen is not None else None


def _scope_assigns(scope):
    """A scope's plain assignments by name, those of the functions inside it included, read once."""
    assigns = getattr(scope, "_assigns", None)
    if assigns is None:
        assigns = {}
        for stmt in ast.walk(scope):
            if isinstance(stmt, ast.Assign):
                for target in stmt.targets:
                    if isinstance(target, ast.Name):
                        assigns.setdefault(target.id, []).append(stmt)
        scope._assigns = assigns
    return assigns


def _parameters(function):
    args = function.args
    names = [a.arg for a in args.posonlyargs + args.args + args.kwonlyargs]
    return set(names) | {a.arg for a in (args.vararg, args.kwarg) if a is not None}


def python_text(node, substitute=lambda value: "\x00"):
    """The text of a string expression: a constant, an f-string with each {...} replaced by
    substitute(its expression), or a + of those; None for anything else."""
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    if isinstance(node, ast.JoinedStr):
        return "".join(v.value if isinstance(v, ast.Constant) else substitute(v.value) for v in node.values)
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        left, right = python_text(node.left, substitute), python_text(node.right, substitute)
        if left is not None or right is not None:
            return (left if left is not None else "\x00") + (right if right is not None else "\x00")
    return None


def python_env_elements(node, depth=0):
    """The environment elements of an expression that builds one: a dict display (a ** spread is
    unknown), dict(...) or os.environ.copy() with keywords, os.environ, a | of those, or a name bound
    to one, with the `X["K"] = v` assignments and X.update(K=v) calls made to that name after it."""
    if isinstance(node, ast.Dict):
        out = []
        for key, value in zip(node.keys, node.values):
            if key is None:
                out += python_env_elements(value, depth + 1) if isinstance(value, ast.Dict) else [UNKNOWN]
            elif isinstance(key, ast.Constant) and isinstance(key.value, str):
                out.append(("set", key.value, python_text(value)))
            else:
                out.append(UNKNOWN)
        return out
    if isinstance(node, ast.Call):
        out = [UNKNOWN] if node.args or not isinstance(node.func, ast.Name) else []
        return out + [("set", k.arg, python_text(k.value)) if k.arg else UNKNOWN for k in node.keywords]
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.BitOr):
        return python_env_elements(node.left, depth + 1) + python_env_elements(node.right, depth + 1)
    if isinstance(node, ast.Attribute) and node.attr == "environ":
        return [INHERIT]
    if isinstance(node, ast.Name) and depth < 4:
        bound = python_bound(node.id, node)
        if bound is None:
            return [UNKNOWN]
        out = python_env_elements(bound, depth + 1)
        for stmt in ast.walk(python_scope(node)):
            if not (bound.lineno < getattr(stmt, "lineno", 0) < node.lineno):
                continue
            if isinstance(stmt, ast.Assign) and any(isinstance(t, ast.Subscript) and isinstance(t.value, ast.Name)
                                                    and t.value.id == node.id for t in stmt.targets):
                target = next(t for t in stmt.targets if isinstance(t, ast.Subscript))
                key = target.slice
                out.append(("set", key.value, python_text(stmt.value))
                           if isinstance(key, ast.Constant) and isinstance(key.value, str) else UNKNOWN)
            elif isinstance(stmt, ast.Call) and isinstance(stmt.func, ast.Attribute) and stmt.func.attr == "update" \
                    and isinstance(stmt.func.value, ast.Name) and stmt.func.value.id == node.id:
                for arg in stmt.args:
                    out += python_env_elements(arg, depth + 1)
                out += [("set", k.arg, python_text(k.value)) if k.arg else UNKNOWN for k in stmt.keywords]
        return out
    return [UNKNOWN]
