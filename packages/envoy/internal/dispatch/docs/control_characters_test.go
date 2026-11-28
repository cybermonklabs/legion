package docs

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestSeedRenderingRefusesControlCharacters(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		markdown  string
		wantField string
		wantCode  string
	}{
		{
			name:      "id line feed",
			markdown:  ":::check{id=\"k&#xA;forged\"}\nBody.\n:::\n",
			wantField: "field id",
			wantCode:  "U+000A",
		},
		{
			name:      "title ESC CSI",
			markdown:  ":::callout{#n1 kind=\"note\" title=\"x&#x1B;[1Gforged\"}\nBody.\n:::\n",
			wantField: "field title",
			wantCode:  "U+001B",
		},
		{
			name:      "ci_check U+2028",
			markdown:  ":::check{#c1 negative_control=\"break\" ci_check=\"x&#x2028;forged\"}\nBody.\n:::\n",
			wantField: "field ci_check",
			wantCode:  "U+2028",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SeedRendering(tc.markdown)
			if !errors.Is(err, pmdoc.ErrControlCharacter) {
				t.Fatalf("SeedRendering = %v, want pmdoc.ErrControlCharacter", err)
			}
			if !strings.Contains(err.Error(), tc.wantField) || !strings.Contains(err.Error(), tc.wantCode) {
				t.Fatalf("SeedRendering error = %q, want %s and %s", err.Error(), tc.wantField, tc.wantCode)
			}
		})
	}
}

func TestReconcileControlCharactersRepairsBadAttributesAndEmitsEvents(t *testing.T) {
	t.Parallel()
	tree, err := pmdoc.Parse(strings.Join([]string{
		":::callout{#n1 kind=\"note\" title=\"t&#x1B;[1Gbad\"}\nCallout.\n:::",
		":::check{#c1 negative_control=\"break\" ci_check=\"x&#xA;bad\"}\nCheck.\n:::",
		":::check{#c2 negative_control=\"break\" ci_check=\"ci/build\"}\nGood.\n:::",
	}, "\n\n") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	var rec settlementReconciliation
	actor := model.Actor{Kind: "user", ID: "alice"}
	issueKey := "CTL-1"
	rec.reconcileControlCharacters("art-1", artifactOwner{Kind: "issue", IssueKey: &issueKey}, tree, actor)

	if len(rec.repairs) != 2 {
		t.Fatalf("repairs = %d, want 2", len(rec.repairs))
	}
	if len(rec.events) != 2 {
		t.Fatalf("events = %d, want 2", len(rec.events))
	}
	if got := tree.Children[0].Attrs["title"]; got != "" {
		t.Fatalf("callout title after repair = %q, want default empty string", got)
	}
	if _, exists := tree.Children[1].Attrs["ci_check"]; exists {
		t.Fatalf("check c1 ci_check still present after repair: %v", tree.Children[1].Attrs["ci_check"])
	}
	if got := tree.Children[2].Attrs["ci_check"]; got != "ci/build" {
		t.Fatalf("honest check c2 ci_check = %q, want ci/build", got)
	}
	if err := refuseControlCharacters(tree, nil); err != nil {
		t.Fatalf("tree after reconcileControlCharacters still refused: %v", err)
	}
}
