package main

import (
	"errors"
	"io"
	"strings"
)

// commandInput prevents a prompt's reader from consuming bytes intended for
// the next prompt or menu action.
type commandInput struct{ source io.Reader }

func (in *commandInput) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return in.source.Read(p)
}
func sharedInput(in io.Reader) *commandInput {
	if shared, ok := in.(*commandInput); ok {
		return shared
	}
	return &commandInput{source: in}
}
func inputLine(in io.Reader) (string, error) {
	var b strings.Builder
	var one [1]byte
	for {
		n, err := in.Read(one[:])
		if n != 0 {
			if one[0] == '\n' {
				return strings.TrimSpace(b.String()), nil
			}
			if b.Len() >= 65536 {
				return "", errors.New("input exceeds 64 KiB")
			}
			b.WriteByte(one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && b.Len() > 0 {
				return strings.TrimSpace(b.String()), nil
			}
			return "", err
		}
	}
}
