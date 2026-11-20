"""Go and TypeScript read a statement at a time, for check-go-offline.sh and check-omp-offline.sh:
Source gives a file's calls with their arguments across lines, its assignments and the value a name
is bound to, its functions and the scope a name gets its value in, and the argv a TypeScript spawn
is given. The environment a call or an *exec.Cmd sets is source_env.py's; the tokens are
source_scan.py's lexer's.

Imported with PYTHONPATH naming the directory this file lives in. Its own test is
source_scan.test.sh.
"""
import bisect
import re

from source_scan import lex

_KEYWORDS = {
    "go": {"if", "for", "switch", "func", "return", "go", "defer", "select", "range", "else", "case", "var", "const",
           "type", "import", "package", "chan", "map", "struct", "interface"},
    "ts": {"if", "for", "while", "switch", "catch", "function", "return", "typeof", "do", "else", "in", "of", "case",
           "void", "yield", "delete", "throw", "const", "let", "var", "new", "await", "export", "import", "class"},
}
# A line ending in one of these does not continue the expression on the next line, unless that
# line starts with an operator.
_CONTINUES = set(".?:+-*/%|&=<>,!^~")


class Call:
    """A call in a Source: its callee as written (`exec.Command`, `Bun.spawn`, `$` for a tagged
    template), the token index it starts at, its parentheses, and the token range of each argument."""

    __slots__ = ("callee", "at", "open", "close", "args", "tagged")

    def __init__(self, callee, at, open_, close, args, tagged=False):
        self.callee, self.at, self.open, self.close, self.args, self.tagged = callee, at, open_, close, args, tagged

    @property
    def name(self):
        return self.callee.rsplit(".", 1)[-1]


class Function:
    """A function in a Source: the name it is declared or bound with (None for a literal no name is
    bound to), the token it starts at (`func`, `function`, `async`, its name or its parameters), the
    parenthesis that opens its parameters, the token range of each parameter's name (None for one
    that is no plain name), and its body as a token range: inside its braces, or a TypeScript
    arrow's expression."""

    __slots__ = ("name", "at", "open", "params", "body")

    def __init__(self, name, at, open_, params, body):
        self.name, self.at, self.open, self.params, self.body = name, at, open_, params, body

    def __repr__(self):
        return f"Function({self.name}, {self.at})"


# TypeScript: a word before a method's name in a class or an object literal.
_TS_MODIFIERS = {"async", "static", "public", "private", "protected", "readonly", "override", "get", "set", "abstract"}


