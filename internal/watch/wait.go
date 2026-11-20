// Package watch implements `perritite wait`: block until someone else
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

	"github.com/chadrem/perritite/internal/frame"
	"github.com/chadrem/perritite/internal/slack"
	"github.com/chadrem/perritite/internal/thread"
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
	// Interval is the poll cadence. No poll is ever closer than this to the
	// previous one, across runs too.
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
	// MaxRun ends the run by this deadline (RESTART, or NEW if messages are
	// in hand), for hosts that cap background commands. 0 = no cap.
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

// maxBackoff caps the delay between polls while errors persist (but never
// below --interval).
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

// session is one run's mutable state.
type session struct {
	w     *Waiter
	ctx   context.Context
	opts  Options
	ref   thread.Ref
	st    State
	start time.Time
	// deadline is --max-run (zero if none).
	deadline time.Time
	// seen is how far this run has read; st.Cursor (committed) only moves
	// past messages that have been printed, or own posts with nothing
	// pending before them. So st is always safe to save, and a stop or
	// crash mid-debounce re-delivers pending messages instead of losing them.
	seen                      string
	pending                   []slack.Message
	firstPending, lastPending time.Time
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
	s := &session{w: w, ctx: ctx, opts: opts, ref: ref, start: clock.Now()}
	if opts.MaxRun > 0 {
		s.deadline = s.start.Add(opts.MaxRun)
	}

	lock, err := w.acquire(ctx, ref, LockInfo{PID: w.PID, Started: s.start.Unix()}, opts.Replace)
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

	budget := &errorBudget{window: opts.ErrorWindow, interval: opts.Interval, deadline: s.deadline}
	var me slack.AuthInfo
	if err := w.retry(ctx, budget, func() (err error) { me, err = w.Svc.Whoami(ctx); return }); err != nil {
		return w.ended(ctx, err)
	}
	if w.Owners[me.UserID] {
		return Failed, fmt.Errorf("the token posts as %s, which is also in owner_user_ids, so that user's messages look like Claude's own and would never be delivered; give Claude its own Slack identity (a bot token, or a separate user)", me.UserID)
	}

	if s.st, err = w.Store.Load(ref.Channel, ref.ThreadTS); err != nil {
		return Failed, err
	}
	s.st.URL, s.st.Channel, s.st.ThreadTS = opts.URL, ref.Channel, ref.ThreadTS
	switch {
	case opts.After != "":
		s.st.Cursor, s.st.LastActivity = opts.After, s.start.Unix()
	case s.st.Cursor == "":
		var msgs []slack.Message
		if err := w.retry(ctx, budget, func() (err error) {
			msgs, err = w.Svc.API.Replies(ctx, ref.Channel, ref.ThreadTS, "")
			return
		}); err != nil {
			return w.ended(ctx, err)
		}
		s.st.Cursor = ref.ThreadTS
		if sorted := thread.SortUnique(msgs); len(sorted) > 0 {
			s.st.Cursor = sorted[len(sorted)-1].TS
		}
		s.st.LastActivity, s.st.LastPollMS = s.start.Unix(), clock.Now().UnixMilli()
	}
	if s.st.LastActivity == 0 {
		s.st.LastActivity = s.start.Unix()
	}
	if err := w.Store.Save(s.st); err != nil {
		return Failed, err
	}
	s.seen = s.st.Cursor

	// Pace the first poll: never closer than --interval to the last one,
	// even if that was the previous run's.
	if s.st.LastPollMS > 0 {
		wait := opts.Interval - clock.Now().Sub(time.UnixMilli(s.st.LastPollMS))
		if wait > opts.Interval {
			wait = opts.Interval // a clock stepped backwards must not become a long hang
		}
		if wait > 0 {
			if out, done, err := s.sleep(wait); done {
				return out, err
			}
		}
	}
	return s.loop(budget, me.UserID)
}

