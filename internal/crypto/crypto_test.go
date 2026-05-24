package crypto

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	cases := []int64{0, 1, 100, 1024, 64 * 1024, 64*1024 + 1, 256 * 1024, 1<<20 + 17}
	for _, size := range cases {
		c, err := NewCipher(mustKey(t), 1024) // small frame size to exercise multi-frame
		if err != nil {
			t.Fatal(err)
		}
		plain := make([]byte, size)
		_, _ = rand.Read(plain)

		enc, _ := c.NewEncrypter(bytes.NewReader(plain), size)
		ct, err := io.ReadAll(enc)
		if err != nil {
			t.Fatalf("size %d: encrypt: %v", size, err)
		}
		if int64(len(ct)) != c.CiphertextSize(size) {
			t.Fatalf("size %d: ciphertext len %d, want %d", size, len(ct), c.CiphertextSize(size))
		}

		dec, err := c.NewDecrypter(bytes.NewReader(ct))
		if err != nil {
			t.Fatalf("size %d: new decrypter: %v", size, err)
		}
		got, err := io.ReadAll(dec)
		if err != nil {
			t.Fatalf("size %d: decrypt: %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size %d: roundtrip mismatch (got %d bytes)", size, len(got))
		}
	}
}

func TestTamperDetected(t *testing.T) {
	c, _ := NewCipher(mustKey(t), 1024)
	plain := bytes.Repeat([]byte("A"), 4096)
	encR, _ := c.NewEncrypter(bytes.NewReader(plain), int64(len(plain)))
	ct, _ := io.ReadAll(encR)

	// Flip one byte in the first frame's ciphertext.
	ct[HeaderSize+5] ^= 0x80
	dec, err := c.NewDecrypter(bytes.NewReader(ct))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(dec); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
}

func TestRange(t *testing.T) {
	c, _ := NewCipher(mustKey(t), 1024)
	plain := make([]byte, 10*1024+123)
	_, _ = rand.Read(plain)
	encR, _ := c.NewEncrypter(bytes.NewReader(plain), int64(len(plain)))
	ct, _ := io.ReadAll(encR)

	cases := []struct{ start, end int64 }{
		{0, 0},
		{0, 1023},
		{500, 2500},
		{1024, 2047},
		{int64(len(plain)) - 1000, int64(len(plain)) - 1},
		{0, int64(len(plain)) - 1},
	}
	for _, c2 := range cases {
		var out bytes.Buffer
		if err := c.DecryptRange(&out, bytes.NewReader(ct), int64(len(ct)), c2.start, c2.end); err != nil {
			t.Fatalf("range [%d,%d]: %v", c2.start, c2.end, err)
		}
		want := plain[c2.start : c2.end+1]
		if !bytes.Equal(out.Bytes(), want) {
			t.Fatalf("range [%d,%d]: got %d bytes want %d", c2.start, c2.end, out.Len(), len(want))
		}
	}
}
