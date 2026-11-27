// packages/envoy/internal/cipher/proxy/bound.go
// One request on a bound host: the proxyrules.Request it is read as, the one (name, slot) pair of
// the grant it is handled for, the real value behind that pair, the decision and the swap
// (proxyrules.Decide and proxyrules.Rewrite, which decide and swap everything here, with a private
// project's Slack check beside them for a Limited name, slack.go), what goes upstream, and the
// line the call logs.
package proxy

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/fold"
)

// mintAgainBefore is how close to its expiry a minted value may come before the proxy reads the
// grant again, which mints it again. Five minutes, so that no call starts on a token about to
// end: a request the proxy carries may run for minutes - a long poll, a large upload - and a
// token that lapses in the middle of one fails the call rather than being retried.
const mintAgainBefore = 5 * time.Minute

// grantValues is one grant's real values on this pod, read once and then held until a name's
// own reread drops it (DropName), the policy version the read ran under stops being the live
// one, or a minted value comes within mintAgainBefore of its expiry; and the whole entry is
// dropped once the grant's end has passed, by the one rule valuesFor's doc states.
type grantValues struct {
	// read is the read itself: two connections of one grant arriving together take it in turn,
	// and the second reads what the first answered rather than running the release checks
	// again.
	read sync.Mutex

	// mu guards the fields below, and is never held across a read of the broker: a reread
	// that drops a name (DropName) runs on the pod's own reread path and must not wait behind
	// one grant's release checks.
	mu sync.Mutex
	// values is nil until the read has succeeded. A read that failed leaves it nil, so a
	// refusal the broker may not repeat - a database that did not answer - is not held against
	// the next connection, while a refusal it will repeat costs one more read and the same
	// answer. It is replaced, never written to: a caller is reading the map it was handed.
	values map[string]requests.AirlockSecret
	// version is the policy version that read ran under, as the broker answered it with the
	// values (AirlockValues).
	version string
	// drops counts the drops this grant has seen, and keptAt the count values was read at.
	// They differ exactly while the held answer is older than the last reread of one of its
	// names, which is what makes the next request read again - a read that began before a
	// drop landed included, since such an answer is its own request's and no later one's.
	drops, keptAt uint64
	// ends is the grant's end, its connection's Expires, and zero for a grant that carries none.
	// It is set when the entry is made and never after, since nothing extends a grant's end, and
	// read under the Server's valuesMu, which is what valuesFor drops an ended grant's entry under.
	ends time.Time
}

// held is the grant's values when they may still be used, and the drop count the read that took
// them began at. They may not be used when no read has succeeded, when a name of the grant has
// been reread since, when the policy has moved on from the version the read ran under
// (version, against the live one), or when a minted value is
// within mintAgainBefore of its expiry: that one name is what the read is for, but the read is
// of the whole grant, since AirlockValues answers a grant and not a name.
//
// The policy version is what carries a change the broker took by its periodic reload - a retag
// a console or an infra-applier made, a raised tier, a disabled version - to the values a pod
// already holds: a name's reread drops the names served from the value it read where it moved
// that value (DropName), and nothing else did. It holds each profile's digest too
// (policy.NewSet), so a broker serving an edited profile answers another version, and a token
// minted under the profile as it was is not spent again. The values are read again, which runs
// the release checks again, and a grant those checks now refuse ends there: one whose profile is
// not the one its request was decided under among them.
//
// keep is Options.KeepValues: it holds the answer across all three, the reread, the moved
// version and the margin alike, which is the negative control of the checks they belong to and
// is refused on a production start.
func (g *grantValues) held(now time.Time, version string, keep bool) (map[string]requests.AirlockSecret, uint64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.values == nil {
		return nil, g.drops, false
	}
	if keep {
		return g.values, g.drops, true
	}
	if g.keptAt != g.drops || g.version != version {
		return nil, g.drops, false
	}
	for _, secret := range g.values {
		if !secret.Expires.IsZero() && secret.Expires.Sub(now) < mintAgainBefore {
			return nil, g.drops, false
		}
	}
	return g.values, g.drops, true
}

// keep holds what one read answered, and the policy version it ran under, unless a name was
// dropped while that read ran: such an answer is the request that asked for it and no later
// one's.
func (g *grantValues) keep(values map[string]requests.AirlockSecret, version string, drops uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.drops != drops {
		return
	}
	g.values, g.version, g.keptAt = values, version, drops
}

// drop takes name out of this grant's values and leaves every other name of it in place.
func (g *grantValues) drop(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, held := g.values[name]
	if !held && g.values != nil && g.keptAt == g.drops {
		// A whole answer this name was never part of, with no read behind it: this reread is
		// no business of this grant's, and ending the answer would cost it a release check
		// for nothing.
		return
	}
	if held {
		// Copied rather than written to, since a request is reading the map it was handed.
		values := maps.Clone(g.values)
		delete(values, name)
		g.values = values
	}
	// Counted even where nothing was deleted: a read may be running that is about to hold
	// this name's old value, and the count is what makes such an answer its own request's.
	g.drops++
}

// valuesFor is the values of grant, the connection read of the grant a request is handled for:
// AirlockValues once per grant per pod, as its own contract asks, and the answer kept for every
// later connection and request of that grant until something ends it - the reread of one of its
// names, a policy version that has moved since the read, or a minted value coming within
// mintAgainBefore of its expiry.
//
// The map holds one entry per grant this pod serves, and this is the one rule of when an entry
// leaves it whole. A grant whose connection carries an end (grantValues.ends, its Expires) ends
// there, and nothing extends it: a grant released from a stored secret at its grant's expiry, and
// a minted capability's, which come one per use - the approve capability's one per review - at the
// end it was made with. So the read that adds a grant's entry first drops the entry of every grant
// whose end this pod's clock has reached, and the map never holds one entry per use since the pod
// started. A grant with no end (a grant that lasts until its session ends) keeps its entry until
// the pod is rolled, which every deployment and every change to the profiles file does, and a
// grant ended early, by a revoke or an ended enrollment, keeps its entry until its end passes.
// Holding such an entry is safe: what ends a grant's reach is the connection check, which refuses
// a placeholder whose grant has ended whatever this map still holds, so no request is handed the
// entry. The drop reads nothing: a later request of a grant it dropped is refused by the
// connection check, and one in flight keeps the values it was handed. A pod whose clock runs ahead
// of the database's drops an entry a little early, which costs the grant's next request one more
// read.
func (s *Server) valuesFor(ctx context.Context, grant requests.AirlockConnection) (map[string]requests.AirlockSecret, error) {
	// Read before the map's lock is taken, since a clock that panics would otherwise leave that
	// lock held, and every later request of every grant behind it.
	now := s.now()
	s.valuesMu.Lock()
	held, ok := s.values[grant.GrantID]
	if !ok {
		for id, other := range s.values {
			if !other.ends.IsZero() && !now.Before(other.ends) {
				delete(s.values, id)
			}
		}
		held = &grantValues{}
		if grant.Expires != nil {
			held.ends = *grant.Expires
		}
		s.values[grant.GrantID] = held
	}
	s.valuesMu.Unlock()

	// The live policy version, read once for both of the checks below: the broker holds one
	// snapshot, so this is a read of a pointer and costs no query.
	version := s.grants.PolicyVersion()
	// The common path, checked before the single-flight lock: the grant's values are held and
	// still usable, so every parallel request of one grant answers from them at once. Taking
	// read first would put them all in a queue behind a read that is not going to happen.
	if values, _, fresh := held.held(now, version, s.keepValues); fresh {
		return values, nil
	}

	held.read.Lock()
	defer held.read.Unlock()
	values, drops, fresh := held.held(s.now(), version, s.keepValues)
	if fresh {
		return values, nil
	}
	values, readVersion, err := s.grants.AirlockValues(ctx, grant.GrantID)
	if err != nil {
		return nil, err
	}
	if values == nil {
		// A grant that released no airlock name at all: held as an answer, so the release
		// checks run once for it too.
		values = map[string]requests.AirlockSecret{}
	}
	// The version the read itself ran under, not the one read above: a reload between the two
	// would otherwise mark this answer as made under a policy it was not.
	held.keep(values, readVersion, drops)
	return values, nil
}

