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
	"sync/atomic"
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
	a.records = make(chan requestRecord, 1)
	a.records <- requestRecord{}
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

func TestImportedRestrictionsAndModelAlias(t *testing.T) {
	var calls atomic.Int32
	a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Model    string
			Stream   bool
			Messages []struct{ Content string }
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil || body.Model != "physical-model" ||
			len(body.Messages) != 1 || body.Messages[0].Content != "legacy-alias" {
			t.Error("model alias rewrite changed unrelated content")
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
		}
	})
	a.modelAliases = map[string]string{"legacy-alias": "physical-model"}
	if _, e := a.db.Exec(`UPDATE keys SET allowed_routes='["/v1/chat/completions"]',allowed_models='["legacy-alias"]' WHERE hash=?`, hash(key)); e != nil {
		t.Fatal(e)
	}
	if call(t, s, "/v1/messages", key, `{"model":"legacy-alias"}`).StatusCode != 403 ||
		call(t, s, "/v1/chat/completions", key, `{"model":"other"}`).StatusCode != 403 {
		t.Fatal("imported restrictions were bypassed")
	}
	for _, streamed := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"legacy-alias","messages":[{"content":"legacy-alias"}],"stream":%t}`, streamed)
		res := call(t, s, "/v1/chat/completions", key, body)
		io.Copy(io.Discard, res.Body)
		if res.StatusCode != 200 {
			t.Fatal("permitted alias request failed")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("rejected requests reached upstream")
	}
	if _, e := a.db.Exec("UPDATE keys SET expires=? WHERE hash=?", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), hash(key)); e != nil {
		t.Fatal(e)
	}
	if call(t, s, "/v1/chat/completions", key, `{"model":"legacy-alias"}`).StatusCode != 401 {
		t.Fatal("expired key accepted")
	}
	if _, e := a.db.Exec("UPDATE keys SET expires=NULL,allowed_routes='malformed' WHERE hash=?", hash(key)); e != nil {
		t.Fatal(e)
	}
	if call(t, s, "/v1/chat/completions", key, `{"model":"legacy-alias"}`).StatusCode != 403 {
		t.Fatal("malformed policy allowed access")
	}
	if calls.Load() != 2 {
		t.Fatal("invalid key or policy reached upstream")
	}
}

func TestMCPManagementAndHistory(t *testing.T) {
	a, _, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"data": []any{}})
	})
	secret := strings.Repeat("m", 48)
	call := func(method string, params any, credential, origin string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req := httptest.NewRequest("POST", "http://localhost/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		a.handler().ServeHTTP(rec, req)
		var value map[string]any
		json.Unmarshal(rec.Body.Bytes(), &value)
		return rec.Code, value
	}
	t.Setenv("MCP_ADMIN_TOKEN", "")
	t.Setenv("MCP_ADMIN_USERNAME", "admin")
	if status, _ := call("tools/list", nil, secret, ""); status != 404 {
		t.Fatal("MCP must be opt-in")
	}
	t.Setenv("MCP_ADMIN_TOKEN", secret)
	for _, credential := range []string{"", "wrong", "client-secret"} {
		if status, _ := call("tools/list", nil, credential, ""); status != 401 {
			t.Fatal("MCP accepted non-management credential")
		}
	}
	if status, _ := call("tools/list", nil, secret, "https://evil.example"); status != 403 {
		t.Fatal("cross-origin MCP accepted")
	}
	_, init := call("initialize", map[string]any{"protocolVersion": "2025-06-18"}, secret, "")
	if init["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatal("initialize failed")
	}
	_, tools := call("tools/list", nil, secret, "")
	if len(tools["result"].(map[string]any)["tools"].([]any)) != 5 {
		t.Fatal("tool discovery failed")
	}
	tool := func(name string, args any) map[string]any {
		t.Helper()
		status, v := call("tools/call", map[string]any{"name": name, "arguments": args}, secret, "")
		if status != 200 {
			t.Fatal("MCP failed")
		}
		return v["result"].(map[string]any)
	}
	good := func(name string, args any) map[string]any {
		t.Helper()
		result := tool(name, args)
		if result["isError"] == true {
			t.Fatalf("tool %s failed", name)
		}
		return result["structuredContent"].(map[string]any)
	}
	uid := seedTestUser(t, a, "mcp-user", "user-password-123")
	created := good("create_key", map[string]any{"name": "Agent key", "user_id": uid})["created"].(map[string]any)
	id := int64(created["id"].(float64))
	key := created["key"].(string)
	var owner int64
	if err := a.db.QueryRow("SELECT user_id FROM keys WHERE id=?", id).Scan(&owner); err != nil || owner != uid {
		t.Fatal("key owner not preserved")
	}
	for _, v := range []struct {
		age    time.Duration
		tokens int
	}{{10 * time.Minute, 100}, {2 * time.Hour, 1000}} {
		_, err := a.db.Exec("INSERT INTO requests(key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,outcome) VALUES(?,?,'test','POST','/v1/chat/completions','test',200,1000,?,20,'complete')", id, time.Now().UTC().Add(-v.age).Format(time.RFC3339Nano), v.tokens)
		if err != nil {
			t.Fatal(err)
		}
	}
	checkUsage := func(window string, want float64) {
		t.Helper()
		stats := good("get_stats", map[string]any{"window": window, "key_id": id})
		usage := stats["usage"].([]any)
		if len(usage) != 1 || usage[0].(map[string]any)["input_tokens"] != want {
			t.Fatalf("wrong usage for %s", window)
		}
	}
	checkUsage("1h", 100)
	checkUsage("all", 1100)
	good("revoke_key", map[string]any{"key_id": id})
	active := good("list_keys", map[string]any{})["keys"].([]any)
	for _, v := range active {
		if int64(v.(map[string]any)["id"].(float64)) == id {
			t.Fatal("revoked key visible by default")
		}
	}
	audit := good("list_keys", map[string]any{"include_revoked": true})["keys"].([]any)
	found := false
	for _, v := range audit {
		if int64(v.(map[string]any)["id"].(float64)) == id {
			found = true
		}
	}
	if !found {
		t.Fatal("revoked record deleted")
	}
	checkUsage("all", 1100)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	a.handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("revoked key accepted")
	}
	for _, test := range []struct {
		name string
		args any
	}{
		{"get_stats", map[string]any{"window": "nonsense"}},
		{"create_key", map[string]any{"user_id": 0, "name": "bad"}},
		{"revoke_key", map[string]any{"key_id": id, "unexpected": true}},
		{"get_stats", map[string]any{"key_id": nil}},
	} {
		if tool(test.name, test.args)["isError"] != true {
			t.Fatal("invalid tool arguments accepted")
		}
	}
	if _, err := a.db.Exec("UPDATE users SET admin=0 WHERE username='admin'"); err != nil {
		t.Fatal(err)
	}
	if status, _ := call("tools/list", nil, secret, ""); status != 403 {
		t.Fatal("demoted admin still accepted")
	}
	if _, err := a.db.Exec("UPDATE users SET admin=1,disabled=1 WHERE username='admin'"); err != nil {
		t.Fatal(err)
	}
	if status, _ := call("tools/list", nil, secret, ""); status != 403 {
		t.Fatal("disabled admin still accepted")
	}
}
