package migrate

import (
	"github.com/pressly/goose/v3/lock"
)

func mustLocker() lock.SessionLocker {
	l, err := lock.NewPostgresSessionLocker()
	if err != nil {
		panic(err) // only fails on invalid options, which are static here
	}
	return l
}
