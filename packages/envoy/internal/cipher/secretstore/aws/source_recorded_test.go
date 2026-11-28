package aws_test

// This file holds the source of each secret the AWS store lists, and the source the policy serves
// it by, to what main answered before the store packages moved. Postgres holds a grant's source
// (request_secrets.source), so a source whose format changes ends every live grant of an AWS
// deployment that upgrades. testdata/source-recorded-on-main-aws.json was written at main
// f57cfa7e2ffe by internal/cipher/secrets/source_recorded_test.go and export_test.go as they were
// at 414d2709f4b4, the package the store was in, in a worktree of that commit, from the root of the
// checkout that holds this file:
//
//	git worktree add --detach <tmp>/main f57cfa7e2ffe
//	git show 414d2709f4b4:packages/envoy/internal/cipher/secrets/source_recorded_test.go > <tmp>/main/packages/envoy/internal/cipher/secrets/source_recorded_test.go
//	git show 414d2709f4b4:packages/envoy/internal/cipher/secrets/export_test.go > <tmp>/main/packages/envoy/internal/cipher/secrets/export_test.go
//	go test -C <tmp>/main/packages/envoy ./internal/cipher/secrets/ -run TestSourceFormatRecordedOnMain -args -record
//	cp <tmp>/main/packages/envoy/internal/cipher/secrets/testdata/source-recorded-on-main-aws.json packages/envoy/internal/cipher/secretstore/aws/testdata/
//	git worktree remove --force <tmp>/main
//
// This file writes no record: a record written by the code it checks compares that code with
// itself and proves nothing. A change to main
// that adds, drops or changes a source fails this test in its own pull request: that change edits
// the record there, in the form -record wrote it (json.MarshalIndent with two-space indents and a
// closing newline), which the test holds the file to, and names itself here with the lines of the
// record that moved. A moved source ends the live grants that hold it, so that pull request's
// review decides whether it may move. The sources carry the region "local" and the account
// 000000000000 because the AWS fake (awstest.Local) answers its ARNs so; a real store answers its
// own region and account, and each secret's own name suffix, which realARNs gives the fake, in a
// form of its own (awsNameSuffix).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// recordedSource is one secret's sources as a record holds them: the source its store's listing
// answered, and the one the policy serves it by, which a grant stores.
type recordedSource struct {
	Listed string `json:"listed"`
	Source string `json:"source"`
}

// recordedSecrets are the two secrets the store holds under its prefix, on the agent-secrets key:
// a stored secret, and one whose type tag makes the policy serve it by a type source. The policy
// loads a google-login secret with no profile, so it is the typed one.
var recordedSecrets = []struct {
	slug  string
	tags  map[string]string
	value string
}{
	{"deel-api-key", map[string]string{"owner": "ada@example.com", "tier": "agent"}, "deel-value"},
	{"google-login", map[string]string{"owner": "ada@example.com", "tier": "human", "type": "google-login"},
		`{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`},
}

