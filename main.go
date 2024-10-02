package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"golang.org/x/sync/errgroup"
)

const NetworkBufSize = 256

func main() {
	cmd := &Cmd{}
	kctx := kong.Parse(cmd)

	slogHandler := NewSlogHandler(
		os.Stdout,
		&slog.HandlerOptions{
			Level:     slog.LevelDebug,
			AddSource: true,
		},
		cmd.JsonLogs,
	)
	slog.SetDefault(slog.New(slogHandler))

	if err := kctx.Run(); err != nil {
		slog.Error("failed to run command", "error", err)
		os.Exit(1)
	}
}

type Cmd struct {
	JsonLogs bool `default:"false" help:"Print logs in json."`

	Client CmdClient `cmd:"" help:"Start client side."`
	Server CmdServer `cmd:"" help:"Start server side."`
}

type CmdClient struct {
	ProxyAddress    string `default:"172.17.0.3:22000" help:"Set server proxy address."`
	IncomingAddress string `default:"172.17.0.3:32345" help:"Set server incoming address."`
	TargetAddress   string `default:"172.17.0.2:44000" help:"Set target address."`
}

func (cmd *CmdClient) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errC := make(chan error)
	defer close(errC)

	interrupt := make(chan os.Signal, 1)
	defer close(interrupt)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	go func() {
		errC <- InitProxyConnection(ctx,
			cmd.ProxyAddress, cmd.IncomingAddress, cmd.TargetAddress)
	}()

	select {
	case sig := <-interrupt:
		slog.DebugContext(ctx, "received os signal", "signal", sig.String())
		// To close all goroutines and streams copying.
		cancel()
		if err := <-errC; err != nil {
			return fmt.Errorf("failed to wait for proxy connection: %w", err)
		}
	case err := <-errC:
		if err != nil {
			return fmt.Errorf("failed to establish proxy connection: %w", err)
		}
	}

	return nil
}

type CmdServer struct {
	ClientPort   int `default:"22000" help:"Set client port."`
	ProxyPort    int `default:"32345" help:"Set incoming port."`
	IncomingPort int `default:"14600" help:"Set incoming port."`
}

func (cmd *CmdServer) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	incomingReqC := make(chan ConnID)
	incomingResC := make(map[ConnID]chan int)
	closeProxyFd := make(map[ConnID]chan struct{})

	cl := &Listener[*ClientHandler]{
		port: cmd.ClientPort,
		handler: &ClientHandler{
			incomingReqC: incomingReqC,
		},
		// We want to have only one active client at one moment.
		maxConns: 1,
	}
	pl := &Listener[*ProxyHandler]{
		port: cmd.ProxyPort,
		handler: &ProxyHandler{
			incomingResC: incomingResC,
			closeProxyFd: closeProxyFd,
		},
		maxConns: math.MaxInt,
	}
	il := &Listener[*IncomingHandler]{
		port: cmd.IncomingPort,
		handler: &IncomingHandler{
			incomingReqC: incomingReqC,
			incomingResC: incomingResC,
			closeProxyFd: closeProxyFd,
		},
		maxConns: math.MaxInt,
	}

	errC := make(chan error)
	defer close(errC)

	go func() { errC <- cl.Listen(ctx, true) }()
	go func() { errC <- pl.Listen(ctx, false) }()
	go func() { errC <- il.Listen(ctx, false) }()

	interrupt := make(chan os.Signal, 1)
	defer close(interrupt)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	select {
	case err := <-errC:
		slog.ErrorContext(ctx, "received an error from channel", "error", err)
		cancel()
		// As one listener is already stopped as we received an error from the channel.
		// We need to wait only for two remaining listeners.
		DrainErrC(ctx, errC, 2)
	case sig := <-interrupt:
		slog.DebugContext(ctx, "received os signal", "signal", sig.String())
		cancel()
		// We want to wait for all three listeners.
		DrainErrC(ctx, errC, 3)
	}

	return nil
}

