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

	"agentlite/pkg/citations"
	"agentlite/pkg/toolx"
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
// never flood the context or the log. The page is recorded as a source.
func webFetch(conn toolx.Connector) (toolx.Tool, error) {
	c := *conn.HTTP // the connector's client, with a redirect check of its own
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		return publicHost(req.Context(), req.URL.Hostname())
	}
	return toolx.New(toolx.Spec{
		Name:        "webfetch",
		Description: "Fetch a web page and return its readable text (truncated to 32 KiB) and its source number.",
	}, func(tc toolx.Context, a webFetchArgs) (map[string]any, error) {
		if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
			return nil, fmt.Errorf("url must start with http:// or https://")
		}
		ctx, cancel := context.WithTimeout(tc, webFetchTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return nil, err
		}
		if err := publicHost(ctx, req.URL.Hostname()); err != nil {
			return nil, err
		}
		resp, err := c.Do(req)
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
		title, text := "", string(body)
		if strings.Contains(resp.Header.Get("Content-Type"), "html") {
			title, text = readable(body)
		}
		truncated := len(text) > webFetchText
		if truncated {
			text = text[:webFetchText]
		}
		page := resp.Request.URL.String() // after redirects
		n, err := tc.AddSource(citations.Source{System: conn.ID, URL: page, Title: title})
		if err != nil {
			return nil, err
		}
		return map[string]any{"source": n, "url": page, "title": title, "text": text, "truncated": truncated}, nil
	})
}

// readable returns a page's title and its visible text: the text nodes
// outside script and style elements.
func readable(page []byte) (title, text string) {
	z := html.NewTokenizer(strings.NewReader(string(page)))
	var b strings.Builder
	skip, inTitle := 0, false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.TrimSpace(title), strings.Join(strings.Fields(b.String()), " ")
		case html.StartTagToken:
			switch name, _ := z.TagName(); string(name) {
			case "script", "style":
				skip++
			case "title":
				inTitle = true
			}
		case html.EndTagToken:
			switch name, _ := z.TagName(); string(name) {
			case "script", "style":
				if skip > 0 {
					skip--
				}
			case "title":
				inTitle = false
			}
		case html.TextToken:
			switch {
			case inTitle:
				title += string(z.Text())
			case skip == 0:
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