// DropName drops name's value from every grant this pod holds values for, leaving the rest of
// each grant's names in place: a later request of such a grant reads its values again, and mints
// again what the new version of name needs minting from. The broker calls it for every name whose
// value a reread moved (policy.Lookup.Moved): each name served from a stored value at every
// reread of it, since the reread is the only signal a stored value has a new version, and each
// name served from a type secret at a reread that moved its value version. A grant's values
// are held by the name it was released under, which for a profile of another name is the
// profile's and not its source's.
//
// Options.KeepValues turns it off altogether, because the drop is what a reread does to a stored
// value: held's keep makes a stale answer usable, but a name this has taken out of the map is
// not there to be used, and the next request reads the grant again and finds the new value. That
// switch is the negative control of the check this behaviour belongs to, and a production start
// with it set is refused.
func (s *Server) DropName(name string) {
	if s.keepValues {
		return
	}
	s.valuesMu.Lock()
	defer s.valuesMu.Unlock()
	for _, held := range s.values {
		held.drop(name)
	}
	s.dropAccess(name)
}

// serveBound answers one request on a connection the proxy decrypted: the whole of what the
// airlock does to a request, in the order the refusals have to run in. The grant is read again
// for each of them, so that what a connection may carry is what the grant holds now and not
// what it held when the CONNECT was answered. An HTTP/2 connection carries many requests at
// once, each a stream with an authority of its own, and each stream is one call of this
// function: decided, swapped and forwarded to the CONNECT target as an HTTP/1.1 request is.
func (s *Server) serveBound(w http.ResponseWriter, r *http.Request) {
	info, ok := r.Context().Value(connKey{}).(connInfo)
	if !ok || info.target == "" {
		// The connection was not registered, which only a drain racing a hand-over leaves.
		// Nothing is forwarded: this request has no CONNECT target to check its Host against.
		fail(w, http.StatusInternalServerError, codeTargetLost, "this connection's CONNECT target was lost, so nothing can be decided about this request")
		return
	}
	// A request for another protocol on this connection: what crosses after its answer is no
	// request the proxy reads, so a decision made for it would let the value ride every byte
	// after it. Refused before the grant is read, since no grant carries one. A CONNECT asks the
	// service for a tunnel, in HTTP/1.1 and HTTP/2 alike; HTTP/2's extended CONNECT, which
	// carries :protocol, is reset by net/http's server unless GODEBUG=http2xconnect=1 turns it on,
	// and is a CONNECT here when it does. An upgrade to h2c asks the service to speak HTTP/2 in
	// the clear on this connection; net/http's HTTP/2 server answers any Upgrade header 400
	// itself, so this is HTTP/1.1's. The decision has no wire format (the Lean model's Request has
	// none), so these two are the proxy's, as the upgrade, the AWS signature and the Azure
	// exchange are.
	switch {
	case r.Method == http.MethodConnect:
		s.refuse(w, r, info.callFor(pair{}), http.StatusForbidden, codeProtocolSwitch, "refusing a CONNECT inside a connection the proxy decrypted: it would open a tunnel to the service whose bytes the proxy does not read")
		return
	case httpguts.HeaderValuesContainsToken(r.Header["Upgrade"], "h2c"):
		s.refuse(w, r, info.callFor(pair{}), http.StatusForbidden, codeProtocolSwitch, "refusing an upgrade to h2c: it would carry HTTP/2 in the clear past the requests the proxy reads")
		return
	}
	// The grant behind this connection's password, read again for this request. A grant that
	// has ended, been revoked, had its enrollment's lease lapse or had this name withheld
	// since the CONNECT must stop at the next request on the connection, and the connection's
	// own idle limit is no bound on when that is: a command sending one request every few
	// minutes holds one connection open for as long as it likes. The read is the three
	// indexed reads AirlockConnection makes and nothing else, which is what its own contract
	// says it is for - the full release checks are AirlockValues's, once per grant per pod.
	connection, err := s.grants.AirlockConnection(r.Context(), info.password)
	if err != nil {
		// The one rule for a failed grant read (grantRefusal): the read's own answer about
		// this grant is 407 in the broker's words, which a command reads from its proxy, and
		// a read that did not happen is 503 and says nothing of why. The line is logged
		// against the grant the connection was opened for, which is the only grant this
		// request has a name for.
		status, code, why := s.grantRefusal("connection", err)
		s.refuse(w, r, info.callFor(pair{}), status, code, why)
		return
	}
	// Everything below reads the grant this request was decided for: the names it still
	// carries, the placeholders behind them, and the ids its line logs. Every dial made for it -
	// an Azure exchange's as well as the call's own - runs under that grant too (withCaller), so
	// its line names the caller the request's own line names, and not the one the CONNECT read,
	// which a grant that moved since no longer is.
	info.grant = connection
	r = r.WithContext(withCaller(r.Context(), connection))
	// The person the grant's session acts for, asked of the hold for this request as for the
	// CONNECT, and before any value is read: a request on a connection opened before Dispatch
	// held them stops here, the values this pod holds for the grant left unread (holdRefusal).
	if status, code, why := s.holdRefusal(r.Context(), connection.Operator); status != 0 {
		s.refuse(w, r, info.callFor(pair{}), status, code, why)
		return
	}
	request := &proxyrules.Request{
		ConnectHost: info.host,
		// The port the CONNECT named, which is 443 for every connection the proxy decrypts
		// (serveConnect's candidates answer nothing on another port). The decision reads it
		// all the same, so a connection that ever reached here on another port carries no
		// value off it.
		ConnectPort: info.port,
		// The three names the decision compares are ASCII lower-cased, as a kind's hosts
		// are: host names are case-insensitive and the model compares them exactly, so a
		// client that wrote "API.example.com" in one of the three is not a mismatch. The fold
		// is fold.ASCII, ASCII-only for the reason the proxyrules package gives.
		SNI:        fold.ASCII(sniOf(r)),
		HostHeader: fold.ASCII(hostHeaderOf(r.Host)),
		Method:     r.Method,
		Path:       r.URL.Path,
		// The path as the command sent it, percent-encoding kept, which is the one form that
		// tells an encoded "/" the decision refuses from a "/".
		RawPath: r.URL.EscapedPath(),
		Headers: headersOf(r.Header),
		Query:   r.URL.RawQuery,
		// The decision never reads the body. The airlock swaps headers alone, so nothing it
		// decides depends on the body; reading one would mean holding every upload a command
		// makes in the broker's memory before it could be forwarded, and the proxy streams
		// instead. The one body this function reads is a Limited name's, which slackPostAllowed
		// below reads, bounded, to tell which Slack channel it posts to.
		Body: "",
	}
	// The airlock's rules once for this request, from the live policy: a secret's proxy tag can
	// change between two requests of one connection, as a name can be withheld, and the
	// candidates and the decision below read this one copy, so they decide under one policy.
	rules := s.grants.AirlockRules()
	// The (name, slot) pairs this request may be handled for, from the grant as it is now: a
	// name the operator has withheld since the CONNECT is not among them, and its placeholder
	// stops at this request while the grant's other names go on.
	candidates := candidates(rules, info.grant, info.host, info.port)
	if len(candidates) == 0 {
		// The host was bound for this connection and no name of the grant binds it now: every
		// name that did has been withheld or has ended since the CONNECT. Refused rather than
		// forwarded, since a connection to a bound host that carries no name has no business
		// on it.
		s.refuse(w, r, info.callFor(pair{}), http.StatusForbidden, codeNoBoundName, "no name of this grant is bound to "+info.host)
		return
	}
	chosen, holdsPlaceholder := choosePair(candidates, info.grant, request)
	// The binding's way is how the value behind this name goes into the call: swap and mint, a
	// string a header carries, which the swap below puts there; sign, a key pair the call itself
	// has to be signed with (aws.go); exchange, a federated token that has to be exchanged at a
	// token endpoint for an access token (azure.go); dpop, a token the broker minted for a
	// minted capability's grant, which the swap puts in and mintedTransport signs a DPoP proof of
	// the pod's key for (dpop.go). It is the way candidates already read for this name, carried
	// on the pair rather than looked up again.
	way := chosen.way
	if s.noKind && way != proxyrules.WayDPoP {
		// Options.NoKind: the binding's way is read as a swap, so an exchange or a sign takes
		// the plain header swap below, and the federated token's refusal below holds as it does
		// under every way. It is the negative control of the check those two ways belong to,
		// and is refused on a production start. A dpop binding keeps its way: read as a swap,
		// it would send the grant's token on with no proof and with the command's forwarding
		// headers.
		way = proxyrules.WaySwap
	}
	exchange := way == proxyrules.WayExchange && isTokenRequest(r)
	if exchange {
		// A token request carries the grant's placeholder in its form rather than in a header,
		// so what it is handled for is the federated slot whatever choosePair read of its
		// headers. The exchange below checks the form itself.
		chosen.slot = requests.SlotAzureFederated
	} else if holdsPlaceholder && chosen.slot == requests.SlotAzureFederated {
		// The federated token leaves the broker inside an exchange at the token authority and
		// nowhere else (exchangeToken, azureBound): Entra takes it as the assertion of every
		// application federated with the broker, so the header swap below would hand the host
		// this request goes to - the authority or any other host the kind binds - the
		// assertion of each of them. The refusal follows the slot rather than the way: an
		// azure-login name's value fills this slot whatever way its binding takes
		// (requests.airlockSplitOf reads the policy type first), so under another way, where no
		// exchange is made, it is the same token. A request that
		// carries no placeholder of the grant is still handled for this slot while the grant
		// holds no access slot, and goes on with nothing swapped into it: az reads the tenant's
		// OpenID configuration that way before its first token request.
		s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, codeFederatedSwap, "refusing to swap "+chosen.name+"'s federated token into a request that is no token request: that token leaves the broker only in an exchange at the token authority")
		return
	}
	// The secret this request is handled for and the placeholder of its slot: what the decision
	// and the swap below are over, and what a minted capability's admission decides first.
	grant := proxyrules.Grant{Secret: chosen.name, Placeholder: info.grant.Names[chosen.name].Slots[chosen.slot]}
	if way == proxyrules.WayDPoP {
		// A minted capability: its own admission, every rule of which is read before any value is
		// minted or any dial is made (mintedRefusal, dpop.go). Neither azure branch above is taken
		// for one: its way is dpop, and a minted grant's one slot is SlotValue (requests'
		// mintedConnection), never the federated one.
		if code, why := s.mintedRefusal(rules, r, request, grant, holdsPlaceholder); code != "" {
			s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, code, why)
			return
		}
	}
	values, err := s.valuesFor(r.Context(), info.grant)
	if err != nil {
		// The same rule as the connection read above (grantRefusal), so that one answer does
		// not say the grant has ended where the other says the broker could not read it: the
		// full release checks ending a grant is 407, and a read of them that did not happen
		// is 503 and carries none of its own words, which here would be the store's.
		status, code, why := s.grantRefusal("values", err)
		s.refuse(w, r, info.callFor(chosen), status, code, why)
		return
	}
	secret, ok := values[chosen.name]
	if !ok {
		s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, codeNoValue, "the broker released no value for "+chosen.name)
		return
	}
	if !slotReleased(secret, chosen.slot) {
		// The grant holds a placeholder for a slot the broker's read of this name no longer
		// fills: a github-app whose installation on that owner has gone since the release.
		// Refused rather than forwarded with the placeholder in it, which would read as the
		// service's own refusal.
		s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, codeNoValue, noSlotValue(chosen))
		return
	}
	value, held := secret.Slots[chosen.slot]
	if !held {
		// An access token of an exchange some pod made: the broker holds no value for such a
		// slot, and this pod either holds the token already or exchanges for itself to get it.
		value, err = s.azureAccess(r.Context(), info, chosen, secret)
		if err != nil {
			s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, codeAccessToken, err.Error())
			return
		}
	}
	if secret.Limited && way != proxyrules.WaySwap {
		// A Limited name is spent only on a Slack post, which is a header swap. A binding of
		// another way mints, signs or exchanges what it carries, which slackPostAllowed has no
		// reading of, so it carries nothing rather than go out unchecked.
		s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, codeChannelNotAllowed, channelNotAllowed+": this secret's way is "+way+", not a header swap")
		return
	}

	// refused answers a request the decision will not carry, and reports that it did. The
	// reason and the status are proxyrules.Decide's own, as they were when every path through
	// this function asked it first.
	refused := func() bool {
		allowed, why := proxyrules.Decide(rules, grant, value, request)
		if !allowed {
			s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, codeNotAllowed, why)
		}
		return !allowed
	}
	if exchange {
		if refused() {
			return
		}
		// The one request the proxy answers itself: the broker makes the exchange with the
		// real federated token and hands the command a placeholder access token, as the
		// application and in the tenant the name's own document names and no other.
		s.exchangeToken(w, info, r, chosen, secret)
		return
	}

	// What the request leaves with: its headers and its query. A header swap changes the
	// headers the kind names, and removes Cipher-Repository, and nothing else; the AWS path
	// replaces the signature, which is the Authorization header, the date, the session token and
	// the canonical query, after it removes Cipher-Repository itself, as the swap would, so that
	// header never reaches the service and the signature never covers it.
	header, rawQuery := http.Header(nil), r.URL.RawQuery
	if way == proxyrules.WaySign {
		if refused() {
			return
		}
		r.Header.Del(proxyrules.CipherRepositoryHeader)
		signed, signedQuery, release, why := s.signAWS(r.Context(), r, hostHeaderOf(info.target), info.grant.Names[chosen.name].Slots, secret.Slots)
		if why != "" {
			// A refusal gave back whatever of the hashing budget it took, so there is no
			// release to defer here.
			s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, codeAWSSignature, why)
			return
		}
		// The body signAWS read to hash it is what this request serves upstream, so the
		// budget it is held against is given back when the round trip below has ended and
		// not when the signature was made.
		defer release()
		header, rawQuery = signed, signedQuery
	} else {
		// The swap first, and the decision only to explain a refusal. Decide and Rewrite take
		// the one decision (proxyrules.check), which is deterministic and has no side effect, so
		// Rewrite answers nil for exactly the requests Decide refuses: running it first costs
		// an allowed swap - the commonest request the proxy serves - one decision rather than
		// two, and leaves the refusal, the order its reasons are reported in and the 403
		// exactly as they were. A dpop request has had every rule of the decision but the value's
		// read already, before its values were read (mintedRefusal, proxyrules.Admit).
		rewritten := proxyrules.Rewrite(rules, grant, value, request)
		if rewritten == nil {
			if refused() {
				return
			}
			// Decide allowed what Rewrite refused, which the one decision behind both makes
			// unreachable; refusing rather than forwarding is what keeps it unreachable and
			// harmless if the two ever part.
			s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, codeSwapRefused, "the swap refused a request the decision allowed")
			return
		}
		header = headerOf(rewritten.Headers)
		if secret.Limited {
			// A private project's grant of a project-channels secret
			// (requests.AirlockSecret.Limited): the swap allowed the request, and
			// slackPostAllowed decides whether it is a post this grant may make. What it read is
			// what goes upstream, byte for byte, so the post Slack reads is the one checked.
			body, why := slackPostAllowed(r, secret.Channels)
			if why != "" {
				s.refuse(w, r, info.callFor(chosen), http.StatusForbidden, codeChannelNotAllowed, channelNotAllowed+": "+why)
				return
			}
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		}
	}
	// Headers of the proxy itself, which belong to this connection and not to the service. The
	// swap and the signature have already run, so a header the kind names keeps its value and
	// the signature covers neither of these.
	header.Del("Proxy-Authorization")
	header.Del("Proxy-Connection")

	id := answerOf(w).ID()
	// Every call the proxy carries is held to its grant for as long as it runs, as a request on
	// this connection is held to it when it starts. A call can run for as long as its service
	// answers - a gRPC call whose answers stream, a server-sent event stream, a long poll, an
	// upload - and no request is read on it again, so a watcher reads the grant for it every
	// watch interval (watchCarried) and ends it once the grant stops carrying it. carried is
	// the call's own context: the watcher cancels it with why, which ends the call at the
	// service, and ServeHTTP's return cancels it too, which stops the watcher.
	carried, cut := context.WithCancelCause(r.Context())
	defer cut(nil)
	// What ReverseProxy is handed: this request with the swap's headers and that context, and the
	// trace that marks the moment it may leave for the service (leaving), which is what the
	// ErrorHandler's answers below say of it.
	// ReverseProxy drops the hop-by-hop headers of what it is handed - the Connection header
	// and every header it names, Keep-Alive, Te, Upgrade and the rest - before it builds what
	// leaves, and sets back the ones an upgrade or trailers need, so the swap's headers go to it
	// as the request's own rather than over what it built: a Connection header the command sent
	// would otherwise reach the service, and fail every call to one that speaks HTTP/2.
	out := leaving(w, r.WithContext(carried))
	out.Header = header
	served := &statusWriter{ResponseWriter: w}
	// The command's side of the call, which the watcher ends too: the cancel above ends the
	// call at the service, and neither the command's upload nor a write of the service's answer
	// to the command is ended by it (commandSide).
	toCommand := &commandSide{w: served}
	defer toCommand.finish()
	end, message := func(why *cutCall) { cut(why); toCommand.stop() }, cutMessage
	if decrypted, ok := r.Context().Value(decryptedKey{}).(net.Conn); ok && upgrading(r) {
		// An upgrade is the one request whose connection outlives it. ReverseProxy joins the
		// two connections inside the call below and copies bytes until one end closes, so it is
		// the connection the watcher ends, the command's side first (cutJoined).
		end, message = func(*cutCall) { cutJoined(info.cut, decrypted) }, joinMessage
	}
	go s.watchCarried(carried, info, r, chosen, id, message, end)

	upstreamFailed := s.badGateway(info.host, r.URL.Path)
	// refusedByCut is whether the watcher ended the call before its service answered, which
	// ErrorHandler answers as a refusal; ErrorHandler runs inside ServeHTTP, on this goroutine.
	refusedByCut := false
	// Where the request goes: the CONNECT target over TLS on the proxy's own transport, which
	// checks and dials the target's addresses (dialChecked); or, for a private destination's
	// bound host, that destination URL's scheme and host on that destination's own transport,
	// which checks and dials its addresses (dialUpstream). Every other host keeps the proxy's own
	// transport, so each destination takes the requests of its one host and no other.
	var transport http.RoundTripper = s.upstream
	scheme, upstreamHost := "https", info.target
	if d := s.destinationOf(info.host); d != nil {
		transport, scheme, upstreamHost = d.transport, d.upstream.URL.Scheme, d.upstream.URL.Host
		// A call the destination did not answer is answered without the transport's error,
		// which names the destination's in-cluster address (destinationUnreachable).
		upstreamFailed = s.destinationUnreachable(info.host, r.URL.Path)
	}
	if way == proxyrules.WayDPoP {
		// The swap put the grant's token in; mintedTransport signs its proof with the pod's key,
		// for the bound host's origin whatever address the request is sent to, and hands it to
		// the transport above.
		transport = &mintedTransport{key: s.dpopKey, origin: "https://" + info.host, base: transport}
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		// httputil's own lines - a body copy that failed, which quotes what the service sent -
		// go through this package's log rather than the standard logger (Server.errorLog).
		ErrorLog: s.errorLog,
		// -1: every write is flushed as it arrives, so a streamed answer - a long poll, a
		// server-sent event stream, a large download - reaches the command as the service
		// sends it rather than in buffers. An answer with no body is the one exception
		// (statusWriter.whole).
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{
				Scheme:   scheme,
				Host:     upstreamHost,
				Path:     r.URL.Path,
				RawPath:  r.URL.RawPath,
				RawQuery: rawQuery,
			}
			// The bound host, wherever the request is sent: the service reads the request as one
			// to its public name, which a dpop binding's proof names too.
			pr.Out.Host = hostHeaderOf(info.target)
			if way == proxyrules.WayDPoP {
				// A minted capability's request goes on without any forwarding header the command
				// sent, so Dispatch reads the broker's address and never one the command wrote:
				// ReverseProxy has dropped Forwarded and X-Forwarded-*, which are not set back
				// here, and dpopDroppedHeaders are the two it leaves.
				for _, name := range dpopDroppedHeaders {
					pr.Out.Header.Del(name)
				}
				return
			}
			// ReverseProxy drops the command's own Forwarded and X-Forwarded-* headers before
			// Rewrite runs, for a proxy that writes its own; this one writes none and changes
			// no header but the ones the kind names and Cipher-Repository, which it removes, so
			// they go on as the command sent them, and as an AWS signature covered them - all but
			// one the command's Connection header names, which belongs to its connection to the
			// proxy as every header named there does, and which ReverseProxy dropped for that
			// reason.
			for _, name := range forwardedHeaders {
				values, sent := pr.In.Header[name]
				if sent && !httpguts.HeaderValuesContainsToken(r.Header["Connection"], name) {
					pr.Out.Header[name] = values
				}
			}
		},
		// The one thing read of an answer is whether it has a body at all (statusWriter.whole).
		// It is streamed back as it came otherwise: nothing of it is read, buffered or
		// decompressed, because this design changes no answer from an API service - what it
		// guards is what leaves, not what comes back. The one change is the two headers that
		// say the answer is the service's and not the proxy's own (ownAnswer): the service's own
		// Cipher-Proxy-Code goes, so a service cannot pass for the proxy.
		ModifyResponse: func(answer *http.Response) error {
			if !toCommand.start() {
				// The watcher ended the call between the service's answer and here, so
				// nothing of that answer has reached the command: it is refused as one the
				// service had not answered (ErrorHandler).
				return context.Cause(carried)
			}
			answer.Header.Set(forwardedHeader, "true")
			answer.Header.Del(codeHeader)
			served.whole.Store(answer.ContentLength == 0)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, sent *http.Request, err error) {
			var why *cutCall
			if !errors.As(context.Cause(sent.Context()), &why) {
				upstreamFailed(w, sent, err)
				return
			}
			// The watcher ended this call before its service's answer reached the command, so
			// the answer is the refusal every call of this grant now gets, rather than a 502
			// that would read as a service out of reach. Its record is the call's one line, as
			// every refusal's is. The call may have reached the service before the cut - it can
			// hold a whole write and not have answered, or have answered on the way back - so the
			// answer says Cipher-Proxy-Forwarded: unknown once the transport held a connection for it
			// (leaving, ownAnswer).
			refusedByCut = true
			s.refuse(w, r, info.callFor(chosen), why.status, why.code, why.why)
		},
	}
	// The service's answer, which the proxy carries and does not write: counted served whatever
	// its status, unless the service could not be reached (badGateway) or the watcher ended the
	// call before it answered (refuse).
	answerOf(w).carry()
	proxy.ServeHTTP(served, out)
	if refusedByCut {
		return
	}
	s.log.Info(callMessage, s.callFields(id, info.callFor(chosen), r, served.status, "")...)
}