// TestSourceFormatRecordedOnMain: the AWS store lists each secret with the source main listed it
// with, and the policy serves it by the source main served it by, both on a load (List, Load) and
// on a reread of the one secret (Describe, LoadOne), which is how the broker serves a secret after
// a reread and so the source a grant made then stores.
func TestSourceFormatRecordedOnMain(t *testing.T) {
	for _, row := range []struct {
		store  string
		prefix string
		open   func(t *testing.T, prefix string) secretstore.Store
		// form is what every source the record holds for this store must match, so a record
		// taken against a fake that answers a shape no real store answers is refused here
		// rather than freezing that shape for the moves to be held to.
		form *regexp.Regexp
	}{
		{"aws", "example/agent-secrets/", func(t *testing.T, prefix string) secretstore.Store {
			local := awstest.NewLocal()
			for _, s := range recordedSecrets {
				local.Put(awstest.LocalSecret{Name: prefix + s.slug, KmsKeyID: awstest.KeyARN, Tags: s.tags, Value: s.value})
			}
			return aws.Store{Client: realARNs{local}, KMS: local, KeyARN: awstest.KeyARN}
		}, regexp.MustCompile(`-[a-z0-9]{6}$`)},
	} {
		t.Run(row.store, func(t *testing.T) {
			record := filepath.Join("testdata", "source-recorded-on-main-"+row.store+".json")
			store := row.open(t, row.prefix)
			ctx := context.Background()
			listings, err := store.List(ctx, row.prefix, secretstore.ReadOptions{})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			loader := policy.Loader{Store: store, Prefix: row.prefix}
			set, err := loader.Load(ctx)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, reread := map[string]recordedSource{}, map[string]recordedSource{}
			for _, ls := range listings {
				slug, ok := strings.CutPrefix(ls.ID, row.prefix)
				if !ok {
					t.Fatalf("List answered %s, which is not under the prefix %s", ls.ID, row.prefix)
				}
				name := policy.SlugToName(slug)
				secret, served := set.Secrets[name]
				if !served {
					t.Fatalf("%s is not served (refused: %+v); want each recorded secret served", name, set.Refused)
				}
				got[name] = recordedSource{Listed: ls.Source, Source: secret.Source}
				described, err := store.Describe(ctx, ls.ID, secretstore.ReadOptions{})
				if err != nil {
					t.Fatalf("Describe %s: %v", ls.ID, err)
				}
				lookup, err := loader.LoadOne(ctx, name)
				if err != nil {
					t.Fatalf("LoadOne %s: %v", name, err)
				}
				if !lookup.Served {
					t.Fatalf("%s is not served on a reread (reason %s); want each recorded secret served", name, lookup.Reason)
				}
				reread[name] = recordedSource{Listed: described.Source, Source: lookup.Secret.Source}
			}
			if len(got) != len(recordedSecrets) {
				t.Fatalf("List answered %d secrets, want the %d this test holds: %+v", len(got), len(recordedSecrets), listings)
			}
			raw, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("read %s: %v", record, err)
			}
			var want map[string]recordedSource
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("decode %s: %v", record, err)
			}
			if len(want) == 0 {
				t.Fatalf("UNKNOWN: %s records no secret, so the check compares nothing", record)
			}
			if again, err := json.MarshalIndent(want, "", "  "); err != nil || !bytes.Equal(append(again, '\n'), raw) {
				t.Fatalf("%s holds an entry or a form -record did not write (%s), which the comparison would not read; write it as -record did", record, recordForm(raw, "secret"))
			}
			for _, name := range slices.Sorted(maps.Keys(want)) {
				for _, held := range []struct{ what, source string }{
					{"listed source", want[name].Listed},
					{"served source", want[name].Source},
				} {
					if !row.form.MatchString(held.source) {
						t.Errorf("%s: the recorded %s %q is not of the form a real %s store answers (%s)", name, held.what, held.source, row.store, row.form)
					}
				}
			}
			compareSources(t, "List", "Load", got, want)
			compareSources(t, "Describe, a reread", "LoadOne, a reread", reread, want)
		})
	}
}

// compareSources fails t for each secret whose sources in got differ from the record want: the
// secret, which source, the reads that answered it (listedBy, servedBy), and both values, with
// "absent" for a secret the store answered and the record does not hold, or the reverse.
func compareSources(t *testing.T, listedBy, servedBy string, got, want map[string]recordedSource) {
	t.Helper()
	var names []string
	for name := range got {
		names = append(names, name)
	}
	for name := range want {
		if _, both := got[name]; !both {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		g, answered := got[name]
		w, recorded := want[name]
		if answered != recorded || g.Listed != w.Listed {
			t.Errorf("%s: listed source (%s) %s, recorded on main %s", name, listedBy, sourceText(g.Listed, answered), sourceText(w.Listed, recorded))
		}
		if answered != recorded || g.Source != w.Source {
			t.Errorf("%s: source (%s) %s, recorded on main %s", name, servedBy, sourceText(g.Source, answered), sourceText(w.Source, recorded))
		}
	}
}

// sourceText is a source as a failure prints it: quoted, or "absent" for a secret that side does
// not hold, which an empty source would otherwise print the same as.
func sourceText(source string, present bool) string {
	if !present {
		return "absent"
	}
	return strconv.Quote(source)
}

// recordForm names what makes a record other than what -record wrote: each top-level key it holds
// more than once, of which decoding keeps the last, or else a form written by hand.
func recordForm(raw []byte, entry string) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return err.Error()
	}
	seen, repeated := map[string]bool{}, []string{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err.Error()
		}
		if name, _ := key.(string); seen[name] {
			repeated = append(repeated, fmt.Sprintf("the %s %q more than once", entry, name))
		} else {
			seen[name] = true
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err.Error()
		}
	}
	if len(repeated) == 0 {
		return "an entry or a layout written by hand"
	}
	return strings.Join(repeated, ", ")
}

