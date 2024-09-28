package types

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

const ConnIDSize = 12

type ConnID [ConnIDSize]byte

func (c ConnID) String() string {
	return base64.StdEncoding.EncodeToString(c[:])
}

func RandomConnID() (ConnID, error) {
	var buf ConnID
	if _, err := rand.Read(buf[:]); err != nil {
		return buf, fmt.Errorf("failed to read random bytes: %w", err)
	}
	return buf, nil
}
