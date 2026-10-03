//go:build !linux || !cgo

// Package journal reads Docker container logs from systemd-journald.
package journal

import (
	"errors"

	"github.com/gliderlabs/logspout/router"
)

// Open always fails: journald support needs linux and cgo.
func Open() (router.JournalSource, error) {
	return nil, errors.New("journal log source requires linux and cgo")
}
