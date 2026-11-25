import Airlock.Model

/-!
# Airlock proofs

The theorems requirement R8 asks for — `T1`, `T1_structure`, `T2` and `rewrite_some_iff_allow`
— with `T4`, that every kind of a valid rules file and every inline rule names a host, and
`no_widen`, that a request the decision carries for a name is one its kind's `allow` and its
binding's own `allow` both allow, so a profile never widens its kind. None of them puts a
condition on the incoming request: `check`'s own last refusal — a request that already carries
the real value — is what buys that, so a theorem about the rewrite needs no freshness
hypothesis. `check_ok`, `ite_ok_iff`, `swapHeader_fst`, `swapHeader_of_not_allowed` and
`filter_keepHeader` are the lemmas those are read through. `Axioms.lean` prints the axioms of
every one of them, and check k15 prints their statements.

`no_widen` is proved from `check` itself and comes first, and `check_ok` reads its two `allow`
conjuncts from it, so a model that skips one of the two lists fails the build on `no_widen`.
Both read the refusals off whatever their number (`ok_of_refusals`), so a model that drops a
refusal fails on the theorem whose statement names it, not on a count.
-/

namespace Airlock

/-- The incoming headers without `Cipher-Repository`, in any case spelling: the headers the
rewrite answers. It is written out here, the header's name in the open, rather than read from
the model's `keepHeader`, so a model that kept the header, or removed another one, fails to
prove `T1_structure` rather than restating what it does. -/
def withoutCipherRepository (hs : List (String × String)) : List (String × String) :=
  hs.filter fun h => lowerAscii h.1 != "cipher-repository"

/-- One refusal of `check` read off: an `if` that refuses when its condition holds answers
`ok` exactly when the condition does not hold and the `else` branch answers it. -/
theorem ite_ok_iff {α : Type} {c : Prop} [Decidable c] {e : String} {x : Except String α}
    {v : α} : ((if c then .error e else x) = .ok v) ↔ (¬c ∧ x = .ok v) := by
  split <;> simp_all