func DrainErrC(ctx context.Context, errC chan error, remaining int) {
	for i := 0; i < remaining; i++ {
		if err := <-errC; err != nil {
			slog.ErrorContext(ctx, "received error from channel while draining", "error", err)
		}
	}
}

func InitProxyConnection(ctx context.Context, proxyAddress, incomingAddress, targetAddress string) error {
	proxyAddr, err := net.ResolveTCPAddr("tcp", proxyAddress)
	if err != nil {
		return fmt.Errorf("failed to resolve proxy address: %w", err)
	}
	ctx = CtxWithAttr(ctx, slog.String("proxy_address", proxyAddress))
	incomingAddr, err := net.ResolveTCPAddr("tcp", incomingAddress)
	if err != nil {
		return fmt.Errorf("failed to resolve incoming address: %w", err)
	}
	ctx = CtxWithAttr(ctx, slog.String("incoming_address", incomingAddress))
	targetAddr, err := net.ResolveTCPAddr("tcp", targetAddress)
	if err != nil {
		return fmt.Errorf("failed to resolve target address: %w", err)
	}
	ctx = CtxWithAttr(ctx, slog.String("target_address", targetAddress))

	socket, err := Dial(ctx, proxyAddr, true)
	if err != nil {
		return fmt.Errorf("failed to dial to proxy: %w", err)
	}
	defer func() {
		if err = syscall.Close(socket); err != nil {
			slog.ErrorContext(ctx, "failed to close proxy connection socket", "error", err)
		}
	}()
	slog.InfoContext(ctx, "successfully connected to proxy")

	buf := make([]byte, NetworkBufSize)
	helloT := time.NewTimer(time.Second)
	group := new(errgroup.Group)
	for {
		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "received stop signal, wait for proxy goroutines")
			if err = group.Wait(); err != nil {
				return fmt.Errorf("failed to wait for all proxy connections: %w", err)
			}
			slog.DebugContext(ctx, "all goroutines and proxy connections are stopped")
			return nil
		case <-helloT.C:
			// Sometimes socket can "establish" connection with proxy server,
			// 	but actually there are no real connection between them.
			// To be sure that connection is really established we need to wait
			// 	for hello message from the server.
			// If there are no message until timer is up, it means we are not connected.
			slog.InfoContext(ctx, "hello message from the server is not received")
			return nil
		default:
		}

		n, err := syscall.Read(socket, buf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(time.Millisecond * 5)
				continue
			}
			return fmt.Errorf("failed to read from socket: %w", err)
		}
		if n == 0 {
			slog.InfoContext(ctx, "connection with proxy is closed")
			return nil
		}
		buf = buf[:n]

		req := RPCRequest{}
		if err = req.Decode(buf); err != nil {
			return fmt.Errorf("failed to decode new rpc request from proxy: %w", err)
		}
		switch req.method {
		case Hello:
			// As we received hello message from the server,
			//  it means we are successfully connected and can receive
			//  rpc requests from the server.
			// So we need to stop timer to not exit server loop.
			helloT.Stop()
		case Busy:
			// It means other client is already connected to the server.
			slog.InfoContext(ctx, "server is busy with another client")
			return nil
		case Ping:
			req = RPCRequest{method: Pong}
			buf = req.Encode()
			if _, err = syscall.Write(socket, buf); err != nil {
				return fmt.Errorf("failed to write pong response: %w", err)
			}
		case Connect:
			slog.DebugContext(ctx, "init incoming connection", "conn_id", req.connID)
			group.Go(func() error {
				return InitIncomingConnection(ctx,
					req.connID, incomingAddr, targetAddr)
			})
		default:
			slog.DebugContext(ctx, "rpc request method from proxy is not connect, skip it", "method", req.method)
		}
	}
}

