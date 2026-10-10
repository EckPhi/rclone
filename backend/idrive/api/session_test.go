package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sessionTransport func(*http.Request) (*http.Response, error)

func (f sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testAccess(server string) string {
	data, _ := json.Marshal(map[string]any{"driveServerAddress": server})
	return "header." + base64.RawURLEncoding.EncodeToString(data) + ".signature"
}
func jsonResponse(body string, status int) *http.Response {
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
	return w.Result()
}

func TestBrowserSessionRenewalAndReplay(t *testing.T) {
	for _, operation := range []string{"list", "download", "multipart"} {
		t.Run(operation, func(t *testing.T) {
			var renewals, calls int
			var firstBody string
			var savedAccess, savedDrive, savedEndpoint string
			access := testAccess("evsweb63.idrivesync.com")
			rotated := testAccess("evssyncweb64.idrive.com")
			transport := sessionTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "www.idrive.com" {
					renewals++
					if r.Header.Get("Authorization") != "Bearer "+access && r.Header.Get("Authorization") != "Bearer "+rotated {
						t.Error("wrong bearer credential")
					}
					resp := jsonResponse(`{"driveToken":"fresh"}`, 200)
					if renewals == 1 {
						resp.Header.Set("x-reverse-access-tkn", rotated)
						return jsonResponse(`{"driveToken":"stale"}`, 200), nil
					}
					resp.Header.Set("x-reverse-access-tkn", rotated)
					return resp, nil
				}
				calls++
				token := r.Header.Get("x-auth-token")
				if r.URL.Query().Has("x-auth-token") {
					token = r.URL.Query().Get("x-auth-token")
				}
				if r.Body != nil {
					b, _ := io.ReadAll(r.Body)
					_ = r.Body.Close()
					if calls == 1 {
						firstBody = string(b)
					} else if firstBody != string(b) {
						t.Error("request body not replayed exactly")
					}
				}
				if calls == 1 {
					if token != "stale" {
						t.Error("missing initial token")
					}
					return jsonResponse(`{"message":"ERROR","desc":"INVALID TOKEN"}`, 200), nil
				}
				if token != "fresh" || r.URL.Host != "evssyncweb64.idrive.com" {
					t.Error("renewal did not replace token and endpoint")
				}
				if operation == "download" {
					resp := jsonResponse("payload", 200)
					resp.Header.Set("Content-Disposition", "attachment")
					return resp, nil
				}
				return jsonResponse(`{"message":"SUCCESS","contents":[]}`, 200), nil
			})
			session, err := NewSession(access, transport, func(a, d, e string) { savedAccess, savedDrive, savedEndpoint = a, d, e })
			if err != nil {
				t.Fatal(err)
			}
			endpoint, token, err := session.Credentials(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			client, _ := NewClient(endpoint, token, transport)
			client.UseSession(session)
			switch operation {
			case "list":
				_, err = client.List(context.Background(), "/")
			case "download":
				var body io.ReadCloser
				body, err = client.Open(context.Background(), "/file")
				if err == nil {
					b, _ := io.ReadAll(body)
					_ = body.Close()
					if string(b) != "payload" {
						t.Error("download corrupted")
					}
				}
			case "multipart":
				err = client.Upload(context.Background(), "/file", strings.NewReader("abc"), 3, time.Now())
			}
			if err != nil {
				t.Fatal(err)
			}
			if renewals != 3 || calls != 2 || savedAccess != rotated || savedDrive != "fresh" || savedEndpoint != "https://evssyncweb64.idrive.com" {
				t.Error("unexpected session state")
			}
		})
	}
}

func TestConcurrentSessionRenewal(t *testing.T) {
	var requests atomic.Int32
	s, err := NewSession(testAccess("evssyncweb63.idrive.com"), sessionTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return jsonResponse(`{"driveToken":"fresh"}`, 200), nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.drive = "expired"
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, token, err := s.Credentials(context.Background(), "expired")
			if err != nil || token != "fresh" {
				t.Error("concurrent renewal failed")
			}
		})
	}
	wg.Wait()
	if requests.Load() != 1 {
		t.Error("renewal storm")
	}
}

