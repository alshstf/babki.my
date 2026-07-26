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
	if err := a.sm.RenewToken(ctx); err != nil {
		return err
	}
	a.sm.Put(ctx, sessionUserKey, userID.String())
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
		p, err := a.store.MembershipFor(r.Context(), userID)
		if errors.Is(err, pgx.ErrNoRows) {
			httpjson.Error(w, http.StatusUnauthorized, "membership not found")
			return
		}
		if err != nil {
			httpjson.Error(w, http.StatusInternalServerError, "internal error")
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
