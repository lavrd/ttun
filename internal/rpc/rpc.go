package rpc

import (
	"errors"
	"fmt"
)

var ErrSerialization = errors.New("serialization error")

type Method uint8

const (
	Ping Method = iota + 1
	Pong
)

type Request struct {
	Method Method
	ConnID string
}

func (r *Request) Encode() []byte {
	// 1 byte is for method because it is uint8
	// and other is length of connection id.
	buf := make([]byte, 1+len(r.ConnID))
	buf[0] = byte(r.Method)
	copy(buf[1:], []byte(r.ConnID))
	return buf
}

func (r *Request) Decode(buf []byte) error {
	if buf == nil {
		return fmt.Errorf("buf is nil: %w", ErrSerialization)
	}
	r.Method = Method(buf[0])
	if r.Method == 0 {
		return fmt.Errorf("rpc method is zero: %w", ErrSerialization)
	}
	r.ConnID = string(buf[1:])
	return nil
}
