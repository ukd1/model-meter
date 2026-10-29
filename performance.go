package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

const inferenceSQL = `r.path IN ('/v1/chat/completions','/v1/responses','/v1/messages','/v1/completions') AND r.status>=200 AND r.status<300 AND r.outcome='complete'`
const rateSQL = `CASE WHEN ` + inferenceSQL + ` AND r.output_tokens>=0 AND r.duration_ms>0 THEN r.output_tokens*1000.0/r.duration_ms END`

type histogramBin struct {
	Lower float64  `json:"lower"`
	Upper *float64 `json:"upper"`
	Count int64    `json:"count"`
}
type distribution struct {
	Samples int64          `json:"samples"`
	Mean    *float64       `json:"mean"`
	P50     *float64       `json:"p50"`
	P95     *float64       `json:"p95"`
	Bins    []histogramBin `json:"bins"`
}
type performancePoint struct {
	Time         string   `json:"time"`
	Requests     int64    `json:"requests"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	OutputTPS    *float64 `json:"output_tps"`
	RateSamples  int64    `json:"rate_samples"`
}
type performanceStats struct {
	Start         string             `json:"start"`
	End           string             `json:"end"`
	BucketSeconds int64              `json:"bucket_seconds"`
	Latency       distribution       `json:"latency"`
	OutputRate    distribution       `json:"output_rate"`
	Series        []performancePoint `json:"series"`
}

// Compute in SQLite over the entire authorized window, never the recent-row cap.
func distributionFor(ctx context.Context, tx *sql.Tx, where string, args []any, metric string, edges []float64) (distribution, error) {
	d := distribution{Bins: make([]histogramBin, len(edges))}
	for i, edge := range edges {
		d.Bins[i].Lower = edge
		if i+1 < len(edges) {
			upper := edges[i+1]
			d.Bins[i].Upper = &upper
		}
	}
	base := `SELECT ` + metric + ` AS value FROM requests r JOIN keys k ON k.id=r.key_id WHERE ` + where + ` AND ` + inferenceSQL
	query := `WITH valueset AS (` + base + `), ranked AS (SELECT value,row_number() OVER (ORDER BY value) AS rn,count(*) OVER () AS total FROM valueset WHERE value IS NOT NULL AND value>=0) SELECT count(*),avg(value),max(CASE WHEN rn=(total+1)/2 THEN value END),max(CASE WHEN rn=(total*95+99)/100 THEN value END) FROM ranked`
	if e := tx.QueryRowContext(ctx, query, args...).Scan(&d.Samples, &d.Mean, &d.P50, &d.P95); e != nil {
		return d, e
	}
	cases := []string{"CASE"}
	for i := 1; i < len(edges); i++ {
		cases = append(cases, fmt.Sprintf("WHEN value<%g THEN %d", edges[i], i-1))
	}
	cases = append(cases, fmt.Sprintf("ELSE %d END", len(edges)-1))
	rows, e := tx.QueryContext(ctx, `WITH valueset AS (`+base+`) SELECT `+strings.Join(cases, " ")+` AS bin,count(*) FROM valueset WHERE value IS NOT NULL AND value>=0 GROUP BY bin`, args...)
	if e != nil {
		return d, e
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		var count int64
		if e := rows.Scan(&i, &count); e != nil {
			return d, e
		}
		d.Bins[i].Count = count
	}
	return d, rows.Err()
}
func chartBucket(span float64) int64 {
	for _, seconds := range []int64{1, 5, 10, 30, 60, 300, 600, 900, 1800, 3600, 7200, 21600, 43200, 86400, 604800} {
		if span/float64(seconds) <= 120 {
			return seconds
		}
	}
	return int64(math.Ceil(span/120/86400)) * 86400
}
func (a *App) performance(w http.ResponseWriter, r *http.Request, where string, args []any) {
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	if since := r.URL.Query().Get("since"); since != "" {
		parsed, e := time.Parse(time.RFC3339Nano, since)
		if e != nil || parsed.After(now) {
			fail(w, 400, "since must be a past RFC3339 timestamp")
			return
		}
		start = parsed.UTC()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tx, e := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	// All-time starts at the oldest matching record; an empty window defaults to 1h.
	if r.URL.Query().Get("since") == "" {
		var oldest sql.NullFloat64
		e = tx.QueryRowContext(ctx, `SELECT min(unixepoch(r.time)) FROM requests r JOIN keys k ON k.id=r.key_id WHERE `+where, args...).Scan(&oldest)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		if oldest.Valid {
			start = time.Unix(int64(oldest.Float64), 0).UTC()
		}
	}
	if start.After(now) {
		start = now.Add(-time.Hour)
	}
	where += ` AND julianday(r.time)<=julianday(?)`
	args = append(append([]any{}, args...), now.Format(time.RFC3339Nano))
	stats := performanceStats{Start: start.Format(time.RFC3339Nano), End: now.Format(time.RFC3339Nano), BucketSeconds: chartBucket(now.Sub(start).Seconds()), Series: []performancePoint{}}
	stats.Latency, e = distributionFor(ctx, tx, where, args, `r.duration_ms/1000.0`, []float64{0, .1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 300})
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	stats.OutputRate, e = distributionFor(ctx, tx, where, args, rateSQL, []float64{0, 1, 2, 5, 10, 20, 40, 80, 160, 320})
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	query := fmt.Sprintf(`SELECT (unixepoch(r.time)/%d)*%d AS bucket,count(*),coalesce(sum(r.input_tokens),0),coalesce(sum(r.output_tokens),0),avg(%s),count(%s) FROM requests r JOIN keys k ON k.id=r.key_id WHERE %s GROUP BY bucket ORDER BY bucket`, stats.BucketSeconds, stats.BucketSeconds, rateSQL, rateSQL, where)
	rows, e := tx.QueryContext(ctx, query, args...)
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	for rows.Next() {
		var stamp int64
		var point performancePoint
		if e = rows.Scan(&stamp, &point.Requests, &point.InputTokens, &point.OutputTokens, &point.OutputTPS, &point.RateSamples); e != nil {
			break
		}
		point.Time = time.Unix(stamp, 0).UTC().Format(time.RFC3339)
		stats.Series = append(stats.Series, point)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	if e = tx.Commit(); e != nil {
		fail(w, 500, "database error")
		return
	}
	reply(w, 200, stats)
}
