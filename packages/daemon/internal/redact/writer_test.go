package redact

import (
	"errors"
	"slices"
	"testing"
)

// writes is a writer that keeps each write it is given as one entry, and fails each with err when
// err is set.
type writes struct {
	got [][]byte
	err error
}

func (w *writes) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.got = append(w.got, slices.Clone(p))
	return len(p), nil
}

// A write reaches the writer under Writer in one write, with every URL's query and userinfo
// removed, and is answered with its own length; a write the rule leaves alone reaches it byte for
// byte; an error of the writer is the write's.
func TestWriterGivesEachWriteThroughTheURLRule(t *testing.T) {
	refused := errors.New("the log is closed")
	for _, tc := range []struct {
		name, write, want string
		err               error
	}{
		{
			name:  "a write the rule changes",
			write: `could not report: Post "https://legion-reader@daemon.example/legion/v1?token=` + planted + `": connection refused` + "\n",
			want:  `could not report: Post "https://` + URLWithCredentials + `/legion/v1": connection refused` + "\n",
		},
		{
			name:  "a write with no URL",
			write: "[worker-shim] spawned omp (pid 7)\n",
			want:  "[worker-shim] spawned omp (pid 7)\n",
		},
		{
			name:  "a URL with nothing to remove",
			write: "fetched https://github.com/o/r.git#main\n",
			want:  "fetched https://github.com/o/r.git#main\n",
		},
		{
			name:  "an error of the writer",
			write: "dial https://legion-reader@daemon.example/?token=" + planted + "\n",
			err:   refused,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &writes{err: tc.err}
			n, err := Writer(out).Write([]byte(tc.write))
			if tc.err != nil {
				if n != 0 || !errors.Is(err, tc.err) {
					t.Fatalf("Write = %d, %v; want 0 and the writer's error %v", n, err, tc.err)
				}
				return
			}
			if n != len(tc.write) || err != nil {
				t.Fatalf("Write = %d, %v; want the write's own length %d and no error", n, err, len(tc.write))
			}
			if len(out.got) != 1 || string(out.got[0]) != tc.want {
				t.Fatalf("the writer was given %q, want one write of %q", out.got, tc.want)
			}
		})
	}
}

// Writer of nil is nil, so a caller that reads nil as no output still reads it; Writer of a
// writer Writer made is that writer.
func TestWriterOfNilIsNil(t *testing.T) {
	if got := Writer(nil); got != nil {
		t.Fatalf("Writer(nil) = %#v, want nil", got)
	}
	wrapped := Writer(&writes{})
	if again := Writer(wrapped); again != wrapped {
		t.Fatalf("Writer of a writer Writer made = %#v, want that writer %#v", again, wrapped)
	}
}
