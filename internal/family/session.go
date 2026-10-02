package family

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/platform/httpjson"
)

const sessionUserKey = "user_id"

// sessionSignedInKey is when the session was signed in, in Unix nanoseconds: a
// session older than its user's sessions_revoked_at no longer counts.
const sessionSignedInKey = "signed_in_at"

type ctxKey int

const principalCtxKey ctxKey = 1

// NewSessionManager returns an scs manager backed by the sessions table.
func NewSessionManager(pool *pgxpool.Pool) *scs.SessionManager {
	sm := scs.New()
	sm.Store = pgxstore.New(pool)
	sm.Lifetime = 30 * 24 * time.Hour
	sm.Cookie.Name = "babki_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	// Secure is off by default: homelab installs often run plain http on LAN.
	sm.Cookie.Secure = false
	return sm
}

// Auth couples the session manager with the family store.
type Auth struct {
	sm    *scs.SessionManager
	store *Store
}

func NewAuth(sm *scs.SessionManager, store *Store) *Auth { return &Auth{sm: sm, store: store} }

// SignIn rotates the session token and binds it to the user.
func (a *Auth) SignIn(ctx context.Context, userID uuid.UUID) error {
	return a.SignInAt(ctx, userID, time.Now())
}

// SignInAt is SignIn with the moment the session counts from: the moment the
// user's other sessions were ended, when this one is to outlive them.
func (a *Auth) SignInAt(ctx context.Context, userID uuid.UUID, at time.Time) error {
	if err := a.sm.RenewToken(ctx); err != nil {
		return err
	}
	a.sm.Put(ctx, sessionUserKey, userID.String())
	a.sm.Put(ctx, sessionSignedInKey, at.UnixNano())
	return nil
}

// SignOut destroys the session.
func (a *Auth) SignOut(ctx context.Context) error { return a.sm.Destroy(ctx) }

// RequireAuth authenticates the request via session cookie and stores the
// Principal in the context. Must run inside sm.LoadAndSave.
func (a *Auth) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := a.sm.GetString(r.Context(), sessionUserKey)
		if raw == "" {
			httpjson.Error(w, http.StatusUnauthorized, "authentication required")
			return
		}
		userID, err := uuid.Parse(raw)
		if err != nil {
			httpjson.Error(w, http.StatusUnauthorized, "invalid session")
			return
		}
		p, revoked, err := a.store.sessionFor(r.Context(), userID)
		if errors.Is(err, pgx.ErrNoRows) {
			httpjson.Error(w, http.StatusUnauthorized, "membership not found")
			return
		}
		if err != nil {
			httpjson.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		// A session signed in before the user's password changed, or before they
		// signed out everywhere else, is over. One from before signed-in times
		// were kept reads as signed in at zero, and is over too.
		if revoked != nil && a.sm.GetInt64(r.Context(), sessionSignedInKey) < revoked.UnixNano() {
			_ = a.sm.Destroy(r.Context())
			httpjson.Error(w, http.StatusUnauthorized, "session ended")
			return
		}
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), principalCtxKey, p)))
	})
}

// RequireRole gates the handler by minimum role. Must run after RequireAuth.
func RequireRole(min Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFromContext(r.Context())
		if !ok || !p.Role.AtLeast(min) {
			httpjson.Error(w, http.StatusForbidden, "insufficient role")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// PrincipalFromContext extracts the authenticated principal.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey).(Principal)
	return p, ok
}
