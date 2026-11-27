package repocache

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// signedLife is how long an address sign makes lets its holder download the object: 15 minutes,
// the life the cache's /packs answer promises each address (spec R3). It is
// workspace.BucketFetchTimeout, a fetch's whole attempt on the bucket, by definition rather than by
// a second figure: the attempt's bound and the address's life are one value, so a fetch never
// downloads past its addresses' life, and retireAfter, twice signedLife, which keeps an object no
// set names for 30 minutes so that a download already under way keeps its object, rests on every
// attempt ending within one life of the answer it began on.
const signedLife = workspace.BucketFetchTimeout

// expiresRounding is added to the expiry sign asks for so that the address states signedLife
// whole. storage.SignedURL reads its own clock after the caller's, and states the life as the whole
// seconds from its reading to the expiry, cutting the fraction, so an expiry of exactly the
// caller's time plus 900 s is stated as 899. Half a second covers the moments between the two
// readings and is cut away, so the address states 900, counted from its X-Goog-Date.
const expiresRounding = time.Second / 2

// emulatorHostEnv names the variable that puts a bucket in emulator mode, the name Google's
// clients read for a Cloud Storage emulator: for the real-binary harness and the burst Job, which
// run the cache against package fakegcs's double. The daemon never sets it on a cache pod.
const emulatorHostEnv = "STORAGE_EMULATOR_HOST"

// EmulatorEmail is the service account a bucket in emulator mode signs its addresses as, and so
// the one the double that stands for Cloud Storage and IAM Credentials there must answer and sign
// as (fakegcs.Options.Email, cmd/fakegcs's -email). It names no account of any project:
// emulator.invalid is a domain that cannot exist.
const EmulatorEmail = "legion-repository-cache@emulator.invalid"

// sha256Metadata is the custom metadata key put sets on each object to the SHA-256 of its bytes,
// in hex, and compares before it takes an object already in the bucket for the local file. Cloud
// Storage computes an object's size, CRC32C and MD5 itself; the SHA-256 is the cache's own word,
// the digest an issue pod checks each download against.
const sha256Metadata = "sha256"

// packContentType is the content type put gives each object: a pack and an index are binary, and
// a writer told none would guess one from the first bytes.
const packContentType = "application/octet-stream"

// packDir is the clone's directory of packs, relative to the clone, and the last part of every
// object name's prefix (workspace.PackPrefix), so that an object's name past the prefix is the
// pack's or the index's own file name.
const packDir = "objects/pack/"

// tokenScope is the OAuth scope the cache asks the metadata server's token for: cloud-platform,
// which both Cloud Storage's JSON API and IAM Credentials' signBlob take. What the token may do is
// bounded by the grants of the bucket and of the account, not by the scope.
const tokenScope = iamcredentials.CloudPlatformScope

// tokenEarlyExpiry is how long before its expiry the cache asks the metadata server for a token
// in place of the one it holds: 225 s, as oauth2/google's ComputeTokenSource does, whose note says
// the metadata server's shortest token cache is 4 minutes, so that an earlier refresh is wasted.
const tokenEarlyExpiry = 225 * time.Second

// castagnoli is the table of the Castagnoli polynomial: the CRC32C Cloud Storage keeps of each
// object, which put states for each upload so that Cloud Storage refuses bytes that changed on the
// way.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// bucket is the cache's one view of the Cloud Storage bucket it keeps each pack it serves in
// (runtime.kubernetes.repository_cache.bucket, told it in BucketEnv): the objects under its
// repository's prefix (workspace.PackPrefix), each a pack or an index of the clone under the
// clone's own file name, which it lists, uploads, deletes and signs downloads of. A pack's name is
// its checksum, so an object is written once and never changed: put creates and never replaces,
// and delete removes only the generation its caller listed. Its methods may be called at once from
// several goroutines.
//
// The cache's Google identity needs these permissions, and this code grants none of them; the
// deployment grants them to the Google service account that the ServiceAccount the cache pods
// alone run as (runtime.kubernetes.repository_cache.service_account) is bound to through Workload
// Identity:
//
//   - on the bucket, storage.objects.list (list, by prefix), storage.objects.get (an object's
//     metadata, which put reads before it takes an object already there for the local file),
//     storage.objects.create (put; an upload that may not replace an object needs no other) and
//     storage.objects.delete (delete, of a pack no set names any more);
//   - on its own account, iam.serviceAccounts.signBlob (sign), which
//     roles/iam.serviceAccountTokenCreator granted to the account on itself holds.
//
// The metadata server names that Google service account as the pod's account (openBucket), which
// every address is signed as. A ServiceAccount bound to none holds only the pool's federated
// principal, which has no email: the metadata server names the pool itself, IAM Credentials
// refuses every signBlob as it, and /packs answers 503 for as long as that lasts.
//
// No reader of the packs holds any grant: an issue pod downloads each object through the address
// the cache signed for it, which is its whole access, and holds no Google credential.
type bucket struct {
	name     string
	prefix   string
	client   *storage.Client
	handle   *storage.BucketHandle
	email    string
	signer   signer
	hostname string
	insecure bool
}

