package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/agent/ext"
)

const extensionName = "zot-usage-limites"

var version = "0.1.0"

type config struct {
	Enabled         bool                      `json:"enabled"`
	CacheTTLSeconds int                       `json:"cache_ttl_seconds,omitempty"`
	Providers       map[string]providerConfig `json:"providers,omitempty"`
}

type providerConfig struct {
	Enabled    bool   `json:"enabled"`
	Definition string `json:"definition"`
}

type providerDefinition struct {
	ID          string       `json:"id"`
	DisplayName string       `json:"display_name"`
	Auth        authSpec     `json:"auth"`
	Request     requestSpec  `json:"request"`
	Windows     []windowSpec `json:"windows"`
	Plan        string       `json:"plan,omitempty"`
}

type authSpec struct {
	Provider string `json:"provider"`
	Mode     string `json:"mode"`
}

type requestSpec struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

type windowSpec struct {
	Name        string `json:"name"`
	UsedPercent string `json:"used_percent"`
	ResetAt     string `json:"reset_at"`
	Unit        string `json:"unit,omitempty"`
}

type authFile struct {
	OpenAI credentials `json:"openai"`
}

type credentials struct {
	APIKey string     `json:"api_key,omitempty"`
	OAuth  *oauthData `json:"oauth,omitempty"`
}

type oauthData struct {
	AccessToken string    `json:"access_token"`
	AccountID   string    `json:"account_id"`
	Expiry      time.Time `json:"expiry,omitempty"`
}

type usageReport struct {
	Provider    string
	DisplayName string
	Plan        string
	FetchedAt   time.Time
	Windows     []usageWindow
}

type usageWindow struct {
	Name        string
	UsedPercent *float64
	ResetAt     *time.Time
	Unit        string
}

type app struct {
	mu    sync.Mutex
	root  string
	cache map[string]cachedReport
}

type cachedReport struct {
	report usageReport
	at     time.Time
}

func main() {
	a := &app{cache: make(map[string]cachedReport)}
	e := ext.New(extensionName, version)
	e.Command("limits", "show provider usage limits", func(string) ext.Response {
		return ext.Display(a.render())
	})
	e.OnHello(func(host ext.HostInfo) { a.root = host.ExtensionDir })
	if err := e.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] %v\n", extensionName, err)
	}
}

func (a *app) render() string {
	cfg, err := loadConfig(zotHome())
	if errors.Is(err, os.ErrNotExist) {
		return "Usage limits are disabled. Create $ZOT_HOME/zot-usage-limites.json with {\"enabled\": true} to enable them."
	}
	if err != nil {
		return "Usage limits configuration error: " + err.Error()
	}
	if !cfg.Enabled {
		return "Usage limits are disabled in $ZOT_HOME/zot-usage-limites.json."
	}
	if !cfg.Providers["openai-codex"].Enabled {
		return "OpenAI Codex usage limits are disabled in the extension configuration."
	}
	definition := cfg.Providers["openai-codex"].Definition
	if definition == "" {
		definition = "openai-codex.json"
	}
	report, err := a.fetch("openai-codex", filepath.Join(a.root, "providers", definition), cfg.CacheTTLSeconds)
	if err != nil {
		return "OpenAI Codex usage limits unavailable: " + err.Error()
	}
	return formatReport(report)
}

