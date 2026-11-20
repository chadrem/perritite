// Package watch implements `slack-thread wait`: block until someone else
// posts in a thread, print what they said, and exit.
//
// It exists to make a monitored thread cost one model wakeup per real
// message. It is meant to run as a background command whose EXIT is the
// notification: it prints nothing while waiting, never reports our own
// posts, merges a burst of messages into one exit, and saves its cursor so
// re-arming needs no arguments and loses nothing posted in between.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/chadrem/slack-thread/internal/frame"
	"github.com/chadrem/slack-thread/internal/slack"
	"github.com/chadrem/slack-thread/internal/thread"
)

// Clock is wall time plus an interruptible sleep; tests substitute a fake.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock uses wall time (monotonic reading stripped), so a laptop sleep
// counts toward the idle limit just as it does on the thread.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().Round(0) }

func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Options control one wait.
type Options struct {
	URL string
	// After starts a fresh watch from this ts (usually the announcement).
	// Empty means resume from the saved cursor, or from the newest message
	// if this thread has never been watched.
	After string
	// Idle ends the watch after this long with no posts from anyone
	// (measured across runs). 0 waits forever.
	Idle time.Duration
	// Interval is the poll cadence; no poll is ever closer to the last.
	Interval time.Duration
	// Debounce holds a delivery until the thread has been quiet this long,
	// so a burst of messages is one wakeup. 0 delivers on first sight.
	Debounce time.Duration
	// MaxBatch caps how long a continuous burst can delay delivery.
	MaxBatch time.Duration
	// KeepWarm exits with KEEPWARM after this long without delivering, but
	// only within KeepWarmFor of the last activity, so a busy conversation
	// keeps the model's prompt cache warm. 0 disables it.
	KeepWarm    time.Duration
	KeepWarmFor time.Duration
	// MaxRun exits with RESTART after this long, for hosts that cap
	// background commands (claude -p, the Agent SDK). 0 = no cap.
	MaxRun time.Duration
	// ErrorWindow is how long transient Slack/network errors are ridden out
	// before giving up.
	ErrorWindow time.Duration
	// Replace stops a live waiter on the same thread instead of deferring.
	Replace bool
}

func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = 15 * time.Second
	}
	if o.Debounce < 0 {
		o.Debounce = 0
	}
	if o.MaxBatch <= 0 {
		o.MaxBatch = 2 * time.Minute
	}
	if o.KeepWarmFor <= 0 {
		o.KeepWarmFor = 4 * time.Hour
	}
	if o.ErrorWindow <= 0 {
		o.ErrorWindow = 10 * time.Minute
	}
	return o
}

// Outcome is how a wait ended; it is also the first word of its output.
type Outcome string

const (
	New      Outcome = "NEW"
	Idle     Outcome = "IDLE"
	KeepWarm Outcome = "KEEPWARM"
	Restart  Outcome = "RESTART"
	Already  Outcome = "ALREADY"
	Stopped  Outcome = "STOPPED"
	Failed   Outcome = "ERROR"
)

// Waiter runs waits. Zero-value fields get production defaults.
type Waiter struct {
	Svc   *thread.Service
	Store *Store
	Clock Clock
	// Owners are Slack user ids whose messages are tagged role=owner.
	Owners    map[string]bool
	Out       io.Writer
	PID       int
	Terminate func(int) error
}

// maxBackoff caps the delay between polls while errors persist.
const maxBackoff = 2 * time.Minute

var errMaxRun = errors.New("max run reached")

// Run blocks until the wait ends, writes its outcome to Out, and returns
// it. A non-nil error means the outcome is Failed (exit non-zero).
func (w *Waiter) Run(ctx context.Context, opts Options) (Outcome, error) {
	outcome, err := w.run(ctx, opts.withDefaults())
	if err != nil {
		fmt.Fprintf(w.Out, "ERROR: %s; re-arm to retry, or tell the user and post the sign-off if it keeps failing.\n", frame.OneLine(err.Error()))
		return Failed, err
	}
	return outcome, nil
}