// commandSide is the command's side of one carried call, which the call's watcher ends as well
// as the service's. Cancelling the call's context does not end it, for two reasons:
//
//   - The command's upload. The transport sends the reset that ends the call at the service only
//     once it has closed the request body it was copying from the command, and an HTTP/1.1
//     request body's Close reads on from the command, for up to 256 KiB, until the body ends:
//     an upload the command is still sending holds that Close, and so the service's read of it,
//     for as long as the command likes. So the watcher's end sets a read deadline in the past on
//     the command's side, whatever the call has reached: on HTTP/1.1 every read of the
//     command's connection fails at once, which lets the Close return and closes the connection
//     once the call has been answered; on an HTTP/2 stream net/http ends that stream's request
//     body alone. A read deadline stops no write, so the refusal the call gets before its
//     service has answered (ErrorHandler) still reaches the command.
//   - The service's answer. A write of it that is waiting for the command to read goes on
//     waiting: an HTTP/2 stream whose receive window the client keeps at zero holds the handler
//     in that write for as long as the client likes, and hands the client the answer's bytes when
//     it opens the window. So once the answer has begun reaching the command, the watcher's end
//     sets a write deadline in the past too: on an HTTP/2 stream net/http resets that stream at
//     once (RST_STREAM) and leaves the connection's other streams alone, and on an HTTP/1.1
//     connection, which carries this call alone, the write fails and the connection closes.
//     Before the answer has begun it sets none, so that refusal can be written.
//
// The watcher runs on a goroutine of its own, so the steps are taken under one lock, and nothing
// is set once serveBound has returned and the writer is net/http's again.
type commandSide struct {
	w       http.ResponseWriter
	mu      sync.Mutex
	started bool // the service's answer has begun reaching the command (start)
	ended   bool // the watcher has ended the call (stop)
	done    bool // serveBound has returned (finish)
}

