package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a external manager.
type Client struct {
	baseURL   string
	token     string
	userAgent string
	http      *http.Client
	// stream carries request bodies that are streams (UploadProjectSource). It
	// has no total timeout: an upload is as long as the file and the link are,
	// and DefaultTimeout would cut every large one off. The caller's ctx is the
	// only bound.
	stream *http.Client
}

// Option configures a Client, following the functional-options shape pkg/client
// and pkg/watcherclient already use.
type Option func(*Client)

// WithToken sets the bearer token this watcher authenticates with.
func WithToken(t string) Option { return func(c *Client) { c.token = t } }

// WithUserAgent names the caller.
//
// Not cosmetic: the CLI sign-in reads it to name the token it issues, so
// somebody with four of them can tell which machine to revoke.
func WithUserAgent(ua string) Option { return func(c *Client) { c.userAgent = ua } }

// WithTimeout bounds one request.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http = &http.Client{Timeout: d} }
}

// DefaultTimeout bounds one request when the caller names no other.
//
// Named rather than written inline because it is half of a contract: the
// external manager's own handlers have to finish inside it, or the caller gives up
// first and throws away the answer — including the sentence saying what went
// wrong. The external manager asserts the other half of this against this
// constant, so the two cannot drift apart silently.
const DefaultTimeout = 20 * time.Second

// New builds a client. A request is bounded by default: a stalled external manager
// would otherwise hold the build worker that is waiting for a credential.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: DefaultTimeout},
		stream:  &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// APIError carries the status code, so a caller can tell "you have no access to
// that repository" from "the external manager is down" without matching on message
// text — the mistake pkg/client and pkg/watcherclient both had to correct.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control %s %s: %s (%d)", e.Method, e.Path, e.Message, e.StatusCode)
}

// StatusCode reports the HTTP status behind an error, or 0.
func StatusCode(err error) int {
	var e *APIError
	if asAPIError(err, &e) {
		return e.StatusCode
	}
	return 0
}

// CloneToken asks for a credential to clone repoURL on behalf of namespace.
func (c *Client) CloneToken(ctx context.Context, req CloneTokenRequest) (*CloneTokenResponse, error) {
	var out CloneTokenResponse
	if err := c.do(ctx, http.MethodPost, "/internal/v1/clone-token", true, req, &out); err != nil {
		return nil, err
	}
	if out.Token == "" {
		return nil, fmt.Errorf("control POST /internal/v1/clone-token: no token in the response")
	}
	return &out, nil
}

// ExchangeCLIToken turns the one-time code from a browser sign-in into an API
// token.
//
// Sent without a bearer, because the whole point is that the caller does not
// have one yet: the code and the verifier are the credential, and the verifier
// is what proves this is the process that started the sign-in.
func (c *Client) ExchangeCLIToken(ctx context.Context, code, verifier string) (*CLITokenResponse, error) {
	var out CLITokenResponse
	if err := c.do(ctx, http.MethodPost, "/auth/cli/token", false,
		CLITokenRequest{Code: code, Verifier: verifier}, &out); err != nil {
		return nil, err
	}
	if out.Token == "" {
		return nil, fmt.Errorf("control POST /auth/cli/token: no token in the response")
	}
	return &out, nil
}

// Account reads one account, which is what a CLI can use to check that its
// stored token still works.
//
// Deliberately not /apis/v1/me: that route requires a person, and a token is
// not one. A CLI asking "who am I" is asking which account it acts for.
func (c *Client) Account(ctx context.Context, slug string) (*AccountResponse, error) {
	var out AccountResponse
	if err := c.do(ctx, http.MethodGet, "/apis/v1/accounts/"+url.PathEscape(slug), true, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// do is the one place a request is built, so a new call cannot forget the
// bearer, the limit on the response, or the error envelope.
func (c *Client) do(ctx context.Context, method, path string, auth bool, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if auth && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{
			Method: method, Path: path,
			StatusCode: resp.StatusCode,
			Message:    messageFrom(raw, resp.Status),
		}
	}
	if out == nil {
		return nil
	}
	// A plain-text answer (a build log) is handed back as it is.
	if sp, ok := out.(*string); ok {
		*sp = string(raw)
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("control %s %s: response is not JSON: %w", method, path, err)
	}
	return nil
}

// messageFrom pulls the message out of the external manager's error envelope,
// falling back to the status line. The envelope is
// {"error":{"code":"...","message":"..."}}.
func messageFrom(raw []byte, status string) string {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		if env.Error.Code != "" {
			return env.Error.Code + ": " + env.Error.Message
		}
		return env.Error.Message
	}
	if len(raw) > 0 && len(raw) < 300 {
		return string(raw)
	}
	return status
}
