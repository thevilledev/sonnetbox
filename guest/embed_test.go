package guest

import (
	"crypto/sha256"
	"testing"
)

func TestBytesReturnsIndependentVerifiedCopy(t *testing.T) {
	first := Bytes()
	if len(first) == 0 {
		t.Fatal("embedded guest is empty")
	}
	if got := sha256.Sum256(first); got != SHA256() {
		t.Fatalf("guest digest = %x, want %x", got, SHA256())
	}

	first[0] ^= 0xff
	second := Bytes()
	if first[0] == second[0] {
		t.Fatal("mutating returned bytes changed the embedded guest")
	}
	if got := sha256.Sum256(second); got != SHA256() {
		t.Fatalf("fresh guest digest = %x, want %x", got, SHA256())
	}
}
