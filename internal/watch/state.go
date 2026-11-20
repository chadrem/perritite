package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is what a watch remembers between runs, so re-arming needs no
// arguments and nothing posted between runs is missed.
type State struct {
	URL      string `json:"url"`
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	// Cursor is the newest message ts fully handled: delivered, or our own
	// post with nothing undelivered before it. Never past a message that
	// has not been printed.
	Cursor string `json:"cursor"`
	// LastActivity is the last time anyone (including us) posted; the idle
	// clock and keep-warm window measure from it, across runs.
	LastActivity int64 `json:"last_activity"`
	// LastPollMS (Unix milliseconds) paces the next run's first poll, so
	// re-arming never polls sooner than --interval after the previous
	// run's last poll. Saved after every poll, so a killed run paces too.
	LastPollMS int64 `json:"last_poll_ms,omitempty"`
}

// Store keeps state files and lock files in Dir.
type Store struct {
	Dir string
}

func key(channel, threadTS string) string {
	return channel + "-" + strings.ReplaceAll(threadTS, ".", "")
}

func (s *Store) statePath(channel, threadTS string) string {
	return filepath.Join(s.Dir, key(channel, threadTS)+".json")
}

func (s *Store) lockPath(channel, threadTS string) string {
	return filepath.Join(s.Dir, key(channel, threadTS)+".lock")
}

// Load returns the saved state, or a zero State if there is none.
func (s *Store) Load(channel, threadTS string) (State, error) {
	var st State
	path := s.statePath(channel, threadTS)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("corrupt state file %s (delete it and re-arm with --after): %w", path, err)
	}
	return st, nil
}

// Save writes state atomically (temp file + rename), so a waiter killed
// mid-write never leaves a torn file. Only the lock holder writes state.
func (s *Store) Save(st State) error {
	path := s.statePath(st.Channel, st.ThreadTS)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	// Windows refuses to replace a file another process (antivirus, an
	// indexer) briefly holds open; retry for up to half a second.
	var err2 error
	for i := 0; i < 10; i++ {
		if err2 = os.Rename(tmp.Name(), path); err2 == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	os.Remove(tmp.Name())
	return err2
}

// ---------------------------------------------------------------------------
// Locks: one live waiter per thread
// ---------------------------------------------------------------------------
//
// The lock is an OS file lock (flock on Unix, an exclusive-write handle on
// Windows) held for the waiter's whole life. The OS releases it when the
// process exits or is killed, so there are no stale locks, heartbeats or
// pid-reuse guesses, and a holder's recorded pid is always its own. The
// file is never deleted: deleting a locked file would let a second waiter
// lock a fresh one beside it.

var errLocked = errors.New("locked by another process")

// LockInfo is what a holder writes into its lock file.
type LockInfo struct {
	PID     int   `json:"pid"`
	Started int64 `json:"started"`
}

// ErrHeld means another live waiter holds the thread's lock.
type ErrHeld struct{ Holder LockInfo }

func (e *ErrHeld) Error() string {
	return fmt.Sprintf("another waiter (pid %d) is watching this thread", e.Holder.PID)
}

// Lock is a held thread lock.
type Lock struct{ f *os.File }

// Release clears the recorded pid, then drops the lock (closing the file
// releases the OS lock). A waiter killed outright skips this, which is why
// readers never trust a pid without stableHolder's double check.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = l.f.Truncate(0)
		l.f.Close()
		l.f = nil
	}
}

// Acquire takes the thread's lock without blocking. A live holder yields
// *ErrHeld.
func (s *Store) Acquire(channel, threadTS string, info LockInfo) (*Lock, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := lockFile(s.lockPath(channel, threadTS))
	if errors.Is(err, errLocked) {
		return nil, &ErrHeld{Holder: s.readInfo(channel, threadTS)}
	}
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(info)
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(data, 0)
	}
	return &Lock{f: f}, nil
}

// Holder reports the live holder of a thread's lock, if any, by trying to
// take the lock (and releasing it at once if that succeeds).
func (s *Store) Holder(channel, threadTS string) (*LockInfo, error) {
	if _, err := os.Stat(s.lockPath(channel, threadTS)); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	f, err := lockFile(s.lockPath(channel, threadTS))
	if errors.Is(err, errLocked) {
		info := s.readInfo(channel, threadTS)
		return &info, nil
	}
	if err != nil {
		return nil, err
	}
	f.Close()
	return nil, nil
}

// readInfo reads the holder's pid. A holder that has just taken the lock
// may not have written it yet; an unreadable file yields a zero pid.
func (s *Store) readInfo(channel, threadTS string) LockInfo {
	var info LockInfo
	if data, err := os.ReadFile(s.lockPath(channel, threadTS)); err == nil {
		_ = json.Unmarshal(data, &info)
	}
	return info
}
