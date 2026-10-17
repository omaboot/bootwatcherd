package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"io"
)

// seed is a stream's key, or an echo's nonce: 16 random bytes picked for one request.
type seed [16]byte

func newSeed() seed {
	var s seed
	rand.Read(s[:]) // never fails (crypto/rand, Go 1.24+)
	return s
}

func (s seed) String() string { return hex.EncodeToString(s[:]) }

// stream is the seed's stream (bootwatchd's README): AES-128-CTR with the seed as the key, a
// zero counter block and zero plaintext.
func stream(s seed) io.Reader {
	b, _ := aes.NewCipher(s[:]) // 16 bytes: AES-128, never an error
	return &ctrStream{ctr: cipher.NewCTR(b, make([]byte, aes.BlockSize))}
}

type ctrStream struct{ ctr cipher.Stream }

func (s *ctrStream) Read(p []byte) (int, error) {
	clear(p)
	s.ctr.XORKeyStream(p, p)
	return len(p), nil
}

// match reads r to its end and compares it with the seed's stream: n is every byte read, ok the
// leading bytes that equal the stream.
func match(r io.Reader, s seed) (n, ok int64, err error) {
	want := stream(s)
	buf, exp := make([]byte, 16<<10), make([]byte, 16<<10)
	same := true
	for {
		k, rerr := r.Read(buf)
		if k > 0 {
			if same {
				want.Read(exp[:k])
				i := 0
				for i < k && buf[i] == exp[i] {
					i++
				}
				ok += int64(i)
				same = i == k
			}
			n += int64(k)
		}
		if rerr == io.EOF {
			return n, ok, nil
		}
		if rerr != nil {
			return n, ok, rerr
		}
	}
}
