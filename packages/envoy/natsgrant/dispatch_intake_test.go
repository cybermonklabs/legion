package natsgrant_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/natsgrant"
)

// allows reports whether p's publish allow list admits subject, as nats-server matches a literal
// subject against an entry: token for token, where * is any one token and a last > is one or more.
func allows(p natsgrant.Permissions, subject string) bool {
	tokens := strings.Split(subject, ".")
	return slices.ContainsFunc(p.Publish, func(entry string) bool {
		pattern := strings.Split(entry, ".")
		for i, token := range pattern {
			switch {
			case token == ">":
				return i < len(tokens)
			case i >= len(tokens):
				return false
			case token != "*" && token != tokens[i]:
				return false
			}
		}
		return len(pattern) == len(tokens)
	})
}

const (
	exampleMachine = "example-dispatch"
	ownGen         = "GenNowA1b2C3d4E5f6G7h8"
	oldGen         = "GenOldA1b2C3d4E5f6G7h8"
	otherGen       = "GenElseA1b2C3d4E5f6G7h"
)

func consumerCall(call, name string) string {
	return "$JS.API.CONSUMER." + call + "." + bus.Stream + "." + name
}

func consumerCreate(name string) string {
	return consumerCall("CREATE", name) + ".notifications.github.example"
}

func consumerAck(name string) string {
	return "$JS.ACK." + bus.Stream + "." + name + ".1.7.7.1700000000000000000.0"
}

// A Dispatch connection's name is read as the intake it asks for only in the three shapes Dispatch
// names its own connections, for one of Dispatch's two durables and generation tokens of letters
// and digits alone. Any other name, Dispatch's fixed-mode intakes' and its publisher's included,
// asks for nothing, so a name can never widen the grant past Dispatch's own two durables.
func TestADispatchConnectionNameIsReadAsTheIntakeItNames(t *testing.T) {
	for name, want := range map[string]natsgrant.DispatchIntake{
		natsgrant.DispatchIntakeConnection(bus.DispatchDeliveryDurable, ownGen, ""):      {Logical: bus.DispatchDeliveryDurable, Generation: ownGen},
		natsgrant.DispatchIntakeConnection(bus.DispatchJobDurable, ownGen, oldGen):       {Logical: bus.DispatchJobDurable, Generation: ownGen, Old: oldGen},
		natsgrant.DispatchCleanupConnection(bus.DispatchDeliveryDurable, oldGen):         {Logical: bus.DispatchDeliveryDurable, Generation: oldGen, Cleanup: true},
		"dispatch-intake/delivery-events/" + ownGen:                                      {Logical: bus.DispatchDeliveryDurable, Generation: ownGen},
		"dispatch-intake/delivery-jobs/" + ownGen + "/" + oldGen:                         {Logical: bus.DispatchJobDurable, Generation: ownGen, Old: oldGen},
		"dispatch-cleanup/delivery-jobs/" + oldGen:                                       {Logical: bus.DispatchJobDurable, Generation: oldGen, Cleanup: true},
		"dispatch-intake":                                                                {},
		"dispatch-job-intake":                                                            {},
		"dispatch":                                                                       {},
		"":                                                                               {},
		"dispatch-intake/delivery-events":                                                {},
		"dispatch-intake/delivery-events/":                                               {},
		"dispatch-intake/delivery-events/" + ownGen + "/":                                {},
		"dispatch-intake/delivery-events/" + ownGen + "/" + oldGen + "/" + otherGen:      {},
		"dispatch-cleanup/delivery-events/" + ownGen + "/" + oldGen:                      {},
		"dispatch-intake/listener-m2/" + ownGen:                                          {},
		"dispatch-intake/legion-go-p-github/" + ownGen:                                   {},
		"dispatch-intake/s-x/" + ownGen:                                                  {},
		"dispatch-intake/delivery-events/*":                                              {},
		"dispatch-intake/delivery-events/>":                                              {},
		"dispatch-intake/delivery-events/a.b":                                            {},
		"dispatch-intake/delivery-events/a b":                                            {},
		"dispatch-intake/delivery-events/a~b":                                            {},
		"dispatch-intake/delivery-events/" + ownGen + "/" + ownGen:                       {},
		"dispatch-intake/delivery-events/" + strings.Repeat("a", 65):                     {},
		"other-intake/delivery-events/" + ownGen:                                         {},
		"dispatch-intake/delivery-events/" + ownGen + " dispatch-cleanup/delivery-jobs/": {},
		natsgrant.ListenerIntakeConnection(exampleMachine, ownGen, oldGen):              {ListenerMachine: exampleMachine, Generation: ownGen, Old: oldGen},
		"listener-intake/" + exampleMachine + "/" + ownGen:                              {ListenerMachine: exampleMachine, Generation: ownGen},
		"listener-intake//" + ownGen:                                                     {},
		"listener-intake/" + exampleMachine + "/" + ownGen + "/" + ownGen:                {},
		"listener-intake/" + exampleMachine + "/*":                                       {},
		"listener-intake/" + exampleMachine + "/" + ownGen + "/" + oldGen + "/" + otherGen: {},
		"listener-cleanup/" + exampleMachine + "/" + ownGen:                              {},
	} {
		require.Equal(t, want, natsgrant.ParseDispatchConnection(name), "connection name %q", name)
	}
}

