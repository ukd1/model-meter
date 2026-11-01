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
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	db             *sql.DB
	upstream       string
	upstreamKey    string
	client         *http.Client
	records        chan requestRecord
	loggerWG       sync.WaitGroup
	dropped        atomic.Uint64
	live           liveHub
	allowAnonymous atomic.Bool
	login          loginLimiter
	settingsOnce   sync.Once
	settingsErr    error
	settingsMu     sync.Mutex
	modelAliases   map[string]string
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
	_, e = db.Exec(`CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY,name TEXT NOT NULL,hash TEXT UNIQUE NOT NULL,disabled INTEGER NOT NULL DEFAULT 0,admin INTEGER NOT NULL DEFAULT 0,created TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS keys(id INTEGER PRIMARY KEY,user_id INTEGER REFERENCES users(id),name TEXT NOT NULL,hash TEXT UNIQUE NOT NULL,prefix TEXT NOT NULL,created TEXT NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS requests(id INTEGER PRIMARY KEY,key_id INTEGER REFERENCES keys(id),time TEXT NOT NULL,time_unix INTEGER GENERATED ALWAYS AS (CAST(strftime('%s',time) AS INTEGER)) STORED,model TEXT NOT NULL,method TEXT NOT NULL,path TEXT NOT NULL,source TEXT NOT NULL,status INTEGER NOT NULL,duration_ms INTEGER NOT NULL,input_tokens INTEGER,output_tokens INTEGER,cached_tokens INTEGER,outcome TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS settings(name TEXT PRIMARY KEY,value INTEGER NOT NULL CHECK(value IN (0,1)));
 CREATE TABLE IF NOT EXISTS sessions(hash TEXT PRIMARY KEY,credential_hash TEXT NOT NULL,expires INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires);
 CREATE INDEX IF NOT EXISTS requests_time ON requests(time); CREATE INDEX IF NOT EXISTS requests_key ON requests(key_id);`)
	if e == nil {
		var hasAdmin int
		e = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name='admin'`).Scan(&hasAdmin)
		if e == nil && hasAdmin == 0 {
			_, e = db.Exec(`ALTER TABLE users ADD COLUMN admin INTEGER NOT NULL DEFAULT 0`)
		}
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	for _, column := range []struct{ name, definition string }{
		{"allowed_routes", "TEXT NOT NULL DEFAULT '[]'"},
		{"allowed_models", "TEXT NOT NULL DEFAULT '[]'"},
		{"expires", "TEXT"},
	} {
		var present int
		if e = db.QueryRow("SELECT count(*) FROM pragma_table_info('keys') WHERE name=?", column.name).Scan(&present); e != nil {
			db.Close()
			return nil, e
		}
		if present == 0 {
			if _, e = db.Exec("ALTER TABLE keys ADD COLUMN " + column.name + " " + column.definition); e != nil {
				db.Close()
				return nil, e
			}
		}
	}
	for _, column := range []string{"username", "password_hash"} {
		var exists int
		if e = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name=?`, column).Scan(&exists); e != nil {
			db.Close()
			return nil, e
		}
		if exists == 0 {
			if _, e = db.Exec(`ALTER TABLE users ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); e != nil {
				db.Close()
				return nil, e
			}
		}
	}
	if _, e = db.Exec(`UPDATE users SET username='user-'||id WHERE username=''; CREATE UNIQUE INDEX IF NOT EXISTS users_username ON users(username)`); e != nil {
		db.Close()
		return nil, e
	}
	if e = migrateAnonymousRequests(db); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}

// Encode before committing the status so a failure cannot produce a success
// response with a truncated body.
func reply(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"type":"proxy_error","message":"response encoding failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// JSONContentType accepts the media type with optional parameters. Comparing the
// raw header rejects valid requests such as "application/json; charset=utf-8".
func JSONContentType(r *http.Request) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && media == "application/json"
}
func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]any{"error": map[string]string{"message": msg, "type": "proxy_error"}})
}
func (a *App) handler() http.Handler {
	if err := a.loadSettings(); err != nil {
		log.Print("proxy settings unavailable")
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fail(w, 503, "proxy settings unavailable") })
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", a.mcpHTTP)
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
	mux.HandleFunc("/auth/", a.sessionAPI)
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
	identity, err := a.authenticate(r)
	if trusted, ok := r.Context().Value(mcpIdentityKey{}).(dashboardIdentity); ok {
		identity, err = trusted, nil
	}
	if err != nil {
		fail(w, 401, "sign-in required")
		return
	}
	admin, userID, userName := identity.Admin, identity.ID, identity.Name
	if identity.Cookie && r.Method != "GET" && r.Method != "HEAD" && !dashboardMutationAllowed(r) {
		fail(w, 403, "same-origin dashboard request required")
		return
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
	if (strings.HasPrefix(r.URL.Path, "/admin/users") || r.URL.Path == "/admin/settings") && !admin {
		fail(w, 403, "administrator access required")
		return
	}

	if r.Method == "POST" && !JSONContentType(r) {
		fail(w, 415, "application/json required")
		return
	}
	switch {
	case r.URL.Path == "/admin/settings" && (r.Method == "GET" || r.Method == "POST"):
		a.proxySettings(w, r)
	case r.URL.Path == "/admin/events" && r.Method == "GET":
		a.liveEvents(w, r, userID, admin)
	case r.URL.Path == "/admin/me" && r.Method == "GET":
		reply(w, 200, map[string]any{"admin": admin, "id": userID, "name": userName, "username": identity.Username})
	case r.URL.Path == "/admin/profile" && r.Method == "POST":
		var v struct {
			Name            string
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&v) != nil || len(v.Name) > 100 {
			fail(w, 400, "valid profile required")
			return
		}
		var encoded string
		if v.NewPassword != "" {
			if !validPassword(v.NewPassword) {
				fail(w, 400, "password must contain 12 to 1024 characters")
				return
			}
			if a.db.QueryRow(`SELECT password_hash FROM users WHERE id=?`, userID).Scan(&encoded) != nil || !verifyPassword(v.CurrentPassword, encoded) {
				fail(w, 403, "current password incorrect")
				return
			}
			var err error
			encoded, err = passwordHash(v.NewPassword)
			if err != nil {
				fail(w, 500, "password hashing failed")
				return
			}
		}
		if strings.TrimSpace(v.Name) == "" && encoded == "" {
			fail(w, 400, "name or new password required")
			return
		}
		if _, err := a.db.Exec(`UPDATE users SET name=CASE WHEN ?='' THEN name ELSE ? END,password_hash=CASE WHEN ?='' THEN password_hash ELSE ? END WHERE id=?`, strings.TrimSpace(v.Name), strings.TrimSpace(v.Name), encoded, encoded, userID); err != nil {
			fail(w, 500, "database error")
			return
		}
		a.notifyUser(userID)
		reply(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/admin/users" && r.Method == "GET":
		a.query(w, `SELECT id,name,username,disabled,admin,created FROM users ORDER BY id DESC`)
	case r.URL.Path == "/admin/users" && r.Method == "POST":
		var v struct {
			Name, Username, Password string
			Admin                    bool
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&v) != nil || strings.TrimSpace(v.Name) == "" || len(v.Name) > 100 || !validUsername(normalizeUsername(v.Username)) || (v.Password != "" && !validPassword(v.Password)) {
			fail(w, 400, "name, valid username and password (12 to 1024 characters) required")
			return
		}
		if v.Password == "" {
			v.Password = token()
		}
		encoded, err := passwordHash(v.Password)
		if err != nil {
			fail(w, 500, "password hashing failed")
			return
		}
		res, err := a.db.Exec(`INSERT INTO users(name,username,password_hash,hash,created,admin) VALUES(?,?,?,?,?,?)`, strings.TrimSpace(v.Name), normalizeUsername(v.Username), encoded, hash(token()), time.Now().UTC().Format(time.RFC3339), v.Admin)
		if err != nil {
			fail(w, 409, "username already exists or database unavailable")
			return
		}
		id, _ := res.LastInsertId()
		a.notifyUser(id)
		reply(w, 201, map[string]any{"id": id, "password": v.Password})
	case r.URL.Path == "/admin/users/update" && r.Method == "POST":
		var v struct {
			ID                 int64
			Disabled, Admin    *bool
			Name               string
			Username, Password *string
			Rotate             bool
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&v) != nil || v.ID == 0 || len(v.Name) > 100 {
			fail(w, 400, "valid user required")
			return
		}
		if v.Username != nil {
			normalized := normalizeUsername(*v.Username)
			if !validUsername(normalized) {
				fail(w, 400, "valid username required")
				return
			}
			v.Username = &normalized
		}
		generated := ""
		if v.Rotate {
			generated = token()
			v.Password = &generated
		}
		var encoded *string
		if v.Password != nil {
			if !validPassword(*v.Password) {
				fail(w, 400, "password must contain 12 to 1024 characters")
				return
			}
			h, err := passwordHash(*v.Password)
			if err != nil {
				fail(w, 500, "password hashing failed")
				return
			}
			encoded = &h
		}
		tx, err := a.db.Begin()
		if err != nil {
			fail(w, 500, "database error")
			return
		}
		defer tx.Rollback()
		var wasAdmin, wasDisabled int
		if err = tx.QueryRow(`SELECT admin,disabled FROM users WHERE id=?`, v.ID).Scan(&wasAdmin, &wasDisabled); err != nil {
			fail(w, 404, "user not found")
			return
		}
		res, err := tx.Exec(`UPDATE users SET disabled=COALESCE(?,disabled),admin=COALESCE(?,admin),username=COALESCE(?,username),password_hash=COALESCE(?,password_hash),name=CASE WHEN ?='' THEN name ELSE ? END WHERE id=?`, v.Disabled, v.Admin, v.Username, encoded, strings.TrimSpace(v.Name), strings.TrimSpace(v.Name), v.ID)
		if err != nil {
			fail(w, 409, "username already exists or database unavailable")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, 404, "user not found")
			return
		}
		// Refuse to leave the installation without a usable administrator. The
		// count runs after the write inside the same transaction, so two concurrent
		// demotions cannot both succeed, and bootstrap recovery only runs at startup,
		// which an operator locked out of the dashboard cannot trigger.
		if wasAdmin == 1 && wasDisabled == 0 && ((v.Admin != nil && !*v.Admin) || (v.Disabled != nil && *v.Disabled)) {
			var remaining int
			if err = tx.QueryRow(`SELECT count(*) FROM users WHERE admin=1 AND disabled=0 AND password_hash<>''`).Scan(&remaining); err != nil {
				fail(w, 500, "database error")
				return
			}
			if remaining == 0 {
				fail(w, 400, "at least one active administrator is required")
				return
			}
		}
		if err = tx.Commit(); err != nil {
			fail(w, 500, "database error")
			return
		}
		a.notifyUser(v.ID)
		reply(w, 200, map[string]any{"ok": true, "password": generated})
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
		a.timeseries(w, r.URL.Query().Get("since"), where, args)
	case r.URL.Path == "/admin/status" && r.Method == "GET":
		reply(w, 200, map[string]any{"dropped_records": a.dropped.Load(), "queued_records": len(a.records)})
	case r.URL.Path == "/admin/keys" && r.Method == "GET":
		keyScope := scope
		if r.URL.Query().Get("include_revoked") != "true" {
			keyScope += " AND k.revoked=0"
		}
		a.query(w, `SELECT k.id,k.name,k.prefix,k.created,k.revoked,k.user_id,u.name AS user_name FROM keys k LEFT JOIN users u ON u.id=k.user_id WHERE `+keyScope+` ORDER BY k.id DESC`, scopeArgs...)
	case r.URL.Path == "/admin/keys" && r.Method == "POST":
		var v struct {
			Name   string `json:"name"`
			UserID *int64 `json:"user_id"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil || len(strings.TrimSpace(v.Name)) == 0 || len(v.Name) > 100 {
			fail(w, 400, "name must contain 1–100 characters")
			return
		}
		if !admin || v.UserID == nil {
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
		a.query(w, `SELECT k.id AS key_id,coalesce(k.name,'Anonymous') AS name,k.user_id,(SELECT name FROM users WHERE id=k.user_id) AS user_name,r.model,count(*) AS requests,sum(CASE WHEN r.status>=400 OR r.outcome!='complete' THEN 1 ELSE 0 END) AS errors,coalesce(sum(r.input_tokens),0) AS input_tokens,coalesce(sum(r.output_tokens),0) AS output_tokens,coalesce(sum(r.cached_tokens),0) AS cached_tokens,sum(CASE WHEN r.input_tokens IS NULL THEN 1 ELSE 0 END) AS unmetered,round(avg(r.duration_ms)) AS avg_ms FROM requests r LEFT JOIN keys k ON k.id=r.key_id WHERE `+where+` GROUP BY k.id,r.model ORDER BY requests DESC`, args...)
	case r.URL.Path == "/admin/requests" && r.Method == "GET":
		where, args := scopedFilters()
		a.query(w, `SELECT r.*,coalesce(k.name,'Anonymous') AS key_name FROM requests r LEFT JOIN keys k ON k.id=r.key_id WHERE `+where+` ORDER BY r.id DESC LIMIT 200`, args...)
	default:
		http.NotFound(w, r)
	}
}
func filters(r *http.Request) (string, []any) {
	s := "1=1"
	args := []any{}
	for _, v := range []struct{ param, col string }{{"key", "r.key_id"}, {"model", "r.model"}, {"user", "k.user_id"}} {
		if x := r.URL.Query().Get(v.param); x != "" {
			if v.param == "key" && x == "anonymous" {
				s += " AND r.key_id IS NULL"
				continue
			}
			s += " AND " + v.col + "=?"
			args = append(args, x)
		}
	}
	if x := r.URL.Query().Get("since"); x != "" {
		// The conversion applies to the parameter, so the predicate can still use
		// the requests(time_unix) index.
		s += " AND r.time_unix>=CAST(strftime('%s',?) AS INTEGER)"
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
	if len(r.Header.Values("Authorization")) > 1 || len(r.Header.Values("X-Api-Key")) > 1 {
		return ""
	}
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

func allowedValue(raw, value string) bool {
	var values []string
	if json.Unmarshal([]byte(raw), &values) != nil {
		return false
	}
	if len(values) == 0 {
		return true
	}
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
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
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			m.append(p)
			break
		}
		m.append(p[:i])
		if !m.overflow {
			if line := bytes.TrimSpace(m.buf); bytes.HasPrefix(line, []byte("data:")) {
				m.parse(bytes.TrimSpace(line[5:]))
			}
		}
		m.buf = nil
		m.overflow = false
		p = p[i+1:]
	}
	return n, nil
}

// append keeps at most 8 MiB of the current line; longer lines stay unmetered.
func (m *Meter) append(p []byte) {
	if m.overflow || len(p) == 0 {
		return
	}
	if len(m.buf)+len(p) > 8<<20 {
		m.buf = nil
		m.overflow = true
		return
	}
	m.buf = append(m.buf, p...)
}
func (m *Meter) finish() {
	if !m.SSE && !m.overflow {
		m.parse(m.buf)
	}
}
func (a *App) proxy(w http.ResponseWriter, r *http.Request) {
	var keyID, ownerID any
	var allowedRoutes, allowedModels = "[]", "[]"
	missingCredential := len(r.Header.Values("Authorization")) == 0 && len(r.Header.Values("X-Api-Key")) == 0
	if !missingCredential || !a.allowAnonymous.Load() {
		var id int64
		var owner sql.NullInt64
		credential := proxyCredential(r)
		if credential == "" || a.db.QueryRow(`SELECT k.id,k.user_id,k.allowed_routes,k.allowed_models FROM keys k LEFT JOIN users u ON u.id=k.user_id WHERE k.hash=? AND k.revoked=0 AND (k.expires IS NULL OR julianday(k.expires)>julianday('now')) AND (k.user_id IS NULL OR u.disabled=0)`, hash(credential)).Scan(&id, &owner, &allowedRoutes, &allowedModels) != nil {
			fail(w, 401, "valid proxy key required")
			return
		}
		keyID = id
		// The owner travels with the record so the accounting worker can wake the
		// right dashboards without querying the keys table per batch.
		if owner.Valid {
			ownerID = owner.Int64
		}
	}
	start := time.Now()
	status := 502
	outcome := "upstream_error"
	m := &Meter{Anthropic: r.URL.Path == "/v1/messages"}
	source, _, _ := net.SplitHostPort(r.RemoteAddr)
	// Store only a known route label, never query parameters or arbitrary URL content.
	path, method := proxyRoute(r.URL.Path)
	defer func() {
		record := requestRecord{keyID: keyID, owner: ownerID, args: []any{start.UTC().Format(time.RFC3339Nano), m.Model, r.Method, path, source, status, time.Since(start).Milliseconds(), m.Input, m.Output, m.Cached, outcome}}
		select {
		case a.records <- record:
		default:
			a.dropped.Add(1)
		}
	}()

	// Accept the route by its raw path or by the recorded label, so an allowlist
	// may name either "/v1/models/xyz" or the "/v1/models/:model" entry seen in
	// request history.
	if !allowedValue(allowedRoutes, r.URL.Path) && !allowedValue(allowedRoutes, path) {
		status, outcome = 403, "rejected"
		fail(w, status, "endpoint not permitted for this key")
		return
	}
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
		// Decode only the routing fields first. The body is re-encoded only when a
		// model alias or stream_options injection requires it, which keeps the
		// common path to a single copy of the request instead of three.
		var meta struct {
			Model  json.RawMessage `json:"model"`
			Stream json.RawMessage `json:"stream"`
		}
		if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(body, &meta) != nil {
			status = 400
			outcome = "rejected"
			fail(w, status, "JSON object required")
			return
		}
		json.Unmarshal(meta.Model, &m.Model)
		if !allowedValue(allowedModels, m.Model) {
			status, outcome = 403, "rejected"
			fail(w, status, "model not permitted for this key")
			return
		}
		mapped, alias := a.modelAliases[m.Model]
		var stream bool
		json.Unmarshal(meta.Stream, &stream)
		injectUsage := stream && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions")
		if alias || injectUsage {
			var obj map[string]json.RawMessage
			if json.Unmarshal(body, &obj) != nil {
				status = 400
				outcome = "rejected"
				fail(w, status, "JSON object required")
				return
			}
			if alias {
				obj["model"], _ = json.Marshal(mapped)
				m.Model = mapped
			}
			if injectUsage {
				opts := map[string]any{}
				json.Unmarshal(obj["stream_options"], &opts)
				if opts == nil {
					opts = map[string]any{}
				}
				opts["include_usage"] = true
				obj["stream_options"], _ = json.Marshal(opts)
			}
			if body, e = json.Marshal(obj); e != nil {
				status = 400
				outcome = "rejected"
				fail(w, status, "JSON object required")
				return
			}
		}
		if len(m.Model) > 256 {
			m.Model = ""
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
	db, e := openDB(filepath.Join(dir, "meter.db"))
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	if len(os.Args) > 1 {
		if len(os.Args) != 3 || os.Args[1] != "reset-password" {
			log.Fatal("usage: model-meter [reset-password USERNAME]")
		}
		password, err := resetPassword(db, os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Username: %s\nPassword: %s\n", normalizeUsername(os.Args[2]), password)
		return
	}
	if e = bootstrapAdmin(db); e != nil {
		log.Fatal(e)
	}
	up := strings.TrimRight(env("UPSTREAM_URL", "http://127.0.0.1:8001"), "/")
	u, e := url.Parse(up)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		log.Fatal("invalid upstream URL")
	}
	a := &App{db: db, upstream: up, upstreamKey: os.Getenv("UPSTREAM_API_KEY"), client: &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 3 * time.Minute,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
	}}}
	if e := json.Unmarshal([]byte(env("MODEL_ALIASES", "{}")), &a.modelAliases); e != nil {
		log.Fatal("invalid MODEL_ALIASES JSON")
	}
	a.startLogger()
	defer a.stopLogger()
	srv := &http.Server{Addr: env("LISTEN_ADDR", ":8000"), Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
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
//
// A record carries its owner so the worker can wake the right dashboards without
// querying the keys table for every committed batch.
type requestRecord struct {
	keyID any
	owner any
	args  []any
}

func (a *App) startLogger() {
	a.records = make(chan requestRecord, 8192)
	a.loggerWG.Add(1)
	go func() {
		defer a.loggerWG.Done()
		batch := make([]requestRecord, 0, 100)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			tx, e := a.db.Begin()
			if e == nil {
				for _, record := range batch {
					_, e = tx.Exec(`INSERT INTO requests(key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,cached_tokens,outcome) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, append([]any{record.keyID}, record.args...)...)
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
				owners := map[int64]bool{}
				for _, record := range batch {
					if owner, ok := record.owner.(int64); ok {
						owners[owner] = true
					}
				}
				// Administrators are always woken; users only for their own records.
				a.live.notify(owners)
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

type mcpIdentityKey struct{}

func (a *App) mcpHTTP(w http.ResponseWriter, r *http.Request) {
	secret, username := os.Getenv("MCP_ADMIN_TOKEN"), os.Getenv("MCP_ADMIN_USERNAME")
	if len(secret) < 32 || username == "" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") != "" {
		clone := r.Clone(r.Context())
		clone.Header = r.Header.Clone()
		clone.Header.Set("X-Model-Meter", "dashboard")
		if !dashboardMutationAllowed(clone) {
			fail(w, 403, "origin not allowed")
			return
		}
	}
	auth := r.Header.Get("Authorization")
	supplied := strings.TrimPrefix(auth, "Bearer ")
	if len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare([]byte(hash(supplied)), []byte(hash(secret))) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="model-meter-mcp"`)
		fail(w, 401, "MCP administrator token required")
		return
	}
	var actor dashboardIdentity
	if a.db.QueryRowContext(r.Context(), `SELECT id,name,username,admin FROM users WHERE username=? AND admin=1 AND disabled=0`, username).Scan(&actor.ID, &actor.Name, &actor.Username, &actor.Admin) != nil {
		fail(w, 403, "active MCP administrator required")
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(405)
		return
	}
	version := r.Header.Get("MCP-Protocol-Version")
	if version != "" && version != "2025-03-26" && version != "2025-06-18" && version != "2025-11-25" {
		fail(w, 400, "unsupported protocol version")
		return
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" {
		fail(w, 415, "application/json required")
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "application/json") || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		fail(w, 406, "Accept must include application/json and text/event-stream")
		return
	}
	var m struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	rpcError := func(code int, text string) {
		reply(w, 200, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": code, "message": text}})
	}
	if dec.Decode(&m) != nil {
		rpcError(-32700, "invalid JSON")
		return
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF || m.JSONRPC != "2.0" || m.Method == "" {
		rpcError(-32600, "invalid request")
		return
	}
	if len(m.ID) == 0 {
		if m.Method == "notifications/initialized" || m.Method == "notifications/cancelled" {
			w.WriteHeader(202)
			return
		}
		fail(w, 400, "request id required")
		return
	}
	var id any
	if json.Unmarshal(m.ID, &id) != nil {
		rpcError(-32600, "invalid id")
		return
	}
	switch id.(type) {
	case string, float64:
	default:
		rpcError(-32600, "invalid id")
		return
	}
	response := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			response["error"] = map[string]any{"code": -32602, "message": "invalid parameters"}
			break
		}
		v := p.ProtocolVersion
		if v != "2025-03-26" && v != "2025-06-18" && v != "2025-11-25" {
			v = "2025-06-18"
		}
		response["result"] = map[string]any{"protocolVersion": v, "serverInfo": map[string]string{"name": "model-meter", "version": "1.0.0"}, "capabilities": map[string]any{"tools": map[string]any{}}}
	case "ping":
		response["result"] = map[string]any{}
	case "tools/list":
		integer := map[string]any{"type": "integer", "minimum": 1}
		tools := []any{}
		add := func(name, description string, props map[string]any, required ...string) {
			if required == nil {
				required = []string{}
			}
			tools = append(tools, map[string]any{"name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}})
		}
		add("list_users", "List named users and IDs, without credentials.", map[string]any{})
		add("list_keys", "List active key metadata. Optionally include revoked keys for audit.", map[string]any{"include_revoked": map[string]any{"type": "boolean"}})
		add("create_key", "Create an inference key for an active user. The secret is returned once; handle privately.", map[string]any{"user_id": integer, "name": map[string]any{"type": "string", "minLength": 1, "maxLength": 100}}, "user_id", "name")
		add("revoke_key", "Revoke a key for new requests, retaining historical usage.", map[string]any{"key_id": integer}, "key_id")
		add("get_stats", "Usage and performance by time window, including revoked history. Optional user/key/model filters.", map[string]any{"window": map[string]any{"type": "string", "enum": []string{"1m", "5m", "15m", "1h", "4h", "12h", "24h", "7d", "30d", "all"}}, "user_id": integer, "key_id": integer, "model": map[string]any{"type": "string", "maxLength": 256}})
		response["result"] = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			response["error"] = map[string]any{"code": -32602, "message": "invalid parameters"}
			break
		}
		value, err := a.mcpCall(r, actor, p.Name, p.Arguments)
		if err != nil {
			response["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}
			break
		}
		encoded, _ := json.Marshal(value)
		response["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(encoded)}}, "structuredContent": value, "isError": false}
	default:
		response["error"] = map[string]any{"code": -32601, "message": "method not found"}
	}
	reply(w, 200, response)
}

func (a *App) mcpCall(r *http.Request, actor dashboardIdentity, name string, raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	fields := map[string][]string{"list_users": {}, "list_keys": {"include_revoked"}, "create_key": {"user_id", "name"}, "revoke_key": {"key_id"}, "get_stats": {"window", "user_id", "key_id", "model"}}
	allowed, ok := fields[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool")
	}
	var supplied map[string]json.RawMessage
	if json.Unmarshal(raw, &supplied) != nil || supplied == nil {
		return nil, fmt.Errorf("arguments must be an object")
	}
	for k := range supplied {
		found := false
		for _, v := range allowed {
			if k == v {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("unsupported argument")
		}
	}
	var args struct {
		UserID         int64  `json:"user_id"`
		KeyID          int64  `json:"key_id"`
		Name           string `json:"name"`
		Window         string `json:"window"`
		Model          string `json:"model"`
		IncludeRevoked bool   `json:"include_revoked"`
	}
	if json.Unmarshal(raw, &args) != nil || args.UserID < 0 || args.KeyID < 0 || len(args.Model) > 256 {
		return nil, fmt.Errorf("invalid arguments")
	}
	for _, k := range []string{"user_id", "key_id"} {
		if v, ok := supplied[k]; ok && (string(v) == "null" || (k == "user_id" && args.UserID == 0) || (k == "key_id" && args.KeyID == 0)) {
			return nil, fmt.Errorf("IDs must be positive integers")
		}
	}
	invoke := func(method, path string, body any) (any, error) {
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(context.WithValue(r.Context(), mcpIdentityKey{}, actor), method, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		a.adminAPI(rec, req)
		if rec.Code >= 400 {
			return nil, fmt.Errorf("operation rejected (HTTP %d)", rec.Code)
		}
		var result any
		if json.Unmarshal(rec.Body.Bytes(), &result) != nil {
			return nil, fmt.Errorf("invalid operation result")
		}
		return result, nil
	}
	switch name {
	case "list_users":
		v, e := invoke("GET", "/admin/users", nil)
		return map[string]any{"users": v}, e
	case "list_keys":
		v, e := invoke("GET", "/admin/keys?include_revoked="+strconv.FormatBool(args.IncludeRevoked), nil)
		return map[string]any{"keys": v}, e
	case "create_key":
		if args.UserID <= 0 || strings.TrimSpace(args.Name) == "" || len(args.Name) > 100 {
			return nil, fmt.Errorf("positive user_id and name (1-100 characters) required")
		}
		v, e := invoke("POST", "/admin/keys", map[string]any{"name": args.Name, "user_id": args.UserID})
		return map[string]any{"created": v}, e
	case "revoke_key":
		if args.KeyID <= 0 {
			return nil, fmt.Errorf("positive key_id required")
		}
		v, e := invoke("POST", "/admin/revoke", map[string]any{"id": args.KeyID})
		return map[string]any{"revoked": v}, e
	case "get_stats":
		periods := map[string]time.Duration{"1m": time.Minute, "5m": 5 * time.Minute, "15m": 15 * time.Minute, "1h": time.Hour, "4h": 4 * time.Hour, "12h": 12 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}
		if args.Window == "" {
			args.Window = "1h"
		}
		q := url.Values{}
		if args.Window != "all" {
			duration, ok := periods[args.Window]
			if !ok {
				return nil, fmt.Errorf("unsupported window")
			}
			q.Set("since", time.Now().UTC().Add(-duration).Format(time.RFC3339Nano))
		}
		if args.UserID > 0 {
			q.Set("user", strconv.FormatInt(args.UserID, 10))
		}
		if args.KeyID > 0 {
			q.Set("key", strconv.FormatInt(args.KeyID, 10))
		}
		if args.Model != "" {
			q.Set("model", args.Model)
		}
		usage, e := invoke("GET", "/admin/usage?"+q.Encode(), nil)
		if e != nil {
			return nil, e
		}
		perf, e := invoke("GET", "/admin/performance?"+q.Encode(), nil)
		return map[string]any{"window": args.Window, "usage": usage, "performance": perf}, e
	}
	return nil, fmt.Errorf("unknown tool")
}
