// Package api implements the observed IDrive Cloud Drive web protocol.
package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// ErrUnauthorized indicates that the Cloud Drive session needs renewal.
var ErrUnauthorized = errors.New("idrive: session expired or unauthorized")

// ErrNotFound indicates a missing remote file or directory.
var ErrNotFound = errors.New("idrive: path not found")

// Client accesses Cloud Drive with an existing web session token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// Entry is a file or directory returned by the Cloud Drive listing endpoint.
type Entry struct {
	Name        string `json:"name"`
	IsDir       bool   `json:"is_dir"`
	Size        string `json:"size"`
	Version     string `json:"ver"`
	ModTime     string `json:"lmd"`
	ModTimeUnix string `json:"lmd_web"`
}

// CreateDirectory creates one directory below an existing parent.
func (c *Client) CreateDirectory(ctx context.Context, parent, name string) error {
	return c.form(ctx, "/createFolder", url.Values{"p": {parent}, "foldername": {name}, "json": {"yes"}, "sync_acc": {"yes"}, "device_id": {""}})
}

// Delete moves a file or directory to IDrive Trash.
func (c *Client) Delete(ctx context.Context, remotePath string) error {
	return c.form(ctx, "/deleteFile", url.Values{"p": {remotePath}, "json": {"yes"}, "trash": {"no"}, "device_id": {""}})
}

// Upload transfers a known-size file using the web console's chunk protocol.
func (c *Client) Upload(ctx context.Context, remotePath string, in io.Reader, size int64, modTime time.Time) error {
	if size < 0 {
		return errors.New("idrive: uploads require a known size")
	}
	if size <= 1<<20 {
		data, err := io.ReadAll(io.LimitReader(in, size+1))
		if err != nil || int64(len(data)) != size {
			return errors.New("idrive: upload input does not match declared size")
		}
		return c.uploadPart(ctx, remotePath, data, size, modTime, map[string]string{"chunk": "0", "totalChunks": "1", "total_checksum": checksum(data)})
	}
	const chunkSize = int64(2000000)
	chunks := (size-1)/chunkSize + 1
	hash := md5.New()
	var session string
	for index, offset := int64(0), int64(0); offset < size; index++ {
		data := make([]byte, min(chunkSize, size-offset))
		if _, err := io.ReadFull(in, data); err != nil {
			return errors.New("idrive: upload input does not match declared size")
		}
		if session == "" {
			var err error
			session, err = c.uploadSession(ctx, path.Base(remotePath), size)
			if err != nil {
				return err
			}
		}
		_, _ = hash.Write(data)
		fields := map[string]string{"chunk": strconv.FormatInt(index, 10), "totalChunks": strconv.FormatInt(chunks, 10), "chunk_size": strconv.Itoa(len(data)), "upload_session_id": session}
		if err := c.uploadPart(ctx, remotePath, data, size, modTime, fields); err != nil {
			return err
		}
		offset += int64(len(data))
	}
	extra, err := io.ReadAll(io.LimitReader(in, 1))
	if err != nil || len(extra) != 0 {
		return errors.New("idrive: upload input exceeds declared size")
	}
	return c.uploadPart(ctx, remotePath, nil, size, modTime, map[string]string{"chunk": strconv.FormatInt(chunks, 10), "totalChunks": strconv.FormatInt(chunks, 10), "upload_session_id": session, "close_session": "true", "total_checksum": hex.EncodeToString(hash.Sum(nil))})
}

