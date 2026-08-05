package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/secretbox"
)

// ErrTokenRejected means the broker was reached and refused the token. It is
// kept apart from ErrBrokerUnreachable because the two are opposite news: this
// one is answered by pasting a new token, the other by waiting. The HTTP layer
// turns them into different status codes for exactly that reason, and the
// status code is the only place the difference is stated — the error text is
// prose, which no client may branch on.
var ErrTokenRejected = errors.New("tinvest: the broker refused this token")

// ErrBrokerUnreachable means the call to the broker did not produce an answer
// this program could use: the network, a gateway error, a rate limit that did
// not clear, a response that would not parse. Anything except a refusal of the
// token itself, which is ErrTokenRejected.
var ErrBrokerUnreachable = errors.New("tinvest: the broker could not be reached")

// ErrConnectionNotActive means a sync was asked for on a connection the
// scheduler would skip: one the owner switched off, or one parked waiting for a
// new token. Queueing it anyway would put a job in the queue that the worker
// drops on sight, and the owner would be told a sync was coming that never
// runs.
var ErrConnectionNotActive = errors.New("tinvest: the connection is not active, so it cannot be synced")

// ErrBrokerAccountAlreadyLinked means one of the broker accounts picked is
// already imported by a connection of this space. Two links onto one broker
// account would produce two babki accounts holding the same operations twice
// over, and nothing downstream could tell which copy was which.
var ErrBrokerAccountAlreadyLinked = errors.New("tinvest: that broker account is already imported by another connection of this space")

// ErrBrokerAccountNotImportable means a picked broker account is not one the
// token can see, or is of a kind this program does not import (see
// importableAccountTypes). Refused rather than linked-and-ignored: a link this
// program cannot read would sync forever and produce nothing, while looking on
// screen exactly like one that works.
//
// It is answered with 422 and not with ErrTokenRejected's 400, because the two
// arrive on one path and mean opposite things about the token — see writeError
// in http.go for what a client does with the difference.
var ErrBrokerAccountNotImportable = errors.New("tinvest: the token does not see that broker account, or it is not of a kind this program imports")

// importableAccountTypes is which of the broker's account kinds this program
// offers to import: an ordinary brokerage account and an ИИС. The values are
// the gateway's own enum members, kept as the strings they arrive as (see
// Account.Type).
//
// EVERYTHING ELSE THE TOKEN CAN SEE IS LEFT OUT, and it is left out because
// nothing here knows how to read it, not because it is unimportant: the
// «инвесткопилка» and the card and savings accounts a T-Bank token also reaches
// carry operations of shapes this importer's projection has no rules for, and
// offering them would produce a connection whose whole history lands in the
// unparsed list.
var importableAccountTypes = map[string]bool{
	"ACCOUNT_TYPE_TINKOFF":     true,
	"ACCOUNT_TYPE_TINKOFF_IIS": true,
}

// importedAccountCurrency and importedAccountInstitution are what an imported
// account is created as.
//
// THE CURRENCY IS LOAD-BEARING AND NOT A DEFAULT. After every successful sync
// the reconciliation writes the broker's own ruble balance onto the linked
// account as a balance mark, and it refuses outright to write that mark onto an
// account kept in any other currency (see ErrAccountNotInRubles). An account
// created in dollars here would therefore reconcile and then fail on the last
// step of every single run.
const (
	importedAccountCurrency    = "RUB"
	importedAccountInstitution = "Т-Банк"
)

// tokenLast4Len is how much of the token is ever shown again: enough for the
// owner to tell one token from another, and short enough to be no use to anyone
// who reads it. There is no bound anywhere on how long a token is, so a shorter
// token yields a shorter tail rather than a panic — see tokenLast4.
const tokenLast4Len = 4

// tokenLast4 is the tail of the token that is published in place of it.
// Measured in runes and not bytes: a token is expected to be ASCII, but nothing
// checks that, and slicing a multi-byte character in half would put invalid
// UTF-8 into a JSON response.
func tokenLast4(token string) string {
	r := []rune(token)
	if len(r) <= tokenLast4Len {
		return string(r)
	}
	return string(r[len(r)-tokenLast4Len:])
}

