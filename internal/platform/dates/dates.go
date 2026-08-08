// Package dates states, once, how far into the future a person is allowed to
// date a record they are entering by hand.
//
// It is a package of its own for the reason internal/platform/currency is: the
// rule was written out twice, in two Go packages — a balance mark's as_of
// (internal/account, in parseAsOf) and an operation's occurred_on
// (internal/operation, on both the ordinary write path and the transfer one) —
// and two statements of one rule are two things that can drift apart. Neither
// copy was wrong; they were the same arithmetic spelled twice, and the second
// one's comment already had to say it "mirrors the account package's as_of
// slack", which is a comment doing the job an import should.
//
// It deliberately says nothing about how far BACK a date may go. That bound is
// not shared: an operation dated centuries ago sorts to the front of the FIFO
// queue and quietly changes which lots a later sale releases, while a balance
// mark dated centuries ago is simply an old mark that the latest one still
// wins over — one is a wrong number nobody is told about, the other is a
// visible mistake in a row the reader can retype. So the floor lives with the
// operation rule it protects (see internal/operation), and this package holds
// only what both callers genuinely have in common.
package dates

import "time"

// LatestRecordable is the newest date a hand-entered record may carry: one day
// past the UTC "today" boundary.
//
// The extra day is slack for time zones, not tolerance for future dates. A
// date-only field carries no zone, and the server's "today" is UTC's; a user
// anywhere from UTC+3 to UTC+12 reaches their own local tomorrow while the
// server is still on today, and must be able to record what they did this
// evening. Their tomorrow-in-UTC is still today somewhere on Earth. Anything
// further out than that is a genuine future date and is refused by the callers.
//
// Returned as a value rather than kept as a package variable because it moves:
// it is read at each write, so a process running across midnight does not go on
// refusing the new day.
func LatestRecordable() time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
}
