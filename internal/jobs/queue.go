package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"

	appdb "github.com/aidc-lab/ialbum/internal/db"
)

type State string

const (
	Pending     State = "pending"
	Running     State = "running"
	Succeeded   State = "succeeded"
	Failed      State = "failed"
	Cancelled   State = "cancelled"
	WaitingAuth State = "waiting-auth"
)

type Job struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	DedupeKey   string          `json:"dedupeKey"`
	Payload     json.RawMessage `json:"payload"`
	State       State           `json:"state"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"maxAttempts"`
	NextRunAt   time.Time       `json:"nextRunAt"`
	LeaseUntil  *time.Time      `json:"leaseUntil,omitempty"`
	LastError   string          `json:"lastError,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}
type Handler func(context.Context, Job) error
type Queue struct {
	db          *appdb.DB
	lease       time.Duration
	handlers    map[string]Handler
	workerCount int
	wg          sync.WaitGroup
	cancel      context.CancelFunc
}

func New(db *appdb.DB, workerCount int) *Queue {
	if workerCount < 1 {
		workerCount = 1
	}
	return &Queue{db: db, lease: 2 * time.Minute, handlers: map[string]Handler{}, workerCount: workerCount}
}
func (q *Queue) Register(kind string, handler Handler) { q.handlers[kind] = handler }
func (q *Queue) Enqueue(ctx context.Context, kind, dedupe string, payload any, runAt time.Time) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	id := newID()
	now := time.Now().UTC()
	_, err = q.db.ExecContext(ctx, `INSERT INTO jobs(id,type,dedupe_key,payload_json,state,next_run_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(dedupe_key) DO UPDATE SET payload_json=excluded.payload_json,state=CASE WHEN jobs.state IN ('failed','cancelled','waiting-auth') THEN 'pending' ELSE jobs.state END,next_run_at=MIN(jobs.next_run_at,excluded.next_run_at),updated_at=excluded.updated_at`, id, kind, dedupe, string(raw), string(Pending), runAt.Unix(), now.Unix(), now.Unix())
	if err != nil {
		return "", err
	}
	var actual string
	err = q.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE dedupe_key=?`, dedupe).Scan(&actual)
	return actual, err
}
func (q *Queue) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	q.cancel = cancel
	_, _ = q.db.ExecContext(ctx, `UPDATE jobs SET state='pending',lease_until=NULL,heartbeat_at=NULL,updated_at=? WHERE state='running' AND (lease_until IS NULL OR lease_until < ?)`, time.Now().Unix(), time.Now().Unix())
	for i := 0; i < q.workerCount; i++ {
		q.wg.Add(1)
		go q.worker(ctx)
	}
}
func (q *Queue) Stop() {
	if q.cancel != nil {
		q.cancel()
	}
	q.wg.Wait()
}
func (q *Queue) worker(ctx context.Context) {
	defer q.wg.Done()
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			job, err := q.claim(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				continue
			}
			q.run(ctx, job)
		}
	}
}
func (q *Queue) claim(ctx context.Context) (Job, error) {
	now := time.Now().UTC()
	lease := now.Add(q.lease)
	rows, err := q.db.QueryContext(ctx, `SELECT id FROM jobs WHERE state='pending' AND next_run_at<=? ORDER BY next_run_at,created_at LIMIT 10`, now.Unix())
	if err != nil {
		return Job{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		result, err := q.db.ExecContext(ctx, `UPDATE jobs SET state='running',attempts=attempts+1,lease_until=?,heartbeat_at=?,updated_at=? WHERE id=? AND state='pending'`, lease.Unix(), now.Unix(), now.Unix(), id)
		if err != nil {
			continue
		}
		count, _ := result.RowsAffected()
		if count == 1 {
			return q.Get(ctx, id)
		}
	}
	return Job{}, sql.ErrNoRows
}
func (q *Queue) run(parent context.Context, job Job) {
	handler := q.handlers[job.Type]
	if handler == nil {
		q.finish(parent, job, fmt.Errorf("no handler for job type %s", job.Type))
		return
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(q.lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				_, _ = q.db.ExecContext(context.Background(), `UPDATE jobs SET heartbeat_at=?,lease_until=? WHERE id=? AND state='running'`, now.Unix(), now.Add(q.lease).Unix(), job.ID)
			}
		}
	}()
	err := handler(ctx, job)
	close(done)
	q.finish(parent, job, err)
}
func (q *Queue) finish(ctx context.Context, job Job, runErr error) {
	now := time.Now().UTC()
	if runErr == nil {
		_, _ = q.db.ExecContext(ctx, `UPDATE jobs SET state='succeeded',lease_until=NULL,heartbeat_at=NULL,last_error='',updated_at=? WHERE id=?`, now.Unix(), job.ID)
		return
	}
	state := Pending
	next := now.Add(backoff(job.Attempts))
	if errors.Is(runErr, ErrWaitingAuth) {
		state = WaitingAuth
		next = now.Add(24 * time.Hour)
	} else if job.Attempts >= job.MaxAttempts {
		state = Failed
	}
	_, _ = q.db.ExecContext(ctx, `UPDATE jobs SET state=?,next_run_at=?,lease_until=NULL,heartbeat_at=NULL,last_error=?,updated_at=? WHERE id=?`, state, next.Unix(), truncate(runErr.Error(), 2000), now.Unix(), job.ID)
}
func (q *Queue) Get(ctx context.Context, id string) (Job, error) {
	var j Job
	var payload, state string
	var next, created, updated int64
	var lease sql.NullInt64
	err := q.db.QueryRowContext(ctx, `SELECT id,type,dedupe_key,payload_json,state,attempts,max_attempts,next_run_at,lease_until,last_error,created_at,updated_at FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.Type, &j.DedupeKey, &payload, &state, &j.Attempts, &j.MaxAttempts, &next, &lease, &j.LastError, &created, &updated)
	j.Payload = json.RawMessage(payload)
	j.State = State(state)
	j.NextRunAt = time.Unix(next, 0).UTC()
	if lease.Valid {
		value := time.Unix(lease.Int64, 0).UTC()
		j.LeaseUntil = &value
	}
	j.CreatedAt = time.Unix(created, 0).UTC()
	j.UpdatedAt = time.Unix(updated, 0).UTC()
	return j, err
}
func (q *Queue) List(ctx context.Context, state string, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	query := `SELECT id FROM jobs`
	args := []any{}
	if state != "" {
		query += ` WHERE state=?`
		args = append(args, state)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Job, 0)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		job, err := q.Get(ctx, id)
		if err == nil {
			result = append(result, job)
		}
	}
	return result, rows.Err()
}
func (q *Queue) Retry(ctx context.Context, id string) error {
	result, err := q.db.ExecContext(ctx, `UPDATE jobs SET state='pending',attempts=0,next_run_at=?,last_error='',updated_at=? WHERE id=? AND state IN ('failed','waiting-auth','cancelled')`, time.Now().Unix(), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (q *Queue) CancelAlbum(ctx context.Context, albumID string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE jobs SET state='cancelled',updated_at=? WHERE state IN ('pending','waiting-auth') AND json_extract(payload_json,'$.albumId')=?`, time.Now().Unix(), albumID)
	return err
}
func backoff(attempt int) time.Duration {
	seconds := 30 * math.Pow(2, float64(max(attempt-1, 0)))
	if seconds > 21600 {
		seconds = 21600
	}
	return time.Duration(seconds*float64(time.Second)) + time.Duration(rand.IntN(15000))*time.Millisecond
}
func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n]
}
func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

var ErrWaitingAuth = errors.New("storage waiting for authorization")
