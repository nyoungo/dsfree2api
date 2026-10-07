package httpx

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

// TestStreamTiming measures whether the httpx stack (tls-client + proxy)
// delivers a slow-drip HTTP response progressively or in one burst.
//
//	httpbin drip: delay=2s, then numbytes=8 over duration=4s.
//
// Progressive: TTFB ~2s, done ~6s. Buffered: TTFB ~6s, all at once.
func TestStreamTiming(t *testing.T) {
	if os.Getenv("STREAM_TIMING") == "" {
		t.Skip("set STREAM_TIMING=1 to run")
	}
	proxy := os.Getenv("STREAM_PROXY")
	s, err := NewSession(Options{ProxyURL: proxy, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()

	url := "https://httpbin.org/drip?duration=4&numbytes=8&delay=2"
	start := time.Now()
	rc, status, err := s.Stream(context.Background(), Request{Method: "GET", URL: url})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer rc.Close()
	t.Logf("proxy=%q status=%d headers in %v", proxy, status, time.Since(start).Round(time.Millisecond))

	buf := make([]byte, 256)
	var readAt []time.Duration
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			readAt = append(readAt, time.Since(start))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Logf("read err after %d reads: %v", len(readAt), rerr)
			break
		}
	}
	total := time.Since(start)
	if len(readAt) == 0 {
		t.Fatalf("no bytes read")
	}
	t.Logf("first byte=%v last byte=%v reads=%d", readAt[0].Round(time.Millisecond), total.Round(time.Millisecond), len(readAt))
	for i, ts := range readAt {
		if i < 6 || i >= len(readAt)-3 {
			t.Logf("  read[%d] @ %v", i, ts.Round(time.Millisecond))
		}
	}
}
