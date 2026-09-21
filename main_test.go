package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAuthReadsOAuthWithoutExposingToken(t *testing.T) {
	home := t.TempDir()
	secret := "synthetic-access-token"
	data := `{"openai":{"oauth":{"access_token":"` + secret + `","account_id":"acct-test"}}}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := loadAuth(home, authSpec{Provider: "openai", Mode: "oauth"})
	if err != nil {
		t.Fatal(err)
	}
	if auth.accessToken != secret || auth.accountID != "acct-test" {
		t.Fatalf("auth = %#v", auth)
	}
}

func TestFetchUsesDefinitionAndNormalizesWindows(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"openai":{"oauth":{"access_token":"token","account_id":"acct"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOT_HOME", home)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "acct" {
			t.Errorf("account id = %q", got)
		}
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":28,"reset_at":4102444800}}}`))
	}))
	defer server.Close()

	definitionPath := filepath.Join(home, "provider.json")
	definition := providerDefinition{
		ID:          "test-provider",
		DisplayName: "Test Provider",
		Auth:        authSpec{Provider: "openai", Mode: "oauth"},
		Request:     requestSpec{URL: server.URL, Headers: map[string]string{"chatgpt-account-id": "$auth.account_id"}},
		Windows:     []windowSpec{{Name: "Primary", UsedPercent: "/rate_limit/primary_window/used_percent", ResetAt: "/rate_limit/primary_window/reset_at"}},
	}
	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definitionPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	a := &app{cache: make(map[string]cachedReport)}
	report, err := a.fetch("test-provider", definitionPath, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Windows) != 1 || report.Windows[0].UsedPercent == nil || *report.Windows[0].UsedPercent != 28 {
		t.Fatalf("report = %#v", report)
	}
	if got := formatReport(report); !strings.Contains(got, "28% used") {
		t.Fatalf("formatted report = %q", got)
	}
}

func TestExpiredOAuthIsRejected(t *testing.T) {
	home := t.TempDir()
	data := `{"openai":{"oauth":{"access_token":"token","account_id":"acct","expiry":"` + time.Now().Add(-time.Hour).Format(time.RFC3339) + `"}}}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadAuth(home, authSpec{Provider: "openai", Mode: "oauth"})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %v, want expired credential error", err)
	}
}
