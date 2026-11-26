package workspace

// PackManifest is the repository cache's answer to GET /packs, one JSON object, where its packs are
// kept in a Cloud Storage bucket (runtime.kubernetes.repository_cache.bucket): the bucket's name,
// the clone's refs, and every pack of the clone with its index, each an object of the bucket that
// the answer names with a signed address to download it by. The cache's /packs handler is its only
// writer, and an issue pod's fetch its reader.
//
// One answer is one snapshot of the cache's clone, the same one /tar would stream: Refs are exactly
// the files of that snapshot outside objects/pack/ (FeedEntry's rules), and Objects every pack and
// index of it, each pack with its own index and nothing else, every one of them uploaded whole
// before the cache listed it. Each SHA-256 comes from the cache, over the channel its token
// authorizes, never from the bucket, so a reader that checks every download against it takes no
// byte the bucket changed.
type PackManifest struct {
	Bucket  string       `json:"bucket"`
	Refs    []PackRef    `json:"refs"`
	Objects []PackObject `json:"objects"`
}

// PackRef is one ref file of a PackManifest: its path relative to the bare clone, slash-separated
// (HEAD, packed-refs, refs/...), and the file's exact bytes, which JSON carries in base64.
type PackRef struct {
	Path  string `json:"path"`
	Bytes []byte `json:"bytes"`
}

// PackObject is one pack or index of a PackManifest: its object name in the bucket,
// <owner>/<repo>/objects/pack/pack-<id>.pack or .idx (the owner and the repository lower-cased),
// whose last segment is the file's name in the clone's objects/pack; its size in bytes; the SHA-256
// of the cache's own file, in lower-case hex; and a V4 signed address that lets its holder GET the
// object, and nothing else, for 15 minutes from the answer. The address is a credential for its
// object for its life: a reader writes it to no log.
type PackObject struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}
