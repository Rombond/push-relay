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
	Server   string // host of the Tipsarr server the alert is about, so the app opens the right account

	// Optional presentation (v0.3.0). Zero values keep the v0.2.0 behaviour and payload.
	Level    string // "", "passive", "active", "time-sensitive"
	Silent   bool   // no sound
	Thread   string // groups notifications; "" = one thread per id (v0.2.0)
	Collapse string // a newer push with the same value replaces the older one
	TTL      *int   // seconds Apple/Google keep it for an offline device; nil = 24 h (v0.2.0), 0 = now or never
	Image    string // https URL the app's notification extension downloads (iOS) / shown by Android
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