func InitIncomingConnection(ctx context.Context, connID ConnID, incomingAddress, targetAddress *net.TCPAddr) error {
	ctx = CtxWithAttr(ctx, slog.String("conn_id", connID.String()))
	slog.DebugContext(ctx, "proxy requested new connection")

	incomingSocket, err := Dial(ctx, incomingAddress, false)
	if err != nil {
		return fmt.Errorf("failed to dial incoming: %w", err)
	}
	defer func() {
		slog.DebugContext(ctx, "close incoming socket")
		if err = syscall.Close(incomingSocket); err != nil {
			slog.ErrorContext(ctx, "failed to close incoming socket", "error", err)
		}
	}()
	slog.DebugContext(ctx, "incoming socket is established", "fd", incomingSocket)

	targetSocket, err := Dial(ctx, targetAddress, false)
	if err != nil {
		return fmt.Errorf("failed to dial target: %w", err)
	}
	defer func() {
		slog.DebugContext(ctx, "close target socket")
		if err = syscall.Close(targetSocket); err != nil {
			slog.ErrorContext(ctx, "failed to close target socket", "error", err)
		}
	}()
	slog.DebugContext(ctx, "target socket is established", "fd", targetSocket)

	req := RPCRequest{
		method: Ack,
		connID: connID,
	}
	buf := req.Encode()
	if _, err = syscall.Write(incomingSocket, buf); err != nil {
		return fmt.Errorf("failed to write to incoming rpc request: %w", err)
	}

	CopyStreams(ctx, incomingSocket, targetSocket)
	return nil
}

type Handler interface {
	ID() string
	Handle(ctx context.Context, fd int) error
	Hello(ctx context.Context, fd int) error
	Close()
}

type Listener[T Handler] struct {
	port     int
	handler  T
	maxConns int
}

func (l *Listener[T]) Listen(ctx context.Context, nonblock bool) error {
	ctx = CtxWithAttr(ctx, slog.String("id", l.handler.ID()))
	defer l.handler.Close()

	socket, err := CreateSocket(true)
	if err != nil {
		return fmt.Errorf("failed to create socket: %w", err)
	}
	if err = ListenOnSocket(socket, l.port); err != nil {
		return fmt.Errorf("failed to listen socket: %w", err)
	}
	slog.DebugContext(ctx, "listen for connections to socket", "port", l.port)

	group := new(errgroup.Group)
	group.SetLimit(l.maxConns)
	for {
		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "break listen array because of stop signal")
			if err = group.Wait(); err != nil {
				slog.ErrorContext(ctx, "failed to wait for goroutines", "error", err)
			}
			return nil
		default:
		}

		fd, addr, err := syscall.Accept(socket)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(time.Millisecond * 5)
				continue
			}
			return fmt.Errorf("failed to accept new connection: %w", err)
		}
		if nonblock {
			if err = syscall.SetNonblock(fd, true); err != nil {
				return fmt.Errorf("failed to set fd as nonblock: %w", err)
			}
		}

		ok := group.TryGo(InitListenerHandler(ctx, fd, addr, l.handler))
		if !ok {
			ReplyBusy(ctx, fd)
			continue
		}
	}
}

func ReplyBusy(ctx context.Context, fd int) {
	slog.WarnContext(ctx, "cannot init new connection because of goroutines limit")
	rpc := RPCRequest{method: Busy}
	buf := rpc.Encode()
	if _, err := syscall.Write(fd, buf); err != nil {
		slog.ErrorContext(ctx, "failed to write busy message to fd", "error", err)
	}
	if err := syscall.Close(fd); err != nil {
		slog.ErrorContext(ctx, "failed to close fd", "error", err)
	}
}

func InitListenerHandler(
	ctx context.Context, fd int, addr syscall.Sockaddr, handler Handler,
) func() error {

	return func() error {
		ctx = CtxWithAttr(ctx, slog.Int("fd", fd))
		ctx = CtxWithAttr(ctx, slog.String("addr", SockaddrToString(addr)))
		slog.DebugContext(ctx, "new connection")
		defer func() {
			if err := syscall.Close(fd); err != nil {
				slog.ErrorContext(ctx, "failed to close fd", "error", err)
				return
			}
			slog.DebugContext(ctx, "connection fd is successfully closed")
		}()
		if err := handler.Hello(ctx, fd); err != nil {
			return fmt.Errorf("failed to say hello: %w", err)
		}
		return handler.Handle(ctx, fd)
	}
}

