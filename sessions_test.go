package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sessionRequest(t *testing.T, s *httptest.Server, method, path, body string, cookie *http.Cookie, dashboard bool, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if dashboard {
		req.Header.Set("X-Model-Meter", "dashboard")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func sessionLogin(t *testing.T, s *httptest.Server, password string) *http.Cookie {
	t.Helper()
	username := password
	if password == "admin-secret" {
		username = "admin"
	}
	if password == "user-secret" {
		username = "alice"
	}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	res := sessionRequest(t, s, "POST", "/auth/login", string(body), nil, true, "")
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("login status %d: %s", res.StatusCode, b)
	}
	for _, c := range res.Cookies() {
		if c.Value != "" && c.MaxAge > 0 {
			return c
		}
	}
	t.Fatal("login did not issue a persistent session cookie")
	return nil
}

func TestSessionPersistsAndDoesNotAuthorizeProxy(t *testing.T) {
	_, s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("cookie-only request reached upstream") })
	c := sessionLogin(t, s, "admin-secret")
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("unsafe session cookie attributes: HttpOnly=%v SameSite=%v Path=%q Domain=%q", c.HttpOnly, c.SameSite, c.Path, c.Domain)
	}
	if c.Value == "admin-secret" || strings.Contains(c.Value, "admin-secret") {
		t.Fatal("cookie stores sign-in credential")
	}
	if c.Expires.Before(time.Now().Add(time.Hour)) {
		t.Fatal("session does not survive browser restart")
	}
	for i := 0; i < 2; i++ {
		res := sessionRequest(t, s, "GET", "/admin/me", "", c, false, "")
		if res.StatusCode != 200 {
			t.Fatalf("restoring session: %d", res.StatusCode)
		}
		var me struct{ Admin bool }
		if err := json.NewDecoder(res.Body).Decode(&me); err != nil || !me.Admin {
			t.Fatalf("incorrect identity: %+v %v", me, err)
		}
	}
	if res := sessionRequest(t, s, "GET", "/v1/models", "", c, false, ""); res.StatusCode != 401 {
		t.Fatalf("dashboard session accepted as API key: %d", res.StatusCode)
	}
}

