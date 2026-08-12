package core

import (
	"github.com/google/uuid"
)

// ID is a stable UUID for board/schematic items.
type ID uuid.UUID

// NewID returns a random v4 ID.
func NewID() ID {
	return ID(uuid.New())
}

// ParseID parses a UUID string.
func ParseID(s string) (ID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ID{}, err
	}
	return ID(u), nil
}

// String returns the canonical UUID string.
func (id ID) String() string {
	return uuid.UUID(id).String()
}

// IsZero reports whether id is the nil UUID.
func (id ID) IsZero() bool {
	return uuid.UUID(id) == uuid.Nil
}

// MarshalText implements encoding.TextMarshaler for map keys / JSON strings.
func (id ID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (id *ID) UnmarshalText(b []byte) error {
	u, err := uuid.Parse(string(b))
	if err != nil {
		return err
	}
	*id = ID(u)
	return nil
}
