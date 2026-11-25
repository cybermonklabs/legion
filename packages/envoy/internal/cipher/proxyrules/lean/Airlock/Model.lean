/-!
# Airlock core model

The rules are the profiles file's kinds and one binding for each name the broker serves
behind the proxy. A binding names the kind its secret's `proxy` tag names (or holds the
inline rule of a transitional `dpop` proxy block), and may carry its profile's own `allow`.

The broker swaps placeholder `P` of grant `G` (for the name `S`) with `S`'s real value,
only in the request headers `S`'s kind names, only on a CONNECT host that kind binds, only
when the TLS name and the Host header equal that host, and only when both the kind's `allow`
and the binding's own `allow` allow the request, so a profile narrows its kind and never
widens it. The query and the body are never touched, and every `Cipher-Repository` header
is removed.

Strings are Lean `String`s (valid UTF-8); the Go side refuses to send anything else.
Basic credentials are decoded in the model itself (base64, Go `StdEncoding` semantics:
CR/LF ignored, padding required, non-zero trailing bits accepted); the decoded
`user:pass` must be valid UTF-8 to count as a Basic credential.

This file is the model of `packages/envoy/internal/cipher/proxyrules/airlock.go`: every
definition here has a Go function of the same name, and the differential fuzz test drives
the two against each other. A change on one side is a change on the other.
-/
namespace Airlock

/-! ## Data -/

/-- One method and path prefix a credential may travel on. A kind's row holds no host; a
profile's row may hold one of its kind's hosts, and then allows the request on that host
alone. `host` is an `Option` because Go writes the field `omitempty`: a row with no host
reaches this model with no `host` key, which reads as `none`, and `none` and `some ""` are
both Go's empty host. -/
structure Allow where
  method : String
  pathPrefix : String
  host : Option String := none
  deriving Repr, BEq

/-- One kind of the profiles file: the hosts it binds, the headers its credential goes
into and its `allow`. Its ways, variables, placeholder prefix and longest grant are no part
of the decision, and the Go side's JSON keys for them are not read here. -/
structure Kind where
  name : String
  hosts : List String
  /-- `none` means the default `["authorization"]`. Compared ASCII-case-insensitively. -/
  headers : Option (List String) := none
  /-- `none`: any method and path. `some []`: nothing allowed. -/
  allow : Option (List Allow) := none
  deriving Repr

/-- One name behind the proxy: the kind it is decided under, or an inline rule (a binding
names one or holds the other, never both), and its profile's own `allow`, `none` when the
profile has none. `way` is the way its secret's type takes into a call, no part of the
decision. The inline rule is a transitional `dpop` proxy block's; it goes when a kind with the
`dpop` way replaces the profiles file's `dpop` blocks. -/
structure Binding where
  name : String
  kind : String
  way : String
  allow : Option (List Allow) := none
  inline : Option Kind := none
  deriving Repr

structure Rules where
  kinds : List Kind
  names : List Binding
  deriving Repr

structure Grant where
  secret : String
  placeholder : String
  deriving Repr