// storedObject is one pack or index list found in the bucket: its object name, the generation it
// was written as, which a delete of it must match, its size and CRC32C as Cloud Storage computed
// them of the bytes it holds, and the SHA-256 its sha256Metadata states, as put wrote it ("" where
// it states none).
type storedObject struct {
	Name       string
	Generation int64
	Size       int64
	CRC32C     uint32
	SHA256     string
}

// localObject is one pack or index of the clone as put uploads it: the file's path, and its
// size, SHA-256 in hex and CRC32C as read from its bytes (localObjectOf). A pack and its index
// never change once git wrote them, so a caller may keep these figures for a file it has not seen
// change rather than read the file again.
type localObject struct {
	Path   string
	Size   int64
	SHA256 string
	CRC32C uint32
}

// signer signs bytes as the bucket's identity: the seam each address sign makes is signed
// through. Its one implementation, in production and in emulator mode alike, is iamSigner.
type signer interface {
	Sign(ctx context.Context, payload []byte) ([]byte, error)
}

// googleEndpoints are the base addresses a production bucket sends its Cloud Storage JSON API and
// IAM Credentials requests to: both empty, Google's own, as openBucket uses them. A test sets both
// to the double, and GCE_METADATA_HOST, which the metadata client reads, to drive the production
// path against it, the metadata server's identity and token included.
type googleEndpoints struct {
	storage string
	iam     string
}

// bucketAccess is how a bucket reaches Google in one mode: the account its addresses are signed
// as, the host and scheme they name, and the options of its Storage and IAM Credentials clients,
// which carry the credential, or none, and the endpoint.
type bucketAccess struct {
	email    string
	hostname string
	insecure bool
	storage  []option.ClientOption
	iam      []option.ClientOption
}

// openBucket opens the bucket name for repo's packs, or answers nil and no error, having sent no
// request and read nothing, where name is empty: a cache told no bucket keeps none and serves /tar
// alone.
//
// In production only the cache pod's own Google identity reaches the bucket and IAM Credentials:
// the service account the GKE metadata server names (instance/service-accounts/default/email),
// which every address is signed as, and the access token the metadata server issues that account
// (metadataTokens), refreshed before it expires, which every Storage and IAM Credentials request
// carries. No credential is looked for anywhere else: no application default credentials, which
// would read the key file GOOGLE_APPLICATION_CREDENTIALS names or gcloud's, no key file of any
// kind and no impersonation chain; and nothing is uploaded through a signed address. A signed
// download is HTTPS on workspace.CloudStorageHost, path style.
//
// Where STORAGE_EMULATOR_HOST is set, it must be a loopback host:port (emulatorHost), refused
// before any request otherwise. Storage and IAM Credentials requests then go to http://<host> with
// no credential, the identity is EmulatorEmail, and an address names
// http://<host>/<bucket>/<object>. The same V4 builder (storage.SignedURL) and the same IAM
// Credentials request (iamSigner) serve both modes.
func openBucket(ctx context.Context, name string, repo ghrepo.Repository) (*bucket, error) {
	return openBucketAt(ctx, name, repo, googleEndpoints{})
}

