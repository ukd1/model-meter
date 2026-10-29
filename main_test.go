package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, upstream http.HandlerFunc) (*App, *httptest.Server, string) {
	t.Helper()
	up := httptest.NewServer(upstream)
	db, e := openDB(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	a := &App{db: db, admin: "admin-secret", upstream: up.URL, client: up.Client()}
	a.startLogger()
	_, e = db.Exec(`INSERT INTO keys(name,hash,prefix,created) VALUES(?,?,?,?)`, "test", hash("client-secret"), "client", time.Now().Format(time.RFC3339))
	if e != nil {
		t.Fatal(e)
	}
	s := httptest.NewServer(a.handler())
	t.Cleanup(func() { s.Close(); a.stopLogger(); db.Close(); up.Close() })
	return a, s, "client-secret"
}
func call(t *testing.T, s *httptest.Server, path, key, body string) *http.Response {
	t.Helper()
	method := "GET"
	if body != "" {
		method = "POST"
	}
	r, _ := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	res, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}
func awaitRows(t *testing.T, a *App, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		a.db.QueryRow("SELECT count(*) FROM requests").Scan(&count)
		if count == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("accounting did not flush")
}
func TestProxyAccountingPrivacyAndRevocation(t *testing.T) {
	a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("client credential leaked upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"test-model","choices":[{"message":{"content":"PRIVATE RESPONSE"}}],"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`)
	})
	res := call(t, s, "/v1/chat/completions?secret=PRIVATE", key, `{"model":"test-model","messages":[{"role":"user","content":"PRIVATE PROMPT"}]}`)
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(b, []byte("PRIVATE RESPONSE")) {
		t.Fatal("response changed")
	}
	awaitRows(t, a, 1)
	var in, out, cached int
	var path string
	if e := a.db.QueryRow("SELECT input_tokens,output_tokens,cached_tokens,path FROM requests").Scan(&in, &out, &cached, &path); e != nil || in != 12 || out != 7 || cached != 4 || path != "/v1/chat/completions" {
		t.Fatal(in, out, cached, path, e)
	}
	rr := call(t, s, "/admin/requests", "admin-secret", "")
	data, _ := io.ReadAll(rr.Body)
	if bytes.Contains(data, []byte("PRIVATE")) || bytes.Contains(data, []byte(key)) {
		t.Fatal("body or secret persisted")
	}
	if call(t, s, "/admin/keys", key, "").StatusCode != 401 {
		t.Fatal("admin access not protected")
	}
	if call(t, s, "/admin/revoke", "admin-secret", `{"id":1}`).StatusCode != 200 {
		t.Fatal("revoke failed")
	}
	if call(t, s, "/v1/models", key, "").StatusCode != 401 {
		t.Fatal("revoked key accepted")
	}
}
func TestStreamUsageAndFlush(t *testing.T) {
	a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		json.NewDecoder(r.Body).Decode(&v)
		if v["stream_options"].(map[string]any)["include_usage"] != true {
			t.Error("usage not requested")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
		io.WriteString(w, "data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	})
	res := call(t, s, "/v1/chat/completions", key, `{"model":"m","stream":true}`)
	start := time.Now()
	buf := make([]byte, 10)
	res.Body.Read(buf)
	if time.Since(start) > 100*time.Millisecond {
		t.Error("stream buffered")
	}
	io.Copy(io.Discard, res.Body)
	awaitRows(t, a, 1)
	var in, out int
	a.db.QueryRow("SELECT input_tokens,output_tokens FROM requests").Scan(&in, &out)
	if in != 9 || out != 3 {
		t.Fatal(in, out)
	}
}
func TestMeterResponsesAndChunkBoundaries(t *testing.T) {
	m := &Meter{SSE: true}
	for _, b := range []byte("data: {\"response\":{\"model\":\"m\",\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\r\n\r\n") {
		m.Write([]byte{b})
	}
	if m.Input == nil || *m.Input != 8 || *m.Output != 2 {
		t.Fatal(m)
	}
}
func TestMissingUsageAndError(t *testing.T) {
	a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"private error"}}`)
	})
	res := call(t, s, "/v1/chat/completions", key, `{"model":"m"}`)
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != 429 {
		t.Fatal(res.StatusCode)
	}
	awaitRows(t, a, 1)
	var missing bool
	a.db.QueryRow("SELECT input_tokens IS NULL FROM requests").Scan(&missing)
	if !missing {
		t.Fatal("missing usage counted as zero")
	}
}
func TestFullLogQueueDoesNotBlock(t *testing.T) {
	a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	a.stopLogger()
	a.records = make(chan []any, 1)
	a.records <- nil
	start := time.Now()
	res := call(t, s, "/v1/models", key, "")
	io.Copy(io.Discard, res.Body)
	if time.Since(start) > time.Second {
		t.Fatal("full queue blocked response")
	}
	if a.dropped.Load() != 1 {
		t.Fatal("missing dropped count")
	} // Restore a worker for fixture cleanup.
	a.startLogger()
}
