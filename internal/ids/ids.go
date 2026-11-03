// Package ids generates entity IDs. New IDs are UUIDv7.
package ids

import (
	"fmt"

	"github.com/google/uuid"
)

// Generator produces new IDs. Domain code takes a Generator so tests can
// supply deterministic IDs.
type Generator interface {
	New() string
}

// V7 generates random, time-ordered UUIDv7 strings.
type V7 struct{}

// New returns a new UUIDv7. It panics only if the system's random source
// fails, which the uuid package also treats as unrecoverable.
func (V7) New() string {
	return uuid.Must(uuid.NewV7()).String()
}

// Parse validates s as a UUID and returns its canonical lowercase form.
func Parse(s string) (string, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid UUID %q: %w", s, err)
	}
	return u.String(), nil
}
