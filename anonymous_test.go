package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func anonymousCall(t *testing.T, s *httptest.Server, method, path, body string, headers http.Header) *http.Response {
	t.Helper()
	r, err := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = headers
	res, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func setAnonymous(t *testing.T, s *httptest.Server, enabled bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]bool{"allow_anonymous": enabled})
	res := call(t, s, "/admin/settings", "admin-secret", string(body))
	if res.StatusCode != 200 {
		t.Fatalf("settings status: %d", res.StatusCode)
	}
}

func TestAnonymousAuthorizationAccountingAndPersistence(t *testing.T) {
	var upstreamCalls atomic.Int64
	a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" {
			t.Error("client credentials forwarded upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"PRIVATE RESPONSE"}}],"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`)
	})
	if got := anonymousCall(t, s, "GET", "/v1/models", "", nil).StatusCode; got != 401 {
		t.Fatalf("anonymous access enabled by default: %d", got)
	}
	setAnonymous(t, s, true)
	for _, headers := range []http.Header{
		{"Authorization": {"Bearer invalid"}},
		{"Authorization": {"Bearer "}},
		{"Authorization": {""}},
		{"Authorization": {"Basic invalid"}},
		{"X-Api-Key": {"invalid"}},
		{"X-Api-Key": {""}},
		{"Authorization": {"Bearer " + key}, "X-Api-Key": {"different"}},
		{"Authorization": {"Bearer " + key, "Bearer different"}},
		{"X-Api-Key": {key, "different"}},
	} {
		if got := anonymousCall(t, s, "GET", "/v1/models", "", headers).StatusCode; got != 401 {
			t.Errorf("provided invalid credentials bypassed validation: %d", got)
		}
	}
	if upstreamCalls.Load() != 0 {
		t.Fatal("rejected credentials reached upstream")
	}
	res := anonymousCall(t, s, "POST", "/v1/chat/completions?secret=PRIVATE", `{"model":"m","messages":[{"role":"user","content":"PRIVATE PROMPT"}]}`, nil)
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != 200 {
		t.Fatal("anonymous inference rejected", res.StatusCode)
	}
	awaitRows(t, a, 1)
	var id sql.NullInt64
	var in, out, cached int
	var path, source string
	if err := a.db.QueryRow(`SELECT key_id,input_tokens,output_tokens,cached_tokens,path,source FROM requests`).Scan(&id, &in, &out, &cached, &path, &source); err != nil || id.Valid || in != 12 || out != 7 || cached != 4 || path != "/v1/chat/completions" || source != "127.0.0.1" {
		t.Fatalf("anonymous accounting: key=%v tokens=%d/%d/%d path=%s source=%s err=%v", id, in, out, cached, path, source, err)
	}
	requests := call(t, s, "/admin/requests?key=anonymous", "admin-secret", "")
	data, _ := io.ReadAll(requests.Body)
	if strings.Contains(string(data), "PRIVATE") || !strings.Contains(string(data), `"key_name":"Anonymous"`) {
		t.Fatal("anonymous request metadata missing or private content persisted")
	}
	var dbPath string
	var seq int
	var name string
	if err := a.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &dbPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := &App{db: reopened, upstream: a.upstream, client: a.client}
	restarted.startLogger()
	defer restarted.stopLogger()
	restartedServer := httptest.NewServer(restarted.handler())
	defer restartedServer.Close()
	if got := anonymousCall(t, restartedServer, "GET", "/v1/models", "", nil).StatusCode; got != 200 {
		t.Fatalf("anonymous setting did not persist: %d", got)
	}
	setAnonymous(t, s, false)
	if got := anonymousCall(t, s, "GET", "/v1/models", "", nil).StatusCode; got != 401 {
		t.Fatalf("disabling anonymous access had no effect: %d", got)
	}
	if got := call(t, s, "/v1/models", key, "").StatusCode; got != 200 {
		t.Fatalf("disabling anonymous mode broke valid keys: %d", got)
	}
}

func TestAnonymousSettingsAuthorizationAndLiveIsolation(t *testing.T) {
	a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	aliceID := seedTestUser(t, a, "Alice", "user-secret")
	adminEvents, _ := a.live.subscribe(0, true)
	defer a.live.unsubscribe(adminEvents)
	aliceEvents, _ := a.live.subscribe(aliceID, false)
	defer a.live.unsubscribe(aliceEvents)
	for _, body := range []string{"", `{"allow_anonymous":true}`} {
		if got := call(t, s, "/admin/settings", "alice:user-secret", body).StatusCode; got != 403 {
			t.Fatalf("user accessed settings: %d", got)
		}
	}
	if got := anonymousCall(t, s, "GET", "/admin/settings", "", nil).StatusCode; got != 401 {
		t.Fatalf("unauthenticated settings access: %d", got)
	}
	for _, body := range []string{`{}`, `{"allow_anonymous":null}`, `{"allow_anonymous":"true"}`, `{"allow_anonymous":true,"unexpected":1}`, `{"allow_anonymous":true}{}`} {
		if got := call(t, s, "/admin/settings", "admin-secret", body).StatusCode; got != 400 {
			t.Fatalf("invalid settings accepted: %d", got)
		}
	}
	adminCookie := sessionLogin(t, s, "admin-secret")
	if got := sessionRequest(t, s, "POST", "/admin/settings", `{"allow_anonymous":true}`, adminCookie, false, "").StatusCode; got != 403 {
		t.Fatalf("settings skipped CSRF validation: %d", got)
	}
	setAnonymous(t, s, true)
	select {
	case <-adminEvents.changes:
	default:
		t.Fatal("administrator missed settings update")
	}
	res := anonymousCall(t, s, "GET", "/v1/models", "", nil)
	io.Copy(io.Discard, res.Body)
	select {
	case <-adminEvents.changes:
		var count int
		a.db.QueryRow(`SELECT count(*) FROM requests WHERE key_id IS NULL`).Scan(&count)
		if count != 1 {
			t.Fatal("anonymous event preceded accounting commit")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("administrator missed anonymous accounting event")
	}
	select {
	case <-aliceEvents.changes:
		t.Fatal("anonymous or settings activity leaked to regular user")
	default:
	}
	if _, err := a.db.Exec(`UPDATE keys SET revoked=1 WHERE hash=?`, hash(key)); err != nil {
		t.Fatal(err)
	}
	if got := call(t, s, "/v1/models", key, "").StatusCode; got != 401 {
		t.Fatalf("revoked key accepted in anonymous mode: %d", got)
	}
	// Cookie-only proxy requests remain anonymous, without attributing usage to
	// the signed-in administrator or conferring access to admin routes.
	if got := sessionRequest(t, s, "GET", "/v1/models", "", adminCookie, false, "").StatusCode; got != 200 {
		t.Fatalf("cookie-only anonymous request rejected: %d", got)
	}
}

func TestAnonymousUsagePerformanceAndUserScope(t *testing.T) {
	a, s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {})
	aliceID := seedTestUser(t, a, "Alice", "user-secret")
	seedTestUser(t, a, "bob", "bob")
	if _, err := a.db.Exec(`UPDATE keys SET user_id=? WHERE id=1`, aliceID); err != nil {
		t.Fatal(err)
	}
	for _, keyID := range []any{nil, int64(1)} {
		if _, err := a.db.Exec(`INSERT INTO requests(key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,cached_tokens,outcome) VALUES(?,?,'m','POST','/v1/chat/completions','127.0.0.1',200,1000,12,7,4,'complete')`, keyID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		credential, query string
		want              int
	}{
		{"admin-secret", "", 2}, {"admin-secret", "?key=anonymous", 1}, {"admin-secret", "?key=1", 1},
		{"alice:user-secret", "", 1}, {"alice:user-secret", "?key=anonymous", 0}, {"bob", "", 0},
		{"admin-secret", "?key=anonymous&user=" + jsonNumber(aliceID), 0},
	} {
		for _, endpoint := range []string{"usage", "requests", "timeseries"} {
			res := call(t, s, "/admin/"+endpoint+tc.query, tc.credential, "")
			var rows []map[string]any
			if err := json.NewDecoder(res.Body).Decode(&rows); err != nil || res.StatusCode != 200 {
				t.Fatalf("%s: status=%d err=%v", endpoint, res.StatusCode, err)
			}
			count := len(rows)
			if endpoint == "timeseries" {
				count = 0
				for _, row := range rows {
					count += int(row["requests"].(float64))
				}
			}
			if count != tc.want {
				t.Fatalf("%s scope %s %s: got %d want %d", endpoint, tc.credential, tc.query, count, tc.want)
			}
			if endpoint == "usage" && tc.query == "?key=anonymous" && tc.want == 1 {
				if rows[0]["key_id"] != nil || rows[0]["user_id"] != nil || rows[0]["name"] != "Anonymous" || rows[0]["input_tokens"] != float64(12) {
					t.Fatal("anonymous usage identity or tokens changed", rows[0])
				}
			}
		}
		res := call(t, s, "/admin/performance"+tc.query, tc.credential, "")
		var stats performanceStats
		if err := json.NewDecoder(res.Body).Decode(&stats); err != nil || res.StatusCode != 200 {
			t.Fatalf("performance: status=%d err=%v", res.StatusCode, err)
		}
		if stats.Latency.Samples != int64(tc.want) || stats.OutputRate.Samples != int64(tc.want) {
			t.Fatalf("performance scope leaked or omitted anonymous usage: %+v", tc)
		}
	}
}

func TestAnonymousStreamingErrorsAndBoundedLogging(t *testing.T) {
	a, s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"message":"PRIVATE ERROR"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":4,\"output_tokens\":0}}}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n")
	})
	setAnonymous(t, s, true)
	for _, request := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/messages", `{"model":"m","stream":true}`, 200},
		{"GET", "/v1/models", "", 429},
		{"POST", "/v1/unknown", `{}`, 405},
	} {
		res := anonymousCall(t, s, request.method, request.path, request.body, nil)
		io.Copy(io.Discard, res.Body)
		if res.StatusCode != request.status {
			t.Fatalf("%s status=%d", request.path, res.StatusCode)
		}
	}
	awaitRows(t, a, 3)
	var input, output, cached int
	if err := a.db.QueryRow(`SELECT input_tokens,output_tokens,cached_tokens FROM requests WHERE path='/v1/messages' AND key_id IS NULL`).Scan(&input, &output, &cached); err != nil || input != 14 || output != 6 || cached != 4 {
		t.Fatalf("anonymous stream usage: %d/%d/%d %v", input, output, cached, err)
	}
	var unmeteredErrors int
	if err := a.db.QueryRow(`SELECT count(*) FROM requests WHERE key_id IS NULL AND status>=400 AND input_tokens IS NULL`).Scan(&unmeteredErrors); err != nil || unmeteredErrors != 2 {
		t.Fatalf("anonymous errors not logged: %d %v", unmeteredErrors, err)
	}
	a.stopLogger()
	a.records = make(chan requestRecord, 1)
	a.records <- requestRecord{}
	started := time.Now()
	res := anonymousCall(t, s, "GET", "/v1/models", "", nil)
	io.Copy(io.Discard, res.Body)
	if time.Since(started) > time.Second || a.dropped.Load() != 1 {
		t.Error("anonymous logging blocked a response or failed to count a dropped record")
	}
	a.startLogger() // Restore a worker for fixture cleanup.
}

func TestAnonymousMigrationPreservesHistoryAndIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE users(id INTEGER PRIMARY KEY,name TEXT NOT NULL,hash TEXT UNIQUE NOT NULL,disabled INTEGER NOT NULL DEFAULT 0,created TEXT NOT NULL);
 CREATE TABLE keys(id INTEGER PRIMARY KEY,user_id INTEGER REFERENCES users(id),name TEXT NOT NULL,hash TEXT UNIQUE NOT NULL,prefix TEXT NOT NULL,created TEXT NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE requests(id INTEGER PRIMARY KEY,key_id INTEGER NOT NULL REFERENCES keys(id),time TEXT NOT NULL,model TEXT NOT NULL,method TEXT NOT NULL,path TEXT NOT NULL,source TEXT NOT NULL,status INTEGER NOT NULL,duration_ms INTEGER NOT NULL,input_tokens INTEGER,output_tokens INTEGER,cached_tokens INTEGER,outcome TEXT NOT NULL);
 CREATE INDEX custom_requests_model ON requests(model);
 INSERT INTO users(id,name,hash,created) VALUES(7,'Existing','hash-user','2026-01-01');
 INSERT INTO keys(id,user_id,name,hash,prefix,created) VALUES(42,7,'Existing key','hash-key','prefix','2026-01-01');
 INSERT INTO requests(id,key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,cached_tokens,outcome) VALUES(99,42,'2026-01-01','old-model','POST','/v1/messages','old-source',200,50,20,3,9,'complete');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id, keyID, owner, cached int
	var model string
	if err := db.QueryRow(`SELECT r.id,r.key_id,k.user_id,r.cached_tokens,r.model FROM requests r JOIN keys k ON k.id=r.key_id`).Scan(&id, &keyID, &owner, &cached, &model); err != nil || id != 99 || keyID != 42 || owner != 7 || cached != 9 || model != "old-model" {
		t.Fatalf("migration lost existing history: %d %d %d %d %s %v", id, keyID, owner, cached, model, err)
	}
	for _, index := range []string{"custom_requests_model", "requests_time", "requests_key"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&count); err != nil || count != 1 {
			t.Fatalf("migration lost index %s: %v", index, err)
		}
	}
	insert := `INSERT INTO requests(key_id,time,model,method,path,source,status,duration_ms,outcome) VALUES(?,'2026-01-01','m','GET','/v1/models','local',200,1,'complete')`
	if _, err := db.Exec(insert, nil); err != nil {
		t.Fatal("migration still requires a key", err)
	}
	if _, err := db.Exec(insert, 1000000); err == nil {
		t.Fatal("migration disabled key foreign-key validation")
	}
	if err := migrateAnonymousRequests(db); err != nil {
		t.Fatal("migration not idempotent", err)
	}
}
