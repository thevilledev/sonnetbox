package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

type publicABIManifest struct {
	ABIVersion     uint32 `json:"abi_version"`
	JsonnetVersion string `json:"jsonnet_version"`
	Artifact       struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	} `json:"artifact"`
	WASIModule string `json:"wasi_module"`
	HostImport struct {
		Module  string   `json:"module"`
		Name    string   `json:"name"`
		Params  []string `json:"params"`
		Results []string `json:"results"`
	} `json:"host_import"`
	Exports map[string]struct {
		Params  []string `json:"params"`
		Results []string `json:"results"`
	} `json:"exports"`
	Operations       map[string]uint32 `json:"operations"`
	EvaluationStatus map[string]uint32 `json:"evaluation_status"`
	HostStatus       map[string]uint32 `json:"host_status"`
	Fuel             string            `json:"fuel"`
}

func TestPublicABIManifestMatchesImplementation(t *testing.T) {
	root := filepath.Join("..", "..")
	raw := readABITestFile(t, filepath.Join(root, "abi", "v7", "manifest.json"))
	var manifest publicABIManifest
	if err := DecodeJSON(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ABIVersion != ABIVersion {
		t.Fatalf("manifest ABI = %d, want %d", manifest.ABIVersion, ABIVersion)
	}
	if manifest.JsonnetVersion != "v0.22.0" ||
		manifest.WASIModule != "wasi_snapshot_preview1" ||
		manifest.HostImport.Module != "sonnetbox_host" ||
		manifest.HostImport.Name != "call" ||
		manifest.Fuel != "host-defined" {
		t.Fatalf("unexpected public ABI metadata: %#v", manifest)
	}
	if len(manifest.Exports) != 9 {
		t.Fatalf("export count = %d, want 9", len(manifest.Exports))
	}

	artifact := readABITestFile(t, filepath.Join(root, manifest.Artifact.Path))
	if len(artifact) != manifest.Artifact.Size {
		t.Fatalf("artifact size = %d, want %d", len(artifact), manifest.Artifact.Size)
	}
	digest := sha256.Sum256(artifact)
	if got := hex.EncodeToString(digest[:]); got != manifest.Artifact.SHA256 {
		t.Fatalf("artifact SHA-256 = %s, want %s", got, manifest.Artifact.SHA256)
	}

	assertABIValues(t, "operations", manifest.Operations, map[string]uint32{
		"resolve_import":  OperationResolveImport,
		"call_capability": OperationCallCapability,
	})
	assertABIValues(t, "evaluation status", manifest.EvaluationStatus, map[string]uint32{
		"ok":              EvalOK,
		"invalid_request": EvalInvalidRequest,
		"jsonnet_error":   EvalJsonnetError,
		"host_error":      EvalHostError,
		"limit":           EvalLimit,
		"internal":        EvalInternal,
	})
	assertABIValues(t, "host status", manifest.HostStatus, map[string]uint32{
		"ok":              HostOK,
		"denied":          HostDenied,
		"handler_failure": HostHandlerFailure,
		"limit":           HostLimit,
		"canceled":        HostCanceled,
		"malformed":       HostMalformed,
	})
}

func TestPublicABIVectorsDecodeStrictly(t *testing.T) {
	testdata := filepath.Join("..", "..", "abi", "v7", "testdata")
	tests := []struct {
		name   string
		target any
	}{
		{"evaluation_request.json", &EvaluationRequest{}},
		{"import_request.json", &ImportRequest{}},
		{"import_response.json", &ImportResponse{}},
		{"capability_request.json", &CapabilityRequest{}},
		{"capability_response.json", &CapabilityResponse{}},
		{"guest_error.json", &GuestError{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := readABITestFile(t, filepath.Join(testdata, test.name))
			if err := DecodeJSON(raw, test.target); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func readABITestFile(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name) //nolint:gosec // Callers supply only repository-owned ABI fixture paths.
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertABIValues(
	t *testing.T,
	name string,
	got map[string]uint32,
	want map[string]uint32,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s count = %d, want %d", name, len(got), len(want))
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s %q = %d, want %d", name, key, got[key], value)
		}
	}
}
