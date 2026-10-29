package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Notifications contain no request data. Each subscriber has a single slot;
// slow dashboards coalesce changes without blocking the accounting worker.
type liveSubscription struct {
	userID  int64
	admin   bool
	changes chan struct{}
}
type liveHub struct {
	mu          sync.Mutex
	subscribers map[*liveSubscription]struct{}
	closed      bool
}

func (h *liveHub) subscribe(userID int64, admin bool) (*liveSubscription, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	if h.subscribers == nil {
		h.subscribers = make(map[*liveSubscription]struct{})
	}
	s := &liveSubscription{userID: userID, admin: admin, changes: make(chan struct{}, 1)}
	h.subscribers[s] = struct{}{}
	return s, true
}
func (h *liveHub) unsubscribe(s *liveSubscription) {
	h.mu.Lock()
	delete(h.subscribers, s)
	h.mu.Unlock()
}
func (h *liveHub) notify(owners map[int64]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subscribers {
		if s.admin || owners[s.userID] {
			select {
			case s.changes <- struct{}{}:
			default:
			}
		}
	}
}
func (h *liveHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subscribers {
		close(s.changes)
		delete(h.subscribers, s)
	}
}
func (a *App) notifyUser(id int64) { a.live.notify(map[int64]bool{id: true}) }
func (a *App) notifyKeys(ids ...any) {
	if len(ids) == 0 {
		return
	}
	// Anonymous records have no owner. Wake administrators without exposing
	// their activity to regular users or inventing a proxy key.
	keys := make([]any, 0, len(ids))
	for _, id := range ids {
		if id != nil {
			keys = append(keys, id)
		}
	}
	if len(keys) == 0 {
		a.live.notify(nil)
		return
	}
	ids = keys
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, e := a.db.Query(`SELECT DISTINCT user_id FROM keys WHERE id IN (`+placeholders+`)`, ids...)
	if e != nil {
		return
	}
	owners := map[int64]bool{}
	for rows.Next() {
		var id sql.NullInt64
		if rows.Scan(&id) == nil {
			owners[id.Int64] = true
		}
	}
	rows.Close()
	a.live.notify(owners)
}

// Recheck bearer or cookie access before every change and heartbeat, including
// logout, expired sessions, user disabling, and password resets.
func (a *App) liveEvents(w http.ResponseWriter, r *http.Request, userID int64, admin bool) {
	sub, ok := a.live.subscribe(userID, admin)
	if !ok {
		fail(w, 503, "service stopping")
		return
	}
	defer a.live.unsubscribe(sub)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	control := http.NewResponseController(w)
	send := func(event string) bool {
		_ = control.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, e := fmt.Fprintf(w, "event: %s\ndata: {}\n\n", event); e != nil {
			return false
		}
		return control.Flush() == nil
	}
	if !send("ready") {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		event := "change"
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-sub.changes:
			if !ok {
				return
			}
		case <-heartbeat.C:
			event = "heartbeat"
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		identity, err := a.authenticate(r.WithContext(ctx))
		cancel()
		if err != nil || identity.ID != userID || identity.Admin != admin {
			send("unauthorized")
			return
		}
		if !send(event) {
			return
		}
	}
}
