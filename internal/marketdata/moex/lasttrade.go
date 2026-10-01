package moex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// lastTradeSessions is how many sessions back one history request looks. ISS
// pages history by at most 100 rows, one row per session, so this is one
// request and about five months.
const lastTradeSessions = 100

type tradedKey struct{ board, secid string }

// tradedAnswer is what lastTradeDay found for one session of one security.
type tradedAnswer struct{ session, day time.Time }

// lastTradeDay returns the last session, on or before session, in which secid
// traded on the board.
//
// ISS keeps one history row per security per session whether or not it traded,
// with the number of trades in it: checked on 2026-10-01, RU000A103AP6 on TQCB
// had PREVPRICE 77.5 beside PREVDATE 2026-09-30, zero trades in every session
// back to 2026-08-24, and one trade that day closing at 77.5.
//
// Three answers are not that day, and each is the best this can say:
//
//   - History that does not reach session yet (or holds no rows at all, as for
//     a security listed today) cannot say whether session traded. session is
//     returned, which is what the board itself said, and nothing is remembered,
//     so the next call asks again.
//   - No trade in any of the sessions read: the oldest of them is returned. The
//     price is at least that old, and a reader told "not updated since" that
//     day is told less than the truth and nothing false.
//
// THE ANSWER IS REMEMBERED PER SECURITY for as long as the board reports the
// same session, because it cannot change: the refresh runs every half hour and
// the session moves once a day, so history is read once a day per security
// instead of forty-eight times.
func (c *Client) lastTradeDay(ctx context.Context, b board, secid string, session time.Time) (time.Time, error) {
	key := tradedKey{board: b.label, secid: secid}
	c.mu.Lock()
	known, ok := c.traded[key]
	c.mu.Unlock()
	if ok && known.session.Equal(session) {
		return known.day, nil
	}

	sessions, err := c.fetchSessions(ctx, b, secid, session)
	if err != nil {
		return time.Time{}, err
	}
	if len(sessions) == 0 || !sessions[0].day.Equal(session) {
		c.log.Debug("moex: the security's history does not reach the session its price is dated by, keeping the session's date",
			"board", b.label, "ticker", secid, "session", session.Format(time.DateOnly))
		return session, nil
	}
	day := sessions[len(sessions)-1].day
	traded := false
	for _, s := range sessions {
		if s.trades > 0 {
			day, traded = s.day, true
			break
		}
	}
	if !traded {
		c.log.Debug("moex: no trade in any session read, dating the price by the oldest of them",
			"board", b.label, "ticker", secid, "sessions", len(sessions), "oldest", day.Format(time.DateOnly))
	}

	c.mu.Lock()
	c.traded[key] = tradedAnswer{session: session, day: day}
	c.mu.Unlock()
	return day, nil
}

// tradedSession is one row of a security's history: a session and how many
// trades it saw.
type tradedSession struct {
	day    time.Time
	trades int64
}

// fetchSessions reads the security's sessions up to and including till, newest
// first.
func (c *Client) fetchSessions(ctx context.Context, b board, secid string, till time.Time) ([]tradedSession, error) {
	endpoint := fmt.Sprintf("%s%s%s.json?iss.meta=off&iss.only=history&history.columns=TRADEDATE,NUMTRADES"+
		"&sort_order=desc&limit=%d&till=%s",
		c.baseURL, b.history, url.PathEscape(secid), lastTradeSessions, till.Format(time.DateOnly))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("moex: %s: history of %s: build request: %w", b.label, secid, err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moex: %s: history of %s: request: %w", b.label, secid, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("moex: %s: history of %s: unexpected status %d", b.label, secid, resp.StatusCode)
	}

	var body struct {
		History struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"history"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("moex: %s: history of %s: decode: %w", b.label, secid, err)
	}

	dateIdx, tradesIdx := -1, -1
	for i, name := range body.History.Columns {
		switch name {
		case "TRADEDATE":
			dateIdx = i
		case "NUMTRADES":
			tradesIdx = i
		}
	}
	if len(body.History.Data) > 0 && (dateIdx < 0 || tradesIdx < 0) {
		return nil, fmt.Errorf("moex: %s: history of %s: response missing TRADEDATE or NUMTRADES column", b.label, secid)
	}

	out := make([]tradedSession, 0, len(body.History.Data))
	for i, fields := range body.History.Data {
		if len(fields) <= dateIdx || len(fields) <= tradesIdx {
			return nil, fmt.Errorf("moex: %s: history of %s: row %d has %d fields", b.label, secid, i, len(fields))
		}
		text, _ := fields[dateIdx].(string)
		day, err := time.Parse(time.DateOnly, text)
		if err != nil {
			return nil, fmt.Errorf("moex: %s: history of %s: row %d: TRADEDATE %v is not a day", b.label, secid, i, fields[dateIdx])
		}
		// A null count is a session with nothing to count.
		var trades int64
		if num, ok := fields[tradesIdx].(json.Number); ok {
			f, err := num.Float64()
			if err != nil {
				return nil, fmt.Errorf("moex: %s: history of %s: row %d: NUMTRADES %q: %w", b.label, secid, i, num.String(), err)
			}
			trades = int64(f)
		}
		out = append(out, tradedSession{day: day, trades: trades})
	}
	return out, nil
}
