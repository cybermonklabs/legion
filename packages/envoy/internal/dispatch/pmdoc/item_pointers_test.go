package pmdoc

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The shipped schema's parent and widened needs, each held to its reach (R2, R10 and R11 of the
// issue-tree spec). A parent names a block of a document of the writing issue or of an issue above
// it, or an ask, a comment or a message of one of those that is a conversation node, or a block of
// one; it is refused at an item or item block no row holds under the issue its reference spells,
// at one of a sibling tree, at one that is no conversation node (an agent's status comment, a
// message of the Legion daemon's session), and from a document a project holds. A needs names a
// plan task of the writing issue or of an issue above it, or an issue under the writing issue, and
// is refused at an issue no row holds and at an issue outside the writing issue's tree. A check's
// serves still points at a requirement alone, and every refusal names the attribute and the
// reference.
func TestCheckBlockPointersHoldsAParentAndANeedsToTheirReach(t *testing.T) {
	const (
		comment       = "11111111-1111-4111-8111-111111111111"
		elsewhere     = "22222222-2222-4222-8222-222222222222"
		status        = "33333333-3333-4333-8333-333333333333"
		daemon        = "44444444-4444-4444-8444-444444444444"
		aboveMessage  = "55555555-5555-4555-8555-555555555555"
		siblingAsk    = "66666666-6666-4666-8666-666666666666"
		missing       = "77777777-7777-4777-8777-777777777777"
		personComment = "dispatch://CORE-1/comment/" + comment + "#b-b1"
		movedComment  = "dispatch://CORE-1/comment/" + elsewhere + "#b-b1"
		statusComment = "dispatch://CORE-1/comment/" + status + "#b-b1"
		daemonMessage = "dispatch://CORE-1/message/" + daemon + "#b-b1"
		aboveBlock    = "dispatch://CORE-7/message/" + aboveMessage + "#b-b2"
		siblingPart   = "dispatch://CORE-8/ask/" + siblingAsk + "#b-b1"
		missingBlock  = "dispatch://CORE-1/comment/" + missing + "#b-b1"
		requirement   = "dispatch://CORE-1/spec#b-r1"
		siblingSpec   = "dispatch://CORE-8/spec#b-s1"
		criterion     = "dispatch://CORE-1/spec#b-k1"
		wholeComment  = "dispatch://CORE-1/comment/" + comment
		wholeStatus   = "dispatch://CORE-1/comment/" + status
		wholeDaemon   = "dispatch://CORE-1/message/" + daemon
		wholeAbove    = "dispatch://CORE-7/message/" + aboveMessage
		wholeSibling  = "dispatch://CORE-8/ask/" + siblingAsk
		wholeMissing  = "dispatch://CORE-1/comment/" + missing
	)
	targets := map[string]BlockTarget{
		"comment/" + comment + "#b1":      {Issue: "CORE-1", Conversation: true},
		"comment/" + elsewhere + "#b1":    {Issue: "CORE-8", Conversation: true},
		"comment/" + status + "#b1":       {Issue: "CORE-1"},
		"message/" + daemon + "#b1":       {Issue: "CORE-1"},
		"message/" + aboveMessage + "#b2": {Issue: "CORE-7", Conversation: true},
		"ask/" + siblingAsk + "#b1":       {Issue: "CORE-8", Conversation: true},
		comment:                           {Issue: "CORE-1", Conversation: true},
		status:                            {Issue: "CORE-1"},
		daemon:                            {Issue: "CORE-1"},
		aboveMessage:                      {Issue: "CORE-7", Conversation: true},
		siblingAsk:                        {Issue: "CORE-8", Conversation: true},
		"CORE-1/spec#r1":                  {Types: []string{"requirement"}, Document: "CORE-1/spec", Issue: "CORE-1"},
		"CORE-8/spec#s1":                  {Types: []string{"requirement"}, Document: "CORE-8/spec", Issue: "CORE-8"},
		"CORE-1/spec#k1":                  {Types: []string{"criterion"}, Document: "CORE-1/spec", Issue: "CORE-1"},
		"CORE-1/spec#t5":                  {Types: []string{"plan_task"}, Document: "CORE-1/spec", Issue: "CORE-1"},
		"CORE-2":                          {Issue: "CORE-2"},
		"CORE-3":                          {Issue: "CORE-3"},
		"CORE-8":                          {Issue: "CORE-8"},
	}
	// The write lands in the spec of CORE-1, under CORE-7; CORE-2 is its child and CORE-3 a child
	// of CORE-2; CORE-8 is a sibling tree.
	fleet := func(creatable int) func() (int, error) { return func() (int, error) { return creatable, nil } }
	writing := BlockWriting{Document: "CORE-1/spec", Issue: "CORE-1", Ancestors: []string{"CORE-7"}, Descendants: []string{"CORE-2", "CORE-3"}, Creatable: fleet(9)}
	resolver := func(writing BlockWriting) func([]text.Ref) (BlockWriting, map[string]BlockTarget, error) {
		return func(refs []text.Ref) (BlockWriting, map[string]BlockTarget, error) {
			found := map[string]BlockTarget{}
			for _, ref := range refs {
				if target, ok := targets[text.TargetID(ref)]; ok {
					found[text.TargetID(ref)] = target
				}
			}
			return writing, found, nil
		}
	}
	parse := func(markdown string) *Node {
		t.Helper()
		tree, err := Parse(markdown)
		if err != nil {
			t.Fatalf("parse %q: %v", markdown, err)
		}
		return tree
	}
	parent := func(typ, ref string) *Node {
		return parse(":::" + typ + "{#n1 parent=\"" + ref + "\"}\nThe words.\n:::\n")
	}
	needs := func(ref string) *Node {
		return parse(":::plan_task{#t1 needs=\"" + ref + "\"}\nDo it.\n:::\n")
	}
	for _, test := range []struct {
		name    string
		tree    *Node
		refusal []string
	}{
		{name: "a criterion under a person's comment of its issue", tree: parent("criterion", personComment)},
		{name: "a requirement under a message of the issue above", tree: parent("requirement", aboveBlock)},
		{name: "a check under a block of its issue's document", tree: parent("check", requirement)},
		{name: "a plan task under a person's comment", tree: parent("plan_task", personComment)},
		{name: "a criterion under an ask of a sibling tree", tree: parent("criterion", siblingPart),
			refusal: []string{`"parent"`, siblingPart, "a block of an ask of CORE-8", "CORE-1 or of an issue above it"}},
		{name: "a criterion under a comment no row holds", tree: parent("criterion", missingBlock),
			refusal: []string{`"parent"`, missingBlock, "which no comment of CORE-1 holds"}},
		{name: "a criterion under a comment of another issue than its reference spells", tree: parent("criterion", movedComment),
			refusal: []string{`"parent"`, movedComment, "which no comment of CORE-1 holds"}},
		{name: "a criterion under an agent's status comment", tree: parent("criterion", statusComment),
			refusal: []string{`"parent"`, statusComment, "no conversation node"}},
		{name: "a criterion under a message of the daemon's session", tree: parent("criterion", daemonMessage),
			refusal: []string{`"parent"`, daemonMessage, "no conversation node", "Legion daemon's session"}},
		{name: "a requirement under a block of a sibling tree", tree: parent("requirement", siblingSpec),
			refusal: []string{`"parent"`, siblingSpec, "CORE-8", "CORE-1 or of an issue above it"}},
		{name: "a criterion under a person's comment itself", tree: parent("criterion", wholeComment)},
		{name: "a requirement under a message of the issue above itself", tree: parent("requirement", wholeAbove)},
		{name: "a criterion under an ask of a sibling tree itself", tree: parent("criterion", wholeSibling),
			refusal: []string{`"parent"`, wholeSibling, "an ask of CORE-8", "CORE-1 or of an issue above it"}},
		{name: "a criterion under a comment no row is", tree: parent("criterion", wholeMissing),
			refusal: []string{`"parent"`, wholeMissing, "which is no comment of CORE-1"}},
		{name: "a criterion under an agent's status comment itself", tree: parent("criterion", wholeStatus),
			refusal: []string{`"parent"`, wholeStatus, "a comment that is no conversation node"}},
		{name: "a criterion under a message of the daemon's session itself", tree: parent("criterion", wholeDaemon),
			refusal: []string{`"parent"`, wholeDaemon, "a message that is no conversation node", "Legion daemon's session"}},
		{name: "a plan task needing an issue under its issue", tree: needs("dispatch://CORE-2")},
		{name: "a plan task needing an issue two levels under its issue", tree: needs("dispatch://CORE-3")},
		{name: "a plan task needing an issue of a sibling tree", tree: needs("dispatch://CORE-8"),
			refusal: []string{`"needs"`, "dispatch://CORE-8", "not under CORE-1"}},
		{name: "a plan task needing an issue no row holds", tree: needs("dispatch://CORE-9"),
			refusal: []string{`"needs"`, "dispatch://CORE-9", "which is no issue"}},
		{name: "a requirement serving a criterion", tree: parse(":::requirement{#r9 serves=\"" + criterion + "\"}\nIt follows.\n:::\n")},
		{name: "a check serving a criterion", tree: parse(":::check{#c9 serves=\"" + criterion + "\"}\nIt proves.\n:::\n"),
			refusal: []string{`"serves"`, criterion, `"criterion"`, "requirement"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckBlockPointers(test.tree, nil, resolver(writing))
			if len(test.refusal) == 0 {
				if err != nil {
					t.Fatalf("CheckBlockPointers = %v, want accepted", err)
				}
				return
			}
			if !errors.Is(err, ErrBlockPointer) || errors.Is(err, ErrSchema) {
				t.Fatalf("CheckBlockPointers = %v, want ErrBlockPointer and no schema refusal", err)
			}
			for _, want := range test.refusal {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("CheckBlockPointers = %v, want it to name %s", err, want)
				}
			}
		})
	}

	// A document a project holds has no issue to hold a conversation or an issue under it.
	project := BlockWriting{Document: "CORE/handbook-md", Creatable: fleet(9)}
	for _, tree := range []*Node{parent("criterion", personComment), needs("dispatch://CORE-2")} {
		if err := CheckBlockPointers(tree, nil, resolver(project)); !errors.Is(err, ErrBlockPointer) || !strings.Contains(err.Error(), "a document a project holds") {
			t.Errorf("a project document's pointer = %v, want it refused as one a project holds", err)
		}
	}

	// An issue under needs is an item of a kind version 9 adds (needs' ItemVersions): a write
	// introduces one only while the processes serving the database may create it, and a build of
	// version 8 would read it as a value outside the schema. A plan task's needs, a needs the tree
	// before the write already held, and a parent of no versions are taken whatever the fleet.
	behind := writing
	behind.Creatable = fleet(8)
	err := CheckBlockPointers(needs("dispatch://CORE-2"), nil, resolver(behind))
	for _, want := range []string{`"needs"`, "dispatch://CORE-2", `cannot write it yet`, `item "issue" of attribute "needs" of typed block "plan_task"`} {
		if !errors.Is(err, ErrBlockPointer) || !strings.Contains(err.Error(), want) {
			t.Fatalf("an issue needs while the fleet creates up to 8 = %v, want it refused naming %s", err, want)
		}
	}
	for name, write := range map[string][2]*Node{
		"a needs the tree already held": {needs("dispatch://CORE-2"), needs("dispatch://CORE-2")},
		"a plan task's needs":           {needs("dispatch://CORE-1/spec#b-t5"), nil},
		"a parent":                      {parent("criterion", personComment), nil},
	} {
		if err := CheckBlockPointers(write[0], write[1], resolver(behind)); err != nil {
			t.Errorf("%s while the fleet creates up to 8 = %v, want it taken", name, err)
		}
	}
	unread := writing
	unread.Creatable = nil
	if err := CheckBlockPointers(needs("dispatch://CORE-2"), nil, resolver(unread)); !errors.Is(err, ErrBlockPointer) || !strings.Contains(err.Error(), "cannot write it yet") {
		t.Errorf("an issue needs with no fleet read = %v, want it refused", err)
	}
}