// openBucketAt is openBucket with a production bucket's Storage and IAM Credentials requests sent
// to at, a test's seam: emulator mode takes its one address from STORAGE_EMULATOR_HOST.
func openBucketAt(ctx context.Context, name string, repo ghrepo.Repository, at googleEndpoints) (*bucket, error) {
	if name == "" {
		return nil, nil
	}
	if repo.IsZero() {
		return nil, errors.New("a bucket's repository is required")
	}
	if value := os.Getenv(emulatorHostEnv); value != "" {
		host, err := emulatorHost(value)
		if err != nil {
			return nil, err
		}
		return newBucket(ctx, name, repo, bucketAccess{
			email:    EmulatorEmail,
			hostname: host,
			insecure: true,
			storage:  []option.ClientOption{option.WithoutAuthentication(), option.WithEndpoint("http://" + host + "/storage/v1/")},
			iam:      []option.ClientOption{option.WithoutAuthentication(), option.WithEndpoint("http://" + host + "/")},
		})
	}
	identity := metadata.NewWithOptions(&metadata.Options{})
	email, err := identity.EmailWithContext(ctx, "default")
	if err != nil {
		return nil, fmt.Errorf("read the cache's Google service account from the metadata server: %w", err)
	}
	if email == "" {
		return nil, errors.New("the metadata server names no Google service account for the cache")
	}
	tokens := option.WithTokenSource(oauth2.ReuseTokenSourceWithExpiry(nil, metadataTokens{identity}, tokenEarlyExpiry))
	// The address's Hostname is set, so that no environment variable moves a production address
	// elsewhere: storage.SignedURL takes STORAGE_EMULATOR_HOST's host where no Hostname is set.
	production := bucketAccess{email: email, hostname: workspace.CloudStorageHost, storage: []option.ClientOption{tokens}, iam: []option.ClientOption{tokens}}
	if at.storage != "" {
		production.storage = append(production.storage, option.WithEndpoint(at.storage))
	}
	if at.iam != "" {
		production.iam = append(production.iam, option.WithEndpoint(at.iam))
	}
	return newBucket(ctx, name, repo, production)
}

// newBucket makes the bucket's Storage client and its signer's IAM Credentials client with
// through's options, neither of which sends a request until the bucket does. The Storage client's
// own metrics stay off whatever the environment says (GCP_STORAGE_GO_ENABLE_OTEL_METRICS turns
// them on): they would export to Cloud Monitoring, a service the cache has no grant on.
func newBucket(ctx context.Context, name string, repo ghrepo.Repository, through bucketAccess) (*bucket, error) {
	client, err := storage.NewClient(ctx, append(through.storage, storage.WithDisabledClientMetrics())...)
	if err != nil {
		return nil, fmt.Errorf("make the Cloud Storage client of bucket %s: %w", name, err)
	}
	iam, err := iamcredentials.NewService(ctx, through.iam...)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("make the IAM Credentials client that signs as %s: %w", through.email, err)
	}
	return &bucket{
		name:     name,
		prefix:   workspace.PackPrefix(repo),
		client:   client,
		handle:   client.Bucket(name),
		email:    through.email,
		signer:   iamSigner{service: iam, email: through.email},
		hostname: through.hostname,
		insecure: through.insecure,
	}, nil
}

// emulatorHost is value, STORAGE_EMULATOR_HOST's, where it is a loopback host:port: an address of
// 127.0.0.0/8 or ::1, or localhost, then a port. Anything else is refused, a URL and a bare host
// among them, naming the variable alone: a refused value is no loopback host:port, so it can be a
// URL with user information or anything else a person put there, and is printed nowhere. Emulator
// mode sends every Storage and IAM Credentials request to that address with no credential and
// signs downloads for it, so it takes only a double on the cache's own host.
func emulatorHost(value string) (string, error) {
	refuse := func() error {
		return fmt.Errorf("%s is not a loopback host:port (an address of 127.0.0.0/8 or ::1, or localhost, then a port): the cache takes a Cloud Storage emulator only on its own host", emulatorHostEnv)
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", refuse()
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return "", refuse()
	}
	if host == "localhost" {
		return value, nil
	}
	if address, err := netip.ParseAddr(host); err != nil || !address.IsLoopback() || address.Zone() != "" {
		return "", refuse()
	}
	return value, nil
}

// metadataTokens is a production bucket's token source, which oauth2.ReuseTokenSourceWithExpiry
// keeps and refreshes: the access token of the pod's Google service account, which the GKE
// metadata server issues (instance/service-accounts/default/token), asked of the metadata client
// that read the account's email. It is oauth2/google's ComputeTokenSource without that source's
// gate, metadata.OnGCE, which probes the metadata server's address and DNS name once per process
// and keeps its first answer for the process's life, so a cache whose first probe lost to a DNS
// answer not yet ready at its pod's start would be refused every token until the pod was replaced.
// Each request is bounded by the metadata client: 5 s an attempt, at most 5 retries.
type metadataTokens struct {
	client *metadata.Client
}

