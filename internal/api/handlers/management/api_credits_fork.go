package management

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Fork-owned: adds the Console API credit grant that comes with a Claude Max login to
// the usage read the management panel makes through api-call. The grant becomes one
// extra key in the usage body, shaped like the dollar windows the stock panel already
// draws (limit, spent, remaining, reset), so the panel needs no change. The numbers
// come from the state file of the credit-expiry job (linkendr tools,
// mac/cliproxy-credit-expiry). Only the v8 route gets the row; the deprecated v0 route
// stays as upstream has it. APICall keeps a one-line call into this file.

const (
	// apiCreditsEnv overrides the path of the credit-expiry state file.
	apiCreditsEnv = "CLIPROXY_API_CREDITS"
	// apiCreditsKey is the usage body key; the panel shows it as the row label.
	apiCreditsKey = "API credits"
	// apiCreditsMaxAge drops a state file the job has not written for this long.
	apiCreditsMaxAge = 15 * time.Minute
	claudeUsageHost  = "api.anthropic.com"
	claudeUsagePath  = "/api/oauth/usage"
)

type apiCreditsState struct {
	CheckedAt string           `json:"checked_at"`
	Grants    []apiCreditGrant `json:"grants"`
}

type apiCreditGrant struct {
	Account      string   `json:"account"`
	Expires      string   `json:"expires"`
	AmountUSD    *float64 `json:"amount_usd"`
	SpentUSD     *float64 `json:"spent_usd"`
	RemainingUSD *float64 `json:"remaining_usd"`
}

// apiCreditsWindow is the dollar window shape the panel reads from a usage body.
type apiCreditsWindow struct {
	LimitDollars     float64 `json:"limit_dollars"`
	UsedDollars      float64 `json:"used_dollars"`
	RemainingDollars float64 `json:"remaining_dollars"`
	Utilization      float64 `json:"utilization"`
	ResetsAt         string  `json:"resets_at"`
}

func apiCreditsPath() string {
	if path := strings.TrimSpace(os.Getenv(apiCreditsEnv)); path != "" {
		return path
	}
	if testing.Testing() {
		return ""
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "cliproxyapi", "credit-expiry", "state.json")
}

// withAPICredits returns body with the account's credit grant added when the call is a
// successful v8 usage read of a Claude login and the state file holds a fresh grant for
// that login's email. Any other call, or any missing piece, returns body unchanged.
func withAPICredits(v8 bool, auth *coreauth.Auth, method string, target *url.URL, status int, body []byte, now time.Time) []byte {
	if !v8 || auth == nil || target == nil || status != http.StatusOK || method != http.MethodGet {
		return body
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") ||
		!strings.EqualFold(target.Hostname(), claudeUsageHost) || target.Path != claudeUsagePath {
		return body
	}
	email, _ := auth.Metadata["email"].(string)
	email = strings.TrimSpace(email)
	if email == "" {
		return body
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return body
	}
	window, ok := readAPICreditWindow(apiCreditsPath(), email, now)
	if !ok {
		return body
	}
	doc := ojson.NewDoc(trimmed)
	if errSet := doc.Set(apiCreditsKey, window); errSet != nil {
		log.Warnf("api credits: cannot add the credit row to the usage body: %v", errSet)
		return body
	}
	return doc.Bytes()
}

func readAPICreditWindow(path, email string, now time.Time) (apiCreditsWindow, bool) {
	if path == "" {
		return apiCreditsWindow{}, false
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		log.Debugf("api credits: state file %s not read: %v", path, errRead)
		return apiCreditsWindow{}, false
	}
	var state apiCreditsState
	if errJSON := json.Unmarshal(raw, &state); errJSON != nil {
		log.Warnf("api credits: state file %s is not valid JSON: %v", path, errJSON)
		return apiCreditsWindow{}, false
	}
	checked, errTime := time.Parse(time.RFC3339Nano, state.CheckedAt)
	if errTime != nil {
		log.Warnf("api credits: state file %s has no valid checked_at: %v", path, errTime)
		return apiCreditsWindow{}, false
	}
	if age := now.Sub(checked); age > apiCreditsMaxAge || age < -apiCreditsMaxAge {
		log.Debugf("api credits: state file %s is %s old, not shown", path, age.Round(time.Second))
		return apiCreditsWindow{}, false
	}
	for _, grant := range state.Grants {
		if !strings.EqualFold(strings.TrimSpace(grant.Account), email) {
			continue
		}
		if grant.AmountUSD == nil || grant.SpentUSD == nil || grant.RemainingUSD == nil || *grant.AmountUSD <= 0 {
			log.Warnf("api credits: grant for %s in %s lacks amount_usd, spent_usd or remaining_usd", email, path)
			return apiCreditsWindow{}, false
		}
		return apiCreditsWindow{
			LimitDollars:     *grant.AmountUSD,
			UsedDollars:      *grant.SpentUSD,
			RemainingDollars: *grant.RemainingUSD,
			Utilization:      *grant.SpentUSD / *grant.AmountUSD * 100,
			ResetsAt:         grant.Expires,
		}, true
	}
	return apiCreditsWindow{}, false
}
