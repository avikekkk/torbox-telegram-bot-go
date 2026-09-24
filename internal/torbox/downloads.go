package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

// Link is a download TorBox knows about, with its CDN URL when one is ready.
type Link struct {
	Kind string
	ID   int64
	Name string
	// URL is empty while TorBox is still fetching an uncached download.
	URL string
}

// endpoints are the per-library API paths.
type endpoints struct {
	list, request, requestID, control string
}

var paths = map[string]endpoints{
	KindTorrent: {"/api/torrents/mylist", "/api/torrents/requestdl", "torrent_id", "/api/torrents/controltorrent"},
	KindUsenet:  {"/api/usenet/mylist", "/api/usenet/requestdl", "usenet_id", "/api/usenet/controlusenetdownload"},
	KindWebDL:   {"/api/webdl/mylist", "/api/webdl/requestdl", "web_id", "/api/webdl/controlwebdownload"},
}

// createdIDKeys are the fields each create endpoint returns the new ID in.
var createdIDKeys = map[string][]string{
	KindTorrent: {"torrent_id", "id"},
	KindUsenet:  {"usenetdownload_id", "usenet_id", "id"},
	KindWebDL:   {"webdownload_id", "webdl_id", "id"},
}

// AddTorrent adds a magnet, or a .torrent file when file is not nil, then asks
// for its link. A cached torrent links at once; an uncached one has no URL yet,
// which is not an error.
func (c *Client) AddTorrent(ctx context.Context, magnet, name string, file []byte, filename string) (*Link, error) {
	if magnet == "" && file == nil {
		return nil, errorf(0, "magnet or torrent file is required to create a torrent")
	}
	form := newForm()
	form.field("magnet", magnet)
	form.field("name", name)
	form.file(filename, file, "application/x-bittorrent")
	return c.add(ctx, KindTorrent, "/api/torrents/createtorrent", form, name)
}

// AddUsenet uploads an NZB file.
func (c *Client) AddUsenet(ctx context.Context, nzb []byte, filename, name string) (*Link, error) {
	if nzb == nil {
		return nil, errorf(0, "an NZB file is required for a usenet download")
	}
	form := newForm()
	form.field("name", name)
	form.field("post_processing", "-1")
	form.file(filename, nzb, "application/x-nzb")
	return c.add(ctx, KindUsenet, "/api/usenet/createusenetdownload", form, name)
}

// AddWeb adds a hoster or direct URL.
func (c *Client) AddWeb(ctx context.Context, link string) (*Link, error) {
	if link == "" {
		return nil, errorf(0, "link is required to create a web download")
	}
	body := url.Values{"link": {link}}.Encode()
	return c.addBody(ctx, KindWebDL, call{
		method:      http.MethodPost,
		path:        "/api/webdl/createwebdownload",
		body:        []byte(body),
		contentType: "application/x-www-form-urlencoded",
		creates:     true,
	}, "")
}

func (c *Client) add(ctx context.Context, kind, path string, form *form, name string) (*Link, error) {
	body, contentType, err := form.encode()
	if err != nil {
		return nil, err
	}
	return c.addBody(ctx, kind, call{
		method:      http.MethodPost,
		path:        path,
		body:        body,
		contentType: contentType,
		creates:     true,
	}, name)
}

func (c *Client) addBody(ctx context.Context, kind string, req call, name string) (*Link, error) {
	if err := c.quota.check(kind); err != nil {
		return nil, err
	}
	result, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	c.quota.record(kind)

	data, _ := result.data().(map[string]any)
	var id int64
	found := false
	for _, key := range createdIDKeys[kind] {
		if id, found = toInt(data[key]); found {
			break
		}
	}
	if !found {
		return nil, errorf(0, "TorBox accepted the %s download but returned no ID", kind)
	}

	link := &Link{Kind: kind, ID: id, Name: firstNonEmpty(toString(data["name"]), name)}
	// The order matters: requestdl only knows a download once it exists. A
	// failure here is expected for anything uncached, so it only leaves the
	// URL empty.
	link.URL, _ = c.RequestLink(ctx, kind, id, nil, true)
	return link, nil
}

