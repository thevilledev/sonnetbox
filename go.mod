module github.com/thevilledev/sonnetbox

go 1.25.12

require (
	github.com/google/go-jsonnet v0.22.0
	github.com/tetratelabs/wazero v1.12.0
)

replace github.com/tetratelabs/wazero => github.com/thevilledev/wazero v0.0.0-20260724181017-b55482bf4b01

require (
	golang.org/x/crypto v0.45.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	sigs.k8s.io/yaml v1.4.0 // indirect
)
