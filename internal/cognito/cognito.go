// Package cognito logs in to an AWS Cognito user pool with USER_SRP_AUTH and
// refreshes the tokens, using only the standard library. It speaks the
// unauthenticated JSON API that public app clients use, so no AWS SDK or
// signing is needed.
package cognito

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Tokens are the result of a login or refresh.
type Tokens struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
	Expires      time.Time
}

// Client logs one user in to one pool.
type Client struct {
	Region   string // e.g. "us-east-2"
	PoolID   string // e.g. "us-east-2_ghlOXVLi1"
	ClientID string
	HTTP     *http.Client
	// Endpoint overrides https://cognito-idp.<region>.amazonaws.com/ (tests).
	Endpoint string
	// Now and Random are for tests; nil means time.Now and crypto/rand.
	Now    func() time.Time
	Random io.Reader
}

// Error is an error reply from Cognito, e.g. NotAuthorizedException.
type Error struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return "cognito: " + e.Type + ": " + e.Message }

// NotAuthorized reports whether err is Cognito refusing the credentials or
// the refresh token.
func NotAuthorized(err error) bool {
	var ce *Error
	return errors.As(err, &ce) && strings.HasSuffix(ce.Type, "NotAuthorizedException")
}

type authResult struct {
	AuthenticationResult *struct {
		IdToken      string
		AccessToken  string
		RefreshToken string
		ExpiresIn    int
	}
	ChallengeName       string
	ChallengeParameters map[string]string
	Session             string
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) call(ctx context.Context, op string, body any) (*authResult, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := c.Endpoint
	if url == "" {
		url = "https://cognito-idp." + c.Region + ".amazonaws.com/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSCognitoIdentityProviderService."+op)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		ce := &Error{}
		if json.Unmarshal(data, ce) != nil || ce.Type == "" {
			return nil, fmt.Errorf("cognito %s: HTTP %d", op, resp.StatusCode)
		}
		return nil, ce
	}
	var r authResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("cognito %s: %w", op, err)
	}
	return &r, nil
}

func (c *Client) tokens(r *authResult, oldRefresh string) (*Tokens, error) {
	a := r.AuthenticationResult
	if a == nil || a.IdToken == "" {
		return nil, fmt.Errorf("cognito: no tokens in reply (challenge %q)", r.ChallengeName)
	}
	t := &Tokens{IDToken: a.IdToken, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken,
		Expires: c.now().Add(time.Duration(a.ExpiresIn) * time.Second)}
	if t.RefreshToken == "" {
		t.RefreshToken = oldRefresh // refreshes do not return a new one
	}
	return t, nil
}

// Login runs USER_SRP_AUTH for username and password.
func (c *Client) Login(ctx context.Context, username, password string) (*Tokens, error) {
	s, err := newSRP(c.Random)
	if err != nil {
		return nil, err
	}
	r, err := c.call(ctx, "InitiateAuth", map[string]any{
		"AuthFlow":       "USER_SRP_AUTH",
		"ClientId":       c.ClientID,
		"AuthParameters": map[string]string{"USERNAME": username, "SRP_A": s.publicHex()},
	})
	if err != nil {
		return nil, err
	}
	if r.ChallengeName != "PASSWORD_VERIFIER" {
		return nil, fmt.Errorf("cognito: unexpected challenge %q", r.ChallengeName)
	}
	p := r.ChallengeParameters
	userID := p["USER_ID_FOR_SRP"]
	poolName := c.PoolID[strings.Index(c.PoolID, "_")+1:]
	stamp := c.now().UTC().Format("Mon Jan 2 15:04:05 UTC 2006")
	sig, err := s.signature(poolName, userID, password, p["SALT"], p["SRP_B"], p["SECRET_BLOCK"], stamp)
	if err != nil {
		return nil, err
	}
	req := map[string]any{
		"ChallengeName": "PASSWORD_VERIFIER",
		"ClientId":      c.ClientID,
		"ChallengeResponses": map[string]string{
			"USERNAME":                    userID,
			"TIMESTAMP":                   stamp,
			"PASSWORD_CLAIM_SECRET_BLOCK": p["SECRET_BLOCK"],
			"PASSWORD_CLAIM_SIGNATURE":    sig,
		},
	}
	if r.Session != "" { // the pool may not issue one; an empty one is rejected
		req["Session"] = r.Session
	}
	r, err = c.call(ctx, "RespondToAuthChallenge", req)
	if err != nil {
		return nil, err
	}
	return c.tokens(r, "")
}

// Refresh exchanges a refresh token for new ID and access tokens.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	r, err := c.call(ctx, "InitiateAuth", map[string]any{
		"AuthFlow":       "REFRESH_TOKEN_AUTH",
		"ClientId":       c.ClientID,
		"AuthParameters": map[string]string{"REFRESH_TOKEN": refreshToken},
	})
	if err != nil {
		return nil, err
	}
	return c.tokens(r, refreshToken)
}
