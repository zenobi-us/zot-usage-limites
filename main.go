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

const extensionName = "zot-usage-limits"

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

type providerCandidate struct {
	Definition providerDefinition
	Path       string
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

const limitsPanelID = "usage-limits"

type app struct {
	mu      sync.Mutex
	panelMu sync.Mutex
	root    string
	cache   map[string]cachedReport
	panel   panelState
}

type panelState struct {
	settings     bool
	verbose      bool
	selected     int
	lines        []string
	verboseLines []string
	pendingSince time.Time
}

type cachedReport struct {
	report usageReport
	at     time.Time
}

func main() {
	a := &app{cache: make(map[string]cachedReport)}
	e := ext.New(extensionName, version)
	e.Command("usage", "show provider usage limits", func(string) ext.Response {
		a.resetPanel()
		a.setPending(true)
		go a.refreshPanel(e)
		return ext.OpenPanel(limitsPanelID, "/usage", a.panelLines(""), panelFooter(false))
	})
	e.OnPanelKey(limitsPanelID, func(key, text string) {
		if key == "rune" {
			key = text
		}
		switch key {
		case "up":
			a.moveSelection(-1)
		case "down":
			a.moveSelection(1)
		case "esc":
			if a.inSettings() {
				a.setSettings(false)
				e.RenderPanel(limitsPanelID, "/usage", a.panelLines(""), panelFooter(false))
			}
		case "r":
			a.setPending(true)
			go a.refreshPanel(e)
		case "s":
			a.setSettings(true)
		case "v":
			a.toggleVerbose()
		}
		if key == "r" || key == "s" || key == "v" || key == "up" || key == "down" {
			e.RenderPanel(limitsPanelID, a.panelTitle(), a.panelLines(""), panelFooter(a.inSettings()))
		}
	}, func() { a.resetPanel() })
	e.OnHello(func(host ext.HostInfo) { a.root = host.ExtensionDir })
	if err := e.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] %v\n", extensionName, err)
	}
}

func (a *app) resetPanel() {
	a.panelMu.Lock()
	a.panel = panelState{}
	a.panelMu.Unlock()
}

func (a *app) setSettings(settings bool) {
	a.panelMu.Lock()
	a.panel.settings = settings
	a.panel.selected = 0
	a.panelMu.Unlock()
}

func (a *app) inSettings() bool {
	a.panelMu.Lock()
	defer a.panelMu.Unlock()
	return a.panel.settings
}

func (a *app) moveSelection(delta int) {
	a.panelMu.Lock()
	defer a.panelMu.Unlock()
	if !a.panel.settings {
		return
	}
	a.panel.selected = (a.panel.selected + delta + 2) % 2
}

func (a *app) panelTitle() string {
	if a.inSettings() {
		return "/usage / settings"
	}
	return "/usage"
}

func panelFooter(settings bool) string {
	if settings {
		return "↑/↓ move   enter change   esc back   r refresh"
	}
	return "↑/↓ move   r refresh   v verbose   s settings   esc close"
}

func (a *app) panelLines(status string) []string {
	a.panelMu.Lock()
	settings, selected := a.panel.settings, a.panel.selected
	verbose := a.panel.verbose
	cachedLines := append([]string(nil), a.panel.lines...)
	if verbose {
		cachedLines = append([]string(nil), a.panel.verboseLines...)
	}
	a.panelMu.Unlock()
	if status != "" {
		return []string{status}
	}
	if !a.pendingSince().IsZero() {
		return []string{fmt.Sprintf("Request pending… %s", elapsed(a.pendingSince()))}
	}
	if settings {
		providerMarker, cacheMarker := "  ", "  "
		if selected == 0 {
			providerMarker = "▸ "
		} else {
			cacheMarker = "▸ "
		}
		lines := []string{providerMarker + "provider       auto-detect from auth.json", cacheMarker + "cache          configured TTL (default 60 seconds)"}
		if status != "" {
			lines = append(lines, "", status)
		}
		return lines
	}
	if len(cachedLines) > 0 {
		return cachedLines
	}
	return []string{"No usage data loaded."}
}