func TestSessionLogoutInvalidatesCopiedCookie(t *testing.T) {
	_, s, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
	c := sessionLogin(t, s, "admin-secret")
	res := sessionRequest(t, s, "POST", "/auth/logout", "{}", c, true, "")
	if res.StatusCode != 200 && res.StatusCode != 204 {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	cleared := false
	for _, cookie := range res.Cookies() {
		if cookie.Name == c.Name && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not expire browser cookie")
	}
	if res := sessionRequest(t, s, "GET", "/admin/me", "", c, false, ""); res.StatusCode != 401 {
		t.Fatalf("copied cookie remained usable after logout: %d", res.StatusCode)
	}
}

func TestSessionCSRFAndBearerRejection(t *testing.T) {
	_, s, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
	if res := sessionRequest(t, s, "POST", "/auth/login", `{"username":"admin","password":"admin-secret"}`, nil, false, ""); res.StatusCode != 403 {
		t.Fatalf("login without CSRF header: %d", res.StatusCode)
	}
	c := sessionLogin(t, s, "admin-secret")
	for _, path := range []string{"/auth/logout", "/admin/keys"} {
		if res := sessionRequest(t, s, "POST", path, `{"name":"csrf"}`, c, false, ""); res.StatusCode != 403 {
			t.Errorf("%s without CSRF header: %d", path, res.StatusCode)
		}
	}
	if res := sessionRequest(t, s, "GET", "/admin/me", "", c, false, "invalid"); res.StatusCode != 401 {
		t.Fatalf("invalid explicit bearer fell back to cookie: %d", res.StatusCode)
	}
	if res := sessionRequest(t, s, "POST", "/admin/keys", `{"name":"bearer"}`, nil, false, "admin-secret"); res.StatusCode != 401 {
		t.Fatalf("legacy bearer unexpectedly accepted: %d", res.StatusCode)
	}
	if res := sessionRequest(t, s, "POST", "/admin/keys", `{"name":"cookie"}`, c, true, ""); res.StatusCode != 201 {
		t.Fatalf("cookie mutation failed: %d", res.StatusCode)
	}
}

func TestSessionCredentialRevocation(t *testing.T) {
	for _, action := range []string{"disabled", "rotated"} {
		t.Run(action, func(t *testing.T) {
			a, s, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
			id := seedTestUser(t, a, "Alice", "user-secret")
			c := sessionLogin(t, s, "user-secret")
			if res := sessionRequest(t, s, "GET", "/admin/users", "", c, false, ""); res.StatusCode != 403 {
				t.Fatalf("user got admin access: %d", res.StatusCode)
			}
			var body string
			if action == "disabled" {
				body = `{"id":` + jsonNumber(id) + `,"disabled":true}`
			} else {
				body = `{"id":` + jsonNumber(id) + `,"password":"replacement-password"}`
			}
			if res := call(t, s, "/admin/users/update", "admin-secret", body); res.StatusCode != 200 {
				t.Fatalf("update: %d", res.StatusCode)
			}
			if res := sessionRequest(t, s, "GET", "/admin/me", "", c, false, ""); res.StatusCode != 401 {
				t.Fatalf("%s user session still valid: %d", action, res.StatusCode)
			}
		})
	}
}

func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestSessionRejectsBadPasswordsAndCrossOrigin(t *testing.T) {
	_, s, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
	for _, credential := range []string{"wrong", "client-secret"} {
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": credential})
		if res := sessionRequest(t, s, "POST", "/auth/login", string(body), nil, true, ""); res.StatusCode != 401 {
			t.Errorf("invalid sign-in credential status: %d", res.StatusCode)
		}
	}
	req, _ := http.NewRequest("POST", s.URL+"/auth/login", strings.NewReader(`{"username":"admin","password":"admin-secret"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Model-Meter", "dashboard")
	req.Header.Set("Origin", "https://attacker.example")
	res, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("cross-origin login: %d", res.StatusCode)
	}
}

func TestSessionExpiryAndRestart(t *testing.T) {
	a, s, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
	c := sessionLogin(t, s, "admin-secret")
	// A new App and HTTP handler reuse only persisted data, not process memory.
	replacement := &App{db: a.db}
	restored := httptest.NewServer(replacement.handler())
	defer restored.Close()
	if res := sessionRequest(t, restored, "GET", "/admin/me", "", c, false, ""); res.StatusCode != 200 {
		t.Fatalf("session lost after app restart: %d", res.StatusCode)
	}
	var storedHash, credentialHash string
	if err := a.db.QueryRow("SELECT hash,credential_hash FROM sessions").Scan(&storedHash, &credentialHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != hash(c.Value) || !verifyPassword("admin-secret", credentialHash) {
		t.Fatal("session secrets not stored as hashes")
	}

	if _, err := a.db.Exec("UPDATE sessions SET expires=?", time.Now().Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if res := sessionRequest(t, s, "GET", "/admin/me", "", c, false, ""); res.StatusCode != 401 {
		t.Fatalf("expired session accepted: %d", res.StatusCode)
	}
}

func TestSessionCookieSecureOnHTTPS(t *testing.T) {
	a, _, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
	secure := httptest.NewTLSServer(a.handler())
	defer secure.Close()
	c := sessionLogin(t, secure, "admin-secret")
	if !c.Secure {
		t.Fatal("HTTPS session cookie missing Secure")
	}
}

func TestSessionNamedAdminPasswordRotation(t *testing.T) {
	a, s, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
	c := sessionLogin(t, s, "admin-secret")
	encoded, err := passwordHash("new-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec(`UPDATE users SET password_hash=? WHERE id=0`, encoded); err != nil {
		t.Fatal(err)
	}
	if res := sessionRequest(t, s, "GET", "/admin/me", "", c, false, ""); res.StatusCode != 401 {
		t.Fatalf("session survived password rotation: %d", res.StatusCode)
	}
}

func TestNamedAdminRolesAndProfile(t *testing.T) {
	_, s, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
	res := call(t, s, "/admin/users", "admin-secret", `{"name":"Second Admin","username":"second","password":"second-password","admin":true}`)
	if res.StatusCode != 201 {
		t.Fatalf("create named admin: %d", res.StatusCode)
	}
	var created struct{ ID int64 }
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if res := call(t, s, "/admin/users", "second:second-password", ""); res.StatusCode != 200 {
		t.Fatalf("named admin privileges: %d", res.StatusCode)
	}
	if res := call(t, s, "/admin/profile", "second:second-password", `{"name":"Renamed Admin"}`); res.StatusCode != 200 {
		t.Fatalf("named admin profile: %d", res.StatusCode)
	}
	if res := call(t, s, "/admin/users/update", "admin-secret", `{"id":`+jsonNumber(created.ID)+`,"admin":false}`); res.StatusCode != 200 {
		t.Fatalf("demote: %d", res.StatusCode)
	}
	if res := call(t, s, "/admin/users", "second:second-password", ""); res.StatusCode != 403 {
		t.Fatalf("demoted session retained privileges: %d", res.StatusCode)
	}
	if res := call(t, s, "/admin/me", "second:second-password", ""); res.StatusCode != 200 {
		t.Fatalf("demotion broke regular access: %d", res.StatusCode)
	}
}

func TestPasswordHashSaltAndMalformedEncoding(t *testing.T) {
	first, err := passwordHash("example-password")
	if err != nil {
		t.Fatal(err)
	}
	second, err := passwordHash("example-password")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || strings.Contains(first, "example-password") {
		t.Fatal("password hashing lacks random salt")
	}
	if !verifyPassword("example-password", first) || verifyPassword("wrong", first) {
		t.Fatal("password verification incorrect")
	}
	for _, value := range []string{"", hash("example-password"), "pbkdf2-sha256$0$00$00", "pbkdf2-sha256$999999999999$00$00"} {
		if verifyPassword("example-password", value) {
			t.Fatal("malformed password hash accepted")
		}
	}
}

func TestLegacyDatabaseMigrationPreservesUsersWithoutTokenLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE users(id INTEGER PRIMARY KEY,name TEXT NOT NULL,hash TEXT UNIQUE NOT NULL,disabled INTEGER NOT NULL DEFAULT 0,created TEXT NOT NULL); INSERT INTO users(id,name,hash,created) VALUES(1,'Legacy','legacy-hash','2026-01-01')`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	migrated, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var username, password, legacy string
	var admin bool
	if err = migrated.QueryRow(`SELECT username,password_hash,hash,admin FROM users WHERE id=1`).Scan(&username, &password, &legacy, &admin); err != nil {
		t.Fatal(err)
	}
	if username != "user-1" || password != "" || legacy != "legacy-hash" || admin {
		t.Fatalf("unexpected migration %q %t", username, admin)
	}
	app := &App{db: migrated}
	server := httptest.NewServer(app.handler())
	defer server.Close()
	if res := sessionRequest(t, server, "GET", "/admin/me", "", nil, false, "legacy-token"); res.StatusCode != 401 {
		t.Fatal("legacy bearer accepted")
	}
	body := `{"username":"user-1","password":"legacy-token"}`
	if res := sessionRequest(t, server, "POST", "/auth/login", body, nil, true, ""); res.StatusCode != 401 {
		t.Fatal("legacy token accepted as password")
	}
}

func TestOwnPasswordChangeRequiresCurrentAndRevokesSession(t *testing.T) {
	a, s, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
	seedTestUser(t, a, "Alice", "user-secret")
	c := sessionLogin(t, s, "user-secret")
	for _, body := range []string{`{"new_password":"new-password-123","current_password":"wrong"}`, `{"new_password":"new-password-123"}`} {
		if res := sessionRequest(t, s, "POST", "/admin/profile", body, c, true, ""); res.StatusCode != 403 {
			t.Fatalf("changed password without current password: %d", res.StatusCode)
		}
	}
	if res := sessionRequest(t, s, "POST", "/admin/profile", `{"new_password":"new-password-123","current_password":"user-secret"}`, c, true, ""); res.StatusCode != 200 {
		t.Fatalf("own password change: %d", res.StatusCode)
	}
	if res := sessionRequest(t, s, "GET", "/admin/me", "", c, false, ""); res.StatusCode != 401 {
		t.Fatal("old session survived password change")
	}
	if res := sessionRequest(t, s, "POST", "/auth/login", `{"username":"alice","password":"user-secret"}`, nil, true, ""); res.StatusCode != 401 {
		t.Fatal("old password accepted")
	}
	if res := sessionRequest(t, s, "POST", "/auth/login", `{"username":"alice","password":"new-password-123"}`, nil, true, ""); res.StatusCode != 200 {
		t.Fatal("new password rejected")
	}
}

func TestBootstrapAndLocalPasswordReset(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	previous := log.Writer()
	var captured bytes.Buffer
	log.SetOutput(&captured)
	defer log.SetOutput(previous)
	if err = bootstrapAdmin(db); err != nil {
		t.Fatal("bootstrap failed")
	}
	var count int
	var encoded string
	if err = db.QueryRow(`SELECT count(*),password_hash FROM users WHERE username='admin' AND admin=1 AND disabled=0`).Scan(&count, &encoded); err != nil || count != 1 {
		t.Fatal("bootstrap did not create named admin")
	}
	fields := strings.Fields(captured.String())
	if len(fields) == 0 {
		t.Fatal("bootstrap did not supply initial credentials")
	}
	password := fields[len(fields)-1]
	if !verifyPassword(password, encoded) {
		t.Fatal("bootstrap credentials mismatch")
	}
	captured.Reset()
	if err = bootstrapAdmin(db); err != nil {
		t.Fatal("second bootstrap failed")
	}
	if captured.Len() != 0 {
		t.Fatal("bootstrap repeated initial credentials")
	}
	if _, err = db.Exec(`UPDATE users SET disabled=1 WHERE username='admin'`); err != nil {
		t.Fatal(err)
	}
	replacement, err := resetPassword(db, "admin")
	if err != nil {
		t.Fatal("reset failed")
	}
	var disabled bool
	if err = db.QueryRow(`SELECT password_hash,disabled FROM users WHERE username='admin'`).Scan(&encoded, &disabled); err != nil {
		t.Fatal(err)
	}
	if disabled || !verifyPassword(replacement, encoded) || verifyPassword(password, encoded) {
		t.Fatal("reset did not replace password and enable user")
	}
	if _, err = resetPassword(db, "missing"); err == nil {
		t.Fatal("reset of nonexistent user succeeded")
	}
}

func TestConfiguredHTTPSPublicOrigins(t *testing.T) {
	t.Setenv("PUBLIC_ORIGINS", "https://proxy.example.test,https://192.0.2.10")
	for _, host := range []string{"proxy.example.test", "192.0.2.10"} {
		r := httptest.NewRequest("POST", "http://"+host+"/auth/login", nil)
		r.Header.Set("X-Model-Meter", "dashboard")
		r.Header.Set("Origin", "https://"+host)
		if !dashboardMutationAllowed(r) || !sessionCookie(r, "session", time.Now().Add(time.Hour), 3600).Secure {
			t.Fatal("TLS proxy origin rejected or cookie not Secure")
		}
		r.Header.Set("Origin", "https://attacker.example")
		if dashboardMutationAllowed(r) {
			t.Fatal("cross-origin request accepted")
		}
		r.Header.Set("Origin", "https://"+host)
		r.Header.Del("X-Model-Meter")
		if dashboardMutationAllowed(r) {
			t.Fatal("CSRF header requirement bypassed")
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1/auth/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	if requestScheme(r) != "http" || sessionCookie(r, "session", time.Now(), 3600).Secure {
		t.Fatal("untrusted forwarded scheme accepted")
	}
}

func TestAdminKeyDefaultsToNamedOwner(t *testing.T) {
	a, s, _ := fixture(t, func(http.ResponseWriter, *http.Request) {})
	res := call(t, s, "/admin/keys", "admin-secret", `{"name":"Named admin key"}`)
	if res.StatusCode != 201 {
		t.Fatal("key creation failed")
	}
	var created struct {
		ID int64
	}
	if e := json.NewDecoder(res.Body).Decode(&created); e != nil {
		t.Fatal(e)
	}
	var owner int64
	if e := a.db.QueryRow("SELECT user_id FROM keys WHERE id=?", created.ID).Scan(&owner); e != nil || owner != 0 {
		t.Fatal("named administrator key is unowned", e)
	}
}
