package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	providerGeneric   = "generic"
	providerKavenegar = "kavenegar"

	defaultKavenegarAPIBase = "https://api.kavenegar.com/v1"

	envNotifierEnabled      = "MENTION_NOTIFIER_ENABLED"
	envNotifierTeamIDs      = "MENTION_NOTIFIER_TEAM_IDS"
	envNotifierChannelIDs   = "MENTION_NOTIFIER_CHANNEL_IDS"
	envNotifierProvider     = "MENTION_NOTIFIER_PROVIDER"
	envNotifierNotifyURL    = "MENTION_NOTIFIER_NOTIFY_URL"
	envNotifierAuthToken    = "MENTION_NOTIFIER_AUTH_TOKEN"
	envNotifierCSVFile      = "MENTION_NOTIFIER_CSV_FILE"
	envNotifierTimeout      = "MENTION_NOTIFIER_TIMEOUT_SECONDS"
	envNotifierExpandGroups = "MENTION_NOTIFIER_EXPAND_GROUPS"
	envNotifierDebugLogging = "MENTION_NOTIFIER_DEBUG_LOGGING"
	envNotifierDebugLogPath = "MENTION_NOTIFIER_DEBUG_LOG_PATH"
	envNotifierText          = "MENTION_NOTIFIER_TEXT"
	envNotifierSiteURL       = "MENTION_NOTIFIER_SITE_URL"

	envKavenegarAPIKey       = "KAVENEGAR_API_KEY"
	envKavenegarAPIURL       = "KAVENEGAR_API_URL"
	envKavenegarSender       = "KAVENEGAR_SENDER"
	envKavenegarTemplate     = "KAVENEGAR_TEMPLATE"
	envKavenegarTemplateName = "KAVENEGAR_TEMPLATE_NAME"
)

type Plugin struct {
	plugin.MattermostPlugin

	mu       sync.RWMutex
	table    map[string]string
	config   Configuration
	client   *http.Client
	debugLog *fileLogger

	wg sync.WaitGroup
}

type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*s = arr
		return nil
	}

	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}

	*s = parseEnvIDList(single)
	return nil
}

type Configuration struct {
	Enabled bool `json:"Enabled"`

	TeamIDs    stringList `json:"TeamIDs"`
	ChannelIDs stringList `json:"ChannelIDs"`

	// Provider selects how a notification is delivered.
	// "generic"   -> JSON POST to NotifyURL
	// "kavenegar" -> KavehNegar SMS REST API
	Provider string `json:"Provider"`

	// Generic webhook provider settings.
	NotifyURL string `json:"NotifyURL"`
	AuthToken string `json:"AuthToken"`

	// KavehNegar provider settings.
	KavenegarAPIKey   string `json:"KavenegarAPIKey"`
	KavenegarAPIURL   string `json:"KavenegarAPIURL"`
	KavenegarSender   string `json:"KavenegarSender"`
	KavenegarTemplate string `json:"KavenegarTemplate"`

	// KavenegarTemplateName, when set, switches delivery from plain
	// sms/send.json to KavehNegar's verify/lookup.json endpoint using
	// this pre-approved template name. Faster and higher priority than a
	// plain SMS. When set, KavenegarSender / KavenegarTemplate / Text are
	// ignored for this recipient, since the message text lives in the
	// KavehNegar-side template instead.
	KavenegarTemplateName string `json:"KavenegarTemplateName"`

	// MENTION_NOTIFIER_TEXT overrides the SMS text.
	//
	// Supported placeholders:
	//   {{user}}    - Mattermost user who created the post
	//   {{channel}} - Mattermost channel name
	//   {{message}} - original Mattermost post message
	Text string `json:"Text"`

	// SiteURL overrides Mattermost's own ServiceSettings.SiteURL when
	// building the post permalink sent as %token to a KavehNegar
	// verify/lookup template. Leave blank to use Mattermost's real
	// SiteURL config - this only matters if that's unset, wrong for your
	// use case, or you want the link to point somewhere else entirely.
	SiteURL string `json:"SiteURL"`

	CSVPath        string `json:"CSVPath"`
	TimeoutSeconds int    `json:"TimeoutSeconds"`

	ExpandGroupMentions bool `json:"ExpandGroupMentions"`

	DebugLogging bool   `json:"DebugLogging"`
	DebugLogPath string `json:"DebugLogPath"`
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

type kavenegarResponse struct {
	Return struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
	} `json:"return"`
}

