package baidu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(value any) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}

func TestDeviceAuthorizationProtocol(t *testing.T) {
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		switch r.URL.Path {
		case "/oauth/2.0/device/code":
			if r.Method != http.MethodGet || r.URL.Query().Get("client_id") != "app-key" {
				t.Errorf("unexpected device request: %s %s", r.Method, r.URL.RawQuery)
			}
			return jsonResponse(map[string]any{"device_code": "device-code", "user_code": "ABCD-EFGH", "verification_url": "https://example.test/verify", "expires_in": 600, "interval": 1}), nil
		case "/oauth/2.0/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			want := url.Values{"grant_type": {"device_token"}, "code": {"device-code"}, "client_id": {"app-key"}, "client_secret": {"secret-key"}}
			for key := range want {
				if r.Form.Get(key) != want.Get(key) {
					t.Errorf("%s=%q want %q", key, r.Form.Get(key), want.Get(key))
				}
			}
			return jsonResponse(map[string]any{"access_token": "access", "refresh_token": "refresh", "expires_in": 3600}), nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
	})}
	auth, err := StartDeviceAuthorization(context.Background(), client, "https://baidu.test", "app-key")
	if err != nil {
		t.Fatal(err)
	}
	if auth.DeviceCode != "device-code" || auth.Interval != 5 {
		t.Fatalf("unexpected authorization: %+v", auth)
	}
	token, state, err := PollDeviceAuthorization(context.Background(), client, "https://baidu.test", "app-key", "secret-key", auth.DeviceCode)
	if err != nil || state != "" || token.AccessToken != "access" || token.RefreshToken != "refresh" {
		t.Fatalf("unexpected token: %+v %q %v", token, state, err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestDeviceAuthorizationPending(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(map[string]any{"error": "authorization_pending", "error_description": "wait"}), nil
	})}
	_, state, err := PollDeviceAuthorization(context.Background(), client, "https://baidu.test", "app", "secret", "code")
	if state != "authorization_pending" || err == nil {
		t.Fatalf("state=%q err=%v", state, err)
	}
}
