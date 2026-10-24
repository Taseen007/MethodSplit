// Package middleware holds small, stateless request-signing helpers shared
// between the gateway (which verifies signatures) and msclient (which
// creates them) — see ARCHITECTURE.md §6.
package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// CanonicalString is what gets signed: METHOD \n PATH_WITH_QUERY \n
// TIMESTAMP \n hex(SHA256(BODY)).
func CanonicalString(method, pathWithQuery, timestamp, bodyHashHex string) string {
	return method + "\n" + pathWithQuery + "\n" + timestamp + "\n" + bodyHashHex
}

func Sign(secret, canonical string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature compares in constant time (crypto/hmac.Equal), never with
// ==, so response timing can't leak how much of the signature was correct.
func VerifySignature(secret, canonical, signatureHex string) bool {
	expected := Sign(secret, canonical)
	return hmac.Equal([]byte(expected), []byte(signatureHex))
}
