// Package guestblob embeds the reproducibly built Jsonnet WASI guest.
package guestblob

import _ "embed"

// Module is the reproducibly generated secure Jsonnet WASI reactor.
//
//go:embed securejsonnet.wasm
var Module []byte
