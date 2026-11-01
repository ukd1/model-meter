package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func emptyUpstream(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }

// Media types carry optional parameters. A raw string comparison rejects
// "application/json; charset=utf-8", which any ordinary client may send.
func TestJSONMediaTypeParametersAccepted(t *testing.T) {
	_, s, _ := fixture(t, emptyUpstream)
	body := `{"username":"admin","password":"admin-secret"}`
	for _, header := range []string{"application/json", "application/json; charset=utf-8", "APPLICATION/JSON;charset=UTF-8"} {
		req, err := http.NewRequest("POST", s.URL+"/auth/login", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", header)
		req.Header.Set("X-Model-Meter", "dashboard")
		res, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%q rejected with %d", header, res.StatusCode)
		}
	}
	for _, header := range []string{"text/plain", "", "application/x-www-form-urlencoded"} {
		req, err := http.NewRequest("POST", s.URL+"/auth/login", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set("Content-Type", header)
		}
		req.Header.Set("X-Model-Meter", "dashboard")
		res, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("%q accepted with %d", header, res.StatusCode)
		}
	}
}

// Bootstrap recovery only runs at startup, so losing every administrator would
// lock operators out until they can edit the database by hand.
func TestLastAdministratorCannotBeRemoved(t *testing.T) {
	a, s, _ := fixture(t, emptyUpstream)
	for _, body := range []string{`{"ID":0,"Admin":false}`, `{"ID":0,"Disabled":true}`} {
		res := call(t, s, "/admin/users/update", "admin-secret", body)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: last administrator removal returned %d", body, res.StatusCode)
		}
	}
	var admin, disabled int
	if err := a.db.QueryRow(`SELECT admin,disabled FROM users WHERE id=0`).Scan(&admin, &disabled); err != nil {
		t.Fatal(err)
	}
	if admin != 1 || disabled != 0 {
		t.Fatalf("rejected update still applied: admin=%d disabled=%d", admin, disabled)
	}
	// A second administrator may be demoted while one usable account remains.
	var id int64
	encoded, err := passwordHash("bob-secret")
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.db.Exec(`INSERT INTO users(name,username,password_hash,hash,admin,created) VALUES('Bob','bob',?,'bob-hash',1,?)`, encoded, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if res := call(t, s, "/admin/users/update", "admin-secret", fmt.Sprintf(`{"ID":%d,"Admin":false}`, id)); res.StatusCode != http.StatusOK {
		t.Fatalf("demoting one of two administrators returned %d", res.StatusCode)
	}
}

// Repeated failures must be refused without re-checking the password, and the
// budget is per account so unknown usernames cannot lock out a real operator.
func TestLoginAttemptsAreThrottled(t *testing.T) {
	_, s, _ := fixture(t, emptyUpstream)
	bad := `{"username":"admin","password":"wrong-password"}`
	for i := 0; i < defaultLoginUser; i++ {
		if res := sessionRequest(t, s, "POST", "/auth/login", bad, nil, true, ""); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d returned %d", i, res.StatusCode)
		}
	}
	res := sessionRequest(t, s, "POST", "/auth/login", `{"username":"admin","password":"admin-secret"}`, nil, true, "")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt returned %d", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatal("throttled response has no Retry-After")
	}
	// A different account is unaffected by this account's failures.
	if res := sessionRequest(t, s, "POST", "/auth/login", `{"username":"nobody","password":"whatever-value"}`, nil, true, ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unrelated account returned %d", res.StatusCode)
	}
}

// RFC3339Nano drops trailing zeros, so timestamps of mixed precision do not sort
// as strings. A stable numeric column keeps window filters exact and indexable.
func TestRequestTimeIsNormalisedToEpoch(t *testing.T) {
	a, _, _ := fixture(t, emptyUpstream)
	stamps := []string{"2026-01-02T03:04:05.5Z", "2026-01-02T03:04:05Z", "2026-01-02T03:04:05.000000001Z"}
	for _, stamp := range stamps {
		insertTestRequest(t, a, stamp, "/v1/chat/completions")
	}
	for _, stamp := range stamps {
		var unix int64
		if err := a.db.QueryRow(`SELECT time_unix FROM requests WHERE time=?`, stamp).Scan(&unix); err != nil {
			t.Fatalf("%s: %v", stamp, err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Fatal(err)
		}
		if unix != parsed.Unix() {
			t.Fatalf("%s: time_unix=%d want %d", stamp, unix, parsed.Unix())
		}
	}
	// The window index is created with the column, including for legacy tables.
	var indexes int
	if err := a.db.QueryRow(`SELECT count(*) FROM pragma_index_list('requests') WHERE name='requests_unix'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 1 {
		t.Fatal("requests_time_unix index missing")
	}
}

// A ?since window must be applied by the stored epoch column, not by rebuilding
// a julian day for every row.
func TestPerformanceWindowExcludesOlderRequests(t *testing.T) {
	a, s, _ := fixture(t, emptyUpstream)
	now := time.Now().UTC()
	insertTestRequest(t, a, now.Add(-2*time.Hour).Format(time.RFC3339Nano), "/v1/chat/completions")
	insertTestRequest(t, a, now.Add(-time.Minute).Format(time.RFC3339Nano), "/v1/chat/completions")
	since := url.QueryEscape(now.Add(-30 * time.Minute).Format(time.RFC3339Nano))
	res := call(t, s, "/admin/performance?since="+since, "admin-secret", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("performance status %d", res.StatusCode)
	}
	var got struct {
		Latency struct {
			Samples int64 `json:"samples"`
		} `json:"latency"`
		Series []struct {
			Requests int64 `json:"requests"`
		} `json:"series"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Latency.Samples != 1 {
		t.Fatalf("window samples=%d want 1", got.Latency.Samples)
	}
	var total int64
	for _, point := range got.Series {
		total += point.Requests
	}
	if total != 1 {
		t.Fatalf("window requests=%d want 1", total)
	}
}

// The chart endpoint used to ignore ?since and always reported one hour.
func TestTimeseriesHonoursSince(t *testing.T) {
	a, s, _ := fixture(t, emptyUpstream)
	now := time.Now().UTC()
	insertTestRequest(t, a, now.Add(-2*time.Hour).Format(time.RFC3339Nano), "/v1/chat/completions")
	insertTestRequest(t, a, now.Add(-time.Minute).Format(time.RFC3339Nano), "/v1/chat/completions")
	if total := timeseriesTotal(t, call(t, s, "/admin/timeseries", "admin-secret", "")); total != 1 {
		t.Fatalf("default window total=%d want 1", total)
	}
	since := url.QueryEscape(now.Add(-3 * time.Hour).Format(time.RFC3339Nano))
	if total := timeseriesTotal(t, call(t, s, "/admin/timeseries?since="+since, "admin-secret", "")); total != 2 {
		t.Fatalf("explicit window total=%d want 2", total)
	}
}

func timeseriesTotal(t *testing.T, res *http.Response) int64 {
	t.Helper()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("timeseries status %d", res.StatusCode)
	}
	var rows []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, row := range rows {
		if n, ok := row["requests"].(float64); ok {
			total += int64(n)
		}
	}
	return total
}

func insertTestRequest(t *testing.T, a *App, stamp, path string) {
	t.Helper()
	_, err := a.db.Exec(`INSERT INTO requests(key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,cached_tokens,outcome) VALUES(NULL,?,'m','POST',?,'127.0.0.1',200,500,10,20,0,'complete')`, stamp, path)
	if err != nil {
		t.Fatal(err)
	}
}

var _ = httptest.NewServer
