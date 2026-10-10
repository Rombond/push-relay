// Package fcm sends notifications through Firebase Cloud Messaging (HTTP v1).
package fcm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"golang.org/x/oauth2/google"

	"github.com/Rombond/push-relay/internal/push"
)

const (
	BaseURL = "https://fcm.googleapis.com"
	scope   = "https://www.googleapis.com/auth/firebase.messaging"
)

type Client struct {
	project string
	base    string
	http    *http.Client
}

// New builds a client that fetches and refreshes its own OAuth2 token from a service account file.
func New(ctx context.Context, project string, credentials []byte) (*Client, error) {
	conf, err := google.JWTConfigFromJSON(credentials, scope)
	if err != nil {
		return nil, fmt.Errorf("parse FCM credentials: %w", err)
	}
	return &Client{project: project, base: BaseURL, http: conf.Client(ctx)}, nil
}

// NewWithClient is for tests: any client, any base URL.
func NewWithClient(project, base string, c *http.Client) *Client {
	return &Client{project: project, base: base, http: c}
}

type sendBody struct {
	Message message `json:"message"`
}

type message struct {
	Token        string            `json:"token"`
	Notification notification      `json:"notification"`
	Data         map[string]string `json:"data"`
	Android      android           `json:"android"`
}

type notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type android struct {
	Priority     string       `json:"priority"`
	Notification androidNotif `json:"notification"`
	TTL          string       `json:"ttl"`
}

type androidNotif struct {
	ChannelID string `json:"channel_id"`
	Tag       string `json:"tag,omitempty"`
}

func data(m push.Message) map[string]string {
	d := map[string]string{"event": m.Event, "id": m.ID}
	if m.Server != "" {
		d["server"] = m.Server
	}
	return d
}

func (c *Client) Send(ctx context.Context, m push.Message) push.Result {
	body, _ := json.Marshal(sendBody{Message: message{
		Token:        m.Token,
		Notification: notification{Title: m.Title, Body: m.Body},
		Data:         data(m),
		Android: android{
			Priority:     "high",
			TTL:          "86400s",
			Notification: androidNotif{ChannelID: m.Category, Tag: m.ID},
		},
	}})
	url := fmt.Sprintf("%s/v1/projects/%s/messages:send", c.base, c.project)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return push.Result{Status: push.Failed, Error: "build request"}
	}
	req.Header.Set("content-type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return push.Result{Status: push.Failed, Error: "fcm unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return push.Result{Status: push.Sent}
	}
	var fail struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&fail)
	code := fail.Error.Status
	for _, d := range fail.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
		}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound, code == "UNREGISTERED",
		code == "INVALID_ARGUMENT", code == "SENDER_ID_MISMATCH":
		return push.Result{Status: push.InvalidToken}
	case resp.StatusCode == http.StatusTooManyRequests, code == "QUOTA_EXCEEDED":
		retry := 60
		if v, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && v > 0 {
			retry = v
		}
		return push.Result{Status: push.RateLimited, RetryAfter: retry}
	}
	msg := code
	if msg == "" {
		msg = "fcm status " + strconv.Itoa(resp.StatusCode)
	}
	return push.Result{Status: push.Failed, Error: msg}
}
