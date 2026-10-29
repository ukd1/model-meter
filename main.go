package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed index.html
var page []byte

//go:embed dashboard.js
var dashboardJS []byte

//go:embed dashboard.css
var dashboardCSS []byte

type App struct {
	db          *sql.DB
	upstream    string
	upstreamKey string
	admin       string
	client      *http.Client
	records     chan []any
	loggerWG    sync.WaitGroup
	dropped     atomic.Uint64
	live        liveHub
}

func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func openDB(path string) (*sql.DB, error) {
	db, e := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(8)
	_, e = db.Exec(`CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY,name TEXT NOT NULL,hash TEXT UNIQUE NOT NULL,disabled INTEGER NOT NULL DEFAULT 0,created TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS keys(id INTEGER PRIMARY KEY,user_id INTEGER REFERENCES users(id),name TEXT NOT NULL,hash TEXT UNIQUE NOT NULL,prefix TEXT NOT NULL,created TEXT NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS requests(id INTEGER PRIMARY KEY,key_id INTEGER NOT NULL REFERENCES keys(id),time TEXT NOT NULL,model TEXT NOT NULL,method TEXT NOT NULL,path TEXT NOT NULL,source TEXT NOT NULL,status INTEGER NOT NULL,duration_ms INTEGER NOT NULL,input_tokens INTEGER,output_tokens INTEGER,cached_tokens INTEGER,outcome TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS requests_time ON requests(time); CREATE INDEX IF NOT EXISTS requests_key ON requests(key_id);`)
	return db, e
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]any{"error": map[string]string{"message": msg, "type": "proxy_error"}})
}
func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", a.proxy)
	mux.HandleFunc("GET /dashboard.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(dashboardJS)
	})
	mux.HandleFunc("GET /dashboard.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(dashboardCSS)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if a.db.Ping() != nil {
			fail(w, 503, "database unavailable")
			return
		}
		reply(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/admin/", a.adminAPI)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}
func (a *App) adminAPI(w http.ResponseWriter, r *http.Request) {
	credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	admin := subtle.ConstantTimeCompare([]byte(credential), []byte(a.admin)) == 1
	var userID int64
	var userName string
	if !admin {
		if a.db.QueryRow(`SELECT id,name FROM users WHERE hash=? AND disabled=0`, hash(credential)).Scan(&userID, &userName) != nil {
			fail(w, 401, "valid dashboard token required")
			return
		}
	}
	scope := "1=1"
	scopeArgs := []any{}
	if !admin {
		scope = "k.user_id=?"
		scopeArgs = append(scopeArgs, userID)
	}
	scopedFilters := func() (string, []any) {
		where, args := filters(r)
		return where + " AND " + scope, append(args, scopeArgs...)
	}
	if strings.HasPrefix(r.URL.Path, "/admin/users") && !admin {
		fail(w, 403, "administrator access required")
		return
	}

	if r.Method == "POST" && r.Header.Get("Content-Type") != "application/json" {
		fail(w, 415, "application/json required")
		return
	}
	switch {
	case r.URL.Path == "/admin/events" && r.Method == "GET":
		a.liveEvents(w, r, userID, admin, credential)
	case r.URL.Path == "/admin/me" && r.Method == "GET":
		reply(w, 200, map[string]any{"admin": admin, "id": userID, "name": userName})
	case r.URL.Path == "/admin/profile" && r.Method == "POST":
		var v struct{ Name string }
		if admin {
			fail(w, 400, "administrator has no user profile")
			return
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil || strings.TrimSpace(v.Name) == "" || len(v.Name) > 100 {
			fail(w, 400, "valid name required")
			return
		}
		if _, e := a.db.Exec(`UPDATE users SET name=? WHERE id=?`, strings.TrimSpace(v.Name), userID); e != nil {
			fail(w, 500, "database error")
			return
		}
		a.notifyUser(userID)
		reply(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/admin/users" && r.Method == "GET":
		a.query(w, `SELECT id,name,disabled,created FROM users ORDER BY id DESC`)
	case r.URL.Path == "/admin/users" && r.Method == "POST":
		var v struct{ Name string }
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil || strings.TrimSpace(v.Name) == "" || len(v.Name) > 100 {
			fail(w, 400, "valid name required")
			return
		}
		secret := "mu_" + token()
		res, e := a.db.Exec(`INSERT INTO users(name,hash,created) VALUES(?,?,?)`, strings.TrimSpace(v.Name), hash(secret), time.Now().UTC().Format(time.RFC3339))
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		id, _ := res.LastInsertId()
		a.notifyUser(id)
		reply(w, 201, map[string]any{"id": id, "token": secret})
	case r.URL.Path == "/admin/users/update" && r.Method == "POST":
		var v struct {
			ID       int64
			Disabled bool
			Name     string
			Rotate   bool
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil || v.ID == 0 || len(v.Name) > 100 {
			fail(w, 400, "valid user required")
			return
		}
		secret := ""
		var res sql.Result
		var e error
		if v.Rotate {
			secret = "mu_" + token()
			res, e = a.db.Exec(`UPDATE users SET hash=? WHERE id=?`, hash(secret), v.ID)
		} else {
			res, e = a.db.Exec(`UPDATE users SET disabled=?,name=CASE WHEN ?='' THEN name ELSE ? END WHERE id=?`, v.Disabled, strings.TrimSpace(v.Name), strings.TrimSpace(v.Name), v.ID)
		}
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, 404, "user not found")
			return
		}
		a.notifyUser(v.ID)
		reply(w, 200, map[string]any{"ok": true, "token": secret})
	case r.URL.Path == "/admin/key-update" && r.Method == "POST":
		var v struct {
			ID   int64
			Name string
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil || strings.TrimSpace(v.Name) == "" || len(v.Name) > 100 {
			fail(w, 400, "valid name required")
			return
		}
		res, e := a.db.Exec(`UPDATE keys AS k SET name=? WHERE id=? AND `+scope, append([]any{strings.TrimSpace(v.Name), v.ID}, scopeArgs...)...)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, 404, "key not found")
			return
		}
		a.notifyKeys(v.ID)
		reply(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/admin/performance" && r.Method == "GET":
		where, args := scopedFilters()
		a.performance(w, r, where, args)
	case r.URL.Path == "/admin/timeseries" && r.Method == "GET":
		where, args := scopedFilters()
		a.query(w, `SELECT strftime('%Y-%m-%dT%H:%M:00Z',r.time) AS time,count(*) AS requests,coalesce(sum(input_tokens),0) AS input_tokens,coalesce(sum(output_tokens),0) AS output_tokens,round(avg(duration_ms)) AS avg_ms FROM requests r JOIN keys k ON k.id=r.key_id WHERE julianday(r.time)>=julianday('now','-1 hour') AND `+where+` GROUP BY 1 ORDER BY 1`, args...)
	case r.URL.Path == "/admin/status" && r.Method == "GET":
		reply(w, 200, map[string]any{"dropped_records": a.dropped.Load(), "queued_records": len(a.records)})
	case r.URL.Path == "/admin/keys" && r.Method == "GET":
		a.query(w, `SELECT k.id,k.name,k.prefix,k.created,k.revoked,k.user_id,u.name AS user_name FROM keys k LEFT JOIN users u ON u.id=k.user_id WHERE `+scope+` ORDER BY k.id DESC`, scopeArgs...)
	case r.URL.Path == "/admin/keys" && r.Method == "POST":
		var v struct {
			Name   string `json:"name"`
			UserID *int64 `json:"user_id"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil || len(strings.TrimSpace(v.Name)) == 0 || len(v.Name) > 100 {
			fail(w, 400, "name must contain 1–100 characters")
			return
		}
		if !admin {
			v.UserID = &userID
		}
		if v.UserID != nil {
			var valid int
			if a.db.QueryRow(`SELECT id FROM users WHERE id=? AND disabled=0`, *v.UserID).Scan(&valid) != nil {
				fail(w, 400, "active user required")
				return
			}
		}
		k := "mm_" + token()
		res, e := a.db.Exec(`INSERT INTO keys(user_id,name,hash,prefix,created) VALUES(?,?,?,?,?)`, v.UserID, strings.TrimSpace(v.Name), hash(k), k[:11], time.Now().UTC().Format(time.RFC3339))
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		id, _ := res.LastInsertId()
		a.notifyKeys(id)
		reply(w, 201, map[string]any{"id": id, "key": k})
	case r.URL.Path == "/admin/revoke" && r.Method == "POST":
		var v struct {
			ID int64 `json:"id"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil {
			fail(w, 400, "invalid id")
			return
		}
		res, e := a.db.Exec(`UPDATE keys AS k SET revoked=1 WHERE id=? AND `+scope, append([]any{v.ID}, scopeArgs...)...)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, 404, "key not found")
			return
		}
		a.notifyKeys(v.ID)
		reply(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/admin/usage" && r.Method == "GET":
		where, args := scopedFilters()
		a.query(w, `SELECT k.id AS key_id,k.name,k.user_id,(SELECT name FROM users WHERE id=k.user_id) AS user_name,r.model,count(*) AS requests,sum(CASE WHEN r.status>=400 OR r.outcome!='complete' THEN 1 ELSE 0 END) AS errors,sum(r.input_tokens) AS input_tokens,sum(r.output_tokens) AS output_tokens,sum(r.cached_tokens) AS cached_tokens,sum(CASE WHEN r.input_tokens IS NULL THEN 1 ELSE 0 END) AS unmetered,round(avg(r.duration_ms)) AS avg_ms FROM requests r JOIN keys k ON k.id=r.key_id WHERE `+where+` GROUP BY k.id,r.model ORDER BY requests DESC`, args...)
	case r.URL.Path == "/admin/requests" && r.Method == "GET":
		where, args := scopedFilters()
		a.query(w, `SELECT r.*,k.name AS key_name FROM requests r JOIN keys k ON k.id=r.key_id WHERE `+where+` ORDER BY r.id DESC LIMIT 200`, args...)
	default:
		http.NotFound(w, r)
	}
}
func filters(r *http.Request) (string, []any) {
	s := "1=1"
	args := []any{}
	for _, v := range []struct{ param, col string }{{"key", "r.key_id"}, {"model", "r.model"}, {"user", "k.user_id"}} {
		if x := r.URL.Query().Get(v.param); x != "" {
			s += " AND " + v.col + "=?"
			args = append(args, x)
		}
	}
	if x := r.URL.Query().Get("since"); x != "" {
		s += " AND julianday(r.time)>=julianday(?)"
		args = append(args, x)
	}
	return s, args
}
func (a *App) query(w http.ResponseWriter, q string, args ...any) {
	rows, e := a.db.Query(q, args...)
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range values {
			ptr[i] = &values[i]
		}
		if rows.Scan(ptr...) != nil {
			fail(w, 500, "database error")
			return
		}
		m := map[string]any{}
		for i, k := range cols {
			m[k] = values[i]
		}
		out = append(out, m)
	}
	if rows.Err() != nil {
		fail(w, 500, "database error")
		return
	}
	reply(w, 200, out)
}

type Meter struct {
	Input      *int64
	Output     *int64
	Cached     *int64
	Model      string
	buf        []byte
	SSE        bool
	overflow   bool
	Anthropic  bool
	rawInput   *int64
	cacheRead  *int64
	cacheWrite *int64
}

func (m *Meter) parse(b []byte) { m.parseEnvelope(b, 0) }

func (m *Meter) parseEnvelope(b []byte, depth int) {
	if depth > 4 {
		return
	}
	var v struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Usage *struct {
			Prompt        *int64 `json:"prompt_tokens"`
			Completion    *int64 `json:"completion_tokens"`
			Input         *int64 `json:"input_tokens"`
			Output        *int64 `json:"output_tokens"`
			CacheRead     *int64 `json:"cache_read_input_tokens"`
			CacheWrite    *int64 `json:"cache_creation_input_tokens"`
			PromptDetails struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			InputDetails struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Response json.RawMessage `json:"response"`
		Message  json.RawMessage `json:"message"`
	}
	if json.Unmarshal(b, &v) != nil {
		return
	}
	if v.Type == "message" || v.Type == "message_start" || v.Type == "message_delta" {
		m.Anthropic = true
	}
	if len(v.Response) > 0 {
		m.parseEnvelope(v.Response, depth+1)
	}
	if v.Type == "message_start" && len(v.Message) > 0 {
		m.parseEnvelope(v.Message, depth+1)
	}
	if v.Model != "" && len(v.Model) <= 256 {
		m.Model = v.Model
	}
	if v.Usage == nil {
		return
	}
	u := v.Usage
	// Usage events are cumulative snapshots; merge only fields actually present.
	if u.Prompt != nil {
		m.Input = u.Prompt
	}
	if u.Completion != nil {
		m.Output = u.Completion
	}
	if u.PromptDetails.Cached != nil {
		m.Cached = u.PromptDetails.Cached
	}
	if u.Output != nil {
		m.Output = u.Output
	}
	if !m.Anthropic {
		if u.Input != nil {
			m.Input = u.Input
		}
		if u.InputDetails.Cached != nil {
			m.Cached = u.InputDetails.Cached
		}
		return
	}
	// Anthropic excludes cache reads/writes from input_tokens. Normalize to the
	// total input convention used by Chat Completions and Responses.
	if u.Input != nil {
		m.rawInput = u.Input
	}
	if u.CacheRead != nil {
		m.cacheRead = u.CacheRead
		m.Cached = u.CacheRead
	}
	if u.CacheWrite != nil {
		m.cacheWrite = u.CacheWrite
	}
	if m.rawInput != nil {
		total := *m.rawInput
		if m.cacheRead != nil {
			total += *m.cacheRead
		}
		if m.cacheWrite != nil {
			total += *m.cacheWrite
		}
		m.Input = &total
	}
}

// Accept SDK-standard authentication without forwarding the caller's key.
func proxyCredential(r *http.Request) string {
	bearer := ""
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		bearer = strings.TrimPrefix(auth, "Bearer ")
	}
	apiKey := r.Header.Get("X-Api-Key")
	if auth != "" && bearer == "" {
		return ""
	}
	if bearer != "" && apiKey != "" && bearer != apiKey {
		return ""
	}
	if bearer != "" {
		return bearer
	}
	return apiKey
}

