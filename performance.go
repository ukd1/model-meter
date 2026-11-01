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
	ExpiresAt     *string            `json:"expires_at"`
	BucketSeconds int64              `json:"bucket_seconds"`
	Latency       distribution       `json:"latency"`
	OutputRate    distribution       `json:"output_rate"`
	Series        []performancePoint `json:"series"`
}

// windowTable holds the authorized window once per request. Percentiles,
// histograms and the series all read it, so the requests table is scanned once
// instead of five times. The surrounding transaction pins one connection, which
// makes the temporary table private to the request.
const windowTable = "mm_window"

func buildWindow(ctx context.Context, tx *sql.Tx, where string, args []any, bucket int64) error {
	if _, e := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+windowTable+`(bucket INTEGER,duration REAL,rate REAL,input_tokens INTEGER,output_tokens INTEGER)`); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, `DELETE FROM `+windowTable); e != nil {
		return e
	}
	// duration is NULL for non-inference rows so the latency distribution keeps
	// its documented sample set, while the series still counts every request.
	query := fmt.Sprintf(`INSERT INTO %s SELECT (r.time_unix/%d)*%d,CASE WHEN %s THEN r.duration_ms/1000.0 END,%s,r.input_tokens,r.output_tokens FROM requests r LEFT JOIN keys k ON k.id=r.key_id WHERE %s`, windowTable, bucket, bucket, inferenceSQL, rateSQL, where)
	_, e := tx.ExecContext(ctx, query, args...)
	return e
}