func (s *session) loop(budget *errorBudget, self string) (Outcome, error) {
	w, opts, clock := s.w, s.opts, s.w.clock()
	for {
		msgs, err := s.poll()
		now := clock.Now()
		s.st.LastPollMS = now.UnixMilli()
		s.save() // st.Cursor never passes an undelivered message, so this is always safe
		if err != nil {
			if s.ctx.Err() != nil {
				return s.stop()
			}
			// Only the --max-run deadline means max-run. An ordinary HTTP
			// timeout also matches context.DeadlineExceeded and must be
			// ridden out as a transient error, not turned into RESTART.
			if s.pastDeadline(now) {
				return s.maxRun()
			}
			delay, giveUp := budget.fail(err, now)
			switch {
			case errors.Is(giveUp, errMaxRun):
				return s.maxRun()
			case giveUp != nil && len(s.pending) > 0:
				// Defensive: a settling burst's next poll is scheduled for
				// when it has settled, so the case below normally fires
				// first. Either way, what we hold is real; deliver it and
				// let the next run report the error.
				return s.deliver()
			case giveUp != nil:
				return Failed, giveUp
			case len(s.pending) > 0 && s.settled(now):
				return s.deliver()
			}
			if out, done, err := s.sleep(delay); done {
				return out, err
			}
			continue
		}
		budget.ok()

		committed := false
		for _, m := range thread.Newer(thread.SortUnique(msgs), s.seen) {
			s.seen, s.st.LastActivity = m.TS, now.Unix()
			if m.User != "" && m.User == self {
				// Our own post: never wakes anyone.
				if len(s.pending) == 0 {
					s.st.Cursor, committed = m.TS, true
				}
				continue
			}
			if len(s.pending) == 0 {
				s.firstPending = now
			}
			s.pending = append(s.pending, m)
			s.lastPending = now
		}

		if len(s.pending) > 0 && s.settled(now) {
			return s.deliver()
		}
		if committed {
			if err := w.Store.Save(s.st); err != nil {
				return Failed, err
			}
		}
		if s.pastDeadline(now) {
			return s.maxRun()
		}
		if len(s.pending) == 0 {
			quiet := now.Sub(time.Unix(s.st.LastActivity, 0))
			switch {
			case opts.Idle > 0 && quiet >= opts.Idle:
				s.save()
				fmt.Fprintf(w.Out, "IDLE: no posts in this thread for %s; the watch has ended.\n", formatDuration(opts.Idle))
				return Idle, nil
			case opts.KeepWarm > 0 && now.Sub(s.start) >= opts.KeepWarm && quiet <= opts.KeepWarmFor:
				s.save()
				fmt.Fprintln(w.Out, "KEEPWARM: no new messages; re-arm to keep watching.")
				return KeepWarm, nil
			}
		}

		delay := opts.Interval
		if len(s.pending) > 0 {
			// Settling a burst: poll again once the debounce (or the
			// MaxBatch cap) is up, but never sooner than --interval.
			remaining := opts.Debounce - now.Sub(s.lastPending)
			if batch := opts.MaxBatch - now.Sub(s.firstPending); batch < remaining {
				remaining = batch
			}
			if remaining > delay {
				delay = remaining
			}
		}
		if out, done, err := s.sleep(delay); done {
			return out, err
		}
	}
}

// poll fetches replies newer than seen, bounded by the --max-run deadline.
func (s *session) poll() ([]slack.Message, error) {
	ctx := s.ctx
	if !s.deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, s.deadline)
		defer cancel()
	}
	return s.w.Svc.API.Replies(ctx, s.ref.Channel, s.ref.ThreadTS, s.seen)
}

// sleep waits d, unless that would carry the run past --max-run, in which
// case it ends the run now rather than overshoot or poll early. done
// reports that the run is over (outcome and error are then final).
func (s *session) sleep(d time.Duration) (Outcome, bool, error) {
	clock := s.w.clock()
	if !s.deadline.IsZero() && clock.Now().Add(d).After(s.deadline) {
		out, err := s.maxRun()
		return out, true, err
	}
	if err := clock.Sleep(s.ctx, d); err != nil {
		out, err := s.stop()
		return out, true, err
	}
	return "", false, nil
}

func (s *session) settled(now time.Time) bool {
	return now.Sub(s.lastPending) >= s.opts.Debounce || now.Sub(s.firstPending) >= s.opts.MaxBatch
}

func (s *session) pastDeadline(now time.Time) bool {
	return !s.deadline.IsZero() && !now.Before(s.deadline)
}

// deliver prints the pending messages, then commits the cursor past them:
// a crash between the two re-delivers rather than loses.
func (s *session) deliver() (Outcome, error) {
	s.w.printBatch(s.ctx, s.ref, s.pending)
	s.st.Cursor = s.seen
	return New, s.w.Store.Save(s.st)
}

// maxRun ends a run at its --max-run deadline, delivering anything in hand.
func (s *session) maxRun() (Outcome, error) {
	if len(s.pending) > 0 {
		return s.deliver()
	}
	s.save()
	return s.w.restart()
}

