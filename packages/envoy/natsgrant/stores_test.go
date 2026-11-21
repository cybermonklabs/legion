package natsgrant_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/natsgrant"
)

// jetStreamEntries is each entry of p, publish then subscribe, that can match a JetStream API,
// acknowledgement or flow-control subject or a KV bucket's key: one that collides with $JS.> or
// $KV.>, wildcards included.
func jetStreamEntries(p natsgrant.Permissions) []string {
	var entries []string
	for _, entry := range slices.Concat(p.Publish, p.Subscribe) {
		subject := strings.Fields(entry)[0]
		if server.SubjectsCollide(subject, "$JS.>") || server.SubjectsCollide(subject, "$KV.>") {
			entries = append(entries, entry)
		}
	}
	return entries
}

// A session holds no JetStream or KV subject, so it neither reads nor writes a store and the server
// answers nothing it asks into one: a session that pulls its own consumer was measured to overwrite
// a bucket's record with the pull's status answer. The control is that grant, Session plus its
// consumer's pull, which the check names.
func TestNoSessionHoldsAJetStreamSubject(t *testing.T) {
	session, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	for name, grant := range map[string]natsgrant.Permissions{"Session(ses_a)": session, "SessionPattern()": natsgrant.SessionPattern()} {
		require.Empty(t, jetStreamEntries(grant), "%s holds a JetStream or KV subject", name)
	}
	t.Run("control: a session that pulls its own consumer", func(t *testing.T) {
		pull := "$JS.API.CONSUMER.MSG.NEXT." + bus.Stream + ".s-ses_a"
		grant := session
		grant.Publish = append(slices.Clone(session.Publish), pull)
		require.Equal(t, []string{pull}, jetStreamEntries(grant))
	})
}

// storeAccess is what grant reaches of each store it names: whether it writes it and whether it
// reads it, by the rules natsgrant.Store gives. A store named by a wildcard is no store.
func storeAccess(grant natsgrant.Permissions) (writes, reads map[string]bool) {
	writes, reads = map[string]bool{}, map[string]bool{}
	name := func(stream string) string { return strings.TrimPrefix(stream, "KV_") }
	add := func(set map[string]bool, stream string) {
		if stream != "" && stream != "*" && stream != ">" {
			set[name(stream)] = true
		}
	}
	for _, entry := range grant.Publish {
		subject := strings.Fields(entry)[0]
		tokens := strings.Split(subject, ".")
		switch {
		case tokens[0] == "$KV" && len(tokens) > 1:
			add(writes, tokens[1])
		case tokens[0] == "$JS" && len(tokens) > 2 && (tokens[1] == "ACK" || tokens[1] == "FC"):
			add(reads, tokens[2])
		case tokens[0] == "$JS" && len(tokens) > 4 && tokens[1] == "API" && tokens[2] == "STREAM":
			switch tokens[3] {
			case "CREATE", "UPDATE", "DELETE", "PURGE":
				add(writes, tokens[4])
			case "INFO":
				add(reads, tokens[4])
			case "MSG":
				if len(tokens) > 5 {
					add(reads, tokens[5])
				}
			}
		case tokens[0] == "$JS" && len(tokens) > 4 && tokens[1] == "API" && tokens[2] == "DIRECT":
			add(reads, tokens[4])
		case tokens[0] == "$JS" && len(tokens) > 4 && tokens[1] == "API" && tokens[2] == "CONSUMER":
			stream := tokens[4]
			if tokens[3] == "MSG" || tokens[3] == "DURABLE" {
				if len(tokens) < 6 {
					continue
				}
				stream = tokens[5]
			}
			add(reads, stream)
		default:
			for _, captured := range bus.StreamSubjects() {
				if server.SubjectsCollide(subject, captured) {
					add(writes, bus.Stream)
				}
			}
		}
	}
	return writes, reads
}