// accountCreator is the account store as this package's connection setup uses
// it — one method, declared locally, which is the convention this package
// follows for every other module's store (see balanceMarker and engineReader in
// reconcile.go). *account.Store satisfies it structurally.
type accountCreator interface {
	Create(ctx context.Context, spaceID uuid.UUID, ownerUserID *uuid.UUID,
		name string, t account.Type, currency, institution string) (account.Account, error)
}

// AccountPick is one broker account the owner chose to import, and what to call
// the babki account it is imported into.
type AccountPick struct {
	BrokerAccountID string
	AccountName     string
}

// ConnectionUpdate is a partial change to a connection: a nil field is left
// alone. Pointers rather than empty values, because "" is something a caller
// can send by accident and it has to be refused rather than read as "leave it".
type ConnectionUpdate struct {
	Token  *string
	Status *ConnectionStatus
}

// ConnectionView is one connection with everything a screen shows about it:
// the connection row, the broker accounts it imports, when it last synced
// successfully and what the last check against the broker said.
//
// LastSuccessfulSyncAt and LastReconcile are both nil-able and mean different
// absences: never synced successfully, and never reconciled. Neither is derived
// from the other — a run can succeed without reconciling, and a reconciliation
// belongs to a run that succeeded — so they are read separately and published
// separately.
type ConnectionView struct {
	Connection           Connection
	Links                []AccountLink
	LastSuccessfulSyncAt *time.Time
	LastReconcile        *SyncRun
}

// Service is the request path of the T-Invest importer: everything a person
// does to a connection, as opposed to what the background worker does with one.
//
// EVERY METHOD HERE IS OWNER-ONLY, AND THE CHECK LIVES IN THIS FILE rather than
// in the handlers — the same place family.Service.UpdateSpace keeps its own. A
// role check in a route can be forgotten when a route is added; a role check as
// the first statement of the method cannot be reached around at all, and a
// second caller (a future CLI, a job) gets it for free.
type Service struct {
	store     *Store
	accounts  accountCreator
	box       *secretbox.Box
	newClient clientFactory
	inserter  jobInserter
	log       *slog.Logger
}

func NewService(store *Store, accounts accountCreator, box *secretbox.Box,
	newClient clientFactory, inserter jobInserter, log *slog.Logger,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: store, accounts: accounts, box: box,
		newClient: newClient, inserter: inserter, log: log,
	}
}

// requireOwner is the first statement of every exported method below.
func requireOwner(p family.Principal) error {
	if p.Role != family.RoleOwner {
		return family.ErrForbidden
	}
	return nil
}

// checkToken asks the broker which accounts a token can see and keeps the ones
// this program imports.
//
// THE TOKEN IS NOT EDITED ON ITS WAY THROUGH. It is refused when empty and
// otherwise sent exactly as it arrived — no trimming, no case folding. A secret
// this server quietly rewrote would be a secret whose stored form is not the
// one the owner pasted, and the contract says only that it must not be empty,
// which is the whole of what is checked here.
func (s *Service) checkToken(ctx context.Context, token string) ([]Account, error) {
	if token == "" {
		return nil, fmt.Errorf("%w: token must not be empty", family.ErrValidation)
	}
	c, err := s.newClient(token)
	if err != nil {
		// Not ErrBrokerUnreachable: nothing has been asked of the broker yet.
		// This is this process failing to build a client — a broken certificate
		// pool, say — which is a fault of the instance and belongs in a 500.
		return nil, fmt.Errorf("tinvest: build the broker client: %w", err)
	}
	accounts, err := c.GetAccounts(ctx)
	if errors.Is(err, ErrTokenInvalid) {
		return nil, ErrTokenRejected
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBrokerUnreachable, err)
	}
	out := make([]Account, 0, len(accounts))
	for _, a := range accounts {
		if importableAccountTypes[a.Type] {
			out = append(out, a)
		}
	}
	return out, nil
}

