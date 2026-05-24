// Package crypto implements Telang's per-object encryption: AES-256-GCM in
// 64 KB frames, with a random per-object nonce and a frame-counter AAD.
//
// On-disk layout:
//
//	[ header: version(1) | base_nonce(12) | frame_size(4 BE) ]   = 17 bytes
//	[ frame_0_ciphertext + 16 byte GCM tag ]
//	[ frame_1_ciphertext + 16 byte GCM tag ]
//	...
//
// Per-frame nonce derivation keeps the random portion of the base nonce
// intact and XORs the frame index into the low 8 bytes; the AAD is the
// frame index in big-endian so reordering or splicing frames breaks
// authentication.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Version         uint8 = 1
	HeaderSize            = 1 + 12 + 4 // 17
	NonceLen              = 12
	TagLen                = 16
	DefaultFrameSize      = 64 * 1024
)

var (
	ErrBadVersion = errors.New("crypto: unsupported version byte")
	ErrShortInput = errors.New("crypto: truncated ciphertext")
)

// Cipher is constructed once per (bucket, key) and reused across uploads.
// It holds the AEAD which is safe for concurrent use.
type Cipher struct {
	aead      cipher.AEAD
	frameSize int
}

func NewCipher(key []byte, frameSize int) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: want 32-byte key, got %d", len(key))
	}
	if frameSize <= 0 {
		frameSize = DefaultFrameSize
	}
	if frameSize > (1 << 30) {
		return nil, fmt.Errorf("crypto: frame size %d too large", frameSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead, frameSize: frameSize}, nil
}

func (c *Cipher) FrameSize() int { return c.frameSize }

// CiphertextSize returns the size of the encrypted blob produced from a
// plaintext of size plaintextSize, including the header.
func (c *Cipher) CiphertextSize(plaintextSize int64) int64 {
	if plaintextSize < 0 {
		return 0
	}
	fs := int64(c.frameSize)
	full := plaintextSize / fs
	rem := plaintextSize % fs
	out := int64(HeaderSize) + full*(fs+int64(TagLen))
	if rem > 0 || plaintextSize == 0 {
		// We always emit at least a final (possibly empty) frame so an
		// authentic empty object still produces a tagged terminal frame.
		out += rem + int64(TagLen)
	}
	return out
}

// frameNonce derives the unique nonce for frame i from the base nonce.
func (c *Cipher) frameNonce(base []byte, i uint64) []byte {
	if len(base) != NonceLen {
		panic("crypto: base nonce length")
	}
	var fn [NonceLen]byte
	copy(fn[:], base)
	// XOR frame index into the low 8 bytes.
	var ib [8]byte
	binary.BigEndian.PutUint64(ib[:], i)
	for k := 0; k < 8; k++ {
		fn[4+k] ^= ib[k]
	}
	return fn[:]
}

func frameAAD(i uint64) []byte {
	var aad [8]byte
	binary.BigEndian.PutUint64(aad[:], i)
	return aad[:]
}

// NewEncrypter wraps a plaintext reader so that reads return the on-disk
// ciphertext layout. The total returned byte count equals CiphertextSize
// (caller may pass it through io.CopyN). plaintextSize must match the bytes
// actually read from src; if src yields fewer bytes the encrypter returns
// io.ErrUnexpectedEOF.
//
// A fresh random 12-byte base nonce is generated for every call. Callers
// who need to record the nonce separately (e.g. to store in metadata for
// later range reads) should use NewEncrypterWithNonce instead.
func (c *Cipher) NewEncrypter(src io.Reader, plaintextSize int64) (io.Reader, []byte) {
	base := make([]byte, NonceLen)
	_, _ = rand.Read(base)
	return c.NewEncrypterWithNonce(src, plaintextSize, base), base
}

// NewEncrypterWithNonce is the deterministic variant. The caller is
// responsible for generating a fresh, never-reused base nonce.
func (c *Cipher) NewEncrypterWithNonce(src io.Reader, plaintextSize int64, base []byte) io.Reader {
	header := make([]byte, HeaderSize)
	header[0] = Version
	copy(header[1:1+NonceLen], base)
	binary.BigEndian.PutUint32(header[1+NonceLen:], uint32(c.frameSize))
	return &encrypter{
		c:       c,
		src:     src,
		remain:  plaintextSize,
		base:    append([]byte(nil), base...),
		header:  header,
		buf:     make([]byte, 0, c.frameSize+TagLen),
		emitted: false,
	}
}

type encrypter struct {
	c       *Cipher
	src     io.Reader
	base    []byte
	header  []byte
	headerN int
	remain  int64
	idx     uint64
	buf     []byte // pending ciphertext frame
	out     int    // bytes already drained from buf
	emitted bool   // whether at least one frame has been emitted
	done    bool
}

func (e *encrypter) Read(p []byte) (int, error) {
	written := 0
	// Drain header first.
	if e.headerN < len(e.header) {
		n := copy(p, e.header[e.headerN:])
		e.headerN += n
		written += n
		p = p[n:]
		if len(p) == 0 {
			return written, nil
		}
	}
	for len(p) > 0 {
		if e.out < len(e.buf) {
			n := copy(p, e.buf[e.out:])
			e.out += n
			written += n
			p = p[n:]
			continue
		}
		if e.done {
			if written == 0 {
				return 0, io.EOF
			}
			return written, nil
		}
		if err := e.nextFrame(); err != nil {
			if errors.Is(err, io.EOF) {
				if written == 0 {
					return 0, io.EOF
				}
				return written, nil
			}
			return written, err
		}
	}
	return written, nil
}

