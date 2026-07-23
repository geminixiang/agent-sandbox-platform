package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const (
	leasePath            = "v1/leases"
	maxJSONRequestBytes  = 1024 * 1024
	maxJSONResponseBytes = 16 * 1024 * 1024
	userAgent            = "agent-sandbox-go/" + Version
)

type ClientOptions struct {
	BaseURL     string
	Credentials Credentials
	HTTPClient  *http.Client
}

type Client struct {
	baseURL     *url.URL
	credentials Credentials
	httpClient  *http.Client
	ownsClient  bool

	mu     sync.RWMutex
	closed bool
}

func NewClient(options ClientOptions) (*Client, error) {
	if options.Credentials == nil {
		return nil, errors.New("sandbox: credentials are required")
	}
	baseURL, err := url.Parse(options.BaseURL)
	if err != nil || !baseURL.IsAbs() || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, errors.New("sandbox: base URL must be an absolute HTTP or HTTPS URL")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("sandbox: base URL must not contain user info, a query, or a fragment")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/"
	baseURL.RawPath = ""

	httpClient := options.HTTPClient
	ownsClient := false
	if httpClient == nil {
		httpClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
		ownsClient = true
	}
	return &Client{baseURL: baseURL, credentials: options.Credentials, httpClient: httpClient, ownsClient: ownsClient}, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	alreadyClosed := c.closed
	c.closed = true
	c.mu.Unlock()
	if !alreadyClosed && c.ownsClient {
		c.httpClient.CloseIdleConnections()
	}
	return nil
}

func (c *Client) Create(ctx context.Context, options CreateOptions) (*Sandbox, error) {
	result, err := c.Acquire(ctx, options)
	return result.Sandbox, err
}

func (c *Client) Acquire(ctx context.Context, options CreateOptions) (AcquireResult, error) {
	if strings.TrimSpace(options.Pool) == "" {
		return AcquireResult{}, errors.New("sandbox: pool must not be blank")
	}
	key := options.IdempotencyKey
	if key == "" {
		var err error
		key, err = newIdempotencyKey()
		if err != nil {
			return AcquireResult{}, fmt.Errorf("sandbox: generate idempotency key: %w", err)
		}
	}
	if strings.TrimSpace(key) == "" {
		return AcquireResult{}, errors.New("sandbox: idempotency key must not be blank")
	}
	body := acquireRequest{Pool: options.Pool}
	if options.TTLSeconds != 0 {
		if options.TTLSeconds < 0 {
			return AcquireResult{}, errors.New("sandbox: TTLSeconds must be positive")
		}
		body.TTLSeconds = &options.TTLSeconds
	}
	var response acquireResponse
	if err := c.doJSON(ctx, http.MethodPost, leasePath, nil, map[string]string{"Idempotency-Key": key}, body, &response, http.StatusCreated); err != nil {
		return AcquireResult{}, err
	}
	if err := validateRecord(response.Lease); err != nil {
		return AcquireResult{}, err
	}
	return AcquireResult{Sandbox: newSandbox(c, response.Lease), Replayed: response.Replayed, IdempotencyKey: key}, nil
}

func (c *Client) Get(ctx context.Context, id string) (*Sandbox, error) {
	return c.Connect(ctx, id)
}

func (c *Client) Connect(ctx context.Context, id string) (*Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("sandbox: id must not be blank")
	}
	var response leaseResponse
	path := leasePath + "/" + url.PathEscape(id)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, nil, &response, http.StatusOK); err != nil {
		return nil, err
	}
	if err := validateRecord(response.Lease); err != nil {
		return nil, err
	}
	return newSandbox(c, response.Lease), nil
}

func (c *Client) ListPage(ctx context.Context, options ListOptions) (Page, error) {
	query := make(url.Values)
	if options.Pool != "" {
		query.Set("pool", options.Pool)
	}
	if options.Limit != 0 {
		if options.Limit < 1 || options.Limit > 100 {
			return Page{}, errors.New("sandbox: list limit must be between 1 and 100")
		}
		query.Set("limit", strconv.Itoa(options.Limit))
	}
	if options.Cursor != "" {
		query.Set("cursor", options.Cursor)
	}
	var response listResponse
	if err := c.doJSON(ctx, http.MethodGet, leasePath, query, nil, nil, &response, http.StatusOK); err != nil {
		return Page{}, err
	}
	page := Page{Sandboxes: make([]*Sandbox, 0, len(response.Leases))}
	if response.NextCursor != nil {
		page.NextCursor = *response.NextCursor
	}
	for _, record := range response.Leases {
		if err := validateRecord(record); err != nil {
			return Page{}, err
		}
		page.Sandboxes = append(page.Sandboxes, newSandbox(c, record))
	}
	return page, nil
}