type fileLogger struct {
	mu sync.Mutex
	f  *os.File
}

func openFileLogger(path string) (*fileLogger, error) {
	if path == "" {
		return nil, errors.New("empty log path")
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	f, err := os.OpenFile(
		path,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY,
		0o644,
	)
	if err != nil {
		return nil, err
	}

	return &fileLogger{f: f}, nil
}

func (l *fileLogger) Printf(format string, args ...interface{}) {
	if l == nil || l.f == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	_, _ = fmt.Fprintf(
		l.f,
		"%s "+format+"\n",
		append(
			[]interface{}{time.Now().UTC().Format(time.RFC3339)},
			args...,
		)...,
	)
}

func (l *fileLogger) Close() error {
	if l == nil || l.f == nil {
		return nil
	}

	return l.f.Close()
}

func (p *Plugin) OnActivate() error {
	loadDotEnvFile(defaultDotEnvPath())

	p.setClient(&http.Client{
		Timeout: defaultTimeoutSeconds * time.Second,
	})

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
	p.setDebugLog(nil)
	return nil
}

func (p *Plugin) OnConfigurationChange() error {
	// Re-read .env every time configuration is reloaded.
	loadDotEnvFile(defaultDotEnvPath())
	return p.reloadConfiguration()
}

func defaultDebugLogPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "mention-notifier-debug.log"
	}

	return filepath.Join(
		filepath.Dir(exe),
		"mention-notifier-debug.log",
	)
}

func defaultDotEnvPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}

	return filepath.Join(filepath.Dir(exe), ".env")
}


func loadDotEnvFile(path string) {
	if path == "" {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}

		// Explicit process environment variables always win.
		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}

		value := strings.TrimSpace(parts[1])

		// Remove matching surrounding quotes.
		if len(value) >= 2 {
			first := value[0]
			last := value[len(value)-1]

			if (first == '\'' && last == '\'') ||
				(first == '"' && last == '"') {
				value = value[1 : len(value)-1]
			}
		}

		// Support escaped newlines in .env values.
		value = strings.ReplaceAll(value, `\n`, "\n")
		value = strings.ReplaceAll(value, `\r`, "\r")
		value = strings.ReplaceAll(value, `\t`, "\t")

		os.Setenv(key, value)
	}
}



