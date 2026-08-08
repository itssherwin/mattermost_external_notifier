package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestExtractMentions(t *testing.T) {
	message := "Hi @Alice and @bob, email alice@example.com, plus @channel @here @all and @alice."
	got := extractMentions(message)
	want := []string{"alice", "bob", "alice"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestIsMonitoredScope(t *testing.T) {
	tests := []struct {
		name                string
		teamID, channelID   string
		teamIDs, channelIDs []string
		want                bool
	}{
		{"all", "team1", "channel1", nil, nil, true},
		{"team match", "team1", "channel1", []string{"team1"}, nil, true},
		{"team mismatch", "team2", "channel1", []string{"team1"}, nil, false},
		{"channel match", "team1", "channel1", nil, []string{"channel1"}, true},
		{"channel mismatch", "team1", "channel2", nil, []string{"channel1"}, false},
		{"both match", "team1", "channel1", []string{"team1"}, []string{"channel1"}, true},
		{"one mismatch", "team2", "channel1", []string{"team1"}, []string{"channel1"}, false},
	}

	p := &Plugin{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.isMonitoredScope(tt.teamID, tt.channelID, tt.teamIDs, tt.channelIDs); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidatedNotifyURL(t *testing.T) {
	valid := []string{
		"https://example.com/notify",
		"http://127.0.0.1:8080/notify?source=mattermost",
	}
	for _, value := range valid {
		if _, err := validatedNotifyURL(value); err != nil {
			t.Errorf("expected %q to be valid: %v", value, err)
		}
	}

	invalid := []string{"", "example.com/notify", "ftp://example.com/notify"}
	for _, value := range invalid {
		if _, err := validatedNotifyURL(value); err == nil {
			t.Errorf("expected %q to be invalid", value)
		}
	}
}

func TestStringListUnmarshal(t *testing.T) {
	var list StringList
	if err := list.UnmarshalJSON([]byte(`"team1, team2,team1"`)); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(list, ","), "team1,team2"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNotifySendsNumberInJSON(t *testing.T) {
	var gotBody string
	var gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	p := &Plugin{client: &http.Client{Timeout: time.Second}}
	config := Configuration{NotifyURL: server.URL, TimeoutSeconds: 1, AuthToken: "secret"}
	post := &model.Post{
		Id:        "post1",
		UserId:    "author1",
		TeamId:    "team1",
		ChannelId: "channel1",
		Message:   "hello @alice",
	}

	p.notify(config, "alice", "0901234567", post)

	if gotAuth != "Bearer secret" {
		t.Fatalf("got authorization %q", gotAuth)
	}
	if !strings.Contains(gotBody, "0901234567") {
		t.Fatalf("request body did not contain number: %s", gotBody)
	}
}
