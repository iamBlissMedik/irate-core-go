// Package timeutil holds small calendar helpers.
package timeutil

import "time"

// StartOfDay returns midnight at the start of t's calendar day in loc.
// Business "days" (daily limits, dashboard "today") follow the configured
// BUSINESS_TIMEZONE, not the server's or UTC's midnight.
func StartOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}