// A parent holds at most one reference, because a node has one parent; needs names no
// conversation and serves names no item at all, whatever the item.
func TestAParentHoldsOneReferenceAndOnlyItNamesTheConversation(t *testing.T) {
	const (
		first  = "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b1"
		second = "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b2"
	)
	if _, err := Parse(":::criterion{#k1 parent=\"" + first + "\"}\nI want it.\n:::\n"); err != nil {
		t.Fatalf("a criterion under one comment block = %v, want accepted", err)
	}
	for markdown, reason := range map[string]string{
		":::criterion{#k1 parent=\"" + first + " " + second + "\"}\nI want it.\n:::\n": "a block has one parent",
		":::plan_task{#t1 needs=\"" + first + "\"}\nDo it.\n:::\n":                     "is a block of a comment",
		":::check{#c1 serves=\"" + first + "\"}\nIt proves.\n:::\n":                    "is a block of a comment",
		":::requirement{#r1 serves=\"dispatch://CORE-2\"}\nIt follows.\n:::\n":         "is not a block reference",
	} {
		if _, err := Parse(markdown); err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("Parse(%q) = %v, want it refused as one that %s", markdown, err, reason)
		}
	}
	typ, _ := typedBlock("criterion")
	if err := validateAttributeValue(ParentAttribute, typ.Attributes.Get(ParentAttribute), []string{first, second}); err == nil {
		t.Error("a tree's parent holding two references is accepted")
	}
}

