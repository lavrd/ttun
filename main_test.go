package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSuccess(t *testing.T) {
	r := require.New(t)
	testCases := []struct {
		req RPCRequest
		buf []byte
	}{
		{RPCRequest{method: Ping, connID: [12]byte{}}, []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{RPCRequest{method: Pong, connID: [12]byte{}}, []byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{RPCRequest{method: Connect, connID: [12]byte{44}}, []byte{3, 44, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{RPCRequest{method: Ack, connID: [12]byte{55}}, []byte{4, 55, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run("", func(t *testing.T) {
			buf := tc.req.Encode()
			r.Equal(tc.buf, buf)
			req := RPCRequest{}
			err := req.Decode(buf)
			r.NoError(err)
			r.Equal(tc.req, req)
		})
	}
}

func TestBufIsNil(t *testing.T) {
	r := require.New(t)
	req := RPCRequest{}
	err := req.Decode(nil)
	r.Error(err)
	r.ErrorIs(err, ErrSerialization)
	r.ErrorContains(err, "buf is nil")
}

func TestIncorrectBufLength(t *testing.T) {
	r := require.New(t)
	req := RPCRequest{}
	err := req.Decode([]byte{0})
	r.Error(err)
	r.ErrorIs(err, ErrSerialization)
	r.ErrorContains(err, "bad request size")
}

func TestMethodIsZero(t *testing.T) {
	r := require.New(t)
	req := RPCRequest{}
	err := req.Decode([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	r.Error(err)
	r.ErrorIs(err, ErrSerialization)
	r.ErrorContains(err, "rpc method is zero")
}
