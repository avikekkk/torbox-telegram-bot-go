// Package telegraph publishes the full search-result listing to graph.org.
//
// Content is built as Telegraph's node tree directly rather than as an HTML
// string, so there is no HTML-to-node conversion step to get wrong.
package telegraph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiBase = "https://api.graph.org"

// Node is a Telegraph content node. Exactly one of Text or the element fields
// is used; MarshalJSON picks the right shape.
type Node struct {
	Text     string
	Tag      string
	Children []Node
}

// Text returns a bare text node.
func Text(s string) Node { return Node{Text: s} }

// Tag returns an element node wrapping children.
func Tag(tag string, children ...Node) Node { return Node{Tag: tag, Children: children} }

// MarshalJSON renders a text node as a JSON string and an element node as the
// {tag, children} object Telegraph expects.
func (n Node) MarshalJSON() ([]byte, error) {
	if n.Tag == "" {
		return json.Marshal(n.Text)
	}
	element := map[string]any{"tag": n.Tag}
	if len(n.Children) > 0 {
		element["children"] = n.Children
	}
	return json.Marshal(element)
}

// Page is a created Telegraph page.
type Page struct {
	URL  string
	Path string
}

// Client is an authenticated Telegraph account.
//
// The access token is kept for the lifetime of the search session: revoking it
// straight after creation, as the Python bot originally did, left the page
// permanently uneditable and so impossible to redact.
type Client struct {
	token string
	http  *http.Client
}

// NewAccount creates a throwaway Telegraph account.
func NewAccount(ctx context.Context, shortName string) (*Client, error) {
	c := &Client{http: &http.Client{Timeout: 30 * time.Second}}

	var account struct {
		AccessToken string `json:"access_token"`
	}
	if err := c.call(ctx, "createAccount", url.Values{"short_name": {shortName}}, &account); err != nil {
		return nil, err
	}
	if account.AccessToken == "" {
		return nil, fmt.Errorf("telegraph: no access token returned")
	}
	c.token = account.AccessToken
	return c, nil
}

// CreatePage publishes a new page.
func (c *Client) CreatePage(ctx context.Context, title, authorName string, content []Node) (Page, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return Page{}, err
	}
	params := url.Values{
		"access_token":   {c.token},
		"title":          {title},
		"author_name":    {authorName},
		"content":        {string(encoded)},
		"return_content": {"false"},
	}

	var page struct {
		URL  string `json:"url"`
		Path string `json:"path"`
	}
	if err := c.call(ctx, "createPage", params, &page); err != nil {
		return Page{}, err
	}
	return Page{URL: page.URL, Path: page.Path}, nil
}

// EditPage replaces the content of an existing page.
func (c *Client) EditPage(ctx context.Context, path, title, authorName string, content []Node) error {
	encoded, err := json.Marshal(content)
	if err != nil {
		return err
	}
	params := url.Values{
		"access_token":   {c.token},
		"title":          {title},
		"author_name":    {authorName},
		"content":        {string(encoded)},
		"return_content": {"false"},
	}
	return c.call(ctx, "editPage/"+path, params, nil)
}

func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/"+method,
		strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	var envelope struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("telegraph: %s: %w", method, err)
	}
	if !envelope.OK {
		return fmt.Errorf("telegraph: %s: %s", method, envelope.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}
