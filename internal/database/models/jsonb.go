package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// JSONB is a []byte that round-trips through PostgreSQL's jsonb column type.
type JSONB []byte

func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return "[]", nil
	}
	return string(j), nil
}

func (j *JSONB) Scan(value any) error {
	if value == nil {
		*j = JSONB("[]")
		return nil
	}
	switch v := value.(type) {
	case []byte:
		*j = make(JSONB, len(v))
		copy(*j, v)
		return nil
	case string:
		*j = JSONB(v)
		return nil
	}
	return fmt.Errorf("JSONB: unsupported scan type %T", value)
}

func (j JSONB) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("[]"), nil
	}
	return json.RawMessage(j).MarshalJSON()
}

func (j *JSONB) UnmarshalJSON(data []byte) error {
	*j = make(JSONB, len(data))
	copy(*j, data)
	return nil
}