// pastDeadline is the deadline that ends a read or a write at once: any instant before now.
var pastDeadline = time.Unix(1, 0)

// start marks the service's answer as about to be written to the command, and answers false
// where the watcher has already ended the call, whose answer must then not be written.
func (c *commandSide) start() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return false
	}
	c.started = true
	return true
}

// stop ends the command's side of the call: its upload, and the service's answer where that has
// begun reaching it.
func (c *commandSide) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended = true
	if c.done {
		return
	}
	controller := http.NewResponseController(c.w)
	_ = controller.SetReadDeadline(pastDeadline)
	if c.started {
		_ = controller.SetWriteDeadline(pastDeadline)
	}
}

// finish marks serveBound as returned, after which stop leaves the writer alone.
func (c *commandSide) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = true
}

// forwardedHeaders are the forwarding headers httputil.ReverseProxy drops from a request before
// Rewrite runs, which serveBound sets back as the command sent them, each that the command's
// Connection header does not name.
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// dpopDroppedHeaders are the forwarding headers ReverseProxy leaves on a request, which a request
// to a host a dpop binding binds loses in Rewrite: with the four ReverseProxy drops and
// serveBound does not set back for that kind, they are the six that Dispatch reads a caller's
// address from (internal/dispatch/routes's forwardingHeaders), so a minted capability's request
// reaches Dispatch with the broker's address and none the command wrote.
var dpopDroppedHeaders = []string{"X-Real-IP", "Via"}

