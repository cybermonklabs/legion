package repocache

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// retireAfter is how long the cache keeps an object of its prefix in the bucket once neither its
// clone nor its listed set holds it, counted from the switch that first left it out: 30 minutes,
// twice signedLife. Every address /packs signed for the object was signed before that switch, so
// each has expired 15 minutes before the delete, and a download that began on one has had as long
// again to end.
const retireAfter = 2 * signedLife

// signConcurrency is how many addresses one /packs answer signs at once: each is one IAM
// Credentials signBlob request, and a clone holds a few packs, each with its index, so one answer
// waits about one request's time rather than one per object, and a burst of answers does not open
// a connection per object.
const signConcurrency = 8

// listedSet is the one set of packs the cache lists, which every /packs answer is signed from: the
// bucket its objects are in, which signs their addresses; the refs of the clone's snapshot it was
// made from, each with its file's exact bytes, encoded once when the set is made, as a /packs
// answer states them (workspace.PackManifest.Refs), so an answer encodes only its objects; and each
// pack of that snapshot with its index, a pack before its index, every one of them uploaded whole
// before the set was installed. A set never changes once made: the cache installs the next in its
// place with one atomic store (cache.listed), so a reader that loaded one reads the whole of it,
// and compares the pointer to learn whether it is still the one listed.
type listedSet struct {
	store   *bucket
	refs    json.RawMessage
	objects []listedObject
}

// listedObject is one pack or index of a listedSet: its object name in the bucket and its file's
// figures as the cache read them (localObject), whose size and SHA-256 /packs states.
type listedObject struct {
	name  string
	local localObject
}

// same reports whether o and other name one object with one size and SHA-256.
func (o listedObject) same(other listedObject) bool {
	return o.name == other.name && o.local.Size == other.local.Size && o.local.SHA256 == other.local.SHA256
}

// holds reports whether the set lists name. A nil set lists nothing.
func (s *listedSet) holds(name string) bool {
	return s != nil && slices.ContainsFunc(s.objects, func(o listedObject) bool { return o.name == name })
}

// holdsSame reports whether the set lists o's name with o's size and SHA-256: an object this
// process uploaded whole, or found whole, before it installed the set, which a later set lists
// again with no request to the bucket. A nil set lists nothing.
func (s *listedSet) holdsSame(o listedObject) bool {
	return s != nil && slices.ContainsFunc(s.objects, o.same)
}

// lists reports whether the set lists exactly next's refs and objects: the same ref files with the
// same bytes, and the same objects in the same order, each with the same size and SHA-256. A nil
// set lists nothing.
func (s *listedSet) lists(next *listedSet) bool {
	return s != nil && bytes.Equal(s.refs, next.refs) && slices.EqualFunc(s.objects, next.objects, listedObject.same)
}

// packDigest is what the cache last read of one pack or index file (cache.digests, by the file's
// path): the file as it stood, its size, modification time and inode, and the figures read from
// its bytes. A file that still stands so is not read again at the next sync: git never changes a
// pack or an index once it wrote it, and a file written again in its place has another inode or
// time.
type packDigest struct {
	size    int64
	modTime time.Time
	inode   uint64
	local   localObject
}

// absence is one object of the repository's prefix that neither the clone nor the listed set
// holds (cache.absent, by object name): the time of the sync that first left it out, from which
// retireAfter counts, and the generation the bucket listed it at, which its delete must match.
type absence struct {
	since      time.Time
	generation int64
}

// syncPacks publishes the clone's packs to the bucket the cache keeps them in (publishPacks), where
// Options.Bucket names one, within the shorter of Options.Refresh and workspace.FetchTimeout, so a
// bucket that answers slowly delays the next refresh by at most that and holds nothing /tar or
// /healthz waits on. A failure is logged and ends this attempt alone: the listed set stays the one
// before it, /tar and /healthz answer as they did, a due repack still runs, and the next refresh
// tries again. Its caller holds maint, so no fetch or repack changes the clone while it runs.
func (c *cache) syncPacks(ctx context.Context) {
	if c.opts.Bucket == "" {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, min(c.opts.Refresh, workspace.FetchTimeout))
	defer cancel()
	if err := c.publishPacks(bounded); err != nil && ctx.Err() == nil {
		c.opts.Log(fmt.Sprintf("publish the packs to bucket %s failed; /packs keeps the set it had, and the next refresh tries again: %s", c.opts.Bucket, oneLine(err)))
	}
}