// Pager traverses all pages, including empty intermediate pages.
type Pager struct {
	client  *Client
	ctx     context.Context
	options ListOptions
	seen    map[string]struct{}
	page    Page
	index   int
	started bool
	done    bool
	current *Sandbox
	err     error
}

func (c *Client) Pager(options ListOptions) *Pager {
	return &Pager{client: c, options: options, seen: make(map[string]struct{})}
}

func (c *Client) Iterate(ctx context.Context, options ListOptions) *Pager {
	pager := c.Pager(options)
	pager.ctx = ctx
	return pager
}

func (i *Pager) Next(contexts ...context.Context) bool {
	ctx := i.ctx
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if i.done || i.err != nil {
		return false
	}
	for {
		if i.index < len(i.page.Sandboxes) {
			i.current = i.page.Sandboxes[i.index]
			i.index++
			return true
		}
		if i.started && i.page.NextCursor == "" {
			i.done = true
			return false
		}
		if i.started {
			if _, exists := i.seen[i.page.NextCursor]; exists {
				i.err = &Error{Code: codeInvalidResponse, Message: "sandbox: platform repeated a list cursor"}
				return false
			}
			i.seen[i.page.NextCursor] = struct{}{}
			i.options.Cursor = i.page.NextCursor
		}
		i.page, i.err = i.client.ListPage(ctx, i.options)
		i.started = true
		i.index = 0
	}
}

func (i *Pager) Sandbox() *Sandbox { return i.current }
func (i *Pager) Err() error        { return i.err }

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, headers map[string]string, body, target any, expectedStatus int) error {
	var content io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("sandbox: encode request: %w", err)
		}
		if len(encoded) > maxJSONRequestBytes {
			return &Error{Code: "BODY_TOO_LARGE", Message: "sandbox: JSON request exceeds 1 MiB"}
		}
		content = bytes.NewReader(encoded)
	}
	request, err := c.newRequest(ctx, method, path, query, content)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return abortedError(ctx.Err())
		}
		return operationError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		return decodePlatformError(response)
	}
	if target == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, maxJSONResponseBytes+1))
		return err
	}
	return decodeJSONResponse(response.Body, target)
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	if ctx == nil {
		return nil, errors.New("sandbox: nil context")
	}
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return nil, &Error{Code: codeClientClosed, Message: "sandbox: client is closed"}
	}
	token, err := c.credentials.GetToken(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, abortedError(ctx.Err())
		}
		return nil, operationError(err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("sandbox: token must not be blank")
	}
	reference, _ := url.Parse(path)
	endpoint := c.baseURL.ResolveReference(reference)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", userAgent)
	return request, nil
}

func decodeJSONResponse(body io.Reader, target any) error {
	limited := &io.LimitedReader{R: body, N: maxJSONResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(target); err != nil {
		return &Error{Code: codeInvalidResponse, Message: "sandbox: platform returned invalid JSON", Cause: err}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return &Error{Code: codeInvalidResponse, Message: "sandbox: platform returned trailing or oversized JSON", Cause: err}
	}
	if limited.N <= 0 {
		return &Error{Code: codeInvalidResponse, Message: "sandbox: platform JSON response exceeds 16 MiB"}
	}
	return nil
}

func decodePlatformError(response *http.Response) error {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := decodeJSONResponse(io.LimitReader(response.Body, 1024*1024), &payload); err != nil || payload.Error.Message == "" {
		return &Error{StatusCode: response.StatusCode, Message: fmt.Sprintf("sandbox: platform returned HTTP %d", response.StatusCode), Cause: err}
	}
	return &Error{StatusCode: response.StatusCode, Code: payload.Error.Code, Message: payload.Error.Message}
}

func validateRecord(record LeaseRecord) error {
	if record.ID == "" || record.Pool == "" || record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() || record.LastUsedAt.IsZero() {
		return &Error{Code: codeInvalidResponse, Message: "sandbox: platform returned an invalid lease record"}
	}
	switch record.Status {
	case LeaseActive, LeaseReleased, LeaseExpired:
		return nil
	default:
		return &Error{Code: codeInvalidResponse, Message: "sandbox: platform returned an invalid lease status"}
	}
}

func newIdempotencyKey() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

type acquireRequest struct {
	Pool       string `json:"pool"`
	TTLSeconds *int   `json:"ttlSeconds,omitempty"`
}
type acquireResponse struct {
	Lease    LeaseRecord `json:"lease"`
	Replayed bool        `json:"replayed"`
}
type leaseResponse struct {
	Lease LeaseRecord `json:"lease"`
}
type listResponse struct {
	Leases     []LeaseRecord `json:"leases"`
	NextCursor *string       `json:"nextCursor"`
}
