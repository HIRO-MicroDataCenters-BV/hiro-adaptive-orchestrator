package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// postJSON sends body to url as JSON, with apiKey as a bearer token when set, and decodes a 200 reply into reply.
func postJSON(ctx context.Context, client *http.Client, url, apiKey string, body, reply any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to encode the request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the model answered with status %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(reply); err != nil {
		return fmt.Errorf("failed to decode the reply: %w", err)
	}
	return nil
}
