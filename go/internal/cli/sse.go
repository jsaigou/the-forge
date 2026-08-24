package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// StreamEvents subscribes to GET /api/v1/events (SSE) and dispatches each
// event body to the handler registered for its name. Unknown events are
// ignored. Returns when ctx is cancelled or the stream breaks.
func (c *Client) StreamEvents(ctx context.Context, handlers map[string]func(raw json.RawMessage)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("SSE: HTTP %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if h, ok := handlers[event]; ok && h != nil {
				h(json.RawMessage(data))
			}
			if event == "smith:message_done" || event == "" {
				// fallthrough handled by caller via context
			}
		case line == "":
			event = ""
		}
	}
	return sc.Err()
}