// storeFaults names each way the table natsgrant.Stores disagrees with the grants: a store a grant
// writes or reads that the table does not give that grant's kind, or one it gives that no grant of
// that kind reaches; a writer or reader that is no service kind; a session grant that names a
// store; and a store holding a project's privacy or the writer hold.
func storeFaults(stores []natsgrant.Store, grants map[string][]natsgrant.Permissions, sessions map[string]natsgrant.Permissions) []string {
	var faults []string
	derivedWriters, derivedReaders := map[string][]string{}, map[string][]string{}
	for kind, kindGrants := range grants {
		for _, grant := range kindGrants {
			writes, reads := storeAccess(grant)
			for store := range writes {
				if !slices.Contains(derivedWriters[store], kind) {
					derivedWriters[store] = append(derivedWriters[store], kind)
				}
			}
			for store := range reads {
				if !slices.Contains(derivedReaders[store], kind) {
					derivedReaders[store] = append(derivedReaders[store], kind)
				}
			}
		}
	}
	table := map[string]natsgrant.Store{}
	for _, store := range stores {
		table[store.Name] = store
		for _, kind := range slices.Concat(store.Writers, store.Readers) {
			if _, service := grants[kind]; !service {
				faults = append(faults, fmt.Sprintf("store %s names %s, which is no service kind", store.Name, kind))
			}
		}
		if store.Name == "envoy_private_projects" || store.Name == "envoy_maintenance" {
			faults = append(faults, "store "+store.Name+" holds what a project's privacy or the writer hold decides")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(mergeKeys(derivedWriters, derivedReaders, table))) {
		row := table[name]
		if got, want := sortedSet(derivedWriters[name]), sortedSet(row.Writers); !slices.Equal(got, want) {
			faults = append(faults, fmt.Sprintf("store %s is written by %v, the table says %v", name, got, want))
		}
		if got, want := sortedSet(derivedReaders[name]), sortedSet(row.Readers); !slices.Equal(got, want) {
			faults = append(faults, fmt.Sprintf("store %s is read by %v, the table says %v", name, got, want))
		}
		for sessionName, grant := range sessions {
			for _, entry := range slices.Concat(grant.Publish, grant.Subscribe) {
				if strings.Contains(entry, "KV_"+name) || strings.Contains(entry, "$KV."+name+".") || strings.Contains(entry, "."+name+".") || strings.HasSuffix(entry, "."+name) {
					faults = append(faults, fmt.Sprintf("%s names store %s in %s", sessionName, name, entry))
				}
			}
		}
	}
	return faults
}

func mergeKeys(a, b map[string][]string, c map[string]natsgrant.Store) map[string]bool {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range c {
		keys[k] = true
	}
	return keys
}

func sortedSet(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

// bucketConstants is each const of the non-test Go files under root whose name ends in Bucket and
// whose value is a string literal, name to value.
func bucketConstants(t *testing.T, root string) map[string]string {
	t.Helper()
	constants := map[string]string{}
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, ident := range value.Names {
					if !strings.HasSuffix(ident.Name, "Bucket") || i >= len(value.Values) {
						continue
					}
					if literal, ok := value.Values[i].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						constants[ident.Name], _ = strconv.Unquote(literal.Value)
					}
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
	return constants
}

// retiringBuckets are bucket constants of internal/bus that are no store of this package's grants
// and are on their way out: the private-projects bucket, which only the auth service grants
// (natscallout's scopeGrant) and which goes with those grants once Dispatch's privacy answer is
// read through the broker.
var retiringBuckets = []string{"PrivateProjectsBucket"}

// Every bucket and stream is sorted by who writes and who reads it (natsgrant.Stores): the table
// agrees with what each service's grant reaches, every writer and reader is a service, no session
// grant names a store, no store holds a project's privacy or the writer hold, and every bucket
// constant of internal/bus has its row. The controls: a new bucket constant with no row, named by
// the walk; and $KV.envoy_sessions.> added to Session, named with the bucket.
func TestEveryBucketIsSortedByTier(t *testing.T) {
	grants := map[string][]natsgrant.Permissions{
		"listener": {natsgrant.Listener("example-hub"), natsgrant.ListenerHost("example-hub")},
		"dispatch": {natsgrant.Dispatch("example-dispatch")},
		"daemon":   {natsgrant.Daemon()},
		"measure":  {natsgrant.Measure()},
	}
	session, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	sessions := map[string]natsgrant.Permissions{"Session(ses_a)": session, "SessionPattern()": natsgrant.SessionPattern()}
	require.Empty(t, storeFaults(natsgrant.Stores(), grants, sessions))

	rows := map[string]bool{}
	for _, store := range natsgrant.Stores() {
		rows[store.Name] = true
	}
	unsorted := func(constants map[string]string) []string {
		var missing []string
		for name, value := range constants {
			if !rows[value] && !slices.Contains(retiringBuckets, name) {
				missing = append(missing, name)
			}
		}
		slices.Sort(missing)
		return missing
	}
	constants := bucketConstants(t, filepath.Join("..", "internal", "bus"))
	require.Contains(t, constants, "SessionBucket", "the walk read no bucket constant of internal/bus")
	require.Empty(t, unsorted(constants), "a bucket constant of internal/bus has no row in natsgrant.Stores")

	t.Run("control: a new bucket constant with no row", func(t *testing.T) {
		require.Equal(t, []string{"ScratchBucket"}, unsorted(bucketConstants(t, filepath.Join("testdata", "stores"))))
	})
	t.Run("control: a session grant naming a bucket", func(t *testing.T) {
		planted := session
		planted.Publish = append(slices.Clone(session.Publish), "$KV."+bus.SessionBucket+".>")
		faults := strings.Join(storeFaults(natsgrant.Stores(), grants, map[string]natsgrant.Permissions{"Session(ses_a)": planted}), "\n")
		require.Contains(t, faults, "Session(ses_a) names store "+bus.SessionBucket)
	})
}
