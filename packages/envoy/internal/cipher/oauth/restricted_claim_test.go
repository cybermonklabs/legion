package oauth

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
)

// memberKinds answers MemberKind from a map, or err; the exchange's restricted claim reads nothing
// else of it, so its other two answers refuse.
type memberKinds struct {
	kinds map[string]string
	err   error
}

var errNotAsked = errors.New("the restricted claim asks only MemberKind")

func (memberKinds) Access(context.Context, string, string) (projectaccess.Answer, error) {
	return projectaccess.Answer{}, errNotAsked
}

func (memberKinds) States(context.Context, []string) (map[string]projectaccess.Answer, error) {
	return nil, errNotAsked
}

func (m memberKinds) MemberKind(_ context.Context, person string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.kinds[person], nil
}

// TestTheExchangeMintsTheRestrictedClaimFromTheMemberKind: an exchanged token's restricted claim
// is false only where the broker knows its person is a full member (Dispatch answers full, or the
// trade is the person's own sign-in) and for a token that names no person; a restricted member, a
// person Dispatch holds no row for, a kind it could not be asked, and a broker with no check
// configured all mint restricted, so a token whose person's kind is not known sees only its list.
// The control row (Dispatch answering full) is the one that mints false through Dispatch, so the
// restricted rows fail for the kind and not for a reader that always answers true.
func TestTheExchangeMintsTheRestrictedClaimFromTheMemberKind(t *testing.T) {
	kinds := memberKinds{kinds: map[string]string{"full@example.com": projectaccess.MemberFull, "outside@example.com": projectaccess.MemberRestricted, "gone@example.com": projectaccess.MemberNone}}
	for _, row := range []struct {
		name, tokenType, person string
		access                  projectaccess.Access
		want                    bool
	}{
		{"control: Dispatch answers full", oauthwire.SubjectTokenTypeKubernetes, "full@example.com", kinds, false},
		{"Dispatch answers restricted", oauthwire.SubjectTokenTypeKubernetes, "outside@example.com", kinds, true},
		{"Dispatch holds no row", oauthwire.SubjectTokenTypeKubernetes, "gone@example.com", kinds, true},
		{"Dispatch could not be asked", oauthwire.SubjectTokenTypeKubernetes, "full@example.com", memberKinds{err: errors.New("dispatch down")}, true},
		{"no check configured", oauthwire.SubjectTokenTypeKubernetes, "full@example.com", nil, true},
		{"a person's sign-in", oauthwire.SubjectTokenTypePerson, "outside@example.com", kinds, false},
		{"a token naming no person", oauthwire.SubjectTokenTypeKubernetes, "", kinds, false},
	} {
		if got := memberRestricted(context.Background(), memberKindOnce(row.access), row.tokenType, row.person); got != row.want {
			t.Errorf("%s: restricted = %v, want %v", row.name, got, row.want)
		}
	}
}

// TestAnAbsentRestrictedClaimReadsAsRestricted: every reader takes an introspection without the
// restricted claim (a token an earlier broker minted) as a restricted member's, so it sees only its
// list; the claim false is a full member's, and true a restricted member's.
func TestAnAbsentRestrictedClaimReadsAsRestricted(t *testing.T) {
	for _, row := range []struct {
		name   string
		answer oauthwire.Introspection
		want   bool
	}{
		{"absent", oauthwire.Introspection{Active: true}, true},
		{"false", oauthwire.Introspection{Active: true, Restricted: new(false)}, false},
		{"true", oauthwire.Introspection{Active: true, Restricted: new(true)}, true},
	} {
		if got := row.answer.IsRestricted(); got != row.want {
			t.Errorf("%s: IsRestricted = %v, want %v", row.name, got, row.want)
		}
	}
}
