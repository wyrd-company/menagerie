// ---
// relationships:
//   implements: authentication
// ---

package auth

import (
	"container/list"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
)

// maxPendingLogins bounds the sign-ins started but not yet completed.
// Starting a sign-in needs no session, so the bound keeps unauthenticated
// requests from growing memory; the oldest pending sign-in is dropped
// first, and its callback then asks the browser to sign in again.
const maxPendingLogins = 256

// tokenLength is the length of a randomToken.
const tokenLength = 43

// randomToken returns 32 random bytes, base64url without padding.
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b)
}

func equalTokens(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// pendingLogin is one sign-in between the authorization request and its
// callback.
type pendingLogin struct {
	// binding is the browser's login cookie value; the callback must
	// present it, so a callback started in another browser is refused.
	binding  string
	verifier string
	nonce    string
	returnTo string
}

// pendingLogins holds pending sign-ins by state. Each is taken at most
// once.
type pendingLogins struct {
	mu    sync.Mutex
	max   int
	order *list.List // of state, oldest first
	byKey map[string]*list.Element
	value map[string]pendingLogin
}

func newPendingLogins(maxEntries int) *pendingLogins {
	return &pendingLogins{max: maxEntries, order: list.New(), byKey: map[string]*list.Element{}, value: map[string]pendingLogin{}}
}

func (p *pendingLogins) add(state string, l pendingLogin) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.order.Len() >= p.max {
		oldest := p.order.Front()
		key := oldest.Value.(string)
		p.order.Remove(oldest)
		delete(p.byKey, key)
		delete(p.value, key)
	}
	p.byKey[state] = p.order.PushBack(state)
	p.value[state] = l
}

// take removes and returns the pending sign-in for state.
func (p *pendingLogins) take(state string) (pendingLogin, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byKey[state]
	if !ok {
		return pendingLogin{}, false
	}
	l := p.value[state]
	p.order.Remove(e)
	delete(p.byKey, state)
	delete(p.value, state)
	return l, true
}

// Session is a signed-in manager.
type Session struct {
	Subject string
	Name    string
}

// sessions holds manager sessions by id, in memory only. A session ends at
// sign-out or when the process exits.
type sessions struct {
	mu   sync.Mutex
	byID map[string]Session
}

func newSessions() *sessions { return &sessions{byID: map[string]Session{}} }

func (s *sessions) create(sess Session) string {
	id := randomToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[id] = sess
	return id
}

func (s *sessions) get(id string) (Session, bool) {
	if id == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	return sess, ok
}

func (s *sessions) end(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}
