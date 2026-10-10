package idrive

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/idrive/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fstest/fstests"
	"github.com/rclone/rclone/lib/encoder"
)

func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{RemoteName: "TestIdrive:", NilObject: (*Object)(nil)})
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestListingAndRange(t *testing.T) {
	client, err := api.NewClient("https://test.example", "token", testTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch r.URL.Path {
		case "/evs/v2/browseFolder":
			_ = r.ParseForm()
			if r.Form.Get("p") != "/root" {
				t.Error("listing escaped configured root")
			}
			_ = json.NewEncoder(w).Encode(api.Listing{Message: "SUCCESS", Path: "/root", Contents: []api.Entry{
				{Name: "file.txt", Size: "6", ModTimeUnix: "1791663287"},
				{Name: "folder", IsDir: true},
			}})
		case "/evs/v2/downloadFile":
			if r.URL.Query().Get("p") != "/root/file.txt" {
				t.Error("wrong download path")
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "abcdef")
		default:
			t.Error("unexpected request")
		}
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fs{name: "test", root: "root", client: client}
	f.features = (&fs.Features{}).Fill(context.Background(), f)
	entries, err := f.List(context.Background(), "")
	if err != nil || len(entries) != 2 {
		t.Fatalf("listing failed: %v", err)
	}
	o, err := f.NewObject(context.Background(), "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if o.Size() != 6 || o.ModTime(context.Background()).Unix() != 1791663287 {
		t.Error("incorrect file metadata")
	}
	if _, err := f.NewObject(context.Background(), "missing.txt"); err != fs.ErrorObjectNotFound {
		t.Errorf("wrong missing-file error: %v", err)
	}
	if _, err := f.NewObject(context.Background(), "folder"); err != fs.ErrorIsDir {
		t.Errorf("wrong directory error: %v", err)
	}
	body, err := o.Open(context.Background(), &fs.RangeOption{Start: 2, End: 4})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(data) != "cde" {
		t.Fatalf("invalid range: %q %v", data, err)
	}
	if _, err := f.List(context.Background(), "../outside"); err == nil {
		t.Error("allowed parent traversal")
	}
	if _, err := f.NewObject(context.Background(), "folder/../file.txt"); err == nil {
		t.Error("allowed object parent traversal")
	}
}

func TestConfiguration(t *testing.T) {
	f, err := NewFs(context.Background(), "test", "", configmap.Simple{"endpoint": "https://test.example", "token": obscure.MustObscure("secret")})
	if err != nil {
		t.Fatal(err)
	}
	if f.Name() != "test" || f.Root() != "" || strings.Contains(f.String(), "secret") {
		t.Error("invalid filesystem description")
	}
	if f.Precision() != fs.ModTimeNotSupported {
		t.Error("wrong timestamp precision")
	}
	if _, err := NewFs(context.Background(), "test", "../outside", configmap.Simple{"endpoint": "https://test.example", "token": obscure.MustObscure("secret")}); err == nil {
		t.Error("allowed root traversal")
	}
}

func TestFilenameEncoding(t *testing.T) {
	f := &Fs{root: "root", enc: defaultEncoding}
	for _, raw := range []string{" leading space", "trailing space ", ".hidden", "trailing.", "a?b%46.txt", "invalid utf-8\xfe", "middle.dot", "fullwidth．dot", "fullwidth．·pair", "slash/name"} {
		standard := encoder.Standard.Encode(raw)
		encoded, err := f.absolute(standard)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(encoded, "/root/")
		if name == raw {
			t.Errorf("restricted filename was not encoded: %q", raw)
		}
		if f.enc.ToStandardName(decodePeriods(name)) != standard {
			t.Errorf("filename did not round trip: %q", raw)
		}
	}
}

func TestFileExtensionsRemainIntact(t *testing.T) {
	f := &Fs{root: "root", enc: defaultEncoding}
	got, err := f.fileAbsolute("folder.with.dot/report.txt")
	if err != nil || got != "/root/folder．·with．·dot/report.txt" {
		t.Fatalf("unexpected encoded file path: %q %v", got, err)
	}
}

func TestNilObjectString(t *testing.T) {
	var o *Object
	if o.String() != "<nil>" {
		t.Fatal("nil object string")
	}
}

func TestDistinctUploadTimestamps(t *testing.T) {
	for _, previousEpoch := range []int64{1000000000, 4102444800} {
		uploaded := false
		client, err := api.NewClient("https://test.example", "token", testTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			switch r.URL.Path {
			case "/evs/v2/browseFolder":
				_ = json.NewEncoder(w).Encode(api.Listing{Message: "SUCCESS", Contents: []api.Entry{{Name: "same.txt", Size: "3", ModTimeUnix: strconv.FormatInt(previousEpoch, 10)}}})
			case "/evs/v2/upload":
				uploaded = true
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Fatal(err)
				}
				defer r.MultipartForm.RemoveAll()
				epochMillis, err := strconv.ParseInt(r.FormValue("lmd"), 10, 64)
				if err != nil || epochMillis/1000 <= previousEpoch {
					t.Error("upload timestamp did not advance")
				}
				_, _ = io.WriteString(w, `{"message":"SUCCESS"}`)
			case "/evs/v2/deleteFile":
				t.Error("overwrite unexpectedly deleted the existing file")
			default:
				t.Errorf("unexpected request: %s", r.URL.Path)
			}
			return w.Result(), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		f := &Fs{client: client, enc: defaultEncoding}
		o := &Object{f: f, remote: "same.txt"}
		src := object.NewStaticObjectInfo("same.txt", time.Unix(previousEpoch, 0), 3, true, nil, nil)
		if err := o.Update(context.Background(), strings.NewReader("bbb"), src); err != nil {
			t.Fatal(err)
		}
		if !uploaded {
			t.Error("replacement was not uploaded")
		}
	}
}

func TestAuthorizationConfiguration(t *testing.T) {
	ctx := context.Background()
	m := configmap.Simple{}
	out, err := configure(ctx, "test", m, fs.ConfigIn{})
	if err != nil || out.State != "auth_mode" || out.Option.Name != "config_auth_mode" {
		t.Fatal("missing authorization choice")
	}
	out, err = configure(ctx, "test", m, fs.ConfigIn{State: "browser_open", Result: "false"})
	if err != nil || !out.Option.IsPassword || out.Option.Name != "config_access_token" {
		t.Fatal("browser token prompt is not masked/ephemeral")
	}
	out, err = configure(ctx, "test", m, fs.ConfigIn{State: "browser_token", Result: obscure.MustObscure("invalid-token")})
	if err != nil || out.Error == "" || out.State != "choose_auth" || len(m) != 0 {
		t.Error("failed token import persisted credentials")
	}
	_, err = configure(ctx, "test", m, fs.ConfigIn{State: "manual_endpoint", Result: "https://test.example"})
	if err != nil {
		t.Fatal(err)
	}
	m.Set("access_token", obscure.MustObscure("old-browser-token"))
	_, err = configure(ctx, "test", m, fs.ConfigIn{State: "manual_token", Result: obscure.MustObscure("manual-token")})
	if err != nil || m["access_token"] != "" || obscure.MustReveal(m["token"]) != "manual-token" {
		t.Error("manual reconnect retained browser authorization")
	}
}

func TestBrowserConfigurationLive(t *testing.T) {
	token := os.Getenv("RCLONE_TEST_IDRIVE_ACCESS_TOKEN")
	if token == "" {
		t.Skip("set RCLONE_TEST_IDRIVE_ACCESS_TOKEN for browser authorization validation")
	}
	m := configmap.Simple{}
	out, err := configure(context.Background(), "browser-test", m, fs.ConfigIn{State: "browser_token", Result: obscure.MustObscure(token)})
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("authorization did not finish: %s", out.Error)
	}
	if m["access_token"] == "" || m["token"] == "" || m["endpoint"] == "" {
		t.Fatal("session was not persisted")
	}
	if _, ok := m["config_access_token"]; ok {
		t.Fatal("ephemeral input persisted")
	}
	f, err := NewFs(context.Background(), "browser-test", "", m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.List(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}
