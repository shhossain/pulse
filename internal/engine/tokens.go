package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"pulse/internal/audit"
	"pulse/internal/paths"
)

// Token is a job-scoped push token. Only its hash is stored; the plain
// value is shown once, when it's created.
type Token struct {
	ID       string     `json:"id"`
	Job      string     `json:"job"`
	Name     string     `json:"name,omitempty"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used,omitempty"`
	hash     string
}

// TokenPrefix marks Pulse job tokens (easy to spot in secret scanners).
const TokenPrefix = "pls_"

func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func (e *Engine) loadTokens() error {
	rows, err := e.st.Tokens()
	if err != nil {
		return err
	}
	for _, t := range rows {
		tok := &Token{ID: t.ID, Job: t.Job, Name: t.Name, Created: t.Created, LastUsed: t.LastUsed, hash: t.Hash}
		e.tokens[t.Hash] = tok
	}
	return nil
}

// CreateToken makes a token that can only push to job. It returns the
// plain token (shown once) and its record.
func (e *Engine) CreateToken(job, name string) (string, Token, error) {
	job = CleanJobName(job)
	plain := TokenPrefix + paths.RandomHex(24)
	t := &Token{ID: "t_" + paths.RandomHex(4), Job: job, Name: name, Created: e.now().UTC(), hash: hashToken(plain)}
	if err := e.st.SaveToken(t.ID, t.hash, t.Job, t.Name, t.Created); err != nil {
		return "", Token{}, err
	}
	e.mu.Lock()
	e.tokens[t.hash] = t
	e.mu.Unlock()
	audit.Log("token.created", map[string]any{"id": t.ID, "job": t.Job, "name": t.Name})
	return plain, *t, nil
}

// Tokens lists job tokens (without their values).
func (e *Engine) Tokens() []Token {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Token, 0, len(e.tokens))
	for _, t := range e.tokens {
		out = append(out, *t)
	}
	return out
}

// RevokeToken deletes a job token by id.
func (e *Engine) RevokeToken(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for h, t := range e.tokens {
		if t.ID == id {
			if err := e.st.DeleteToken(id); err != nil {
				return err
			}
			delete(e.tokens, h)
			audit.Log("token.revoked", map[string]any{"id": id, "job": t.Job})
			return nil
		}
	}
	return fmt.Errorf("no token %q", id)
}

// LookupToken returns the job a plain job token may push to.
func (e *Engine) LookupToken(plain string) (string, bool) {
	if len(plain) < len(TokenPrefix) || plain[:len(TokenPrefix)] != TokenPrefix {
		return "", false
	}
	h := hashToken(plain)
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tokens[h]
	if !ok {
		return "", false
	}
	if t.LastUsed == nil || e.now().Sub(*t.LastUsed) > time.Minute { // don't write on every push
		now := e.now().UTC()
		t.LastUsed = &now
		_ = e.st.TouchToken(t.ID, now)
	}
	return t.Job, true
}

// RunJob returns the job a run belongs to ("" if unknown).
func (e *Engine) RunJob(id string) string {
	e.mu.Lock()
	r := e.runs[id]
	e.mu.Unlock()
	if r != nil {
		return r.Job
	}
	if stored, err := e.st.Run(id); err == nil && stored != nil {
		return stored.Job
	}
	return ""
}
