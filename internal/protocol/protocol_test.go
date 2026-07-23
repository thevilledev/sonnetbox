package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestImportResponseRoundTrip(t *testing.T) {
	want := ImportResponse{
		Canonical: "lib/data.jsonnet",
		Content:   []byte{0, 1, 2, 255},
	}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got ImportResponse
	if err := DecodeJSON(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.Canonical != want.Canonical || !bytes.Equal(got.Content, want.Content) {
		t.Fatalf("unexpected response: %#v", got)
	}
}

func TestDecodeJSONRejectsUnknownAndTrailingFields(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte(`{"from":"","path":"x","extra":true}`),
		[]byte(`{"from":"","path":"x"} {}`),
	} {
		var request ImportRequest
		if err := DecodeJSON(payload, &request); err == nil {
			t.Fatalf("expected %q to be rejected", payload)
		}
	}
}

func TestABIConstants(t *testing.T) {
	if ABIVersion != 5 {
		t.Fatalf("ABI version = %d, want 5", ABIVersion)
	}
	statuses := []uint32{
		HostOK,
		HostDenied,
		HostHandlerFailure,
		HostLimit,
		HostCanceled,
		HostMalformed,
	}
	for want, got := range statuses {
		if uint32(want) != got {
			t.Fatalf("host status at index %d = %d", want, got)
		}
	}
	if OperationResolveImport != 1 || OperationCallCapability != 2 {
		t.Fatalf(
			"unexpected operations: import=%d capability=%d",
			OperationResolveImport,
			OperationCallCapability,
		)
	}
}

func TestOutputFramesRoundTrip(t *testing.T) {
	filesPayload, err := EncodeMultiOutput(map[string]string{
		"b.json": "second",
		"a.json": "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := DecodeMultiOutput(filesPayload)
	if err != nil {
		t.Fatal(err)
	}
	if string(files["a.json"]) != "first" || string(files["b.json"]) != "second" {
		t.Fatalf("unexpected multi-file output: %#v", files)
	}

	streamPayload, err := EncodeStreamOutput([]string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	documents, err := DecodeStreamOutput(streamPayload)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 ||
		string(documents[0]) != "first" ||
		string(documents[1]) != "second" {
		t.Fatalf("unexpected stream output: %#v", documents)
	}
}

func TestOutputFramesRejectMalformedData(t *testing.T) {
	for _, payload := range [][]byte{
		nil,
		{1, 0, 0, 0},
		{1, 0, 0, 0, 4, 0, 0, 0, 'a'},
		{0, 0, 0, 0, 1},
	} {
		if _, err := DecodeMultiOutput(payload); err == nil {
			t.Fatalf("expected malformed multi-file payload %v to fail", payload)
		}
		if _, err := DecodeStreamOutput(payload); err == nil {
			t.Fatalf("expected malformed stream payload %v to fail", payload)
		}
	}
}

func TestGuestErrorLimitMetadata(t *testing.T) {
	want := GuestError{
		Kind:    "host request bytes",
		Message: "request exceeds limit",
		Limit:   1024,
		Actual:  2048,
	}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got GuestError
	if err := DecodeJSON(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("guest error = %#v, want %#v", got, want)
	}
}

func FuzzDecodeImportResponse(f *testing.F) {
	f.Add([]byte(`{"canonical":"lib/data.jsonnet","content_base64":"AAEC/w=="}`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var response ImportResponse
		if err := DecodeJSON(payload, &response); err != nil {
			return
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var roundTrip ImportResponse
		if err := DecodeJSON(encoded, &roundTrip); err != nil {
			t.Fatal(err)
		}
		if response.Canonical != roundTrip.Canonical ||
			!bytes.Equal(response.Content, roundTrip.Content) {
			t.Fatal("round trip changed response")
		}
	})
}

func FuzzPack(f *testing.F) {
	f.Add(uint32(0), uint32(0))
	f.Add(uint32(5), ^uint32(0))
	f.Fuzz(func(t *testing.T, status, length uint32) {
		gotStatus, gotLength := Unpack(Pack(status, length))
		if gotStatus != status || gotLength != length {
			t.Fatal("round trip failed")
		}
	})
}
