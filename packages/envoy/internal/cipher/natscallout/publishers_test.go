package natscallout

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/natsgrant"
)

// calloutPublishersFile is the list legacy-subjects-gate.sh's fact (a3), the publishers, reads for
// a hub behind this auth service: each kind of BROKER_NATS_SERVICES entry, and "session" for a
// session's token, whose connections the service admits with a grant that may publish a
// notification subject. The gate counts every entry of those kinds as a publisher.
const calloutPublishersFile = "../../../scripts/lib/legacy-subjects-gate-callout-publishers.json"

// serviceKinds is every NATSServiceKind constant config declares, read from its source, so a kind
// added there is read here with no list of this test's own.
func serviceKinds(t *testing.T) []config.NATSServiceKind {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../config/nats_services.go", nil, 0)
	require.NoError(t, err)
	var kinds []config.NATSServiceKind
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value := spec.(*ast.ValueSpec)
			if kind, ok := value.Type.(*ast.Ident); !ok || kind.Name != "NATSServiceKind" {
				continue
			}
			for _, expr := range value.Values {
				literal, ok := expr.(*ast.BasicLit)
				require.True(t, ok && literal.Kind == token.STRING, "a NATSServiceKind constant of config is no string literal: %#v", expr)
				name, err := strconv.Unquote(literal.Value)
				require.NoError(t, err)
				kinds = append(kinds, config.NATSServiceKind(name))
			}
		}
	}
	return kinds
}

// admittedServiceGrant is the grant this service answers a connection of an entry of kind with,
// as decideService builds it for the widest entry of that kind (every project, or, for an edge
// listener, one project it lists), and whether it admits one at all: scopeGrant and guard refuse
// the others.
func admittedServiceGrant(kind config.NATSServiceKind) (natsgrant.Permissions, bool) {
	entry := config.NATSService{Kind: kind, MachineID: "example-machine", AllProjects: true}
	if kind == config.NATSEdgeListener {
		entry.AllProjects, entry.Projects = false, []string{"PRIV"}
	}
	grant, err := scopeGrant(entry, serviceGrant(entry))
	if err != nil || guard(grant) != nil {
		return natsgrant.Permissions{}, false
	}
	return grant, true
}

// reachesNotifications and publishesNotifications are the gate's rule
// (lib/legacy-subjects-gate-publishers.jq, reaches_notifications and can_publish_notifications): a
// publish allow entry reaches a notification subject when it is `>` or has two tokens or more, the
// first notifications or `*`; an allow list that is empty or holds a blank entry allows every
// subject; and a deny list takes a grant's notification subjects away only through one entry that
// matches them all.
func reachesNotifications(pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	tokens := strings.Split(pattern, ".")
	return pattern == ">" || (len(tokens) >= 2 && (tokens[0] == "notifications" || tokens[0] == "*"))
}

func publishesNotifications(grant natsgrant.Permissions) bool {
	open := len(grant.Publish) == 0 || slices.ContainsFunc(grant.Publish, func(entry string) bool { return strings.TrimSpace(entry) == "" })
	if !open && !slices.ContainsFunc(grant.Publish, reachesNotifications) {
		return false
	}
	return !slices.ContainsFunc(grant.PublishDeny, func(entry string) bool {
		entry = strings.TrimSpace(entry)
		return entry == ">" || entry == "*.>" || entry == "notifications.>"
	})
}

// TestTheGateCountsEveryCalloutKindWhoseGrantPublishesANotificationSubject: the gate's list is the
// kinds this service admits with a grant that may publish a notification subject, read from the
// grants themselves (serviceGrant, scopeGrant, guard and natsgrant.Session), so a kind that gains
// such a publish fails this test until the gate counts it.
func TestTheGateCountsEveryCalloutKindWhoseGrantPublishesANotificationSubject(t *testing.T) {
	kinds := serviceKinds(t)
	for _, known := range []config.NATSServiceKind{config.NATSDispatch, config.NATSListener, config.NATSDaemon, config.NATSMeasure, config.NATSEdgeListener} {
		require.Contains(t, kinds, known, "the read of config's source found the kinds %q, and not %s", kinds, known)
	}
	want := []string{}
	for _, kind := range kinds {
		if grant, admitted := admittedServiceGrant(kind); admitted && publishesNotifications(grant) {
			want = append(want, string(kind))
		}
	}
	session, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	if guard(session) == nil && publishesNotifications(session) {
		want = append(want, "session")
	}
	slices.Sort(want)

	raw, err := os.ReadFile(calloutPublishersFile)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var list struct {
		Kinds []string `json:"kinds"`
	}
	require.NoError(t, decoder.Decode(&list))
	got := slices.Clone(list.Kinds)
	slices.Sort(got)
	wantJSON, err := json.Marshal(struct {
		Kinds []string `json:"kinds"`
	}{want})
	require.NoError(t, err)
	require.Equal(t, want, got, "%s must list the kinds whose admitted grant may publish a notification subject: write %s", calloutPublishersFile, wantJSON)
}

// The rule itself, held on the shapes the gate's rows hold it on (legacy-subjects-gate.test.sh).
func TestPublishesNotificationsReadsAllowAndDenyAsTheGateDoes(t *testing.T) {
	for _, row := range []struct {
		name  string
		grant natsgrant.Permissions
		want  bool
	}{
		{"notifications.>", natsgrant.Permissions{Publish: []string{"notifications.>"}}, true},
		{"one family", natsgrant.Permissions{Publish: []string{"notifications.agent.>"}}, true},
		{"any first token", natsgrant.Permissions{Publish: []string{"*.>"}}, true},
		{"every subject", natsgrant.Permissions{Publish: []string{">"}}, true},
		{"no allow list", natsgrant.Permissions{}, true},
		{"a blank entry", natsgrant.Permissions{Publish: []string{" "}}, true},
		{"a deny of one family", natsgrant.Permissions{Publish: []string{">"}, PublishDeny: []string{"notifications.agent.>"}}, true},
		{"a deny of every notification subject", natsgrant.Permissions{Publish: []string{">"}, PublishDeny: []string{"notifications.>"}}, false},
		{"one token", natsgrant.Permissions{Publish: []string{"notifications"}}, false},
		{"another family", natsgrant.Permissions{Publish: []string{"$JS.API.>", "_INBOX.>", "agentstream.*.frames"}}, false},
	} {
		require.Equal(t, row.want, publishesNotifications(row.grant), row.name)
	}
}