// Token asks the metadata server for the account's token, for tokenScope. Its error never quotes
// the answer, which may hold a token.
func (m metadataTokens) Token() (*oauth2.Token, error) {
	answer, err := m.client.GetWithContext(context.Background(), "instance/service-accounts/default/token?scopes="+url.QueryEscape(tokenScope))
	if err != nil {
		return nil, fmt.Errorf("read the cache's access token from the metadata server: %w", err)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal([]byte(answer), &token); err != nil || token.AccessToken == "" || token.ExpiresIn <= 0 {
		return nil, errors.New("the metadata server answered no access token with a lifetime")
	}
	return &oauth2.Token{AccessToken: token.AccessToken, TokenType: token.TokenType, Expiry: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)}, nil
}

// iamSigner signs as email through IAM Credentials' signBlob (projects.serviceAccounts.signBlob),
// which signs with a key Google keeps for the account, so the cache holds no key. It asks for the
// account itself, through no delegation chain, with the client's credential, which is that
// account's own token in production and none in emulator mode.
type iamSigner struct {
	service *iamcredentials.Service
	email   string
}

// Sign answers IAM Credentials' signature of payload, decoded from the signedBlob it answers in
// base64; the request ends with ctx.
func (s iamSigner) Sign(ctx context.Context, payload []byte) ([]byte, error) {
	request := &iamcredentials.SignBlobRequest{Payload: base64.StdEncoding.EncodeToString(payload)}
	answer, err := s.service.Projects.ServiceAccounts.SignBlob("projects/-/serviceAccounts/"+s.email, request).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sign as %s through IAM Credentials: %w", s.email, err)
	}
	signature, err := base64.StdEncoding.DecodeString(answer.SignedBlob)
	if err != nil || len(signature) == 0 {
		return nil, fmt.Errorf("IAM Credentials answered no signature in base64 for %s", s.email)
	}
	return signature, nil
}

// clonePath maps an object name to the clone-relative path of the pack or index it holds,
// objects/pack/<file>, where name is the bucket's prefix then one file name the clone's tar
// streams there, pack-<id>.pack or pack-<id>.idx (workspace.PackFileName), and refuses any other
// name: no object the cache did not write is taken for a pack, and no name outside the prefix is
// uploaded, deleted or signed.
func (b *bucket) clonePath(name string) (string, error) {
	file, ok := strings.CutPrefix(name, b.prefix)
	if !ok || !workspace.PackFileName(file) {
		return "", fmt.Errorf("%q is no object of the cache's in bucket %s: it writes only %spack-<id>.pack and pack-<id>.idx", name, b.name, b.prefix)
	}
	return packDir + file, nil
}

// objectName is clonePath's inverse: the object name of the pack or index at path, relative to the
// clone, refusing a path that is no pack or index the clone's tar streams.
func (b *bucket) objectName(path string) (string, error) {
	file, ok := strings.CutPrefix(path, packDir)
	if !ok || !workspace.PackFileName(file) {
		return "", fmt.Errorf("%q is no pack or index of the clone: the bucket holds only %spack-<id>.pack and pack-<id>.idx", path, packDir)
	}
	return b.prefix + file, nil
}

// list is every pack and index of the repository in the bucket, by object name: the objects under
// its prefix, which the list request names (storage.Query.Prefix), so that Cloud Storage answers
// nothing of another repository's, and of those each whose name is one the cache writes
// (clonePath). Every other name the list answered is in ignored, for its caller to count: such a
// name is Cloud Storage's words, which can quote the bearer the list carried, and prints nowhere.
// The caller never deletes one, since the cache did not write it.
func (b *bucket) list(ctx context.Context) (objects map[string]storedObject, ignored []string, err error) {
	objects = map[string]storedObject{}
	it := b.handle.Objects(ctx, &storage.Query{Prefix: b.prefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return objects, ignored, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("list %s in bucket %s: %w", b.prefix, b.name, err)
		}
		if _, err := b.clonePath(attrs.Name); err != nil {
			ignored = append(ignored, attrs.Name)
			continue
		}
		objects[attrs.Name] = storedOf(attrs)
	}
}