type ClientHandler struct {
	incomingReqC chan ConnID
}

func (h *ClientHandler) ID() string { return "client-handler" }

func (h *ClientHandler) Handle(ctx context.Context, fd int) error {
	t := time.NewTimer(time.Second)
	defer t.Stop()

	req := RPCRequest{method: Ping}
	reqBuf := req.Encode()
	resBuf := make([]byte, NetworkBufSize)

	for {
		select {
		case <-t.C:
			if _, err := syscall.Write(fd, reqBuf); err != nil {
				return fmt.Errorf("failed to ping client: %w", err)
			}
			for {
				n, err := syscall.Read(fd, resBuf)
				if errors.Is(err, syscall.EAGAIN) {
					time.Sleep(time.Millisecond * 5)
					continue
				}
				if err != nil {
					return fmt.Errorf("failed to read from socket: %w", err)
				}
				if n == 0 {
					slog.DebugContext(ctx, "client was disconnected")
					return nil
				}
				resBuf = resBuf[:n]
				req = RPCRequest{}
				if err = req.Decode(resBuf); err != nil {
					return fmt.Errorf("failed to decode rpc request (maybe pong): %w", err)
				}
				if req.method != Pong {
					slog.ErrorContext(ctx, "response method should be pong after ping request", "method", req.method)
					return nil
				}
				break
			}
			t.Reset(time.Second)
		case connID, ok := <-h.incomingReqC:
			// So new incoming connection is established.
			// We need to ask client to open new connection to start data proxy streams.
			if !ok {
				slog.DebugContext(ctx, "incoming request channel was closed")
				return nil
			}
			ctx = CtxWithAttr(ctx, slog.String("conn_id", connID.String()))
			slog.DebugContext(ctx, "received new incoming request")
			req := RPCRequest{
				method: Connect,
				connID: connID,
			}
			buf := req.Encode()
			if _, err := syscall.Write(fd, buf); err != nil {
				return fmt.Errorf("failed to write connect message to fd: %w", err)
			}
			slog.DebugContext(ctx, "rpc request to client was sent")
		}
	}
}

func (h *ClientHandler) Hello(ctx context.Context, fd int) error {
	rpc := RPCRequest{method: Hello}
	buf := rpc.Encode()
	if _, err := syscall.Write(fd, buf); err != nil {
		return fmt.Errorf("failed to write hello message to fd: %w", err)
	}
	slog.DebugContext(ctx, "hello message was sent")
	return nil
}

func (h *ClientHandler) Close() {}

type ProxyHandler struct {
	incomingResC map[ConnID]chan int
	closeProxyFd map[ConnID]chan struct{}
}

func (h *ProxyHandler) ID() string { return "proxy-handler" }

func (h *ProxyHandler) Handle(ctx context.Context, fd int) error {
	buf := make([]byte, NetworkBufSize)
	for {
		n, err := syscall.Read(fd, buf)
		if errors.Is(err, syscall.EAGAIN) {
			time.Sleep(time.Millisecond * 5)
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to read from fd: %w", err)
		}
		buf = buf[:n]
		break
	}
	req := RPCRequest{}
	if err := req.Decode(buf); err != nil {
		return fmt.Errorf("failed to decode request: %w", err)
	}
	// Client should answer that target connection is established and it is ready to proxy traffic.
	if req.method != Ack {
		slog.DebugContext(ctx, "rpc request message is not ack", "method", req.method)
		return nil
	}

	incomingResC := h.incomingResC[req.connID]
	defer func() {
		close(incomingResC)
		delete(h.incomingResC, req.connID)
	}()

	closeProxyFd := make(chan struct{})
	h.closeProxyFd[req.connID] = closeProxyFd

	incomingResC <- fd

	<-closeProxyFd

	return nil
}

