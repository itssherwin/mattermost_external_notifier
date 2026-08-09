package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

const (
	defaultTimeoutSeconds = 10
	maxTimeoutSeconds     = 120

	providerGeneric   = "generic"
	providerKavenegar = "kavenegar"

	kavenegarBaseURL = "https://api.kavenegar.com/v1/%s/sms/send.json"
)

type Plugin struct {
	plugin.MattermostPlugin

	mu     sync.RWMutex
	table  map[string]string
	config Configuration
	client *http.Client

	wg sync.WaitGroup
}

type Configuration struct {
	Enabled bool `json:"enabled"`

	TeamIDs    []string `json:"team_ids"`
	ChannelIDs []string `json:"channel_ids"`

	// Provider selects how a notification is delivered.
	// "generic"   -> JSON POST to NotifyURL (original behavior)
	// "kavenegar" -> KavehNegar SMS REST API
	Provider string `json:"provider"`

	// Generic webhook provider settings.
	NotifyURL string `json:"notify_url"`
	AuthToken string `json:"auth_token"`

	// KavehNegar provider settings.
	KavenegarAPIKey   string `json:"kavenegar_api_key"`
	KavenegarSender   string `json:"kavenegar_sender"`
	KavenegarTemplate string `json:"kavenegar_message_template"`

	CSVPath        string `json:"csv_path"`
	TimeoutSeconds int    `json:"timeout_seconds"`

	// ExpandGroupMentions, when true, causes @group-name mentions that match
	// a real Mattermost Group to notify every member of that group, in
	// addition to (or instead of) a plain @username mention.
	ExpandGroupMentions bool `json:"expand_group_mentions"`
}

type Notification struct {
	Name      string `json:"name"`
	Number    string `json:"number"`
	PostID    string `json:"post_id"`
	UserID    string `json:"user_id"`
	TeamID    string `json:"team_id"`
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
}

// kavenegarResponse mirrors the small slice of KavehNegar's response we
// care about for error reporting. See https://kavenegar.com/rest.html
type kavenegarResponse struct {
	Return struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
	} `json:"return"`
}

func (p *Plugin) OnActivate() error {
	p.setClient(&http.Client{Timeout: defaultTimeoutSeconds * time.Second})

	p.mu.Lock()
	p.config = defaultConfiguration()
	p.mu.Unlock()

	if err := p.reloadConfiguration(); err != nil {
		return err
	}
	return nil
}

func (p *Plugin) OnDeactivate() error {
	p.wg.Wait()
	return nil
}

func (p *Plugin) OnConfigurationChange() error {
	return p.reloadConfiguration()
}

func (p *Plugin) reloadConfiguration() error {
	config := defaultConfiguration()
	if err := p.API.LoadPluginConfiguration(&config); err != nil {
		return fmt.Errorf("load plugin configuration: %w", err)
	}

	config.TeamIDs = normalizeIDList(config.TeamIDs)
	config.ChannelIDs = normalizeIDList(config.ChannelIDs)
	config.Provider = normalizeProvider(config.Provider)

	if err := validateConfiguration(config); err != nil {
		return err
	}

	table, err := loadCSV(config.CSVPath)
	if err != nil {
		return fmt.Errorf("load CSV: %w", err)
	}

	timeout := time.Duration(config.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds * time.Second
	}

	p.mu.Lock()
	p.config = config
	p.table = table
	p.mu.Unlock()

	// Guarded by setClient so notify() never reads a client concurrently
	// with this write (previously a data race).
	p.setClient(&http.Client{Timeout: timeout})

	p.API.LogInfo(
		"mention notifier configuration loaded",
		"provider", config.Provider,
		"teams", len(config.TeamIDs),
		"channels", len(config.ChannelIDs),
		"csv_entries", len(table),
		"timeout_seconds", config.TimeoutSeconds,
	)

	return nil
}

func (p *Plugin) setClient(c *http.Client) {
	p.mu.Lock()
	p.client = c
	p.mu.Unlock()
}

func (p *Plugin) getClient() *http.Client {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.client
}

func (p *Plugin) MessageHasBeenPosted(
	_ *plugin.Context,
	post *model.Post,
) {
	if post == nil {
		return
	}

	config := p.getConfiguration()
	if !config.Enabled {
		return
	}

	channel, appErr := p.API.GetChannel(post.ChannelId)
	if appErr != nil {
		p.API.LogError(
			"failed to resolve channel for post",
			"channel_id", post.ChannelId,
			"error", appErr.Error(),
		)
		return
	}
	if channel == nil {
		p.API.LogError(
			"failed to resolve channel for post",
			"channel_id", post.ChannelId,
			"error", "channel was nil",
		)
		return
	}

	teamID := channel.TeamId

	if !p.isMonitoredScope(
		teamID,
		post.ChannelId,
		config.TeamIDs,
		config.ChannelIDs,
	) {
		return
	}

	mentions := extractMentions(post.Message)
	if len(mentions) == 0 {
		return
	}

	seen := make(map[string]struct{}, len(mentions))
	for _, token := range mentions {
		targets := []string{token}

		if config.ExpandGroupMentions {
			if members, isGroup := p.resolveGroupMembers(token); isGroup {
				// token matched a real Mattermost Group: notify its
				// members instead of treating "token" as a username.
				targets = members
			}
		}

		for _, name := range targets {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}

			number := p.lookup(name)
			if number == "" {
				p.API.LogDebug(
					"mentioned user has no CSV mapping",
					"username", name,
				)
				continue
			}

			p.wg.Add(1)
			go func(name, number, teamID string, post *model.Post, config Configuration) {
				defer p.wg.Done()
				p.notify(config, name, number, teamID, post)
			}(name, number, teamID, post, config)
		}
	}
}