// The grant rows of an intake connection named for one generation and the one it replaces: it
// creates, reads and acknowledges its own generation and reads and deletes the old one, and
// nothing else of any consumer the fixed grant does not already reach. Its own listener durable is
// still its own, and the fixed names stay until their rights are revoked separately.
func TestAnIntakeConnectionIsGrantedItsOwnGenerationsExactly(t *testing.T) {
	logical := bus.DispatchDeliveryDurable
	own, old, other := logical+"~"+ownGen, logical+"~"+oldGen, logical+"~"+otherGen
	grant := natsgrant.Dispatch(exampleMachine, natsgrant.ParseDispatchConnection(natsgrant.DispatchIntakeConnection(logical, ownGen, oldGen)))
	for subject, want := range map[string]bool{
		consumerCreate(own):                       true,
		consumerCall("INFO", own):                 true,
		consumerAck(own):                          true,
		consumerCall("INFO", old):                 true,
		consumerCall("DELETE", old):               true,
		consumerCall("DELETE", own):               false,
		consumerCreate(old):                       false,
		consumerAck(old):                          false,
		consumerCreate(other):                     false,
		consumerCall("INFO", other):               false,
		consumerCall("DELETE", other):             false,
		consumerAck(other):                        false,
		consumerCreate("s-x"):                     false,
		consumerCall("INFO", "s-x"):               false,
		consumerCall("DELETE", "s-x"):             false,
		consumerCreate("listener-m2"):             false,
		consumerCall("INFO", "listener-m2"):       false,
		consumerCreate("legion-go-p-github"):      false,
		consumerCall("INFO", "legion-go-p-github"): false,
		consumerAck("legion-go-p-github"):         false,
		consumerCall("INFO", bus.ListenerDurable(exampleMachine)):                       true,
		consumerCall("CREATE", bus.ListenerDurable(exampleMachine)) + ".notifications.>": false,
		consumerCall("CREATE", bus.ListenerDurable(exampleMachine)):                     true,
		consumerCreate(bus.DispatchJobDurable + "~" + ownGen):                           false,
		consumerCreate(logical):                   true,
		consumerCall("DELETE", logical):           true,
	} {
		require.Equal(t, want, allows(grant, subject), "%s for an intake of %s over %s", subject, ownGen, oldGen)
	}
	// Control: the same rows under Dispatch's fixed grant, with no intake, refuse every fresh name.
	fixed := natsgrant.Dispatch(exampleMachine, natsgrant.DispatchIntake{})
	for _, subject := range []string{consumerCreate(own), consumerCall("INFO", own), consumerAck(own), consumerCall("DELETE", old)} {
		require.False(t, allows(fixed, subject), "the fixed grant admits %s", subject)
	}
	require.Equal(t, fixed.Publish, grant.Publish[:len(fixed.Publish)], "an intake's grant is the fixed grant followed by its own entries")
	require.Equal(t, fixed.Subscribe, grant.Subscribe)
}

// A cleanup connection reads and deletes its one generation and nothing more of the fresh names:
// it creates nothing and acknowledges nothing.
func TestACleanupConnectionReadsAndDeletesItsOneGeneration(t *testing.T) {
	logical := bus.DispatchJobDurable
	gone, other := logical+"~"+oldGen, logical+"~"+otherGen
	grant := natsgrant.Dispatch(exampleMachine, natsgrant.ParseDispatchConnection(natsgrant.DispatchCleanupConnection(logical, oldGen)))
	for subject, want := range map[string]bool{
		consumerCall("INFO", gone):    true,
		consumerCall("DELETE", gone):  true,
		consumerCreate(gone):          false,
		consumerAck(gone):             false,
		consumerCall("INFO", other):   false,
		consumerCall("DELETE", other): false,
		consumerCall("DELETE", bus.DispatchDeliveryDurable+"~"+oldGen): false,
	} {
		require.Equal(t, want, allows(grant, subject), "%s for a cleanup of %s", subject, oldGen)
	}
}

// selectionKey is the selection bucket's subject of the listener durable of machine id.
func selectionKey(machineID string) string {
	return "$KV." + consumernames.SessionBucket + "." + consumernames.DurableKey(bus.ListenerDurable(machineID))
}

