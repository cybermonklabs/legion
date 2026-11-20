"""The environment a Go or TypeScript program gives a command it starts, read from a Source
(source_statements.py): a Go []string or an *exec.Cmd's own Env, a TypeScript env object or a call's
options. Each reader returns a list of elements in the order the program makes them (source_scan's
INHERIT, UNKNOWN and CLEAR, and ("set", NAME, VALUE)); the last element that may set a variable
decides it.

Imported with PYTHONPATH naming the directory this file lives in. Its own test is
source_scan.test.sh.
"""
from source_scan import INHERIT, UNKNOWN


def env_elements(src, a, b, depth=0, known=None):
    """The environment elements of an expression that builds one: in Go a []string (append of
    os.Environ() and "K=V" strings, a []string{...}, a name bound to either), in TypeScript an
    object ({ ...process.env, K: "v" }, a name bound to one, with what later statements set in it:
    ts_changes). known maps a name to the elements it stands for (a Cmd's own Env, read before a
    later assignment appends to it)."""
    if b <= a:
        return [UNKNOWN]
    tokens = src.tokens
    name = src.name(a, b)
    if name:
        if known and name in known:
            return list(known[name])
        bound = src.bound(name, a) if depth < 4 else None
        if bound:
            out = env_elements(src, *bound, depth + 1, known)
            return out + ts_changes(src, name, bound[1], a) if src.lang == "ts" else out
        return [UNKNOWN]
    if src.lang == "ts":
        if src.punct(a, "{") and src.partner.get(a) == b - 1:
            out = []
            for pa, pb in src.split(a + 1, b - 1):
                if src.punct(pa, "..."):
                    out.append(UNKNOWN)
                    continue
                colon = next((j for j in range(pa, pb) if src.punct(j, ":")), None)
                key_token = tokens[pa]
                key = key_token.value if key_token.kind in ("ident", "string") and colon == pa + 1 else None
                if colon is None and pb - pa == 1 and key_token.kind == "ident":
                    out.append(("set", key_token.value, None))
                elif key is None:
                    out.append(UNKNOWN)
                else:
                    out.append(("set", key, src.string(colon + 1, pb)))
            return out
        return [UNKNOWN]
    # Go.
    call = src.call_of(a, b)
    if call and call.callee == "os.Environ":
        return [INHERIT]
    if call and call.callee == "append" and call.args:
        out = list(env_elements(src, *call.args[0], depth, known))
        for ea, eb in call.args[1:]:
            if src.punct(eb - 1, "..."):
                out += env_elements(src, ea, eb - 1, depth, known)
            else:
                out.append(_go_setting(src, ea, eb))
        return out
    if src.punct(a, "[") and src.punct(a + 1, "]"):
        brace = next((j for j in range(a, b) if src.punct(j, "{")), None)
        if brace is not None and src.partner.get(brace) == b - 1:
            return [_go_setting(src, ea, eb) for ea, eb in src.split(brace + 1, b - 1)]
    if tokens[a].kind == "ident" and src.punct(a + 1, "[") and src.partner.get(a + 1) == b - 1:
        return env_elements(src, a, a + 1, depth, known)  # env[:len(env):len(env)]
    return [UNKNOWN]


def _go_setting(src, a, b):
    token = src.tokens[a]
    if token.kind == "string" and "=" in token.value:
        key, value = token.value.split("=", 1)
        return ("set", key, value if b - a == 1 else None)
    return UNKNOWN


def go_cmd_envs(src, call):
    """The *exec.Cmd that a call of exec.Command makes, and how its environment is set: the
    variable it is assigned to, and the rhs range of each `X.Env = ...` of that variable after
    it in the same declaration, in order, before the variable is assigned again; (None, []) when
    the call is assigned to no variable."""
    target = None
    for lhs, rhs, k in src.assignments():
        for index, (ra, rb) in enumerate(rhs):
            if ra == call.at and rb == call.close + 1 and index < len(lhs):
                target = src.name(*lhs[index])
        if target:
            break
    if not target:
        return None, []
    low, high = src.region(call.at)
    sets = []
    for lhs, rhs, k in src.assignments():
        if k <= call.close or not low <= k < high:
            continue
        names = [src.name(a, b) for a, b in lhs]
        if target in names:
            break
        sets += [rhs[index] for index, name in enumerate(names) if name == f"{target}.Env" and index < len(rhs)]
    return target, sets


