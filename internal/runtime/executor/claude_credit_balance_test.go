package executor

import (
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
)

// An API key with no prepaid credit left is a credential failure, not a request fault:
// it is reported as a credential-wide 402 so the key cools down and the request rotates.
func TestClassifyClaudeCreditBalanceTooLow(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`)
	err := classifyClaudeUpstreamError(http.StatusBadRequest, http.Header{}, body)
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusPaymentRequired {
		t.Fatalf("status = %v, want 402", err)
	}
	var scoped interface{ IsCredentialScoped() bool }
	if !errors.As(err, &scoped) || !scoped.IsCredentialScoped() {
		t.Fatal("credit refusal is not credential scoped")
	}
	if clienterror.IsRequestFault(status.StatusCode(), err) {
		t.Fatal("credit refusal classified as a request fault")
	}
}

func TestClassifyClaudeOtherBadRequestUnchanged(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: field required"}}`)
	err := classifyClaudeUpstreamError(http.StatusBadRequest, http.Header{}, body)
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %v, want 400", err)
	}
}
