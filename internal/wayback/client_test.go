package wayback

import (
	"errors"
	"net/http"
	"testing"
)

func TestNewClientUsesHTTP11Transport(t *testing.T) {
	client := NewClient("access", "secret")
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.httpClient.Transport)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = true, want false")
	}
	if transport.TLSNextProto != nil {
		t.Fatal("TLSNextProto is configured, want HTTP/2 disabled")
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil")
	}
	if len(transport.TLSClientConfig.NextProtos) != 1 || transport.TLSClientConfig.NextProtos[0] != "http/1.1" {
		t.Fatalf("TLS ALPN protocols = %v, want [http/1.1]", transport.TLSClientConfig.NextProtos)
	}
}

func TestSubmissionResultClassifiesImmediatePermanentError(t *testing.T) {
	_, err := submissionResult(submitResponse{
		Status:    "error",
		StatusExt: "error:blocked-url",
		Message:   "This URL is in the Save Page Now service block list.",
	}, "https://example.com")

	var permanent *PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("error = %v, want PermanentError", err)
	}
	if permanent.StatusExt != "error:blocked-url" {
		t.Fatalf("StatusExt = %q, want error:blocked-url", permanent.StatusExt)
	}
}

func TestSubmissionResultReturnsImmediateCapture(t *testing.T) {
	result, err := submissionResult(submitResponse{
		Status:    "success",
		Timestamp: "20260731000000",
	}, "https://example.com")
	if err != nil {
		t.Fatalf("submissionResult: %v", err)
	}
	const want = "https://web.archive.org/web/20260731000000/https://example.com"
	if result == nil || result.ArchiveURL != want {
		t.Fatalf("result = %#v, want archive URL %q", result, want)
	}
}

func TestSubmissionResultClassifiesGoneAsPermanent(t *testing.T) {
	_, err := submissionResult(submitResponse{
		Status:    "error",
		StatusExt: "error:gone",
		Message:   "The requested resource is gone.",
	}, "https://example.com")

	var permanent *PermanentError
	if !errors.As(err, &permanent) {
		t.Fatalf("error = %v, want PermanentError", err)
	}
	if permanent.StatusExt != "error:gone" {
		t.Fatalf("StatusExt = %q, want error:gone", permanent.StatusExt)
	}
}
