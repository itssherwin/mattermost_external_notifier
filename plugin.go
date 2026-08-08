package main

import (
	"bytes"
	"context"
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
	defaultCSVFile     = "data.csv"
	defaultTimeout     = 10 * time.Second
	maxCSVRecordFields = 2
)

// StringList accepts either a comma-separated string (the System Console
// representation) or a JSON string array (useful for direct configuration).
type StringList []string

func (s *StringList) UnmarshalJSON(data []byte) error {
	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		*s = normalizeIDList(list)
		return nil
	}

	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("expected string or string array: %w", err)
	}

	*s = normalizeIDList(strings.Split(value, ","))
	return nil
}

func normalizeIDList(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

type Plugin struct {
	plugin.MattermostPlugin

	mu     sync.RWMutex
	table  map[string]string
	config Configuration
	client *http.Client
	wg     sync.WaitGroup
}

type Configuration struct {
	Enabled        bool       `json:"enabled"`
	TeamIDs        StringList `json:"team_ids"`
	ChannelIDs     StringList `json:"channel_ids"`
	NotifyURL      string     `json:"notify_url"`
	CSVFile        string     `json:"csv_file"`
	TimeoutSeconds int        `json:"timeout_seconds"`
	AuthToken      string     `json:"auth_token"`
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
	p.client = &http.Client{Timeout: defaultTimeout}

	if err := p.OnConfigurationChange(); err != nil {
		return err
	}

	return nil
}

func (p *Plugin) OnConfigurationChange() error {
	config := Configuration{}
	if err := p.API.LoadPluginConfiguration(&config); err != nil {
		return fmt.Errorf("load plugin configuration: %w", err)
	}

	applyEnvironmentDefaults(&config)

	if err := validateConfiguration(config); err != nil {
		return err
	}

	timeout := requestTimeout(config)

	p.mu.Lock()
	p.config = config
	p.client = &http.Client{Timeout: timeout}
	p.mu.Unlock()

	if err := p.loadCSV(config.CSVFile); err != nil {
		return fmt.Errorf("load CSV mapping: %w", err)
	}

	p.API.LogInfo(
		"mention notifier configuration loaded",
		"enabled", config.Enabled,
		"teams", len(config.TeamIDs),
		"channels", len(config.ChannelIDs),
	)

	return nil
}

func (p *Plugin) OnDeactivate() {
	p.wg.Wait()
}

func (p *Plugin) MessageHasBeenPosted(
	_ *plugin.Context,
	post *model.Post,
) {
	config := p.getConfiguration()

	if !config.Enabled {
		return
	}

	if !p.isMonitoredScope(post.TeamId, post.ChannelId, config.TeamIDs, config.ChannelIDs) {
		return
	}

	mentions := extractMentions(post.Message)
	if len(mentions) == 0 {
		return
	}

	seen := make(map[string]struct{}, len(mentions))
	for _, name := range mentions {
		name = normalizeUsername(name)
		if name == "" {
			continue
		}

		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		number := p.lookup(name)
		if number == "" {
			p.API.LogDebug("mentioned user not found in CSV", "username", name)
			continue
		}

		p.wg.Add(1)
		go func(name, number string, post *model.Post, config Configuration) {
			defer p.wg.Done()
			p.notify(config, name, number, post)
		}(name, number, post, config)
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
	teamMatch := len(teamIDs) == 0 || contains(teamIDs, teamID)
	channelMatch := len(channelIDs) == 0 || contains(channelIDs, channelID)

	// Empty means "all". If both filters are configured, both must match.
	return teamMatch && channelMatch
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == target {
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

func (p *Plugin) loadCSV(filename string) error {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		filename = defaultCSVFile
	}

	path := filename
	if !filepath.IsAbs(path) {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		path = filepath.Join(filepath.Dir(exe), path)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1

	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	if len(records) == 0 {
		return fmt.Errorf("%q is empty", path)
	}

	table := make(map[string]string, len(records)-1)
	start := 0
	if isCSVHeader(records[0]) {
		start = 1
	}

	for i := start; i < len(records); i++ {
		row := records[i]
		if len(row) < maxCSVRecordFields {
			p.API.LogWarn("skipping CSV row with fewer than two fields", "row", i+1)
			continue
		}

		name := normalizeUsername(row[0])
		number := strings.TrimSpace(row[1])
		if name == "" || number == "" {
			continue
		}

		table[name] = number
	}

	if len(table) == 0 {
		return fmt.Errorf("%q contains no usable mappings", path)
	}

	p.mu.Lock()
	p.table = table
	p.mu.Unlock()

	return nil
}

func isCSVHeader(row []string) bool {
	if len(row) < 2 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(row[0]), "mention") &&
		strings.EqualFold(strings.TrimSpace(row[1]), "number")
}

func (p *Plugin) notify(
	config Configuration,
	name string,
	number string,
	post *model.Post,
) {
	baseURL, err := validatedNotifyURL(config.NotifyURL)
	if err != nil {
		p.API.LogError("invalid notification URL", "error", err.Error())
		return
	}

	payload := Notification{
		Name:      name,
		Number:    number,
		PostID:    post.Id,
		UserID:    post.UserId,
		TeamID:    post.TeamId,
		ChannelID: post.ChannelId,
		Message:   post.Message,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		p.API.LogError("failed to encode notification", "error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout(config))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(body))
	if err != nil {
		p.API.LogError("failed to create notification request", "error", err.Error())
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mattermost-mention-notifier/1")

	if token := strings.TrimSpace(config.AuthToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	p.mu.RLock()
	client := p.client
	p.mu.RUnlock()
	if client == nil {
		client = &http.Client{Timeout: requestTimeout(config)}
	}

	resp, err := client.Do(req)
	if err != nil {
		p.API.LogError("notification request failed", "name", name, "error", err.Error())
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
		return
	}

	p.API.LogDebug("notification sent", "name", name, "status", resp.Status)
}

func requestTimeout(config Configuration) time.Duration {
	if config.TimeoutSeconds > 0 {
		return time.Duration(config.TimeoutSeconds) * time.Second
	}
	return defaultTimeout
}

func validatedNotifyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("notify_url is required")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("URL scheme must be http or https")
	}
	if u.Host == "" {
		return "", errors.New("URL host is required")
	}
	return u.String(), nil
}

func validateConfiguration(config Configuration) error {
	if config.TimeoutSeconds < 0 {
		return errors.New("timeout_seconds cannot be negative")
	}

	if config.Enabled {
		if _, err := validatedNotifyURL(config.NotifyURL); err != nil {
			return fmt.Errorf("invalid notify_url: %w", err)
		}
	}
	return nil
}

func applyEnvironmentDefaults(config *Configuration) {
	if strings.TrimSpace(config.NotifyURL) == "" {
		config.NotifyURL = strings.TrimSpace(os.Getenv("MENTION_NOTIFIER_NOTIFY_URL"))
	}
	if strings.TrimSpace(config.CSVFile) == "" {
		if value := strings.TrimSpace(os.Getenv("MENTION_NOTIFIER_CSV_FILE")); value != "" {
			config.CSVFile = value
		}
	}
	if config.TimeoutSeconds == 0 {
		if value := strings.TrimSpace(os.Getenv("MENTION_NOTIFIER_TIMEOUT_SECONDS")); value != "" {
			if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
				config.TimeoutSeconds = seconds
			}
		}
	}
	if strings.TrimSpace(config.AuthToken) == "" {
		config.AuthToken = strings.TrimSpace(os.Getenv("MENTION_NOTIFIER_AUTH_TOKEN"))
	}
}

var mentionRegexp = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])@([A-Za-z0-9_.-]+)`)

func extractMentions(message string) []string {
	matches := mentionRegexp.FindAllStringSubmatch(message, -1)
	result := make([]string, 0, len(matches))

	for _, match := range matches {
		if len(match) > 1 {
			name := normalizeUsername(match[1])
			switch name {
			case "", "all", "channel", "here":
				continue
			default:
				result = append(result, name)
			}
		}
	}

	return result
}

func normalizeUsername(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	return strings.TrimPrefix(name, "@")
}

func main() {
	plugin.ClientMain(&Plugin{})
}