// awsNameSuffix is the hyphen and six characters Secrets Manager puts at the end of a secret's
// ARN, which it picks when it creates the secret. This one is read from the secret's name, so each
// run gives a name the same six characters and the record holds one text; a real store's six say
// nothing, and are no part of what this record freezes. Its alphabet is the lowercase half of the
// characters Secrets Manager picks from.
func awsNameSuffix(name string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	sum := sha256.Sum256([]byte(name))
	suffix := make([]byte, 6)
	for i := range suffix {
		suffix[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return "-" + string(suffix)
}

// realARNs is the AWS fake answering the ARNs a real Secrets Manager answers: awstest.ARN ends an
// ARN at the secret's name, where Secrets Manager ends it with that name's own suffix
// (awsNameSuffix), so a record taken from the bare fake would hold a source no deployment has, and
// a move that rebuilt an ARN from the name, or trimmed its suffix, would pass against it. Every
// ARN it answers carries the suffix, and every call it takes one from reads it back.
type realARNs struct{ aws.SecretsManagerAPI }

// suffixed is an ARN of the fake's form with its secret's suffix on the end; anything else, a
// secret's bare name among them, is left as it is.
func suffixed(id, name string) string {
	if !strings.HasPrefix(id, awstest.ARN("")) {
		return id
	}
	return id + awsNameSuffix(name)
}

// unsuffixed is id with that suffix taken off again, for the fake, which knows none. An id that is
// no ARN of this form, or whose last seven bytes are not the suffix of the name before them, is
// left as it is, so the fake answers it as it answers any other id.
func unsuffixed(id string) string {
	prefix := awstest.ARN("")
	if !strings.HasPrefix(id, prefix) {
		return id
	}
	name := strings.TrimPrefix(id, prefix)
	if len(name) < 7 || awsNameSuffix(name[:len(name)-7]) != name[len(name)-7:] {
		return id
	}
	return prefix + name[:len(name)-7]
}

func (r realARNs) ListSecrets(ctx context.Context, in *secretsmanager.ListSecretsInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	out, err := r.SecretsManagerAPI.ListSecrets(ctx, in, opts...)
	if err != nil {
		return out, err
	}
	for i, entry := range out.SecretList {
		out.SecretList[i].ARN = sdkaws.String(suffixed(sdkaws.ToString(entry.ARN), sdkaws.ToString(entry.Name)))
	}
	return out, nil
}

func (r realARNs) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	asked := *in
	asked.SecretId = sdkaws.String(unsuffixed(sdkaws.ToString(in.SecretId)))
	out, err := r.SecretsManagerAPI.DescribeSecret(ctx, &asked, opts...)
	if err != nil {
		return out, err
	}
	out.ARN = sdkaws.String(suffixed(sdkaws.ToString(out.ARN), sdkaws.ToString(out.Name)))
	return out, nil
}

func (r realARNs) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	asked := *in
	asked.SecretId = sdkaws.String(unsuffixed(sdkaws.ToString(in.SecretId)))
	out, err := r.SecretsManagerAPI.GetSecretValue(ctx, &asked, opts...)
	if err != nil {
		return out, err
	}
	out.ARN = sdkaws.String(suffixed(sdkaws.ToString(out.ARN), sdkaws.ToString(out.Name)))
	return out, nil
}