func (a *app) toggleVerbose() {
	a.panelMu.Lock()
	a.panel.verbose = !a.panel.verbose
	a.panelMu.Unlock()
}

func (a *app) setPanelLines(lines, verboseLines []string) {
	a.panelMu.Lock()
	a.panel.lines = append([]string(nil), lines...)
	a.panel.verboseLines = append([]string(nil), verboseLines...)
	a.panelMu.Unlock()
}

func (a *app) setPending(pending bool) {
	a.panelMu.Lock()
	if pending {
		a.panel.pendingSince = time.Now()
	} else {
		a.panel.pendingSince = time.Time{}
	}
	a.panelMu.Unlock()
}

func (a *app) pendingSince() time.Time {
	a.panelMu.Lock()
	defer a.panelMu.Unlock()
	return a.panel.pendingSince
}

func elapsed(since time.Time) string {
	if since.IsZero() {
		return "0s"
	}
	return time.Since(since).Round(time.Second).String()
}

func (a *app) refreshPanel(e *ext.Extension) {
	stop := make(chan struct{})
	go a.renderPending(e, stop)
	defer close(stop)
	defer a.setPending(false)

	cfg, err := loadConfig(zotHome())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		a.setPending(false)
		e.RenderPanel(limitsPanelID, "/usage", []string{"Configuration error: " + err.Error()}, panelFooter(a.inSettings()))
		return
	}
	candidates, err := a.detectProviders()
	if err != nil {
		a.setPending(false)
		e.RenderPanel(limitsPanelID, "/usage", []string{"Provider detection error: " + err.Error()}, panelFooter(a.inSettings()))
		return
	}
	lines := []string{}
	verboseLines := []string{}
	for _, candidate := range candidates {
		override, configured := cfg.Providers[candidate.Definition.ID]
		if configured && !override.Enabled {
			continue
		}
		ttl := cfg.CacheTTLSeconds
		report, fetchErr := a.fetch(candidate.Definition.ID, candidate.Path, ttl)
		if fetchErr != nil {
			failure := []string{candidate.Definition.DisplayName + ": unavailable", "  " + fetchErr.Error(), ""}
			lines = append(lines, failure...)
			verboseLines = append(verboseLines, failure...)
			continue
		}
		reportLines := panelReportLines(report, false)
		lines = append(lines, reportLines...)
		verboseLines = append(verboseLines, panelReportLines(report, true)...)
	}
	if len(lines) == 0 {
		lines = []string{"No providers found for the credentials in zot auth.json."}
		verboseLines = append([]string(nil), lines...)
	}
	a.setPanelLines(lines, verboseLines)
	a.setPending(false)
	e.RenderPanel(limitsPanelID, a.panelTitle(), a.panelLines(""), panelFooter(a.inSettings()))
}

func (a *app) renderPending(e *ext.Extension, stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.RenderPanel(limitsPanelID, a.panelTitle(), a.panelLines(""), panelFooter(a.inSettings()))
		case <-stop:
			return
		}
	}
}

func (a *app) detectProviders() ([]providerCandidate, error) {
	entries, err := os.ReadDir(filepath.Join(a.root, "providers"))
	if err != nil {
		return nil, fmt.Errorf("read provider definitions: %w", err)
	}
	result := make([]providerCandidate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(a.root, "providers", entry.Name())
		definition, err := loadDefinition(path)
		if err != nil {
			continue
		}
		if authAvailable(zotHome(), definition.Auth) {
			result = append(result, providerCandidate{Definition: definition, Path: path})
		}
	}
	return result, nil
}

