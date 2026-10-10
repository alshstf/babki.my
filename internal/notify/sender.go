package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// pushHosts are the push services a browser may give an address at. Any
// other address is refused: the server sends to it on its own, and must not
// be made to knock on a machine of the home network.
var pushHosts = []string{
	"fcm.googleapis.com",        // Chrome, Edge on Android, most Android browsers
	"android.googleapis.com",    // older Android browsers
	"jmt17.google.com",          // Chromium builds that are not Chrome
	"push.services.mozilla.com", // Firefox
	"push.apple.com",            // Safari, iPhone (web.push.apple.com)
	"notify.windows.com",        // Edge on Windows
}

// AllowedEndpoint says whether an address is a push service's.
func AllowedEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range pushHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// serviceHost is the push service of an address, for the log: never the
// whole address, which names the device.
func serviceHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Hostname()
	}
	return ""
}

// Sender delivers a push to a device. Gone is true when the push service
// says the device is no more: it is then forgotten.
type Sender interface {
	Send(ctx context.Context, sub Subscription, r Reminder) (gone bool, err error)
}

// subscriber names the software to the push services (the VAPID «sub»).
const subscriber = "https://github.com/alshstf/babki.my"

// WebPush sends through the browsers' push services.
type WebPush struct {
	keys   Keys
	client *http.Client
}

func NewWebPush(keys Keys) *WebPush {
	return &WebPush{keys: keys, client: &http.Client{Timeout: 10 * time.Second}}
}

func (w *WebPush) Send(ctx context.Context, sub Subscription, r Reminder) (bool, error) {
	if !AllowedEndpoint(sub.Endpoint) {
		return true, nil
	}
	payload, err := json.Marshal(map[string]string{"title": r.Title, "body": r.Body, "url": r.URL, "tag": r.Key})
	if err != nil {
		return false, err
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload,
		&webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth}},
		&webpush.Options{
			HTTPClient: w.client, Subscriber: subscriber, TTL: 24 * 60 * 60, Urgency: webpush.UrgencyNormal,
			VAPIDPublicKey: w.keys.Public, VAPIDPrivateKey: w.keys.Private,
		})
	if err != nil {
		return false, fmt.Errorf("notify: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return true, fmt.Errorf("notify: push service answered %d", resp.StatusCode)
	case resp.StatusCode >= 300:
		return false, fmt.Errorf("notify: push service answered %d", resp.StatusCode)
	}
	return false, nil
}
