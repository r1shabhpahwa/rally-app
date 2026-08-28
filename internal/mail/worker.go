package mail

import (
	"context"
	"log/slog"
	"time"

	"badminton/internal/model"
	"badminton/internal/store"
)

// Retry schedule. After the last entry is exhausted the message is marked
// failed and shows up in the delivery log for the organizer to retry by hand.
var backoff = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
}

const maxAttempts = 5

// Worker drains the email outbox. One goroutine, one message at a time, rate
// limited: on a single core that is plenty for a 32-person mailing list, and it
// keeps the app well under any provider's throttle.
type Worker struct {
	store  *store.Store
	sender Sender
	log    *slog.Logger
	gap    time.Duration // minimum spacing between sends
	wake   chan struct{} // nudged on enqueue so mail goes out promptly
	tick   time.Duration // fallback poll interval for retries
}

// NewWorker builds the outbox worker. ratePerSecond controls send spacing.
func NewWorker(s *store.Store, sender Sender, log *slog.Logger, ratePerSecond float64) *Worker {
	if ratePerSecond <= 0 {
		ratePerSecond = 1
	}
	return &Worker{
		store:  s,
		sender: sender,
		log:    log,
		gap:    time.Duration(float64(time.Second) / ratePerSecond),
		wake:   make(chan struct{}, 1),
		tick:   10 * time.Second,
	}
}

// Wake asks the worker to check the queue now instead of waiting for the next
// tick, so clicking "Send Invitation" produces mail within a second.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// Run drains the queue until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	timer := time.NewTicker(w.tick)
	defer timer.Stop()
	for {
		w.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-timer.C:
		}
	}
}

func (w *Worker) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		msgs, err := w.store.ClaimDue(ctx, time.Now().Unix(), 20)
		if err != nil {
			w.log.Error("outbox: claim failed", "err", err)
			return
		}
		if len(msgs) == 0 {
			return
		}
		for _, m := range msgs {
			if ctx.Err() != nil {
				return
			}
			w.deliver(ctx, m)
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.gap):
			}
		}
	}
}

func (w *Worker) deliver(ctx context.Context, m model.OutboxMessage) {
	err := w.sender.Send(ctx, m)
	if err == nil {
		if err := w.store.MarkSent(ctx, m.ID); err != nil {
			w.log.Error("outbox: mark sent failed", "id", m.ID, "err", err)
		}
		w.log.Info("email sent", "to", m.ToEmail, "kind", m.Kind, "id", m.ID)
		return
	}

	attempts := m.Attempts + 1
	if IsPermanent(err) || attempts >= maxAttempts {
		if ferr := w.store.MarkFailed(ctx, m.ID, err.Error()); ferr != nil {
			w.log.Error("outbox: mark failed errored", "id", m.ID, "err", ferr)
		}
		w.log.Error("email permanently failed", "to", m.ToEmail, "kind", m.Kind, "id", m.ID, "attempts", attempts, "err", err)
		return
	}

	delay := backoff[min(m.Attempts, len(backoff)-1)]
	next := time.Now().Add(delay).Unix()
	if rerr := w.store.MarkRetry(ctx, m.ID, next, err.Error()); rerr != nil {
		w.log.Error("outbox: mark retry errored", "id", m.ID, "err", rerr)
	}
	w.log.Warn("email send failed, will retry", "to", m.ToEmail, "id", m.ID, "attempt", attempts, "retry_in", delay, "err", err)
}