// put makes name's object hold local's file, returning only once Cloud Storage has stored the
// whole object, so a caller may name it in a listed set as soon as put returns, and answers whether
// it wrote the object (uploaded) or took one already there. An object already under name is taken
// for the file where its size, CRC32C and sha256Metadata are all the file's, with nothing uploaded;
// one that differs in any of them is an error naming the name and the field, and is left as it is:
// put never replaces or deletes an object, since a pack's name is its checksum and an object of
// that name holding other bytes is a fault to report, not a pack to overwrite.
//
// The upload streams the file (a storage.Writer holds one chunk of it at a time), creates the
// object only where none of its name exists (ifGenerationMatch=0), sets sha256Metadata to the
// file's SHA-256 and states its CRC32C, so that Cloud Storage refuses bytes that changed on the
// way. Where another upload created the object between put's read and its own (412), put reads the
// object again and takes it only where it holds the file's bytes: that object is another writer's,
// so put answers it taken, not uploaded.
func (b *bucket) put(ctx context.Context, name string, local localObject) (uploaded bool, err error) {
	if _, err := b.clonePath(name); err != nil {
		return false, err
	}
	if err := local.check(); err != nil {
		return false, fmt.Errorf("upload %s: %w", name, err)
	}
	object := b.handle.Object(name)
	attrs, err := object.Attrs(ctx)
	switch {
	case err == nil:
		return false, b.reuse(name, storedOf(attrs), local)
	case !errors.Is(err, storage.ErrObjectNotExist):
		return false, fmt.Errorf("read %s in bucket %s: %w", name, b.name, err)
	}
	created, err := upload(ctx, object, local)
	if statusOf(err) == http.StatusPreconditionFailed {
		attrs, err := object.Attrs(ctx)
		if err != nil {
			return false, fmt.Errorf("read %s in bucket %s again, after another upload created it first: %w", name, b.name, err)
		}
		return false, b.reuse(name, storedOf(attrs), local)
	}
	if err != nil {
		return false, fmt.Errorf("upload %s to bucket %s: %w", name, b.name, err)
	}
	if field := created.differs(local); field != "" {
		return false, fmt.Errorf("bucket %s stored %s with another %s than the cache sent", b.name, name, field)
	}
	return true, nil
}

// upload creates object from local's file, which it streams, and answers what Cloud Storage
// stored once the writer closed successfully. A read that fails, or that finds the file another
// size than local states, stops the upload by ending the writer's context, as a storage.Writer is
// stopped with nothing created.
func upload(ctx context.Context, object *storage.ObjectHandle, local localObject) (storedObject, error) {
	file, err := os.Open(local.Path)
	if err != nil {
		return storedObject{}, err
	}
	defer file.Close()
	writing, stop := context.WithCancel(ctx)
	defer stop()
	w := object.If(storage.Conditions{DoesNotExist: true}).NewWriter(writing)
	w.ContentType = packContentType
	w.Metadata = map[string]string{sha256Metadata: local.SHA256}
	w.CRC32C, w.SendCRC32C = local.CRC32C, true
	sent, err := io.Copy(w, io.LimitReader(file, local.Size+1))
	if err == nil && sent != local.Size {
		err = fmt.Errorf("%s is no longer the %d bytes it was read as", local.Path, local.Size)
	}
	if err != nil {
		stop()
		_ = w.Close()
		return storedObject{}, err
	}
	if err := w.Close(); err != nil {
		return storedObject{}, err
	}
	return storedOf(w.Attrs()), nil
}

// reuse takes o, the object already under name, for local's file where it holds the same bytes
// (differs), and otherwise refuses it, leaving it as it is. The refusal names the object by name,
// the name the cache asked for: o's own is Cloud Storage's words.
func (b *bucket) reuse(name string, o storedObject, local localObject) error {
	if field := o.differs(local); field != "" {
		return fmt.Errorf("bucket %s already holds %s with another %s: the cache never replaces an object, so it leaves it as it is", b.name, name, field)
	}
	return nil
}

// delete removes name's object where it is still the generation its caller listed
// (ifGenerationMatch): one that is gone already (404) counts as removed, and one of another
// generation (412) is an error and is left as it is, since it is not the object the caller
// decided to remove. A generation of 0 or less is refused before any request, since a delete with
// no generation to match would remove whatever object holds the name.
func (b *bucket) delete(ctx context.Context, name string, generation int64) error {
	if _, err := b.clonePath(name); err != nil {
		return err
	}
	if generation <= 0 {
		return fmt.Errorf("delete %s: %d is no generation of an object", name, generation)
	}
	err := b.handle.Object(name).If(storage.Conditions{GenerationMatch: generation}).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("delete %s (generation %d) from bucket %s: %w", name, generation, b.name, err)
	}
	return nil
}