func (h *ProxyHandler) Hello(context.Context, int) error { return nil }

func (h *ProxyHandler) Close() {
	// Only this handler writes to this channel so it should close it.
	for _, ch := range h.incomingResC {
		close(ch)
	}
}

type IncomingHandler struct {
	incomingReqC chan ConnID
	incomingResC map[ConnID]chan int
	closeProxyFd map[ConnID]chan struct{}
}

func (h *IncomingHandler) ID() string { return "incoming-handler" }

func (h *IncomingHandler) Handle(ctx context.Context, fd int) error {
	connID := RandomConnID()
	ctx = CtxWithAttr(ctx, slog.String("conn_id", connID.String()))

	// We need to set up and save this channel,
	// before send incoming request to client handler,
	// otherwise there will be panic in client handler,
	// because it will not find a proper channel with exact connection id.
	incomingResC := make(chan int)
	h.incomingResC[connID] = incomingResC

	// Send request to establish new proxy connection.
	h.incomingReqC <- connID
	slog.DebugContext(ctx, "new incoming request was sent")

	// New client is connected to proxy server and there is fd.
	proxyFd := <-incomingResC
	slog.DebugContext(ctx, "received proxy fd", "proxy_fd", proxyFd)

	closeProxyFd := h.closeProxyFd[connID]
	defer func() {
		// Notify proxy connection that streams are done and connection can be closed.
		closeProxyFd <- struct{}{}
		close(closeProxyFd)
		delete(h.closeProxyFd, connID)
	}()

	CopyStreams(ctx, fd, proxyFd)
	return nil
}

func (h *IncomingHandler) Hello(context.Context, int) error { return nil }

func (h *IncomingHandler) Close() {
	// This function is called when all incoming connections are closed.
	// So we need to close this channel because no one will write to it.
	close(h.incomingReqC)
	// Only this handler writes to this channel so it should close it.
	for _, ch := range h.closeProxyFd {
		close(ch)
	}
}

func CopyData[T int | uintptr](ctx context.Context, srcFd, dstFd T, errC chan error) {
	ctx = CtxWithAttr(ctx, slog.Int("src_fd", int(srcFd)))
	ctx = CtxWithAttr(ctx, slog.Int("dst_fd", int(dstFd)))

	defer ShutdownSocket(ctx, dstFd)

	buf := make([]byte, NetworkBufSize)
	for {
		select {
		// Received stop os signal.
		case <-ctx.Done():
			slog.DebugContext(ctx, "stop signal received on reading, stop copying data")
			errC <- nil
			return
		default:
		}

		n, err := syscall.Read(int(srcFd), buf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(time.Millisecond * 5)
				continue
			}
			errC <- PrepareCopyDataErr("failed to read from source socket", err, srcFd, dstFd)
			return
		}
		if n == 0 {
			slog.DebugContext(ctx, "connection with source socket is closed")
			errC <- nil
			return
		}
		buf = buf[:n]

		for {
			select {
			case <-ctx.Done():
				slog.DebugContext(ctx, "stop signal received on writing, stop copying data")
				errC <- nil
				return
			default:
			}

			_, err = syscall.Write(int(dstFd), buf)
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(time.Millisecond * 5)
				continue
			}
			if err != nil {
				errC <- PrepareCopyDataErr("failed to write to destination socket", err, srcFd, dstFd)
				return
			}
			break
		}
	}
}

func PrepareCopyDataErr[T int | uintptr](message string, err error, srcFd, dstFd T) error {
	return fmt.Errorf("%s: src_fd=%d;dst_fd=%d: %w", message, srcFd, dstFd, err)
}