// upgrading reports whether a request asks to leave HTTP behind: a Connection header naming the
// Upgrade token and an Upgrade header naming a protocol, which is how net/http and
// httputil.ReverseProxy themselves read one.
func upgrading(r *http.Request) bool {
	return httpguts.HeaderValuesContainsToken(r.Header["Connection"], "Upgrade") && r.Header.Get("Upgrade") != ""
}

// joinMessage is the message of the line the close of a joined connection logs. The call itself
// logs its own line when the join ends (callMessage), so this one is no second call line: it
// says why the join ended rather than how the call did.
const joinMessage = "airlock proxy: a joined connection its grant no longer carries"

// cutMessage is the message of the line the end of any other carried call its grant stopped
// carrying logs: a streamed answer, a long poll, a gRPC call whose answers stream. It says why the
// call was ended, as joinMessage does for a joined connection. A call cut before its service
// answered is refused after it (serveBound's ErrorHandler), and its refusal record is its call's
// one line; one cut while its answer streams logs this line alone, since ReverseProxy aborts the
// handler that was copying it.
const cutMessage = "airlock proxy: a carried call its grant no longer carries"

// cutCall is why the grant behind a carried call stopped carrying it: the status, the code and
// the words every later call of that grant is refused with. The watcher cancels the call's
// context with it, which is how serveBound's ErrorHandler tells a call the watcher ended from a
// service that could not be reached.
type cutCall struct {
	status int
	code   string
	why    string
}

