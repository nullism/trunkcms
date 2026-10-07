// Package session implements stateless, AES-GCM encrypted cookie sessions.
// No GitHub token is ever stored: a session is only who you are.
package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

const (
	cookieName = "trunkcms_session"
	stateName  = "trunkcms_oauth"
	// hintName is a readable, non-secret cookie that tells public pages a
	// session probably exists, so only editors load the admin bar. It grants
	// nothing. render.AdminBarLoader checks for it by name.
	hintName = "trunkcms_editor"
	TTL      = 7 * 24 * time.Hour
)

type Session struct {
	Login   string `json:"l"`
	ID      int64  `json:"i"`
	Name    string `json:"n,omitempty"`
	Issued  int64  `json:"t"`
	Expires int64  `json:"e"`
	CSRF    string `json:"c"`
}

type oauthState struct {
	State    string `json:"s"`
	ReturnTo string `json:"r"`
	Expires  int64  `json:"e"`
}

// Manager seals cookies with the first key and opens them with any key,
// so keys can be rotated by prepending a new one.
type Manager struct {
	aeads  []cipher.AEAD
	secure bool
}

func New(keys [][]byte, secure bool) (*Manager, error) {
	if len(keys) == 0 {
		return nil, errors.New("session: at least one key is required")
	}
	m := &Manager{secure: secure}
	for _, k := range keys {
		if len(k) != 32 {
			return nil, errors.New("session: keys must be 32 bytes")
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		m.aeads = append(m.aeads, gcm)
	}
	return m, nil
}

func (m *Manager) seal(name string, v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	a := m.aeads[0]
	nonce := make([]byte, a.NonceSize())
	rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(a.Seal(nonce, nonce, plain, []byte(name))), nil
}

func (m *Manager) open(name, s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	for _, a := range m.aeads {
		if len(raw) < a.NonceSize() {
			continue
		}
		plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte(name))
		if err == nil {
			return json.Unmarshal(plain, v)
		}
	}
	return errors.New("session: invalid cookie")
}

func (m *Manager) cookie(name, value string, maxAge time.Duration) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", HttpOnly: true, Secure: m.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(maxAge.Seconds()),
	}
}

// Start issues a new session cookie.
func (m *Manager) Start(w http.ResponseWriter, login string, id int64, name string) error {
	now := time.Now()
	s := Session{Login: login, ID: id, Name: name, Issued: now.Unix(), Expires: now.Add(TTL).Unix(), CSRF: RandomToken()}
	v, err := m.seal(cookieName, s)
	if err != nil {
		return err
	}
	http.SetCookie(w, m.cookie(cookieName, v, TTL))
	m.SetHint(w, &s)
	return nil
}

// SetHint sets the editor hint cookie to expire with s.
func (m *Manager) SetHint(w http.ResponseWriter, s *Session) {
	c := m.cookie(hintName, "1", time.Until(time.Unix(s.Expires, 0)))
	c.HttpOnly = false
	http.SetCookie(w, c)
}

// HasHint reports whether the request carries the editor hint cookie.
func HasHint(r *http.Request) bool {
	_, err := r.Cookie(hintName)
	return err == nil
}

// Get returns the request's session, or nil.
func (m *Manager) Get(r *http.Request) *Session {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	var s Session
	if m.open(cookieName, c.Value, &s) != nil || time.Now().Unix() > s.Expires {
		return nil
	}
	return &s
}

func (m *Manager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, m.cookie(cookieName, "", -time.Second))
	m.ClearHint(w)
}

func (m *Manager) ClearHint(w http.ResponseWriter) {
	c := m.cookie(hintName, "", -time.Second)
	c.HttpOnly = false
	http.SetCookie(w, c)
}

// BeginOAuth stores a short-lived OAuth state and returns it.
func (m *Manager) BeginOAuth(w http.ResponseWriter, returnTo string) (string, error) {
	st := oauthState{State: RandomToken(), ReturnTo: returnTo, Expires: time.Now().Add(10 * time.Minute).Unix()}
	v, err := m.seal(stateName, st)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, m.cookie(stateName, v, 10*time.Minute))
	return st.State, nil
}

// FinishOAuth validates the returned state and yields the saved return path.
func (m *Manager) FinishOAuth(w http.ResponseWriter, r *http.Request, state string) (string, error) {
	http.SetCookie(w, m.cookie(stateName, "", -time.Second))
	c, err := r.Cookie(stateName)
	if err != nil {
		return "", errors.New("login expired, please try again")
	}
	var st oauthState
	if err := m.open(stateName, c.Value, &st); err != nil || st.State != state || time.Now().Unix() > st.Expires {
		return "", errors.New("login state mismatch, please try again")
	}
	return st.ReturnTo, nil
}

func RandomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
