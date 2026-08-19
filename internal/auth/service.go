package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	appdb "github.com/aidc-lab/ialbum/internal/db"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrSetupClosed        = errors.New("setup is already complete")
	ErrInvalidSetupToken  = errors.New("invalid or expired setup token")
	ErrInvalidSession     = errors.New("invalid session")
	ErrInvalidCSRF        = errors.New("invalid csrf token")
	ErrRateLimited        = errors.New("too many login attempts")
)

type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"createdAt"`
}

type Session struct {
	ID, UserID, Username, Token, CSRFToken string
	CreatedAt, LastSeenAt, ExpiresAt       time.Time
}

type loginAttempt struct {
	count                     int
	blockedUntil, lastAttempt time.Time
}

type Service struct {
	db                   *appdb.DB
	setupToken           string
	setupExpires         time.Time
	idleTTL, absoluteTTL time.Duration
	mu                   sync.Mutex
	attempts             map[string]loginAttempt
}

func NewService(db *appdb.DB, setupTTL, idleTTL, absoluteTTL time.Duration) (*Service, error) {
	s := &Service{db: db, idleTTL: idleTTL, absoluteTTL: absoluteTTL, attempts: map[string]loginAttempt{}}
	hasUser, err := db.HasUser(context.Background())
	if err != nil {
		return nil, err
	}
	if !hasUser {
		token, err := randomToken(32)
		if err != nil {
			return nil, err
		}
		s.setupToken = token
		s.setupExpires = time.Now().Add(setupTTL)
	}
	return s, nil
}

func (s *Service) SetupToken() (string, time.Time, bool) {
	if s.setupToken == "" {
		return "", time.Time{}, false
	}
	return s.setupToken, s.setupExpires, true
}

func (s *Service) IsSetup(ctx context.Context) (bool, error) { return s.db.HasUser(ctx) }

func (s *Service) Setup(ctx context.Context, token, username, password string) (User, error) {
	hasUser, err := s.db.HasUser(ctx)
	if err != nil {
		return User{}, err
	}
	if hasUser {
		return User{}, ErrSetupClosed
	}
	if time.Now().After(s.setupExpires) || subtle.ConstantTimeCompare([]byte(token), []byte(s.setupToken)) != 1 {
		return User{}, ErrInvalidSetupToken
	}
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 64 {
		return User{}, errors.New("username must contain 3 to 64 characters")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	id := newID()
	_, err = s.db.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES(?,?,?,?,?)`, id, username, hash, now.Unix(), now.Unix())
	if err != nil {
		return User{}, fmt.Errorf("create administrator: %w", err)
	}
	s.setupToken = ""
	s.setupExpires = time.Time{}
	return User{ID: id, Username: username, CreatedAt: now}, nil
}