func (w *Waiter) run(ctx context.Context, opts Options) (Outcome, error) {
	ref, err := thread.ParseURL(opts.URL)
	if err != nil {
		return Failed, err
	}
	if ref.ThreadTS == "" {
		return Failed, errors.New("wait needs a thread URL (a link with /p<ts>), not a channel")
	}
	if opts.After != "" && !thread.ValidTS(opts.After) {
		return Failed, fmt.Errorf("--after must be a dotted Slack ts like 1752241234.567890 (the ts= that post prints), not %q", opts.After)
	}
	clock := w.clock()
	start := clock.Now()

	lock, err := w.acquire(ctx, ref, LockInfo{PID: w.PID, Started: start.Unix()}, opts.Replace)
	if err != nil {
		var held *ErrHeld
		if errors.As(err, &held) {
			since := ""
			if held.Holder.Started > 0 {
				since = ", since " + time.Unix(held.Holder.Started, 0).Format(time.RFC3339)
			}
			fmt.Fprintf(w.Out, "ALREADY: another waiter (pid %d%s) is watching this thread; nothing to do. Use --replace to take over.\n",
				held.Holder.PID, since)
			return Already, nil
		}
		if ctx.Err() != nil {
			return w.stopped()
		}
		return Failed, err
	}
	defer lock.Release()

	budget := &errorBudget{window: opts.ErrorWindow, interval: opts.Interval}
	if opts.MaxRun > 0 {
		budget.deadline = start.Add(opts.MaxRun)
	}
	var me slack.AuthInfo
	if err := w.retry(ctx, budget, func() (err error) { me, err = w.Svc.Whoami(ctx); return }); err != nil {
		return w.ended(ctx, err)
	}
	if w.Owners[me.UserID] {
		return Failed, fmt.Errorf("the token posts as %s, which is also in owner_user_ids, so that user's messages look like Claude's own and would never be delivered; give Claude its own Slack identity (a bot token, or a separate user)", me.UserID)
	}

	st, err := w.Store.Load(ref.Channel, ref.ThreadTS)
	if err != nil {
		return Failed, err
	}
	st.URL, st.Channel, st.ThreadTS = opts.URL, ref.Channel, ref.ThreadTS
	switch {
	case opts.After != "":
		st.Cursor, st.LastActivity = opts.After, start.Unix()
	case st.Cursor == "":
		var msgs []slack.Message
		if err := w.retry(ctx, budget, func() (err error) {
			msgs, err = w.Svc.API.Replies(ctx, ref.Channel, ref.ThreadTS, "")
			return
		}); err != nil {
			return w.ended(ctx, err)
		}
		st.Cursor = ref.ThreadTS
		if sorted := thread.SortUnique(msgs); len(sorted) > 0 {
			st.Cursor = sorted[len(sorted)-1].TS
		}
		st.LastActivity = start.Unix()
	}
	if st.LastActivity == 0 {
		st.LastActivity = start.Unix()
	}
	if err := w.Store.Save(st); err != nil {
		return Failed, err
	}

	// seen is how far this run has read; st.Cursor (committed) only moves
	// past messages that have been printed, or own posts with nothing
	// pending before them. A stop or crash mid-debounce therefore
	// re-delivers pending messages next run instead of losing them.
	seen := st.Cursor
	var pending []slack.Message
	var firstPending, lastPending time.Time
	for {
		msgs, err := w.Svc.API.Replies(ctx, ref.Channel, ref.ThreadTS, seen)
		now := clock.Now()
		if err != nil {
			if ctx.Err() != nil {
				return w.stopped()
			}
			delay, giveUp := budget.fail(err, now)
			if giveUp != nil {
				return w.ended(ctx, giveUp)
			}
			if err := clock.Sleep(ctx, delay); err != nil {
				return w.stopped()
			}
			continue
		}
		budget.ok()

		committed := false
		for _, m := range thread.Newer(thread.SortUnique(msgs), seen) {
			seen, st.LastActivity = m.TS, now.Unix()
			if m.User != "" && m.User == me.UserID {
				// Our own post: never wakes anyone.
				if len(pending) == 0 {
					st.Cursor, committed = m.TS, true
				}
				continue
			}
			if len(pending) == 0 {
				firstPending = now
			}
			pending = append(pending, m)
			lastPending = now
		}

		if len(pending) > 0 && (now.Sub(lastPending) >= opts.Debounce || now.Sub(firstPending) >= opts.MaxBatch) {
			w.printBatch(ctx, ref, pending)
			// Print first, then save: a crash between them re-delivers
			// rather than loses.
			st.Cursor = seen
			return New, w.Store.Save(st)
		}
		if committed {
			if err := w.Store.Save(st); err != nil {
				return Failed, err
			}
		}

		if len(pending) == 0 {
			quiet := now.Sub(time.Unix(st.LastActivity, 0))
			switch {
			case opts.Idle > 0 && quiet >= opts.Idle:
				fmt.Fprintf(w.Out, "IDLE: no posts in this thread for %s; the watch has ended.\n", formatDuration(opts.Idle))
				return Idle, nil
			case opts.KeepWarm > 0 && now.Sub(start) >= opts.KeepWarm && quiet <= opts.KeepWarmFor:
				fmt.Fprintln(w.Out, "KEEPWARM: no new messages; re-arm to keep watching.")
				return KeepWarm, nil
			case opts.MaxRun > 0 && now.Sub(start) >= opts.MaxRun:
				return w.restart()
			}
		}

		delay := opts.Interval
		if len(pending) > 0 {
			// Settling a burst: poll again once --debounce has passed, but
			// never sooner than --interval (rate-limited apps rely on it).
			if remaining := opts.Debounce - now.Sub(lastPending); remaining > delay {
				delay = remaining
			}
		}
		if err := clock.Sleep(ctx, delay); err != nil {
			return w.stopped()
		}
	}
}

