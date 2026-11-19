package cognito

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakePool plays Cognito's side of SRP for one user, so a wrong step on the
// client shows up as a signature mismatch, the way the real pool reports it.
type fakePool struct {
	t                  *testing.T
	pool, user, pass   string
	salt               *big.Int
	b, B, A            *big.Int
	secret             []byte
	refreshOK          bool
	sawEmptySession    bool
	initiates, refresh int
}

func (f *fakePool) verifier() *big.Int {
	up := sha256.Sum256([]byte(f.pool + f.user + ":" + f.pass))
	x := hexHashInt(padHex(f.salt) + hex.EncodeToString(up[:]))
	return new(big.Int).Exp(bigG, x, bigN)
}

func (f *fakePool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AuthFlow           string
		AuthParameters     map[string]string
		ChallengeResponses map[string]string
		Session            *string
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Fatal(err)
	}
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	deny := func(msg string) {
		w.WriteHeader(400)
		reply(map[string]string{"__type": "NotAuthorizedException", "message": msg})
	}
	tokens := map[string]any{"AuthenticationResult": map[string]any{
		"IdToken": "id", "AccessToken": "access", "RefreshToken": "refresh", "ExpiresIn": 3600}}

	switch op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AWSCognitoIdentityProviderService."); {
	case op == "InitiateAuth" && body.AuthFlow == "REFRESH_TOKEN_AUTH":
		f.refresh++
		if !f.refreshOK || body.AuthParameters["REFRESH_TOKEN"] != "refresh" {
			deny("Refresh Token has expired")
			return
		}
		res := tokens["AuthenticationResult"].(map[string]any)
		delete(res, "RefreshToken")
		reply(tokens)
	case op == "InitiateAuth" && body.AuthFlow == "USER_SRP_AUTH":
		f.initiates++
		f.A, _ = new(big.Int).SetString(body.AuthParameters["SRP_A"], 16)
		f.b = big.NewInt(123456789)
		// B = k*v + g^b
		f.B = new(big.Int).Mul(bigK, f.verifier())
		f.B.Add(f.B, new(big.Int).Exp(bigG, f.b, bigN))
		f.B.Mod(f.B, bigN)
		f.secret = []byte("secret block bytes")
		reply(map[string]any{"ChallengeName": "PASSWORD_VERIFIER", "ChallengeParameters": map[string]string{
			"USER_ID_FOR_SRP": "uuid-1", "SALT": f.salt.Text(16), "SRP_B": f.B.Text(16),
			"SECRET_BLOCK": base64.StdEncoding.EncodeToString(f.secret), "USERNAME": "uuid-1"}})
	case op == "RespondToAuthChallenge":
		if body.Session != nil && *body.Session == "" {
			f.sawEmptySession = true
		}
		c := body.ChallengeResponses
		// S = (A * v^u) ^ b
		u := hexHashInt(padHex(f.A) + padHex(f.B))
		S := new(big.Int).Exp(f.verifier(), u, bigN)
		S.Mul(S, f.A)
		S.Exp(S, f.b, bigN)
		key := hkdf(mustHex(padHex(S)), mustHex(padHex(u)))
		m := hmac.New(sha256.New, key)
		m.Write([]byte(f.pool + "uuid-1"))
		m.Write(f.secret)
		m.Write([]byte(c["TIMESTAMP"]))
		want := base64.StdEncoding.EncodeToString(m.Sum(nil))
		if c["PASSWORD_CLAIM_SIGNATURE"] != want {
			deny("Incorrect username or password.")
			return
		}
		if c["TIMESTAMP"] != "Thu Nov 5 09:04:05 UTC 2026" {
			f.t.Errorf("timestamp %q", c["TIMESTAMP"])
		}
		reply(tokens)
	default:
		f.t.Fatalf("unexpected call %s %s", op, body.AuthFlow)
	}
}

func newFake(t *testing.T, pass string) (*fakePool, *Client) {
	f := &fakePool{t: t, pool: "Pool1", user: "uuid-1", pass: pass}
	f.salt, _ = new(big.Int).SetString("f00dfeedbeef1234", 16)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := &Client{Region: "us-east-2", PoolID: "us-east-2_Pool1", ClientID: "app", Endpoint: srv.URL,
		Now:    func() time.Time { return time.Date(2026, 11, 5, 9, 4, 5, 0, time.UTC) },
		Random: bytes.NewReader(bytes.Repeat([]byte{0x5a}, 128))}
	return f, c
}

func TestLoginSignsTheChallenge(t *testing.T) {
	f, c := newFake(t, "hunter2")
	tok, err := c.Login(context.Background(), "me@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if tok.IDToken != "id" || tok.RefreshToken != "refresh" || !tok.Expires.Equal(c.Now().Add(time.Hour)) {
		t.Fatalf("tokens %+v", tok)
	}
	if f.sawEmptySession {
		t.Error("sent an empty Session, which Cognito rejects")
	}
}

func TestLoginWrongPassword(t *testing.T) {
	_, c := newFake(t, "hunter2")
	_, err := c.Login(context.Background(), "me@example.com", "hunter3")
	if !NotAuthorized(err) {
		t.Fatalf("want NotAuthorized, got %v", err)
	}
}

func TestRefreshKeepsRefreshToken(t *testing.T) {
	f, c := newFake(t, "pw")
	f.refreshOK = true
	tok, err := c.Refresh(context.Background(), "refresh")
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "refresh" || tok.IDToken != "id" {
		t.Fatalf("tokens %+v", tok)
	}
	f.refreshOK = false
	if _, err := c.Refresh(context.Background(), "refresh"); !NotAuthorized(err) {
		t.Fatalf("want NotAuthorized, got %v", err)
	}
}

func TestPadHex(t *testing.T) {
	for in, want := range map[int64]string{0xf: "0f", 0x7f: "7f", 0x80: "0080", 0x1234: "1234", 0x8234: "008234"} {
		if got := padHex(big.NewInt(in)); got != want {
			t.Errorf("padHex(%x) = %s, want %s", in, got, want)
		}
	}
}
