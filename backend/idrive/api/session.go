package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Session manages a browser-authorized web session and Cloud Drive tokens.
type Session struct {
	mu                      sync.Mutex
	access, drive, endpoint string
	http                    *http.Client
	save                    func(access, drive, endpoint string)
}

var driveHost = regexp.MustCompile(`^evssyncweb[0-9]+\.idrive\.com$`)

// NewSession creates a session from a browser access token. Save must not log credentials.
func NewSession(access string, transport http.RoundTripper, save func(string, string, string)) (*Session, error) {
	if !validToken(access) {
		return nil, errors.New("idrive: invalid browser access token")
	}
	endpoint, err := sessionEndpoint(access)
	if err != nil {
		return nil, err
	}
	return &Session{access: access, endpoint: endpoint, save: save, http: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("idrive: authentication redirects disabled")
	}}}, nil
}

func validToken(token string) bool {
	return strings.TrimSpace(token) != "" && !strings.ContainsAny(token, "\r\n")
}

func sessionEndpoint(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("idrive: browser access token must be a JWT")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("idrive: invalid browser token claims")
	}
	var claims struct {
		Server     string `json:"driveServerAddress"`
		Encryption string `json:"enc_type"`
		Validated  bool   `json:"isPvtKeyValidated"`
	}
	if json.Unmarshal(data, &claims) != nil {
		return "", errors.New("idrive: invalid browser token claims")
	}
	if claims.Encryption == "PRIVATE" && !claims.Validated {
		return "", errors.New("idrive: unlock Cloud Drive with your private key in the browser before authorizing")
	}
	host := claims.Server
	if strings.HasPrefix(host, "https://") {
		u, e := url.Parse(host)
		if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return "", errors.New("idrive: invalid Cloud Drive server")
		}
		host = u.Host
	}
	host = strings.Replace(host, "evsweb", "evssyncweb", 1)
	if strings.HasSuffix(host, ".idrivesync.com") {
		host = strings.TrimSuffix(host, ".idrivesync.com") + ".idrive.com"
	}
	if !driveHost.MatchString(host) {
		return "", errors.New("idrive: unsupported Cloud Drive server in browser token")
	}
	return "https://" + host, nil
}

// Credentials obtains a Cloud Drive token, or reuses a token another request renewed.
func (s *Session) Credentials(ctx context.Context, failed string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drive != "" && (failed == "" || failed != s.drive) {
		return s.endpoint, s.drive, nil
	}
	for attempt := 0; attempt < 5; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.idrive.com/auth/authenticate/cloud/token/drive", nil)
		req.Header.Set("Authorization", "Bearer "+s.access)
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", "", ctx.Err()
			}
			return "", "", errors.New("idrive: browser session renewal request failed")
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		_ = resp.Body.Close()
		if readErr != nil || len(data) > 1<<20 {
			return "", "", errors.New("idrive: invalid session renewal response")
		}
		rotated := resp.Header.Get("Authorization")
		if rotated == "" {
			rotated = resp.Header.Get("x-reverse-access-tkn")
		}
		rotated = strings.TrimPrefix(rotated, "Bearer ")
		changed := rotated != "" && rotated != s.access
		if changed {
			endpoint, e := sessionEndpoint(rotated)
			if e != nil {
				return "", "", e
			}
			s.access = rotated
			s.endpoint = endpoint
			if s.save != nil {
				s.save(s.access, s.drive, s.endpoint)
			}
		}
		if resp.StatusCode == 425 && resp.Header.Get("x-refresh-status") == "in-progress" || resp.StatusCode == 410 && resp.Header.Get("x-active-status") == "stale-token" {
			timer := time.NewTimer(time.Duration(1<<attempt) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", "", ctx.Err()
			case <-timer.C:
			}
			continue
		}
		// The web client reissues the request after access-token rotation.
		if changed && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 410 || resp.StatusCode == 451 {
				return "", "", errors.New("idrive: browser authorization expired; run rclone config reconnect remote:")
			}
			return "", "", fmt.Errorf("idrive: session renewal HTTP status %d", resp.StatusCode)
		}
		var reply struct {
			Drive string `json:"driveToken"`
		}
		if json.Unmarshal(data, &reply) != nil || !validToken(reply.Drive) {
			return "", "", errors.New("idrive: Cloud Drive token missing from renewal response")
		}
		s.drive = reply.Drive
		if s.save != nil {
			s.save(s.access, s.drive, s.endpoint)
		}
		return s.endpoint, s.drive, nil
	}
	return "", "", errors.New("idrive: browser session renewal did not complete; reconnect this remote")
}
