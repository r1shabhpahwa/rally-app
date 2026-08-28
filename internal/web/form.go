package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// formInt reads an integer field, falling back to def when absent or unparsable.
func formInt(r *http.Request, name string, def int) int {
	v := strings.TrimSpace(r.PostFormValue(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// formCents parses a money field written as "35", "$35", "35.00" or "$35.50".
func formCents(r *http.Request, name string) (int64, error) {
	raw := strings.TrimSpace(r.PostFormValue(name))
	raw = strings.TrimPrefix(raw, "$")
	raw = strings.ReplaceAll(raw, ",", "")
	if raw == "" {
		return 0, fmt.Errorf("enter an amount")
	}
	whole, frac, hasFrac := strings.Cut(raw, ".")
	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || dollars < 0 {
		return 0, fmt.Errorf("%q is not a valid amount", raw)
	}
	cents := dollars * 100
	if hasFrac {
		frac = (frac + "00")[:2]
		c, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a valid amount", raw)
		}
		cents += c
	}
	return cents, nil
}

// localTime combines a date field and a time field into an instant in loc.
func localTime(date, clock string, loc *time.Location) (time.Time, error) {
	date, clock = strings.TrimSpace(date), strings.TrimSpace(clock)
	if date == "" || clock == "" {
		return time.Time{}, fmt.Errorf("a date and a time are both required")
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %s is not a valid date and time", date, clock)
	}
	return t, nil
}

// validClock checks an HH:MM field.
func validClock(v string) (string, error) {
	v = strings.TrimSpace(v)
	if _, err := time.Parse("15:04", v); err != nil {
		return "", fmt.Errorf("%q is not a valid time", v)
	}
	return v, nil
}

// validDate checks a YYYY-MM-DD field.
func validDate(v string) (string, error) {
	v = strings.TrimSpace(v)
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return "", fmt.Errorf("%q is not a valid date", v)
	}
	return v, nil
}

// looksLikeEmail is a deliberately loose check: the real validation is whether
// the message is deliverable, which only the outbox can tell us.
func looksLikeEmail(v string) bool {
	v = strings.TrimSpace(v)
	at := strings.LastIndex(v, "@")
	if at <= 0 || at == len(v)-1 || strings.ContainsAny(v, " \t\r\n") {
		return false
	}
	return strings.Contains(v[at+1:], ".")
}
