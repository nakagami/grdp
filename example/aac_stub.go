//go:build !darwin || !cgo

package main

import (
	"errors"

	"github.com/nakagami/grdp/plugin/rdpsnd"
)

// aacDecoder is a stub for platforms without AudioToolbox support.
type aacDecoder struct{}

func newAACDecoder(_ rdpsnd.AudioFormat) (*aacDecoder, error) {
	return nil, errors.New("AAC decoding not supported on this platform")
}

func (d *aacDecoder) Decode(_ []byte) ([]byte, error) {
	return nil, errors.New("AAC decoding not supported on this platform")
}

func (d *aacDecoder) Close() {}
