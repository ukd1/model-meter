package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestPerformanceWindowAndMetrics(t *testing.T) {
	a, s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {})
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	insert := func(at time.Time, path, model string, status int, ms int, tokens any, outcome string) {
		t.Helper()
		_, e := a.db.Exec(`INSERT INTO requests(key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,outcome) VALUES(1,?,?, 'POST',?,'127.0.0.1',?,?,10,?,?)`, at.Format(time.RFC3339Nano), model, path, status, ms, tokens, outcome)
		if e != nil {
			t.Fatal(e)
		}
	}
	// More than the recent request limit, and a sample outside the old 1h graph.
	for i := 0; i < 205; i++ {
		insert(now.Add(-2*time.Hour), "/v1/messages", "m", 200, 2000, 20, "complete")
	}
	insert(now.Add(-48*time.Hour), "/v1/responses", "m", 200, 10000, 1000, "complete")
	insert(now, "/v1/models", "m", 200, 1, 99999, "complete")
	insert(now, "/v1/messages", "m", 429, 1, 99999, "complete")
	insert(now, "/v1/messages", "m", 200, 1, 99999, "client_disconnected")
	insert(now, "/v1/messages", "m", 200, 0, 5, "complete")
	insert(now, "/v1/messages", "m", 200, 1000, nil, "complete")
	res := call(t, s, "/admin/performance?model=m&since="+url.QueryEscape(since), "admin-secret", "")
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	var stats performanceStats
	if e := json.NewDecoder(res.Body).Decode(&stats); e != nil {
		t.Fatal(e)
	}
	if stats.Latency.Samples != 207 || stats.OutputRate.Samples != 205 {
		t.Fatal(stats.Latency.Samples, stats.OutputRate.Samples)
	}
	if *stats.Latency.P50 != 2 || *stats.Latency.P95 != 2 || *stats.OutputRate.Mean != 10 {
		t.Fatal("wrong summary", stats)
	}
	var latencyCount, rateCount int64
	for _, bin := range stats.Latency.Bins {
		latencyCount += bin.Count
	}
	for _, bin := range stats.OutputRate.Bins {
		rateCount += bin.Count
		if bin.Lower == 10 && bin.Count != 205 {
			t.Fatal("boundary bin")
		}
	}
	if latencyCount != 207 || rateCount != 205 {
		t.Fatal("histogram count mismatch")
	}
	var rateSamples int64
	for _, point := range stats.Series {
		rateSamples += point.RateSamples
		if point.OutputTPS != nil && *point.OutputTPS != 10 {
			t.Fatal("invalid series rate")
		}
	}
	if rateSamples != 205 {
		t.Fatal("series restricted to 1h or recent rows")
	}
	if stats.BucketSeconds != 900 {
		t.Fatal("wrong 24h resolution", stats.BucketSeconds)
	}
	res = call(t, s, "/admin/performance?model=m", "admin-secret", "")
	json.NewDecoder(res.Body).Decode(&stats)
	if stats.OutputRate.Samples != 206 {
		t.Fatal("all-time lost older data")
	}
}
func TestPerformanceIsolationAndEmpty(t *testing.T) {
	a, s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, name := range []string{"alice", "bob"} {
		seedTestUser(t, a, name, name)
	}
	a.db.Exec(`UPDATE keys SET user_id=1 WHERE id=1`)
	a.db.Exec(`INSERT INTO requests(key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,outcome) VALUES(1,?,'m','POST','/v1/chat/completions','local',200,1000,3,5,'complete')`, time.Now().UTC().Format(time.RFC3339Nano))
	for _, tc := range []struct {
		key, query string
		want       int64
	}{{"alice", "", 1}, {"bob", "", 0}, {"bob", "?user=1&key=1", 0}, {"admin-secret", "?user=2", 0}, {"admin-secret", "?model=other", 0}} {
		res := call(t, s, "/admin/performance"+tc.query, tc.key, "")
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
		var stats performanceStats
		json.NewDecoder(res.Body).Decode(&stats)
		if stats.OutputRate.Samples != tc.want {
			t.Fatal("scope leaked", tc, stats.OutputRate.Samples)
		}
		if tc.want == 0 && (stats.OutputRate.Mean != nil || stats.Latency.P50 != nil || len(stats.Series) != 0) {
			t.Fatal("empty results must not invent zero samples")
		}
	}
	if call(t, s, "/admin/performance?since=invalid", "admin-secret", "").StatusCode != 400 {
		t.Fatal("invalid timestamp accepted")
	}
}
func TestChartBuckets(t *testing.T) {
	for _, hours := range []float64{1, 24, 168, 720, 87600} {
		step := chartBucket(hours * 3600)
		if step < 1 || hours*3600/float64(step) > 120 {
			t.Fatal(hours, step)
		}
	}
}

