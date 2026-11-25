package main

// ES256 access tokens (RFC 9068 shape: typ "at+jwt"), stdlib only.

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
)

var b64 = base64.RawURLEncoding

type claims struct {
	Iss      string `json:"iss"`
	Sub      string `json:"sub"`
	Aud      string `json:"aud"`
	Exp      int64  `json:"exp"`
	Iat      int64  `json:"iat"`
	Jti      string `json:"jti"`
	Scope    string `json:"scope"`
	ClientID string `json:"client_id"`
	// Gen is the server's revocation generation; the TEST hook
	// /test/expire bumps it, invalidating every outstanding token.
	Gen int64 `json:"gen"`
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

func signJWT(key *ecdsa.PrivateKey, kid string, c claims) (string, error) {
	h, err := json.Marshal(jwtHeader{Alg: "ES256", Typ: "at+jwt", Kid: kid})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	in := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	d := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, key, d[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + b64.EncodeToString(sig), nil
}

var errBadToken = errors.New("malformed or unsigned token")

// parseJWT checks structure, alg and signature only; claim checks
// (iss/aud/exp/gen) are the resource server's job.
func parseJWT(pub *ecdsa.PublicKey, kid, tok string) (*claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errBadToken
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, errBadToken
	}
	var h jwtHeader
	if json.Unmarshal(hb, &h) != nil || h.Alg != "ES256" || h.Kid != kid || h.Typ != "at+jwt" {
		return nil, errBadToken
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errBadToken
	}
	d := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, d[:], r, s) {
		return nil, errBadToken
	}
	pb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, errBadToken
	}
	var c claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, errBadToken
	}
	return &c, nil
}

// jwk returns the public key as an RFC 7517 JWK (for /jwks).
func jwk(pub *ecdsa.PublicKey, kid string) map[string]string {
	b, _ := pub.Bytes() // uncompressed 0x04||X||Y
	return map[string]string{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": kid,
		"x": b64.EncodeToString(b[1:33]), "y": b64.EncodeToString(b[33:65]),
	}
}