func TestSessionFailureBoundsAndSanitization(t *testing.T) {
	secret := testAccess("evssyncweb63.idrive.com")
	for _, status := range []int{401, 403, 451, 200} {
		s, _ := NewSession(secret, sessionTransport(func(r *http.Request) (*http.Response, error) {
			return jsonResponse(`{"secret":"do-not-print"}`, status), nil
		}), nil)
		_, _, err := s.Credentials(context.Background(), "")
		if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "do-not-print") {
			t.Error("invalid or unsafe error")
		}
	}
	s, _ := NewSession(secret, sessionTransport(func(r *http.Request) (*http.Response, error) { return nil, errors.New(secret) }), nil)
	_, _, err := s.Credentials(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Error("network error exposed token")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, _ = NewSession(secret, sessionTransport(func(r *http.Request) (*http.Response, error) { return nil, ctx.Err() }), nil)
	if _, _, err = s.Credentials(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Error("cancellation lost")
	}
	var count int
	transport := sessionTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "www.idrive.com" {
			return jsonResponse(`{"driveToken":"fresh"}`, 200), nil
		}
		count++
		return jsonResponse(`{"desc":"INVALID TOKEN"}`, 403), nil
	})
	s, _ = NewSession(secret, transport, nil)
	c, _ := NewClient("https://evssyncweb63.idrive.com", "old", transport)
	c.UseSession(s)
	if _, err = c.List(context.Background(), "/"); !errors.Is(err, ErrUnauthorized) || count != 2 {
		t.Error("authentication retry not bounded")
	}
}

func TestServerValidation(t *testing.T) {
	for _, server := range []string{"evil.example", "https://evssyncweb63.idrive.com@evil.example", "https://evssyncweb63.idrive.com:443", "https://evssyncweb63.idrive.com/path", "evssyncweb63.idrive.com.evil.example"} {
		if _, err := NewSession(testAccess(server), nil, nil); err == nil {
			t.Errorf("accepted untrusted server %s", server)
		}
	}
}

func TestAccessRotationBackoffAndPersistence(t *testing.T) {
	access := testAccess("evssyncweb63.idrive.com")
	rotated := testAccess("evssyncweb64.idrive.com")
	calls, saves := 0, 0
	s, _ := NewSession(access, sessionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			resp := jsonResponse(`{}`, 410)
			resp.Header.Set("x-active-status", "stale-token")
			resp.Header.Set("Authorization", rotated)
			return resp, nil
		}
		if r.Header.Get("Authorization") != "Bearer "+rotated {
			t.Error("rotated access token not used")
		}
		return jsonResponse(`{"driveToken":"fresh"}`, 200), nil
	}), func(a, d, e string) {
		if a != rotated || e != "https://evssyncweb64.idrive.com" {
			t.Error("rotation not persisted")
		}
		saves++
	})
	endpoint, drive, err := s.Credentials(context.Background(), "")
	if err != nil || drive != "fresh" || endpoint != "https://evssyncweb64.idrive.com" || calls != 2 || saves != 2 {
		t.Error("rotation flow failed")
	}
	calls = 0
	s, _ = NewSession(access, sessionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		resp := jsonResponse(`{}`, 425)
		resp.Header.Set("x-refresh-status", "in-progress")
		return resp, nil
	}), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err = s.Credentials(ctx, ""); !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Error("backoff did not honor cancellation")
	}
}

func TestPrivateKeyMustBeUnlocked(t *testing.T) {
	for _, validated := range []bool{false, true} {
		data, _ := json.Marshal(map[string]any{"driveServerAddress": "evssyncweb63.idrive.com", "enc_type": "PRIVATE", "isPvtKeyValidated": validated})
		_, err := NewSession("header."+base64.RawURLEncoding.EncodeToString(data)+".signature", nil, nil)
		if validated == (err != nil) {
			t.Error("private key validation gate incorrect")
		}
	}
}