// An issue under a plan task's needs is an element of the manifest read from version 9, as the
// attribute's ItemVersions give it: a document holding one needs version 9 of a page that writes it
// (DocumentSince), and the manifest as a build of version 8 installs it leaves the item kind out of
// needs, which keeps the plan tasks it points at.
func TestAnIssueUnderNeedsIsAnElementReadFromVersionNine(t *testing.T) {
	element := Element{Kind: ItemElement, Block: "plan_task", Attribute: "needs", Name: string(BlockItemIssue)}
	if versions, declared := element.Versions(); !declared || versions.Since != 9 || versions.CreatableSince != 9 {
		t.Fatalf("%s = %+v, %t; want since and creatable_since 9", element, versions, declared)
	}
	tree, err := Parse(":::plan_task{#t1 needs=\"dispatch://CORE-2\"}\nAfter the child.\n:::\n")
	if err != nil {
		t.Fatalf("parse a plan task needing an issue: %v", err)
	}
	if since := DocumentSince(tree); since != 9 {
		t.Errorf("DocumentSince of a plan task needing an issue = %d, want 9", since)
	}
	plain, err := Parse(":::plan_task{#t1 needs=\"dispatch://CORE-1/spec#b-t0\"}\nAfter it.\n:::\n")
	if err != nil {
		t.Fatalf("parse a plan task needing a plan task: %v", err)
	}
	if since := DocumentSince(plain); since >= 9 {
		t.Errorf("DocumentSince of a plan task needing a plan task = %d, want below 9", since)
	}

	var store BlockSchema
	if err := json.Unmarshal(SchemaJSON(), &store); err != nil {
		t.Fatalf("decode the installed schema: %v", err)
	}
	eight := asOfVersion(store, 8)
	index := slices.IndexFunc(eight.Types, func(typ BlockTypeSchema) bool { return typ.Name == "plan_task" })
	needs, declared := eight.Types[index].Attributes.Lookup("needs")
	if !declared || len(needs.Items) != 0 || len(needs.ItemVersions) != 0 || !slices.Equal(needs.Targets, []string{"plan_task"}) {
		t.Errorf("needs as a build of version 8 installs it = %+v (declared %t), want the plan tasks it points at and no item", needs, declared)
	}
}