func (c *Client) uploadSession(ctx context.Context, name string, size int64) (string, error) {
	values := url.Values{"file_name": {name}, "file_size": {strconv.FormatInt(size, 10)}}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/evs/v2/upload_session", strings.NewReader(values.Encode()))
	if err != nil {
		return "", errors.New("idrive: could not construct session request")
	}
	req.Header.Set("x-auth-token", c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	resp, err := c.request(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var reply struct {
		Message, Desc string
		Session       string `json:"upload_session_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply); err != nil {
		return "", errors.New("idrive: invalid upload session response")
	}
	if reply.Message != "SUCCESS" {
		return "", replyError(reply.Desc)
	}
	if reply.Session == "" {
		return "", errors.New("idrive: upload session identifier missing")
	}
	return reply.Session, nil
}

func (c *Client) uploadPart(ctx context.Context, remotePath string, data []byte, size int64, modTime time.Time, fields map[string]string) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if data != nil {
		part, err := w.CreateFormFile("file", path.Base(remotePath))
		if err != nil {
			return errors.New("idrive: invalid upload filename")
		}
		if _, err = part.Write(data); err != nil {
			return err
		}
	}
	fields["p"] = path.Dir(remotePath)
	fields["json"] = "yes"
	fields["lmd"] = strconv.FormatInt(modTime.UnixMilli(), 10)
	fields["file_size"] = strconv.FormatInt(size, 10)
	fields["file_name"] = path.Base(remotePath)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/evs/v2/upload", &body)
	if err != nil {
		return errors.New("idrive: could not construct upload request")
	}
	req.Header.Set("x-auth-token", c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.request(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkReply(resp.Body)
}

func (c *Client) form(ctx context.Context, endpoint string, values url.Values) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/evs/v2"+endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return errors.New("idrive: could not construct operation request")
	}
	req.Header.Set("x-auth-token", c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	resp, err := c.request(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkReply(resp.Body)
}

func checkReply(body io.Reader) error {
	var reply struct{ Message, Desc string }
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&reply); err != nil {
		return errors.New("idrive: invalid operation response")
	}
	if reply.Message == "SUCCESS" {
		return nil
	}
	return replyError(reply.Desc)
}

// Listing contains a directory's entries and the server's reported path.
type Listing struct {
	Message  string  `json:"message"`
	Desc     string  `json:"desc"`
	Path     string  `json:"path"`
	Contents []Entry `json:"contents"`
}

// NewClient validates an HTTPS server URL and uses the supplied HTTP transport.
// Redirects are rejected to prevent session tokens reaching another endpoint.
func NewClient(serverURL, token string, transport http.RoundTripper) (*Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return nil, errors.New("idrive: server must be an HTTPS origin without credentials, path, query, or fragment")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("idrive: a valid Cloud Drive token is required")
	}
	return &Client{
		baseURL: strings.TrimRight(serverURL, "/"),
		token:   token,
		http: &http.Client{
			Transport: transport,
			Timeout:   2 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("idrive: redirects are disabled")
			},
		},
	}, nil
}

// List reads a Cloud Drive directory without modifying it.
func (c *Client) List(ctx context.Context, remotePath string) (*Listing, error) {
	values := url.Values{"p": {remotePath}, "json": {"yes"}, "sync_acc": {"yes"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/evs/v2/browseFolder", strings.NewReader(values.Encode()))
	if err != nil {
		return nil, errors.New("idrive: could not construct listing request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("x-auth-token", c.token)
	resp, err := c.request(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	const maxListingSize = 32 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxListingSize+1))
	if err != nil || len(data) > maxListingSize {
		return nil, errors.New("idrive: listing response failed or exceeded 32 MiB")
	}
	var listing Listing
	if err = json.Unmarshal(data, &listing); err != nil {
		return nil, errors.New("idrive: invalid listing response")
	}
	if listing.Message != "SUCCESS" {
		return nil, replyError(listing.Desc)
	}
	// The server can report SUCCESS with no entries after a directory is deleted.
	// Verify empty non-root directories against their parent listing.
	if len(listing.Contents) == 0 && path.Clean(remotePath) != "/" {
		parent, err := c.List(ctx, path.Dir(remotePath))
		if err != nil {
			return nil, err
		}
		found := false
		for _, entry := range parent.Contents {
			if entry.Name == path.Base(remotePath) && entry.IsDir {
				found = true
				break
			}
		}
		if !found {
			return nil, ErrNotFound
		}
	}
	return &listing, nil
}

// Open downloads the current version of a Cloud Drive file.
// The caller must close the returned body. The observed web protocol uses a
// query token; request URLs must never be logged.
func (c *Client) Open(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	values := url.Values{"p": {remotePath}, "version": {"0"}, "x-auth-token": {c.token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/evs/v2/downloadFile?"+values.Encode(), nil)
	if err != nil {
		return nil, errors.New("idrive: could not construct download request")
	}
	resp, err := c.request(req)
	if err != nil {
		return nil, err
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if resp.Header.Get("Content-Disposition") == "" && strings.Contains(contentType, "application/json") {
		defer resp.Body.Close()
		if err := checkReply(resp.Body); err != nil {
			return nil, err
		}
		return nil, errors.New("idrive: download returned an API response instead of a file")
	}
	if resp.Header.Get("Content-Disposition") == "" && strings.Contains(contentType, "text/html") {
		_ = resp.Body.Close()
		return nil, errors.New("idrive: download returned an HTML page instead of a file")
	}
	return resp.Body, nil
}

func (c *Client) request(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		// net/http errors include the request URL, which may contain the token.
		return nil, errors.New("idrive: HTTP request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, ErrUnauthorized
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("idrive: HTTP status %d", resp.StatusCode)
	}
	return resp, nil
}

func replyError(desc string) error {
	upper := strings.ToUpper(desc)
	if strings.Contains(upper, "INVALID TOKEN") {
		return ErrUnauthorized
	}
	if strings.Contains(upper, "INVALID PATH") || strings.Contains(upper, "NO SUCH FILE") || strings.Contains(upper, "NOT FOUND") {
		return ErrNotFound
	}
	return errors.New("idrive: operation rejected by server")
}

func checksum(data []byte) string { sum := md5.Sum(data); return hex.EncodeToString(sum[:]) }
