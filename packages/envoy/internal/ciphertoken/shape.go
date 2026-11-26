package ciphertoken

import (
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// tokenShape is what an answer's project scope and its OAuth scope together make of a token
// issued for acting as a session (shapeOf).
type tokenShape int

const (
	// shapeNotASession is an answer no verifier reads as a session's token, whatever its
	// switches: a project scope that is absent or a word this package does not know; the project
	// scope none in any shape but shapeReadOnly's or beside a cut; the project scope results in
	// any shape but shapeResults'; the project scope public beside a person, without a service
	// or beside any OAuth scope or a cut; the project scope named beside no person and any OAuth
	// scope; the scope read or results or a cut beside the project scope named or person; a
	// narrowed project scope in any shape but shapeReadOnly's; the scope approve in any shape but
	// shapeApproveOnly's; or a run beside any token but a results token.
	// The consented scope is no session without its person, beside a service, read, a cut or a
	// non-consentable word.
	shapeNotASession tokenShape = iota
	// shapeOrdinary is the project scope named beside a person with no scope read, results or
	// approve and no cut or run; named beside no person with no OAuth scope, no cut and no run;
	// the project scope person with no scope read, results or approve and no cut or run; or the
	// project scope public beside a service, no person, no OAuth scope, no cut and no run: every
	// token the broker minted before read entries existed, every job's token, and every token it
	// mints from any other entry than a read or result entry, for a trade that does not ask for
	// read.
	// It also is the consented scope beside its person, with no read or cut and consentable scopes
	// alone.
	shapeOrdinary
	// shapeReadOnly is a token that only reads, which a verifier accepts only under
	// Options.ServesReadOnly. It is the token a read entry mints (BROKER_EXCHANGE_READ_ACCOUNTS):
	// the project scope none, the scope exactly the one word read, a service, no person, no
	// projects, no cut and no run. It is also a token narrowed to reading because its trade asked
	// for read: a narrowed project scope (read-named, read-person, read-none) beside the scope
	// read, or read person for a read-person token that names its person, with a cut or none, and
	// no run.
	shapeReadOnly
	// shapeResults is the token a result entry mints (BROKER_EXCHANGE_RESULT_WORKFLOWS): the
	// project scope results, the scope exactly the one word results, a person, one project, no
	// service, and the run it was minted for, which reads whole (oauthwire.ParseCIRun). A verifier
	// accepts it only under Options.ServesResults.
	shapeResults
	// shapeApproveOnly is the token the broker mints for a person's approve grant and spends
	// through its proxy: the project scope person, the scope exactly the one word approve, a
	// person, and no service, no projects and no cut (approveShapeOf). A verifier accepts it only
	// under Options.ServesApproveOnly.
	shapeApproveOnly
)

// shapeOf is the one rule that says what a read-only token, a results token, an approve-only
// token and a job's token are, and so what every other answer is beside them. The limited shapes
// are exact. The read entry's token is the project scope ProjectScopeNone, a Scope that is the
// one word ScopeRead, a service, no person, no projects and no cut, and
// oauthwire.ProjectScopeReadNone is held to the same but may carry a cut. A token of
// oauthwire.ProjectScopeReadNamed or oauthwire.ProjectScopeReadPerson holds the one word
// ScopeRead, and a read-person token that names its person may hold ScopeRead and
// oauthwire.ScopePerson; sessionCaller holds its projects to its base word's rules. A results
// token is the project scope ProjectScopeResults, a Scope that is the one word ScopeResults, a
// person, one project, no service, no cut and a run that reads whole (oauthwire.ParseCIRun),
// which shapeOf answers beside the shape, so the run is read once. The approve-only token is the
// project scope ProjectScopePerson, a Scope that is the one word ScopeApprove, a person, and no
// service, no projects and no cut (approveShapeOf). An answer carrying none, results or a narrowed word in
// any other shape is no session's token, and so is one whose Scope holds the word read or
// results, or that names a cut or carries a run, beside the project scope named or person, one
// that names a cut beside none or results, one that carries a run beside any project scope but
// results, and one whose Scope holds approve in any other shape: no broker mints any of them,
// since a cut needs a trade that asks for read, which mints a narrowed word, and a run a result
// entry, which mints results; a word that limits a token, beside a project scope that writes,
// would let a token meant to read, or to write only its results, write; and approve beside
// anything else would read as an ordinary token whose scope a reader of ordinary tokens ignores.
//
// A consented token is consented.go's: it names its person and its sign-in, no service, no read
// and no cut, and each scope word is consentable. Its projects are read under its person scope.
//
// A token that names no person and is no read-only token — a job's, or an account entry's naming
// no person — holds no OAuth scope at all, since the broker grants neither one, and no cut, since
// the exchange narrows neither. A job's public shape is ProjectScopePublic beside a service, no
// person and no scope word; public beside a person, which no entry that names a person mints,
// with no service, or beside any scope word is no session's token. The named shape of both is
// ProjectScopeNamed beside no person and no scope word; named beside no person and any scope
// word, person and approve included, is no session's token, while named beside a person keeps
// the scope its entry grants.
//
// Scope is read as RFC 7662 spells it, words separated by spaces, and as the set of words RFC 6749
// (section 3.3) makes it: the order of its words means nothing, so read person and person read
// are one scope. A word named twice is no set the broker grants, and no read-only shape. It is
// read one word at a time and with no slice built of them, because this runs on every request a
// verifier decides, an answer served from the cache included.
func shapeOf(answer Introspection) (tokenShape, *oauthwire.Run) {
	// A run belongs to a results token alone: one rule, ahead of every shape, so no case below and
	// no shape added later can take a token that carries a run as any other kind.
	if answer.CI != nil && answer.ProjectScope != ProjectScopeResults {
		return shapeNotASession, nil
	}
	// The approve-only shape next and whole (approveShapeOf), so no word of the read and results
	// shapes below, and no order among their cases, can make a token that holds approve anything
	// but the one shape it is.
	if shape, holds := approveShapeOf(answer); holds {
		return shape, nil
	}
	// reads and persons count the two words a narrowed token's scope is made of, results the one
	// word a results token's scope is, and other is any word besides them.
	reads, persons, results, other := 0, 0, 0, false
	for word := range strings.FieldsSeq(answer.Scope) {
		switch word {
		case ScopeRead:
			reads++
		case oauthwire.ScopePerson:
			persons++
		case ScopeResults:
			results++
		default:
			other = true
		}
	}
	read := reads > 0
	// scoped is whether the answer holds any OAuth scope at all.
	scoped := read || persons > 0 || results > 0 || other
	readAlone := reads == 1 && persons == 0 && results == 0 && !other
	readPerson := reads == 1 && persons == 1 && results == 0 && !other
	resultsAlone := results == 1 && reads == 0 && persons == 0 && !other
	switch answer.ProjectScope {
	case ProjectScopeNamed:
		// A named token that names no person is a job's or an account entry's naming no person,
		// and the broker grants neither one an OAuth scope.
		if read || results > 0 || answer.AsOf != "" || (answer.Person == "" && scoped) {
			return shapeNotASession, nil
		}
		return shapeOrdinary, nil
	case ProjectScopePerson:
		if read || results > 0 || answer.AsOf != "" {
			return shapeNotASession, nil
		}
		return shapeOrdinary, nil
	case oauthwire.ProjectScopeConsented:
		if consentedToken(answer) {
			return shapeOrdinary, nil
		}
	case ProjectScopePublic:
		if !scoped && answer.AsOf == "" && answer.Person == "" && answer.Service != "" {
			return shapeOrdinary, nil
		}
	case ProjectScopeNone:
		// The read entry's token: a trade that asks for no scope keeps the word none, and the
		// broker refuses a cut on a trade that does not ask for read, so none never carries one.
		if readAlone && answer.AsOf == "" && answer.Person == "" && len(answer.Projects) == 0 && answer.Service != "" {
			return shapeReadOnly, nil
		}
	case oauthwire.ProjectScopeReadNone:
		if readAlone && answer.Person == "" && len(answer.Projects) == 0 && answer.Service != "" {
			return shapeReadOnly, nil
		}
	case oauthwire.ProjectScopeReadNamed:
		if readAlone {
			return shapeReadOnly, nil
		}
	case oauthwire.ProjectScopeReadPerson:
		if readAlone || (readPerson && answer.Person != "") {
			return shapeReadOnly, nil
		}
	case ProjectScopeResults:
		// A run missing a field, or holding one in another form, is no results token's: a service
		// holds what the token may write to that run's commit, repository and job.
		if resultsAlone && answer.AsOf == "" && answer.Person != "" && len(answer.Projects) == 1 && answer.Service == "" && answer.CI != nil {
			if run, err := oauthwire.ParseCIRun(*answer.CI); err == nil {
				return shapeResults, &run
			}
		}
	}
	return shapeNotASession, nil
}

// approveShapeOf is shapeOf's reading of an answer whose Scope holds the word ScopeApprove, and
// whether it holds it at all. Such an answer is the approve-only token when its Scope is that one
// word, its project scope ProjectScopePerson, and it names a person, no service, no projects and
// no cut; in any other shape it is no session's token. Scope is read one word at a time with no
// slice built, as shapeOf reads it.
func approveShapeOf(answer Introspection) (tokenShape, bool) {
	approves, others := 0, 0
	for word := range strings.FieldsSeq(answer.Scope) {
		if word == ScopeApprove {
			approves++
		} else {
			others++
		}
	}
	switch {
	case approves == 0:
		return shapeNotASession, false
	case approves == 1 && others == 0 && answer.ProjectScope == ProjectScopePerson && answer.AsOf == "" &&
		answer.Person != "" && answer.Service == "" && len(answer.Projects) == 0:
		return shapeApproveOnly, true
	}
	return shapeNotASession, true
}