func (w *Waiter) restart() (Outcome, error) {
	fmt.Fprintln(w.Out, "RESTART: no new messages before --max-run; re-arm to keep watching.")
	return Restart, nil
}

func (w *Waiter) stopped() (Outcome, error) {
	fmt.Fprintln(w.Out, "STOPPED: the watch was stopped; nothing to do.")
	return Stopped, nil
}

// ended maps how a retry loop ended: cancelled, max-run, or a real failure.
func (w *Waiter) ended(ctx context.Context, err error) (Outcome, error) {
	switch {
	case ctx.Err() != nil:
		return w.stopped()
	case errors.Is(err, errMaxRun):
		return w.restart()
	default:
		return Failed, err
	}
}

// acquire takes the lock; with replace it first stops a live holder and
// waits (up to 10s) for the OS to release its lock.
func (w *Waiter) acquire(ctx context.Context, ref thread.Ref, info LockInfo, replace bool) (*Lock, error) {
	lock, err := w.Store.Acquire(ref.Channel, ref.ThreadTS, info)
	var held *ErrHeld
	if !replace || !errors.As(err, &held) {
		return lock, err
	}
	if held.Holder.PID <= 0 {
		return nil, errors.New("--replace: the current waiter has not recorded its pid yet; retry in a moment")
	}
	terminate := w.Terminate
	if terminate == nil {
		terminate = Terminate
	}
	if err := terminate(held.Holder.PID); err != nil {
		return nil, fmt.Errorf("could not stop waiter pid %d: %w", held.Holder.PID, err)
	}
	clock := w.clock()
	for i := 0; i < 100; i++ {
		if lock, err = w.Store.Acquire(ref.Channel, ref.ThreadTS, info); !errors.As(err, &held) {
			return lock, err
		}
		if err := clock.Sleep(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("waiter pid %d did not exit after being stopped", held.Holder.PID)
}

func (w *Waiter) clock() Clock {
	if w.Clock == nil {
		return RealClock{}
	}
	return w.Clock
}

func (w *Waiter) retry(ctx context.Context, b *errorBudget, fn func() error) error {
	clock := w.clock()
	for {
		err := fn()
		if err == nil {
			b.ok()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay, giveUp := b.fail(err, clock.Now())
		if giveUp != nil {
			return giveUp
		}
		if err := clock.Sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// errorBudget rides out transient failures with capped exponential backoff
// until they have lasted `window`; permanent errors fail at once. A gap
// between failures far longer than any backoff means the machine slept,
// so that starts a fresh streak rather than counting the sleep as outage.
type errorBudget struct {
	window   time.Duration
	interval time.Duration
	deadline time.Time // --max-run, zero if none
	since    time.Time
	last     time.Time
	failures int
}

func (e *errorBudget) ok() { e.since, e.last, e.failures = time.Time{}, time.Time{}, 0 }

// fail records err and returns how long to wait, or the error to give up with.
func (e *errorBudget) fail(err error, now time.Time) (time.Duration, error) {
	if !slack.IsTransient(err) {
		return 0, err
	}
	if !e.deadline.IsZero() && !now.Before(e.deadline) {
		return 0, errMaxRun
	}
	if !e.last.IsZero() && now.Sub(e.last) > 2*maxBackoff+e.interval {
		e.since, e.failures = time.Time{}, 0
	}
	if e.since.IsZero() {
		e.since = now
	}
	e.last = now
	e.failures++
	if now.Sub(e.since) >= e.window {
		return 0, fmt.Errorf("giving up after %s of errors: %w", formatDuration(now.Sub(e.since)), err)
	}
	delay := e.interval
	for i := 1; i < e.failures && delay < maxBackoff; i++ {
		delay *= 2
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	if !e.deadline.IsZero() && now.Add(delay).After(e.deadline) {
		delay = e.deadline.Sub(now)
	}
	return delay, nil
}

// ---------------------------------------------------------------------------
// Output framing: the security boundary
// ---------------------------------------------------------------------------

// Protocol lines start at column 0 (NEW, IDLE, KEEPWARM, RESTART, ALREADY,
// STOPPED, ERROR). Every line of thread-authored content goes through
// frame.Quote, and the author name is stripped to a safe charset AFTER the
// keyed user=/role= tokens, so nothing a teammate types can forge an event
// header or claim an identity.

func (w *Waiter) printBatch(ctx context.Context, ref thread.Ref, msgs []slack.Message) {
	for _, m := range msgs {
		n := w.Svc.Normalize(ctx, m, ref, true)
		user, role := n.UserID, "member"
		switch {
		case user == "":
			user, role = "bot", "bot"
		case w.Owners[user]:
			role = "owner"
		}
		fmt.Fprintf(w.Out, "NEW ts=%s user=%s role=%s | %s — %s\n", n.TS, user, role, frame.SafeAuthor(n.Author), n.Time)
		if n.Text != "" {
			fmt.Fprintln(w.Out, frame.Quote(n.Text))
		}
		for _, f := range n.Files {
			detail := f.Path
			if detail == "" {
				detail = f.Note
			}
			fmt.Fprintln(w.Out, frame.Quote("[file] "+f.Name+" -> "+detail))
		}
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0 && d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return d.String()
	}
}

// Stop ends the live waiter on a thread, if any, and waits briefly for it
// to exit. The pid comes from a lock that is currently held, so it is the
// waiter's own, never a recycled one.
func Stop(ctx context.Context, store *Store, rawURL string, terminate func(int) error, clock Clock) (string, error) {
	ref, err := thread.ParseURL(rawURL)
	if err != nil {
		return "", err
	}
	if ref.ThreadTS == "" {
		return "", errors.New("stop needs a thread URL")
	}
	holder, err := store.Holder(ref.Channel, ref.ThreadTS)
	if err != nil {
		return "", err
	}
	for i := 0; holder != nil && holder.PID <= 0 && i < 20; i++ { // just started: pid not written yet
		if err := clock.Sleep(ctx, 50*time.Millisecond); err != nil {
			return "", err
		}
		holder, _ = store.Holder(ref.Channel, ref.ThreadTS)
	}
	if holder == nil {
		return "no waiter is watching this thread", nil
	}
	if holder.PID <= 0 {
		return "", errors.New("a waiter holds this thread but has not recorded its pid; retry in a moment")
	}
	if err := terminate(holder.PID); err != nil {
		return "", fmt.Errorf("could not stop waiter pid %d: %w", holder.PID, err)
	}
	for i := 0; i < 100; i++ {
		if h, _ := store.Holder(ref.Channel, ref.ThreadTS); h == nil {
			return fmt.Sprintf("stopped waiter pid %d", holder.PID), nil
		}
		if err := clock.Sleep(ctx, 100*time.Millisecond); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("signalled waiter pid %d; it has not exited yet", holder.PID), nil
}