// Compute over the entire authorized window, never the recent-row cap.
func distributionFor(ctx context.Context, tx *sql.Tx, metric string, edges []float64) (distribution, error) {
	d := distribution{Bins: make([]histogramBin, len(edges))}
	for i, edge := range edges {
		d.Bins[i].Lower = edge
		if i+1 < len(edges) {
			upper := edges[i+1]
			d.Bins[i].Upper = &upper
		}
	}
	query := `WITH ranked AS (SELECT ` + metric + ` AS value,row_number() OVER (ORDER BY ` + metric + `) AS rn,count(*) OVER () AS total FROM ` + windowTable + ` WHERE ` + metric + ` IS NOT NULL AND ` + metric + `>=0) SELECT count(*),avg(value),max(CASE WHEN rn=(total+1)/2 THEN value END),max(CASE WHEN rn=(total*95+99)/100 THEN value END) FROM ranked`
	if e := tx.QueryRowContext(ctx, query).Scan(&d.Samples, &d.Mean, &d.P50, &d.P95); e != nil {
		return d, e
	}
	cases := []string{"CASE"}
	for i := 1; i < len(edges); i++ {
		cases = append(cases, fmt.Sprintf("WHEN "+metric+"<%g THEN %d", edges[i], i-1))
	}
	cases = append(cases, fmt.Sprintf("ELSE %d END", len(edges)-1))
	rows, e := tx.QueryContext(ctx, `SELECT `+strings.Join(cases, " ")+` AS bin,count(*) FROM `+windowTable+` WHERE `+metric+` IS NOT NULL AND `+metric+`>=0 GROUP BY bin`)
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
	bounded := r.URL.Query().Get("since") != ""
	if bounded {
		parsed, e := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
		if e != nil || parsed.After(now) {
			fail(w, 400, "since must be a past RFC3339 timestamp")
			return
		}
		start = parsed.UTC()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// The window table is written inside this transaction, so it is not opened
	// read-only; nothing outside the temporary schema is modified.
	tx, e := a.db.BeginTx(ctx, nil)
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	// All-time starts at the oldest matching record; an empty window defaults to 1h.
	if !bounded {
		var oldest sql.NullInt64
		e = tx.QueryRowContext(ctx, `SELECT min(r.time_unix) FROM requests r LEFT JOIN keys k ON k.id=r.key_id WHERE `+where, args...).Scan(&oldest)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		if oldest.Valid {
			start = time.Unix(oldest.Int64, 0).UTC()
		}
	}
	if start.After(now) {
		start = now.Add(-time.Hour)
	}
	// A record is included when its whole second is not after "now", which keeps
	// rows written in the current second visible.
	where += ` AND r.time_unix<=CAST(strftime('%s',?) AS INTEGER)`
	args = append(append([]any{}, args...), now.Format(time.RFC3339Nano))
	stats := performanceStats{Start: start.Format(time.RFC3339Nano), End: now.Format(time.RFC3339Nano), BucketSeconds: chartBucket(now.Sub(start).Seconds()), Series: []performancePoint{}}
	if bounded {
		// Tell the browser when a record next leaves the moving window. Include
		// every matching request, since errors and model-list calls affect totals
		// even though they do not contribute to inference distributions.
		var oldest string
		e = tx.QueryRowContext(ctx, `SELECT r.time FROM requests r LEFT JOIN keys k ON k.id=r.key_id WHERE `+where+` ORDER BY r.time_unix,r.id LIMIT 1`, args...).Scan(&oldest)
		if e != nil && e != sql.ErrNoRows {
			fail(w, 500, "database error")
			return
		}
		if e == nil {
			at, err := time.Parse(time.RFC3339Nano, oldest)
			if err != nil {
				fail(w, 500, "database error")
				return
			}
			// Leave a small margin past SQLite's inclusive, millisecond-precision
			// boundary so browser timer rounding does not immediately rearm it.
			expires := at.Add(now.Sub(start)).Add(50 * time.Millisecond).UTC().Format(time.RFC3339Nano)
			stats.ExpiresAt = &expires
		}
	}
	if e = buildWindow(ctx, tx, where, args, stats.BucketSeconds); e != nil {
		fail(w, 500, "database error")
		return
	}
	stats.Latency, e = distributionFor(ctx, tx, "duration", []float64{0, .1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 300})
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	stats.OutputRate, e = distributionFor(ctx, tx, "rate", []float64{0, 1, 2, 5, 10, 20, 40, 80, 160, 320})
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	rows, e := tx.QueryContext(ctx, `SELECT bucket,count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0),avg(rate),count(rate) FROM `+windowTable+` GROUP BY bucket ORDER BY bucket`)
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

// timeseries returns bucketed totals for the selected window. Bucket width uses
// the same rule as the charts so the response stays bounded for long windows,
// and the window itself follows the same ?since contract as every other
// reporting route instead of an implicit fixed hour.
func (a *App) timeseries(w http.ResponseWriter, since string, where string, args []any) {
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	if since == "" {
		where += ` AND r.time_unix>=?`
		args = append(append([]any{}, args...), start.Unix())
	} else if parsed, e := time.Parse(time.RFC3339Nano, since); e == nil && !parsed.After(now) {
		start = parsed.UTC()
	}
	bucket := chartBucket(now.Sub(start).Seconds())
	query := fmt.Sprintf(`SELECT (r.time_unix/%d)*%d AS stamp,count(*) AS requests,coalesce(sum(r.input_tokens),0) AS input_tokens,coalesce(sum(r.output_tokens),0) AS output_tokens,round(avg(r.duration_ms)) AS avg_ms FROM requests r LEFT JOIN keys k ON k.id=r.key_id WHERE %s GROUP BY stamp ORDER BY stamp`, bucket, bucket, where)
	rows, e := a.db.Query(query, args...)
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var stamp int64
		var requests, input, output, avg int64
		if rows.Scan(&stamp, &requests, &input, &output, &avg) != nil {
			fail(w, 500, "database error")
			return
		}
		out = append(out, map[string]any{"time": time.Unix(stamp, 0).UTC().Format(time.RFC3339), "requests": requests, "input_tokens": input, "output_tokens": output, "avg_ms": avg})
	}
	if rows.Err() != nil {
		fail(w, 500, "database error")
		return
	}
	reply(w, 200, out)
}
