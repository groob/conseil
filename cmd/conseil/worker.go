package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type modelRunner interface {
	stream(context.Context, []chatMessage, emitEvent) (string, error)
}

type worker struct {
	store      *store
	runner     modelRunner
	hub        *eventHub
	logger     *slog.Logger
	runTimeout time.Duration
	wake       chan struct{}
}

func newWorker(store *store, runner modelRunner, hub *eventHub, logger *slog.Logger, runTimeout time.Duration) *worker {
	return &worker{
		store:      store,
		runner:     runner,
		hub:        hub,
		logger:     logger,
		runTimeout: runTimeout,
		wake:       make(chan struct{}, 1),
	}
}

func (w *worker) notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *worker) run(ctx context.Context) error {
	retry := time.NewTicker(time.Second)
	defer retry.Stop()

	for {
		r, startedEvent, ok, err := w.store.claimNextRun(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.Error("claiming queued run", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-retry.C:
			}
			continue
		}
		if !ok {
			select {
			case <-ctx.Done():
				return nil
			case <-w.wake:
			case <-retry.C:
			}
			continue
		}

		w.hub.publish(startedEvent.RunID)
		w.process(ctx, r)
	}
}

func (w *worker) process(ctx context.Context, r run) {
	messages, err := w.store.messagesForRun(ctx, r)
	if err != nil {
		w.recordFailure(ctx, r, fmt.Errorf("loading conversation messages: %w", err))
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, w.runTimeout)
	defer cancel()

	emit := func(eventType string, payload any) error {
		e, err := w.store.appendRunEvent(runCtx, r, eventType, payload)
		if err != nil {
			return err
		}
		w.hub.publish(e.RunID)
		return nil
	}

	content, err := w.runner.stream(runCtx, messages, emit)
	if err != nil {
		w.recordFailure(ctx, r, err)
		return
	}

	w.complete(ctx, r, content)
}

func (w *worker) complete(ctx context.Context, r run, content string) {
	retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	retry := time.NewTicker(250 * time.Millisecond)
	defer retry.Stop()

	var completionErr error
	for attempt := 1; ; attempt++ {
		events, err := w.store.completeRun(retryCtx, r, content)
		if err == nil {
			for _, e := range events {
				w.hub.publish(e.RunID)
			}
			return
		}
		completionErr = err
		if attempt == 1 {
			w.logger.Warn("retrying run completion", "run_id", r.ID, "error", err)
		}
		select {
		case <-ctx.Done():
			w.recordFailureWithOutput(ctx, r, fmt.Errorf("persisting completed model output: %w", completionErr), content)
			return
		case <-retryCtx.Done():
			w.recordFailureWithOutput(ctx, r, fmt.Errorf("persisting completed model output: %w", completionErr), content)
			return
		case <-retry.C:
		}
	}
}

func (w *worker) recordFailure(ctx context.Context, r run, runErr error) {
	w.recordFailureWithOutput(ctx, r, runErr, "")
}

func (w *worker) recordFailureWithOutput(ctx context.Context, r run, runErr error, output string) {
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	e, err := w.store.failRun(persistCtx, r, runErr, output)
	if err != nil {
		w.logger.Error("recording failed run", "run_id", r.ID, "run_error", runErr, "error", err)
		return
	}
	w.hub.publish(e.RunID)
}

func persistenceContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}

type eventHub struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[string]map[uint64]chan struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subscribers: make(map[string]map[uint64]chan struct{})}
}

func (h *eventHub) subscribe(runID string) (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	id := h.nextID
	updates := make(chan struct{}, 1)
	if h.subscribers[runID] == nil {
		h.subscribers[runID] = make(map[uint64]chan struct{})
	}
	h.subscribers[runID][id] = updates

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subscribers[runID], id)
			if len(h.subscribers[runID]) == 0 {
				delete(h.subscribers, runID)
			}
		})
	}
	return updates, unsubscribe
}

func (h *eventHub) publish(runID string) {
	if runID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, updates := range h.subscribers[runID] {
		select {
		case updates <- struct{}{}:
		default:
		}
	}
}