/-- The last conjunct of a right-nested conjunction is the `ok` the refusals were read off
down to; `ok_of_refusals` walks to it whatever the number of refusals before it, so the proofs
below hold the conditions `check` reads, not how many of them there are. -/
macro "ok_of_refusals" h:ident : tactic =>
  `(tactic| repeat (first | exact $h | replace $h:ident := ($h).2))

/-- No widening: a request `check` carries for a name is one the name's kind's `allow` allows
and one its binding's own `allow` allows. A profile's `allow` narrows its one name and never
widens its kind, whatever rows it holds: a row its kind's `allow` does not hold allows
nothing. -/
theorem no_widen {rules : Rules} {g : Grant} {value : String} {r : Request} {b : Binding}
    {k : Kind} (h : check rules g value r = .ok (b, k)) :
    lookup rules g.secret = some b ∧ kindOf rules b = some k ∧
      allowOK k.allow r = true ∧ allowOK b.allow r = true := by
  unfold check at h
  rw [ite_ok_iff] at h
  obtain ⟨-, h⟩ := h
  split at h
  · cases h
  rename_i b' hl
  split at h
  · cases h
  rename_i k' hk
  simp only [ite_ok_iff] at h
  have hok : (Except.ok (b', k') : Except String (Binding × Kind)) = .ok (b, k) := by
    ok_of_refusals h
  cases hok
  refine ⟨hl, hk, ?_, ?_⟩ <;> simp_all

/-- Everything `check` establishes when it accepts: the binding and the kind, that the kind
names a host and binds the CONNECT host, that the CONNECT port is 443, that the TLS name and
the Host header are that host, that the path holds no dot segment, that a request under either
`allow` list (`listed`) holds no empty segment and no dot segment with a `;` parameter in its
path and, as the command sent it, no encoded slash, no backslash and no double-encoded `/`, `.`
or `\`, that the kind's `allow` and the binding's `allow` both allow the request (read from
`no_widen`), that a request under either `allow` list carries no method override header and no
method override parameter, and that no header of the incoming request carries the value. -/
theorem check_ok {rules : Rules} {g : Grant} {value : String} {r : Request} {b : Binding}
    {k : Kind} (h : check rules g value r = .ok (b, k)) :
    lookup rules g.secret = some b ∧ kindOf rules b = some k ∧ k.hosts ≠ [] ∧
      hostBound k.hosts r.connectHost = true ∧ r.connectPort = 443 ∧
      r.sni = r.connectHost ∧ r.hostHeader = r.connectHost ∧
      hasDotSegment r.path = false ∧ (listed k b && hasEmptySegment r.path) = false ∧
      (listed k b && hasEncodedSlash r.rawPath) = false ∧
      (listed k b && hasBackslash r.rawPath) = false ∧
      (listed k b && hasDotSegmentParameter r.path) = false ∧
      (listed k b && hasDoubleEncoding r.rawPath) = false ∧
      allowOK k.allow r = true ∧ allowOK b.allow r = true ∧
      (listed k b && hasMethodOverride r) = false ∧
      (listed k b && hasMethodOverrideParameter r.query) = false ∧
      ∀ hd ∈ r.headers, carries value hd.2 = false := by
  obtain ⟨hl, hk, hka, hba⟩ := no_widen h
  unfold check at h
  rw [ite_ok_iff] at h
  obtain ⟨-, h⟩ := h
  split at h
  · cases h
  rename_i b' _
  split at h
  · cases h
  rename_i k' _
  simp only [ite_ok_iff] at h
  have hok : (Except.ok (b', k') : Except String (Binding × Kind)) = .ok (b, k) := by
    ok_of_refusals h
  cases hok
  refine ⟨hl, hk, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_, ?_, hka, hba, ?_, ?_, ?_⟩ <;>
    (simp_all; try ok_of_refusals h)

/-- `rewriteReq` succeeds exactly when `decideReq` allows: a swap happens exactly when the
decision allows. -/
theorem rewrite_some_iff_allow (rules : Rules) (g : Grant) (value : String) (r : Request) :
    (rewriteReq rules g value r).isSome = (decideReq rules g value r == .allow) := by
  unfold rewriteReq decideReq
  split <;> rfl

/-- The swap never touches a header's name, whichever branch it takes. -/
theorem swapHeader_fst (allowed : List String) (p value : String) (h : String × String) :
    (swapHeader allowed p value h).1 = h.1 := by
  simp only [swapHeader]; try (split <;> rfl)

/-- A header whose name the kind does not list comes out of the swap as it went in. -/
theorem swapHeader_of_not_allowed {allowed : List String} {p value : String}
    {h : String × String} (hn : allowed.contains (lowerAscii h.1) = false) :
    swapHeader allowed p value h = h := by
  simp only [swapHeader, hn, Bool.false_eq_true, ite_false]

/-- The model's `keepHeader` is the filter `withoutCipherRepository` writes out. -/
theorem filter_keepHeader (hs : List (String × String)) :
    hs.filter keepHeader = withoutCipherRepository hs := rfl

/-- T1 (structural form): the rewrite answers the request that came in without its
`Cipher-Repository` headers, header for header, at every position. The method, the query and
the body are the ones that came in; the answer has as many headers as the incoming request
has headers not named `Cipher-Repository`; the header at each position has the name the kept
header at that position had; where that name is not one the kind lists, the whole pair is the
kept pair, byte for byte; and no header of the answer is named `Cipher-Repository`, in any
case spelling.

Every clause is pinned by position, not by "some input header of the same name": a rewrite
that dropped, repeated or reordered the headers of one name, or that appended anything to the
method, would answer a request this statement does not describe, which is what requirement R8
and check k15 hold the rewrite to. The six forwarding headers the proxy removes at a private
destination are removed outside this model, after the rewrite. -/
theorem T1_structure {rules : Rules} {g : Grant} {value : String} {r r' : Request}
    (h : rewriteReq rules g value r = some r') :
    ∃ b k, lookup rules g.secret = some b ∧ kindOf rules b = some k ∧
      r'.query = r.query ∧ r'.body = r.body ∧ r'.method = r.method ∧
      r'.headers.length = (withoutCipherRepository r.headers).length ∧
      (∀ (i : Nat) (hi : i < (withoutCipherRepository r.headers).length)
          (hi' : i < r'.headers.length),
        r'.headers[i].1 = (withoutCipherRepository r.headers)[i].1 ∧
          ((allowedHeaders k).contains (lowerAscii (withoutCipherRepository r.headers)[i].1) = false →
            r'.headers[i] = (withoutCipherRepository r.headers)[i])) ∧
      ∀ hd ∈ r'.headers, lowerAscii hd.1 ≠ "cipher-repository" := by
  unfold rewriteReq at h
  split at h
  · rename_i bk hc
    cases h
    obtain ⟨hl, hk, -⟩ := check_ok hc
    refine ⟨bk.1, bk.2, hl, hk, rfl, rfl, rfl, by simp [filter_keepHeader], ?_, ?_⟩
    · intro i hi hi'
      simp only [List.getElem_map, filter_keepHeader]
      exact ⟨swapHeader_fst _ _ _ _, fun hn => swapHeader_of_not_allowed hn⟩
    · intro hd hm
      obtain ⟨h0, hm0, rfl⟩ := List.mem_map.mp hm
      rw [swapHeader_fst]
      have hkeep := (List.mem_filter.mp hm0).2
      simpa [keepHeader, cipherRepositoryHeader] using hkeep
  · cases h

/-- T1: when `rewriteReq` answers a request, its query and its body are the ones that came
in, and every header of the answer that `carries` the value has a name the kind's header list
holds. There is no hypothesis beyond the rewrite: `check` refuses a request that already
carries the value, and `carries` is false for the empty value, so neither a freshness
hypothesis nor a hypothesis about the value is needed. -/
theorem T1 {rules : Rules} {g : Grant} {value : String} {r r' : Request}
    (h : rewriteReq rules g value r = some r') :
    ∃ b k, lookup rules g.secret = some b ∧ kindOf rules b = some k ∧
      r'.query = r.query ∧ r'.body = r.body ∧
      ∀ hd ∈ r'.headers, carries value hd.2 = true →
        (allowedHeaders k).contains (lowerAscii hd.1) = true := by
  unfold rewriteReq at h
  split at h
  · rename_i bk hc
    cases h
    obtain ⟨hl, hk, -, -, -, -, -, -, -, -, -, -, -, -, -, -, -, hfresh⟩ := check_ok hc
    refine ⟨bk.1, bk.2, hl, hk, rfl, rfl, ?_⟩
    intro hd hm hcar
    obtain ⟨h0, hm0, rfl⟩ := List.mem_map.mp hm
    have hin : h0 ∈ r.headers := (List.mem_filter.mp hm0).1
    rw [swapHeader_fst]
    cases hc' : (allowedHeaders bk.2).contains (lowerAscii h0.1)
    · rw [swapHeader_of_not_allowed hc', hfresh h0 hin] at hcar
      cases hcar
    · rfl
  · cases h

/-- T2: a rewrite happens only on a CONNECT host the name's kind binds, only on port 443 of
that host, only when the TLS name and the Host header both equal that host, and only on a path
that holds no dot segment. When the kind or the binding has an `allow` list (`listed`), it also
happens only on a path that holds no empty segment and no dot segment with a `;` parameter, and
that the command sent with no encoded slash, no backslash and no double-encoded `/`, `.` or `\`,
and only on a request that carries no method override header and no method override parameter.
The answer goes to the same host, port, TLS name and Host, on the same path as the command sent
it, with the same query.

The port is part of the statement because every name compared here carries none, so 443 is
the only port the rules speak about. The path rules are part of it because an `allow` prefix
bounds the path the service will read, and a path a reader resolves, merges or decodes into
another path is one the airlock carries none of under a list. The method override header and
parameter are part of it because `allowOK` reads the method on the request line, and a service
that reads either acts on another method; with neither `allow` list every path and every method
is allowed already. -/
theorem T2 {rules : Rules} {g : Grant} {value : String} {r r' : Request}
    (h : rewriteReq rules g value r = some r') :
    ∃ b k, lookup rules g.secret = some b ∧ kindOf rules b = some k ∧
      hostBound k.hosts r.connectHost = true ∧
      r.connectPort = 443 ∧ r.sni = r.connectHost ∧ r.hostHeader = r.connectHost ∧
      hasDotSegment r.path = false ∧ (listed k b && hasEmptySegment r.path) = false ∧
      (listed k b && hasEncodedSlash r.rawPath) = false ∧
      (listed k b && hasBackslash r.rawPath) = false ∧
      (listed k b && hasDotSegmentParameter r.path) = false ∧
      (listed k b && hasDoubleEncoding r.rawPath) = false ∧
      (listed k b && hasMethodOverride r) = false ∧
      (listed k b && hasMethodOverrideParameter r.query) = false ∧
      r'.connectHost = r.connectHost ∧ r'.connectPort = r.connectPort ∧ r'.sni = r.sni ∧
      r'.hostHeader = r.hostHeader ∧ r'.path = r.path ∧ r'.rawPath = r.rawPath ∧
      r'.query = r.query := by
  unfold rewriteReq at h
  split at h
  · rename_i bk hc
    cases h
    obtain ⟨hl, hk, -, hb, hp, hs, hh, hd, he, hx, hbs, hdp, hde, -, -, hm, hq, -⟩ := check_ok hc
    exact ⟨bk.1, bk.2, hl, hk, hb, hp, hs, hh, hd, he, hx, hbs, hdp, hde, hm, hq, rfl, rfl, rfl,
      rfl, rfl, rfl, rfl⟩
  · cases h

/-- T4: every kind of a valid rules file names a host, every inline rule names a host, and
every name of a valid file is decided under a kind that names a host. A kind binds its
credential to its hosts, and one that names none would hold every value of the kind back and
carry it nowhere, so `validate` refuses one rather than leaving `check` to refuse every
request for it. -/
theorem T4 {rules : Rules} (hv : validate rules = true) :
    (∀ k ∈ rules.kinds, k.hosts ≠ []) ∧
      (∀ b ∈ rules.names, ∀ k, b.inline = some k → k.hosts ≠ []) ∧
      ∀ b ∈ rules.names, ∃ k, kindOf rules b = some k ∧ k.hosts ≠ [] := by
  simp only [validate, Bool.and_eq_true, List.all_eq_true] at hv
  obtain ⟨⟨⟨-, hkinds⟩, -⟩, hnames⟩ := hv
  have hk : ∀ k ∈ rules.kinds, k.hosts ≠ [] := by
    intro k hm hnil
    have := hkinds k hm
    simp [hostsOK, hnil] at this
  have hi : ∀ b ∈ rules.names, ∀ k, b.inline = some k → k.hosts ≠ [] := by
    intro b hm k hin hnil
    have := hnames b hm
    simp [hin, hostsOK, hnil] at this
  refine ⟨hk, hi, ?_⟩
  intro b hm
  cases hin : b.inline with
  | some k => exact ⟨k, by simp [kindOf, hin], hi b hm k hin⟩
  | none =>
    have hdecl := hnames b hm
    simp only [hin, List.contains_iff_mem, List.mem_map] at hdecl
    obtain ⟨k0, hk0, hname⟩ := hdecl
    have hsome : (rules.kinds.find? (·.name == b.kind)).isSome := by
      rw [List.find?_isSome]
      exact ⟨k0, hk0, by simp [hname]⟩
    obtain ⟨k, hfind⟩ := Option.isSome_iff_exists.mp hsome
    exact ⟨k, by simp [kindOf, hin, hfind], hk k (List.mem_of_find?_eq_some hfind)⟩

end Airlock