func (p *Plugin) reloadConfiguration() error {
	config := defaultConfiguration()

	if err := p.API.LoadPluginConfiguration(&config); err != nil {
		return fmt.Errorf("load plugin configuration: %w", err)
	}

	config.TeamIDs = normalizeIDList(config.TeamIDs)
	config.ChannelIDs = normalizeIDList(config.ChannelIDs)
	config.Provider = normalizeProvider(config.Provider)

	// Environment values override Mattermost System Console values.
	applyEnvOverrides(&config)

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

	p.setClient(&http.Client{
		Timeout: timeout,
	})

	if config.DebugLogging {
		path := strings.TrimSpace(config.DebugLogPath)

		if path == "" {
			path = defaultDebugLogPath()
		}

		logger, err := openFileLogger(path)
		if err != nil {
			p.API.LogError(
				"failed to open debug log file",
				"path", path,
				"error", err.Error(),
			)
			p.setDebugLog(nil)
		} else {
			p.setDebugLog(logger)

			p.API.LogInfo(
				"debug logging enabled",
				"path", path,
			)
		}
	} else {
		p.setDebugLog(nil)
	}

	p.API.LogInfo(
		"mention notifier configuration loaded",
		"provider", config.Provider,
		"teams", len(config.TeamIDs),
		"channels", len(config.ChannelIDs),
		"csv_entries", len(table),
		"timeout_seconds", config.TimeoutSeconds,
		"debug_logging", config.DebugLogging,
	)

	// Never log secrets.
	p.debugf(
		"CONFIG provider=%s enabled=%t teams=%v channels=%v csv_entries=%d timeout=%d",
		config.Provider,
		config.Enabled,
		config.TeamIDs,
		config.ChannelIDs,
		len(table),
		config.TimeoutSeconds,
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

func (p *Plugin) setDebugLog(l *fileLogger) {
	p.mu.Lock()

	old := p.debugLog
	p.debugLog = l

	p.mu.Unlock()

	if old != nil {
		if err := old.Close(); err != nil {
			p.API.LogError(
				"failed to close debug log file",
				"error", err.Error(),
			)
		}
	}
}

func (p *Plugin) getDebugLog() *fileLogger {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.debugLog
}

func (p *Plugin) debugf(format string, args ...interface{}) {
	if l := p.getDebugLog(); l != nil {
		l.Printf(format, args...)
	}
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

	p.debugf(
		"TAGGED post_id=%s channel_id=%s team_id=%s mentions=%v message=%q",
		post.Id,
		post.ChannelId,
		teamID,
		mentions,
		post.Message,
	)

	seen := make(map[string]struct{}, len(mentions))

	for _, token := range mentions {
		targets := []string{token}

		if config.ExpandGroupMentions {
			if members, isGroup := p.resolveGroupMembers(token); isGroup {
				targets = members

				p.debugf(
					"GROUP token=%s members=%v",
					token,
					members,
				)
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

				p.debugf(
					"EXTRACTED user=%s from_token=%s number=<no csv mapping>",
					name,
					token,
				)

				continue
			}

			p.debugf(
				"EXTRACTED user=%s from_token=%s number=%s",
				name,
				token,
				number,
			)

			p.wg.Add(1)

			go func(
				name,
				number,
				teamID string,
				post *model.Post,
				config Configuration,
			) {
				defer p.wg.Done()

				p.notify(
					config,
					name,
					number,
					teamID,
					post,
				)
			}(
				name,
				number,
				teamID,
				post,
				config,
			)
		}
	}
}

func (p *Plugin) resolveGroupMembers(
	token string,
) ([]string, bool) {
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

		return nil, true
	}

	return usernames, true
}

func (p *Plugin) groupMemberUsernames(
	groupID string,
) ([]string, error) {
	const perPage = 200

	var usernames []string

	for page := 0; ; page++ {
		users, appErr := p.API.GetGroupMemberUsers(
			groupID,
			page,
			perPage,
		)

		if appErr != nil {
			return nil, errors.New(appErr.Error())
		}

		for _, u := range users {
			if u != nil && u.Username != "" {
				usernames = append(
					usernames,
					strings.ToLower(u.Username),
				)
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

func matchesConfiguredID(
	value string,
	configured []string,
) bool {
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
		filename = "data.csv"
	}

	if !filepath.IsAbs(filename) {
		if exe, err := os.Executable(); err == nil {
			filename = filepath.Join(
				filepath.Dir(exe),
				filename,
			)
		}
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

	return (name == "username" ||
		name == "user" ||
		name == "name") &&
		(number == "number" ||
			number == "phone" ||
			number == "phone_number")
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

	p.debugf(
		"NOTIFY provider=%s user=%s number=%s post_id=%s channel_id=%s",
		config.Provider,
		name,
		number,
		post.Id,
		post.ChannelId,
	)

	switch config.Provider {
	case providerKavenegar:
		if strings.TrimSpace(config.KavenegarTemplateName) != "" {
			p.notifyKavenegarTemplate(
				config,
				name,
				number,
				teamID,
				post,
			)
		} else {
			p.notifyKavenegar(
				config,
				name,
				number,
				post,
			)
		}

	default:
		p.notifyGeneric(
			config,
			name,
			number,
			teamID,
			post,
		)
	}
}

func (p *Plugin) notifyGeneric(
	config Configuration,
	name string,
	number string,
	teamID string,
	post *model.Post,
) {
	u, err := url.Parse(
		strings.TrimSpace(config.NotifyURL),
	)

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

	req.Header.Set(
		"Content-Type",
		"application/json",
	)

	if token := strings.TrimSpace(config.AuthToken); token != "" {
		req.Header.Set(
			"Authorization",
			"Bearer "+token,
		)
	}

	p.debugf(
		"GENERIC REQUEST user=%s number=%s url=%s",
		name,
		number,
		u.String(),
	)

	resp, err := p.clientFor(config).Do(req)
	if err != nil {
		p.API.LogError(
			"notification request failed",
			"name", name,
			"error", err.Error(),
		)

		p.debugf(
			"GENERIC RESPONSE user=%s number=%s error=%s",
			name,
			number,
			err.Error(),
		)

		return
	}

	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(resp.Body)

	p.debugf(
		"GENERIC RESPONSE user=%s number=%s http_status=%d body=%q read_error=%v",
		name,
		number,
		resp.StatusCode,
		string(responseBody),
		readErr,
	)

	if resp.StatusCode < http.StatusOK ||
		resp.StatusCode >= http.StatusMultipleChoices {

		p.API.LogError(
			"notification returned non-2xx",
			"name", name,
			"status", resp.Status,
		)
	}
}

func (p *Plugin) notifyKavenegar(
	config Configuration,
	name string,
	number string,
	post *model.Post,
) {
	apiKey := strings.TrimSpace(config.KavenegarAPIKey)

	if apiKey == "" {
		p.API.LogError(
			"kavenegar_api_key is not configured",
		)

		p.debugf(
			"KAVENEGAR ERROR user=%s reason=no_api_key",
			name,
		)

		return
	}

	receptor := normalizeIranianNumber(number)

	if receptor == "" {
		p.API.LogError(
			"no usable phone number for recipient",
			"name", name,
		)

		p.debugf(
			"KAVENEGAR ERROR user=%s reason=no_receptor",
			name,
		)

		return
	}

	// Resolve the Mattermost user who created the post, and the channel
	// they posted in.
	senderName, channelName := p.resolveSenderAndChannel(post)

	// Build the SMS text from MENTION_NOTIFIER_TEXT.
	//
	// Supported:
	//   {{user}}
	//   {{channel}}
	//   {{message}}
	message := strings.TrimSpace(config.Text)

	if message == "" {
		message = "{{user}} شما را در {{channel}} تگ کرد"
	}

	message = strings.ReplaceAll(
		message,
		"{{user}}",
		senderName,
	)

	message = strings.ReplaceAll(
		message,
		"{{channel}}",
		channelName,
	)

	message = strings.ReplaceAll(
		message,
		"{{message}}",
		post.Message,
	)

	endpoint := kavenegarEndpoint(
		config,
		apiKey,
		"sms/send.json",
	)

	q := url.Values{}

	q.Set("receptor", receptor)
	q.Set("message", message)

	// Sender is optional.
	// Do not send it unless explicitly configured.
	sender := strings.TrimSpace(config.KavenegarSender)

	if sender != "" {
		q.Set("sender", sender)
	}

	p.debugf(
		"KAVENEGAR REQUEST user=%s receptor=%s sender=%q sms=%q channel=%s sender_user=%s",
		name,
		receptor,
		sender,
		message,
		channelName,
		senderName,
	)

	req, err := http.NewRequest(
		http.MethodGet,
		endpoint+"?"+q.Encode(),
		nil,
	)
	if err != nil {
		p.API.LogError(
			"failed to create KavehNegar request",
			"error", err.Error(),
		)

		p.debugf(
			"KAVENEGAR ERROR user=%s reason=create_request error=%s",
			name,
			err.Error(),
		)

		return
	}

	req.Header.Set("Accept", "application/json")

	resp, err := p.clientFor(config).Do(req)
	if err != nil {
		p.API.LogError(
			"kavenegar request failed",
			"name", name,
			"error", err.Error(),
		)

		p.debugf(
			"KAVENEGAR ERROR user=%s number=%s reason=http_request error=%s",
			name,
			number,
			err.Error(),
		)

		return
	}

	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		p.API.LogError(
			"failed to read kavenegar response",
			"name", name,
			"error", err.Error(),
		)

		p.debugf(
			"KAVENEGAR ERROR user=%s reason=read_response error=%s",
			name,
			err.Error(),
		)

		return
	}

	p.debugf(
		"KAVENEGAR RESPONSE user=%s number=%s http_status=%d content_type=%q body=%q",
		name,
		number,
		resp.StatusCode,
		resp.Header.Get("Content-Type"),
		string(responseBody),
	)

	var parsed kavenegarResponse

	decodeErr := json.Unmarshal(
		responseBody,
		&parsed,
	)

	if resp.StatusCode < http.StatusOK ||
		resp.StatusCode >= http.StatusMultipleChoices {

		if decodeErr == nil {
			p.API.LogError(
				"kavenegar returned non-2xx",
				"name", name,
				"status", resp.Status,
				"api_status", parsed.Return.Status,
				"api_message", parsed.Return.Message,
				"hint", kavenegarErrorHint(parsed.Return.Status),
			)
		} else {
			p.API.LogError(
				"kavenegar returned non-2xx",
				"name", name,
				"status", resp.Status,
				"response", string(responseBody),
			)
		}

		return
	}

	if decodeErr != nil {
		p.API.LogError(
			"failed to decode kavenegar response",
			"name", name,
			"error", decodeErr.Error(),
			"response", string(responseBody),
		)

		return
	}

	if parsed.Return.Status != http.StatusOK {
		p.API.LogError(
			"kavenegar rejected the message",
			"name", name,
			"status", parsed.Return.Status,
			"message", parsed.Return.Message,
			"hint", kavenegarErrorHint(parsed.Return.Status),
		)

		return
	}

	p.API.LogInfo(
		"kavenegar SMS accepted",
		"name", name,
		"sender_user", senderName,
		"channel", channelName,
		"status", parsed.Return.Status,
	)
}

// resolveSenderAndChannel resolves the display name of whoever created
// post, and the display name of the channel it was posted in. Falls back
// to raw IDs if either lookup fails, so callers always get a usable
// (if less friendly) string instead of an error.
func (p *Plugin) resolveSenderAndChannel(post *model.Post) (senderName string, channelName string) {
	senderName = post.UserId

	if user, appErr := p.API.GetUser(post.UserId); appErr == nil && user != nil {
		senderName = user.GetDisplayName(model.ShowNicknameFullName)

		if strings.TrimSpace(senderName) == "" {
			senderName = user.Username
		}
	}

	channelName = post.ChannelId

	if channel, appErr := p.API.GetChannel(post.ChannelId); appErr == nil && channel != nil {
		channelName = channel.DisplayName

		if strings.TrimSpace(channelName) == "" {
			channelName = channel.Name
		}
	}

	return senderName, channelName
}

// postPermalink builds a Mattermost permalink to a post
// ({SiteURL}/{team-name}/pl/{post-id}), used as the %token value sent to
// KavehNegar's verify/lookup template. config.SiteURL (if set) takes
// priority over Mattermost's own ServiceSettings.SiteURL. Falls back to a
// bare post ID if no SiteURL is available or the team can't be resolved,
// so the notification still carries something usable instead of failing
// outright.
func (p *Plugin) postPermalink(config Configuration, teamID string, postID string) string {
	siteURL := strings.TrimRight(strings.TrimSpace(config.SiteURL), "/")

	if siteURL == "" {
		if cfg := p.API.GetConfig(); cfg != nil && cfg.ServiceSettings.SiteURL != nil {
			siteURL = strings.TrimRight(*cfg.ServiceSettings.SiteURL, "/")
		}
	}

	teamName := ""
	if team, appErr := p.API.GetTeam(teamID); appErr == nil && team != nil {
		teamName = team.Name
	}

	if siteURL == "" || teamName == "" {
		return postID
	}

	return fmt.Sprintf("%s/%s/pl/%s", siteURL, teamName, postID)
}

// kavenegarErrorHint gives a short human-readable explanation for
// KavehNegar's documented verify/lookup error codes, purely to make debug
// logs easier to read at a glance. See https://kavenegar.com/rest.html
func kavenegarErrorHint(status int) string {
	switch status {
	case 418:
		return "insufficient account credit"
	case 422:
		return "receptor or token contains an invalid character"
	case 424:
		return "template not found or not yet approved"
	case 426:
		return "requires the advanced service to be enabled"
	case 428:
		return "call delivery requires a numeric-only token"
	case 431:
		return "token contains a newline, space, underscore, or separator"
	case 432:
		return "template does not define the token parameter used"
	case 501:
		return "account restricted to sending only to its own registered number - complete KavehNegar account verification to lift this"
	case 607:
		return "invalid tag name"
	default:
		return ""
	}
}

// notifyKavenegarTemplate sends a notification via KavehNegar's
// verify/lookup.json endpoint using a pre-approved template (configured
// in the KavehNegar panel), instead of a freeform SMS body. Faster and
// higher priority than a plain SMS, but every token placeholder used by
// the template must be supplied as a query parameter here.
//
// GET https://api.kavenegar.com/v1/{API-KEY}/verify/lookup.json?receptor=...&token=...&token10=...&token20=...&template=...
// Docs: https://kavenegar.com/rest.html (Verify / Lookup)
//
// Written for a template shaped like:
//
//	%token10 شما را در گروه %token20 منشن کرده است. لینک پیام: %token
//
// where:
//   - token   -> permalink URL to the post (KavehNegar disallows spaces
//     in this field, which a URL naturally satisfies)
//   - token10 -> the mentioning user's display name (up to 5 spaces
//     allowed)
//   - token20 -> the channel's display name (up to 8 spaces allowed)
//
// Adjust which fields are populated if your template uses
// token/token2/token3 differently.
func (p *Plugin) notifyKavenegarTemplate(
	config Configuration,
	name string,
	number string,
	teamID string,
	post *model.Post,
) {
	apiKey := strings.TrimSpace(config.KavenegarAPIKey)
	if apiKey == "" {
		p.API.LogError("kavenegar_api_key is not configured")
		p.debugf("KAVENEGAR VERIFY ERROR user=%s reason=no_api_key", name)
		return
	}

	templateName := strings.TrimSpace(config.KavenegarTemplateName)
	if templateName == "" {
		p.API.LogError("kavenegar_template_name is not configured")
		p.debugf("KAVENEGAR VERIFY ERROR user=%s reason=no_template_name", name)
		return
	}

	receptor := normalizeIranianNumber(number)
	if receptor == "" {
		p.API.LogError("no usable phone number for recipient", "name", name)
		p.debugf("KAVENEGAR VERIFY ERROR user=%s reason=no_receptor", name)
		return
	}

	senderName, channelName := p.resolveSenderAndChannel(post)
	link := p.postPermalink(config, teamID, post.Id)

	endpoint := kavenegarEndpoint(config, apiKey, "verify/lookup.json")

	q := url.Values{}
	q.Set("receptor", receptor)
	q.Set("template", templateName)
	q.Set("token", link)
	q.Set("token10", senderName)
	q.Set("token20", channelName)

	p.debugf(
		"KAVENEGAR VERIFY REQUEST user=%s receptor=%s template=%s token=%s token10=%s token20=%s",
		name, receptor, templateName, link, senderName, channelName,
	)

	req, err := http.NewRequest(http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		p.API.LogError("failed to create KavehNegar request", "error", err.Error())
		p.debugf("KAVENEGAR VERIFY ERROR user=%s reason=create_request error=%s", name, err.Error())
		return
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.clientFor(config).Do(req)
	if err != nil {
		p.API.LogError("kavenegar verify request failed", "name", name, "error", err.Error())
		p.debugf("KAVENEGAR VERIFY ERROR user=%s number=%s reason=http_request error=%s", name, number, err.Error())
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		p.API.LogError("failed to read kavenegar verify response", "name", name, "error", err.Error())
		p.debugf("KAVENEGAR VERIFY ERROR user=%s reason=read_response error=%s", name, err.Error())
		return
	}

	p.debugf(
		"KAVENEGAR VERIFY RESPONSE user=%s number=%s http_status=%d content_type=%q body=%q",
		name, number, resp.StatusCode, resp.Header.Get("Content-Type"), string(responseBody),
	)

	var parsed kavenegarResponse
	decodeErr := json.Unmarshal(responseBody, &parsed)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if decodeErr == nil {
			p.API.LogError(
				"kavenegar verify returned non-2xx",
				"name", name,
				"status", resp.Status,
				"api_status", parsed.Return.Status,
				"api_message", parsed.Return.Message,
				"hint", kavenegarErrorHint(parsed.Return.Status),
			)
		} else {
			p.API.LogError(
				"kavenegar verify returned non-2xx",
				"name", name,
				"status", resp.Status,
				"response", string(responseBody),
			)
		}
		return
	}

	if decodeErr != nil {
		p.API.LogError(
			"failed to decode kavenegar verify response",
			"name", name,
			"error", decodeErr.Error(),
			"response", string(responseBody),
		)
		return
	}

	if parsed.Return.Status != http.StatusOK {
		p.API.LogError(
			"kavenegar rejected the verify request",
			"name", name,
			"status", parsed.Return.Status,
			"hint", kavenegarErrorHint(parsed.Return.Status),
			"message", parsed.Return.Message,
		)
		return
	}

	p.API.LogInfo(
		"kavenegar verify SMS accepted",
		"name", name,
		"template", templateName,
		"sender_user", senderName,
		"channel", channelName,
		"status", parsed.Return.Status,
	)
}

func applyEnvOverrides(config *Configuration) {
	// Generic notifier settings.

	if v, ok := os.LookupEnv(envNotifierEnabled); ok {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			config.Enabled = parsed
		}
	}

	if v, ok := os.LookupEnv(envNotifierTeamIDs); ok {
		config.TeamIDs = parseEnvIDList(v)
	}

	if v, ok := os.LookupEnv(envNotifierChannelIDs); ok {
		config.ChannelIDs = parseEnvIDList(v)
	}

	if v, ok := os.LookupEnv(envNotifierProvider); ok &&
		strings.TrimSpace(v) != "" {
		config.Provider = normalizeProvider(v)
	}

	if v, ok := os.LookupEnv(envNotifierNotifyURL); ok {
		config.NotifyURL = strings.TrimSpace(v)
	}

	if v, ok := os.LookupEnv(envNotifierAuthToken); ok {
		config.AuthToken = strings.TrimSpace(v)
	}

	if v, ok := os.LookupEnv(envNotifierCSVFile); ok &&
		strings.TrimSpace(v) != "" {
		config.CSVPath = strings.TrimSpace(v)
	}

	if v, ok := os.LookupEnv(envNotifierTimeout); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			config.TimeoutSeconds = parsed
		}
	}

	if v, ok := os.LookupEnv(envNotifierExpandGroups); ok {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			config.ExpandGroupMentions = parsed
		}
	}

	if v, ok := os.LookupEnv(envNotifierDebugLogging); ok {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			config.DebugLogging = parsed
		}
	}

	if v, ok := os.LookupEnv(envNotifierDebugLogPath); ok {
		config.DebugLogPath = strings.TrimSpace(v)
	}

	// SMS text.
	//
	// This takes precedence over the System Console Text setting.
	if v, ok := os.LookupEnv(envNotifierText); ok {
		config.Text = strings.TrimSpace(v)
	}

	if v, ok := os.LookupEnv(envNotifierSiteURL); ok {
		config.SiteURL = strings.TrimSpace(v)
	}

	// KavehNegar settings.

	if v := strings.TrimSpace(os.Getenv(envKavenegarAPIKey)); v != "" {
		config.KavenegarAPIKey = v
	}

	if v := strings.TrimSpace(os.Getenv(envKavenegarAPIURL)); v != "" {
		config.KavenegarAPIURL = v
	}

	if v := strings.TrimSpace(os.Getenv(envKavenegarSender)); v != "" {
		config.KavenegarSender = v
	}

	if v := os.Getenv(envKavenegarTemplate); v != "" {
		config.KavenegarTemplate = v
	}

	if v := strings.TrimSpace(os.Getenv(envKavenegarTemplateName)); v != "" {
		config.KavenegarTemplateName = v
	}
}