func (e *encrypter) nextFrame() error {
	if e.remain == 0 && e.emitted {
		e.done = true
		return io.EOF
	}
	size := int64(e.c.frameSize)
	if size > e.remain {
		size = e.remain
	}
	plain := make([]byte, size)
	if size > 0 {
		if _, err := io.ReadFull(e.src, plain); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	e.remain -= size

	nonce := e.c.frameNonce(e.base, e.idx)
	aad := frameAAD(e.idx)
	ct := e.c.aead.Seal(nil, nonce, plain, aad)
	e.buf = ct
	e.out = 0
	e.idx++
	e.emitted = true
	if e.remain == 0 {
		e.done = true
	}
	return nil
}

// NewDecrypter wraps a ciphertext reader and returns a plaintext reader.
// The input must contain a full Telang header followed by zero or more
// authenticated frames.
func (c *Cipher) NewDecrypter(src io.Reader) (io.Reader, error) {
	d := &decrypter{c: c, src: src}
	if err := d.readHeader(); err != nil {
		return nil, err
	}
	return d, nil
}

type decrypter struct {
	c          *Cipher
	src        io.Reader
	base       [NonceLen]byte
	frameSize  int
	idx        uint64
	plain      []byte
	out        int
	atEOF      bool
}

func (d *decrypter) readHeader() error {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(d.src, hdr[:]); err != nil {
		return fmt.Errorf("crypto: read header: %w", err)
	}
	if hdr[0] != Version {
		return ErrBadVersion
	}
	copy(d.base[:], hdr[1:1+NonceLen])
	d.frameSize = int(binary.BigEndian.Uint32(hdr[1+NonceLen:]))
	if d.frameSize <= 0 || d.frameSize > (1<<30) {
		return fmt.Errorf("crypto: bad frame size in header: %d", d.frameSize)
	}
	return nil
}

func (d *decrypter) Read(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if d.out < len(d.plain) {
			n := copy(p, d.plain[d.out:])
			d.out += n
			written += n
			p = p[n:]
			continue
		}
		if d.atEOF {
			if written == 0 {
				return 0, io.EOF
			}
			return written, nil
		}
		if err := d.nextFrame(); err != nil {
			if errors.Is(err, io.EOF) {
				if written == 0 {
					return 0, io.EOF
				}
				return written, nil
			}
			return written, err
		}
	}
	return written, nil
}

func (d *decrypter) nextFrame() error {
	buf := make([]byte, d.frameSize+TagLen)
	n, err := io.ReadFull(d.src, buf)
	if errors.Is(err, io.EOF) {
		d.atEOF = true
		return io.EOF
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		if n < TagLen {
			return ErrShortInput
		}
		buf = buf[:n]
		d.atEOF = true
	} else if err != nil {
		return err
	}
	nonce := d.c.frameNonce(d.base[:], d.idx)
	aad := frameAAD(d.idx)
	plain, err := d.c.aead.Open(nil, nonce, buf, aad)
	if err != nil {
		return fmt.Errorf("crypto: frame %d: %w", d.idx, err)
	}
	d.idx++
	d.plain = plain
	d.out = 0
	return nil
}

// DecryptRange decrypts and writes plaintext bytes [start, end] (inclusive)
// to dst, given a ReaderAt over the full ciphertext blob and the on-disk
// total ciphertext size. start must be <= end < plaintextSize.
//
// This is the core of range-GET: only the frames overlapping the requested
// range are fetched and decrypted.
func (c *Cipher) DecryptRange(dst io.Writer, src io.ReaderAt, ciphertextSize int64, start, end int64) error {
	if start < 0 || end < start {
		return errors.New("crypto: bad range")
	}
	hdr := make([]byte, HeaderSize)
	if _, err := src.ReadAt(hdr, 0); err != nil {
		return fmt.Errorf("crypto: read header: %w", err)
	}
	if hdr[0] != Version {
		return ErrBadVersion
	}
	var base [NonceLen]byte
	copy(base[:], hdr[1:1+NonceLen])
	frameSize := int(binary.BigEndian.Uint32(hdr[1+NonceLen:]))
	if frameSize <= 0 {
		return errors.New("crypto: bad frame size")
	}

	firstFrame := uint64(start) / uint64(frameSize)
	lastFrame := uint64(end) / uint64(frameSize)

	for i := firstFrame; i <= lastFrame; i++ {
		// Compute on-disk offset and how many ciphertext bytes this frame holds.
		frameOffset := int64(HeaderSize) + int64(i)*int64(frameSize+TagLen)
		remaining := ciphertextSize - frameOffset
		if remaining <= int64(TagLen) {
			return fmt.Errorf("crypto: frame %d past ciphertext end", i)
		}
		readSize := int64(frameSize + TagLen)
		if readSize > remaining {
			readSize = remaining
		}
		buf := make([]byte, readSize)
		if _, err := src.ReadAt(buf, frameOffset); err != nil {
			return fmt.Errorf("crypto: read frame %d: %w", i, err)
		}
		nonce := c.frameNonce(base[:], i)
		aad := frameAAD(i)
		plain, err := c.aead.Open(nil, nonce, buf, aad)
		if err != nil {
			return fmt.Errorf("crypto: frame %d: %w", i, err)
		}
		frameStart := int64(i) * int64(frameSize)
		frameEnd := frameStart + int64(len(plain)) - 1

		lo := int64(0)
		hi := int64(len(plain))
		if start > frameStart {
			lo = start - frameStart
		}
		if end < frameEnd {
			hi = end - frameStart + 1
		}
		if _, err := dst.Write(plain[lo:hi]); err != nil {
			return err
		}
	}
	return nil
}
