package raindrop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	baseURL = "https://api.raindrop.io/rest/v1"
	perPage = 50
)

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
func (c *Client) FetchAll() ([]Bookmark, error) {
	return c.paginate(time.Time{})
}

// FetchSince paginates newest-first and stops when items are older than since.
func (c *Client) FetchSince(since time.Time) ([]Bookmark, error) {
	return c.paginate(since)
}

func (c *Client) paginate(since time.Time) ([]Bookmark, error) {
	var all []Bookmark
	for page := 0; ; page++ {
		if page > 0 {
			time.Sleep(time.Duration(c.rateLimitMs) * time.Millisecond)
		}

		url := fmt.Sprintf("%s/raindrops/0?sort=-created&page=%d&perpage=%d", baseURL, page, perPage)
		var resp listResponse
		if err := c.get(url, &resp); err != nil {
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
func (c *Client) GetNote(id int64) (string, error) {
	url := fmt.Sprintf("%s/raindrop/%d", baseURL, id)
	var resp struct {
		Item raindropItem `json:"item"`
	}
	if err := c.get(url, &resp); err != nil {
		return "", err
	}
	return resp.Item.Note, nil
}

// AppendNote appends the archive URL to the existing note.
// It is idempotent: if the archive URL is already present it returns immediately.
func (c *Client) AppendNote(id int64, archiveURL string) error {
	existing, err := c.GetNote(id)
	if err != nil {
		return fmt.Errorf("get note: %w", err)
	}

	// Guard against duplicate entries if MarkSynced failed on a previous run.
	if strings.Contains(existing, archiveURL) {
		return nil
	}

	suffix := fmt.Sprintf("\nArchived: %s", archiveURL)
	note := existing + suffix
	if len(note) > 10000 {
		return fmt.Errorf("note would exceed 10,000 chars (current: %d)", len(existing))
	}

	url := fmt.Sprintf("%s/raindrop/%d", baseURL, id)
	body, _ := json.Marshal(map[string]string{"note": note})

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
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

	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("PUT /raindrop/%d: status %d", id, httpResp.StatusCode)
	}
	return nil
}

func (c *Client) get(url string, out any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
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
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