func CopyStreams[T int | uintptr](ctx context.Context, srcFd, dstFd T) {
	errC := make(chan error)
	defer close(errC)
	go CopyData(ctx, srcFd, dstFd, errC)
	go CopyData(ctx, dstFd, srcFd, errC)
	slog.DebugContext(ctx, "goroutines to copy streams are started")
	for i := 0; i < 2; i++ {
		if err := <-errC; err != nil {
			slog.ErrorContext(ctx, "received error from copy data goroutine", "error", err)
		}
	}
	slog.DebugContext(ctx, "goroutines to copy streams are stopped")
}

func CreateSocket(nonblock bool) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return 0, fmt.Errorf("failed to create socket: %w", err)
	}
	if nonblock {
		if err = syscall.SetNonblock(fd, true); err != nil {
			return 0, fmt.Errorf("failed to set socket nonblock: %w", err)
		}
	}
	return fd, nil
}

func ListenOnSocket(fd, port int) error {
	addr := syscall.SockaddrInet4{Port: port}
	copy(addr.Addr[:], []byte{0, 0, 0, 0})
	if err := syscall.Bind(fd, &addr); err != nil {
		return fmt.Errorf("failed to bind socket to address: %w", err)
	}
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}
	return nil
}

func ShutdownSocket[T int | uintptr](ctx context.Context, fd T) {
	ctx = CtxWithAttr(ctx, slog.Int("fd", int(fd)))
	slog.DebugContext(ctx, "try to shut down socket")
	if err := syscall.Shutdown(int(fd), syscall.SHUT_WR); err != nil {
		slog.ErrorContext(ctx, "failed to shut down socket", "error", err)
	}
}

func Dial(ctx context.Context, address *net.TCPAddr, nonblocking bool) (int, error) {
	socket, err := CreateSocket(nonblocking)
	if err != nil {
		return 0, fmt.Errorf("failed to create socket: %w", err)
	}
	sockaddr := syscall.SockaddrInet4{Port: address.Port}
	copy(sockaddr.Addr[:], address.IP.To4())
	slog.InfoContext(ctx, "starting to connect to socket")
	for {
		err = syscall.Connect(socket, &sockaddr)
		if errors.Is(err, syscall.EINPROGRESS) {
			slog.DebugContext(ctx, "connecting to socket is in progress", "error", err)
			time.Sleep(time.Millisecond * 100)
			continue
		}
		if errors.Is(err, syscall.EALREADY) {
			slog.DebugContext(ctx, "socket connection is already established", "error", err)
			return socket, nil
		}
		if err != nil {
			return 0, fmt.Errorf("failed to connect to socket: %w", err)
		}
		return socket, nil
	}
}

func SockaddrToString(sa syscall.Sockaddr) string {
	switch v := sa.(type) {
	case *syscall.SockaddrInet4:
		ip := net.IPv4(v.Addr[0], v.Addr[1], v.Addr[2], v.Addr[3])
		return fmt.Sprintf("%s:%d", ip.String(), v.Port)
	default:
		return "unknown address"
	}
}

const ConnIDSize = 12

type ConnID [ConnIDSize]byte

func (c ConnID) String() string { return string(c[:]) }

