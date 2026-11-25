package api

import (
	"net/http"
	"slices"
	"testing"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestALabelTheDatabaseLowersToLegionFreesNoLegionIssue: R26 must not let a credential with
// neither scope free the status of an issue under a labelled issue, whatever the collation of
// the labels column.
//
// R26 reads "the legion label" in two ways. The removal rule reads it in Go
// (holdsLegionLabel, strings.EqualFold). The walk that says whether an issue is a Legion issue
// reads it in SQL (legionWalkSQL, lower(label collate "C") = 'legion'). The two agree because
// lower() in "C" changes only A-Z. In the column's own collation, lower() can change more:
// glibc's en_US.UTF-8 and C.UTF-8 lower U+0130 (İ) to i, and the Postgres of CI's envoy-go job
// is "initialized with locale en_US.utf8". A walk in such a collation reads LEGİON as the legion
// label, and the removal rule does not. Then a credential with neither scope frees the status in
// three calls:
//
//   - It moves the issue under a root that it made with the label LEGİON. The walk reads a
//     labelled issue above the issue after the move, so R26 takes the move.
//   - It takes LEGİON off that root. Go reads no legion label in the labels before the call, so
//     R26 takes the removal.
//   - It writes the issue's status. No labelled issue is above the issue now, so R26 takes the
//     write.
//
// The test puts the labels column of its database under C.utf8, glibc's C.UTF-8. So without the
// walk's collation "C" the test fails on every server that has glibc's C.UTF-8, a server whose
// database collation is ICU's included. With it, R26 refuses the move, takes the removal (the
// root is no Legion issue), and refuses the status write: the issue keeps its parent and its
// status. The shared token, a project's broker token and a personal token that holds no scope
// each make the three calls, in a subtest of their own.
func TestALabelTheDatabaseLowersToLegionFreesNoLegionIssue(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	ctx := t.Context()
	if _, err := rig.database.Pool.Exec(ctx, `alter table issues alter column labels type text[] collate "C.utf8"`); err != nil {
		t.Fatalf("put the labels column under C.utf8: %v", err)
	}
	var lowered bool
	if err := rig.database.Pool.QueryRow(ctx, `select lower($1::text collate "C.utf8") = 'legion'`, "LEG\u0130ON").Scan(&lowered); err != nil {
		t.Fatalf("lower LEG\u0130ON under C.utf8: %v", err)
	}
	if !lowered {
		t.Fatalf("this server's C.utf8 does not lower U+0130 to i, so the test proves nothing here")
	}

	for _, name := range []string{legionSharedToken, legionProjectBrokerToken, legionUnscopedPersonalToken} {
		credential := rig.credential(t, name)
		t.Run(name, func(t *testing.T) {
			labelled := rig.issue(t, []string{"legion"}, "", "")
			issue := rig.issue(t, nil, labelled, "in_progress")
			made := credential.send(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": brokerTestProject, "title": rig.title(), "force": true, "labels": []string{"LEG\u0130ON"},
			})
			if made.Code != http.StatusCreated {
				t.Fatalf("make a root labelled LEG\u0130ON: status=%d body=%s", made.Code, made.Body.String())
			}
			lookAlike := decodeBody[model.Issue](t, made).Key

			before := readLegionIssue(t, rig.database, issue)
			moved := credential.send(t, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"parent": lookAlike})
			requireLegionStatusRefusal(t, moved)
			requireLegionIssueUnchanged(t, rig.database, issue, before)

			removed := credential.send(t, http.MethodPatch, "/api/v1/issues/"+lookAlike, map[string]any{"labels": []string{}})
			if removed.Code != http.StatusOK {
				t.Errorf("take LEG\u0130ON off %s, which is no Legion issue: status=%d body=%s, want 200",
					lookAlike, removed.Code, removed.Body.String())
			}

			written := credential.send(t, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": "todo"})
			requireLegionStatusRefusal(t, written)
			requireLegionIssueUnchanged(t, rig.database, issue, before)
		})
	}
}