// Dynamic model identifiers stay out of persisted URL metadata.
func proxyRoute(path string) (label, method string) {
	switch path {
	case "/v1/models":
		return path, "GET"
	case "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/messages", "/v1/embeddings":
		return path, "POST"
	}
	if id, ok := strings.CutPrefix(path, "/v1/models/"); ok && id != "" && !strings.Contains(id, "/") {
		return "/v1/models/:model", "GET"
	}
	return "unsupported", ""
}

func (m *Meter) Write(p []byte) (int, error) {
	n := len(p)
	if !m.SSE {
		if len(m.buf)+n <= 8<<20 {
			m.buf = append(m.buf, p...)
		} else {
			m.overflow = true
			m.buf = nil
		}
		return n, nil
	}
	for _, c := range p {
		if c == '\n' {
			if !m.overflow {
				line := bytes.TrimSpace(m.buf)
				if bytes.HasPrefix(line, []byte("data:")) {
					m.parse(bytes.TrimSpace(line[5:]))
				}
			}
			m.buf = nil
			m.overflow = false
		} else if !m.overflow {
			if len(m.buf) < 8<<20 {
				m.buf = append(m.buf, c)
			} else {
				m.buf = nil
				m.overflow = true
			}
		}
	}
	return n, nil
}
func (m *Meter) finish() {
	if !m.SSE && !m.overflow {
		m.parse(m.buf)
	}
}
func (a *App) proxy(w http.ResponseWriter, r *http.Request) {
	var id int64
	if a.db.QueryRow(`SELECT k.id FROM keys k LEFT JOIN users u ON u.id=k.user_id WHERE k.hash=? AND k.revoked=0 AND (k.user_id IS NULL OR u.disabled=0)`, hash(proxyCredential(r))).Scan(&id) != nil {
		fail(w, 401, "valid proxy key required")
		return
	}
	start := time.Now()
	status := 502
	outcome := "upstream_error"
	m := &Meter{Anthropic: r.URL.Path == "/v1/messages"}
	source, _, _ := net.SplitHostPort(r.RemoteAddr)
	// Store only a known route label, never query parameters or arbitrary URL content.
	path, method := proxyRoute(r.URL.Path)
	defer func() {
		record := []any{id, start.UTC().Format(time.RFC3339Nano), m.Model, r.Method, path, source, status, time.Since(start).Milliseconds(), m.Input, m.Output, m.Cached, outcome}
		select {
		case a.records <- record:
		default:
			a.dropped.Add(1)
		}
	}()

	if method != r.Method {
		status = 405
		outcome = "rejected"
		fail(w, status, "unsupported endpoint or method")
		return
	}
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if e != nil {
		status = 413
		outcome = "rejected"
		fail(w, status, "request too large")
		return
	}
	if r.Method == "POST" {
		var obj map[string]json.RawMessage
		if json.Unmarshal(body, &obj) != nil || obj == nil {
			status = 400
			outcome = "rejected"
			fail(w, status, "JSON object required")
			return
		}
		json.Unmarshal(obj["model"], &m.Model)
		if len(m.Model) > 256 {
			m.Model = ""
		}
		var stream bool
		json.Unmarshal(obj["stream"], &stream)
		if stream && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions") {
			opts := map[string]any{}
			json.Unmarshal(obj["stream_options"], &opts)
			if opts == nil {
				opts = map[string]any{}
			}
			opts["include_usage"] = true
			obj["stream_options"], _ = json.Marshal(opts)
			body, _ = json.Marshal(obj)
		}
	}
	req, e := http.NewRequestWithContext(r.Context(), r.Method, a.upstream+r.URL.RequestURI(), bytes.NewReader(body))
	if e != nil {
		fail(w, 502, "upstream request failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// Forward only protocol control headers, never arbitrary client headers.
	for _, header := range []string{"Anthropic-Version", "Anthropic-Beta", "OpenAI-Beta"} {
		if value := r.Header.Get(header); value != "" {
			req.Header.Set(header, value)
		}
	}
	if a.upstreamKey != "" {
		if m.Anthropic || r.Header.Get("X-Api-Key") != "" {
			req.Header.Set("X-Api-Key", a.upstreamKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+a.upstreamKey)
		}
	}
	res, e := a.client.Do(req)
	if e != nil {
		fail(w, 502, "upstream unavailable")
		return
	}
	defer res.Body.Close()
	status = res.StatusCode
	m.SSE = strings.Contains(res.Header.Get("Content-Type"), "text/event-stream")
	for _, k := range []string{"Content-Type", "Retry-After", "X-Request-Id", "Request-Id"} {
		if v := res.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(status)
	buf := make([]byte, 32*1024)
	outcome = "complete"
	for {
		n, er := res.Body.Read(buf)
		if n > 0 {
			m.Write(buf[:n])
			if _, ew := w.Write(buf[:n]); ew != nil {
				outcome = "client_disconnected"
				break
			}
			if m.SSE {
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
		if er != nil {
			if er != io.EOF {
				outcome = "upstream_read_error"
			}
			break
		}
	}
	m.finish()
}
func main() {
	dir := env("DATA_DIR", "data")
	if e := os.MkdirAll(dir, 0700); e != nil {
		log.Fatal(e)
	}
	adminPath := filepath.Join(dir, "admin-token")
	b, e := os.ReadFile(adminPath)
	if os.IsNotExist(e) {
		b = []byte(token())
		e = os.WriteFile(adminPath, b, 0600)
	}
	if e != nil {
		log.Fatal(e)
	}
	db, e := openDB(filepath.Join(dir, "meter.db"))
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	up := strings.TrimRight(env("UPSTREAM_URL", "http://127.0.0.1:8000"), "/")
	u, e := url.Parse(up)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		log.Fatal("invalid upstream URL")
	}
	a := &App{db: db, upstream: up, upstreamKey: os.Getenv("UPSTREAM_API_KEY"), admin: strings.TrimSpace(string(b)), client: &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}
	a.startLogger()
	defer a.stopLogger()
	srv := &http.Server{Addr: env("LISTEN_ADDR", ":8001"), Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() {
		log.Printf("Model Meter listening on %s", srv.Addr)
		if e := srv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
			log.Fatal(e)
		}
	}()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	a.live.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e := srv.Shutdown(ctx); e != nil {
		fmt.Fprintln(os.Stderr, "shutdown deadline exceeded")
		os.Exit(1)
	}
}

// The request path only performs a non-blocking enqueue. One worker owns batch writes.
func (a *App) startLogger() {
	a.records = make(chan []any, 8192)
	a.loggerWG.Add(1)
	go func() {
		defer a.loggerWG.Done()
		batch := make([][]any, 0, 100)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			tx, e := a.db.Begin()
			if e == nil {
				for _, r := range batch {
					_, e = tx.Exec(`INSERT INTO requests(key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,cached_tokens,outcome) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r...)
					if e != nil {
						break
					}
				}
				if e == nil {
					e = tx.Commit()
				} else {
					tx.Rollback()
				}
			}
			if e != nil {
				a.dropped.Add(uint64(len(batch)))
				log.Print("request accounting batch failed; dropped-record counter increased")
			}
			if e == nil {
				ids := make([]any, 0, len(batch))
				seen := map[any]bool{}
				for _, record := range batch {
					if !seen[record[0]] {
						seen[record[0]] = true
						ids = append(ids, record[0])
					}
				}
				a.notifyKeys(ids...)
			} else {
				a.live.notify(nil)
			}
			batch = batch[:0]
		}
		for first := range a.records {
			batch = append(batch, first)
			// Drain only work already queued. Never wait to fill a batch.
		drain:
			for len(batch) < 100 {
				select {
				case record, ok := <-a.records:
					if !ok {
						break drain
					}
					batch = append(batch, record)
				default:
					break drain
				}
			}
			flush()
		}

	}()
}
func (a *App) stopLogger() { close(a.records); a.loggerWG.Wait() }
