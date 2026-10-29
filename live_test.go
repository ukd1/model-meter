package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func eventClient(t *testing.T, server *httptest.Server, token string) (<-chan string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/admin/events", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	response, e := server.Client().Do(r)
	if e != nil {
		cancel()
		t.Fatal(e)
	}
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		response.Body.Close()
		cancel()
		t.Fatal("stream did not connect", response.StatusCode)
	}
	events := make(chan string, 32)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				select {
				case events <- strings.TrimPrefix(line, "event: "):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return events, func() { cancel(); response.Body.Close() }
}
func expectEvent(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok || got != want {
			t.Fatalf("event %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", want)
	}
}
func TestLiveCommittedAccounting(t *testing.T) {
	a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	events, stop := eventClient(t, s, "admin-secret")
	defer stop()
	expectEvent(t, events, "ready")
	res := call(t, s, "/v1/models", key, "")
	io.Copy(io.Discard, res.Body)
	expectEvent(t, events, "change")
	var count int
	a.db.QueryRow(`SELECT count(*) FROM requests`).Scan(&count)
	if count != 1 {
		t.Fatal("event preceded accounting commit")
	}
	a.live.close()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("stream not closed")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown left stream open")
	}
}
func TestLiveUserScopeAndRevocation(t *testing.T) {
	a, s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, name := range []string{"alice", "bob"} {
		if _, e := a.db.Exec(`INSERT INTO users(name,hash,created) VALUES(?,?,?)`, name, hash(name), time.Now().Format(time.RFC3339)); e != nil {
			t.Fatal(e)
		}
	}
	alice, stopAlice := eventClient(t, s, "alice")
	defer stopAlice()
	bob, stopBob := eventClient(t, s, "bob")
	defer stopBob()
	admin, stopAdmin := eventClient(t, s, "admin-secret")
	defer stopAdmin()
	for _, ch := range []<-chan string{alice, bob, admin} {
		expectEvent(t, ch, "ready")
	}
	res := call(t, s, "/admin/keys", "alice", `{"name":"Alice key"}`)
	io.Copy(io.Discard, res.Body)
	expectEvent(t, alice, "change")
	expectEvent(t, admin, "change")
	select {
	case e := <-bob:
		t.Fatal("other user's activity leaked", e)
	case <-time.After(50 * time.Millisecond):
	}
	res = call(t, s, "/admin/users/update", "admin-secret", `{"id":1,"rotate":true}`)
	io.Copy(io.Discard, res.Body)
	expectEvent(t, alice, "unauthorized")
	select {
	case _, ok := <-alice:
		if ok {
			t.Fatal("revoked stream still open")
		}
	case <-time.After(time.Second):
		t.Fatal("revoked stream did not close")
	}
}
func TestLiveRejectsInvalidAuthentication(t *testing.T) {
	_, s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {})
	res := call(t, s, "/admin/events", "invalid", "")
	if res.StatusCode != 401 {
		t.Fatal(res.StatusCode)
	}
	if res.Header.Get("Content-Type") == "text/event-stream" {
		t.Fatal("unauthenticated event stream")
	}
}
func TestLiveSlowSubscribersCoalesce(t *testing.T) {
	h := &liveHub{}
	alice, _ := h.subscribe(1, false)
	bob, _ := h.subscribe(2, false)
	admin, _ := h.subscribe(0, true)
	start := time.Now()
	for i := 0; i < 10000; i++ {
		h.notify(map[int64]bool{1: true})
	}
	if time.Since(start) > time.Second {
		t.Fatal("notifications blocked on slow subscriber")
	}
	if len(alice.changes) != 1 || len(admin.changes) != 1 || len(bob.changes) != 0 {
		t.Fatal("coalescing or scope failed")
	}
	h.close()
	h.notify(map[int64]bool{1: true})
	if _, ok := h.subscribe(1, false); ok {
		t.Fatal("subscribed to closed hub")
	}
}