// The listener beside Dispatch shares Dispatch's grant, so that grant carries the one selection
// record that listener keeps: the write of its own durable's key, for the machine the auth service
// granted (never one a connection name says), and the bucket's leader-answered get it reads that
// key and the sentinel with. Another machine's key, a session's key, the sentinel's write and the
// whole bucket are refused, whatever the connection is named. The control is the bucket's
// wildcard: a grant holding it admits another machine's key, so the refusal rows can fail.
func TestDispatchsListenerWritesOnlyItsOwnSelectionKey(t *testing.T) {
	stream := "KV_" + consumernames.SessionBucket
	for _, name := range []string{"dispatch", natsgrant.DispatchIntakeConnection(bus.DispatchDeliveryDurable, ownGen, oldGen), "listener-intake/m2/" + ownGen} {
		grant := natsgrant.Dispatch(exampleMachine, natsgrant.ParseDispatchConnection(name))
		for subject, want := range map[string]bool{
			selectionKey(exampleMachine):                              true,
			"$JS.API.STREAM.MSG.GET." + stream:                        true,
			selectionKey("m2"):                                        false,
			"$KV." + consumernames.SessionBucket + ".s-x":             false,
			"$KV." + consumernames.SessionBucket + "." + consumernames.SentinelKey: false,
			"$KV." + consumernames.SessionBucket + ".durable.other":   false,
			"$JS.API.STREAM.CREATE." + stream:                         false,
			"$JS.API.STREAM.UPDATE." + stream:                         false,
			"$JS.API.STREAM.PURGE." + stream:                          false,
			"$JS.API.STREAM.DELETE." + stream:                         false,
			"$JS.API.STREAM.MSG.DELETE." + stream:                     false,
		} {
			require.Equal(t, want, allows(grant, subject), "%s for the connection %q", subject, name)
		}
		for _, entry := range grant.Publish {
			require.False(t, strings.HasPrefix(entry, "$KV."+consumernames.SessionBucket+".") && strings.ContainsAny(entry, "*>"),
				"the connection %q holds the bucket wildcard %s", name, entry)
		}
	}
	wildcard := natsgrant.Dispatch(exampleMachine, natsgrant.DispatchIntake{})
	wildcard.Publish = append(wildcard.Publish, "$KV."+consumernames.SessionBucket+".>")
	require.True(t, allows(wildcard, selectionKey("m2")), "control: the bucket's wildcard admits another machine's key")
}

// A listener intake connection, listener-intake/<machine>/<generation>[/<old generation>], is
// granted the consumer calls of its own listener durable's fresh generation, as the listener's
// fixed durable is granted today (info, create, acknowledgements: a push consumer), and the info
// and delete of the generation it replaces; nothing of another generation, another machine's
// listener durable or a session's consumer. The machine is the auth service's (the grant's
// machine id): a name naming another machine is granted no generation at all.
func TestAListenerIntakeConnectionIsGrantedItsOwnGenerationsExactly(t *testing.T) {
	durable := bus.ListenerDurable(exampleMachine)
	own, old, other := durable+"~"+ownGen, durable+"~"+oldGen, durable+"~"+otherGen
	elsewhere := bus.ListenerDurable("m2") + "~" + ownGen
	grant := natsgrant.Dispatch(exampleMachine, natsgrant.ParseDispatchConnection("listener-intake/"+exampleMachine+"/"+ownGen+"/"+oldGen))
	for subject, want := range map[string]bool{
		consumerCall("CREATE", own):       true,
		consumerCall("INFO", own):         true,
		consumerAck(own):                  true,
		consumerCall("INFO", old):         true,
		consumerCall("DELETE", old):       true,
		consumerCall("DELETE", own):       false,
		consumerCall("CREATE", old):       false,
		consumerAck(old):                  false,
		consumerCall("CREATE", other):     false,
		consumerCall("INFO", other):       false,
		consumerCall("DELETE", other):     false,
		consumerCall("CREATE", elsewhere): false,
		consumerCall("INFO", elsewhere):   false,
		consumerCall("CREATE", "s-x"):     false,
		consumerCall("DELETE", "s-x"):     false,
	} {
		require.Equal(t, want, allows(grant, subject), "%s for a listener intake of %s over %s", subject, ownGen, oldGen)
	}
	// The name's machine is not the grant's: no generation of either machine.
	foreign := natsgrant.Dispatch(exampleMachine, natsgrant.ParseDispatchConnection("listener-intake/m2/"+ownGen+"/"+oldGen))
	for _, subject := range []string{consumerCall("CREATE", own), consumerCall("CREATE", elsewhere), consumerCall("DELETE", old), consumerCall("DELETE", bus.ListenerDurable("m2")+"~"+oldGen)} {
		require.False(t, allows(foreign, subject), "a listener intake named for m2 was granted %s", subject)
	}
	// Control: the fixed grant refuses the same generation, so the rows above read the name.
	require.False(t, allows(natsgrant.Dispatch(exampleMachine, natsgrant.DispatchIntake{}), consumerCall("CREATE", own)))
}

// The fixed grant, with no intake, is the set the hubs render today, entry for entry, with the
// listener's own selection key and the bucket's get after it (withListenerSelectionKey): an intake
// adds entries after those and changes none of them.
func TestTheFixedDispatchGrantIsTheOneWithNoIntake(t *testing.T) {
	var base mergeBase
	readJSON(t, "testdata/merge-base-grants.json", &base)
	want := withListenerSelectionKey(base.Dispatch[exampleMachine], exampleMachine)
	requireSet(t, want, natsgrant.Dispatch(exampleMachine, natsgrant.DispatchIntake{}), "Dispatch with no intake")
	requireSet(t, want, natsgrant.Dispatch(exampleMachine, natsgrant.ParseDispatchConnection("dispatch-intake")), "Dispatch for the fixed intake's name")
}
