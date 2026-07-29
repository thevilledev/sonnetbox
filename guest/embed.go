// Package guest provides the reproducibly built Jsonnet WASI guest for hosts
// that implement the public sonnetbox ABI.
package guest

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
)

// module is the reproducibly generated secure Jsonnet WASI reactor.
//
//go:embed sonnetbox.wasm
var module []byte

var moduleSHA256 = sha256.Sum256(module)

// Bytes returns a copy of the guest WebAssembly module. Callers may compile or
// persist the returned bytes without being able to mutate the package's
// embedded copy.
func Bytes() []byte {
	return bytes.Clone(module)
}

// SHA256 returns the digest of the bytes returned by [Bytes].
func SHA256() [sha256.Size]byte {
	return moduleSHA256
}
