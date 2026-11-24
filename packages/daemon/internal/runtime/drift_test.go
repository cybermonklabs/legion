package runtime

import (
	"slices"
	"testing"
)

// The kinds whose answer the spec names, each in its own direction. Every other declared kind
// stands where the declarations put it, with the reason beside it.
var (
	drainingKinds = []DriftKind{
		DriftImage, DriftResources, DriftVolumes, DriftMounts, DriftDisk, DriftSandbox, DriftRoleOverlays,
		DriftLeakCheck, DriftGhOwner,
	}
	breakingKinds = []DriftKind{
		DriftAccount, DriftGhRepositoryDir, DriftGhNoPairs, DriftEnrollment, DriftExchangeIdentity,
		DriftSessionStore, DriftUnreadableShape, DriftFloor,
	}
)

// Each kind of drift is declared once, as one a pod's roles keep working under (it drains) or one
// they cannot (it breaks), with the reason beside it. The image, the resources, the volumes, the
// mounts, the disk, the sandbox class, the role overlays, the leak check digest and a GitHub owner
// the pod lacks drain; the account, the two gh layouts from before contract 152, the enrollment,
// the exchange identity, the session store, a recorded shape that cannot be read and the floor
// break. A kind no declaration names breaks, since nothing shows its roles can keep working under
// it.
func TestTheDriftKindDeclarations(t *testing.T) {
	seen := map[DriftKind]bool{}
	for _, d := range DriftDeclarations() {
		if d.Kind == "" {
			t.Errorf("a declaration names no kind: %+v", d)
		}
		if seen[d.Kind] {
			t.Errorf("the kind %q is declared twice", d.Kind)
		}
		seen[d.Kind] = true
		if d.Reason == "" {
			t.Errorf("the kind %q is declared with no reason", d.Kind)
		}
	}
	for _, kind := range drainingKinds {
		if !seen[kind] {
			t.Errorf("the kind %q is not declared", kind)
		}
		if !kind.Drains() {
			t.Errorf("the kind %q breaks, want it to drain: the old pod keeps what it was made with and its roles keep working", kind)
		}
	}
	for _, kind := range breakingKinds {
		if !seen[kind] {
			t.Errorf("the kind %q is not declared", kind)
		}
		if kind.Drains() {
			t.Errorf("the kind %q drains, want it to break: a role can lose a credential or a store it writes", kind)
		}
	}
	if len(seen) != len(drainingKinds)+len(breakingKinds) {
		t.Errorf("%d kinds are declared, want the %d this test names", len(seen), len(drainingKinds)+len(breakingKinds))
	}
	if undeclared := DriftKind("a-kind-no-declaration-names"); undeclared.Drains() {
		t.Errorf("an undeclared kind drains, want it to break")
	}
}

// A round's reasons are read by kind and logged by kind: Breaks is whether any reason is of a kind
// that breaks, Kinds names each reason's kind in order, once, and String is the phrases as a
// round's terms record them, each reason of a kind that breaks starting with BreakingDrift and no
// reason of a kind that drains carrying it.
func TestDriftReasonsAreReadByKind(t *testing.T) {
	draining := DriftReasons{{Kind: DriftImage, Text: "image a, now b"}, {Kind: DriftLeakCheck, Text: "leak check x, now y"}}
	if draining.Breaks() {
		t.Errorf("reasons %v break, want none to: each drains", draining)
	}
	if got, want := draining.String(), "image a, now b, leak check x, now y"; got != want {
		t.Errorf("terms of reasons that only drain = %q, want %q, with no marker", got, want)
	}
	mixed := append(slices.Clone(draining), DriftReason{Kind: DriftAccount, Text: "account a, now b"}, DriftReason{Kind: DriftImage, Text: "image c, now d"})
	if !mixed.Breaks() {
		t.Errorf("reasons %v do not break, want them to: the account breaks", mixed)
	}
	if got, want := mixed.Kinds(), []DriftKind{DriftImage, DriftLeakCheck, DriftAccount}; !slices.Equal(got, want) {
		t.Errorf("kinds = %v, want %v", got, want)
	}
	if got, want := mixed.String(), "image a, now b, leak check x, now y, [breaks the pod] account a, now b, image c, now d"; got != want {
		t.Errorf("terms = %q, want %q", got, want)
	}
	if BreakingDrift != "[breaks the pod] " {
		t.Errorf("BreakingDrift = %q, want the marker INFRA-3271's R7 reads, %q", BreakingDrift, "[breaks the pod] ")
	}
}