func (c *cutCall) Error() string { return c.why }

// watchCarried ends a carried call once the grant behind it stops carrying it, and it is the
// whole of what holds such a call to its grant once it has started: no request is read on it
// again, and the idle limit bounds nothing on a call whose service keeps sending. So the read
// every request makes for itself runs here instead, on the watch interval, until the call's own
// context ends, which ServeHTTP's return does. A call shorter than one interval is read no
// second time. message is the line the end logs and end is how the call is ended: the cancel of
// its context, or the cut of a joined connection.
func (s *Server) watchCarried(ctx context.Context, info connInfo, r *http.Request, chosen pair, id, message string, end func(*cutCall)) {
	ticker := time.NewTicker(s.watch)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		why := s.carryRefusal(ctx, info, chosen)
		if why == nil {
			continue
		}
		s.log.Info(message, s.callFields(id, info.callFor(chosen), r, why.status, why.why)...)
		// The end stops the call in both directions, so the ServeHTTP this watcher was
		// started for returns, and its cancel would stop this goroutine.
		end(why)
		return
	}
}

// cutJoined stops the bytes of a joined connection and then closes it. The stop comes first
// because the connection is a *tls.Conn, and tls.Conn.Close writes close_notify, which the
// command reads as the end of the stream, and closes the socket under it only after that
// write, which crypto/tls gives five seconds of its own: in that window the copy from the
// command is still running on an open socket, so a command that answers the end of the stream
// with one more frame would have that frame carried to the service after the airlock decided
// the grant no longer carries this connection.
//
// What shuts that window is the connection under the TLS, which carries no byte it reads once
// the cut is decided (cutConn, where why a read deadline does not decide it is written out).
// The flag goes first because it is the decision; the deadline follows, since it is what wakes
// a read parked in the poller; and the close follows that, still writing close_notify, so the
// command reads a clean end of stream, a read deadline bounding no write.
func cutJoined(cut *cutConn, c net.Conn) {
	cut.stopped.Store(true)
	_ = c.SetReadDeadline(time.Now())
	_ = c.Close()
}

// carryRefusal is why the grant behind a carried call no longer carries it, and nil while it
// does: the grant's own expiry, the connection read's refusals, the person the grant's session
// acts for held by Dispatch, the name the call was decided for going out of the grant, which is
// how a name an operator withholds reaches a call that reads no request again, and the slot it
// was decided for no longer released (slotReleased), the rule a new call of that slot is refused
// by - a github-app installation gone since the release. The second read is the one every other
// request makes for the values it is handled for (valuesFor): it answers from what this pod holds
// unless the live policy version has moved or the name has been reread, which is a pointer read,
// so it costs a carried call nothing on the common path and reads the grant again exactly when a
// policy change could have ended it - a retag, a raised tier, a narrowed requesters list - which
// the connection read never sees. Any error of either read that is no refusal of this grant
// leaves the call alone, with the error logged where grantRefusal logs one: a read that failed
// says nothing about the grant, and a database failover must cut no WebSocket and no stream the
// airlock carries.
//
// The hold is read as a request reads it (holdRefusal) but for two things: any answer that holds
// the person ends the call, a held answer the broker could not keep among them (hold.HeldUnkept),
// since that answer still says Dispatch holds them; and of the checks that decide nothing, one
// whose person's last not-held answer has passed its deadline (hold.DeadlinePassed) ends the call
// too, 503 HOLD_UNCHECKABLE in fixed words, since that deadline is the bound the broker serves the
// person to and a call opened before it must not outlive it. Every other check that decides
// nothing - Dispatch refusing the broker's own account, an answer the broker can neither ask for
// nor read - leaves the call to the reads after it, logged, as a read that failed does.
// The hold is asked whatever the connection read answers: of the operator that read names, and,
// when the read fails with no refusal of the grant, of the operator the call was admitted under
// (connInfo.grant). A read that fails so is the requests package's project check that cannot ask
// Dispatch, among others, and that is the outage in which only the hold's kept answer and its
// deadline still say whether the person may be served, so such a read must not skip them.
func (s *Server) carryRefusal(ctx context.Context, info connInfo, chosen pair) *cutCall {
	if expires := info.grant.Expires; expires != nil && !s.now().Before(*expires) {
		return &cutCall{http.StatusProxyAuthRequired, codeGrantNotLive, "this grant expired while the proxy carried this call"}
	}
	grant, err := s.grants.AirlockConnection(ctx, info.password)
	if err != nil {
		if cut := s.carryRead("connection", err); cut != nil {
			return cut
		}
		return s.carryHold(ctx, info.grant.Operator)
	}
	if cut := s.carryHold(ctx, grant.Operator); cut != nil {
		return cut
	}
	if _, carried := grant.Names[chosen.name]; !carried {
		return &cutCall{http.StatusForbidden, codeNoBoundName, chosen.name + " is no longer a name of this grant"}
	}
	values, err := s.valuesFor(ctx, grant)
	if err != nil {
		return s.carryRead("values", err)
	}
	secret, carried := values[chosen.name]
	if !carried {
		return &cutCall{http.StatusForbidden, codeNoValue, chosen.name + " is no longer a name of this grant"}
	}
	if !slotReleased(secret, chosen.slot) {
		return &cutCall{http.StatusForbidden, codeNoValue, noSlotValue(chosen)}
	}
	return nil
}

// carryHold is carryRefusal's hold of operator, the person a carried call's grant acts for, by the
// gate's Verdict: nil for no person (an unmapped pod's grant, a service's) and while the gate
// serves them, else the cut a held answer or a passed deadline makes, as carryRefusal says.
func (s *Server) carryHold(ctx context.Context, operator string) *cutCall {
	if operator == "" {
		return nil
	}
	switch v := s.hold.Decide(ctx, operator); v.Kind {
	case hold.Served:
		return nil
	case hold.Held, hold.HeldUnkept:
		return &cutCall{http.StatusForbidden, hold.CodeHeld, v.HeldWords()}
	case hold.DeadlinePassed:
		s.log.Error(holdUnreadable, "person", operator, "error", v.Err)
		status, code, words := v.Refusal()
		return &cutCall{status, code, words}
	default:
		// Every other check that decides nothing — Dispatch refusing the broker's own account
		// (hold.BrokerRefused), an answer the broker can neither ask for nor read (hold.Uncheckable)
		// — leaves the call to the reads after it, logged as a read that failed is.
		s.log.Error(holdUnreadable, "person", operator, "error", v.Err)
	}
	return nil
}