// CheckToken lists the accounts a read-only token can see, so the owner can
// pick which to import. Nothing is stored: the token is used for this one call
// and dropped.
func (s *Service) CheckToken(ctx context.Context, p family.Principal, token string) ([]Account, error) {
	if err := requireOwner(p); err != nil {
		return nil, err
	}
	return s.checkToken(ctx, token)
}

// CreateConnection stores the token, creates one babki account per picked
// broker account, links them, and queues the first import.
//
// THE ORDER IS CHOSEN SO THAT A REFUSAL WRITES NOTHING. Everything that can be
// judged without touching the database is judged first — the shape of the
// request, then what the broker says the token can see, then whether any of the
// picks is already imported — so a token the broker rejects, a duplicate pick
// and an account already connected all leave the space exactly as it was.
//
// THE CONNECTION IS BORN SWITCHED OFF AND IS SWITCHED ON ONLY ONCE ITS ACCOUNTS
// AND LINKS ARE WRITTEN. A connection that is active is one the hourly scheduler
// will sync, and until the links exist there is nothing to sync into — so
// "active" is a statement about a connection that is otherwise complete, rather
// than the state it is created in. Exactly one step follows the switch-on,
// queueing the first import, and outcome 4 below says why it is on that side.
//
// WHAT A FAILURE PART-WAY THROUGH LEAVES BEHIND. Once the writes begin there are
// four outcomes, and all four are named here because the cleanup is a best
// effort and not a guarantee (see undoConnection, which logs its own failure and
// returns the original one):
//
//  1. a write fails and the cleanup succeeds: the connection and its links are
//     gone. The babki accounts already created are NOT — deleting an account is
//     something this program never does behind the owner's back, the same rule
//     that makes DeleteConnection leave accounts standing — so what is left is
//     one or more empty accounts on the accounts screen, which the owner can
//     archive.
//  2. a write fails and the cleanup fails too: a half-built connection stays,
//     and it stays SWITCHED OFF. The scheduler passes it over and nothing offers
//     its token to the broker of its own accord; the owner sees a disabled
//     connection they can delete. This is the outcome the parking above exists
//     for: the same leftover with status active would have been imported from,
//     hourly, into accounts the owner was told had not been made.
//  3. switching it on fails: same as 2 — it was never on.
//  4. queueing the first sync fails (the only step after the connection is on)
//     and the cleanup fails too: what stays is a COMPLETE, active connection
//     whose owner was told the request failed. The hourly dispatcher syncs it
//     within the hour, into the accounts and links made for it, which is what
//     would have happened had the request succeeded; the owner's screen shows
//     the connection, which they can delete. The queueing is deliberately after the
//     switch-on and not before: a job queued for a connection the scheduler is
//     not yet allowed to touch is one the worker drops on sight (see
//     connectionNotActiveMessage), and the first import would then wait for the
//     next hour for no reason.
//
// The alternative, one transaction spanning three stores, is not available here:
// each store owns its own statement and none of them takes a transaction from
// outside.
//
// THE "ALREADY IMPORTED" CHECK ABOVE IS NOT RACE-PROOF, and no constraint is
// behind it. Neither unique index on tinvest_account_links covers the rule: the
// one on (connection_id, broker_account_id) says nothing about two DIFFERENT
// connections naming one broker account, and the one on account_id is about the
// babki account, of which two such connections would make two. Two creations
// racing each other would therefore both pass the check and both be written. One
// person clicking one button cannot do it, which is why it is left as it is —
// but a reader must not take this check for a constraint.
func (s *Service) CreateConnection(ctx context.Context, p family.Principal,
	token string, picks []AccountPick,
) (ConnectionView, error) {
	if err := requireOwner(p); err != nil {
		return ConnectionView{}, err
	}
	if err := validatePicks(picks); err != nil {
		return ConnectionView{}, err
	}
	brokerAccounts, err := s.checkToken(ctx, token)
	if err != nil {
		return ConnectionView{}, err
	}
	byID := make(map[string]Account, len(brokerAccounts))
	for _, a := range brokerAccounts {
		byID[a.ID] = a
	}
	chosen := make([]Account, 0, len(picks))
	for _, pick := range picks {
		a, ok := byID[pick.BrokerAccountID]
		if !ok {
			return ConnectionView{}, fmt.Errorf("%w: %s", ErrBrokerAccountNotImportable, pick.BrokerAccountID)
		}
		chosen = append(chosen, a)
	}
	linked, err := s.linkedBrokerAccounts(ctx, p.SpaceID)
	if err != nil {
		return ConnectionView{}, err
	}
	for _, pick := range picks {
		if linked[pick.BrokerAccountID] {
			return ConnectionView{}, fmt.Errorf("%w: %s", ErrBrokerAccountAlreadyLinked, pick.BrokerAccountID)
		}
	}

	conn, err := s.store.CreateConnection(ctx, p.SpaceID, s.box.Seal([]byte(token)),
		tokenLast4(token), StatusDisabled)
	if err != nil {
		return ConnectionView{}, err
	}
	links := make([]AccountLink, 0, len(picks))
	for i, pick := range picks {
		// Shared rather than personal (a nil owner): an imported account is the
		// household's in the same way a hand-entered brokerage account is, and
		// nothing about connecting a broker says otherwise.
		acc, err := s.accounts.Create(ctx, p.SpaceID, nil, pick.AccountName,
			account.TypeBrokerage, importedAccountCurrency, importedAccountInstitution)
		if err != nil {
			return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
		}
		link, err := s.store.CreateLink(ctx, AccountLink{
			ConnectionID:      conn.ID,
			SpaceID:           p.SpaceID,
			AccountID:         acc.ID,
			BrokerAccountID:   pick.BrokerAccountID,
			BrokerAccountName: chosen[i].Name,
			BrokerAccountType: chosen[i].Type,
			OpenedOn:          chosen[i].OpenedOn,
		})
		if err != nil {
			return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
		}
		links = append(links, link)
	}

	// Everything the connection needs is written, so it may be switched on. The
	// row is read back rather than patched in memory: what this answers with is
	// then the row as stored, and a status that failed to stick cannot be
	// published as though it had.
	if err := s.store.UpdateConnectionStatus(ctx, conn.ID, StatusActive); err != nil {
		return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
	}
	active, err := s.store.ConnectionByID(ctx, p.SpaceID, conn.ID)
	if err != nil {
		return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
	}

	// THROUGH EnqueueSync AND NOT BY BUILDING THE INSERT HERE. The hourly
	// schedule and the owner's button go through the same helper so that all
	// three ways of starting a sync land in one class of uniqueness; two places
	// spelling the arguments out separately is exactly how the schedule and a
	// manual run would become able to run over one connection at once.
	if _, err := EnqueueSync(ctx, s.inserter, conn.ID, TriggerInitial); err != nil {
		return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
	}
	return ConnectionView{Connection: active, Links: links}, nil
}

