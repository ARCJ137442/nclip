// Package accounts owns the small single-server account database. Sharing stays
// link-based: no groups, share ACLs, or per-share ownership entities are created.
package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Permissions are independent. Manage grants administration, not content reading.
type Permissions struct {
	Upload   bool `json:"upload"`
	Download bool `json:"download"`
	Manage   bool `json:"manage"`
}

// User is the public account representation. Password hashes never leave Store.
type User struct {
	ID          string      `json:"id"`
	Username    string      `json:"username"`
	Permissions Permissions `json:"permissions"`
	Disabled    bool        `json:"disabled"`
	CreatedAt   time.Time   `json:"created_at"`
	Version     uint64      `json:"version"`
}

type record struct {
	User
	PasswordHash string `json:"password_hash"`
}

// Session contains no raw authentication token; the map key is its SHA-256 hash.
type Session struct {
	UserID    string    `json:"user_id"`
	Revision  uint64    `json:"revision"`
	CSRF      string    `json:"csrf"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Authentication binds a verified password to the exact account revision.
type Authentication struct {
	User
	revision uint64
}

// UserChange names the complete account update and its optimistic version.
type UserChange struct {
	ID, Username, Password string
	Permissions            Permissions
	Disabled               bool
	Version                uint64
}

type database struct {
	Version  int                `json:"version"`
	Users    map[string]record  `json:"users"`
	Sessions map[string]Session `json:"sessions"`
}

// Store uses an atomically replaced private JSON file and an in-process lock.
// Multiple application writers against the same account file are unsupported.
type Store struct {
	mu        sync.RWMutex
	path      string
	state     database
	dummyHash string
}

const passwordCost = 12
const sessionLifetime = 7 * 24 * time.Hour
const maxSessionsPerUser = 10
const maxUsers = 1000

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{2,63}$`)

// ErrInput is safe to expose as a machine-readable validation code.
type ErrInput string

func (e ErrInput) Error() string { return string(e) }

// Open loads accounts, or bootstraps exactly one manager from a private password
// file on first startup. Later startups ignore bootstrap credentials entirely.
func Open(dir, username, passwordFile string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "accounts.json")}
	data, err := os.ReadFile(s.path) // #nosec G304 -- configured private account directory
	if err == nil {
		if err := json.Unmarshal(data, &s.state); err != nil {
			return nil, fmt.Errorf("decode accounts: %w", err)
		}
		if s.state.Version != 1 || s.state.Users == nil || s.state.Sessions == nil || !hasManager(s.state.Users) {
			return nil, fmt.Errorf("invalid account database")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := s.bootstrap(username, passwordFile); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("nclip-invalid-account-placeholder"), passwordCost)
	if err != nil {
		return nil, err
	}
	s.dummyHash = string(hash)
	return s, nil
}

func token() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func tokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func passwordHash(password string) (string, error) {
	if len(password) < 8 || len(password) > 72 {
		return "", ErrInput("password_length")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), passwordCost)
	return string(hash), err
}

func normalizeUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(username) {
		return "", ErrInput("invalid_username")
	}
	return username, nil
}

func (s *Store) bootstrap(username, passwordFile string) error {
	name, err := normalizeUsername(username)
	if err != nil {
		return err
	}
	if passwordFile == "" {
		return fmt.Errorf("first accounts startup requires an administrator password file")
	}
	password, err := os.ReadFile(passwordFile) // #nosec G304 -- explicit administrator secret file
	if err != nil {
		return fmt.Errorf("read bootstrap secret: %w", err)
	}
	hash, err := passwordHash(strings.TrimRight(string(password), "\r\n"))
	if err != nil {
		return err
	}
	id, err := token()
	if err != nil {
		return err
	}
	state := database{Version: 1, Users: map[string]record{id: {User: User{ID: id, Username: name, Permissions: Permissions{Upload: true, Download: true, Manage: true}, CreatedAt: time.Now().UTC(), Version: 1}, PasswordHash: hash}}, Sessions: map[string]Session{}}
	return s.commit(state)
}

func hasManager(users map[string]record) bool {
	for _, u := range users {
		if !u.Disabled && u.Permissions.Manage {
			return true
		}
	}
	return false
}

func (s *Store) clone() database {
	return database{Version: 1, Users: maps.Clone(s.state.Users), Sessions: maps.Clone(s.state.Sessions)}
}

// commit publishes in-memory state only after the file replacement succeeds.
func (s *Store) commit(next database) (result error) {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), "accounts-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	s.state = next
	return nil
}

// Authenticate performs equivalent-cost verification for unknown usernames.
func (s *Store) Authenticate(username, password string) (Authentication, error) {
	name := strings.ToLower(strings.TrimSpace(username))
	s.mu.RLock()
	var found record
	for _, u := range s.state.Users {
		if u.Username == name {
			found = u
			break
		}
	}
	hash := s.dummyHash
	if found.ID != "" {
		hash = found.PasswordHash
	}
	s.mu.RUnlock()
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if err != nil || found.ID == "" || found.Disabled {
		return Authentication{}, ErrInput("invalid_credentials")
	}
	return Authentication{User: found.User, revision: found.Version}, nil
}

