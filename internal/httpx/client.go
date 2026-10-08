// Package httpx wraps a TLS-fingerprint-impersonating HTTP client behind a
// small stdlib-shaped interface, so the rest of the code base never has to
// depend on the forked net/http types used by the fingerprinting library.
package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// DefaultUserAgent mirrors config.DefaultUserAgent without an import cycle.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

// DefaultProfile 返回出站连接使用的 TLS/HTTP2 指纹档案。必须与
// DefaultUserAgent 保持同一代际：JA3 指纹说 Chrome/120、UA 却说
// Chrome/149，属于自相矛盾的客户端特征，容易被上游当成异常流量。
func DefaultProfile() profiles.ClientProfile {
	return profiles.Chrome_150
}

// DefaultMaxBodyBytes 非流式响应体的默认读取上限。上游（或中间人）发一个
// 无限大的 body 时，io.ReadAll 会把网关内存直接吃光。
const DefaultMaxBodyBytes int64 = 64 << 20

// ErrBodyTooLarge 响应体超过读取上限时返回（原样透出，方便上层判断）。
var ErrBodyTooLarge = errors.New("response body exceeds the configured limit")

// readLimited 读满上限即报错，不把超限部分继续吞进内存。
func readLimited(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = DefaultMaxBodyBytes
	}
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrBodyTooLarge
	}
	return b, nil
}

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
	// MaxBodyBytes 非流式响应体读取上限，0 = DefaultMaxBodyBytes。
	MaxBodyBytes int64
}

type tlsSession struct {
	inner tls_client.HttpClient

	mu      sync.Mutex
	cookies map[string]string
	closed  bool

	maxBody int64
}

// NewSession builds a Chrome-impersonating session.
func NewSession(opts Options) (Session, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	tlsOpts := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(DefaultProfile()),
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
		maxBody: opts.MaxBodyBytes,
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
	body, err := readLimited(resp.Body, s.maxBody)
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
		body, _ := readLimited(resp.Body, s.maxBody)
		resp.Body.Close()
		return nil, resp.StatusCode, fmt.Errorf("upstream status %d: %s", resp.StatusCode, truncate(string(body), 400))
	}
	return resp.Body, resp.StatusCode, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 按字节截断可能切碎多字节字符，回退到字符边界，避免产出非法 UTF-8。
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
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