// undoConnection removes a connection whose setup failed part-way and returns
// the failure that caused it. The cleanup's own error is logged rather than
// returned: the caller asked about the original failure, and replacing it with
// "and then the cleanup failed too" would hide the thing that went wrong first.
//
// IT IS A BEST EFFORT AND NOT A GUARANTEE — a log line is all a failed removal
// produces — which is why the connection it removes was created switched off.
// What a failure here leaves standing is a connection the scheduler passes over,
// rather than one it syncs from; CreateConnection's own doc names every outcome.
//
// The context is deliberately NOT the request's. A failure here is very often a
// canceled request, and a cleanup running on that same context would be
// canceled before it deleted anything — leaving exactly the half-built
// connection this exists to remove. A short budget of its own is what makes the
// removal happen; five seconds is generous for one DELETE by primary key.
func (s *Service) undoConnection(spaceID, id uuid.UUID, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), undoTimeout)
	defer cancel()
	if err := s.store.DeleteConnection(ctx, spaceID, id); err != nil {
		s.log.Error("tinvest: removing a half-built connection failed", "connection", id, "err", err)
	}
	return cause
}

// undoTimeout bounds the cleanup above.
const undoTimeout = 5 * time.Second

// validatePicks judges the request's own shape, before anything is asked of the
// broker or of the database.
func validatePicks(picks []AccountPick) error {
	if len(picks) == 0 {
		return fmt.Errorf("%w: pick at least one broker account to import", family.ErrValidation)
	}
	seen := make(map[string]bool, len(picks))
	for _, p := range picks {
		if p.BrokerAccountID == "" {
			return fmt.Errorf("%w: broker_account_id must not be empty", family.ErrValidation)
		}
		if p.AccountName == "" {
			return fmt.Errorf("%w: account_name must not be empty", family.ErrValidation)
		}
		if seen[p.BrokerAccountID] {
			// Caught here rather than left to the (connection, broker account)
			// unique index, which would refuse the second link with a driver
			// error after the first account had already been created.
			return fmt.Errorf("%w: broker account %s is named twice", family.ErrValidation, p.BrokerAccountID)
		}
		seen[p.BrokerAccountID] = true
	}
	return nil
}