// TestLegionAndALookAlikeLabelAreTwoLabels: Dispatch keeps two labels as one only where
// strings.EqualFold matches them (labelKey). R26 (holdsLegionLabel) and the Legion daemon
// (CarriesLegionLabel) read the legion label by that rule.
//
// strings.ToLower maps LEGİON (with U+0130) to legion, but strings.EqualFold does not match the
// two labels: U+0130 is alone in its unicode.SimpleFold orbit. A de-duplication by
// strings.ToLower reads legion as a copy of LEGİON. Then labels_add of legion on an issue that
// carries LEGİON adds nothing, and R26 and the daemon read no Legion issue. So the daemon hands
// the issue over, and a credential with neither scope can still write its status. The test:
//
//   - A person makes an issue labelled LEGİON and moves it to in_progress. The daemon's broker
//     token adds legion, as the daemon does when it hands an issue over. The issue then carries
//     both labels, and R26 refuses a status write from the shared token: it is a Legion issue.
//   - A person makes an issue with the labels Ä and ä. strings.EqualFold matches the two, so the
//     issue carries Ä alone.
//   - A person's PATCH writes the labels legion and LEGİON. The issue keeps both, in that order.
func TestLegionAndALookAlikeLabelAreTwoLabels(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	t.Run("the daemon adds legion to an issue that carries LEG\u0130ON", func(t *testing.T) {
		issue := rig.issue(t, []string{"LEG\u0130ON"}, "", "in_progress")
		added := rig.credential(t, legionDaemonToken).send(t, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{
			"labels_add": []string{"legion"},
		})
		if added.Code != http.StatusOK {
			t.Fatalf("add legion to %s with the daemon's broker token: status=%d body=%s, want 200",
				issue, added.Code, added.Body.String())
		}
		before := readLegionIssue(t, rig.database, issue)
		if want := []string{"LEG\u0130ON", "legion"}; !slices.Equal(before.labels, want) {
			t.Fatalf("%s after legion was added: labels %q, want %q", issue, before.labels, want)
		}
		written := rig.credential(t, legionSharedToken).send(t, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": "todo"})
		requireLegionStatusRefusal(t, written)
		requireLegionIssueUnchanged(t, rig.database, issue, before)
	})
	t.Run("\u00c4 and \u00e4 are one label", func(t *testing.T) {
		issue := rig.issue(t, []string{"\u00c4", "\u00e4"}, "", "")
		if got, want := readLegionIssue(t, rig.database, issue).labels, []string{"\u00c4"}; !slices.Equal(got, want) {
			t.Errorf("%s made with the labels \u00c4 and \u00e4: labels %q, want %q", issue, got, want)
		}
	})
	t.Run("a person writes legion and LEG\u0130ON", func(t *testing.T) {
		issue := rig.issue(t, nil, "", "")
		written := rig.asPerson(t, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{
			"labels": []string{"legion", "LEG\u0130ON"},
		})
		if written.Code != http.StatusOK {
			t.Fatalf("write the labels legion and LEG\u0130ON on %s as a person: status=%d body=%s, want 200",
				issue, written.Code, written.Body.String())
		}
		if got, want := readLegionIssue(t, rig.database, issue).labels, []string{"legion", "LEG\u0130ON"}; !slices.Equal(got, want) {
			t.Errorf("%s after the person's write: labels %q, want %q", issue, got, want)
		}
	})
}

// TestEachLetterOfTheLegionLabelFoldsOnlyBetweenItsASCIICases holds the fact that the walk's
// comment rests on (legionManagedStatusSQL, issue_parent.go). The walk compares
// lower(label collate "C") with legionLabel, and holdsLegionLabel compares a label with it by
// strings.EqualFold. lower() in "C" changes only A-Z. strings.EqualFold matches a rune with each
// rune of its unicode.SimpleFold orbit. So the two agree only while each letter of legionLabel
// folds only between its own ASCII lower and upper case.
//
// A legionLabel with a k or an s breaks that. k folds with U+212A (the Kelvin sign), and s folds
// with U+017F (long s). lower() in "C" does not change either rune, so the walk would read no
// legion label where holdsLegionLabel reads one.
func TestEachLetterOfTheLegionLabelFoldsOnlyBetweenItsASCIICases(t *testing.T) {
	for _, letter := range legionLabel {
		if letter > unicode.MaxASCII || !unicode.IsLetter(letter) {
			t.Errorf("the legion label %q holds %q, which is not an ASCII letter", legionLabel, letter)
			continue
		}
		lower, upper := unicode.ToLower(letter), unicode.ToUpper(letter)
		for folded := unicode.SimpleFold(letter); folded != letter; folded = unicode.SimpleFold(folded) {
			if folded != lower && folded != upper {
				t.Errorf("%q of the legion label %q folds with %U (%q), which is neither %q nor %q: "+
					"lower() in the collation \"C\" does not change it, so the walk and strings.EqualFold disagree",
					letter, legionLabel, folded, folded, lower, upper)
			}
		}
	}
}
