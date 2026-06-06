package wayback

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	saveURL    = "https://web.archive.org/save"
	pollPeriod = 5 * time.Second
	pollTimeout = 2 * time.Minute
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

// Archive submits targetURL to the Wayback Machine and polls until done.
// Returns Result on success, *PermanentError or *TransientError on failure.
func (c *Client) Archive(targetURL string) (*Result, error) {
	jobID, err := c.submit(targetURL)
	if err != nil {
		return nil, &TransientError{Message: err.Error()}
	}
	return c.poll(jobID, targetURL)
}

type submitResponse struct {
	JobID string `json:"job_id"`
	URL   string `json:"url"`
}

func (c *Client) submit(targetURL string) (string, error) {
	body := url.Values{}
	body.Set("url", targetURL)
	body.Set("skip_first_archive", "1")

	req, err := http.NewRequest(http.MethodPost, saveURL, strings.NewReader(body.Encode()))
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
	Status    string `json:"status"`
	JobID     string `json:"job_id"`
	Timestamp string `json:"timestamp"`
	OriginalURL string `json:"original_url"`
	StatusExt string `json:"status_ext"`
	Message   string `json:"message"`
}

func (c *Client) poll(jobID, originalURL string) (*Result, error) {
	deadline := time.Now().Add(pollTimeout)
	pollURL := fmt.Sprintf("%s/status/%s", saveURL, jobID)

	for time.Now().Before(deadline) {
		time.Sleep(pollPeriod)

		req, err := http.NewRequest(http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, &TransientError{Message: err.Error()}
		}
		req.Header.Set("Authorization", fmt.Sprintf("LOW %s:%s", c.accessKey, c.secretKey))
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, &TransientError{Message: err.Error()}
		}

		var sr statusResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&sr)
		resp.Body.Close()

		if decodeErr != nil {
			return nil, &TransientError{Message: decodeErr.Error()}
		}

		switch sr.Status {
		case "success":
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
			return nil, &TransientError{Message: fmt.Sprintf("unexpected status %q", sr.Status)}
		}
	}

	return nil, &TransientError{Message: "poll timeout after 2 minutes"}
}
