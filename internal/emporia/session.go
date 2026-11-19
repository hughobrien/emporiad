// Package emporia reads devices and usage from Emporia's cloud API
// (api.emporiaenergy.com), the only interface the Vue energy monitors have.
package emporia

import (
	"context"
	"sync"
	"time"

	"github.com/hughobrien/emporiad/internal/cognito"
)

// Emporia's Cognito pool and app client, as used by its app and by PyEmVue.
const (
	Region   = "us-east-2"
	PoolID   = "us-east-2_ghlOXVLi1"
	ClientID = "4qte47jbstod8apnfic0bunmrq"
)

// Authenticator is what the session needs from Cognito.
type Authenticator interface {
	Login(ctx context.Context, username, password string) (*cognito.Tokens, error)
	Refresh(ctx context.Context, refreshToken string) (*cognito.Tokens, error)
}

// Session hands out a current ID token: it logs in once, refreshes before
// expiry, and logs in again when the refresh token stops working.
type Session struct {
	Auth     Authenticator
	Username string
	Password string
	Now      func() time.Time

	mu     sync.Mutex
	tokens *cognito.Tokens
}

// NewSession returns a session against Emporia's real pool.
func NewSession(username, password string) *Session {
	return &Session{
		Auth:     &cognito.Client{Region: Region, PoolID: PoolID, ClientID: ClientID, HTTP: &httpClient},
		Username: username,
		Password: password,
	}
}

func (s *Session) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Token returns an ID token valid for at least another minute.
func (s *Session) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens != nil && s.now().Add(time.Minute).Before(s.tokens.Expires) {
		return s.tokens.IDToken, nil
	}
	if s.tokens != nil && s.tokens.RefreshToken != "" {
		t, err := s.Auth.Refresh(ctx, s.tokens.RefreshToken)
		if err == nil {
			s.tokens = t
			return t.IDToken, nil
		}
		if !cognito.NotAuthorized(err) {
			return "", err
		}
		// The refresh token expired or was revoked: log in from scratch.
	}
	t, err := s.Auth.Login(ctx, s.Username, s.Password)
	if err != nil {
		s.tokens = nil
		return "", err
	}
	s.tokens = t
	return t.IDToken, nil
}

// Expire makes the next Token call refresh, after the API refused a token.
func (s *Session) Expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens != nil {
		s.tokens.Expires = time.Time{}
	}
}
