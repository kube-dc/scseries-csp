package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Credentials are what a CSP token stands for.
type Credentials struct {
	Array    string `json:"a"`
	User     string `json:"u"`
	Password string `json:"p"`
	Expiry   int64  `json:"e"`
}

// TokenCodec seals credentials into opaque tokens (AES-256-GCM). Every replica sharing the key can open
// any token, so the CSP needs no session affinity and survives restarts without forcing re-logins.
type TokenCodec struct {
	aead cipher.AEAD
	ttl  time.Duration
}

// NewTokenCodec uses a 32-byte key; a nil key generates a random one (single-replica / tests only).
func NewTokenCodec(key []byte, ttl time.Duration) (*TokenCodec, error) {
	if key == nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("token key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &TokenCodec{aead: aead, ttl: ttl}, nil
}

// Seal issues a token for c (expiry set from the TTL).
func (t *TokenCodec) Seal(c Credentials) (string, int64, error) {
	c.Expiry = time.Now().Add(t.ttl).Unix()
	plain, err := json.Marshal(c)
	if err != nil {
		return "", 0, err
	}
	nonce := make([]byte, t.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", 0, err
	}
	sealed := t.aead.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), c.Expiry, nil
}

// ErrBadToken means the token is unknown, tampered with, or expired; the driver re-authenticates on 401.
var ErrBadToken = errors.New("invalid or expired x-auth-token")

// Open returns the credentials of a valid token.
func (t *TokenCodec) Open(token string) (Credentials, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < t.aead.NonceSize() {
		return Credentials{}, ErrBadToken
	}
	plain, err := t.aead.Open(nil, raw[:t.aead.NonceSize()], raw[t.aead.NonceSize():], nil)
	if err != nil {
		return Credentials{}, ErrBadToken
	}
	var c Credentials
	if err := json.Unmarshal(plain, &c); err != nil || time.Now().Unix() > c.Expiry {
		return Credentials{}, ErrBadToken
	}
	return c, nil
}
