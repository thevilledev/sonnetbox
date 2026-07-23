package protocol

import (
	"bytes"
	"testing"
)

func TestImportResponseRoundTrip(t *testing.T) {
	content := []byte{0, 1, 2, 255}
	payload, err := EncodeImportResponse("lib/data.bin", content)
	if err != nil {
		t.Fatal(err)
	}
	canonical, decoded, err := DecodeImportResponse(payload)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "lib/data.bin" || !bytes.Equal(decoded, content) {
		t.Fatalf("unexpected response: %q %v", canonical, decoded)
	}
}

func FuzzDecodeImportResponse(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 'a', 'b'})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		canonical, content, err := DecodeImportResponse(payload)
		if err != nil {
			return
		}
		encoded, err := EncodeImportResponse(canonical, content)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encoded, payload) {
			t.Fatalf("round trip changed payload")
		}
	})
}

func FuzzPack(f *testing.F) {
	f.Add(uint32(0), uint32(0))
	f.Add(uint32(5), ^uint32(0))
	f.Fuzz(func(t *testing.T, status, length uint32) {
		gotStatus, gotLength := Unpack(Pack(status, length))
		if gotStatus != status || gotLength != length {
			t.Fatalf("round trip failed")
		}
	})
}
