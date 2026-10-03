//go:build linux && cgo

// Package journal reads Docker container logs from systemd-journald.
package journal

import (
	"fmt"
	"os"
	"time"

	"github.com/coreos/go-systemd/v22/sdjournal"

	"github.com/gliderlabs/logspout/cfg"
	"github.com/gliderlabs/logspout/router"
)

const (
	persistentDir = "/var/log/journal"
	volatileDir   = "/run/log/journal"
	waitTimeout   = time.Second
	// Docker writes its container logs through dockerd
	dockerMatch = "_COMM=dockerd"
)

type source struct {
	j *sdjournal.Journal
}

// Open opens the journal for reading new Docker container log entries.
// When BACKLOG=true the existing entries are read first.
func Open() (router.JournalSource, error) {
	dir, err := journalDir()
	if err != nil {
		return nil, err
	}
	j, err := sdjournal.NewJournalFromDir(dir)
	if err != nil {
		return nil, fmt.Errorf("open journal %s: %w", dir, err)
	}
	if err := prepare(j); err != nil {
		_ = j.Close()
		return nil, err
	}
	return &source{j: j}, nil
}

func prepare(j *sdjournal.Journal) error {
	if err := j.AddMatch(dockerMatch); err != nil {
		return err
	}
	// 0 disables the limit on field size
	if err := j.SetDataThreshold(0); err != nil {
		return err
	}
	if cfg.GetEnvDefault("BACKLOG", "") == "true" {
		return j.SeekHead()
	}
	return j.SeekRealtimeUsec(uint64(time.Now().UnixMicro()))
}

func journalDir() (string, error) {
	if dir := os.Getenv("JOURNAL_PATH"); dir != "" {
		return dir, nil
	}
	for _, dir := range []string{persistentDir, volatileDir} {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return dir, nil
		}
	}
	return "", fmt.Errorf("no journal found in %s or %s; is the journald option enabled?", persistentDir, volatileDir)
}

func (s *source) Next() (*router.JournalEntry, error) {
	for {
		n, err := s.j.Next()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			s.j.Wait(waitTimeout)
			continue
		}
		entry, err := s.j.GetEntry()
		if err != nil {
			return nil, err
		}
		return &router.JournalEntry{
			Fields:   entry.Fields,
			Realtime: time.UnixMicro(int64(entry.RealtimeTimestamp)),
		}, nil
	}
}

func (s *source) Close() error {
	return s.j.Close()
}
