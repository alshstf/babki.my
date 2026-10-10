package notify

import (
	"log/slog"
	"net/http"

	"github.com/alexedwards/scs/v2"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Limits on what a browser hands over for a device.
const (
	maxEndpoint = 2048
	maxKey      = 256
)

// Handler lets a member turn reminders on and off for a device.
type Handler struct {
	store  *Store
	keys   *Keys
	sender Sender
	auth   *family.Auth
	sm     *scs.SessionManager
	log    *slog.Logger
}

// NewHandler serves the push endpoints; keys and sender are nil when the
// process has no encryption key to derive the push keys from.
func NewHandler(store *Store, keys *Keys, sender Sender, auth *family.Auth, sm *scs.SessionManager, log *slog.Logger) *Handler {
	return &Handler{store: store, keys: keys, sender: sender, auth: auth, sm: sm, log: log}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	member := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	srv.Mount("GET /api/v1/push/key", member(h.handleKey))
	srv.Mount("PUT /api/v1/push/subscriptions", member(h.handleSubscribe))
	srv.Mount("DELETE /api/v1/push/subscriptions", member(h.handleUnsubscribe))
	srv.Mount("POST /api/v1/push/test", member(h.handleTest))
}

func (h *Handler) handleKey(w http.ResponseWriter, _ *http.Request) {
	if h.keys == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "push is off: the server has no encryption key")
		return
	}
	httpjson.Write(w, http.StatusOK, apitypes.PushKey{PublicKey: h.keys.Public})
}

func (h *Handler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.PushSubscriptionRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	switch {
	case len(req.Endpoint) > maxEndpoint || !AllowedEndpoint(req.Endpoint):
		httpjson.Error(w, http.StatusBadRequest, "endpoint must be a known push service's https address")
		return
	case req.P256dh == "" || req.Auth == "" || len(req.P256dh) > maxKey || len(req.Auth) > maxKey:
		httpjson.Error(w, http.StatusBadRequest, "p256dh and auth are the device's keys")
		return
	}
	err := h.store.Subscribe(r.Context(), p.SpaceID, Subscription{Endpoint: req.Endpoint, UserID: p.UserID, P256dh: req.P256dh, Auth: req.Auth})
	if err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	endpoint := r.URL.Query().Get("endpoint")
	if endpoint == "" {
		httpjson.Error(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	found, err := h.store.Unsubscribe(r.Context(), p.UserID, endpoint)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if !found {
		httpjson.Error(w, http.StatusNotFound, "no such device of yours")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTest pushes a sample to the member's devices now, so they see it
// works; it is not noted as sent.
func (h *Handler) handleTest(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	if h.sender == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "push is off: the server has no encryption key")
		return
	}
	devices, err := h.store.Devices(r.Context(), p.SpaceID, &p.UserID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	delivered := 0
	sample := Reminder{Key: "test", Title: "babki.my", Body: "Напоминания о платежах включены на этом устройстве.", URL: "/accounts"}
	for _, d := range devices {
		gone, err := h.sender.Send(r.Context(), d, sample)
		switch {
		case gone:
			_ = h.store.Forget(r.Context(), d.Endpoint)
		case err != nil:
			h.log.Warn("test push not delivered", "error", err)
		default:
			delivered++
		}
	}
	httpjson.Write(w, http.StatusOK, apitypes.PushTestResult{Devices: len(devices), Delivered: delivered})
}
