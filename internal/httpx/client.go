// Package httpx wraps a TLS-fingerprint-impersonating HTTP client behind a
// small stdlib-shaped interface, so the rest of the code base never has to
// depend on the forked net/http types used by the fingerprinting library.
package httpx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// DefaultUserAgent mirrors config.DefaultUserAgent without an import cycle.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

type Request struct {
	Method  string
	URL     string
	Header  map[string]string
	Body    []byte
	Timeout time.Duration // 0 = session-level timeout only
}

type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Session is a browser-like cookie jar plus connection pool, bound to one
// site/proxy route.
type Session interface {
	Do(ctx context.Context, r Request) (*Response, error)
	Stream(ctx context.Context, r Request) (io.ReadCloser, int, error)
	Cookies() map[string]string
	SetCookies(map[string]string)
	Close()
}

type Options struct {
	ProxyURL string
	Timeout  time.Duration
}

type tlsSession struct {
	inner tls_client.HttpClient

	mu      sync.Mutex
	cookies map[string]string
	closed  bool
}

// NewSession builds a Chrome-impersonating session.
func NewSession(opts Options) (Session, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	tlsOpts := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profiles.Chrome_120),
		tls_client.WithTimeoutSeconds(int(timeout.Seconds()) + 1),
		tls_client.WithRandomTLSExtensionOrder(),
	}
	if p := opts.ProxyURL; p != "" {
		tlsOpts = append(tlsOpts, tls_client.WithProxyUrl(p))
	}
	inner, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), tlsOpts...)
	if err != nil {
		return nil, fmt.Errorf("create tls session: %w", err)
	}
	return &tlsSession{
		inner:   inner,
		cookies: map[string]string{},
	}, nil
}

func (s *tlsSession) Cookies() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.cookies))
	for k, v := range s.cookies {
		out[k] = v
	}
	return out
}

func (s *tlsSession) SetCookies(in map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range in {
		s.cookies[k] = v
	}
}

func (s *tlsSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.inner.CloseIdleConnections()
}

func (s *tlsSession) newRequest(r Request) (*fhttp.Request, error) {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := fhttp.NewRequest(method, r.URL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header = fhttp.Header{}
	req.Header.Set("User-Agent", DefaultUserAgent)
	req.Header.Set("Accept", "*/*")
	if len(r.Body) > 0 && r.Header["Content-Type"] == "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range r.Header {
		req.Header.Set(k, v)
	}

	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	s.mu.Lock()
	names := make([]string, 0, len(s.cookies))
	for k := range s.cookies {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		if n == "" {
			continue
		}
		req.AddCookie(&fhttp.Cookie{Name: n, Value: s.cookies[n], Path: "/"})
	}
	s.mu.Unlock()
	_ = u
	return req, nil
}

func (s *tlsSession) capture(resp *fhttp.Response) {
	cookies := (&http.Response{Header: http.Header(resp.Header)}).Cookies()
	if len(cookies) == 0 {
		for _, line := range http.Header(resp.Header).Values("Set-Cookie") {
			if c, err := http.ParseSetCookie(line); err == nil && c.Name != "" {
				cookies = append(cookies, c)
			}
		}
	}
	if len(cookies) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		if c.MaxAge < 0 {
			delete(s.cookies, c.Name)
			continue
		}
		if c.Value == "" {
			continue
		}
		s.cookies[c.Name] = c.Value
	}
}

func (s *tlsSession) Do(ctx context.Context, r Request) (*Response, error) {
	req, err := s.newRequest(r)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	resp, err := s.inner.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	s.capture(resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return &Response{Status: resp.StatusCode, Header: http.Header(resp.Header), Body: body}, nil
}

// Stream performs the request and returns the still-open body. The caller
// must Close it; closing also releases the underlying connection.
func (s *tlsSession) Stream(ctx context.Context, r Request) (io.ReadCloser, int, error) {
	req, err := s.newRequest(r)
	if err != nil {
		return nil, 0, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := s.inner.Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, err
	}
	s.capture(resp)
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, resp.StatusCode, fmt.Errorf("upstream status %d: %s", resp.StatusCode, truncate(string(body), 400))
	}
	return resp.Body, resp.StatusCode, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// StdClient is a plain net/http client used for non-fingerprinted calls
// (Turnstile solver API, proxy health checks).
func StdClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}