// resolveGroupMembers checks whether token matches a real Mattermost Group
// by name. If it does, it returns every member's lowercased username and
// true. If token doesn't match a group, it returns (nil, false) and the
// caller should treat token as a plain @username mention instead.
func (p *Plugin) resolveGroupMembers(token string) ([]string, bool) {
	group, appErr := p.API.GetGroupByName(token)
	if appErr != nil || group == nil {
		return nil, false
	}

	usernames, err := p.groupMemberUsernames(group.Id)
	if err != nil {
		p.API.LogError(
			"failed to list members for group mention",
			"group", token,
			"error", err.Error(),
		)
		// The group exists but we couldn't list its members - do not fall
		// back to treating "token" as a literal username, since it almost
		// certainly isn't one.
		return nil, true
	}

	return usernames, true
}

func (p *Plugin) groupMemberUsernames(groupID string) ([]string, error) {
	const perPage = 200

	var usernames []string
	for page := 0; ; page++ {
		users, appErr := p.API.GetGroupMemberUsers(groupID, page, perPage)
		if appErr != nil {
			return nil, errors.New(appErr.Error())
		}
		for _, u := range users {
			if u != nil && u.Username != "" {
				usernames = append(usernames, strings.ToLower(u.Username))
			}
		}
		if len(users) < perPage {
			break
		}
	}

	return usernames, nil
}

func (p *Plugin) getConfiguration() Configuration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.config
}

func (p *Plugin) isMonitoredScope(
	teamID string,
	channelID string,
	teamIDs []string,
	channelIDs []string,
) bool {
	return matchesConfiguredID(teamID, teamIDs) &&
		matchesConfiguredID(channelID, channelIDs)
}

func matchesConfiguredID(value string, configured []string) bool {
	if len(configured) == 0 {
		return true
	}
	for _, item := range configured {
		if item == value {
			return true
		}
	}
	return false
}

func (p *Plugin) lookup(name string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.table[name]
}

func loadCSV(configuredPath string) (map[string]string, error) {
	filename := strings.TrimSpace(configuredPath)
	if filename == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		filename = filepath.Join(filepath.Dir(exe), "data.csv")
	}

	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1

	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	table := make(map[string]string)
	for i, row := range records {
		if len(row) < 2 {
			continue
		}
		name := normalizeUsername(row[0])
		number := strings.TrimSpace(row[1])

		if i == 0 && isCSVHeader(name, number) {
			continue
		}
		if name == "" || number == "" {
			continue
		}
		table[name] = number
	}

	return table, nil
}

func isCSVHeader(name, number string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	number = strings.ToLower(strings.TrimSpace(number))
	return (name == "username" || name == "user" || name == "name") &&
		(number == "number" || number == "phone" || number == "phone_number")
}

func normalizeUsername(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "@")
	return value
}

// notify dispatches a single notification through whichever provider is
// configured.
func (p *Plugin) notify(
	config Configuration,
	name string,
	number string,
	teamID string,
	post *model.Post,
) {
	if post == nil {
		return
	}

	switch config.Provider {
	case providerKavenegar:
		p.notifyKavenegar(config, name, number, post)
	default:
		p.notifyGeneric(config, name, number, teamID, post)
	}
}

