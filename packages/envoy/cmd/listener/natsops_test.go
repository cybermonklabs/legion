package main

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// natsClientOp is one NATS protocol operation a client sent, as a relay between a client and a
// server reads it (readNATSClientOp): line is the operation's line, with its line ending, fields
// are the line's whitespace-separated words, and payload is the bytes that follow a publish's
// line, its own line ending included. bytes is what a relay writes on to the server to pass the
// operation through.
type natsClientOp struct {
	line    []byte
	fields  []string
	payload []byte
}

// readNATSClientOp reads one whole client operation from reader. It reads the payload of a publish
// (PUB and HPUB, whose last field is its byte count) as well, so the next read starts at the next
// operation and a relay never passes half a publish on. Every other operation is its line alone,
// whatever it is: a relay that only judges publishes needs no more of the protocol than that.
func readNATSClientOp(reader *bufio.Reader) (natsClientOp, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return natsClientOp{}, err
	}
	op := natsClientOp{line: line, fields: strings.Fields(string(line))}
	if !op.publishes() {
		return op, nil
	}
	size, err := strconv.Atoi(op.fields[len(op.fields)-1])
	if err != nil {
		return natsClientOp{}, err
	}
	op.payload = make([]byte, size+len("\r\n"))
	if _, err := io.ReadFull(reader, op.payload); err != nil {
		return natsClientOp{}, err
	}
	return op, nil
}

// publishes reports whether the operation is a publish, whose subject is its second field.
func (o natsClientOp) publishes() bool {
	return len(o.fields) >= 3 && (strings.EqualFold(o.fields[0], "PUB") || strings.EqualFold(o.fields[0], "HPUB"))
}

// subject is the subject a publish names, and empty for every other operation.
func (o natsClientOp) subject() string {
	if !o.publishes() {
		return ""
	}
	return o.fields[1]
}

// bytes is the whole operation as the client sent it.
func (o natsClientOp) bytes() []byte {
	return append(o.line, o.payload...)
}