// publishPacks makes the clone as it stands now the listed set, in the order the bucket's readers
// rely on: it opens the bucket if no earlier sync of this process has (openBucket: the metadata
// server's identity and the clients, which is no check of the cache's start, so a bucket that
// cannot be opened yet is tried again at the next sync); takes the clone's snapshot (snapshotSet),
// whose set holds every pack of the clone that has its index, each with that index, and nothing
// else; uploads each pack and index the listed set does not hold already, each put returning only
// once Cloud Storage stored the whole object, or found an object of that name holding the same
// bytes; and only then installs the new set, with one atomic store, so /packs never names an
// object that is not whole in the bucket, and a pack never without its index. The first failure
// ends it before the store, leaving the set before it listed. Once the new set is installed, it
// retires what no set names any more (retire). A snapshot that lists exactly the listed set's refs
// and objects (listedSet.lists) installs nothing: that set stays listed, so a /packs answer signed
// from it while the sync ran is no answer from a set that was switched, and retire runs at the
// sync's time as it does after a switch, so each object's clock still counts.
func (c *cache) publishPacks(ctx context.Context) error {
	if c.store == nil {
		store, err := openBucket(ctx, c.opts.Bucket, c.opts.Repo)
		if err != nil {
			return fmt.Errorf("open bucket %s: %w", c.opts.Bucket, err)
		}
		c.store = store
	}
	next, inventory, err := c.snapshotSet()
	if err != nil {
		return err
	}
	current := c.listed.Load()
	if current.lists(next) {
		c.retire(ctx, current, inventory, c.clock.Now())
		return nil
	}
	uploaded := 0
	for _, object := range next.objects {
		if current.holdsSame(object) {
			continue
		}
		if err := c.store.put(ctx, object.name, object.local); err != nil {
			return err
		}
		uploaded++
	}
	c.listed.Store(next)
	switched := c.clock.Now()
	if current == nil || uploaded > 0 {
		c.opts.Log(fmt.Sprintf("listed %d packs and indexes in bucket %s, %d of them uploaded now", len(next.objects), c.opts.Bucket, uploaded))
	}
	c.retire(ctx, next, inventory, switched)
	return nil
}

// snapshotSet is the set the clone's snapshot makes, unlisted yet: the snapshot is the one every
// /tar stream takes (snapshot, under the read lock, and taken again while a repack moves the
// packs), its refs are each file outside objects/pack with its bytes, and its objects each pack
// with its index, read for their figures (digest). A pack without its index, or an index without
// its pack, is left out of the set, so it is neither listed nor uploaded, and logged once by this
// process (leaveOut): git reads no pack without its index and no index without its pack, so no ref
// of the clone needs an object only such a file holds, and the rest of the snapshot is listed as
// usual. It answers too the names of every pack and index the snapshot holds, a file left out of
// the set among them, the clone's inventory, which retire spares, so no object of such a name is
// deleted from the bucket while the clone holds the file. The snapshot's packs are closed before
// it returns, since maint, which its caller holds, keeps every fetch and repack from changing them
// while the upload reads them by path.
func (c *cache) snapshotSet() (*listedSet, map[string]bool, error) {
	entries, err := c.snapshot()
	if err != nil {
		return nil, nil, fmt.Errorf("take the clone's refs and packs: %w", err)
	}
	defer closeEntries(entries)
	type packFile struct {
		path string
		file *os.File
	}
	set := &listedSet{store: c.store}
	var refs []workspace.PackRef
	found := map[string]packFile{}
	for _, entry := range entries {
		path := entry.header.Name
		switch {
		case entry.header.Typeflag == tar.TypeDir:
		case strings.HasPrefix(path, packDir):
			name, err := c.store.objectName(path)
			if err != nil {
				return nil, nil, err
			}
			found[name] = packFile{path: filepath.Join(c.dir, filepath.FromSlash(path)), file: entry.file}
		default:
			refs = append(refs, workspace.PackRef{Path: path, Bytes: entry.body})
		}
	}
	if set.refs, err = json.Marshal(refs); err != nil {
		return nil, nil, fmt.Errorf("encode the clone's refs: %w", err)
	}
	inventory := map[string]bool{}
	digests := map[string]packDigest{}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		inventory[name] = true
		stem, pack := strings.CutSuffix(name, ".pack")
		if !pack {
			if _, paired := found[strings.TrimSuffix(name, ".idx")+".pack"]; !paired {
				c.leaveOut(name, found[name].path, "an index without its pack")
			}
			continue
		}
		index, paired := found[stem+".idx"]
		if !paired {
			c.leaveOut(name, found[name].path, "a pack without its index")
			continue
		}
		packLocal, err := c.digest(found[name].path, found[name].file, digests)
		if err != nil {
			return nil, nil, err
		}
		indexLocal, err := c.digest(index.path, index.file, digests)
		if err != nil {
			return nil, nil, err
		}
		set.objects = append(set.objects, listedObject{name: name, local: packLocal}, listedObject{name: stem + ".idx", local: indexLocal})
	}
	c.digests = digests
	return set, inventory, nil
}

