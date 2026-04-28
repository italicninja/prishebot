package web

import (
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session holds the state for one authenticated web user.
type Session struct {
	ID          string
	UserID      string
	Username    string
	AvatarURL   string
	AccessToken string  // Discord OAuth2 token (used for future API calls)
	Guilds      []Guild // Servers where this user has admin permissions
	ExpiresAt   time.Time
}

// Guild is a Discord server shown in the dashboard.
type Guild struct {
	ID         string
	Name       string
	IconURL    string
	BotPresent bool // true if the bot is already a member of this server
}

// SessionStore is an in-memory store for web sessions.
//
// Why in-memory? For a skeleton, this keeps the project self-contained with
// no external dependencies. The trade-off is that sessions are lost on restart
// and won't work across multiple instances.
//
// To productionise: replace this with a Redis-backed store. The interface is
// small (Create/Get/Delete) so the swap is straightforward.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewSessionStore creates a store and starts a background cleanup goroutine.
func NewSessionStore() *SessionStore {
	s := &SessionStore{sessions: make(map[string]*Session)}
	go s.cleanup()
	return s
}

// Create stores a new session and returns it.
func (s *SessionStore) Create(userID, username, avatarURL, accessToken string, guilds []Guild) *Session {
	sess := &Session{
		ID:          uuid.New().String(),
		UserID:      userID,
		Username:    username,
		AvatarURL:   avatarURL,
		AccessToken: accessToken,
		Guilds:      guilds,
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	return sess
}

// Get retrieves a session by ID. Returns nil, false if not found or expired.
func (s *SessionStore) Get(id string) (*Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok || time.Now().After(sess.ExpiresAt) {
		return nil, false
	}
	return sess, true
}

// Delete removes a session (used on logout).
func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// cleanup runs every 30 minutes and evicts expired sessions so memory doesn't
// grow indefinitely if many users log in without ever logging out.
func (s *SessionStore) cleanup() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[session] cleanup goroutine panicked: %v", r)
		}
	}()
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		for id, sess := range s.sessions {
			if now.After(sess.ExpiresAt) {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}
