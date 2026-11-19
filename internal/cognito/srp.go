package cognito

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
)

// The 3072-bit group from RFC 5054 that Cognito uses, with g = 2.
const nHex = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD1" +
	"29024E088A67CC74020BBEA63B139B22514A08798E3404DD" +
	"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245" +
	"E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED" +
	"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3D" +
	"C2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F" +
	"83655D23DCA3AD961C62F356208552BB9ED529077096966D" +
	"670C354E4ABC9804F1746C08CA18217C32905E462E36CE3B" +
	"E39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9" +
	"DE2BCBF6955817183995497CEA956AE515D2261898FA0510" +
	"15728E5A8AAAC42DAD33170D04507A33A85521ABDF1CBA64" +
	"ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7" +
	"ABF5AE8CDB0933D71E8C94E04A25619DCEE3D2261AD2EE6B" +
	"F12FFA06D98A0864D87602733EC86A64521F2B18177B200C" +
	"BBE117577A615D6C770988C0BAD946E208E24FA074E5AB31" +
	"43DB5BFCE0FD108E4B82D120A93AD2CAFFFFFFFFFFFFFFFF"

var (
	bigN, _ = new(big.Int).SetString(nHex, 16)
	bigG    = big.NewInt(2)
	// k = H(N || g) in Cognito's padding: "00" + N, "0" + g.
	bigK = hexHashInt("00" + nHex + "0" + "2")
)

// padHex renders n as hex the way Cognito does: even length, and a leading
// 00 when the top bit is set, so it reads as positive.
func padHex(n *big.Int) string {
	h := n.Text(16)
	if len(h)%2 == 1 {
		h = "0" + h
	} else if h[0] >= '8' {
		h = "00" + h
	}
	return h
}

func hexHash(h string) []byte {
	b, _ := hex.DecodeString(h)
	s := sha256.Sum256(b)
	return s[:]
}

func hexHashInt(h string) *big.Int { return new(big.Int).SetBytes(hexHash(h)) }

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

type srp struct {
	a, A *big.Int
}

func newSRP(r io.Reader) (*srp, error) {
	if r == nil {
		r = rand.Reader
	}
	buf := make([]byte, 128)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	a := new(big.Int).Mod(new(big.Int).SetBytes(buf), bigN)
	A := new(big.Int).Exp(bigG, a, bigN)
	if A.Sign() == 0 {
		return nil, errors.New("cognito: bad SRP random")
	}
	return &srp{a: a, A: A}, nil
}

func (s *srp) publicHex() string { return s.A.Text(16) }

// signature answers the PASSWORD_VERIFIER challenge.
func (s *srp) signature(poolName, userID, password, saltHex, bHex, secretBlock, stamp string) (string, error) {
	B, ok := new(big.Int).SetString(bHex, 16)
	if !ok || new(big.Int).Mod(B, bigN).Sign() == 0 {
		return "", errors.New("cognito: bad SRP_B")
	}
	salt, ok := new(big.Int).SetString(saltHex, 16)
	if !ok {
		return "", errors.New("cognito: bad SALT")
	}
	block, err := base64.StdEncoding.DecodeString(secretBlock)
	if err != nil {
		return "", errors.New("cognito: bad SECRET_BLOCK")
	}

	u := hexHashInt(padHex(s.A) + padHex(B))
	if u.Sign() == 0 {
		return "", errors.New("cognito: bad SRP u")
	}
	up := sha256.Sum256([]byte(poolName + userID + ":" + password))
	x := hexHashInt(padHex(salt) + hex.EncodeToString(up[:]))

	// S = (B - k*g^x) ^ (a + u*x) mod N
	base := new(big.Int).Exp(bigG, x, bigN)
	base.Mul(base, bigK)
	base.Sub(B, base)
	base.Mod(base, bigN)
	exp := new(big.Int).Mul(u, x)
	exp.Add(exp, s.a)
	S := new(big.Int).Exp(base, exp, bigN)

	key := hkdf(mustHex(padHex(S)), mustHex(padHex(u)))
	msg := append([]byte(poolName+userID), block...)
	msg = append(msg, stamp...)
	return base64.StdEncoding.EncodeToString(hmacSHA256(key, msg)), nil
}

// hkdf is Cognito's one-block HKDF-SHA256 with info "Caldera Derived Key".
func hkdf(ikm, salt []byte) []byte {
	prk := hmacSHA256(salt, ikm)
	return hmacSHA256(prk, []byte("Caldera Derived Key\x01"))[:16]
}

func mustHex(h string) []byte {
	b, _ := hex.DecodeString(h)
	return b
}
