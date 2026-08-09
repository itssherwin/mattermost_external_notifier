package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestExtractMentions(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{
			name:    "single mention",
			message: "hello @alice",
			want:    []string{"alice"},
		},
		{
			name:    "multiple mentions",
			message: "hello @alice and @bob",
			want:    []string{"alice", "bob"},
		},
		{
			name:    "case normalized",
			message: "hello @Alice @BOB",
			want:    []string{"alice", "bob"},
		},
		{
			name:    "duplicates removed",
			message: "@alice hello @Alice @alice",
			want:    []string{"alice"},
		},
		{
			name:    "special mentions ignored",
			message: "@all @channel @here @alice",
			want:    []string{"alice"},
		},
		{
			name:    "email is not a mention",
			message: "mail alice@example.com and mention @bob",
			want:    []string{"bob"},
		},
		{
			name:    "punctuation",
			message: "(@alice), [@bob], and @charlie.",
			want:    []string{"alice", "bob", "charlie"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMentions(tt.message)

			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("extractMentions() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMatchesConfiguredID(t *testing.T) {
	if !matchesConfiguredID("team1", nil) {
		t.Fatal("empty configuration should match all")
	}

	if !matchesConfiguredID("team1", []string{"team1", "team2"}) {
		t.Fatal("configured matching ID should match")
	}

	if matchesConfiguredID("team3", []string{"team1", "team2"}) {
		t.Fatal("unconfigured ID should not match")
	}
}

func TestIsMonitoredScope(t *testing.T) {
	tests := []struct {
		name      string
		teamID    string
		channelID string
		teams     []string
		channels  []string
		want      bool
	}{
		{
			name:      "all teams and channels",
			teamID:    "team1",
			channelID: "channel1",
			want:      true,
		},
		{
			name:      "matching team",
			teamID:    "team1",
			channelID: "channel1",
			teams:     []string{"team1"},
			want:      true,
		},
		{
			name:      "matching channel",
			teamID:    "team1",
			channelID: "channel1",
			channels:  []string{"channel1"},
			want:      true,
		},
		{
			name:      "both match",
			teamID:    "team1",
			channelID: "channel1",
			teams:     []string{"team1"},
			channels:  []string{"channel1"},
			want:      true,
		},
		{
			name:      "team mismatch",
			teamID:    "team2",
			channelID: "channel1",
			teams:     []string{"team1"},
			channels:  []string{"channel1"},
			want:      false,
		},
		{
			name:      "channel mismatch",
			teamID:    "team1",
			channelID: "channel2",
			teams:     []string{"team1"},
			channels:  []string{"channel1"},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (&Plugin{}).isMonitoredScope(
				tt.teamID,
				tt.channelID,
				tt.teams,
				tt.channels,
			)

			if got != tt.want {
				t.Fatalf("isMonitoredScope() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeUsername(t *testing.T) {
	tests := map[string]string{
		" Alice ": "alice",
		"@Alice":  "alice",
		"BOB":     "bob",
	}

	for input, want := range tests {
		if got := normalizeUsername(input); got != want {
			t.Fatalf("normalizeUsername(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidateNotifyURL(t *testing.T) {
	valid := []string{
		"https://example.com/notify",
		"http://localhost:8080/notify",
	}

	for _, value := range valid {
		if err := validateNotifyURL(value); err != nil {
			t.Fatalf("validateNotifyURL(%q) returned error: %v", value, err)
		}
	}

	invalid := []string{
		"",
		"example.com/notify",
		"ftp://example.com/notify",
	}

	for _, value := range invalid {
		if err := validateNotifyURL(value); err == nil {
			t.Fatalf("validateNotifyURL(%q) expected error", value)
		}
	}
}

func TestLoadCSV(t *testing.T) {
	dir := t.TempDir()
	filename := dir + "/data.csv"

	content := "username,number\nAlice,12345\n@Bob,67890\n"
	if err := writeFile(filename, content); err != nil {
		t.Fatal(err)
	}

	table, err := loadCSV(filename)
	if err != nil {
		t.Fatal(err)
	}

	if got := table["alice"]; got != "12345" {
		t.Fatalf("alice = %q, want %q", got, "12345")
	}

	if got := table["bob"]; got != "67890" {
		t.Fatalf("bob = %q, want %q", got, "67890")
	}
}

func TestNotify(t *testing.T) {
	var gotAuth string
	var gotBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}

		gotBody = string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	p := &Plugin{
		client: &http.Client{Timeout: time.Second},
	}

	config := Configuration{
		NotifyURL:      server.URL,
		TimeoutSeconds: 1,
		AuthToken:      "secret",
	}

	post := &model.Post{
		Id:        "post1",
		UserId:    "author1",
		ChannelId: "channel1",
		Message:   "hello @alice",
	}

	p.notify(config, "alice", "0901234567", "team1", post)

	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer secret")
	}

	for _, want := range []string{
		`"name":"alice"`,
		`"number":"0901234567"`,
		`"post_id":"post1"`,
		`"user_id":"author1"`,
		`"team_id":"team1"`,
		`"channel_id":"channel1"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("request body %q does not contain %q", gotBody, want)
		}
	}
}

func writeFile(filename, content string) error {
	return os.WriteFile(filename, []byte(content), 0600)
}