func (s *Service) Login(ctx context.Context, username, password, remoteKey string) (Session, error) {
	key := strings.ToLower(strings.TrimSpace(username)) + "|" + remoteKey
	if s.isBlocked(key) {
		return Session{}, ErrRateLimited
	}
	var user User
	var passwordHash string
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, username, password_hash, created_at FROM users WHERE username = ? COLLATE NOCASE`, strings.TrimSpace(username)).Scan(&user.ID, &user.Username, &passwordHash, &created)
	if err != nil || !VerifyPassword(passwordHash, password) {
		s.recordFailure(key)
		return Session{}, ErrInvalidCredentials
	}
	s.clearFailure(key)
	user.CreatedAt = time.Unix(created, 0).UTC()
	return s.newSession(ctx, user)
}

func (s *Service) newSession(ctx context.Context, user User) (Session, error) {
	token, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(s.absoluteTTL)
	id := newID()
	tokenHash := sha256.Sum256([]byte(token))
	csrfHash := sha256.Sum256([]byte(csrf))
	_, err = s.db.ExecContext(ctx, `INSERT INTO sessions(id,user_id,token_hash,csrf_hash,created_at,last_seen_at,expires_at) VALUES(?,?,?,?,?,?,?)`, id, user.ID, tokenHash[:], csrfHash[:], now.Unix(), now.Unix(), expires.Unix())
	if err != nil {
		return Session{}, fmt.Errorf("save session: %w", err)
	}
	return Session{ID: id, UserID: user.ID, Username: user.Username, Token: token, CSRFToken: csrf, CreatedAt: now, LastSeenAt: now, ExpiresAt: expires}, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrInvalidSession
	}
	tokenHash := sha256.Sum256([]byte(token))
	var session Session
	var created, lastSeen, expires int64
	err := s.db.QueryRowContext(ctx, `SELECT s.id,s.user_id,u.username,s.created_at,s.last_seen_at,s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=?`, tokenHash[:]).Scan(&session.ID, &session.UserID, &session.Username, &created, &lastSeen, &expires)
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	now := time.Now().UTC()
	session.CreatedAt = time.Unix(created, 0).UTC()
	session.LastSeenAt = time.Unix(lastSeen, 0).UTC()
	session.ExpiresAt = time.Unix(expires, 0).UTC()
	if now.After(session.ExpiresAt) || now.Sub(session.LastSeenAt) > s.idleTTL {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, session.ID)
		return Session{}, ErrInvalidSession
	}
	if now.Sub(session.LastSeenAt) > 5*time.Minute {
		_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at=? WHERE id=?`, now.Unix(), session.ID)
		session.LastSeenAt = now
	}
	return session, nil
}

func (s *Service) ValidateCSRF(ctx context.Context, sessionID, token string) error {
	hash := sha256.Sum256([]byte(token))
	var stored []byte
	if err := s.db.QueryRowContext(ctx, `SELECT csrf_hash FROM sessions WHERE id=?`, sessionID).Scan(&stored); err != nil {
		return ErrInvalidCSRF
	}
	if subtle.ConstantTimeCompare(hash[:], stored) != 1 {
		return ErrInvalidCSRF
	}
	return nil
}

func (s *Service) CSRFToken(ctx context.Context, sessionID, rawSessionToken string) (string, error) {
	// CSRF values are intentionally not recoverable. Rotate to a fresh value whenever /me is requested.
	csrf, err := randomToken(32)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(csrf))
	tokenHash := sha256.Sum256([]byte(rawSessionToken))
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET csrf_hash=? WHERE id=? AND token_hash=?`, hash[:], sessionID, tokenHash[:])
	if err != nil {
		return "", err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return "", ErrInvalidSession
	}
	return csrf, nil
}

func (s *Service) Logout(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, sessionID)
	return err
}

func (s *Service) ChangePassword(ctx context.Context, userID, current, next string) error {
	var encoded string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&encoded); err != nil {
		return err
	}
	if !VerifyPassword(encoded, current) {
		return ErrInvalidCredentials
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=?,updated_at=? WHERE id=?`, hash, time.Now().Unix(), userID); err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func ResetPassword(ctx context.Context, databasePath, username, password string) error {
	db, err := appdb.Open(databasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?,updated_at=? WHERE username=? COLLATE NOCASE`, hash, time.Now().Unix(), username)
	if err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions`)
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		_ = tx.Rollback()
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Service) isBlocked(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt := s.attempts[key]
	return time.Now().Before(attempt.blockedUntil)
}
func (s *Service) recordFailure(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[key]
	now := time.Now()
	if now.Sub(a.lastAttempt) > 15*time.Minute {
		a.count = 0
	}
	a.count++
	a.lastAttempt = now
	if a.count >= 5 {
		a.blockedUntil = now.Add(time.Duration(a.count-4) * time.Minute)
	}
	s.attempts[key] = a
}
func (s *Service) clearFailure(key string) { s.mu.Lock(); delete(s.attempts, key); s.mu.Unlock() }

func randomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
