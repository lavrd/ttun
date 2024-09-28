package rpc

import (
	"errors"
	"fmt"

	"ttun/internal/types"
)

// 1 byte for method and connection id is 12 bytes.
const RequestSize = 13

var ErrSerialization = errors.New("serialization error")

type Method uint8

const (
	Ping Method = iota + 1
	Pong
)

type Request struct {
	Method Method
	ConnID types.ConnID
}

func (r *Request) Encode() []byte {
	buf := make([]byte, RequestSize)
	buf[0] = byte(r.Method)
	copy(buf[1:], r.ConnID[:])
	return buf
}

func (r *Request) Decode(buf []byte) error {
	if buf == nil {
		return fmt.Errorf("buf is nil: %w", ErrSerialization)
	}
	if len(buf) != RequestSize {
		return fmt.Errorf("bad request size: %w", ErrSerialization)
	}
	r.Method = Method(buf[0])
	if r.Method == 0 {
		return fmt.Errorf("rpc method is zero: %w", ErrSerialization)
	}
	copy(r.ConnID[:], buf[1:RequestSize])
	return nil
}

func (r *Request) String() string {
	method := "ping"
	if r.Method == 2 {
		method = "pong"
	}
	return fmt.Sprintf("method:%s,conn_id:%s", method, r.ConnID.String())
}