func RandomConnID() ConnID {
	letterRunes := []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	var buf ConnID
	for i := 0; i < ConnIDSize; i++ {
		buf[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return buf
}

type RPCMethod uint8

const (
	Hello RPCMethod = iota + 1
	Busy
	Ping
	Pong
	Connect
	Ack
)

func (m RPCMethod) String() string {
	switch m {
	case Hello:
		return "hello"
	case Busy:
		return "busy"
	case Ping:
		return "ping"
	case Pong:
		return "pong"
	case Connect:
		return "connect"
	case Ack:
		return "ack"
	default:
		return "unknown"
	}
}

const RPCMethodSize = 1
const RPCRequestSize = RPCMethodSize + ConnIDSize

var ErrSerialization = errors.New("serialization error")

type RPCRequest struct {
	method RPCMethod
	connID ConnID
}

func (r *RPCRequest) Encode() []byte {
	buf := make([]byte, RPCRequestSize)
	buf[0] = byte(r.method)
	copy(buf[RPCMethodSize:], r.connID[:])
	return buf
}

func (r *RPCRequest) Decode(buf []byte) error {
	if buf == nil {
		return fmt.Errorf("buf is nil: %w", ErrSerialization)
	}
	if len(buf) != RPCRequestSize {
		return fmt.Errorf("bad request size: %w", ErrSerialization)
	}
	r.method = RPCMethod(buf[0])
	if r.method == 0 {
		return fmt.Errorf("rpc method is zero: %w", ErrSerialization)
	}
	copy(r.connID[:], buf[RPCMethodSize:RPCRequestSize])
	return nil
}

func (r *RPCRequest) String() string {
	method := r.method.String()
	connID := r.connID.String()
	if connID == "" {
		return fmt.Sprintf("method:%s", method)
	}
	return fmt.Sprintf("method:%s,conn_id:%s", method, connID)
}

type CtxKey string

const CtxKeySlogFields CtxKey = "slog_fields"

func CtxWithAttr(ctx context.Context, attr slog.Attr) context.Context {
	if ctx == nil {
		slog.WarnContext(ctx, "context is nil when try to add attributes")
		ctx = context.Background()
	}
	if attrs, ok := ctx.Value(CtxKeySlogFields).([]slog.Attr); ok {
		attrs = append(attrs, attr)
		return context.WithValue(ctx, CtxKeySlogFields, attrs)
	}
	var attrs []slog.Attr
	attrs = append(attrs, attr)
	return context.WithValue(ctx, CtxKeySlogFields, attrs)
}

type LogEvent struct {
	Time    string            `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Data    map[string]string `json:"data"`
}

type SlogHandler struct {
	slog.Handler
	l        *log.Logger
	attrs    []slog.Attr
	jsonLogs bool
}

func NewSlogHandler(
	out io.Writer,
	opts *slog.HandlerOptions,
	jsonLogs bool,
) *SlogHandler {

	return &SlogHandler{
		Handler:  slog.NewTextHandler(out, opts),
		l:        log.New(out, "", 0),
		attrs:    make([]slog.Attr, 0),
		jsonLogs: jsonLogs,
	}
}

func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(CtxKeySlogFields).([]slog.Attr); ok {
		for _, v := range attrs {
			r.AddAttrs(v)
		}
	}
	for _, v := range h.attrs {
		r.AddAttrs(v)
	}
	if h.jsonLogs {
		return h.printJson(r)
	}
	h.printText(r)
	return nil
}

func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	slogAttrs := h.attrs
	slogAttrs = append(slogAttrs, attrs...)
	return &SlogHandler{
		Handler: h.Handler,
		l:       h.l,
		attrs:   slogAttrs,
	}
}

func (h *SlogHandler) printText(r slog.Record) {
	attrs := []byte(" ")
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, []byte(fmt.Sprintf(`%s="%v" `, a.Key, a.Value.String()))...)
		return true
	})
	// If there are no attributes this byte array is empty.
	if len(attrs) != 1 {
		attrs = attrs[:len(attrs)-1] // remove last space
	}
	timeStr := r.Time.Format(time.RFC3339)
	var level string
	switch r.Level {
	case slog.LevelDebug, slog.LevelError:
		level = r.Level.String()
	default:
		// To make all levels aligned (column text alignment).
		level = r.Level.String() + " "
	}
	level += " "
	h.l.Println(timeStr, level, r.Message, string(attrs))
}

func (h *SlogHandler) printJson(r slog.Record) error {
	attrs := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	logEvent := LogEvent{
		Time:    r.Time.Format(time.RFC3339),
		Level:   strings.ToLower(r.Level.String()),
		Message: r.Message,
		Data:    attrs,
	}
	buf, err := json.Marshal(logEvent)
	if err != nil {
		return fmt.Errorf("failed to marshal log event: %w", err)
	}
	h.l.Println(string(buf))
	return nil
}
