package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_Success(t *testing.T) {
	r := require.New(t)
	testCases := []struct {
		req RPCRequest
		buf []byte
	}{
		{RPCRequest{method: Hello, connID: [12]byte{}}, []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{RPCRequest{method: Busy, connID: [12]byte{}}, []byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{RPCRequest{method: Ping, connID: [12]byte{}}, []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{RPCRequest{method: Pong, connID: [12]byte{}}, []byte{4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{RPCRequest{method: Connect, connID: [12]byte{44}}, []byte{5, 44, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{RPCRequest{method: Ack, connID: [12]byte{55}}, []byte{6, 55, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
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

func Test_RequestDecoding(t *testing.T) {
	testCases := []struct {
		buf []byte
		msg string
	}{
		// Buf is nil.
		{nil, "buf is nil"},
		// Incorrect buf length.
		{[]byte{0}, "bad request size"},
		// Method is zero.
		{[]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, "rpc method is zero"},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run("", func(t *testing.T) {
			r := require.New(t)
			req := RPCRequest{}
			err := req.Decode(tc.buf)
			r.Error(err)
			r.ErrorIs(err, ErrSerialization)
			r.ErrorContains(err, tc.msg)
		})
	}
}
