package guestblob

import _ "embed"

// Module is the reproducibly generated secure Jsonnet WASI reactor.
//
//go:embed securejsonnet.wasm
var Module []byte
