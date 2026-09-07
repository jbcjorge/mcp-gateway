package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	errors "github.com/jbcjorge/errors-library"
)

// transportType returns the effective transport type for a remote backend.
// Defaults to "sse" if not explicitly set.
func (b *Backend) transportType() string {
	if b.def.TransportType != "" {
		return b.def.TransportType
	}
	// Default to streamable-http if URL ends with /mcp, otherwise sse
	if strings.HasSuffix(b.def.URL, "/mcp") {
		return "streamable-http"
	}
	return "sse"
}

// sendRemote forwards a JSON-RPC message to a remote backend via HTTP.
func (b *Backend) sendRemote(ctx context.Context, msg []byte) ([]byte, error) {
	b.mu.Lock()
	b.lastUsed = time.Now()
	b.mu.Unlock()

	switch b.transportType() {
	case "streamable-http":
		return b.sendStreamableHTTP(ctx, msg)
	case "sse":
		return b.sendSSE(ctx, msg)
	default:
		return nil, ErrUnknownTransport.Parse(errors.WithParsedMessage(b.transportType()))
	}
}

// sendStreamableHTTP sends a JSON-RPC message via HTTP POST and reads the response.
// Supports both plain JSON responses and SSE-wrapped responses.
func (b *Backend) sendStreamableHTTP(ctx context.Context, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.def.URL, strings.NewReader(string(msg)))
	if err != nil {
		return nil, ErrRemoteRequest.Parse(errors.WithError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range b.def.Headers {
		req.Header.Set(k, v)
	}

	b.logDebug("sending to remote", "method", extractMethod(msg), "url", b.def.URL)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, ErrRemoteRequest.Parse(errors.WithError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, ErrRemoteStatus.Parse(errors.WithParsedMessage(resp.StatusCode), errors.WithSafeData(map[string]any{"status": resp.StatusCode}), errors.WithAdditionalData(map[string]any{"body": string(body)}))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		// Parse SSE response - extract the last "data:" line
		return b.parseSSEResponse(resp.Body)
	}

	// Plain JSON response
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, ErrRemoteRead.Parse(errors.WithError(err))
	}

	b.logDebug("remote response received", "size", len(data))
	return data, nil
}

// sendSSE sends a JSON-RPC message to an SSE endpoint. For SSE transport,
// messages are POSTed to the base URL and the response comes back either
// inline or via the SSE stream.
func (b *Backend) sendSSE(ctx context.Context, msg []byte) ([]byte, error) {
	postURL := b.def.URL
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postURL, strings.NewReader(string(msg)))
	if err != nil {
		return nil, ErrRemoteRequest.Parse(errors.WithError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range b.def.Headers {
		req.Header.Set(k, v)
	}

	b.logDebug("sending to SSE remote", "method", extractMethod(msg), "url", postURL)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, ErrRemoteRequest.Parse(errors.WithError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, ErrRemoteStatus.Parse(errors.WithParsedMessage(resp.StatusCode), errors.WithSafeData(map[string]any{"status": resp.StatusCode}), errors.WithAdditionalData(map[string]any{"body": string(body)}))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return b.parseSSEResponse(resp.Body)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, ErrRemoteRead.Parse(errors.WithError(err))
	}

	b.logDebug("SSE response received", "size", len(data))
	return data, nil
}

// parseSSEResponse extracts the JSON-RPC response from an SSE stream.
func (b *Backend) parseSSEResponse(body io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		} else if line == "" && len(dataLines) > 0 {
			// Empty line = end of event
			data := strings.Join(dataLines, "\n")
			dataLines = nil

			// Check if this is a JSON-RPC response (has "id" field)
			var envelope jsonRPCMessage
			if json.Unmarshal([]byte(data), &envelope) == nil && envelope.ID != nil {
				b.logDebug("SSE response parsed", "size", len(data))
				return []byte(data), nil
			}
			// Otherwise it might be a notification, keep reading
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, ErrSSEStreamRead.Parse(errors.WithError(err))
	}

	// If we got data lines but no empty line terminator (stream ended)
	if len(dataLines) > 0 {
		data := strings.Join(dataLines, "\n")
		return []byte(data), nil
	}

	return nil, ErrSSENoResponse.Parse()
}

// extractMethod pulls the method field from a JSON-RPC message for logging.
func extractMethod(msg []byte) string {
	var env struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(msg, &env) == nil {
		return env.Method
	}
	return "unknown"
}
