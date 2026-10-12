package tools

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

const (
	webFetchTimeout = 20 * time.Second
	webFetchBody    = 2 << 20  // bytes fetched
	webFetchText    = 32 << 10 // bytes returned
)

type webFetchArgs struct {
	URL string `json:"url" jsonschema:"absolute http or https URL to read"`
}

// webFetch fetches a page and returns its visible text, capped so a page can
// never flood the context or the log.
func webFetch() (tool.Tool, error) {
	client := &http.Client{
		Timeout: webFetchTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return publicHost(req.Context(), req.URL.Hostname())
		},
	}
	return functiontool.New(functiontool.Config{
		Name:        "webfetch",
		Description: "Fetch a web page and return its readable text (truncated to 32 KiB).",
	}, func(ctx adkagent.Context, a webFetchArgs) (map[string]any, error) {
		if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
			return nil, fmt.Errorf("url must start with http:// or https://")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return nil, err
		}
		if err := publicHost(ctx, req.URL.Hostname()); err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "agentlite/1.0")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("%s returned HTTP %d", a.URL, resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchBody))
		if err != nil {
			return nil, err
		}
		text := body
		if strings.Contains(resp.Header.Get("Content-Type"), "html") {
			text = []byte(visibleText(body))
		}
		truncated := len(text) > webFetchText
		if truncated {
			text = text[:webFetchText]
		}
		return map[string]any{"url": a.URL, "text": string(text), "truncated": truncated}, nil
	})
}

// visibleText extracts text nodes outside script and style elements.
func visibleText(page []byte) string {
	z := html.NewTokenizer(strings.NewReader(string(page)))
	var b strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(b.String()), " ")
		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) == "script" || string(name) == "style" {
				skip++
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); (string(name) == "script" || string(name) == "style") && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				b.Write(z.Text())
				b.WriteByte(' ')
			}
		}
	}
}

// publicHost rejects hosts that resolve to loopback, private or link-local
// addresses, so a page the model is asked to read cannot be an internal service.
func publicHost(ctx context.Context, host string) error {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() {
			return fmt.Errorf("%s is not a public host", host)
		}
	}
	return nil
}
