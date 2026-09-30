package session

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func roundTrip(t *testing.T, issue, read *Manager) *Session {
	w := httptest.NewRecorder()
	if err := issue.Start(w, "alice", 42, "Alice"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	return read.Get(r)
}

func TestSessionRoundTripAndRotation(t *testing.T) {
	oldKey, newKey := bytes.Repeat([]byte("a"), 32), bytes.Repeat([]byte("b"), 32)
	old, _ := New([][]byte{oldKey}, true)
	rotated, _ := New([][]byte{newKey, oldKey}, true)
	other, _ := New([][]byte{newKey}, true)

	if s := roundTrip(t, old, old); s == nil || s.Login != "alice" || s.ID != 42 || s.CSRF == "" {
		t.Fatalf("round trip: %+v", s)
	}
	if roundTrip(t, old, rotated) == nil {
		t.Fatal("rotated manager should still accept cookies sealed with the old key")
	}
	if roundTrip(t, old, other) != nil {
		t.Fatal("cookie sealed with an unknown key must be rejected")
	}
}

func TestTamperedCookieRejected(t *testing.T) {
	m, _ := New([][]byte{bytes.Repeat([]byte("a"), 32)}, false)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})
	if m.Get(r) != nil {
		t.Fatal("garbage cookie accepted")
	}
}

func TestOAuthState(t *testing.T) {
	m, _ := New([][]byte{bytes.Repeat([]byte("a"), 32)}, false)
	w := httptest.NewRecorder()
	state, _ := m.BeginOAuth(w, "/admin/posts")
	r := httptest.NewRequest("GET", "/admin/callback", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	if next, err := m.FinishOAuth(httptest.NewRecorder(), r, state); err != nil || next != "/admin/posts" {
		t.Fatalf("finish: %q %v", next, err)
	}
	if _, err := m.FinishOAuth(httptest.NewRecorder(), r, "forged"); err == nil {
		t.Fatal("forged state accepted")
	}
}