func parseEnvIDList(value string) stringList {
	parts := strings.Split(value, ",")

	result := make(stringList, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)

		if part != "" {
			result = append(result, part)
		}
	}

	return result
}

func kavenegarEndpoint(
	config Configuration,
	apiKey string,
	pathSuffix string,
) string {
	base := strings.TrimSpace(
		config.KavenegarAPIURL,
	)

	if base == "" {
		base = defaultKavenegarAPIBase
	}

	base = strings.TrimRight(base, "/")

	return fmt.Sprintf(
		"%s/%s/%s",
		base,
		url.PathEscape(apiKey),
		pathSuffix,
	)
}

func renderKavenegarMessage(
	template string,
	name string,
	postMessage string,
) string {
	template = strings.TrimSpace(template)

	if template == "" {
		return fmt.Sprintf(
			"You were mentioned by @%s: %s",
			name,
			postMessage,
		)
	}

	out := strings.ReplaceAll(
		template,
		"{{user}}",
		name,
	)

	out = strings.ReplaceAll(
		out,
		"{{message}}",
		postMessage,
	)

	return out
}

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
	matches := mentionRegexp.FindAllStringSubmatch(
		message,
		-1,
	)

	result := make([]string, 0, len(matches))
	seen := make(map[string]struct{})

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}

		name := normalizeUsername(match[1])

		name = strings.TrimRight(
			name,
			".-",
		)

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
	value = strings.ToLower(
		strings.TrimSpace(value),
	)

	if value == providerKavenegar {
		return providerKavenegar
	}

	return providerGeneric
}