func (s *session) stop() (Outcome, error) {
	s.save() // safe: st.Cursor never passes an undelivered message
	return s.w.stopped()
}

func (s *session) save() { _ = s.w.Store.Save(s.st) }

func (w *Waiter) restart() (Outcome, error) {
	fmt.Fprintln(w.Out, "RESTART: no new messages before --max-run; re-arm to keep watching.")
	return Restart, nil
}

func (w *Waiter) stopped() (Outcome, error) {
	fmt.Fprintln(w.Out, "STOPPED: the watch was stopped; nothing to do.")
	return Stopped, nil
}

// ended maps how a setup retry ended: cancelled, max-run, or a real failure.
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

// acquire takes the lock. A collision is retried briefly first (a `stop`
// probing the lock holds it for an instant). With replace, a live holder
// is stopped and the lock taken once the OS releases it.
func (w *Waiter) acquire(ctx context.Context, ref thread.Ref, info LockInfo, replace bool) (*Lock, error) {
	clock := w.clock()
	var lock *Lock
	var err error
	var held *ErrHeld
	for i := 0; i < 3; i++ {
		if lock, err = w.Store.Acquire(ref.Channel, ref.ThreadTS, info); !errors.As(err, &held) {
			return lock, err
		}
		if err := clock.Sleep(ctx, 50*time.Millisecond); err != nil {
			return nil, err
		}
	}
	if !replace {
		return nil, err
	}
	holder, err := stableHolder(ctx, w.Store, ref, clock)
	if err != nil {
		return nil, err
	}
	if holder != nil {
		terminate := w.Terminate
		if terminate == nil {
			terminate = Terminate
		}
		if err := terminate(holder.PID); err != nil {
			return nil, fmt.Errorf("could not stop waiter pid %d: %w", holder.PID, err)
		}
	}
	for i := 0; i < 100; i++ {
		if lock, err = w.Store.Acquire(ref.Channel, ref.ThreadTS, info); !errors.As(err, &held) {
			return lock, err
		}
		if err := clock.Sleep(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("the waiter on this thread did not exit after being stopped")
}

// stableHolder returns the lock's live holder, or nil if nobody holds it.
// A holder writes its pid just after taking the lock, so a pid is trusted
// only when the lock is held and reads the same, non-zero, 100ms apart:
// never a previous holder's leftover.
func stableHolder(ctx context.Context, store *Store, ref thread.Ref, clock Clock) (*LockInfo, error) {
	var prev *LockInfo
	for i := 0; i < 20; i++ {
		cur, err := store.Holder(ref.Channel, ref.ThreadTS)
		if err != nil || cur == nil {
			return nil, err
		}
		if prev != nil && cur.PID > 0 && cur.PID == prev.PID {
			return cur, nil
		}
		prev = cur
		if err := clock.Sleep(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("a waiter holds this thread but its pid is unreadable; retry in a moment")
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

func (e *errorBudget) maxDelay() time.Duration {
	if e.interval > maxBackoff {
		return e.interval
	}
	return maxBackoff
}

// fail records err and returns how long to wait, or the error to give up
// with (errMaxRun when the next attempt would land past --max-run).
func (e *errorBudget) fail(err error, now time.Time) (time.Duration, error) {
	if !slack.IsTransient(err) {
		return 0, err
	}
	if !e.last.IsZero() && now.Sub(e.last) > 2*e.maxDelay()+e.interval {
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
	for i := 1; i < e.failures && delay < e.maxDelay(); i++ {
		delay *= 2
	}
	if delay > e.maxDelay() {
		delay = e.maxDelay()
	}
	if !e.deadline.IsZero() && now.Add(delay).After(e.deadline) {
		return 0, errMaxRun
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
// to exit. It signals only a pid that a currently-held lock reports
// consistently (see stableHolder), never a previous holder's leftover.
func Stop(ctx context.Context, store *Store, rawURL string, terminate func(int) error, clock Clock) (string, error) {
	ref, err := thread.ParseURL(rawURL)
	if err != nil {
		return "", err
	}
	if ref.ThreadTS == "" {
		return "", errors.New("stop needs a thread URL")
	}
	holder, err := stableHolder(ctx, store, ref, clock)
	if err != nil {
		return "", err
	}
	if holder == nil {
		return "no waiter is watching this thread", nil
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
