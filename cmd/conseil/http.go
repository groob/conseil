package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type api struct {
	store                *store
	worker               *worker
	hub                  *eventHub
	logger               *slog.Logger
	agentName            string
	model                string
	instructions         string
	ownerEmail           string
	allowUnauthenticated bool
}

func (a *api) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.Handle("GET /v1/conversations", a.authorize(http.HandlerFunc(a.listConversations)))
	mux.Handle("POST /v1/conversations", a.authorize(http.HandlerFunc(a.createConversation)))
	mux.Handle("GET /v1/conversations/{conversationID}", a.authorize(http.HandlerFunc(a.getConversation)))
	mux.Handle("POST /v1/conversations/{conversationID}/messages", a.authorize(http.HandlerFunc(a.createMessage)))
	mux.Handle("GET /v1/runs/{runID}/events", a.authorize(http.HandlerFunc(a.streamRunEvents)))
	return securityHeaders(mux)
}

func (a *api) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.allowUnauthenticated {
			next.ServeHTTP(w, r)
			return
		}
		email := strings.TrimSpace(r.Header.Get("X-ExeDev-Email"))
		if email == "" || !strings.EqualFold(email, a.ownerEmail) {
			writeError(w, http.StatusForbidden, "access denied")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.db.PingContext(r.Context()); err != nil {
		a.logger.Error("database health check", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *api) listConversations(w http.ResponseWriter, r *http.Request) {
	conversations, err := a.store.listConversations(r.Context())
	if err != nil {
		a.internalError(w, "listing conversations", err)
		return
	}
	if conversations == nil {
		conversations = []conversation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": conversations})
}

func (a *api) createConversation(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	c, _, err := a.store.createConversation(r.Context(), request.Title)
	if err != nil {
		a.internalError(w, "creating conversation", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"conversation": c})
}

func (a *api) getConversation(w http.ResponseWriter, r *http.Request) {
	conversationID := r.PathValue("conversationID")
	c, events, activeRuns, err := a.store.conversationSnapshot(r.Context(), conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	}
	if err != nil {
		a.internalError(w, "getting conversation snapshot", err)
		return
	}
	if events == nil {
		events = []event{}
	}
	if activeRuns == nil {
		activeRuns = []run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conversation": c,
		"events":       events,
		"active_runs":  activeRuns,
	})
}

func (a *api) createMessage(w http.ResponseWriter, r *http.Request) {
	conversationID := r.PathValue("conversationID")
	if _, err := a.store.conversation(r.Context(), conversationID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	} else if err != nil {
		a.internalError(w, "getting conversation", err)
		return
	}

	var request struct {
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Content = strings.TrimSpace(request.Content)
	if request.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	if !utf8.ValidString(request.Content) {
		writeError(w, http.StatusBadRequest, "content must be valid UTF-8")
		return
	}
	if len(request.Content) > 100_000 {
		writeError(w, http.StatusRequestEntityTooLarge, "content exceeds 100000 bytes")
		return
	}

	run, events, err := a.store.enqueueRun(r.Context(), conversationID, request.Content, a.agentName, "", a.model, a.instructions)
	if err != nil {
		a.internalError(w, "queueing run", err)
		return
	}
	for _, e := range events {
		a.hub.publish(e.RunID)
	}
	a.worker.notify()
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

func (a *api) streamRunEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	if _, err := a.store.run(r.Context(), runID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	} else if err != nil {
		a.internalError(w, "getting run", err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		a.internalError(w, "starting event stream", errors.New("response writer cannot flush"))
		return
	}
	after, err := lastEventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	updates, unsubscribe := a.hub.subscribe(runID)
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		currentRun, err := a.store.run(r.Context(), runID)
		if err != nil {
			a.logger.Error("checking streamed run", "run_id", runID, "error", err)
			return
		}
		terminal := currentRun.Status == "completed" || currentRun.Status == "failed"
		events, err := a.store.eventsForRunAfter(r.Context(), runID, after)
		if err != nil {
			a.logger.Error("reading run event stream", "run_id", runID, "error", err)
			return
		}
		for _, e := range events {
			encoded, err := json.Marshal(e)
			if err != nil {
				a.logger.Error("encoding run event", "run_id", runID, "event_id", e.ID, "error", err)
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, encoded); err != nil {
				return
			}
			after = e.ID
			if e.Type == "run.completed" || e.Type == "run.failed" || e.Type == "run.interrupted" {
				terminal = true
			}
			flusher.Flush()
		}
		if terminal {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-updates:
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func lastEventID(r *http.Request) (int64, error) {
	value := r.Header.Get("Last-Event-ID")
	if queryValue := r.URL.Query().Get("after"); queryValue != "" {
		value = queryValue
	}
	if value == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 0 {
		return 0, errors.New("last event ID must be a non-negative integer")
	}
	return id, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON object")
		return false
	}
	return true
}

func (a *api) internalError(w http.ResponseWriter, operation string, err error) {
	a.logger.Error(operation, "error", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("writing JSON response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
