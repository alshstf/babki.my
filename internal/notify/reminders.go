package notify

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/creditcard"
)

// Reminder is one push: Key names it for good, so it is sent once to each
// member; Owner, when set, is the only member it is for (a personal card).
type Reminder struct {
	Key   string
	Owner *uuid.UUID
	Title string
	Body  string
	URL   string
}

// soonDays is how many days ahead of a payment day the push comes.
const soonDays = 3

// CardReminders are the pushes the cards call for today: three days before a
// payment day and on it — what keeps the grace, then the minimum, unless the
// grace's sum on the same day covers it — and at once when the minimum was
// missed, a grace lost, or the grace taken off the whole debt.
func CardReminders(cards []creditcard.Card, today time.Time) []Reminder {
	var out []Reminder
	for _, c := range cards {
		st, name, cur := c.Status, c.Account.Name, c.Account.Currency
		acc := c.Account.ID.String()
		add := func(key, body string) {
			out = append(out, Reminder{
				Key: "card:" + acc + ":" + key, Owner: c.Account.OwnerUserID,
				Title: name, Body: body, URL: "/accounts/" + acc,
			})
		}
		if st.MinimumMissed && st.Minimum > 0 {
			add("min-missed:"+day(st.MinimumOn), fmt.Sprintf("Обязательный платёж %s не внесён до %s.", amount(st.Minimum, cur), short(st.MinimumOn)))
		}
		for _, l := range st.Lost {
			// Taken off with the rest, it is in the message below.
			if l.Early {
				continue
			}
			add("lost:"+day(l.From), fmt.Sprintf("Льгота по покупкам %s–%s сгорела, осталось %s. Погасите как можно скорее.",
				short(l.From), short(l.To), amount(l.Amount, cur)))
		}
		if !st.GraceOffSince.IsZero() && st.ToRestore > 0 {
			add("grace-off:"+day(st.GraceOffSince), fmt.Sprintf("Льгота снята со всего долга с %s: проценты идут и на покупки, сделанные потом. Чтобы вернуть её, погасите %s.",
				short(st.GraceOffSince), amount(st.ToRestore, cur)))
		}
		var graceDay time.Time
		var graceSum int64
		if len(st.Grace) > 0 {
			g := st.Grace[0]
			graceDay, graceSum = g.On, g.Amount
			switch d := daysBetween(today, g.On); {
			case d == 0:
				add("grace:"+day(g.On)+":today", fmt.Sprintf("Сегодня последний день: внесите %s, чтобы не платить проценты.", amount(g.Amount, cur)))
			case d > 0 && d <= soonDays:
				add("grace:"+day(g.On)+":soon", fmt.Sprintf("%s до %s, чтобы не платить проценты.", amount(g.Amount, cur), short(g.On)))
			}
		}
		coveredByGrace := graceDay.Equal(st.MinimumOn) && graceSum >= st.Minimum
		if st.Minimum > 0 && !st.MinimumMissed && !coveredByGrace {
			switch d := daysBetween(today, st.MinimumOn); {
			case d == 0:
				add("min:"+day(st.MinimumOn)+":today", fmt.Sprintf("Сегодня последний день обязательного платежа: %s.", amount(st.Minimum, cur)))
			case d > 0 && d <= soonDays:
				add("min:"+day(st.MinimumOn)+":soon", fmt.Sprintf("Обязательный платёж %s до %s.", amount(st.Minimum, cur), short(st.MinimumOn)))
			}
		}
	}
	return out
}

func daysBetween(from, to time.Time) int {
	return int(to.Sub(from).Hours() / 24)
}

func day(t time.Time) string   { return t.Format(time.DateOnly) }
func short(t time.Time) string { return t.Format("02.01") }

var symbols = map[string]string{"RUB": "₽", "USD": "$", "EUR": "€", "CNY": "¥", "KZT": "₸"}

// amount writes minor units the Russian way: «42 979,50 ₽».
func amount(minor int64, currency string) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	whole := strconv.FormatInt(minor/100, 10)
	var groups []string
	for len(whole) > 3 {
		groups = append([]string{whole[len(whole)-3:]}, groups...)
		whole = whole[:len(whole)-3]
	}
	groups = append([]string{whole}, groups...)
	symbol, ok := symbols[currency]
	if !ok {
		symbol = currency
	}
	return fmt.Sprintf("%s%s,%02d %s", sign, strings.Join(groups, " "), minor%100, symbol)
}
