package s3api

import (
	"crypto/hmac"
	"crypto/sha256"
	"hash"
	"net/url"
)

func urlQueryUnescape(s string) (string, error) { return url.QueryUnescape(s) }

// hmacWrapper adapts hash.Hash to the macHash interface defined in
// integration_test.go without forcing the test file to import crypto/hmac
// directly (so it can stay focused on the wire format).
type hmacWrapper struct{ h hash.Hash }

func (w hmacWrapper) Write(p []byte) (int, error) { return w.h.Write(p) }
func (w hmacWrapper) Sum(b []byte) []byte         { return w.h.Sum(b) }

func hmacNew(key []byte) hash.Hash { return hmac.New(sha256.New, key) }
