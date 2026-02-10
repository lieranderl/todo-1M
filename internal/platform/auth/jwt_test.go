package auth

import (
	"errors"
	"testing"
	"time"
)

func TestManager_SignAndParse(t *testing.T) {
	m := NewManager("secret", time.Hour)
	now := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }

	tok, err := m.Sign("u1", "alice")
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	claims, err := m.Parse(tok)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if claims.Subject != "u1" || claims.Username != "alice" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestManager_ParseExpired(t *testing.T) {
	m := NewManager("secret", time.Second)
	now := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	tok, err := m.Sign("u1", "alice")
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	m.Now = func() time.Time { return now.Add(2 * time.Second) }
	_, err = m.Parse(tok)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("expected ErrExpiredToken, got %v", err)
	}
}

func TestBearerToken(t *testing.T) {
	if got := BearerToken("Bearer abc"); got != "abc" {
		t.Fatalf("unexpected token: %q", got)
	}
	if got := BearerToken("Basic abc"); got != "" {
		t.Fatalf("expected empty token, got %q", got)
	}
}