// linkedBrokerAccounts is every broker account already imported by any
// connection of this space.
//
// Assembled from the two reads this package already has rather than from a
// query of its own: a space holds a handful of connections and each holds a
// handful of links, so the cost is a few round trips on a screen that makes one
// request an hour at most — and the alternative would be a third statement
// about what a link is, in a package where the other two already agree.
func (s *Service) linkedBrokerAccounts(ctx context.Context, spaceID uuid.UUID) (map[string]bool, error) {
	conns, err := s.store.ListConnections(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range conns {
		links, err := s.store.LinksByConnection(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			out[l.BrokerAccountID] = true
		}
	}
	return out, nil
}

// UpdateConnection replaces the token, switches the connection on or off, or
// both.
//
// A NEW TOKEN IS PROVED BEFORE IT IS STORED. The broker is asked what it can
// see, and only an answer makes the token replace the old one — which is also
// what brings a connection back from token_revoked, since a token the broker
// just accepted is the only evidence that pasting one solved anything. An
// explicit status in the same request wins over that, so a token can be
// replaced on a connection the owner wants to leave switched off.
func (s *Service) UpdateConnection(ctx context.Context, p family.Principal,
	id uuid.UUID, upd ConnectionUpdate,
) (ConnectionView, error) {
	if err := requireOwner(p); err != nil {
		return ConnectionView{}, err
	}
	if upd.Token == nil && upd.Status == nil {
		return ConnectionView{}, fmt.Errorf("%w: nothing to update: give token, status or both", family.ErrValidation)
	}
	if upd.Status != nil && *upd.Status != StatusActive && *upd.Status != StatusDisabled {
		// token_revoked is refused on purpose: it is this server's own record of
		// what the broker said, and a client able to set it could state a fact
		// nobody checked.
		return ConnectionView{}, fmt.Errorf("%w: status must be %q or %q",
			family.ErrValidation, StatusActive, StatusDisabled)
	}
	if _, err := s.store.ConnectionByID(ctx, p.SpaceID, id); err != nil {
		return ConnectionView{}, err
	}

	status := upd.Status
	if upd.Token != nil {
		if _, err := s.checkToken(ctx, *upd.Token); err != nil {
			return ConnectionView{}, err
		}
		if err := s.store.UpdateConnectionToken(ctx, p.SpaceID, id,
			s.box.Seal([]byte(*upd.Token)), tokenLast4(*upd.Token)); err != nil {
			return ConnectionView{}, err
		}
		if status == nil {
			active := StatusActive
			status = &active
		}
	}
	if status != nil {
		if err := s.store.UpdateConnectionStatus(ctx, id, *status); err != nil {
			return ConnectionView{}, err
		}
	}
	return s.Connection(ctx, p, id)
}

// DeleteConnection withdraws the connection to the broker. The babki accounts
// it created and the operations the import wrote into them are left standing —
// they are the owner's data, and the only thing being withdrawn here is the
// authorization to keep reading from the broker (see Store.DeleteConnection).
func (s *Service) DeleteConnection(ctx context.Context, p family.Principal, id uuid.UUID) error {
	if err := requireOwner(p); err != nil {
		return err
	}
	return s.store.DeleteConnection(ctx, p.SpaceID, id)
}

// TriggerSync asks for this connection to be synced now, and reports whether
// the request actually put a job in the queue.
//
// FALSE DOES NOT MEAN "A SYNC IS RUNNING". It means one was already in the
// queue, and the states that count as "in the queue" include a job parked
// waiting out its retry backoff — which River grows into the hours. See
// EnqueueSync, whose result this passes on unchanged.
func (s *Service) TriggerSync(ctx context.Context, p family.Principal, id uuid.UUID) (bool, error) {
	if err := requireOwner(p); err != nil {
		return false, err
	}
	conn, err := s.store.ConnectionByID(ctx, p.SpaceID, id)
	if err != nil {
		return false, err
	}
	if conn.Status != StatusActive {
		return false, fmt.Errorf("%w: it is %q", ErrConnectionNotActive, conn.Status)
	}
	res, err := EnqueueSync(ctx, s.inserter, id, TriggerManual)
	if err != nil {
		return false, fmt.Errorf("tinvest: queue a sync: %w", err)
	}
	return !res.UniqueSkippedAsDuplicate, nil
}

// ListConnections returns every connection of the caller's space, each with
// what a screen shows about it.
func (s *Service) ListConnections(ctx context.Context, p family.Principal) ([]ConnectionView, error) {
	if err := requireOwner(p); err != nil {
		return nil, err
	}
	conns, err := s.store.ListConnections(ctx, p.SpaceID)
	if err != nil {
		return nil, err
	}
	out := make([]ConnectionView, 0, len(conns))
	for _, c := range conns {
		view, err := s.view(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return out, nil
}

// Connection returns one connection of the caller's space. Returns
// pgx.ErrNoRows for a connection that is not the caller's, whether or not it
// exists elsewhere — see Store.ConnectionByID.
func (s *Service) Connection(ctx context.Context, p family.Principal, id uuid.UUID) (ConnectionView, error) {
	if err := requireOwner(p); err != nil {
		return ConnectionView{}, err
	}
	conn, err := s.store.ConnectionByID(ctx, p.SpaceID, id)
	if err != nil {
		return ConnectionView{}, err
	}
	return s.view(ctx, conn)
}

func (s *Service) view(ctx context.Context, conn Connection) (ConnectionView, error) {
	links, err := s.store.LinksByConnection(ctx, conn.ID)
	if err != nil {
		return ConnectionView{}, err
	}
	lastSync, err := s.store.LastSuccessfulSyncAt(ctx, conn.ID)
	if err != nil {
		return ConnectionView{}, err
	}
	lastReconcile, err := s.store.LastReconcile(ctx, conn.ID)
	if err != nil {
		return ConnectionView{}, err
	}
	return ConnectionView{
		Connection:           conn,
		Links:                links,
		LastSuccessfulSyncAt: lastSync,
		LastReconcile:        lastReconcile,
	}, nil
}

// Runs returns one page of the connection's sync log, newest first, and whether
// the log holds more beyond it.
//
// The connection is read first and by the caller's space, which is the whole of
// what keeps this off a stranger's log: Store.RunsByConnection takes no space
// and checks none (see the note on Store).
func (s *Service) Runs(ctx context.Context, p family.Principal, id uuid.UUID, limit, offset int) (
	[]SyncRun, bool, error,
) {
	if err := requireOwner(p); err != nil {
		return nil, false, err
	}
	if _, err := s.store.ConnectionByID(ctx, p.SpaceID, id); err != nil {
		return nil, false, err
	}
	return s.store.RunsByConnection(ctx, id, limit, offset)
}

// Unparsed returns one page of the connection's operations that the projection
// could not read, newest first, and whether there are more beyond it. Scoped by
// the same read Runs is scoped by, for the same reason.
func (s *Service) Unparsed(ctx context.Context, p family.Principal, id uuid.UUID, limit, offset int) (
	[]MirrorRow, bool, error,
) {
	if err := requireOwner(p); err != nil {
		return nil, false, err
	}
	if _, err := s.store.ConnectionByID(ctx, p.SpaceID, id); err != nil {
		return nil, false, err
	}
	return s.store.UnparsedByConnection(ctx, id, limit, offset)
}
