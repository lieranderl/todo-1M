package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("expired token")
)

type Claims struct {
	Subject  string `json:"sub"`
	Username string `json:"username"`
	Exp      int64  `json:"exp"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type Manager struct {
	Secret []byte
	Now    func() time.Time
	TTL    time.Duration
}

func NewManager(secret string, ttl time.Duration) Manager {
	return Manager{
		Secret: []byte(secret),
		Now:    func() time.Time { return time.Now().UTC() },
		TTL:    ttl,
	}
}

func (m Manager) Sign(userID, username string) (string, error) {
	h := header{Alg: "HS256", Typ: "JWT"}
	payload := Claims{
		Subject:  userID,
		Username: username,
		Exp:      m.Now().Add(m.TTL).Unix(),
	}

	hb, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	hEnc := base64.RawURLEncoding.EncodeToString(hb)
	pEnc := base64.RawURLEncoding.EncodeToString(pb)
	signed := hEnc + "." + pEnc
	sig := signHS256([]byte(signed), m.Secret)
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (m Manager) Parse(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}

	signed := parts[0] + "." + parts[1]
	expected := signHS256([]byte(signed), m.Secret)
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	if !hmac.Equal(expected, gotSig) {
		return Claims{}, ErrInvalidToken
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}

	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if claims.Subject == "" || claims.Username == "" || claims.Exp == 0 {
		return Claims{}, ErrInvalidToken
	}
	if m.Now().Unix() >= claims.Exp {
		return Claims{}, ErrExpiredToken
	}
	return claims, nil
}

func signHS256(data, secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(data)
	return h.Sum(nil)
}

func BearerToken(authHeader string) string {
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
