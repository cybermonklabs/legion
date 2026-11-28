package ompsessions

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

var blobHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ReadBlob reads an Oh My Pi image blob by SHA-256, including blobs stored in
// parts. A missing row or incomplete blob returns nil bytes and no error.
func ReadBlob(ctx context.Context, conn *Conn, hash string) ([]byte, error) {
	if !blobHash.MatchString(hash) {
		return nil, fmt.Errorf("read session blob: invalid SHA-256 digest %q", hash)
	}
	var content []byte
	err := inTransaction(ctx, conn, true, func(p pinned) error {
		content = nil
		var body []byte
		var size int64
		err := p.queryRow(ctx, "SELECT content, byte_len FROM public.omp_session_files_blobs WHERE sha256 = $1", hash).Scan(&body, &size)
		if err != nil {
			return err
		}
		if int64(len(body)) != size {
			// The root row holds an ordered string of 64-byte part keys.
			if len(body) == 0 || len(body)%64 != 0 {
				return nil
			}
			keys := make([]string, 0, len(body)/64)
			for i := 0; i < len(body); i += 64 {
				key := string(body[i : i+64])
				if !blobHash.MatchString(key) {
					return nil
				}
				keys = append(keys, key)
			}
			rows, err := p.query(ctx, "SELECT sha256, content FROM public.omp_session_files_blobs WHERE sha256 = ANY($1::text[])", keys)
			if err != nil {
				return err
			}
			defer rows.Close()
			parts := make(map[string][]byte, len(keys))
			for rows.Next() {
				var key string
				var part []byte
				if err := rows.Scan(&key, &part); err != nil {
					return err
				}
				parts[key] = part
			}
			if err := rows.Err(); err != nil {
				return err
			}
			body = nil
			for _, key := range keys {
				part, ok := parts[key]
				if !ok || int64(len(part)) > size-int64(len(body)) {
					return nil
				}
				body = append(body, part...)
			}
		}
		if got := Summarize(body); int64(got.Bytes) == size && got.SHA256 == hash {
			content = body
		}
		return nil
	})
	if isUndefinedTable(err) || errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session blob %s: %w", hash, err)
	}
	return content, nil
}
