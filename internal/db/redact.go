package db

import (
	"errors"
	"strings"
)

// redactDSN replaces any occurrence of the connection string in err's
// message with a placeholder. pgx errors quote the DSN — password and all —
// and these errors are logged at startup.
func redactDSN(err error, dsn string) error {
	if err == nil || dsn == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, dsn) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, dsn, "[redacted DATABASE_URL]"))
}