class Source:
    """A Go or TypeScript file read as tokens, its comments left out: each bracket's partner, each
    token's line, its calls and their arguments across lines, its assignments, and the value the
    nearest assignment before a place binds a name to. A range is (first, end) token indices."""

    def __init__(self, text, lang, tokens=None):
        self.text, self.lang = text, lang
        self.tokens = [t for t in (lex(text, lang) if tokens is None else tokens) if t.kind != "comment"]
        self._starts = [0] + [m.end() for m in re.finditer("\n", text)]
        self.partner, stack = {}, []
        for i, t in enumerate(self.tokens):
            if t.kind != "punct":
                continue
            if t.value in ("(", "[", "{"):
                stack.append(i)
            elif t.value in (")", "]", "}"):
                want = {")": "(", "]": "[", "}": "{"}[t.value]
                k = len(stack) - 1
                while k >= 0 and self.tokens[stack[k]].value != want:
                    k -= 1
                if k >= 0:
                    self.partner[stack[k]], self.partner[i] = i, stack[k]
                    del stack[k:]
        self._calls = self._call_spans = self._assignments = self._bindings = self._regions = None
        self._functions = self._function_at = None

    # Positions.
    def line(self, i):
        return bisect.bisect_right(self._starts, self.tokens[i].start) - 1

    def end_line(self, i):
        return bisect.bisect_right(self._starts, self.tokens[i].end - 1) - 1

    def punct(self, i, value):
        return 0 <= i < len(self.tokens) and self.tokens[i].kind == "punct" and self.tokens[i].value == value

    def _ends(self, i):
        """Whether a line ending at token i ends the statement there (Go's semicolon rule; for
        TypeScript, a token that can end an expression)."""
        t = self.tokens[i]
        if t.kind == "ident":
            return t.value not in _KEYWORDS[self.lang] or t.value in ("return", "break", "continue")
        if t.kind in ("number", "string", "template", "regex"):
            return True
        return t.kind == "punct" and (t.value in (")", "]", "}") or (t.value in "+-" and self.punct(i - 1, t.value)))

    def _breaks(self, i):
        """Whether the statement ends between token i and the next."""
        n = len(self.tokens)
        if i + 1 >= n or self.line(i + 1) <= self.end_line(i) or not self._ends(i):
            return False
        nxt = self.tokens[i + 1]
        if self.lang == "ts" and nxt.kind == "punct" and nxt.value in _CONTINUES:
            return False
        return True

    def split(self, a, b, sep=","):
        """The ranges between the separators at the top level of [a, b)."""
        out, start, i = [], a, a
        while i < b:
            t = self.tokens[i]
            if t.kind == "punct" and t.value in ("(", "[", "{") and i in self.partner:
                i = self.partner[i] + 1
                continue
            if t.kind == "punct" and t.value == sep:
                out.append((start, i))
                start = i + 1
            i += 1
        if start < b:
            out.append((start, b))
        return out

    def expression_end(self, a, limit=None, brace_ends=False):
        """The end of the expression that starts at a: at a ; or a closing bracket of the level it
        is in, at a line break that ends the statement, or, when brace_ends, at a { (an if, for or
        switch header's block)."""
        n = len(self.tokens) if limit is None else limit
        i = a
        while i < n:
            t = self.tokens[i]
            if t.kind == "punct":
                if t.value in (";", ")", "]", "}"):
                    return i
                if t.value == "{" and brace_ends:
                    return i
                if t.value in ("(", "[", "{") and i in self.partner:
                    i = self.partner[i]
            if self._breaks(i):
                return i + 1
            i += 1
        return n

    # Values.
    def string(self, a, b):
        """The text of a range that is one string literal (or a template with no substitution)."""
        if b - a == 1:
            t = self.tokens[a]
            if t.kind == "string" or (t.kind == "template" and not t.subs):
                return t.value
        return None

    def name(self, a, b):
        """The dotted name a range is (`cmd`, `r.omp`, `process.env.X`), a TypeScript `!` after it
        dropped, else None."""
        if b > a and self.lang == "ts" and self.punct(b - 1, "!"):
            b -= 1
        if b <= a or (b - a) % 2 == 0:
            return None
        for k in range(a, b):
            t = self.tokens[k]
            if (k - a) % 2 == 0 and t.kind != "ident":
                return None
            if (k - a) % 2 == 1 and not (t.kind == "punct" and t.value == "."):
                return None
        return "".join(self.tokens[k].value for k in range(a, b))

    def text_of_tokens(self, a, b):
        return self.text[self.tokens[a].start:self.tokens[b - 1].end] if b > a else ""

    def calls(self):
        """Every call, in order: a name or a dotted name followed by its parenthesized arguments,
        and in TypeScript a template tagged by `$` or `Bun.$`."""
        if self._calls is not None:
            return self._calls
        out, tokens, keywords = [], self.tokens, _KEYWORDS[self.lang]
        for i, t in enumerate(tokens):
            if i == 0:
                continue
            prev = tokens[i - 1]
            if t.kind == "punct" and t.value == "(" and i in self.partner and prev.kind == "ident" \
                    and prev.value not in keywords:
                a = i - 1
                while a >= 2 and self.punct(a - 1, ".") and tokens[a - 2].kind == "ident":
                    a -= 2
                if a > 0 and tokens[a - 1].kind == "ident" and tokens[a - 1].value in ("func", "function"):
                    continue
                close = self.partner[i]
                out.append(Call("".join(tokens[k].value for k in range(a, i)), a, i, close, self.split(i + 1, close)))
            elif t.kind == "template" and prev.kind == "ident" and prev.value == "$":
                a = i - 1
                if a >= 2 and self.punct(a - 1, ".") and tokens[a - 2].value == "Bun":
                    a -= 2
                out.append(Call("".join(tokens[k].value for k in range(a, i)), a, i, i, [(i, i + 1)], tagged=True))
        self._calls = out
        return out

    def call_of(self, a, b):
        """The call a range is exactly, else None."""
        if self._call_spans is None:
            self._call_spans = {(call.at, call.close + 1): call for call in self.calls()}
        return self._call_spans.get((a, b))

    def tagged_calls(self, tags):
        """The TypeScript templates tagged by a plain name of tags (a name Bun's $ is imported as),
        as Calls, beside calls()'s `$` and `Bun.$`."""
        out = []
        for i, t in enumerate(self.tokens):
            prev = self.tokens[i - 1] if i else None
            if t.kind == "template" and prev is not None and prev.kind == "ident" and prev.value in tags \
                    and not self.punct(i - 2, "."):
                out.append(Call(prev.value, i - 1, i, i, [(i, i + 1)], tagged=True))
        return out

    # Imports.
    def go_imports(self):
        """Go: (the name each import binds, its path): its alias, or its path's last element; "." for
        a dot import. A blank import binds nothing."""
        out, tokens, n, i = [], self.tokens, len(self.tokens), 0
        while i < n:
            if tokens[i].kind == "ident" and tokens[i].value == "import":
                block = self.punct(i + 1, "(") and i + 1 in self.partner
                j, end = (i + 2, self.partner[i + 1]) if block else (i + 1, min(i + 3, n))
                for k in range(j, end):
                    if tokens[k].kind != "string":
                        continue
                    before = tokens[k - 1]
                    alias = "." if self.punct(k - 1, ".") else before.value \
                        if before.kind == "ident" and before.value != "import" else None
                    if alias != "_":
                        out.append((alias or tokens[k].value.rsplit("/", 1)[-1], tokens[k].value))
                    if not block:
                        break
                i = end
            i += 1
        return out

    def ts_imports(self):
        """TypeScript: {local name: (module, the name it imports)}: `import { a as b } from "m"` binds
        b to ("m", "a"), `import d from "m"` d to ("m", "default"), `import * as ns from "m"` and
        `const ns = require("m")` ns to ("m", "*"), and `const { a: b } = require("m")` b to ("m",
        "a")."""
        out, tokens, n = {}, self.tokens, len(self.tokens)

        def members(a, b, separator):
            for pa, pb in self.split(a, b):
                words = [tokens[k].value for k in range(pa, pb) if tokens[k].value != "type"]
                if len(words) == 3 and words[1] == separator:
                    yield words[2], words[0]
                elif len(words) == 1:
                    yield words[0], words[0]

        for i, t in enumerate(tokens):
            if t.kind != "ident" or t.value != "import" or self.punct(i + 1, "("):
                continue
            j, names = i + 1, []
            if j < n and tokens[j].value == "type":
                j += 1
            if j < n and tokens[j].kind == "ident" and tokens[j].value != "from":
                names.append((tokens[j].value, "default"))
                j += 2 if self.punct(j + 1, ",") else 1
            if self.punct(j, "*") and j + 2 < n and tokens[j + 1].value == "as":
                names.append((tokens[j + 2].value, "*"))
                j += 3
            elif self.punct(j, "{") and j in self.partner:
                names += list(members(j + 1, self.partner[j], "as"))
                j = self.partner[j] + 1
            if j + 1 < n and tokens[j].value == "from" and tokens[j + 1].kind == "string":
                out.update((local, (tokens[j + 1].value, imported)) for local, imported in names)
        for call in self.calls():
            module = self.string(*call.args[0]) if call.callee == "require" and len(call.args) == 1 else None
            if module is None or not self.punct(call.at - 1, "="):
                continue
            k = call.at - 2
            if k >= 0 and tokens[k].kind == "ident":
                out[tokens[k].value] = (module, "*")
            elif self.punct(k, "}") and k in self.partner:
                out.update((local, (module, imported)) for local, imported in members(self.partner[k] + 1, k, ":"))
        return out

    # Assignments and bindings.
    def _statement_start(self, k):
        i = k
        while i > 0:
            prev = self.tokens[i - 1]
            if self._breaks(i - 1):
                return i, None
            if prev.kind == "punct" and prev.value in (")", "]") and (i - 1) in self.partner:
                i = self.partner[i - 1]
                continue
            if prev.kind == "punct" and prev.value in (";", "{", "}", "(", "["):
                return i, None
            if prev.kind == "ident" and prev.value in _KEYWORDS[self.lang]:
                return i, prev.value
            i -= 1
        return 0, None

    def assignments(self):
        """(lhs ranges, rhs ranges, operator index) of each `=` and `:=` assignment and declaration."""
        if self._assignments is not None:
            return self._assignments
        out, tokens = [], self.tokens
        self._assignments = out
        for k, t in enumerate(tokens):
            if not (t.kind == "punct" and t.value == "="):
                continue
            nxt = tokens[k + 1] if k + 1 < len(tokens) else None
            if nxt is not None and nxt.kind == "punct" and nxt.value in ("=", ">") and nxt.start == t.end:
                continue
            prev = tokens[k - 1] if k else None
            end_lhs = k
            if prev is not None and prev.kind == "punct" and prev.end == t.start:
                if prev.value == ":" and self.lang == "go":
                    end_lhs = k - 1
                elif prev.value in "!<>=+-*/%&|^?":
                    continue
            start, keyword = self._statement_start(end_lhs)
            lhs = self.split(start, end_lhs)
            if self.lang == "ts":  # `const x: T = ...`: the name alone
                lhs = [(a, next((j for j in range(a, b) if self.punct(j, ":")), b)) for a, b in lhs]
            elif keyword in ("var", "const"):  # `var x T = ...`: the name alone
                lhs = [(a, a + 1) for a, b in lhs]
            end = self.expression_end(k + 1, brace_ends=keyword in ("if", "for", "switch"))
            out.append((lhs, self.split(k + 1, end), k))
        return out

    def regions(self):
        """Go: the token ranges of the top-level declarations, each from its keyword at column 0 to
        the next; TypeScript: the whole file."""
        if self._regions is None:
            starts = [0]
            if self.lang == "go":
                starts += [i for i, t in enumerate(self.tokens) if i and t.kind == "ident" and t.value in
                           ("func", "var", "const", "type", "import", "package")
                           and t.start == self._starts[self.line(i)]]
            self._regions = (starts, list(zip(starts, starts[1:] + [len(self.tokens)])))
        return self._regions[1]

    def region(self, i):
        self.regions()
        return self._regions[1][max(0, bisect.bisect_right(self._regions[0], i) - 1)]

    def bindings(self):
        """name -> [(operator index, rhs range)] of every assignment to a plain name, in order."""
        if self._bindings is None:
            self._bindings = {}
            for lhs, rhs, k in self.assignments():
                names = [self.name(a, b) for a, b in lhs]
                for index, name in enumerate(names):
                    if not name:
                        continue
                    if len(rhs) == len(lhs):
                        value = rhs[index]
                    elif index == 0 and len(rhs) == 1:  # x, err := f()
                        value = rhs[0]
                    else:
                        continue
                    self._bindings.setdefault(name, []).append((k, value))
        return self._bindings

    def bound_all(self, name, before):
        """The rhs ranges of every assignment of name before token `before` in the same declaration
        (Go) or file (TypeScript), nearest last; else those of the package or the file itself."""
        here = self.region(before)
        found = [r for k, r in self.bindings().get(name, ()) if k < before and here[0] <= k < here[1]]
        if found:
            return found
        return [r for k, r in self.bindings().get(name, ())
                if self.lang == "ts" or self.tokens[self.region(k)[0]].value in ("var", "const")]

    def bound(self, name, before):
        """The rhs range of the nearest assignment of name before token `before` (bound_all's)."""
        found = self.bound_all(name, before)
        return found[-1] if found else None

    def template_text(self, token, substitute):
        """A template's text with each ${...} replaced by substitute(its tokens)."""
        out = []
        for index, (s, e) in enumerate(token.chunks):
            out.append(self.text[s:e])
            if index < len(token.subs):
                out.append(substitute(token.subs[index]))
        return "".join(out)

    # Functions, and where a name gets its value.
    def functions(self):
        """Every function: in Go each `func` declaration, method and literal; in TypeScript each
        `function`, arrow and method of a class or an object literal. A literal's name is the one an
        assignment or an object key binds it to."""
        if self._functions is not None:
            return self._functions
        out = self._go_functions() if self.lang == "go" else self._ts_functions()
        names = {}
        for lhs, rhs, _ in self.assignments():
            if len(lhs) == len(rhs):
                for (la, lb), (ra, _rb) in zip(lhs, rhs):
                    name = self.name(la, lb)
                    if name and "." not in name:
                        names.setdefault(ra, name)
        for fn in out:
            if fn.name is None:
                fn.name = names.get(fn.at)
            if fn.name is None and fn.at >= 2 and self.punct(fn.at - 1, ":") and self.tokens[fn.at - 2].kind in (
                    "ident", "string") and (fn.at < 3 or self.tokens[fn.at - 3].value in ("{", ",")):
                fn.name = self.tokens[fn.at - 2].value  # { key: (...) => ... }
        self._functions = out
        return out

    def _params(self, open_, close):
        """The name of each parameter between a function's parentheses, a TypeScript type's own
        commas (`Record<string, string>`) left inside it."""
        out, parts = [], self.split(open_ + 1, close)
        if self.lang == "ts":
            merged, angle = [], 0
            for a, b in parts:
                if angle > 0:
                    merged[-1] = (merged[-1][0], b)
                else:
                    merged.append((a, b))
                for k in range(a, b):
                    if self.punct(k, "<"):
                        angle += 1
                    elif self.punct(k, ">") and not self.punct(k - 1, "="):
                        angle -= 1
            parts = merged
        for a, b in parts:
            while a < b and (self.punct(a, "...") or self.tokens[a].value in _TS_MODIFIERS - {"async", "get", "set"}):
                a += 1
            out.append((a, a + 1) if a < b and self.tokens[a].kind == "ident" else None)
        return out

    def _go_functions(self):
        tokens, n, out = self.tokens, len(self.tokens), []
        for i, t in enumerate(tokens):
            if t.kind != "ident" or t.value != "func":
                continue
            j, name = i + 1, None
            if j < n and tokens[j].kind == "ident":
                name, j = tokens[j].value, j + 1
            elif self.punct(j, "(") and j in self.partner:
                after = self.partner[j] + 1
                if after + 1 < n and tokens[after].kind == "ident" and tokens[after].value != "func" \
                        and (self.punct(after + 1, "(") or self.punct(after + 1, "[")):
                    name, j = tokens[after].value, after + 1  # a method: func (r *T) Name(
            if self.punct(j, "[") and j in self.partner:  # type parameters
                j = self.partner[j] + 1
            if not (self.punct(j, "(") and j in self.partner):
                continue
            close = self.partner[j]
            k = close + 1
            while k < n:  # the result types, then the body's brace; a func type has none
                tk = tokens[k]
                if tk.kind == "punct" and tk.value in ("(", "[") and k in self.partner:
                    k = self.partner[k] + 1
                    continue
                if tk.kind == "punct" and tk.value == "{" and k in self.partner:
                    if tokens[k - 1].kind == "ident" and tokens[k - 1].value in ("struct", "interface"):
                        k = self.partner[k] + 1
                        continue
                    out.append(Function(name, i, j, self._params(j, close), (k + 1, self.partner[k])))
                    break
                if (tk.kind == "punct" and tk.value in (";", ")", "]", "}", ",", "=")) or self._breaks(k):
                    break
                k += 1
        return out

    def _ts_block(self, k):
        """The body (inside its braces) of a TypeScript function whose parameters close before k:
        an optional `: Type`, then `{`; else None."""
        n, tokens = len(self.tokens), self.tokens
        if self.punct(k, ":"):
            k, angle, typed = k + 1, 0, False
            while k < n:
                tk = tokens[k]
                if tk.kind == "punct":
                    if tk.value in ("(", "[") and k in self.partner:
                        k, typed = self.partner[k] + 1, True
                        continue
                    if tk.value == "{" and k in self.partner and (angle > 0 or not typed):
                        k, typed = self.partner[k] + 1, True  # an object type
                        continue
                    if tk.value == "<":
                        angle += 1
                    elif tk.value == ">" and not self.punct(k - 1, "="):
                        angle -= 1
                    elif tk.value == "{" and angle <= 0:
                        break
                    elif tk.value in (";", ",", ")", "}", "=") and angle <= 0 and not (
                            tk.value == "=" and self.punct(k + 1, ">")):
                        return None
                typed = True
                k += 1
        if self.punct(k, "{") and k in self.partner:
            return (k + 1, self.partner[k])
        return None

    def _ts_functions(self):
        tokens, n, out = self.tokens, len(self.tokens), []
        keywords = _KEYWORDS["ts"]
        for i, t in enumerate(tokens):
            start = i - 1 if i and tokens[i - 1].value == "async" and tokens[i - 1].kind == "ident" else i
            if t.kind == "ident" and t.value == "function":
                j, name = i + 1, None
                if self.punct(j, "*"):
                    j += 1
                if j < n and tokens[j].kind == "ident":
                    name, j = tokens[j].value, j + 1
                if self.punct(j, "<"):  # type parameters
                    while j < n and not self.punct(j, "("):
                        j += 1
                if self.punct(j, "(") and j in self.partner:
                    body = self._ts_block(self.partner[j] + 1)
                    if body:
                        out.append(Function(name, start, j, self._params(j, self.partner[j]), body))
            elif t.kind == "punct" and t.value == "=" and self.punct(i + 1, ">") and tokens[i + 1].start == t.end:
                k, open_, params = i - 1, None, []
                if self.punct(k, ")") and k in self.partner:
                    open_ = self.partner[k]
                elif k >= 0 and tokens[k].kind == "ident" and not self.punct(k - 1, ":"):
                    open_, params = k, [(k, k + 1)]  # x => ...
                else:  # (x): T => ...
                    m = k
                    while m > 0 and i - m < 40 and not (self.punct(m, ")") and self.punct(m + 1, ":")):
                        if self.punct(m, ";") or self.punct(m, "{") or self.punct(m, "}"):
                            break
                        m -= 1
                    if self.punct(m, ")") and self.punct(m + 1, ":") and m in self.partner:
                        k = m
                        open_ = self.partner[m]
                if open_ is None:
                    continue
                if not params:
                    params = self._params(open_, k)
                at = open_ - 1 if open_ and tokens[open_ - 1].value == "async" and tokens[open_ - 1].kind == "ident" \
                    else open_
                after = i + 2
                if self.punct(after, "{") and after in self.partner:
                    body = (after + 1, self.partner[after])
                else:
                    body = (after, self.expression_end(after))
                out.append(Function(None, at, open_, params, body))
            elif t.kind == "ident" and t.value not in keywords and self.punct(i + 1, "(") and i + 1 in self.partner:
                before = tokens[i - 1] if i else None
                if before is not None and not (before.kind == "punct" and before.value in ("{", "}", ";", ",")
                                               or before.kind == "ident" and before.value in _TS_MODIFIERS
                                               or before.kind == "punct" and before.value == "*"):
                    continue
                close = self.partner[i + 1]
                body = self._ts_block(close + 1)
                if body:  # a method: name(params) { ... }
                    at = i - 1 if before is not None and before.kind == "ident" and before.value in _TS_MODIFIERS else i
                    out.append(Function(t.value, at, i + 1, self._params(i + 1, close), body))
        return out

    def function_at(self, i):
        """The innermost function whose body holds token i, else None."""
        if self._function_at is None:
            held = [None] * len(self.tokens)
            for fn in sorted(self.functions(), key=lambda f: f.body[0] - f.body[1]):
                for k in range(fn.body[0], min(fn.body[1], len(held))):
                    held[k] = fn
            self._function_at = held
        return self._function_at[i] if 0 <= i < len(self._function_at) else None

    def resolve(self, name, at):
        """Where a plain name used at token `at` gets its value, scope by scope out from there: in
        the function that holds `at`, every assignment of it made in that function's own body (not a
        function inside it) before `at`, ("bound", [rhs ranges]); else, when that function takes it
        as a parameter, ("param", (function, index)); else the same of each function around it; else
        the assignments of it outside every function, before `at` or, failing that, anywhere in the
        file (a package's var or const, a module's binding); else None."""
        bindings = self.bindings().get(name, ())
        fn, limit = self.function_at(at), at
        while fn is not None:
            found = [rhs for k, rhs in bindings if fn.body[0] <= k and rhs[1] <= limit and self.function_at(k) is fn]
            if found:
                return "bound", found
            for index, param in enumerate(fn.params):
                if param and self.tokens[param[0]].value == name:
                    return "param", (fn, index)
            fn, limit = self.function_at(fn.at), fn.at
        outside = [(k, rhs) for k, rhs in bindings if self.function_at(k) is None]
        found = [rhs for k, rhs in outside if rhs[1] <= limit] or [rhs for k, rhs in outside]
        return ("bound", found) if found else None

    def call_sites(self, fn):
        """The calls of a function in this file by its name, its own definition left out."""
        if not fn.name:
            return []
        return [call for call in self.calls() if call.name == fn.name and call.open != fn.open and not call.tagged]

    def chain_end(self, call):
        """The token after a call and each `.name(...)` or `.name` chained to it."""
        k = call.close + 1
        while self.punct(k, ".") and k + 1 < len(self.tokens) and self.tokens[k + 1].kind == "ident":
            k += 2
            if self.punct(k, "(") and k in self.partner:
                k = self.partner[k] + 1
        return k

    def split_op(self, a, b, op):
        """The ranges between each top-level two-character operator op (`??`, `||`, `&&`) in [a, b)."""
        out, start, i = [], a, a
        while i < b:
            t = self.tokens[i]
            if t.kind == "punct" and t.value in ("(", "[", "{") and i in self.partner:
                i = self.partner[i] + 1
                continue
            if i + 1 < b and self.punct(i, op[0]) and self.punct(i + 1, op[1]) and t.end == self.tokens[i + 1].start:
                out.append((start, i))
                start = i + 2
                i += 2
                continue
            i += 1
        out.append((start, b))
        return out

    def array_elements(self, a, b, depth=0, seen=None):
        """The element ranges of an array literal, or of the one a name is bound to, else None. seen,
        when given, is a set of the caller's: the index of the literal's [ is added to it."""
        if b - a > 1 and self.lang == "ts" and self.punct(b - 1, "!"):
            b -= 1
        if self.punct(a, "[") and self.partner.get(a) == b - 1:
            if seen is not None:
                seen.add(a)
            return self.split(a + 1, b - 1)
        name = self.name(a, b)
        if name and depth < 4:
            bound = self.bound(name, a)
            if bound:
                return self.array_elements(*bound, depth + 1, seen)
        return None

    def ts_spawn_argv(self, call, seen=None):
        """The argv of a TypeScript call that starts a program, and the index of the argument its
        options are in: Bun.spawn's argv array (or `{ cmd: [...] }`, whose options are that object),
        else node's command and the args array after it. (None, 1) for a call with no arguments.
        seen is array_elements': each array literal read is added to it."""
        if not call.args:
            return None, 1
        argv, options = self.array_elements(*call.args[0], seen=seen), 1
        if argv is None and self.punct(call.args[0][0], "{"):
            a, b = call.args[0]
            for pa, pb in self.split(a + 1, b - 1):
                if self.tokens[pa].value == "cmd" and self.punct(pa + 1, ":"):
                    argv, options = self.array_elements(pa + 2, pb, seen=seen), 0
        if argv is None:
            rest = self.array_elements(*call.args[1], seen=seen) if len(call.args) > 1 else None
            argv, options = [call.args[0]] + (rest or []), 2 if rest is not None else 1
        return argv, options