def go_cmd_env(src, call):
    """The environment elements of the *exec.Cmd that a call of exec.Command makes: the last
    `X.Env = ...` go_cmd_envs finds. None when no Env is set (the command inherits the
    environment)."""
    target, sets = go_cmd_envs(src, call)
    env = None
    for a, b in sets:
        # A nil Env is the process's own; `X.Env = append(X.Env, ...)` extends the last.
        env = env_elements(src, a, b, known={f"{target}.Env": env if env is not None else [INHERIT]})
    return env


def ts_options_env(src, call, first):
    """The environment elements of a TypeScript call's options: the `env` of the first object
    argument from index `first` (or of the object a name there is bound to), else None. A spread
    after `env` in that object (`{ env: {...}, ...more }`) may replace it, so it ends unknown, but
    for one of object displays that hold no env (`...(c ? {} : { stdin })`)."""
    for a, b in call.args[first:]:
        name = src.name(a, b)
        if name:
            bound = src.bound(name, a)
            if bound:
                a, b = bound
        if not (src.punct(a, "{") and src.partner.get(a) == b - 1):
            continue
        env = None
        for pa, pb in src.split(a + 1, b - 1):
            if env is not None:
                if src.punct(pa, "...") and not _no_env(src, pa + 1, pb):
                    env = env + [UNKNOWN]
                continue
            key = src.tokens[pa]
            if key.value != "env" or key.kind not in ("ident", "string"):
                continue
            if pb - pa == 1:  # { env }: the name env, read as env_elements reads a name
                env = env_elements(src, pa, pb)
            elif src.punct(pa + 1, ":"):
                env = env_elements(src, pa + 2, pb)
        if env is not None:
            return env
    return None


def _no_env(src, a, b):
    """Whether an expression is an object display with no env key and no spread, or a
    parenthesized one, or a conditional whose two values are each such an expression."""
    while b - a > 1 and src.punct(a, "(") and src.partner.get(a) == b - 1:
        a, b = a + 1, b - 1
    if b - a > 1 and src.punct(a, "{") and src.partner.get(a) == b - 1:
        return all(src.tokens[pa].value not in ("env", "...") for pa, _ in src.split(a + 1, b - 1))
    question = next(iter(src.split(a, b, "?")[1:2]), None)
    if question is None:
        return False
    branches = src.split(question[0], b, ":")
    return len(branches) == 2 and all(_no_env(src, *r) for r in branches)


def ts_changes(src, name, after, before):
    """The environment elements statements between token after (the end of the value a TypeScript
    name is bound to) and token before (where it is read) add to that object, in order: `name.K =
    v` and `name["K"] = v` set K, `delete name.K` unsets it, and `Object.assign(name, ...)` adds
    what each later argument holds. A key this cannot read (`name[k] = v`), and any change made in a
    function that does not hold the read, may set any variable."""
    tokens, out = src.tokens, []
    around, fn = set(), src.function_at(before)
    while fn is not None:
        around.add(fn)
        fn = src.function_at(fn.at)

    def element(k, value):
        held = src.function_at(k)
        return value if held is None or held in around else UNKNOWN

    for lhs, rhs, k in src.assignments():
        if not after <= k < before or len(lhs) != 1:
            continue
        a, b = lhs[0]
        target = src.name(a, b)
        value = src.string(*rhs[0]) if len(rhs) == 1 else None
        if target and target.startswith(name + ".") and "." not in target[len(name) + 1:]:
            out.append((k, element(k, ("set", target[len(name) + 1:], value))))
        elif b - a >= 3 and src.name(a, a + 1) == name and src.partner.get(a + 1) == b - 1:
            key = src.string(a + 2, b - 1)  # name["K"] = v; a computed key may be any
            out.append((k, element(k, ("set", key, value) if key is not None else UNKNOWN)))
    for k in range(after, min(before, len(tokens) - 3)):
        if tokens[k].kind == "ident" and tokens[k].value == "delete" and tokens[k + 1].value == name \
                and src.name(k + 1, k + 4) is not None:
            out.append((k, element(k, ("set", tokens[k + 3].value, None))))
    for call in src.calls():
        if call.callee == "Object.assign" and after <= call.at < before and call.args \
                and src.name(*call.args[0]) == name:
            for a, b in call.args[1:]:
                out += [(call.at, element(call.at, e)) for e in env_elements(src, a, b)]
    return [e for _, e in sorted(out, key=lambda pair: pair[0])]
