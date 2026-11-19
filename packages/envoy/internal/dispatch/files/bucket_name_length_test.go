package files

import (
	"strings"
	"testing"
)

// The refusal of a bucket name's length says "it must be 3 to 63 characters long", so the bound is
// held in characters, each one code point. A name of 40 accented letters is inside it, and is
// refused for the letters it holds; a name of 2 is outside it, and is refused for its length.
func TestABucketNameIsMeasuredInCharacters(t *testing.T) {
	for _, row := range []struct {
		name, bucket, want string
	}{
		{name: "control: 64 Latin letters", bucket: strings.Repeat("a", 64), want: "it must be 3 to 63 characters long"},
		{name: "40 accented letters", bucket: strings.Repeat("é", 40), want: "it holds 'é'"},
		{name: "2 accented letters", bucket: strings.Repeat("é", 2), want: "it must be 3 to 63 characters long"},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := validGCSBucket(row.bucket)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("validGCSBucket of %d characters (%d bytes) = %v, want it to say %q",
					len([]rune(row.bucket)), len(row.bucket), err, row.want)
			}
		})
	}
}
