package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type Sink interface {
	Container(id string, c *Container)
	Synced(running map[string]bool)
	Event(Event)
}

type Watcher struct {
	Client              *Client
	Sink                Sink
	Log                 *slog.Logger
	Backoff, MaxBackoff time.Duration
	Attempts            int
}

const (
	DefaultBackoff    = 100 * time.Millisecond
	DefaultMaxBackoff = 5 * time.Second
	DefaultAttempts   = 12
)

func (w *Watcher) delay(failure int) time.Duration {
	first, most := w.Backoff, w.MaxBackoff
	if first <= 0 {
		first = DefaultBackoff
	}
	if most <= 0 {
		most = DefaultMaxBackoff
	}
	d := first
	for i := 1; i < failure && d < most; i++ {
		d *= 2
	}
	return min(d, most)
}

const queueLen = 4096

func (w *Watcher) Run(ctx context.Context) error {
	if w.Log == nil {
		w.Log = slog.Default()
	}
	attempts := w.Attempts
	if attempts == 0 {
		attempts = DefaultAttempts
	}
	failures := 0
	for {
		synced, err := w.watch(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if synced {
			failures = 0
		}
		failures++
		if failures >= attempts {
			return fmt.Errorf("the Docker engine's events could not be followed after %d attempts: %w", failures, err)
		}
		delay := w.delay(failures)
		w.Log.Warn("docker events: the stream ended; connecting again", "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

var errBehind = errors.New("events arrived faster than they could be handled")

func (w *Watcher) watch(ctx context.Context) (synced bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := w.Client.Events(ctx)
	if err != nil {
		return false, err
	}
	defer stream.Close()

	queue := make(chan Event, queueLen)
	readErr := make(chan error, 1)
	go func() {
		for {
			e, err := stream.Next()
			if err != nil {
				readErr <- err
				return
			}
			select {
			case queue <- e:
			default:
				readErr <- errBehind
				return
			}
		}
	}()

	ids, err := w.running(ctx)
	if err != nil {
		return false, err
	}
	running := make(map[string]bool, len(ids))
	for _, id := range ids {
		on, err := w.refresh(ctx, id)
		if err != nil {
			return false, err
		}
		if on {
			running[id] = true
		}
	}
	w.Sink.Synced(running)
	w.Client.Close()
	w.Log.Debug("docker events: following", "running", len(running))

	for {
		select {
		case e := <-queue:
			w.Sink.Event(e)
			if err := w.react(ctx, e); err != nil {
				return true, err
			}
			if len(queue) == 0 {
				w.Client.Close()
			}
		case err := <-readErr:
			for len(queue) > 0 {
				e := <-queue
				w.Sink.Event(e)
				if w.react(ctx, e) != nil {
					break
				}
			}
			return true, err
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
}

func (w *Watcher) running(ctx context.Context) ([]string, error) {
	return w.Client.Running(ctx)
}

func (w *Watcher) react(ctx context.Context, e Event) error {
	switch e.Type {
	case TypeContainer:
		switch e.Action {
		case "die", "destroy":
			if ValidID(e.ID) {
				w.Sink.Container(e.ID, nil)
			}
		case "start", "restart", "unpause", "rename":
			_, err := w.refresh(ctx, e.ID)
			return err
		}
	case TypeNetwork:
		if e.Action == "connect" || e.Action == "disconnect" {
			_, err := w.refresh(ctx, e.Attributes["container"])
			return err
		}
	}
	return nil
}

func skippable(err error) bool {
	var se *StatusError
	return errors.As(err, &se)
}

func (w *Watcher) refresh(ctx context.Context, id string) (bool, error) {
	if !ValidID(id) {
		return false, nil
	}
	c, err := w.Client.Inspect(ctx, id)
	switch {
	case NotFound(err):
		w.Sink.Container(id, nil)
		return false, nil
	case skippable(err):
		w.Log.Warn("docker events: a container could not be inspected and is skipped", "container", id, "err", err)
		return false, nil
	case err != nil:
		return false, err
	case !c.Running:
		w.Sink.Container(id, nil)
		return false, nil
	}
	c.ID = id
	w.Sink.Container(id, &c)
	return true, nil
}
