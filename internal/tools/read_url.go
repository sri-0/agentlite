package tools

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

const (
	readURLTimeout = 20 * time.Second
	readURLBody    = 2 << 20  // bytes fetched
	readURLText    = 32 << 10 // bytes returned
)

type readURLArgs struct {
	URL string `json:"url" jsonschema:"absolute http or https URL to read"`
}

// readURL fetches a page and returns its visible text, capped so a page can
// never flood the context or the log.
func readURL(Deps) (tool.Tool, error) {
	client := &http.Client{Timeout: readURLTimeout}
	return functiontool.New(functiontool.Config{
		Name:        "read_url",
		Description: "Fetch a web page and return its readable text (truncated to 32 KiB).",
	}, func(ctx adkagent.Context, a readURLArgs) (map[string]any, error) {
		if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
			return nil, fmt.Errorf("url must start with http:// or https://")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
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
		body, err := io.ReadAll(io.LimitReader(resp.Body, readURLBody))
		if err != nil {
			return nil, err
		}
		text := body
		if strings.Contains(resp.Header.Get("Content-Type"), "html") {
			text = []byte(visibleText(body))
		}
		truncated := len(text) > readURLText
		if truncated {
			text = text[:readURLText]
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