// leaveOut logs, once by this process for each object name, that the clone's file at path, what it
// is (a pack without its index, or an index without its pack), is left out of the listed set. Its
// caller holds maint.
func (c *cache) leaveOut(name, path, what string) {
	if c.lone[name] {
		return
	}
	c.lone[name] = true
	c.opts.Log(fmt.Sprintf("the clone holds %s, %s; git reads no pack without its index, so it is left out of the listed set and not uploaded", path, what))
}

// digest is the figures of the pack or index at path, open as file: those cache.digests kept for
// it where the file still stands as it did when they were read (packDigest), and otherwise those
// read now from its bytes, streaming them (localObjectOf). Either way they go into next, the
// digests of the snapshot being taken, which replaces cache.digests whole, so a file the clone no
// longer holds is forgotten. A process starts with none, so a restarted cache reads every file
// again rather than take the figures the bucket states.
func (c *cache) digest(path string, file *os.File, next map[string]packDigest) (localObject, error) {
	info, err := file.Stat()
	if err != nil {
		return localObject{}, fmt.Errorf("stat %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return localObject{}, fmt.Errorf("stat %s: the system answered no inode", path)
	}
	seen := packDigest{size: info.Size(), modTime: info.ModTime(), inode: uint64(stat.Ino)}
	if kept, ok := c.digests[path]; ok && kept.size == seen.size && kept.modTime.Equal(seen.modTime) && kept.inode == seen.inode {
		next[path] = kept
		return kept.local, nil
	}
	local, err := localObjectOf(path)
	if err != nil {
		return localObject{}, err
	}
	if local.Size != seen.size {
		return localObject{}, fmt.Errorf("%s changed while the cache read it: %d bytes, then %d", path, seen.size, local.Size)
	}
	seen.local = local
	next[path] = seen
	return local, nil
}

// retire deletes from the bucket each object of the repository's prefix that neither the clone's
// inventory nor set holds, set being the one listed at switched: the time of the sync that just
// installed it, or that found the clone's snapshot to list it still (publishPacks). It deletes such
// an object once retireAfter has passed since the sync that first left it out. Each such object is
// recorded in cache.absent at the first sync that finds it left out, with that sync's time; the
// record is dropped when the clone or a later set holds the object again, so its clock starts
// afresh if it is left out later; and the object is deleted, matching the generation the bucket
// listed, once a sync comes retireAfter or more after the record's. A record lives in this process
// alone, so a restarted cache counts no object's time from before its own first sync, and never
// from the object's creation, which says nothing of when a set last named it. An object a sync
// uploaded whose set was never installed, which an interrupted upload leaves, is retired the same
// way once a later set does not hold it either.
//
// It runs only after a successful snapshot, and after its switch where the snapshot made a set of
// its own (publishPacks). A failed list or delete is logged and tried again at the next sync, the
// record kept. It touches only what the bucket's list answers under the repository's prefix whose
// name the cache writes (bucket.list): another repository's objects, and a name under the prefix
// the cache did not write, are never deleted, the second logged once by this process. Only a
// running cache retires its own objects: a cache the daemon retires
// (runtime.kubernetes.repository_cache.retain_hours) ends with its pod and deletes nothing in the
// bucket, whose retention the deployment sets.
func (c *cache) retire(ctx context.Context, set *listedSet, inventory map[string]bool, switched time.Time) {
	stored, foreign, err := set.store.list(ctx)
	if err != nil {
		c.opts.Log("list the bucket's packs to retire those no set names; tried again at the next sync: " + oneLine(err))
		return
	}
	for _, name := range foreign {
		if !c.foreign[name] {
			c.foreign[name] = true
			c.opts.Log(fmt.Sprintf("bucket %s holds %s under the repository's prefix, which is no pack or index the cache writes: it is left as it is", set.store.name, name))
		}
	}
	for name := range c.absent {
		if _, ok := stored[name]; !ok {
			delete(c.absent, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(stored)) {
		object := stored[name]
		if inventory[name] || set.holds(name) {
			delete(c.absent, name)
			continue
		}
		record, ok := c.absent[name]
		if !ok || record.generation != object.Generation {
			record = absence{since: switched, generation: object.Generation}
			c.absent[name] = record
		}
		if switched.Sub(record.since) < retireAfter {
			continue
		}
		if err := set.store.delete(ctx, name, object.Generation); err != nil {
			c.opts.Log(fmt.Sprintf("retire %s from bucket %s failed; tried again at the next sync: %s", name, set.store.name, oneLine(err)))
			continue
		}
		delete(c.absent, name)
		c.opts.Log(fmt.Sprintf("deleted %s from bucket %s: neither the clone nor the listed set has held it since %s", name, set.store.name, record.since.UTC().Format(time.RFC3339)))
	}
}

// servePacks answers GET /packs: the listed set as one workspace.PackManifest, with an address
// signed for each of its objects on this request (signedObjects), so every address states its
// whole signedLife, and none is kept from one answer to the next. It reads the set once, and as
// soon as the addresses are signed, before it encodes or writes a byte, it checks that the set is
// still the one listed: one installed while it signed answers 503 with none of the addresses,
// since its objects may be the ones the new set left out, whose retirement counts from that
// switch. It answers 503 too while the cache has listed no set (its first sync has not ended, or
// has failed), and when an address cannot be signed, writing no part of a manifest. It holds no
// lock /tar or a refresh takes, and is no /tar stream: neither streams nor inFlight counts it. Its
// log names no address, token or ref.
func (c *cache) servePacks(w http.ResponseWriter, r *http.Request) {
	set := c.listed.Load()
	if set == nil {
		http.Error(w, "the repository cache has listed no complete set of packs in its bucket yet", http.StatusServiceUnavailable)
		return
	}
	objects, err := signedObjects(r.Context(), set)
	if err != nil {
		if r.Context().Err() == nil {
			c.opts.Log("sign the packs' addresses for " + r.RemoteAddr + ": " + oneLine(err))
		}
		http.Error(w, "the repository cache cannot sign its packs' addresses now", http.StatusServiceUnavailable)
		return
	}
	if c.listed.Load() != set {
		http.Error(w, "the repository cache listed another set of packs while it signed this answer; ask again", http.StatusServiceUnavailable)
		return
	}
	body, err := json.Marshal(packsAnswer{Bucket: set.store.name, Refs: set.refs, Objects: objects})
	if err != nil {
		c.opts.Log("encode the packs' answer for " + r.RemoteAddr + ": " + oneLine(err))
		http.Error(w, "the repository cache cannot encode its packs' answer", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// packsAnswer is a /packs answer as servePacks encodes it: workspace.PackManifest with its refs
// already encoded (listedSet.refs), its fields of the same names, types and order, so that
// json.Marshal writes of it exactly the bytes it writes of the manifest, and an answer encodes only
// its objects.
type packsAnswer struct {
	Bucket  string                 `json:"bucket"`
	Refs    json.RawMessage        `json:"refs"`
	Objects []workspace.PackObject `json:"objects"`
}

// signedObjects is set's objects as a /packs answer states them, each object's address signed now
// by the set's bucket, at most signConcurrency at once, every signature ending with ctx; the first
// failure ends the rest and is its error, with no objects.
func signedObjects(ctx context.Context, set *listedSet) ([]workspace.PackObject, error) {
	objects := make([]workspace.PackObject, len(set.objects))
	group, signing := errgroup.WithContext(ctx)
	group.SetLimit(signConcurrency)
	for i, object := range set.objects {
		group.Go(func() error {
			address, err := set.store.sign(signing, object.name)
			if err != nil {
				return err
			}
			objects[i] = workspace.PackObject{Name: object.name, Size: object.local.Size, SHA256: object.local.SHA256, URL: address}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return objects, nil
}

// closeStore releases the bucket a sync opened, where one did. Serve calls it once its listener
// has stopped, with no refresh running.
func (c *cache) closeStore() {
	if c.store == nil {
		return
	}
	if err := c.store.close(); err != nil {
		c.opts.Log(fmt.Sprintf("close bucket %s: %s", c.opts.Bucket, oneLine(err)))
	}
}
