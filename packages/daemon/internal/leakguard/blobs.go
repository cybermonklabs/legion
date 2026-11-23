package leakguard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// binaryHead is how much of a file git's diff reads to tell whether it is binary
// (xdiff-interface.c's FIRST_FEW_BYTES), and so how much of a blob blobBatch.head reads first.
const binaryHead = 8000

// binary is whether a file is binary as git's diff tells one with no attribute to say, from head,
// the file's first bytes: a zero byte in its first binaryHead bytes.
func binary(head []byte) bool {
	return bytes.IndexByte(head[:min(len(head), binaryHead)], 0) >= 0
}

// blobBatch is one git cat-file --batch over the objects its reader names, read an object at a time
// as git writes them, so that what is held is the object being read and never the batch: one git
// answers a commit's hundreds of blobs, where a git apiece would be hundreds of processes. The
// reader names each blob as it reads it, its first binaryHead bytes first (head): its id is written
// to git's standard input just before its answer is read, so a blob the reader does not name is
// never asked for, and git neither writes it nor, in a partial clone, fetches it. cat-file --batch
// flushes its answer to each id before it reads the next unless --buffer is given, which is git's
// documented interactive use. A blob cat-file does not answer whole is an error, never a file read
// as empty, and the batch ends with close, which stops git where the reader stopped early. A push
// reads the commit objects it adds through one batch of its own the same way, each asked as a
// commit (Guard.pushRecords, blobBatch.object), where a git apiece would be one more process for
// each commit.
type blobBatch struct {
	// in is git's standard input, which head writes each id to, and asks the reading end of it,
	// which this process holds until git has ended (close): a write to in then never fails for a
	// git that ended first, and the read of git's answer that follows meets git's own failure.
	in, asks *os.File
	// out is git's standard output, through pipe, whose reading end close shuts.
	out  *bufio.Reader
	pipe *io.PipeReader
	// stop ends the context git runs under, and ran answers how git ended.
	stop context.CancelFunc
	ran  chan error
	// id is the object being read, the one head asked for, and kind the type it was asked as,
	// which an object cut short is named by (cut).
	id, kind string
	// first holds the first bytes of the object being read (head), one buffer for every object:
	// each reader is done with one object's head before it reads the next.
	first [binaryHead]byte
}

// readBlobs starts the batch, read in dir.
func (g *Guard) readBlobs(ctx context.Context, dir string) (*blobBatch, error) {
	asks, in, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("git cat-file's standard input could not be made: %w", err)
	}
	ctx, stop := context.WithCancel(ctx)
	pipe, into := io.Pipe()
	batch := &blobBatch{in: in, asks: asks, out: bufio.NewReader(pipe), pipe: pipe, stop: stop, ran: make(chan error, 1)}
	go func() {
		var stderr strings.Builder
		status, _, err := g.run(ctx, child{
			argv:   gitArgv("cat-file", "--batch"),
			dir:    dir,
			stdin:  asks,
			stdout: into,
			stderr: &stderr,
		})
		switch {
		case err != nil:
			err = procgroup.Said(err, gitSaid(stderr.String()))
		case status != 0:
			err = gitExited("git cat-file", status, stderr.String())
		}
		// A read past the last thing git wrote meets git's own failure, where it failed.
		into.CloseWithError(err)
		batch.ran <- err
	}()
	return batch, nil
}

// head asks for object id, which must be of type kind ("blob" or "commit"), and reads its header
// and its first binaryHead bytes, or all of it where it is shorter, which say whether a blob is
// binary (binary), and answers how many bytes of it are left. The bytes are the batch's own, good
// until the next head.
func (b *blobBatch) head(id, kind string) (rest int64, head []byte, err error) {
	b.id, b.kind = id, kind
	if _, err := io.WriteString(b.in, id+"\n"); err != nil {
		return 0, nil, err
	}
	// "<id> <type> <size>", and "<id> missing" for no such object.
	header, err := b.out.ReadSlice('\n')
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return 0, nil, err
	}
	fields := strings.Fields(string(header))
	if len(fields) != 3 || fields[0] != id || fields[1] != kind {
		return 0, nil, fmt.Errorf("git cat-file read no %s %s", kind, id)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return 0, nil, fmt.Errorf("git cat-file read no %s %s", kind, id)
	}
	head = b.first[:min(size, binaryHead)]
	if _, err := io.ReadFull(b.out, head); err != nil {
		return 0, nil, b.cut(err)
	}
	return size - int64(len(head)), head, nil
}

// text reads blob id as a text file of at most bound bytes and answers its content. A binary blob
// (binary) it passes over, answering no content, as of a file that holds no line. A text blob above
// bound it leaves unread and answers its size, over, which the caller refuses the read with, naming
// the file in its own words; over is 0 for every other blob. The batch reads nothing after a blob
// left unread, and the caller ends it there (close).
func (b *blobBatch) text(id string, bound int64) (file string, over int64, err error) {
	rest, head, err := b.head(id, "blob")
	if err != nil {
		return "", 0, err
	}
	if binary(head) {
		return "", 0, b.skip(rest)
	}
	if size := int64(len(head)) + rest; size > bound {
		return "", size, nil
	}
	file, err = b.whole(head, rest)
	return file, 0, err
}

// object reads object id, which must be of type kind ("commit"), whole, and answers it: a commit
// object, which a commit's headers are read from (Guard.commitRecords).
func (b *blobBatch) object(id, kind string) (string, error) {
	rest, head, err := b.head(id, kind)
	if err != nil {
		return "", err
	}
	return b.whole(head, rest)
}

// whole reads the rest of the object head read, rest bytes of it, and answers the object whole.
func (b *blobBatch) whole(head []byte, rest int64) (string, error) {
	var content strings.Builder
	content.Grow(len(head) + int(rest))
	content.Write(head)
	if _, err := io.CopyN(&content, b.out, rest); err != nil {
		return "", b.cut(err)
	}
	return content.String(), b.end()
}

// skip passes over the rest of the blob head read, rest bytes of it, unread.
func (b *blobBatch) skip(rest int64) error {
	if _, err := io.CopyN(io.Discard, b.out, rest); err != nil {
		return b.cut(err)
	}
	return b.end()
}

// end reads the newline git writes after each object's content, which is where its size says the
// content ends.
func (b *blobBatch) end() error {
	end, err := b.out.ReadByte()
	if err == nil && end != '\n' {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return b.cut(err)
	}
	return nil
}

// cut is the error of an object whose content git did not write whole: git's own failure where git
// failed, and else the object cut short.
func (b *blobBatch) cut(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("git cat-file answered %s %s cut short", b.kind, b.id)
	}
	return err
}

// close ends the batch and answers how it ended: with failed, the error the reader stopped at,
// where it stopped at one, after it stops git; and else with git's own failure, where git failed,
// once git, its standard input closed, has ended and what it wrote after the last object read is
// passed over.
func (b *blobBatch) close(failed error) error {
	defer b.stop()
	b.in.Close()
	if failed == nil {
		_, failed = io.Copy(io.Discard, b.out)
	} else {
		b.stop()
		b.pipe.CloseWithError(failed)
	}
	ran := <-b.ran
	b.asks.Close()
	if failed == nil {
		return ran
	}
	return failed
}
