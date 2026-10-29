package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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
	a := &App{db: db, upstream: up.URL, client: up.Client()}
	encoded, err := passwordHash("admin-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,name,username,password_hash,hash,admin,created) VALUES(0,'Administrator','admin',?,'legacy-admin',1,?)`, encoded, time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
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
	if strings.HasPrefix(path, "/admin/") {
		if cookie := testDashboardCookie(t, s, key); cookie != nil {
			r.AddCookie(cookie)
		}
		r.Header.Set("X-Model-Meter", "dashboard")
	} else {
		r.Header.Set("Authorization", "Bearer "+key)
	}
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

func TestUserIsolation(t *testing.T) {
	a, s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	})
	createUser := func(name string) (string, int) {
		res := call(t, s, "/admin/users", "admin-secret", `{"name":"`+name+`","username":"`+strings.ToLower(name)+`","password":"`+name+`-password-123"}`)
		var v struct {
			ID int
		}
		json.NewDecoder(res.Body).Decode(&v)
		if res.StatusCode != 201 {
			t.Fatal("user creation failed")
		}
		return strings.ToLower(name) + ":" + name + "-password-123", v.ID
	}
	alice, aliceID := createUser("Alice")
	bob, _ := createUser("Bob")
	res := call(t, s, "/admin/keys", alice, `{"name":"Alice key","user_id":999}`)
	var key struct {
		Key string
		ID  int
	}
	json.NewDecoder(res.Body).Decode(&key)
	if key.Key == "" {
		t.Fatal("user key failed")
	}
	if call(t, s, "/admin/revoke", bob, `{"id":`+fmt.Sprint(key.ID)+`}`).StatusCode != 404 {
		t.Fatal("cross-user revoke")
	}
	if call(t, s, "/admin/key-update", bob, `{"id":`+fmt.Sprint(key.ID)+`,"name":"stolen"}`).StatusCode != 404 {
		t.Fatal("cross-user rename")
	}
	if call(t, s, "/admin/users", bob, "").StatusCode != 403 {
		t.Fatal("user can list other users")
	}
	res = call(t, s, "/v1/chat/completions", key.Key, `{"model":"test"}`)
	io.Copy(io.Discard, res.Body)
	awaitRows(t, a, 1)
	for _, endpoint := range []string{"keys", "usage", "requests", "timeseries"} {
		res = call(t, s, "/admin/"+endpoint+"?user="+fmt.Sprint(aliceID), bob, "")
		body, _ := io.ReadAll(res.Body)
		if strings.TrimSpace(string(body)) != "[]" {
			t.Fatalf("cross-user %s: %s", endpoint, body)
		}
	}
	if call(t, s, "/admin/profile", alice, `{"name":"Alice new"}`).StatusCode != 200 {
		t.Fatal("profile update")
	}
	if call(t, s, "/admin/users/update", "admin-secret", `{"id":`+fmt.Sprint(aliceID)+`,"disabled":true}`).StatusCode != 200 {
		t.Fatal("disable failed")
	}
	if call(t, s, "/v1/models", key.Key, "").StatusCode != 401 {
		t.Fatal("disabled user's key accepted")
	}
	if call(t, s, "/admin/me", alice, "").StatusCode != 401 {
		t.Fatal("disabled login accepted")
	}
}

var testSessionCookies sync.Map

func testDashboardCookie(t *testing.T, s *httptest.Server, credentials string) *http.Cookie {
	t.Helper()
	cacheKey := s.URL + ":" + credentials
	if c, ok := testSessionCookies.Load(cacheKey); ok {
		return c.(*http.Cookie)
	}
	username, password := credentials, credentials
	if credentials == "admin-secret" {
		username = "admin"
	}
	if parts := strings.SplitN(credentials, ":", 2); len(parts) == 2 {
		username, password = parts[0], parts[1]
	}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	res := sessionRequest(t, s, "POST", "/auth/login", string(body), nil, true, "")
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil
	}
	for _, c := range res.Cookies() {
		if c.MaxAge > 0 {
			testSessionCookies.Store(cacheKey, c)
			t.Cleanup(func() { testSessionCookies.Delete(cacheKey) })
			return c
		}
	}
	t.Fatal("missing session cookie")
	return nil
}
func seedTestUser(t *testing.T, a *App, name, password string) int64 {
	t.Helper()
	encoded, err := passwordHash(password)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO users(name,username,password_hash,hash,created) VALUES(?,?,?,?,?)`, name, strings.ToLower(name), encoded, hash("legacy-"+name), time.Now().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return id
}
