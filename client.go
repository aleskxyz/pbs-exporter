package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/semaphore"
)

const apiPrefix = "/api2/json"

// maxErrorBody is how much of an error response is read for its message.
const maxErrorBody = 64 << 10

// maxErrorMessage is the length an error message from PBS is cut to.
const maxErrorMessage = 200

// apiPath builds an API path from its segments, escaping each one, e.g.
// apiPath("admin", "datastore", store, "snapshots").
func apiPath(segments ...string) string {
	var b strings.Builder
	b.WriteString(apiPrefix)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// APIError is a non-200 answer of the PBS API.
type APIError struct {
	// Path is the requested API path, without the query.
	Path string
	// Status is the HTTP status code.
	Status int
	// Message is the error message of PBS, without line breaks and shortened.
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Path, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Path, e.Status, e.Message)
}

// IsForbidden reports whether err is a 403 of the PBS API, i.e. the token lacks
// a privilege.
func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden
}

// errorMessage extracts the message from an error response: the "message"
// field of a JSON body, otherwise the body itself.
func errorMessage(body []byte) string {
	var resp struct {
		Message string `json:"message"`
	}
	msg := string(body)
	if json.Unmarshal(body, &resp) == nil && resp.Message != "" {
		msg = resp.Message
	}
	msg = strings.TrimSpace(sanitize(msg))
	if len(msg) > maxErrorMessage {
		msg = strings.ToValidUTF8(msg[:maxErrorMessage], "") + "…"
	}
	if !utf8.ValidString(msg) {
		msg = strings.ToValidUTF8(msg, "")
	}
	return msg
}

// apiClient makes the requests of one scrape to one PBS.
type apiClient struct {
	endpoint            string
	authorizationHeader string
	client              *http.Client
	// sem limits the concurrent requests to the PBS of a module.
	sem *semaphore.Weighted
}

// get requests path with the query q and decodes the "data" field of the
// answer into out.
func (c *apiClient) get(ctx context.Context, path string, q url.Values, out any) error {
	_, _, err := c.getTimed(ctx, path, q, out)
	return err
}

// getTimed is get that also returns when the request was sent and when the
// answer arrived (waiting for a free request slot does not count).
func (c *apiClient) getTimed(ctx context.Context, path string, q url.Values, out any) (sent, received time.Time, err error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return sent, received, fmt.Errorf("%s: %w", path, err)
	}
	defer c.sem.Release(1)

	u := c.endpoint + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return sent, received, fmt.Errorf("%s: %w", path, err)
	}
	req.Header.Set("Authorization", c.authorizationHeader)

	slog.Debug("request", "url", sanitize(u))
	sent = time.Now()
	resp, err := c.client.Do(req)
	received = time.Now()
	if err != nil {
		return sent, received, fmt.Errorf("%s: %w", path, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing response body", "err", sanitize(err.Error()))
		}
	}()
	slog.Debug("response", "url", sanitize(u), "status", resp.StatusCode, "duration", received.Sub(sent))

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return sent, received, &APIError{Path: path, Status: resp.StatusCode, Message: errorMessage(body)}
	}

	// the "data" field is decoded straight into out
	envelope := struct {
		Data any `json:"data"`
	}{Data: out}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return sent, received, fmt.Errorf("%s: decoding response: %w", path, err)
	}
	return sent, received, nil
}
