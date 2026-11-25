package executor

import (
	"net/http"
	"strings"
)

// Fork-owned: Anthropic's refusal of an API key with no credit left. The type embeds
// statusErr, so it sits in this package, not helps/.

// claudeCreditBalanceRefusal reports whether an upstream 400 is the refusal of an API
// key whose prepaid credit is spent. Another credential may still serve the request, so
// it is returned as a credential-wide 402: the key cools down, the request rotates, and
// the reserve gate sees the key as out of credit.
func claudeCreditBalanceRefusal(statusCode int, body []byte, err statusErr) (claudeCreditBalanceError, bool) {
	if statusCode != http.StatusBadRequest || !claudeBodyIndicatesCreditBalanceTooLow(body) {
		return claudeCreditBalanceError{}, false
	}
	err.code = http.StatusPaymentRequired
	return claudeCreditBalanceError{err}, true
}

// claudeCreditBalanceError is Anthropic's refusal of an API key with no credit left.
type claudeCreditBalanceError struct {
	statusErr
}

func (e claudeCreditBalanceError) IsCredentialScoped() bool {
	return true
}

func (e claudeCreditBalanceError) IsRequestScoped() bool {
	return false
}

// claudeBodyIndicatesCreditBalanceTooLow matches Anthropic's 400 invalid_request_error
// "Your credit balance is too low to access the Anthropic API".
func claudeBodyIndicatesCreditBalanceTooLow(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "credit balance is too low")
}
