package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// TestStatusCodeOfReadsStatusError verifies the accessor that carries an upstream
// status out of the executor into the ABI envelope.
func TestStatusCodeOfReadsStatusError(t *testing.T) {
	err := statusError{statusCode: 403, body: []byte(`{"error":"forbidden"}`)}
	if got := statusCodeOf(err); got != 403 {
		t.Fatalf("statusCodeOf(statusError) = %d, want 403", got)
	}
}

// TestStatusCodeOfIgnoresPlainError ensures ordinary errors report no status so
// they still surface as a generic downstream failure.
func TestStatusCodeOfIgnoresPlainError(t *testing.T) {
	if got := statusCodeOf(errors.New("plain failure")); got != 0 {
		t.Fatalf("statusCodeOf(plain error) = %d, want 0", got)
	}
	if got := statusCodeOf(nil); got != 0 {
		t.Fatalf("statusCodeOf(nil) = %d, want 0", got)
	}
}

// TestStatusCodeOfUnwrapsErrorChain verifies a wrapped statusError is still
// detected, since the host relies on errors.As semantics.
func TestStatusCodeOfUnwrapsErrorChain(t *testing.T) {
	wrapped := errors.Join(errors.New("context"), statusError{statusCode: 429})
	if got := statusCodeOf(wrapped); got != 429 {
		t.Fatalf("statusCodeOf(wrapped) = %d, want 429", got)
	}
}

// TestAbiErrorEnvelopeCarriesHTTPStatus locks the wire contract: a status
// reported by the executor must reach pluginabi.Error.HTTPStatus, because
// pluginhost/rpc_client.go turns that field into the StatusCode() the host reads
// back via clienterror.HTTPStatusFromError. Without it the host can only report
// a generic 500 server_error.
func TestAbiErrorEnvelopeCarriesHTTPStatus(t *testing.T) {
	raw := abiErrorEnvelopeWithStatus("plugin_error", "upstream returned 403", 403)

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.OK {
		t.Fatal("envelope OK = true, want false")
	}
	if env.Error == nil {
		t.Fatal("envelope Error = nil, want populated")
	}
	if env.Error.HTTPStatus != 403 {
		t.Fatalf("HTTPStatus = %d, want 403", env.Error.HTTPStatus)
	}
	if env.Error.Message != "upstream returned 403" {
		t.Fatalf("Message = %q, want %q", env.Error.Message, "upstream returned 403")
	}
}

// TestAbiOKEnvelopeWithErrorPreservesStatus covers the non-streaming executor
// path, where the failure travels inside the method result envelope.
func TestAbiOKEnvelopeWithErrorPreservesStatus(t *testing.T) {
	raw, err := abiOKEnvelopeWithError(nil, statusError{statusCode: 403, body: []byte("denied")})
	if err != nil {
		t.Fatalf("abiOKEnvelopeWithError returned transport error: %v", err)
	}

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Error == nil {
		t.Fatal("envelope Error = nil, want populated")
	}
	if env.Error.HTTPStatus != 403 {
		t.Fatalf("HTTPStatus = %d, want 403", env.Error.HTTPStatus)
	}
}

// TestAbiErrorEnvelopeWithoutStatusOmitsField keeps the default path clean: an
// error with no known status must not fabricate one.
func TestAbiErrorEnvelopeWithoutStatusOmitsField(t *testing.T) {
	raw := abiErrorEnvelope("plugin_error", "boom")

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Error == nil {
		t.Fatal("envelope Error = nil, want populated")
	}
	if env.Error.HTTPStatus != 0 {
		t.Fatalf("HTTPStatus = %d, want 0", env.Error.HTTPStatus)
	}
}