func (a *app) fetch(id, definitionPath string, ttl int) (usageReport, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ttl <= 0 {
		ttl = 60
	}
	if cached, ok := a.cache[id]; ok && time.Since(cached.at) < time.Duration(ttl)*time.Second {
		return cached.report, nil
	}
	definition, err := loadDefinition(definitionPath)
	if err != nil {
		return usageReport{}, err
	}
	auth, err := loadAuth(zotHome(), definition.Auth)
	if err != nil {
		return usageReport{}, err
	}
	method := definition.Request.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, definition.Request.URL, nil)
	if err != nil {
		return usageReport{}, fmt.Errorf("invalid provider URL: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+auth.accessToken)
	for key, value := range definition.Request.Headers {
		req.Header.Set(key, substituteAuth(value, auth))
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) > 0 && via[0].URL.Host != req.URL.Host {
			return errors.New("provider redirected to a different host")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return usageReport{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return usageReport{}, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	var body any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return usageReport{}, fmt.Errorf("invalid provider response: %w", err)
	}
	report := usageReport{Provider: definition.ID, DisplayName: definition.DisplayName, Plan: definition.Plan, FetchedAt: time.Now()}
	for _, spec := range definition.Windows {
		window := usageWindow{Name: spec.Name, Unit: spec.Unit}
		if value, ok := jsonNumber(pointer(body, spec.UsedPercent)); ok {
			window.UsedPercent = &value
		}
		if value, ok := jsonTime(pointer(body, spec.ResetAt)); ok {
			window.ResetAt = &value
		}
		report.Windows = append(report.Windows, window)
	}
	a.cache[id] = cachedReport{report: report, at: time.Now()}
	return report, nil
}

type loadedAuth struct{ accessToken, accountID string }

func loadAuth(home string, spec authSpec) (loadedAuth, error) {
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return loadedAuth{}, errors.New("zot OpenAI credentials were not found; run zot login")
		}
		return loadedAuth{}, fmt.Errorf("read zot auth.json: %w", err)
	}
	var file authFile
	if err := json.Unmarshal(data, &file); err != nil {
		return loadedAuth{}, fmt.Errorf("parse zot auth.json: %w", err)
	}
	if spec.Provider != "openai" {
		return loadedAuth{}, fmt.Errorf("unsupported auth provider %q", spec.Provider)
	}
	if spec.Mode == "oauth" {
		if file.OpenAI.OAuth == nil || file.OpenAI.OAuth.AccessToken == "" {
			return loadedAuth{}, errors.New("OpenAI OAuth credentials are not available; run zot login")
		}
		if !file.OpenAI.OAuth.Expiry.IsZero() && time.Now().After(file.OpenAI.OAuth.Expiry) {
			return loadedAuth{}, errors.New("OpenAI OAuth credentials have expired; run zot login")
		}
		return loadedAuth{accessToken: file.OpenAI.OAuth.AccessToken, accountID: file.OpenAI.OAuth.AccountID}, nil
	}
	if file.OpenAI.APIKey == "" {
		return loadedAuth{}, errors.New("OpenAI API credentials are not available")
	}
	return loadedAuth{accessToken: file.OpenAI.APIKey}, nil
}

func loadConfig(home string) (config, error) {
	var cfg config
	data, err := os.ReadFile(filepath.Join(home, "zot-usage-limites.json"))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func loadDefinition(path string) (providerDefinition, error) {
	var definition providerDefinition
	data, err := os.ReadFile(path)
	if err != nil {
		return definition, fmt.Errorf("read provider definition: %w", err)
	}
	if err := json.Unmarshal(data, &definition); err != nil {
		return definition, fmt.Errorf("parse provider definition: %w", err)
	}
	if definition.ID == "" || definition.Request.URL == "" {
		return definition, errors.New("provider definition is missing id or request URL")
	}
	return definition, nil
}

func zotHome() string {
	if home := os.Getenv("ZOT_HOME"); home != "" {
		return home
	}
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "zot")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "zot")
}

func substituteAuth(value string, auth loadedAuth) string {
	value = strings.ReplaceAll(value, "$auth.access_token", auth.accessToken)
	return strings.ReplaceAll(value, "$auth.account_id", auth.accountID)
}

func pointer(value any, path string) any {
	if path == "" || path == "/" {
		return value
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch current := value.(type) {
		case map[string]any:
			value = current[part]
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(current) {
				return nil
			}
			value = current[i]
		default:
			return nil
		}
	}
	return value
}

func jsonNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case json.Number:
		v, err := n.Float64()
		return v, err == nil
	case string:
		v, err := strconv.ParseFloat(n, 64)
		return v, err == nil
	}
	return 0, false
}

func jsonTime(value any) (time.Time, bool) {
	if n, ok := jsonNumber(value); ok {
		return time.Unix(int64(n), 0), true
	}
	if text, ok := value.(string); ok {
		t, err := time.Parse(time.RFC3339, text)
		return t, err == nil
	}
	return time.Time{}, false
}

func formatReport(report usageReport) string {
	lines := []string{report.DisplayName + " usage limits"}
	if report.Plan != "" {
		lines = append(lines, "Plan: "+report.Plan)
	}
	for _, window := range report.Windows {
		line := "  " + window.Name + ": "
		if window.UsedPercent == nil {
			line += "unavailable"
		} else {
			line += fmt.Sprintf("%.0f%% used", *window.UsedPercent)
		}
		if window.ResetAt != nil {
			line += ", resets " + relativeTime(*window.ResetAt)
		}
		lines = append(lines, line)
	}
	if len(report.Windows) == 0 {
		lines = append(lines, "  No usage windows were returned by the provider.")
	}
	return strings.Join(lines, "\n")
}

func relativeTime(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "now"
	}
	return "in " + d.Round(time.Minute).String()
}
