package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const sessionCookieName = "model_meter_session"
const sessionLifetime = 30 * 24 * time.Hour

var errAuthentication = errors.New("sign-in required")

type dashboardIdentity struct {
	ID       int64
	Name     string
	Username string
	Admin    bool
	Cookie   bool
}

// Sessions reference the salted password hash, never the password itself.
// Checking it on every request makes user disabling and password resets immediate.
func (a *App) credentialIdentity(ctx context.Context, credentialHash string) (dashboardIdentity, error) {
	var p dashboardIdentity
	err := a.db.QueryRowContext(ctx, `SELECT id,name,admin,username FROM users WHERE password_hash=? AND password_hash<>'' AND disabled=0`, credentialHash).Scan(&p.ID, &p.Name, &p.Admin, &p.Username)
	if err != nil {
		return p, errAuthentication
	}
	return p, nil
}

func (a *App) authenticate(r *http.Request) (dashboardIdentity, error) {
	if r.Header.Get("Authorization") != "" {
		return dashboardIdentity{}, errAuthentication
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return dashboardIdentity{}, errAuthentication
	}
	var credentialHash string
	if err = a.db.QueryRowContext(r.Context(), `SELECT credential_hash FROM sessions WHERE hash=? AND expires>?`, hash(cookie.Value), time.Now().Unix()).Scan(&credentialHash); err != nil {
		return dashboardIdentity{}, errAuthentication
	}
	p, err := a.credentialIdentity(r.Context(), credentialHash)
	p.Cookie = true
	return p, err
}

// PUBLIC_ORIGINS declares HTTPS-only external hosts behind TLS-terminating proxies.
// Never infer this from untrusted Forwarded or X-Forwarded-Proto headers.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	for _, value := range strings.Split(env("PUBLIC_ORIGINS", ""), ",") {
		origin, err := url.Parse(strings.TrimSpace(value))
		if err == nil && origin.Scheme == "https" && origin.Host != "" &&
			origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == "" &&
			strings.EqualFold(origin.Host, r.Host) {
			return "https"
		}
	}
	return "http"
}

// A non-simple header prevents cross-origin form submissions from using cookies.
// Origin and Fetch Metadata checks also reject explicitly cross-site requests.
func dashboardMutationAllowed(r *http.Request) bool {
	if r.Header.Get("X-Model-Meter") != "dashboard" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		scheme := requestScheme(r)
		if err != nil || parsed.Scheme != scheme || !strings.EqualFold(parsed.Host, r.Host) || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return false
		}
	}
	return true
}

func sessionCookie(r *http.Request, value string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{Name: sessionCookieName, Value: value, Path: "/", HttpOnly: true, Secure: requestScheme(r) == "https", SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: maxAge}
}

func (a *App) deleteSession(r *http.Request) error {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil
	}
	var credentialHash string
	// Read the owner before removal so existing streams are promptly revalidated.
	_ = a.db.QueryRowContext(r.Context(), `SELECT credential_hash FROM sessions WHERE hash=?`, hash(cookie.Value)).Scan(&credentialHash)
	if _, err = a.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE hash=?`, hash(cookie.Value)); err != nil {
		return err
	}
	p, err := a.credentialIdentity(r.Context(), credentialHash)
	if err == nil {
		a.notifyUser(p.ID)
	}
	return nil
}

func (a *App) sessionAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/auth/login" && r.URL.Path != "/auth/logout" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, 405, "POST required")
		return
	}
	if !dashboardMutationAllowed(r) {
		fail(w, 403, "same-origin dashboard request required")
		return
	}
	if !JSONContentType(r) {
		fail(w, 415, "application/json required")
		return
	}
	if r.URL.Path == "/auth/logout" {
		if err := a.deleteSession(r); err != nil {
			fail(w, 500, "could not end session")
			return
		}
		http.SetCookie(w, sessionCookie(r, "", time.Unix(1, 0), -1))
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	var v struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil || v.Password == "" || len(v.Password) > 1024 || !validUsername(normalizeUsername(v.Username)) {
		fail(w, 400, "username and password required")
		return
	}
	var credentialHash string
	account := normalizeUsername(v.Username)
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	if retry, limited := a.login.blocked("u:"+account, "p:"+peer); limited {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		fail(w, 429, "too many sign-in attempts; wait and try again")
		return
	}
	err := a.db.QueryRowContext(r.Context(), `SELECT password_hash FROM users WHERE username=? AND disabled=0`, account).Scan(&credentialHash)
	if err != nil || !verifyPassword(v.Password, credentialHash) {
		a.login.fail("u:"+account, "p:"+peer)
		fail(w, 401, "username or password incorrect")
		return
	}
	p, err := a.credentialIdentity(r.Context(), credentialHash)
	if err != nil {
		a.login.fail("u:"+account, "p:"+peer)
		fail(w, 401, "username or password incorrect")
		return
	}
	// A valid password clears only the per-account counter; the per-peer budget
	// stays spent so one working account cannot reset a spraying attempt.
	a.login.succeed("u:" + account)
	value := token()
	expires := time.Now().Add(sessionLifetime)
	if _, err = a.db.ExecContext(r.Context(), `INSERT INTO sessions(hash,credential_hash,expires) VALUES(?,?,?)`, hash(value), credentialHash, expires.Unix()); err != nil {
		fail(w, 500, "could not create session")
		return
	}
	if err = a.deleteSession(r); err != nil {
		_, _ = a.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE hash=?`, hash(value))
		fail(w, 500, "could not replace session")
		return
	}
	_, _ = a.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE expires<=?`, time.Now().Unix())
	http.SetCookie(w, sessionCookie(r, value, expires, int(sessionLifetime.Seconds())))
	reply(w, 200, map[string]any{"admin": p.Admin, "id": p.ID, "name": p.Name, "username": p.Username})
}
