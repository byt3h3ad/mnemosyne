package raindrop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	baseURL = "https://api.raindrop.io/rest/v1"
	perPage = 50
)

// PermanentSyncError signals a sync-back that can never succeed
// (bookmark deleted in Raindrop, note already full) and should not be retried.
type PermanentSyncError struct {
	Reason string
}

func (e *PermanentSyncError) Error() string {
	return "permanent sync failure: " + e.Reason
}

// httpStatusError carries the status code so callers can distinguish 404s.
type httpStatusError struct {
	method string
	url    string
	code   int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d", e.method, e.url, e.code)
}

type Client struct {
	token       string
	httpClient  *http.Client
	rateLimitMs int
}

func NewClient(token string, rateLimitMs int) *Client {
	return &Client{
		token:       token,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		rateLimitMs: rateLimitMs,
	}
}

// Bookmark is the subset of raindrop fields we care about.
type Bookmark struct {
	ID      int64
	URL     string
	Note    string
	Created time.Time
}

type raindropItem struct {
	ID      int64     `json:"_id"`
	Link    string    `json:"link"`
	Note    string    `json:"note"`
	Created time.Time `json:"created"`
}

type listResponse struct {
	Result bool           `json:"result"`
	Items  []raindropItem `json:"items"`
}

// FetchAll paginates through every bookmark in all collections.
func (c *Client) FetchAll(ctx context.Context) ([]Bookmark, error) {
	return c.paginate(ctx, time.Time{})
}

// FetchSince paginates newest-first and stops when items are older than since.
func (c *Client) FetchSince(ctx context.Context, since time.Time) ([]Bookmark, error) {
	return c.paginate(ctx, since)
}

func (c *Client) paginate(ctx context.Context, since time.Time) ([]Bookmark, error) {
	var all []Bookmark
	for page := 0; ; page++ {
		if page > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(c.rateLimitMs) * time.Millisecond):
			}
		}

		url := fmt.Sprintf("%s/raindrops/0?sort=-created&page=%d&perpage=%d", baseURL, page, perPage)
		var resp listResponse
		if err := c.get(ctx, url, &resp); err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		if !resp.Result {
			return nil, fmt.Errorf("page %d: raindrop API returned result:false", page)
		}
		if len(resp.Items) == 0 {
			break
		}

		done := false
		for _, item := range resp.Items {
			if !since.IsZero() && item.Created.Before(since) {
				done = true
				break
			}
			all = append(all, Bookmark{
				ID:      item.ID,
				URL:     item.Link,
				Note:    item.Note,
				Created: item.Created,
			})
		}
		if done {
			break
		}
	}
	return all, nil
}

// GetNote fetches the current note for a single raindrop.
func (c *Client) GetNote(ctx context.Context, id int64) (string, error) {
	url := fmt.Sprintf("%s/raindrop/%d", baseURL, id)
	var resp struct {
		Item raindropItem `json:"item"`
	}
	if err := c.get(ctx, url, &resp); err != nil {
		return "", err
	}
	return resp.Item.Note, nil
}

// AppendNote appends the archive URL to the existing note.
// It is idempotent: if the archive URL is already present it returns immediately.
// Returns *PermanentSyncError when the sync can never succeed.
func (c *Client) AppendNote(ctx context.Context, id int64, archiveURL string) error {
	existing, err := c.GetNote(ctx, id)
	if err != nil {
		var se *httpStatusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return &PermanentSyncError{Reason: "bookmark no longer exists in Raindrop"}
		}
		return fmt.Errorf("get note: %w", err)
	}

	// Guard against duplicate entries if MarkSynced failed on a previous run.
	if strings.Contains(existing, archiveURL) {
		return nil
	}

	suffix := fmt.Sprintf("\nArchived: %s", archiveURL)
	note := existing + suffix
	if len(note) > 10000 {
		return &PermanentSyncError{
			Reason: fmt.Sprintf("note would exceed 10,000 chars (current: %d)", len(existing)),
		}
	}

	url := fmt.Sprintf("%s/raindrop/%d", baseURL, id)
	body, _ := json.Marshal(map[string]string{"note": note})

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == http.StatusNotFound {
		return &PermanentSyncError{Reason: "bookmark no longer exists in Raindrop"}
	}
	if httpResp.StatusCode != http.StatusOK {
		return &httpStatusError{method: http.MethodPut, url: url, code: httpResp.StatusCode}
	}
	return nil
}

func (c *Client) get(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{method: http.MethodGet, url: url, code: resp.StatusCode}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