// RequestLink asks for a CDN URL. zip bundles every file into one archive;
// otherwise fileID picks a single file.
func (c *Client) RequestLink(ctx context.Context, kind string, id int64, fileID *int64, zip bool) (string, error) {
	endpoint, ok := paths[kind]
	if !ok {
		return "", errorf(0, "Unknown kind '%s'. Use torrent|usenet|web.", kind)
	}
	query := url.Values{
		"token":            {c.apiKey},
		endpoint.requestID: {strconv.FormatInt(id, 10)},
	}
	if fileID != nil {
		query.Set("file_id", strconv.FormatInt(*fileID, 10))
	}
	if zip {
		query.Set("zip_link", "true")
	}
	result, err := c.do(ctx, call{method: http.MethodGet, path: endpoint.request, query: query})
	if err != nil {
		return "", err
	}
	return extractURL(result.data()), nil
}

func extractURL(data any) string {
	switch value := data.(type) {
	case string:
		return value
	case map[string]any:
		return firstNonEmpty(toString(value["url"]), toString(value["download_url"]), toString(value["link"]))
	}
	return ""
}

// Get fetches one download by ID, bypassing TorBox's cached list.
func (c *Client) Get(ctx context.Context, kind string, id int64) (*Item, error) {
	endpoint, ok := paths[kind]
	if !ok {
		return nil, errorf(0, "Unknown kind '%s'. Use torrent|usenet|web.", kind)
	}
	query := url.Values{"bypass_cache": {"true"}, "id": {strconv.FormatInt(id, 10)}}
	result, err := c.do(ctx, call{method: http.MethodGet, path: endpoint.list, query: query})
	if err != nil {
		return nil, err
	}
	switch data := result.data().(type) {
	case map[string]any:
		item := parseItem(data, kind)
		return &item, nil
	case []any:
		if len(data) > 0 {
			if object, ok := data[0].(map[string]any); ok {
				item := parseItem(object, kind)
				return &item, nil
			}
		}
	}
	return nil, errorf(http.StatusNotFound, "No %s download #%d found.", kind, id)
}

// IsNotFound reports whether err says the download does not exist.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// list fetches one page of a library.
func (c *Client) list(ctx context.Context, kind string, offset, limit int) ([]Item, error) {
	query := url.Values{
		"bypass_cache": {"true"},
		"offset":       {strconv.Itoa(offset)},
		"limit":        {strconv.Itoa(limit)},
	}
	result, err := c.do(ctx, call{method: http.MethodGet, path: paths[kind].list, query: query})
	if err != nil {
		return nil, err
	}
	return parseItems(result.data(), kind), nil
}

func parseItems(data any, kind string) []Item {
	entries, _ := data.([]any)
	items := make([]Item, 0, len(entries))
	for _, entry := range entries {
		if object, ok := entry.(map[string]any); ok {
			items = append(items, parseItem(object, kind))
		}
	}
	return items
}

// listPageSize and listMaxPages bound a full library walk.
const (
	listPageSize = 100
	listMaxPages = 50
)

// ErrTooManyItems means a library did not end within listMaxPages pages.
var ErrTooManyItems = errors.New("too many TorBox items to count safely")

// ListAll walks every page of a library, so queued items past the first page
// still show. On error it returns what it collected so far alongside it.
func (c *Client) ListAll(ctx context.Context, kind string) ([]Item, error) {
	var all []Item
	seen := map[int64]bool{}
	for page := 0; page < listMaxPages; page++ {
		items, err := c.list(ctx, kind, page*listPageSize, listPageSize)
		if err != nil {
			return all, err
		}
		fresh := 0
		for _, item := range items {
			if !seen[item.ID] {
				seen[item.ID] = true
				all = append(all, item)
				fresh++
			}
		}
		// A short page is the last one. A page of nothing new means TorBox is
		// ignoring the offset, which would otherwise loop to the page limit.
		if len(items) < listPageSize || fresh == 0 {
			return all, nil
		}
	}
	return all, ErrTooManyItems
}

