//go:build capture

// The program that captures the two STREAM.INFO answers TestStoredKeysTakesThePageOfTheLeaderAlone
// reads, run by hand, and only when the module's nats-server changes, from packages/envoy:
//
//	go test -tags capture -run '^TestCaptureStreamInfoAnswers$' ./internal/bus/
//
// The lint job holds every JSON file of the repository to Biome's layout
// (.github/scripts/check-biome-tree.sh), and the server sends an answer on one line, so the two
// files are laid out before they are checked in, from the repository's root after `bun install`:
//
//	bunx biome format --write packages/envoy/internal/bus/testdata/stream-info-leader.json packages/envoy/internal/bus/testdata/stream-info-member.json
//
// which changes their whitespace and nothing a decode reads.

package bus

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestCaptureStreamInfoAnswers writes testdata/stream-info-leader.json and
// testdata/stream-info-member.json: the answers a three-member cluster of the module's nats-server
// gives StoredKeys' request (subjectsRequest) for every key of a bucket of three copies holding
// captureKeys. StoredKeys takes a page from the stream's leader alone (current), and which copy
// answered is read from fields the server fills in (`cluster.leader`, `cluster.replicas`), so the
// rule is read on what the server sends rather than on what a scripted answer says it sends.
//
// The first answer is the leader's: with all three members running, the request is sent through a
// member that is not the stream's leader, and the leader answers it, since only the stream's leader
// answers a STREAM.INFO while the stream has one (nats-server v2.15.1 jetstream_api.go:2103-2104).
// The second is that member's own, once the two other members have stopped: the member left has no
// leader, and answers from its own copy, naming none (jetstream_api.go:2100-2104,
// jetstream_cluster.go:14075, 14081-14083). Every copy has applied every write first
// (Cluster.AwaitReplicated), so that copy is as whole as the leader's, and what refuses its page is
// the leader rule alone. The non-leader stops first, so the member left cannot be elected with it
// once the leader stops.
//
// Until the member left sees that it has lost its leader it answers nothing, or answers that the
// cluster is not available (jetstream_api.go:2104-2133), and it cannot be asked when: a request it
// does not answer draws nothing at all. So the request is sent again at every ask of a wait
// (testwait.Until), with no bound of its own, and every answer that has come by then is read. Each
// answer is written as the server sent it, and the two files are written only once both are taken.
func TestCaptureStreamInfoAnswers(t *testing.T) {
	t.Parallel()
	cluster := testnats.StartCluster(t, 3)
	cluster.AwaitReady(t)
	ctx := testwait.Context(t)
	client, err := Connect(cluster.URLs())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := EnsureKeyValue(ctx, client.Conn, js, &nats.KeyValueConfig{Bucket: captureBucket, Replicas: 3, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	for _, key := range captureKeys {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	stream := "KV_" + captureBucket
	cluster.AwaitReplicated(t, stream)
	info, err := js.StreamInfo(stream)
	if err != nil || info.Cluster == nil || cluster.Member(info.Cluster.Leader) < 0 {
		t.Fatalf("read the bucket's stream for its leader: %+v, %v", info, err)
	}
	member := (cluster.Member(info.Cluster.Leader) + 1) % len(cluster.Servers)
	// The connection to the member is made while every member runs: a connection's open waits for
	// JetStream to answer an account info (testnats.Connect), which a member with no leader of the
	// metadata group never does.
	memberConn, _ := memberJetStream(t, cluster, member)
	subject, request, err := subjectsRequest(captureBucket, ">", 0)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	want := make([]string, len(captureKeys))
	for i, key := range captureKeys {
		want[i] = subjectsPrefix(captureBucket) + key
	}
	whole := func(page subjectsPage) error {
		if subjects := slices.Sorted(maps.Keys(page.State.Subjects)); !slices.Equal(subjects, want) {
			return fmt.Errorf("the answer lists %v, want %v", subjects, want)
		}
		return nil
	}

	leaderAnswer, leaderPage := answerTo(t, memberConn, subject, request,
		fmt.Sprintf("the leader of %s to answer a STREAM.INFO sent through member %s", stream, cluster.Name(member)),
		func(page subjectsPage) error {
			if err := current(page, info.State.LastSeq); err != nil {
				return err
			}
			if page.Cluster.Leader == cluster.Name(member) {
				return fmt.Errorf("member %s, the one asked through, answered as the leader", cluster.Name(member))
			}
			return whole(page)
		})
	leader := cluster.Member(leaderPage.Cluster.Leader)
	if leader < 0 {
		t.Fatalf("the leader's answer names %q, no member of the cluster", leaderPage.Cluster.Leader)
	}
	for i := range cluster.Servers {
		if i != member && i != leader {
			cluster.Servers[i].Stop(t)
		}
	}
	cluster.Servers[leader].Stop(t)

	memberAnswer, _ := answerTo(t, memberConn, subject, request,
		fmt.Sprintf("member %s, the one left of three, to answer a STREAM.INFO from its own copy naming no leader", cluster.Name(member)),
		func(page subjectsPage) error {
			if page.Cluster == nil || page.Cluster.Leader != "" {
				return fmt.Errorf("the answer names cluster %+v, want one naming no leader", page.Cluster)
			}
			if page.State.LastSeq != leaderPage.State.LastSeq {
				return fmt.Errorf("the answer's copy has stored through %d, the leader's through %d", page.State.LastSeq, leaderPage.State.LastSeq)
			}
			return whole(page)
		})

	for name, answer := range map[string][]byte{"stream-info-leader.json": leaderAnswer, "stream-info-member.json": memberAnswer} {
		if err := os.WriteFile(filepath.Join("testdata", name), answer, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		t.Logf("wrote testdata/%s: %s", name, answer)
	}
}

// capturedReading is what answerTo read last: the answer, or the send's error, and why it was not
// taken.
type capturedReading struct {
	Answer string
	Why    string
}

// answerTo sends request to subject on conn at every ask of a wait (testwait.Until) and returns the
// first answer, with its page, that accept takes (accept answers nil). An answer that does not
// decode as a page with its state (decodeSubjectsPage), or that accept refuses, is passed over, and
// the wait's failure prints the last one and why. The answers come to one subscription of their
// own, read without waiting at each ask, so a request the server leaves unanswered holds nothing
// up.
func answerTo(t *testing.T, conn *nats.Conn, subject string, request []byte, what string, accept func(subjectsPage) error) ([]byte, subjectsPage) {
	t.Helper()
	answers := make(chan *nats.Msg, 1024)
	sub, err := conn.ChanSubscribe(nats.NewInbox(), answers)
	if err != nil {
		t.Fatalf("subscribe to the answers: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	var data []byte
	var page subjectsPage
	last := capturedReading{Why: "no answer has come"}
	testwait.Until(t, what, func() (capturedReading, bool) {
		if err := conn.PublishRequest(subject, sub.Subject, request); err != nil {
			last = capturedReading{Why: fmt.Sprintf("send: %v", err)}
			return last, false
		}
		for {
			select {
			case answer := <-answers:
				decoded, err := decodeSubjectsPage(answer.Data)
				if err == nil {
					err = accept(decoded)
				}
				if err == nil {
					data, page = answer.Data, decoded
					return capturedReading{Answer: string(answer.Data)}, true
				}
				last = capturedReading{Answer: string(answer.Data), Why: err.Error()}
			default:
				return last, false
			}
		}
	})
	return data, page
}
