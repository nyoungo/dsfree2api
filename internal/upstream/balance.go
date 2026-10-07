package upstream

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
)

// guestCookieName mirrors the upstream guest-token plugin's visitor cookie;
// its value is the "gid" the balance endpoint reports on.
const guestCookieName = "dsgt_gid"

// SiteBalance is the response of GET /wp-json/dsgt/v1/balance. The free-tier
// block only appears when the request carries a bot_id.
type SiteBalance struct {
	Gid     string  `json:"gid"`
	Balance float64 `json:"balance"`
	Free    struct {
		Limit       int    `json:"limit"`
		Used        int    `json:"used"`
		Remaining   int    `json:"remaining"`
		ResetPeriod string `json:"reset_period"`
	} `json:"free"`
}

// FetchBalance queries the site's guest balance endpoint for one visitor id.
// No nonce is required for guests; the id travels as the visitor cookie.
// botID selects the per-bot free tier (0 omits it, which also omits free).
func (c *Client) FetchBalance(
	ctx context.Context,
	site config.Site,
	gid string,
	botID int,
	route Route,
) (*SiteBalance, error) {
	if gid == "" {
		return nil, errf("no guest id for site %s", site.Code)
	}
	sess, err := httpx.NewSession(httpx.Options{ProxyURL: route.Proxy, Timeout: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	sess.SetCookies(map[string]string{guestCookieName: gid})

	url := site.BaseURL + "/wp-json/dsgt/v1/balance?_t=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	if botID > 0 {
		url += "&bot_id=" + itoa(botID)
	}
	c.cfg.RLock()
	ua := c.cfg.Upstream.UserAgent
	c.cfg.RUnlock()
	resp, err := sess.Do(ctx, httpx.Request{
		Method: "GET",
		URL:    url,
		Header: map[string]string{
			"User-Agent": ua,
			"Accept":     "application/json",
			"Referer":    site.BaseURL + "/",
		},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if resp.Status >= 400 {
		return nil, errf("balance http %d: %s", resp.Status, truncate(string(resp.Body), 200))
	}
	var out SiteBalance
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, errf("balance bad json: %v", err)
	}
	return &out, nil
}
