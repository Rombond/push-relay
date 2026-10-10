// Package push holds the types shared by the platform senders and the HTTP server.
package push

import "context"

// Message is what a platform sender delivers. Everything is already validated and localized.
type Message struct {
	Token    string
	Sandbox  bool
	Title    string
	Body     string
	Event    string
	ID       string
	Category string
}

type Status string

const (
	Sent         Status = "sent"
	InvalidToken Status = "invalid_token"
	RateLimited  Status = "rate_limited"
	Failed       Status = "error"
)

// Result is the outcome for one message.
type Result struct {
	Status     Status `json:"status"`
	RetryAfter int    `json:"retryAfter,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Sender delivers one message to one platform (APNs or FCM).
type Sender interface {
	Send(ctx context.Context, m Message) Result
}