// Queued lists downloads waiting in TorBox's central queue, of every kind.
func (c *Client) Queued(ctx context.Context) ([]Item, error) {
	result, err := c.do(ctx, call{method: http.MethodGet, path: "/api/queued/getqueued"})
	if err != nil {
		return nil, err
	}
	entries, _ := result.data().([]any)
	items := make([]Item, 0, len(entries))
	for _, entry := range entries {
		object, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		kind := KindTorrent
		switch typ := strings.ToLower(strings.TrimSpace(toString(object["type"]))); {
		case strings.Contains(typ, "usenet"), strings.Contains(typ, "nzb"):
			kind = KindUsenet
		case strings.Contains(typ, "web"):
			kind = KindWebDL
		}
		item := parseItem(object, kind)
		item.Name = firstNonEmpty(toString(object["name"]), toString(object["name_override"]), "unknown")
		item.State = "queued"
		if !item.HasProgress {
			item.Progress, item.HasProgress = 0, true
		}
		items = append(items, item)
	}
	return items, nil
}

// CollectionQueued is the central queue, the fourth thing /purge empties.
const CollectionQueued = "queued"

// DeleteAll permanently deletes every download in one collection: a library
// kind or CollectionQueued. TorBox finishes the deletion in the background.
func (c *Client) DeleteAll(ctx context.Context, collection string) error {
	path := "/api/queued/controlqueued"
	if collection != CollectionQueued {
		endpoint, ok := paths[collection]
		if !ok {
			return fmt.Errorf("unknown TorBox collection: %s", collection)
		}
		path = endpoint.control
	}
	body, err := json.Marshal(map[string]any{"operation": "delete", "all": true})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, call{method: http.MethodPost, path: path, body: body, contentType: "application/json"})
	return err
}

// Account is the part of /api/user/me the bot shows.
type Account struct {
	Plan           string
	PremiumExpires string
}

// planNames maps TorBox's numeric plan tiers to their names.
var planNames = map[string]string{"0": "Free", "1": "Essential", "2": "Pro", "3": "Standard"}

// Me fetches the account the API key belongs to.
func (c *Client) Me(ctx context.Context) (*Account, error) {
	result, err := c.do(ctx, call{method: http.MethodGet, path: "/api/user/me"})
	if err != nil {
		return nil, err
	}
	data, _ := result.data().(map[string]any)
	plan := toString(data["plan"])
	if name, ok := planNames[plan]; ok {
		plan = name
	}
	return &Account{Plan: plan, PremiumExpires: toString(data["premium_expires_at"])}, nil
}

// form builds a multipart body, the encoding the official TorBox SDK uses for
// the create endpoints that take a file.
type form struct {
	buf    bytes.Buffer
	writer *multipart.Writer
	err    error
}

func newForm() *form {
	f := &form{}
	f.writer = multipart.NewWriter(&f.buf)
	return f
}

// field adds a text field, skipping empty values.
func (f *form) field(name, value string) {
	if f.err != nil || value == "" {
		return
	}
	f.err = f.writer.WriteField(name, value)
}

// file adds the upload, if there is one.
func (f *form) file(filename string, content []byte, contentType string) {
	if f.err != nil || content == nil {
		return
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	header.Set("Content-Type", contentType)
	part, err := f.writer.CreatePart(header)
	if err != nil {
		f.err = err
		return
	}
	_, f.err = part.Write(content)
}

func (f *form) encode() ([]byte, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	if err := f.writer.Close(); err != nil {
		return nil, "", err
	}
	return f.buf.Bytes(), f.writer.FormDataContentType(), nil
}