structure Request where
  connectHost : String
  /-- The port the CONNECT named. Only 443 may carry a value: the three names the decision
  compares carry no port, so 443 is the only port the rules speak about, and another port of
  one name is another service. -/
  connectPort : Nat
  sni : String
  hostHeader : String
  method : String
  path : String
  /-- The path as the command sent it, percent-encoding kept: the one form that tells an
  encoded `/` or `\` from a `/`. -/
  rawPath : String
  headers : List (String × String)
  query : String
  body : String
  deriving Repr

inductive Decision where
  | allow
  | refuse (reason : String)
  deriving Repr, BEq

/-! ## String helpers (on `List Char`, so they are total and plain to reason about) -/

/-- ASCII-only lower-casing (`Char.toLower` touches only `A`–`Z`). Its Go function is
`fold.ASCII` (`packages/envoy/internal/fold`), which `airlock.go` and the proxy port share. -/
def lowerAscii (s : String) : String := s.map Char.toLower

/-- `pat` occurs in `s` as a contiguous run. -/
def containsL : List Char → List Char → Bool
  | [], pat => pat.isEmpty
  | c :: cs, pat => pat.isPrefixOf (c :: cs) || containsL cs pat

def contains (s pat : String) : Bool := containsL s.toList pat.toList

/-- Replace each leftmost, non-overlapping occurrence of a non-empty `pat` by `rep`.
`fuel` ≥ length of the input suffices: every step consumes at least one char. -/
def replaceL (pat rep : List Char) : Nat → List Char → List Char
  | 0, s => s
  | _ + 1, [] => []
  | n + 1, c :: cs =>
    if !pat.isEmpty && pat.isPrefixOf (c :: cs) then
      rep ++ replaceL pat rep n ((c :: cs).drop pat.length)
    else c :: replaceL pat rep n cs

def replaceAll (s pat rep : String) : String :=
  String.ofList (replaceL pat.toList rep.toList s.length s.toList)

/-- Trim HTTP optional whitespace (SP, HTAB) at both ends. -/
def trimOWS (cs : List Char) : List Char :=
  let ows : Char → Bool := fun c => c == ' ' || c == '\t'
  ((cs.dropWhile ows).reverse.dropWhile ows).reverse

/-! ## Base64 (standard alphabet, padded) -/

def b64Val (c : Char) : Option Nat :=
  if 'A' ≤ c ∧ c ≤ 'Z' then some (c.toNat - 'A'.toNat)
  else if 'a' ≤ c ∧ c ≤ 'z' then some (c.toNat - 'a'.toNat + 26)
  else if '0' ≤ c ∧ c ≤ '9' then some (c.toNat - '0'.toNat + 52)
  else if c = '+' then some 62
  else if c = '/' then some 63
  else none

def b64Alphabet : String := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

def b64Char (n : Nat) : Char := (b64Alphabet.toList.getD (n % 64) 'A')

def byte (n : Nat) : UInt8 := (n % 256).toUInt8

def decodeQuads : List Char → Option (List UInt8)
  | [] => some []
  | [a, b, '=', '='] => do
    let x ← b64Val a; let y ← b64Val b
    pure [byte (x * 4 + y / 16)]
  | [a, b, c, '='] => do
    let x ← b64Val a; let y ← b64Val b; let z ← b64Val c
    pure [byte (x * 4 + y / 16), byte ((y % 16) * 16 + z / 4)]
  | a :: b :: c :: d :: rest => do
    let x ← b64Val a; let y ← b64Val b; let z ← b64Val c; let w ← b64Val d
    let tail ← decodeQuads rest
    pure (byte (x * 4 + y / 16) :: byte ((y % 16) * 16 + z / 4) :: byte ((z % 4) * 64 + w) :: tail)
  | _ => none

def b64Decode (cs : List Char) : Option (List UInt8) :=
  decodeQuads (cs.filter fun c => c != '\r' && c != '\n')

def encodeL : List UInt8 → List Char
  | [] => []
  | [a] =>
    let x := a.toNat
    [b64Char (x / 4), b64Char ((x % 4) * 16), '=', '=']
  | [a, b] =>
    let x := a.toNat; let y := b.toNat
    [b64Char (x / 4), b64Char ((x % 4) * 16 + y / 16), b64Char ((y % 16) * 4), '=']
  | a :: b :: c :: rest =>
    let x := a.toNat; let y := b.toNat; let z := c.toNat
    b64Char (x / 4) :: b64Char ((x % 4) * 16 + y / 16) :: b64Char ((y % 16) * 4 + z / 64) ::
      b64Char (z % 64) :: encodeL rest

def b64Encode (s : String) : String := String.ofList (encodeL s.toUTF8.toList)

/-! ## Basic credentials -/

/-- If `v` is `Basic <base64>` (scheme case-insensitive) whose payload decodes to valid
UTF-8, the decoded `user:pass`. -/
def basicPayload (v : String) : Option String :=
  let cs := v.toList
  if cs.length > 6 && lowerAscii (String.ofList (cs.take 6)) == "basic " then
    match b64Decode (trimOWS (cs.drop 6)) with
    | some bytes => String.fromUTF8? (ByteArray.mk bytes.toArray)
    | none => none
  else none

/-- `value` appears in a header value, directly or inside a Basic credential. -/
def mentions (value v : String) : Bool :=
  contains v value || match basicPayload v with
    | some d => contains d value
    | none => false

/-- The request already carries the real value in a header, directly or inside a decoded
Basic credential. The empty value carries nowhere: every string contains it. -/
def carries (value v : String) : Bool := !value.isEmpty && mentions value v

/-- Swap placeholder `p` with `value` in one header value. Inside a Basic credential the
placeholder may be the user or the password; the credential is re-encoded. -/
def substitute (p value v : String) : String :=
  match basicPayload v with
  | some d =>
    if contains d p then String.ofList (v.toList.take 6) ++ b64Encode (replaceAll d p value)
    else replaceAll v p value
  | none => replaceAll v p value

/-! ## Rules -/

/-- A name's binding: the first binding of that name, as Go's `Rules.Lookup` finds it. -/
def lookup (rules : Rules) (name : String) : Option Binding :=
  rules.names.find? (·.name == name)

/-- The kind a binding is decided under: its inline rule, or the first kind of the name it
names, as Go's `Rules.KindOf` answers; `none` for a kind the rules do not declare, which a
valid file never holds. -/
def kindOf (rules : Rules) (b : Binding) : Option Kind :=
  match b.inline with
  | some k => some k
  | none => rules.kinds.find? (·.name == b.kind)

/-- The headers a credential of the kind goes into, ASCII-lower-cased: the kind's own list,
or `["authorization"]` when it names none (Go's `Kind.HeaderNames`). -/
def allowedHeaders (k : Kind) : List String :=
  (k.headers.getD ["authorization"]).map lowerAscii

/-- Exact names, or `*.suffix` patterns matching one or more labels before `suffix`.
Both sides are compared ASCII-lower-cased. -/
def hostBound (hosts : List String) (host : String) : Bool :=
  let pats := hosts.map lowerAscii
  let rec go : List Char → Bool
    | [] => false
    | '.' :: rest => pats.contains ("*." ++ String.ofList rest) || go rest
    | _ :: rest => go rest
  let h := lowerAscii host
  pats.contains h || go h.toList

/-- A path prefix holds a path when the path is the prefix, or continues it at a `/`
boundary: a prefix that ends with `/`, or a path whose next character is `/`. So
`"/storage-limit"` holds `"/storage-limit"` and `"/storage-limit/7"` and not
`"/storage-limitX"`. Read on characters where Go reads bytes; for valid UTF-8 the two
agree, since a prefix of characters is a prefix of bytes ending at a character's end and `/`
is one byte. -/
def prefixOK (pre path : String) : Bool :=
  let p := pre.toList
  let s := path.toList
  p.isPrefixOf s && (s.length == p.length || p.getLast? == some '/' || (s.drop p.length).head? == some '/')

/-- One row allows a request of its method whose path its prefix holds and, for a row that
names a host, whose CONNECT host is that host, the row's host compared ASCII-lower-cased. -/
def rowOK (a : Allow) (r : Request) : Bool :=
  let h := a.host.getD ""
  a.method == r.method && (h.isEmpty || lowerAscii h == r.connectHost) && prefixOK a.pathPrefix r.path

/-- `none` allows any method and path; a list allows what one of its rows allows, so `some []`
allows nothing. -/
def allowOK : Option (List Allow) → Request → Bool
  | none, _ => true
  | some l, r => l.any fun a => rowOK a r

/-- The header names a service may read the request's method from instead of the method on
the request line: `X-HTTP-Method-Override` is the one Google's APIs honour, and
`X-HTTP-Method` and `X-Method-Override` are the two other spellings in the wild. They are
held ASCII-lower-cased, which is how every header name here is compared. -/
def methodOverrideHeaders : List String :=
  ["x-http-method-override", "x-http-method", "x-method-override"]

/-- The request carries one of those headers, whatever its value: the value is the service's
to read, so the airlock refuses the header rather than deciding which methods one of them
could have named. -/
def hasMethodOverride (r : Request) : Bool :=
  r.headers.any fun h => methodOverrideHeaders.contains (lowerAscii h.1)

/-- The query parameter names a service may read the request's method from: Laravel, Symfony
and Express's method-override middleware read `_method`, and `%5fmethod` is that name with its
`_` percent-encoded. Held ASCII-lower-cased, as each parameter's name is compared. -/
def methodOverrideParameters : List String := ["_method", "%5fmethod"]

/-- The query, as the command sent it, holds a parameter of one of those names, whatever its
value or none: the query split on `"&"`, a part's name its text before its first `"="`, or the
whole part when it has none. `String.splitOn "&"` answers as Go's `strings.Split` does, for the
reason `hasDotSegment` gives. -/
def hasMethodOverrideParameter (query : String) : Bool :=
  (query.splitOn "&").any fun part =>
    methodOverrideParameters.contains (lowerAscii (String.ofList (part.toList.takeWhile (· != '='))))

/-- The path holds a segment that is `.` or `..`: the path split on `"/"`, each segment
compared whole, so `"/v1/../admin"` and `"/a/./b"` hold one while `"/v1/..foo"` and
`"/v1/.../x"` hold none.

`String.splitOn "/"` and Go's `strings.Split(path, "/")` answer alike on every input - the
empty path is one empty segment on both sides, and a leading or trailing slash is an empty
segment at that end on both - so the two sides refuse the same paths. -/
def hasDotSegment (path : String) : Bool :=
  (path.splitOn "/").any fun s => s == "." || s == ".."

/-- The path holds an empty segment: two slashes in a row. A reader that merges them reads
`"/a//b"` as `"/a/b"` and one that does not reads another route, so under an `allow` list the
airlock carries neither. A trailing slash is no empty segment here. -/
def hasEmptySegment (path : String) : Bool := contains path "//"

/-- The path as the command sent it holds a percent-encoded `/`, `%2F` or `%2f`: the decoded
path reads it as a separator and a service may read it as a byte of one segment. -/
def hasEncodedSlash (rawPath : String) : Bool := contains (lowerAscii rawPath) "%2f"

/-- The path as the command sent it holds a `\`, literal or percent-encoded (`%5C` or `%5c`): a
reader that takes `\` for `/` reads another path than the literal one an `allow` prefix holds. -/
def hasBackslash (rawPath : String) : Bool :=
  contains rawPath "\\" || contains (lowerAscii rawPath) "%5c"

def namesUnique : List String → Bool
  | [] => true
  | n :: ns => !ns.contains n && namesUnique ns

/-- A kind's name is one lowercase word: `a`-`z` alone, never empty. -/
def kindNameOK (n : String) : Bool := !n.isEmpty && n.toList.all Char.isLower

/-- A character a host label may hold: a letter, a digit or a hyphen. -/
def hostCharOK (c : Char) : Bool :=
  ('a' ≤ c && c ≤ 'z') || ('A' ≤ c && c ≤ 'Z') || ('0' ≤ c && c ≤ '9') || c == '-'

/-- A label of a host: never empty, never starting or ending with a hyphen, and every
character one `hostCharOK` takes. -/
def labelOK : List Char → Bool
  | [] => false
  | c :: cs => c != '-' && (c :: cs).getLast? != some '-' && (c :: cs).all hostCharOK

/-- The labels of a host, splitting on `'.'`: an empty host has one empty label, and a
leading, doubled or trailing dot leaves an empty label, which `labelOK` refuses. -/
def splitDots : List Char → List (List Char)
  | [] => [[]]
  | '.' :: rest => [] :: splitDots rest
  | c :: rest =>
    match splitDots rest with
    | [] => [[c]]
    | l :: ls => (c :: l) :: ls

/-- A host the airlock can bind a request to: a host name `hostBound` holds exactly, or
`"*."` and one, a suffix it looks up. Every label passes `labelOK`, and the last is not all
digits. A host written with a scheme, a path, a port, or `"*"` without the `"*."` that
`hostBound` looks up, matches no request ever and binds the secret nowhere. An IPv4 address
matches the CONNECT host of a request to it, but `check` refuses every such request
`"sni mismatch"`, because a TLS client names no address in its SNI (RFC 6066 section 3). A
host name is told from a dotted-decimal address by its last label, which in a host name is
never all digits (RFC 1123 section 2.1), and that is the test here. The characters are read
as a list, as `hostBound` reads them, so the two cannot drift. -/
def hostShapeOK (h : String) : Bool :=
  let cs := h.toList
  let name := if cs.take 2 == ['*', '.'] then cs.drop 2 else cs
  let labels := splitDots name
  !name.isEmpty && labels.all labelOK && !(labels.getLast?.getD []).all Char.isDigit

/-- One kind's hosts, or one inline rule's: at least one, each one `hostShapeOK` takes. -/
def hostsOK (hosts : List String) : Bool := !hosts.isEmpty && hosts.all hostShapeOK

/-- Rules validation. This is the same answer as Go's `Validate` returning `nil`: the kinds'
names are distinct, each one lowercase word, and each kind names a host and only hosts the
airlock can bind; the bindings' names are distinct, and a binding holds an inline rule and
names no kind, its rule naming a host and only hosts the airlock can bind, or names a kind the
rules declare. A kind of no host would hold every value of the kind back and carry it
nowhere, so it is refused here (theorem `T4`); `check` still refuses every request for one
("secret not behind airlock"), since a decision does not assume a validated file. -/
def validate (rules : Rules) : Bool :=
  let declared := rules.kinds.map (·.name)
  namesUnique declared &&
  rules.kinds.all (fun k => kindNameOK k.name && hostsOK k.hosts) &&
  namesUnique (rules.names.map (·.name)) &&
  rules.names.all fun b =>
    match b.inline with
    | some k => b.kind.isEmpty && hostsOK k.hosts
    | none => declared.contains b.kind

/-! ## Decision and rewrite -/

/-- The kind or the binding has an `allow` list: the rules that keep a list meaning what it
says are read under one. -/
def listed (k : Kind) (b : Binding) : Bool := k.allow.isSome || b.allow.isSome

/-- The one decision both `decideReq` and `rewriteReq` take: the binding and the kind the
request is carried under, or the reason it may not be. The reasons and the order they are
reported in are the contract with Go's `check`.

The name's binding gives its kind, whose hosts and `allow` bind every secret of the kind,
and its profile's own `allow`, which narrows that one name: a request goes through only when
both allow it (theorem `no_widen`), so a profile never widens its kind.

The CONNECT port must be 443: every name the decision compares - the CONNECT host, the TLS
name and the Host header - carries no port, so 443 is the only port the rules speak about,
and another port of one name is another service. It is read above the two name comparisons
because the port is part of what the CONNECT named.

A path that holds a dot segment is refused for every kind: a reader that resolves it (RFC 3986
section 5.2.4) reads another path than the literal one. A path that holds an empty segment, or
a raw path that holds a percent-encoded `/` or a `\` in any spelling, is refused when, and only
when, the kind or the profile has an `allow` list (`listed`): an `allow` prefix bounds the path
the service will read, and a reader that merges slashes, decodes `%2F` or takes `\` for `/`
first reads another path than the literal one the prefix holds. With neither list every path
of the kind's hosts is allowed already, so such a path escapes no prefix. The path rules are
read directly above the `allow` tests because they are what makes those rules mean what they
say.

A request carrying a method override header, or a method override parameter in its query, is
refused when, and only when, the kind or the profile has an `allow` list. `allowOK` compares the
method on the request line, while a service that reads `X-HTTP-Method-Override` (Google's APIs
do), one of its two other spellings, or a `_method` parameter acts on the method that names, so
a list allowing `{POST, /prefix}` would carry the value on a PATCH or a DELETE under that
prefix. With neither list every method is allowed already, so they take nothing away and are
not refused. They are read directly below the `allow` tests.

The last test reads **every** header of the request, not only the ones the kind names, and
no way is exempt: a request that already carries the real value learnt it somewhere other
than this swap, so the airlock refuses it. -/
def check (rules : Rules) (g : Grant) (value : String) (r : Request) :
    Except String (Binding × Kind) :=
  if g.placeholder.isEmpty then .error "empty placeholder" else
  match lookup rules g.secret with
  | none => .error "unknown secret"
  | some b =>
    match kindOf rules b with
    | none => .error "secret not behind airlock"
    | some k =>
      if k.hosts.isEmpty then .error "secret not behind airlock"
      else if !hostBound k.hosts r.connectHost then .error "host not bound"
      else if r.connectPort != 443 then .error "connect port is not 443"
      else if r.sni != r.connectHost then .error "sni mismatch"
      else if r.hostHeader != r.connectHost then .error "host header mismatch"
      else if hasDotSegment r.path then .error "path has a dot segment"
      else if listed k b && hasEmptySegment r.path then .error "path has an empty segment"
      else if listed k b && hasEncodedSlash r.rawPath then .error "path has an encoded slash"
      else if listed k b && hasBackslash r.rawPath then .error "path has a backslash"
      else if !allowOK k.allow r then .error "not allowed by rule"
      else if !allowOK b.allow r then .error "not allowed by the profile's rule"
      else if listed k b && hasMethodOverride r then
        .error "request carries a method override header"
      else if listed k b && hasMethodOverrideParameter r.query then
        .error "request carries a method override parameter"
      else if r.headers.any (fun h => carries value h.2) then
        .error "request already carries the value"
      else .ok (b, k)

def decideReq (rules : Rules) (g : Grant) (value : String) (r : Request) : Decision :=
  match check rules g value r with
  | .ok _ => .allow
  | .error e => .refuse e

/-- The request header a command names a repository in, for the broker to mint a GitHub token
for that one repository. The proxy removes it from every request it reads, so no service ever
sees it. ASCII-lower-cased, as every header name here is compared. -/
def cipherRepositoryHeader : String := "cipher-repository"

/-- A header the rewrite keeps: every one but `Cipher-Repository`, in any case spelling. -/
def keepHeader (h : String × String) : Bool := lowerAscii h.1 != cipherRepositoryHeader

def swapHeader (allowed : List String) (p value : String) (h : String × String) : String × String :=
  if allowed.contains (lowerAscii h.1) then (h.1, substitute p value h.2) else h

def rewriteReq (rules : Rules) (g : Grant) (value : String) (r : Request) : Option Request :=
  match check rules g value r with
  | .ok bk =>
    some { r with headers := (r.headers.filter keepHeader).map (swapHeader (allowedHeaders bk.2) g.placeholder value) }
  | .error _ => none

end Airlock