func TestPerformanceExpiry(t *testing.T) {
	a, s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {})
	alice := seedTestUser(t, a, "alice", "alice")
	bob := seedTestUser(t, a, "bob", "bob")
	if _, err := a.db.Exec(`UPDATE keys SET user_id=? WHERE id=1`, alice); err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO keys(user_id,name,hash,prefix,created) VALUES(?,'bob','bob-key','bob',?)`, bob, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	bobKey, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	aliceFirst := now.Add(-45 * time.Second)
	bobFirst := now.Add(-55 * time.Second)
	insert := func(key int64, at time.Time, model string) {
		t.Helper()
		// Non-inference requests still expire from the request/token totals.
		_, err := a.db.Exec(`INSERT INTO requests(key_id,time,model,method,path,source,status,duration_ms,input_tokens,output_tokens,outcome) VALUES(?,?,?,'GET','/v1/models','local',500,5,NULL,NULL,'complete')`, key, at.Format(time.RFC3339Nano), model)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(1, now.Add(-2*time.Minute), "m")
	insert(1, now.Add(-50*time.Second), "other")
	insert(1, aliceFirst, "m")
	insert(bobKey, bobFirst, "m")
	insert(1, now.Add(time.Minute), "future")
	since := url.QueryEscape(now.Add(-time.Minute).Format(time.RFC3339Nano))
	for _, tc := range []struct {
		name, credential, query string
		first                   time.Time
	}{
		{"bounded", "admin-secret", "?since=" + since, bobFirst},
		{"user scope and model", "alice", "?since=" + since + "&model=m", aliceFirst},
		{"other user scope", "bob", "?since=" + since, bobFirst},
		{"admin user filter", "admin-secret", fmt.Sprintf("?since=%s&user=%d&model=m", since, alice), aliceFirst},
		{"key isolation", "alice", fmt.Sprintf("?since=%s&key=%d", since, bobKey), time.Time{}},
		{"empty", "admin-secret", "?since=" + since + "&model=missing", time.Time{}},
		{"future records excluded", "admin-secret", "?since=" + since + "&model=future", time.Time{}},
		{"all time", "admin-secret", "", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, s, "/admin/performance"+tc.query, tc.credential, "")
			if res.StatusCode != http.StatusOK {
				t.Fatal(res.StatusCode)
			}
			var raw map[string]json.RawMessage
			if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
				t.Fatal(err)
			}
			if _, ok := raw["expires_at"]; !ok {
				t.Fatal("expires_at must be present, including when null")
			}
			var expires *string
			if err := json.Unmarshal(raw["expires_at"], &expires); err != nil {
				t.Fatal(err)
			}
			if tc.first.IsZero() {
				if expires != nil {
					t.Fatalf("unexpected expiry: %s", *expires)
				}
				return
			}
			if expires == nil {
				t.Fatal("missing expiry for bounded nonempty window")
			}
			parse := func(field json.RawMessage) time.Time {
				t.Helper()
				var text string
				if err := json.Unmarshal(field, &text); err != nil {
					t.Fatal(err)
				}
				at, err := time.Parse(time.RFC3339Nano, text)
				if err != nil {
					t.Fatal(err)
				}
				return at
			}
			end := parse(raw["end"])
			want := tc.first.Add(end.Sub(parse(raw["start"]))).Add(50 * time.Millisecond)
			if got := parse(raw["expires_at"]); !got.Equal(want) || !got.After(end) {
				t.Fatalf("expiry = %s, want %s after end %s", got, want, end)
			}
		})
	}
}
