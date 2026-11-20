package watch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chadrem/slack-thread/internal/thread"
)

type stepClock struct {
	now     time.Time
	onSleep func(n int)
	n       int
}

func (c *stepClock) Now() time.Time { return c.now }
func (c *stepClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	c.n++
	if c.onSleep != nil {
		c.onSleep(c.n)
	}
	return nil
}

// A previous holder was SIGKILLed (its pid is still in the file) and a new
// holder has taken the lock but not yet written its own pid: stop must
// signal the new holder, never the leftover.
func TestStableHolderNeverTrustsALeftoverPid(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	ref, _ := thread.ParseURL("https://x.slack.com/archives/C0123ABCDEF/p1000000000000000")
	path := store.lockPath(ref.Channel, ref.ThreadTS)
	if err := os.WriteFile(path, []byte(`{"pid":99999,"started":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := lockFile(path) // the new holder: locked, pid not written yet
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	clock := &stepClock{onSleep: func(n int) {
		if n == 1 {
			_ = f.Truncate(0)
			_, _ = f.WriteAt([]byte(`{"pid":4243,"started":2}`), 0)
		}
	}}
	holder, err := stableHolder(context.Background(), store, ref, clock)
	if err != nil || holder == nil || holder.PID != 4243 {
		t.Fatalf("holder = %+v err = %v", holder, err)
	}
}
