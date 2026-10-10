package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestListProtocol(t *testing.T) {
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/evs/v2/browseFolder" || r.Header.Get("x-auth-token") != "test-token" {
			t.Errorf("unexpected listing request")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("p") != "/folder with space/" || r.Form.Get("sync_acc") != "yes" || r.Form.Get("json") != "yes" || r.Form.Has("device_id") {
			t.Errorf("unexpected listing fields: %v", r.Form)
		}
		_, _ = io.WriteString(w, `{"message":"SUCCESS","path":"/folder with space/","contents":[{"name":"file.txt","is_dir":false,"size":"5","ver":"1","lmd":"2026/10/10 12:00:00"}]}`)
	}))
	defer server.Close()
	c, err := NewClient(server.URL, "test-token", server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := c.List(context.Background(), "/folder with space/")
	if err != nil || listing == nil || len(listing.Contents) != 1 || listing.Contents[0].Name != "file.txt" {
		t.Fatalf("unexpected listing: %v, %v", listing, err)
	}
}

func TestDownloadProtocol(t *testing.T) {
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/evs/v2/downloadFile" || r.URL.Query().Get("p") != "/a & ü.txt" || r.URL.Query().Get("x-auth-token") != "token & ?" || r.URL.Query().Get("version") != "0" {
			t.Error("unexpected download request")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "hello")
	}))
	defer server.Close()
	c, err := NewClient(server.URL, "token & ?", server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.Open(context.Background(), "/a & ü.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "hello" {
		t.Fatalf("unexpected download: %q, %v", data, err)
	}
}

func TestRejectedListing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		auth   bool
	}{
		{"invalid token", 200, `{"message":"ERROR","desc":"INVALID TOKEN"}`, true},
		{"forbidden", 403, `secret-value`, true},
		{"invalid JSON", 200, `secret-value`, false},
		{"missing status", 200, `{}`, false},
		{"server failure", 500, `secret-value`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			c, err := NewClient(server.URL, "secret-value", server.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.List(context.Background(), "/")
			if err == nil || strings.Contains(err.Error(), "secret-value") || errors.Is(err, ErrUnauthorized) != tc.auth {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDownloadDoesNotFollowRedirect(t *testing.T) {
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.example/?x-auth-token=secret-value", http.StatusFound)
	}))
	defer server.Close()
	c, err := NewClient(server.URL, "secret-value", server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Open(context.Background(), "/file")
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	for _, serverURL := range []string{"http://server.example", "https://user:pass@server.example", "https://server.example/path", "https://server.example?token=value", "https://server.example#fragment", "https:///"} {
		if _, err := NewClient(serverURL, "token", nil); err == nil {
			t.Errorf("accepted invalid origin %q", serverURL)
		}
	}
	if _, err := NewClient("https://server.example", "", nil); err == nil {
		t.Error("accepted empty token")
	}
}

func TestMutationProtocol(t *testing.T) {
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-auth-token") != "test-token" || r.Method != "POST" {
			t.Error("missing mutation authentication")
		}
		switch r.URL.Path {
		case "/evs/v2/createFolder":
			_ = r.ParseForm()
			if r.Form.Get("p") != "/parent" || r.Form.Get("foldername") != "child" || r.Form.Get("sync_acc") != "yes" {
				t.Error("invalid create fields")
			}
		case "/evs/v2/deleteFile":
			_ = r.ParseForm()
			if r.Form.Get("p") != "/parent/child" || r.Form.Get("trash") != "no" {
				t.Error("deletion must use normal Trash operation")
			}
		case "/evs/v2/upload":
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Fatal(err)
			}
			defer r.MultipartForm.RemoveAll()
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != "hello" || header.Filename != "test.txt" || r.FormValue("p") != "/parent" || r.FormValue("file_size") != "5" || r.FormValue("lmd") != "1700000000000" || r.FormValue("chunk") != "0" || r.FormValue("totalChunks") != "1" || r.FormValue("total_checksum") != "5d41402abc4b2a76b9719d911017c592" {
				t.Error("invalid upload fields")
			}
		default:
			t.Error("unexpected mutation endpoint")
		}
		_, _ = io.WriteString(w, `{"message":"SUCCESS"}`)
	}))
	c, err := NewClient(server.URL, "test-token", server)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CreateDirectory(context.Background(), "/parent", "child"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), "/parent/child"); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(context.Background(), "/parent/test.txt", strings.NewReader("hello"), 5, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestUploadRejectsInvalidSizeBeforeRequest(t *testing.T) {
	server := newTestServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected HTTP request") }))
	c, err := NewClient(server.URL, "test-token", server)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{-1, 1<<20 + 1, 4, 6} {
		if err := c.Upload(context.Background(), "/test.txt", strings.NewReader("hello"), size, time.Now()); err == nil {
			t.Errorf("accepted invalid upload size %d", size)
		}
	}
}

func TestChunkedUpload(t *testing.T) {
	data := strings.Repeat("abc", 1500000)
	var uploaded strings.Builder
	index := 0
	finalized := false
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/evs/v2/upload_session" {
			_ = r.ParseForm()
			if r.Form.Get("file_size") != "4500000" || r.Form.Get("file_name") != "large.bin" {
				t.Error("invalid session request")
			}
			_, _ = io.WriteString(w, `{"message":"SUCCESS","upload_session_id":"session-1"}`)
			return
		}
		if r.URL.Path != "/evs/v2/upload" {
			t.Fatal("unexpected upload path")
		}
		if err := r.ParseMultipartForm(3 << 20); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("upload_session_id") != "session-1" || r.FormValue("totalChunks") != "3" || r.FormValue("chunk") != strconv.Itoa(index) {
			t.Error("invalid chunk sequence")
		}
		if r.FormValue("close_session") == "true" {
			if index != 3 || r.FormValue("total_checksum") != checksum([]byte(data)) || len(r.MultipartForm.File) != 0 {
				t.Error("invalid finalization")
			}
			finalized = true
		} else {
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			n, _ := io.Copy(&uploaded, file)
			if strconv.FormatInt(n, 10) != r.FormValue("chunk_size") || n > 2000000 {
				t.Error("invalid chunk size")
			}
			index++
		}
		_, _ = io.WriteString(w, `{"message":"SUCCESS"}`)
	}))
	c, err := NewClient(server.URL, "test-token", server)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(context.Background(), "/large.bin", strings.NewReader(data), int64(len(data)), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !finalized || uploaded.String() != data {
		t.Error("chunked upload changed content or did not finalize")
	}
}

// testServer executes HTTP handlers in memory without opening a listener.
type testServer struct {
	URL     string
	handler http.Handler
}

func newTestServer(handler http.Handler) *testServer {
	return &testServer{URL: "https://test.example", handler: handler}
}
func (s *testServer) Close()               {}
func (s *testServer) Client() *http.Client { return &http.Client{Transport: s} }
func (s *testServer) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "test.example" {
		return nil, errors.New("unexpected redirect destination")
	}
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

func TestEmptyMissingDirectory(t *testing.T) {
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"message":"SUCCESS","contents":[]}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "token", server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(context.Background(), "/deleted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected missing directory: %v", err)
	}
	if _, err := client.List(context.Background(), "/"); err != nil {
		t.Fatalf("empty account root: %v", err)
	}
}
