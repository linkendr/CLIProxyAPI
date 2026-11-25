package management

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const apiCreditsUsageBody = `{"seven_day":{"utilization":95.0,"resets_at":"2026-11-26T23:00:00Z"},"extra_usage":{"is_enabled":false}}`

var apiCreditsNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func writeAPICreditsState(t *testing.T, checkedAt string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	state := `{"checked_at":"` + checkedAt + `","config_changed":false,"grants":[` +
		`{"key_name":"claude-console-credit","account":"work@example.com","expires":"2026-12-09T00:00:00+00:00",` +
		`"amount_usd":200.0,"spent_usd":97.95,"remaining_usd":102.05,"draining":false}]}`
	if errWrite := os.WriteFile(path, []byte(state), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	t.Setenv(apiCreditsEnv, path)
}

func claudeAuth(email string) *coreauth.Auth {
	return &coreauth.Auth{Provider: "claude", Metadata: map[string]any{"email": email}}
}

func usageURL(t *testing.T) *url.URL {
	t.Helper()
	u, errParse := url.Parse("https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1")
	if errParse != nil {
		t.Fatal(errParse)
	}
	return u
}

func TestWithAPICreditsAddsDollarRow(t *testing.T) {
	writeAPICreditsState(t, "2026-11-25T11:58:52.938158+00:00")
	out := string(withAPICredits(true, claudeAuth("Work@Example.com"), "GET", usageURL(t), 200, []byte(apiCreditsUsageBody), apiCreditsNow))

	row := ojson.Get(out, "API credits")
	if !row.Exists() {
		t.Fatalf("no credit row in %s", out)
	}
	for field, want := range map[string]float64{"limit_dollars": 200, "used_dollars": 97.95, "remaining_dollars": 102.05} {
		if got := row.Get(field).Float(); got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
	if got := row.Get("utilization").Float(); got < 48.97 || got > 48.98 {
		t.Errorf("utilization = %v, want 48.975", got)
	}
	if got := row.Get("resets_at").String(); got != "2026-12-09T00:00:00+00:00" {
		t.Errorf("resets_at = %q", got)
	}
	if got := ojson.Get(out, "seven_day.utilization").Float(); got != 95 {
		t.Errorf("seven_day changed: %v", got)
	}
}

func TestWithAPICreditsLeavesOtherCallsUnchanged(t *testing.T) {
	writeAPICreditsState(t, "2026-11-25T11:58:52+00:00")
	body := []byte(apiCreditsUsageBody)
	other, errParse := url.Parse("https://api.anthropic.com/api/oauth/profile")
	if errParse != nil {
		t.Fatal(errParse)
	}
	cases := map[string][]byte{
		"v0 route":      withAPICredits(false, claudeAuth("work@example.com"), "GET", usageURL(t), 200, body, apiCreditsNow),
		"other account": withAPICredits(true, claudeAuth("general@example.com"), "GET", usageURL(t), 200, body, apiCreditsNow),
		"other path":    withAPICredits(true, claudeAuth("work@example.com"), "GET", other, 200, body, apiCreditsNow),
		"error status":  withAPICredits(true, claudeAuth("work@example.com"), "GET", usageURL(t), 429, body, apiCreditsNow),
		"codex auth":    withAPICredits(true, &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"email": "work@example.com"}}, "GET", usageURL(t), 200, body, apiCreditsNow),
		"no auth":       withAPICredits(true, nil, "GET", usageURL(t), 200, body, apiCreditsNow),
	}
	for name, out := range cases {
		if string(out) != apiCreditsUsageBody {
			t.Errorf("%s: body changed to %s", name, out)
		}
	}
}

func TestWithAPICreditsSkipsStaleOrMissingState(t *testing.T) {
	body := []byte(apiCreditsUsageBody)
	writeAPICreditsState(t, "2026-11-25T11:40:00+00:00")
	if out := withAPICredits(true, claudeAuth("work@example.com"), "GET", usageURL(t), 200, body, apiCreditsNow); string(out) != apiCreditsUsageBody {
		t.Errorf("stale state added a row: %s", out)
	}
	t.Setenv(apiCreditsEnv, filepath.Join(t.TempDir(), "missing.json"))
	if out := withAPICredits(true, claudeAuth("work@example.com"), "GET", usageURL(t), 200, body, apiCreditsNow); string(out) != apiCreditsUsageBody {
		t.Errorf("missing state added a row: %s", out)
	}
}
