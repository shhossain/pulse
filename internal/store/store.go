// Package store persists jobs, runs and metric series in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"pulse/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id         TEXT PRIMARY KEY,
  job        TEXT NOT NULL,
  seq        INTEGER NOT NULL,
  status     TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  data       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_job ON runs(job, seq);
CREATE INDEX IF NOT EXISTS runs_updated ON runs(updated_at);

CREATE TABLE IF NOT EXISTS series (
  run TEXT NOT NULL,
  key TEXT NOT NULL,
  x   REAL,
  ts  INTEGER NOT NULL,
  v   REAL,          -- NULL when not finite (see nf)
  nf  TEXT           -- 'NaN', 'Inf', '-Inf'
);
CREATE INDEX IF NOT EXISTS series_run_key ON series(run, key, ts);

CREATE TABLE IF NOT EXISTS jobs (
  name   TEXT PRIMARY KEY,
  config TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS asks (
  id     TEXT PRIMARY KEY,
  run    TEXT NOT NULL,
  open   INTEGER NOT NULL,
  data   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tokens (
  id        TEXT PRIMARY KEY,
  hash      TEXT NOT NULL UNIQUE,
  job       TEXT NOT NULL,
  name      TEXT NOT NULL DEFAULT '',
  created   INTEGER NOT NULL,
  last_used INTEGER
);
`

// Store is safe for concurrent use; writes are serialized on one connection.
type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	// Escape the path: '#', '?' and '%' in a folder name would otherwise be
	// read as URI syntax and open a different file.
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	dsn := "file:" + esc + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrations upgrade the database one version at a time (PRAGMA
// user_version). Append only: never edit a migration that has shipped.
var migrations = []string{
	schema, // 1: runs, series, jobs, asks, tokens
}

// SchemaVersion is the version this build expects.
func SchemaVersion() int { return len(migrations) }

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("this database was written by a newer Pulse (schema %d, this build knows %d); update Pulse", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Prune deletes runs that finished before cutoff (with their series and
// questions) and returns their ids so logs can be removed too.
func (s *Store) Prune(cutoff time.Time) ([]string, error) {
	// Never the latest run of a job: it's what the job shows, and it carries
	// the run counter (the next run must not restart at #1).
	rows, err := s.db.Query(`SELECT id FROM runs r WHERE updated_at < ? AND status IN ('done','failed','stopped','lost','idle')
		AND seq < (SELECT MAX(seq) FROM runs WHERE job = r.job)`, cutoff.UnixMilli())
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, id := range ids {
		for _, q := range []string{`DELETE FROM series WHERE run = ?`, `DELETE FROM asks WHERE run = ?`, `DELETE FROM runs WHERE id = ?`} {
			if _, err := tx.Exec(q, id); err != nil {
				return nil, err
			}
		}
	}
	return ids, tx.Commit()
}

// Vacuum reclaims space after pruning.
func (s *Store) Vacuum() error {
	_, err := s.db.Exec(`VACUUM`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Save upserts a run and appends series points in one transaction.
func (s *Store) Save(r *model.Run, points []model.Point) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO runs(id, job, seq, status, updated_at, data) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET job=excluded.job, seq=excluded.seq, status=excluded.status,
		updated_at=excluded.updated_at, data=excluded.data`,
		r.ID, r.Job, r.Seq, string(r.Status), r.UpdatedAt.UnixMilli(), string(data)); err != nil {
		return err
	}
	if len(points) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO series(run, key, x, ts, v, nf) VALUES(?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, p := range points {
			var v, nf any
			switch {
			case math.IsNaN(p.V):
				nf = "NaN"
			case math.IsInf(p.V, 1):
				nf = "Inf"
			case math.IsInf(p.V, -1):
				nf = "-Inf"
			default:
				v = p.V
			}
			var x any
			if p.X != nil {
				x = *p.X
			}
			if _, err := stmt.Exec(p.Run, p.Key, x, p.TS.UnixMilli(), v, nf); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Recent loads runs updated since t, plus any run still marked running.
func (s *Store) Recent(since time.Time) ([]*model.Run, error) {
	rows, err := s.db.Query(`SELECT data FROM runs WHERE updated_at >= ? OR status = 'running' ORDER BY updated_at`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		r := &model.Run{}
		if err := json.Unmarshal([]byte(data), r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Run loads one run by id (nil if missing).
func (s *Store) Run(id string) (*model.Run, error) {
	var data string
	err := s.db.QueryRow(`SELECT data FROM runs WHERE id = ?`, id).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := &model.Run{}
	return r, json.Unmarshal([]byte(data), r)
}

// MaxSeq returns the highest run number for a job (0 if none).
func (s *Store) MaxSeq(job string) (int, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(seq) FROM runs WHERE job = ?`, job).Scan(&n)
	return int(n.Int64), err
}

// SeriesPoint is one stored sample. X is the step (NaN if unknown); TS is unix ms.
type SeriesPoint struct {
	X  float64
	TS int64
	V  float64 // NaN/±Inf for non-finite samples
}

// Series returns all samples of one metric of a run, oldest first.
func (s *Store) Series(run, key string) ([]SeriesPoint, error) {
	rows, err := s.db.Query(`SELECT x, ts, v, nf FROM series WHERE run = ? AND key = ? ORDER BY ts, rowid`, run, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var x, v sql.NullFloat64
		var nf sql.NullString
		var p SeriesPoint
		if err := rows.Scan(&x, &p.TS, &v, &nf); err != nil {
			return nil, err
		}
		p.X = math.NaN()
		if x.Valid {
			p.X = x.Float64
		}
		switch {
		case v.Valid:
			p.V = v.Float64
		case nf.String == "Inf":
			p.V = math.Inf(1)
		case nf.String == "-Inf":
			p.V = math.Inf(-1)
		default:
			p.V = math.NaN()
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// JobRuns returns up to limit runs of a job, newest first.
func (s *Store) JobRuns(job string, limit int) ([]*model.Run, error) {
	rows, err := s.db.Query(`SELECT data FROM runs WHERE job = ? ORDER BY seq DESC LIMIT ?`, job, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		r := &model.Run{}
		if json.Unmarshal([]byte(data), r) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// SaveJob stores a job's config (JSON).
func (s *Store) SaveJob(name string, config []byte) error {
	_, err := s.db.Exec(`INSERT INTO jobs(name, config) VALUES(?, ?)
		ON CONFLICT(name) DO UPDATE SET config = excluded.config`, name, string(config))
	return err
}

// Jobs returns every stored job config.
func (s *Store) Jobs() (map[string][]byte, error) {
	rows, err := s.db.Query(`SELECT name, config FROM jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var name, cfg string
		if err := rows.Scan(&name, &cfg); err != nil {
			return nil, err
		}
		out[name] = []byte(cfg)
	}
	return out, rows.Err()
}

// DeleteJob removes a job's runs, series and config. It returns the run ids
// that were deleted (so their logs can be removed too).
func (s *Store) DeleteJob(job string) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM runs WHERE job = ?`, job)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, q := range []string{
		`DELETE FROM series WHERE run IN (SELECT id FROM runs WHERE job = ?)`,
		`DELETE FROM runs WHERE job = ?`,
		`DELETE FROM jobs WHERE name = ?`,
	} {
		if _, err := tx.Exec(q, job); err != nil {
			return nil, err
		}
	}
	return ids, tx.Commit()
}

// RunBySeq loads a job's run by number (nil if missing).
func (s *Store) RunBySeq(job string, seq int) (*model.Run, error) {
	var data string
	err := s.db.QueryRow(`SELECT data FROM runs WHERE job = ? AND seq = ?`, job, seq).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := &model.Run{}
	return r, json.Unmarshal([]byte(data), r)
}

// CountRuns returns the number of runs of a job.
func (s *Store) CountRuns(job string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE job = ?`, job).Scan(&n)
	return n, err
}

// TokenRow is a stored job token (hash only).
type TokenRow struct {
	ID, Hash, Job, Name string
	Created             time.Time
	LastUsed            *time.Time
}

func (s *Store) SaveToken(id, hash, job, name string, created time.Time) error {
	_, err := s.db.Exec(`INSERT INTO tokens(id, hash, job, name, created) VALUES(?,?,?,?,?)`, id, hash, job, name, created.UnixMilli())
	return err
}

func (s *Store) DeleteToken(id string) error {
	_, err := s.db.Exec(`DELETE FROM tokens WHERE id = ?`, id)
	return err
}

func (s *Store) TouchToken(id string, t time.Time) error {
	_, err := s.db.Exec(`UPDATE tokens SET last_used = ? WHERE id = ?`, t.UnixMilli(), id)
	return err
}

func (s *Store) Tokens() ([]TokenRow, error) {
	rows, err := s.db.Query(`SELECT id, hash, job, name, created, last_used FROM tokens ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRow
	for rows.Next() {
		var t TokenRow
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Hash, &t.Job, &t.Name, &created, &used); err != nil {
			return nil, err
		}
		t.Created = time.UnixMilli(created).UTC()
		if used.Valid {
			u := time.UnixMilli(used.Int64).UTC()
			t.LastUsed = &u
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveAsk stores a question (open or answered).
func (s *Store) SaveAsk(a model.Ask) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	open := 0
	if a.Open() {
		open = 1
	}
	_, err = s.db.Exec(`INSERT INTO asks(id, run, open, data) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET open = excluded.open, data = excluded.data`, a.ID, a.Run, open, string(b))
	return err
}

// RecentAsks loads unanswered questions and ones answered since t.
func (s *Store) RecentAsks(since time.Time) ([]model.Ask, error) {
	rows, err := s.db.Query(`SELECT data FROM asks WHERE open = 1 OR json_extract(data, '$.answered_at') >= ?`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Ask
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var a model.Ask
		if json.Unmarshal([]byte(data), &a) == nil {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// JobNames lists every job that has runs or settings.
func (s *Store) JobNames() ([]string, error) {
	rows, err := s.db.Query(`SELECT job FROM runs UNION SELECT name FROM jobs ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
