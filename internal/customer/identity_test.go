package customer

import (
	"bytes"
	"testing"
)

// BR-085: the stored bytes are not the number, they open back to it, two seals
// of the same number differ (random nonce), and a flipped byte fails to open.
func TestIdentityBox(t *testing.T) {
	box, err := newIdentityBox([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	const number = "3174012345670001"

	a, err := box.seal(number)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(a, []byte(number)) {
		t.Fatal("sealed bytes contain the plaintext number")
	}
	if got, err := box.open(a); err != nil || got != number {
		t.Fatalf("open = %q, %v; want %q", got, err, number)
	}

	b, _ := box.seal(number)
	if bytes.Equal(a, b) {
		t.Fatal("two seals of one number are identical -- nonce is not random")
	}

	a[len(a)-1] ^= 1
	if _, err := box.open(a); err == nil {
		t.Fatal("tampered ciphertext opened")
	}

	if _, err := newIdentityBox([]byte("short")); err == nil {
		t.Fatal("a 5-byte key was accepted")
	}
	if got := lastFour(number); got != "0001" {
		t.Fatalf("lastFour = %q", got)
	}
}