func (p *Plugin) notifyGeneric(
	config Configuration,
	name string,
	number string,
	teamID string,
	post *model.Post,
) {
	u, err := url.Parse(strings.TrimSpace(config.NotifyURL))
	if err != nil {
		p.API.LogError("invalid notification URL", "error", err.Error())
		return
	}

	payload := Notification{
		Name:      name,
		Number:    number,
		PostID:    post.Id,
		UserID:    post.UserId,
		TeamID:    teamID,
		ChannelID: post.ChannelId,
		Message:   post.Message,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		p.API.LogError("failed to encode notification", "error", err.Error())
		return
	}

	req, err := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(string(body)))
	if err != nil {
		p.API.LogError("failed to create HTTP request", "error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(config.AuthToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.clientFor(config).Do(req)
	if err != nil {
		p.API.LogError(
			"notification request failed",
			"name", name,
			"error", err.Error(),
		)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		p.API.LogError(
			"notification returned non-2xx",
			"name", name,
			"status", resp.Status,
		)
	}
}

// notifyKavenegar sends an SMS via the KavehNegar REST API:
// GET https://api.kavenegar.com/v1/{API-KEY}/sms/send.json?receptor=...&sender=...&message=...
// Docs: https://kavenegar.com/rest.html
func (p *Plugin) notifyKavenegar(
	config Configuration,
	name string,
	number string,
	post *model.Post,
) {
	apiKey := strings.TrimSpace(config.KavenegarAPIKey)
	if apiKey == "" {
		p.API.LogError("kavenegar_api_key is not configured")
		return
	}

	receptor := normalizeIranianNumber(number)
	if receptor == "" {
		p.API.LogError("no usable phone number for recipient", "name", name)
		return
	}

	message := renderKavenegarMessage(config.KavenegarTemplate, name, post.Message)

	endpoint := fmt.Sprintf(kavenegarBaseURL, url.PathEscape(apiKey))

	q := url.Values{}
	q.Set("receptor", receptor)
	q.Set("message", message)
	if sender := strings.TrimSpace(config.KavenegarSender); sender != "" {
		q.Set("sender", sender)
	}

	req, err := http.NewRequest(http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		p.API.LogError("failed to create KavehNegar request", "error", err.Error())
		return
	}

	resp, err := p.clientFor(config).Do(req)
	if err != nil {
		p.API.LogError(
			"kavenegar request failed",
			"name", name,
			"error", err.Error(),
		)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		p.API.LogError(
			"kavenegar returned non-2xx",
			"name", name,
			"status", resp.Status,
		)
		return
	}

	// KavehNegar returns HTTP 200 even for some API-level errors (e.g. bad
	// credit, invalid sender), so the response body must be checked too.
	var parsed kavenegarResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		p.API.LogError("failed to decode kavenegar response", "error", err.Error())
		return
	}
	if parsed.Return.Status != http.StatusOK {
		p.API.LogError(
			"kavenegar rejected the message",
			"name", name,
			"status", parsed.Return.Status,
			"message", parsed.Return.Message,
		)
	}
}

func (p *Plugin) clientFor(config Configuration) *http.Client {
	if client := p.getClient(); client != nil {
		return client
	}
	timeout := time.Duration(config.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// renderKavenegarMessage fills a simple {{user}} / {{message}} template, or
// falls back to a sane default if no template is configured.
func renderKavenegarMessage(template, name, postMessage string) string {
	template = strings.TrimSpace(template)
	if template == "" {
		return fmt.Sprintf("You were mentioned by @%s: %s", name, postMessage)
	}
	out := strings.ReplaceAll(template, "{{user}}", name)
	out = strings.ReplaceAll(out, "{{message}}", postMessage)
	return out
}

// normalizeIranianNumber does light cleanup so CSV entries can be stored as
// 0912xxxxxxx, +98912xxxxxxx, or 98912xxxxxxx and still work with KavehNegar,
// which expects the receptor without a leading '+'.
func normalizeIranianNumber(raw string) string {
	number := strings.TrimSpace(raw)
	number = strings.ReplaceAll(number, " ", "")
	number = strings.ReplaceAll(number, "-", "")
	number = strings.TrimPrefix(number, "+")
	return number
}

var mentionRegexp = regexp.MustCompile(
	`(?i)(?:^|[^a-zA-Z0-9_])@([a-zA-Z0-9][a-zA-Z0-9_.-]*)`,
)

func extractMentions(message string) []string {
	matches := mentionRegexp.FindAllStringSubmatch(message, -1)
	result := make([]string, 0, len(matches))
	seen := make(map[string]struct{})

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		name := normalizeUsername(match[1])
		// A trailing '.' or '-' is valid inside a username but almost
		// always sentence punctuation when it's the very last character
		// (e.g. "cc @charlie." at the end of a message), so trim it.
		name = strings.TrimRight(name, ".-")
		if name == "" || isSpecialMention(name) {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}

	return result
}

func isSpecialMention(name string) bool {
	switch strings.ToLower(name) {
	case "all", "channel", "here":
		return true
	default:
		return false
	}
}

func defaultConfiguration() Configuration {
	return Configuration{
		Enabled:             false,
		Provider:            providerGeneric,
		CSVPath:             "data.csv",
		TimeoutSeconds:      defaultTimeoutSeconds,
		ExpandGroupMentions: true,
	}
}

func normalizeProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == providerKavenegar {
		return providerKavenegar
	}
	return providerGeneric
}

func validateConfiguration(config Configuration) error {
	if !config.Enabled {
		return nil
	}

	switch config.Provider {
	case providerKavenegar:
		if strings.TrimSpace(config.KavenegarAPIKey) == "" {
			return errors.New("kavenegar_api_key is required when provider is kavenegar")
		}
	default:
		if err := validateNotifyURL(config.NotifyURL); err != nil {
			return err
		}
	}

	if config.TimeoutSeconds < 1 || config.TimeoutSeconds > maxTimeoutSeconds {
		return fmt.Errorf("timeout_seconds must be between 1 and %d", maxTimeoutSeconds)
	}

	return nil
}

func validateNotifyURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return errors.New("notify_url is required when the plugin is enabled")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid notify_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("notify_url must use http or https")
	}
	if u.Host == "" {
		return errors.New("notify_url must include a host")
	}

	return nil
}

func normalizeIDList(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func main() {
	plugin.ClientMain(&Plugin{})
}