// slotReleased is whether the broker's read of a name still fills the slot a call is handled
// for, the one rule serveBound refuses a new call by and carryRefusal ends a carried one by: a
// slot it holds a value for, or an azure access slot, whose token no broker read holds and the
// proxy holds or exchanges for itself (azureAccess).
func slotReleased(secret requests.AirlockSecret, slot string) bool {
	_, held := secret.Slots[slot]
	return held || isAzureAccessSlot(slot)
}

// noSlotValue is the words a call is refused or ended with when the broker no longer releases the
// slot it is handled for.
func noSlotValue(chosen pair) string {
	return "the broker released no " + chosen.slot + " value for " + chosen.name
}

// carryRead is what one read of carryRefusal's answers for the call: a refusal of this grant
// ends it, in the status, code and words every other call of that grant is refused with, and
// anything else is a read that did not happen, which grantRefusal has logged and which ends
// nothing.
//
// What tells the two apart is grantRefusal's own answer and not the error, because the two do
// not always agree: a mint whose upstream could not be asked reaches here as an ErrMintFailed,
// which is a refusal of the requests package, and grantRefusal answers it 503 all the same,
// since nothing of it says the grant has ended. Reading the error here would end a WebSocket or
// a stream a github-app, google-login or azure-login name still carries every time its upstream
// has a bad minute.
func (s *Server) carryRead(read string, err error) *cutCall {
	status, code, why := s.grantRefusal(read, err)
	if status == http.StatusProxyAuthRequired || status == http.StatusForbidden {
		return &cutCall{status, code, why}
	}
	return nil
}

// callMessage is the message of the one line a call to a bound host logs that the airlock carried
// or failed. A call it refused logs the refusal's line in its place (refuse), so every such call
// still logs exactly one.
const callMessage = "airlock proxy: a call to a bound host"

// call is what is known of a call when the proxy answers it, for the answer's line: the grant
// the proxy password proved (zero until one has), the host the call was for, and the (name,
// slot) pair it was handled for (zero until one was chosen). A refusal of the outer server knows
// the first two at most; a request on a decrypted connection knows its connection's
// (connInfo.callFor).
type call struct {
	grant  requests.AirlockConnection
	host   string
	chosen pair
}

// callFor is what a request on this connection knows of its call, once chosen is.
func (info connInfo) callFor(chosen pair) call {
	return call{grant: info.grant, host: info.host, chosen: chosen}
}

// refuse answers a call the airlock will not carry, and it is the one writer of this port's
// refusals, refuseCredential's 407 among them. The answer is written here, on the connection the
// proxy opened, and nothing more of the request reaches the service: every caller but one refuses
// before it hands the request to a transport, and the one that refuses after, serveBound's
// ErrorHandler for a call its grant watcher cut, answers with Cipher-Proxy-Forwarded: unknown when the
// request may already have reached the service (ownAnswer). c is what is known of the call, code
// names the refusal, and reason is the broker's own words for it.
//
// A refusal (answer.Refused: a status from 400 to 499 but a 429) is one line under
// answer.RefusalMessage at WARN (refusalFields), and then the answer, whose text body ends with
// its answer id, which its X-Broker-Answer-Id header carries too. That line is a refused call's
// one line, written in place of callMessage's. Any other status is one its callers pass here as
// well - an exchange the token endpoint could not make, a grant read that did not happen, an
// endpoint's own status that is no refusal, a 429 among them - which keeps callMessage's line at
// INFO and writes no refusal line, as the API port writes none for a 429.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, c call, status int, code, reason string) {
	rec := answerOf(w)
	rec.note(code)
	ownAnswer(w, code)
	if !answer.Refused(status, code) {
		s.log.Info(callMessage, s.callFields(rec.ID(), c, r, status, reason)...)
		http.Error(w, "airlock: "+reason, status)
		return
	}
	s.log.Warn(answer.RefusalMessage, s.refusalFields(rec.ID(), c, r, status, code, reason)...)
	http.Error(w, "airlock: "+reason+" (answer "+rec.ID()+")", status)
}

// refusalFields is what one refusal of this port logs: the fields of the API port's refusal line
// (answer.RefusalLine), read for this port - no route, since it has no mux; caller class airlock,
// and the grant and its enrollment as the caller once the proxy password has proved them; the
// name the call was handled for as its secrets, once one was chosen, listed and counted by the API
// port's rule (answer.SecretNames) - and beside them the host, the reason and the path before the
// caller's correlation id, and the issue the grant was given for and the slot after it. Never the
// value: no field is read from a header the swap puts one in, nor from the body. The method, the
// host, the path and the correlation id (X-Correlation-Id) are the caller's own text, and a reason
// may quote the host (serveConnect's target that is no host and port, and checkedAddresses' name
// that does not resolve): RefusalLine.Fields cuts each of them, as it cuts every text field of the
// line (answer.ClipFields), and the broker writes the line through its placeholder mask
// (logmask.Writer), which writes each placeholder such text holds as logmask.Mark, since a cut is
// no redaction.
func (s *Server) refusalFields(id string, c call, r *http.Request, status int, code, reason string) []any {
	line := answer.RefusalLine{ID: id, Status: status, Code: code, Method: r.Method,
		CallerClass: answer.CallerAirlock, Caller: callerOf(c.grant), Correlation: answer.CorrelationID(r)}
	if c.chosen.name != "" {
		line.Secrets.Note(c.chosen.name)
	}
	trailing := make([]any, 0, 4)
	if c.grant.GrantID != "" {
		trailing = append(trailing, "issue", c.grant.Issue)
		if c.grant.CheckRunID != "" {
			trailing = append(trailing, "ci_check_run_id", c.grant.CheckRunID)
		}
	}
	if slot := c.chosen.namedSlot(); slot != "" {
		trailing = append(trailing, "slot", slot)
	}
	return line.Fields([]any{
		"host", c.host,
		"reason", reason,
		// The path alone, as callFields: a query can carry anything a command put in it.
		"path", r.URL.Path,
	}, trailing...)
}

// callFields is what one call to a bound host logs when it was carried or failed, and what the
// end of a carried call its grant stopped carrying logs (watchCarried): its answer id, who made
// it, which secret it was handled for, where it went, and how it ended. Never the placeholder and
// never the value - the airlock exists so that neither is ever written down, and a log line is
// the first place one would leak. The reason is there for a call that was not carried alone, so
// a line reads as allowed, or as not and why. Every text field is cut by answer.ClipFields, as
// the refusal line's are, so whatever method and path the caller sent, the line stays one Cloud
// Logging entry.
func (s *Server) callFields(id string, c call, r *http.Request, status int, reason string) []any {
	fields := []any{
		"answer_id", id,
		"enrollment", c.grant.EnrollmentID,
		"grant", c.grant.GrantID,
		"secret", c.chosen.name,
		"issue", c.grant.Issue,
		"host", c.host,
		"method", r.Method,
		// The path alone: a query can carry anything a command put in it.
		"path", r.URL.Path,
		"status", status,
		"allowed", reason == "",
	}
	if c.grant.CheckRunID != "" {
		// A CI run's result grant: the check run of the job whose write this is, which ties
		// the line to the run Dispatch stores the result for.
		fields = append(fields, "ci_check_run_id", c.grant.CheckRunID)
	}
	if slot := c.chosen.namedSlot(); slot != "" {
		fields = append(fields, "slot", slot)
	}
	// The way the name's credential went in, or would have: swap, mint, sign, exchange or dpop.
	// A line of no name (a refusal before one was chosen) names none.
	if c.chosen.way != "" {
		fields = append(fields, "way", c.chosen.way)
	}
	if reason != "" {
		fields = append(fields, "reason", reason)
	}
	return answer.ClipFields(fields)
}