// sign answers an address that lets its holder GET name's object, and nothing else, for
// signedLife: a V4 signed address (GOOG4-RSA-SHA256 over the host header alone, an unsigned
// payload), path style, signed as the bucket's identity through its signer, whose request ends
// with ctx. It refuses a name that is not the cache's (clonePath), and answers no address with a
// signer's failure. The address is a credential for its object for its life: its caller hands it
// to the one reader it signed it for and writes it to no log.
func (b *bucket) sign(ctx context.Context, name string) (string, error) {
	if _, err := b.clonePath(name); err != nil {
		return "", err
	}
	address, err := storage.SignedURL(b.name, name, &storage.SignedURLOptions{
		GoogleAccessID: b.email,
		SignBytes:      func(payload []byte) ([]byte, error) { return b.signer.Sign(ctx, payload) },
		Method:         http.MethodGet,
		Expires:        time.Now().Add(signedLife + expiresRounding),
		Scheme:         storage.SigningSchemeV4,
		Style:          storage.PathStyle(),
		Hostname:       b.hostname,
		Insecure:       b.insecure,
	})
	if err != nil {
		return "", fmt.Errorf("sign a download of %s: %w", name, err)
	}
	return address, nil
}

// close releases the bucket's Storage client.
func (b *bucket) close() error {
	return b.client.Close()
}

// storedOf is the storedObject of Cloud Storage's attributes of an object.
func storedOf(attrs *storage.ObjectAttrs) storedObject {
	return storedObject{Name: attrs.Name, Generation: attrs.Generation, Size: attrs.Size, CRC32C: attrs.CRC32C, SHA256: attrs.Metadata[sha256Metadata]}
}

// differs names the first of the size, the CRC32C and the sha256Metadata in which o is not
// local's, or answers "" where all three match: the one test of whether an object in the bucket
// holds the local file's bytes. A size and a CRC32C are named with both values, figures the cache
// reads as numbers. The sha256Metadata o states is Cloud Storage's words, which can quote the
// bearer the read carried, so it is named only as missing or as not the local file's.
func (o storedObject) differs(local localObject) string {
	switch {
	case o.Size != local.Size:
		return fmt.Sprintf("size (%d bytes, the local file's %d)", o.Size, local.Size)
	case o.CRC32C != local.CRC32C:
		return fmt.Sprintf("CRC32C (%08x, the local file's %08x)", o.CRC32C, local.CRC32C)
	case o.SHA256 != local.SHA256 && o.SHA256 == "":
		return fmt.Sprintf("%s metadata (none, the local file's %s)", sha256Metadata, local.SHA256)
	case o.SHA256 != local.SHA256:
		return fmt.Sprintf("%s metadata (not the local file's %s)", sha256Metadata, local.SHA256)
	}
	return ""
}

// localObjectOf reads the file at path once, streaming it, for its size, SHA-256 and CRC32C.
func localObjectOf(path string) (localObject, error) {
	file, err := os.Open(path)
	if err != nil {
		return localObject{}, err
	}
	defer file.Close()
	digest, crc := sha256.New(), crc32.New(castagnoli)
	size, err := io.Copy(io.MultiWriter(digest, crc), file)
	if err != nil {
		return localObject{}, fmt.Errorf("read %s: %w", path, err)
	}
	return localObject{Path: path, Size: size, SHA256: hex.EncodeToString(digest.Sum(nil)), CRC32C: crc.Sum32()}, nil
}

// check refuses a localObject put could not upload truthfully: one with no path, a negative size,
// or a SHA-256 that is not 64 lower-case hex digits. An object written with a wrong sha256Metadata
// would never match its file again, and put replaces no object, so its name would stay refused.
func (l localObject) check() error {
	if l.Path == "" {
		return errors.New("no local file to upload")
	}
	if l.Size < 0 {
		return fmt.Errorf("%s has a negative size, %d", l.Path, l.Size)
	}
	if digest, err := hex.DecodeString(l.SHA256); err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != l.SHA256 {
		return fmt.Errorf("%s's SHA-256 %q is not 64 lower-case hex digits", l.Path, l.SHA256)
	}
	return nil
}

// statusOf is the HTTP status of the Google API error err holds, or 0 where it holds none.
func statusOf(err error) int {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return 0
}
