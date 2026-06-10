package wayback

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	saveURL         = "https://web.archive.org/save"
	availabilityURL = "https://archive.org/wayback/available"
	pollPeriod      = 5 * time.Second
	pollTimeout     = 2 * time.Minute
)

// permanentErrors is the set of status_ext values that should never be retried.
var permanentErrors = map[string]bool{
	"error:not-found":          true,
	"error:no-access":          true,
	"error:blocked":            true,
	"error:blocked-url":        true,
	"error:invalid-url-syntax": true,
}

// Result is returned by Archive on success.
type Result struct {
	ArchiveURL string
}

// PermanentError signals a URL that should never be retried.
type PermanentError struct {
	StatusExt string
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("permanent failure: %s", e.StatusExt)
}

// TransientError signals a failure that should be retried next run.
type TransientError struct {
	StatusExt string
	Message   string
}

func (e *TransientError) Error() string {
	if e.StatusExt != "" {
		return fmt.Sprintf("transient failure: %s: %s", e.StatusExt, e.Message)
	}
	return fmt.Sprintf("transient failure: %s", e.Message)
}

type Client struct {
	accessKey  string
	secretKey  string
	httpClient *http.Client
}

func NewClient(accessKey, secretKey string) *Client {
	return &Client{
		accessKey:  accessKey,
		secretKey:  secretKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type availabilityResponse struct {
	ArchivedSnapshots struct {
		Closest struct {
			Available bool   `json:"available"`
			URL       string `json:"url"`
			Timestamp string `json:"timestamp"`
		} `json:"closest"`
	} `json:"archived_snapshots"`
}

// FindRecent looks up the most recent existing capture of targetURL via the
// Wayback Availability API. It returns the capture URL and true if one exists
// no older than maxAge. Any lookup failure returns false — the caller should
// fall through to a normal capture.
func (c *Client) FindRecent(ctx context.Context, targetURL string, maxAge time.Duration) (string, bool) {
	reqURL := fmt.Sprintf("%s?url=%s", availabilityURL, url.QueryEscape(targetURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	var ar availabilityResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return "", false
	}

	closest := ar.ArchivedSnapshots.Closest
	if !closest.Available || closest.URL == "" {
		return "", false
	}
	captured, err := time.Parse("20060102150405", closest.Timestamp)
	if err != nil || time.Since(captured) > maxAge {
		return "", false
	}

	// The availability API returns http:// links; normalise to https.
	return strings.Replace(closest.URL, "http://", "https://", 1), true
}

// Archive submits targetURL to the Wayback Machine and polls until done.
// Returns Result on success, *PermanentError or *TransientError on failure.
func (c *Client) Archive(ctx context.Context, targetURL string) (*Result, error) {
	jobID, err := c.submit(ctx, targetURL)
	if err != nil {
		return nil, &TransientError{Message: err.Error()}
	}
	return c.poll(ctx, jobID, targetURL)
}

type submitResponse struct {
	JobID string `json:"job_id"`
	URL   string `json:"url"`
}

func (c *Client) submit(ctx context.Context, targetURL string) (string, error) {
	body := url.Values{}
	body.Set("url", targetURL)
	body.Set("skip_first_archive", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, saveURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf("LOW %s:%s", c.accessKey, c.secretKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("wayback HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("wayback submit status %d", resp.StatusCode)
	}

	var sr submitResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", err
	}
	if sr.JobID == "" {
		return "", fmt.Errorf("empty job_id in response")
	}
	return sr.JobID, nil
}

type statusResponse struct {
	Status      string `json:"status"`
	JobID       string `json:"job_id"`
	Timestamp   string `json:"timestamp"`
	OriginalURL string `json:"original_url"`
	StatusExt   string `json:"status_ext"`
	Message     string `json:"message"`
}

func (c *Client) poll(ctx context.Context, jobID, originalURL string) (*Result, error) {
	deadline := time.Now().Add(pollTimeout)
	pollURL := fmt.Sprintf("%s/status/%s", saveURL, jobID)
	lastErr := ""

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, &TransientError{Message: "interrupted while polling"}
		case <-time.After(pollPeriod):
		}

		sr, err := c.pollOnce(ctx, pollURL)
		if err != nil {
			if ctx.Err() != nil {
				return nil, &TransientError{Message: "interrupted while polling"}
			}
			// A single failed poll doesn't mean the capture failed —
			// keep polling until the deadline.
			lastErr = err.Error()
			continue
		}

		switch sr.Status {
		case "success":
			if sr.Timestamp == "" {
				return nil, &TransientError{Message: "success response missing timestamp"}
			}
			archiveURL := fmt.Sprintf("https://web.archive.org/web/%s/%s", sr.Timestamp, originalURL)
			return &Result{ArchiveURL: archiveURL}, nil

		case "pending":
			continue

		case "error":
			if permanentErrors[sr.StatusExt] {
				return nil, &PermanentError{StatusExt: sr.StatusExt}
			}
			return nil, &TransientError{StatusExt: sr.StatusExt, Message: sr.Message}

		default:
			lastErr = fmt.Sprintf("unexpected status %q", sr.Status)
			continue
		}
	}

	msg := "poll timeout after 2 minutes"
	if lastErr != "" {
		msg = fmt.Sprintf("%s (last poll error: %s)", msg, lastErr)
	}
	return nil, &TransientError{Message: msg}
}

func (c *Client) pollOnce(ctx context.Context, pollURL string) (*statusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("LOW %s:%s", c.accessKey, c.secretKey))
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status endpoint HTTP %d", resp.StatusCode)
	}

	var sr statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	return &sr, nil
}