// pair is one (name, slot) of a grant: which secret a request may be handled for, and which of
// that secret's values. A github-app name has one slot per installation and no value slot, so
// the pair rather than the name is what says which token a request's placeholder stands for.
// way is the binding's own way, read when the pair was made, so that the request this pair is
// chosen for needs no second Lookup of the name.
type pair struct{ name, slot, way string }

// namedSlot is the slot a line names: which of the name's values the call used, where the name
// has more than one. A github-app secret holds one token per installation, and an operator
// reading the line has to know which account's token went to the service, not only which secret
// it came from. A name whose value is the one "value" slot says nothing by naming it, so that
// slot, and none, is "".
func (p pair) namedSlot() string {
	if p.slot == requests.SlotValue {
		return ""
	}
	return p.slot
}

// candidates is every (name, slot) pair of the grant whose name's kind binds the CONNECT host on
// the port the CONNECT named, sorted by name and then by slot. Sorted because the choice below
// takes the first of them, and a map's order would make which value a request is handled for
// depend on the run.
//
// The port is read here because a kind's hosts carry no port, so a kind binds a name on 443 and
// nowhere else, and another port of that name is another service, which no name of this grant
// is a candidate for. An empty answer is therefore also what says a CONNECT is a tunnel rather
// than a connection the proxy reads (serveConnect): one function decides what is bound, and
// nothing beside it repeats that walk.
//
// It is a pure function of the rules, the grant, the host and the port, and it runs once per
// request on a bound connection, over the grant that request read for itself and the rules the
// live policy held then: the grant and a secret's proxy tag are what change between two requests
// of one connection - a name withheld, a name ended, a kind retagged - and a list built once at
// the CONNECT would answer for them as they were then.
func candidates(rules proxyrules.Rules, grant requests.AirlockConnection, host string, port uint16) []pair {
	if port != 443 {
		return nil
	}
	var pairs []pair
	for name, held := range grant.Names {
		binding := rules.Lookup(name)
		kind := rules.KindOf(binding)
		if kind == nil || !proxyrules.HostBound(kind.Hosts, host) {
			continue
		}
		for slot := range held.Slots {
			pairs = append(pairs, pair{name: name, slot: slot, way: binding.Way})
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.slot, b.slot))
	})
	return pairs
}

// choosePair is the one (name, slot) pair a request is handled for: the first candidate a header
// of the request already carries the placeholder of, and otherwise the first candidate.
// holdsPlaceholder is whether the request carries the placeholder of the pair answered, so the
// caller tells the request that asks for that pair's value from the one that only fell to it.
//
// Why one pair at all. The decision and the swap are over one grant and one secret
// (proxyrules.Grant), and the model that is proved is over that pair, so a request is decided for
// one secret or it is decided by something other than the model. Two secrets of one grant can
// be bound to one host - two GitHub tokens of different reach both bind api.github.com - and so
// can two slots of one secret, since a github-app name holds one token per installation, so
// which one a request is for cannot come from the host. What the request itself says is which
// placeholder it put in a header, and that is what is read here (proxyrules.Carries, the same
// function the model calls carries).
//
// A request that carries no placeholder of any candidate is still decided, for the first
// candidate: its kind's hosts, method and path rules and its profile's apply to it, and it is
// forwarded with nothing swapped. That is what makes an allow list a bound on the whole connection
// rather than on the requests that happen to ask for a swap.
func choosePair(candidates []pair, grant requests.AirlockConnection, request *proxyrules.Request) (chosen pair, holdsPlaceholder bool) {
	for _, candidate := range candidates {
		placeholder := grant.Names[candidate.name].Slots[candidate.slot]
		if placeholder == "" {
			continue
		}
		for _, header := range request.Headers {
			if proxyrules.Carries(placeholder, header[1]) {
				return candidate, true
			}
		}
	}
	return candidates[0], false
}

// sniOf is the name the command asked for in its TLS handshake, "" for a client that sent none.
// net/http fills r.TLS for a connection it reads as a *tls.Conn, which is every connection the
// inner server serves.
func sniOf(r *http.Request) string {
	if r.TLS == nil {
		return ""
	}
	return r.TLS.ServerName
}

// headersOf is the request's headers as the decision reads them: every header, every value, in
// one stable order - by name, and within a name in the order they arrived. Stable because the
// swap answers the same list and the headers that go upstream are rebuilt from it, and because
// two runs of one request must be decided alike.
func headersOf(h http.Header) [][2]string {
	names := slices.Sorted(maps.Keys(h))
	headers := make([][2]string, 0, len(h))
	for _, name := range names {
		for _, value := range h[name] {
			headers = append(headers, [2]string{name, value})
		}
	}
	return headers
}

// headerOf turns the swap's answer back into headers for the request upstream. Rewrite changes
// no name and no order, so this is headersOf read backwards; the names are net/http's own
// canonical form, since that is what they came out of the request as.
func headerOf(headers [][2]string) http.Header {
	h := make(http.Header, len(headers))
	for _, header := range headers {
		h[header[0]] = append(h[header[0]], header[1])
	}
	return h
}

// statusWriter records the status the service answered, which the call's log line names. It
// unwraps, so every http.ResponseController call but a flush reaches the real writer, and the
// flush that streams each write (FlushInterval -1) reaches it through FlushError.
//
// An informational answer (1xx, but 101, which ends the exchange) is not the status the call
// ended with, and goes to the command without the two headers that say whose answer it is
// (ownAnswer): ReverseProxy copies a 1xx answer's headers to the command as the service sent
// them, and ModifyResponse, which sets Cipher-Proxy-Forwarded and removes the service's Cipher-Proxy-Code,
// sees only the final answer, so a service could otherwise pass for the proxy in a 1xx.
type statusWriter struct {
	http.ResponseWriter
	status int
	// whole is whether the service's answer has no body (ModifyResponse). It is read by the
	// flush ReverseProxy's own timer runs, on a goroutine of its own.
	whole atomic.Bool
}

// FlushError flushes what has been written, except for an answer with no body, which net/http
// writes whole once the call returns. ReverseProxy flushes the header of every answer it
// streams on a timer of its own, which races the end of an answer with no body: when the timer
// wins, the header goes out alone and the end of the stream after it. A gRPC service answers a
// call that failed with such an answer ("Trailers-Only"), the status in that one header, and a
// gRPC client reads a status that arrives in a header the stream does not end with as no status
// at all: the call fails with "server closed the stream without sending trailers" and not with
// the service's own status.
func (w *statusWriter) FlushError() error {
	if w.whole.Load() {
		return nil
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.Header().Del(forwardedHeader)
		w.Header().Del(codeHeader)
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
