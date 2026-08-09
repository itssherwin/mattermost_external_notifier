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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

const (
	defaultTimeoutSeconds = 10
	maxTimeoutSeconds     = 120
)

type Plugin struct {
	plugin.MattermostPlugin

	mu     sync.RWMutex
	table  map[string]string
	config Configuration

	client *http.Client
	wg     sync.WaitGroup
}

type Configuration struct {
	Enabled        bool     `json:"enabled"`
	TeamIDs        []string `json:"team_ids"`
	ChannelIDs     []string `json:"channel_ids"`
	NotifyURL      string   `json:"notify_url"`
	AuthToken      string   `json:"auth_token"`
	CSVPath        string   `json:"csv_path"`
	TimeoutSeconds int      `json:"timeout_seconds"`
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

func (p *Plugin) OnActivate() error {
	p.client = &http.Client{
		Timeout: defaultTimeoutSeconds * time.Second,
	}
	p.config = defaultConfiguration()

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

	p.client = &http.Client{Timeout: timeout}

	p.API.LogInfo(
		"mention notifier configuration loaded",
		"teams", len(config.TeamIDs),
		"channels", len(config.ChannelIDs),
		"csv_entries", len(table),
		"timeout_seconds", config.TimeoutSeconds,
	)

	return nil
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

	for _, name := range mentions {
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
		go func(
			name string,
			number string,
			teamID string,
			post *model.Post,
			config Configuration,
		) {
			defer p.wg.Done()
			p.notify(config, name, number, teamID, post)
		}(name, number, teamID, post, config)
	}
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
	teamMatch := matchesConfiguredID(teamID, teamIDs)
	channelMatch := matchesConfiguredID(channelID, channelIDs)

	return teamMatch && channelMatch
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

	u, err := url.Parse(strings.TrimSpace(config.NotifyURL))
	if err != nil {
		p.API.LogError(
			"invalid notification URL",
			"error", err.Error(),
		)
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
		p.API.LogError(
			"failed to encode notification",
			"error", err.Error(),
		)
		return
	}

	req, err := http.NewRequest(
		http.MethodPost,
		u.String(),
		strings.NewReader(string(body)),
	)
	if err != nil {
		p.API.LogError(
			"failed to create HTTP request",
			"error", err.Error(),
		)
		return
	}

	req.Header.Set("Content-Type", "application/json")

	if token := strings.TrimSpace(config.AuthToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := p.client
	if client == nil {
		timeout := time.Duration(config.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = defaultTimeoutSeconds * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		p.API.LogError(
			"notification request failed",
			"name", name,
			"number", number,
			"error", err.Error(),
		)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		p.API.LogError(
			"notification returned non-2xx",
			"name", name,
			"number", number,
			"status", resp.Status,
		)
	}
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
		Enabled:        false,
		CSVPath:        "data.csv",
		TimeoutSeconds: defaultTimeoutSeconds,
	}
}

func validateConfiguration(config Configuration) error {
	if !config.Enabled {
		return nil
	}

	if err := validateNotifyURL(config.NotifyURL); err != nil {
		return err
	}

	if config.TimeoutSeconds < 1 || config.TimeoutSeconds > maxTimeoutSeconds {
		return fmt.Errorf(
			"timeout_seconds must be between 1 and %d",
			maxTimeoutSeconds,
		)
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

func parseTimeout(value string) int {
	timeout, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || timeout <= 0 {
		return defaultTimeoutSeconds
	}

	return timeout
}

func main() {
	plugin.ClientMain(&Plugin{})
}