// NewSession rotates a fresh opaque token after credential authentication.
func (s *Store) NewSession(auth Authentication) (string, Session, error) {
	raw, err := token()
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := token()
	if err != nil {
		return "", Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	userID := auth.ID
	u, ok := s.state.Users[userID]
	if !ok || u.Disabled || u.Version != auth.revision {
		return "", Session{}, ErrInput("invalid_credentials")
	}
	next := s.clone()
	now := time.Now().UTC()
	var existing []string
	for key, v := range next.Sessions {
		if !v.ExpiresAt.After(now) {
			delete(next.Sessions, key)
		} else if v.UserID == userID {
			existing = append(existing, key)
		}
	}
	sort.Slice(existing, func(i, j int) bool {
		return next.Sessions[existing[i]].ExpiresAt.Before(next.Sessions[existing[j]].ExpiresAt)
	})
	for len(existing) >= maxSessionsPerUser {
		delete(next.Sessions, existing[0])
		existing = existing[1:]
	}
	session := Session{UserID: userID, Revision: u.Version, CSRF: csrf, ExpiresAt: now.Add(sessionLifetime)}
	next.Sessions[tokenHash(raw)] = session
	if err := s.commit(next); err != nil {
		return "", Session{}, err
	}
	return raw, session, nil
}

// Current checks both expiration and the current account revision on every request.
func (s *Store) Current(raw string) (User, Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.state.Sessions[tokenHash(raw)]
	if !ok || !session.ExpiresAt.After(time.Now()) {
		return User{}, Session{}, false
	}
	u, ok := s.state.Users[session.UserID]
	if !ok || u.Disabled || u.Version != session.Revision {
		return User{}, Session{}, false
	}
	return u.User, session, true
}

// Logout removes the current opaque session without affecting other devices.
func (s *Store) Logout(raw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	delete(next.Sessions, tokenHash(raw))
	return s.commit(next)
}

// List returns credential-free records in stable username order.
func (s *Store) List() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users := make([]User, 0, len(s.state.Users))
	for _, u := range s.state.Users {
		users = append(users, u.User)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	return users
}

// SaveUser creates or replaces an account. A blank password preserves an existing
// hash. Actor authorization and the last-manager invariant share the write lock.
func (s *Store) SaveUser(actorID string, change UserChange) (User, error) {
	name, err := normalizeUsername(change.Username)
	if err != nil {
		return User{}, err
	}
	var hash string
	if change.Password != "" {
		hash, err = passwordHash(change.Password)
		if err != nil {
			return User{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireManager(actorID); err != nil {
		return User{}, err
	}
	for key, u := range s.state.Users {
		if key != change.ID && u.Username == name {
			return User{}, ErrInput("username_exists")
		}
	}
	u, err := s.editRecord(change.ID, hash)
	if err != nil {
		return User{}, err
	}
	if change.ID != "" && u.Version != change.Version {
		return User{}, ErrInput("user_changed")
	}
	u.Username = name
	u.Permissions = change.Permissions
	u.Disabled = change.Disabled
	u.Version++
	next := s.clone()
	next.Users[u.ID] = u
	if !hasManager(next.Users) {
		return User{}, ErrInput("last_manager")
	}
	revokeSessions(&next, u.ID)
	if err := s.commit(next); err != nil {
		return User{}, err
	}
	return u.User, nil
}

func (s *Store) requireManager(id string) error {
	u, ok := s.state.Users[id]
	if !ok || u.Disabled || !u.Permissions.Manage {
		return ErrInput("permission_denied")
	}
	return nil
}

func (s *Store) editRecord(id, hash string) (record, error) {
	if id != "" {
		u, ok := s.state.Users[id]
		if !ok {
			return record{}, ErrInput("user_not_found")
		}
		if hash != "" {
			u.PasswordHash = hash
		}
		return u, nil
	}
	if hash == "" {
		return record{}, ErrInput("password_required")
	}
	if len(s.state.Users) >= maxUsers {
		return record{}, ErrInput("user_limit")
	}
	id, err := token()
	if err != nil {
		return record{}, err
	}
	return record{User: User{ID: id, CreatedAt: time.Now().UTC()}, PasswordHash: hash}, nil
}

func revokeSessions(next *database, userID string) {
	for key, v := range next.Sessions {
		if v.UserID == userID {
			delete(next.Sessions, key)
		}
	}
}

// DeleteUser atomically prevents deleting the last active manager.
func (s *Store) DeleteUser(actorID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireManager(actorID); err != nil {
		return err
	}
	if _, ok := s.state.Users[id]; !ok {
		return ErrInput("user_not_found")
	}
	next := s.clone()
	delete(next.Users, id)
	revokeSessions(&next, id)
	if !hasManager(next.Users) {
		return ErrInput("last_manager")
	}
	return s.commit(next)
}