func panelReportLines(report usageReport, verbose bool) []string {
	lines := make([]string, 0, len(report.Windows)*2+7)
	if report.DisplayName != "" {
		lines = append(lines, report.DisplayName+" usage limits")
	}
	if verbose {
		if report.Provider != "" {
			lines = append(lines, "provider "+report.Provider)
		}
		if report.Plan != "" {
			lines = append(lines, "plan "+report.Plan)
		}
	}
	for _, window := range report.Windows {
		line := "✓ " + window.Name
		if window.UsedPercent == nil {
			line += "      unavailable"
		} else {
			line += fmt.Sprintf("      %.0f%% used", *window.UsedPercent)
		}
		if verbose && window.Unit != "" {
			line += " (" + window.Unit + ")"
		}
		lines = append(lines, line)
		if window.ResetAt != nil {
			lines = append(lines, "  resets "+relativeTime(*window.ResetAt))
		}
	}
	updated := "updated just now"
	if !report.FetchedAt.IsZero() {
		updated = "updated " + relativeTime(report.FetchedAt)
	}
	lines = append(lines, "", updated)
	if verbose && !report.FetchedAt.IsZero() {
		lines = append(lines, "fetched at "+report.FetchedAt.Format(time.RFC3339))
	}
	return lines
}

func (a *app) render() string {
	cfg, err := loadConfig(zotHome())
	if errors.Is(err, os.ErrNotExist) {
		return "Usage limits are disabled. Create $ZOT_HOME/zot-usage-limits.json with {\"enabled\": true} to enable them."
	}
	if err != nil {
		return "Usage limits configuration error: " + err.Error()
	}
	if !cfg.Enabled {
		return "Usage limits are disabled in $ZOT_HOME/zot-usage-limits.json."
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
			return loadedAuth{}, fmt.Errorf("%s credentials were not found; run zot login", spec.Provider)
		}
		return loadedAuth{}, fmt.Errorf("read zot auth.json: %w", err)
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(data, &providers); err != nil {
		return loadedAuth{}, fmt.Errorf("parse zot auth.json: %w", err)
	}
	raw, ok := providers[spec.Provider]
	if !ok {
		if additional, exists := providers["additional_api_key_creds"]; exists {
			var entries map[string]json.RawMessage
			if json.Unmarshal(additional, &entries) == nil {
				raw, ok = entries[spec.Provider]
			}
		}
	}
	if !ok {
		return loadedAuth{}, fmt.Errorf("%s credentials are not available", spec.Provider)
	}
	var credentials map[string]json.RawMessage
	if err := json.Unmarshal(raw, &credentials); err != nil {
		return loadedAuth{}, fmt.Errorf("parse %s credentials: %w", spec.Provider, err)
	}
	if spec.Mode == "oauth" {
		var oauth oauthData
		if value, ok := credentials["oauth"]; !ok || json.Unmarshal(value, &oauth) != nil || oauth.AccessToken == "" {
			return loadedAuth{}, fmt.Errorf("%s OAuth credentials are not available; run zot login", spec.Provider)
		}
		if !oauth.Expiry.IsZero() && time.Now().After(oauth.Expiry) {
			return loadedAuth{}, fmt.Errorf("%s OAuth credentials have expired; run zot login", spec.Provider)
		}
		return loadedAuth{accessToken: oauth.AccessToken, accountID: oauth.AccountID}, nil
	}
	var apiKey string
	if value, ok := credentials["api_key"]; ok {
		_ = json.Unmarshal(value, &apiKey)
	}
	if apiKey == "" {
		return loadedAuth{}, fmt.Errorf("%s API credentials are not available", spec.Provider)
	}
	return loadedAuth{accessToken: apiKey}, nil
}

func authAvailable(home string, spec authSpec) bool {
	_, err := loadAuth(home, spec)
	return err == nil
}

func loadConfig(home string) (config, error) {
	var cfg config
	data, err := os.ReadFile(filepath.Join(home, "zot-usage-limits.json"))
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