func validateConfiguration(
	config Configuration,
) error {
	if !config.Enabled {
		return nil
	}

	switch config.Provider {
	case providerKavenegar:
		if strings.TrimSpace(
			config.KavenegarAPIKey,
		) == "" {
			return errors.New(
				"kavenegar_api_key is required when provider is kavenegar",
			)
		}

	default:
		if err := validateNotifyURL(
			config.NotifyURL,
		); err != nil {
			return err
		}
	}

	if config.TimeoutSeconds < 1 ||
		config.TimeoutSeconds > maxTimeoutSeconds {
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
		return errors.New(
			"notify_url is required when the plugin is enabled",
		)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf(
			"invalid notify_url: %w",
			err,
		)
	}

	if u.Scheme != "http" &&
		u.Scheme != "https" {
		return errors.New(
			"notify_url must use http or https",
		)
	}

	if u.Host == "" {
		return errors.New(
			"notify_url must include a host",
		)
	}

	return nil
}

func normalizeIDList(
	values []string,
) []string {
	result := make([]string, 0, len(values))

	for _, value := range values {
		value = strings.TrimSpace(value)

		if value != "" {
			result = append(result, value)
		}
	}

	return result
}

func (p *Plugin) clientFor(
	config Configuration,
) *http.Client {
	if client := p.getClient(); client != nil {
		return client
	}

	timeout := time.Duration(
		config.TimeoutSeconds,
	) * time.Second

	if timeout <= 0 {
		timeout = defaultTimeoutSeconds * time.Second
	}

	return &http.Client{
		Timeout: timeout,
	}
}

func main() {
	plugin.ClientMain(&Plugin{})
}