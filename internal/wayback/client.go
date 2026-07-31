package wayback

import (
	"context"
	"crypto/tls"
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
	"error:gone":               true,
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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Wayback's save endpoint currently returns HTTP 498 for Go's HTTP/2
	// requests. Keep the client on HTTP/1.1 while retaining default transport
	// settings such as proxy and TLS configuration.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = nil
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &Client{
		accessKey:  accessKey,
		secretKey:  secretKey,
		httpClient: &http.Client{Timeout: 30 * time.Second, Transport: transport},
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
	submission, err := c.submit(ctx, targetURL)
	if err != nil {
		return nil, &TransientError{Message: err.Error()}
	}
	if result, err := submissionResult(submission, targetURL); result != nil || err != nil {
		return result, err
	}
	if submission.JobID == "" {
		return nil, &TransientError{Message: "wayback submit response missing job_id"}
	}
	return c.poll(ctx, submission.JobID, targetURL)
}

type submitResponse struct {
	JobID       string `json:"job_id"`
	URL         string `json:"url"`
	Status      string `json:"status"`
	Timestamp   string `json:"timestamp"`
	OriginalURL string `json:"original_url"`
	StatusExt   string `json:"status_ext"`
	Message     string `json:"message"`
}

func submissionResult(submission submitResponse, targetURL string) (*Result, error) {
	switch submission.Status {
	case "":
		return nil, nil
	case "success":
		if submission.Timestamp == "" {
			return nil, &TransientError{Message: "successful Wayback submit response missing timestamp"}
		}
		return &Result{ArchiveURL: fmt.Sprintf("https://web.archive.org/web/%s/%s", submission.Timestamp, targetURL)}, nil
	case "error":
		if permanentErrors[submission.StatusExt] {
			return nil, &PermanentError{StatusExt: submission.StatusExt}
		}
		return nil, &TransientError{StatusExt: submission.StatusExt, Message: submission.Message}
	case "pending":
		return nil, nil
	default:
		return nil, &TransientError{StatusExt: submission.StatusExt, Message: fmt.Sprintf("unexpected submit status %q: %s", submission.Status, submission.Message)}
	}
}

func (c *Client) submit(ctx context.Context, targetURL string) (submitResponse, error) {
	body := url.Values{}
	body.Set("url", targetURL)
	body.Set("skip_first_archive", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, saveURL, strings.NewReader(body.Encode()))
	if err != nil {
		return submitResponse{}, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("LOW %s:%s", c.accessKey, c.secretKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return submitResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return submitResponse{}, fmt.Errorf("wayback HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return submitResponse{}, fmt.Errorf("wayback submit status %d", resp.StatusCode)
	}

	var sr submitResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return submitResponse{}, err
	}
	return sr, nil
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
