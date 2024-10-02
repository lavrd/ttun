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

type CmdClient struct{}

func (cmd *CmdClient) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errC := make(chan error)
	defer close(errC)

	interrupt := make(chan os.Signal, 1)
	defer close(interrupt)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	go func() { errC <- InitProxyConnection(ctx) }()

	select {
	case sig := <-interrupt:
		slog.Debug("received os signal", "signal", sig.String())
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

type CmdServer struct{}

func (cmd *CmdServer) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handshakeReqC := make(chan ConnID)
	handshakeResC := make(map[ConnID]chan int)
	closeProxyFd := make(map[ConnID]chan struct{})

	cl := &Listener[*ClientHandler]{
		port: 22000,
		handler: &ClientHandler{
			handshakeReqC: handshakeReqC,
		},
		// We want to have only one active client at one moment.
		maxConns: 1,
	}
	pl := &Listener[*ProxyHandler]{
		port: 32345,
		handler: &ProxyHandler{
			handshakeResC: handshakeResC,
			closeProxyFd:  closeProxyFd,
		},
		maxConns: math.MaxInt,
	}
	il := &Listener[*IncomingHandler]{
		port: 14600,
		handler: &IncomingHandler{
			handshakeReqC: handshakeReqC,
			handshakeResC: handshakeResC,
			closeProxyFd:  closeProxyFd,
		},
		maxConns: math.MaxInt,
	}

	group := new(errgroup.Group)
	group.Go(func() error { return cl.Listen(ctx, true) })
	group.Go(func() error { return pl.Listen(ctx, false) })
	group.Go(func() error { return il.Listen(ctx, false) })

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	signalName := (<-interrupt).String()
	slog.Debug("received os signal", "signal", signalName)
	cancel()

	if err := group.Wait(); err != nil {
		return fmt.Errorf("failed to wait for goroutines: %w", err)
	}

	return nil
}

func InitProxyConnection(ctx context.Context) error {
	socket, err := Dial(ctx, "172.17.0.3:22000", true)
	if err != nil {
		return fmt.Errorf("failed to dial to proxy: %w", err)
	}
	defer func() {
		if err = syscall.Close(socket); err != nil {
			slog.Error("failed to close proxy connection socket", "error", err)
		}
	}()
	slog.InfoContext(ctx, "successfully connected to proxy")

	buf := make([]byte, NetworkBufSize)
	group := new(errgroup.Group)
	for {
		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "received stop signal, wait for proxy goroutines")
			if err = group.Wait(); err != nil {
				return fmt.Errorf("failed to wait for all proxy connections: %w", err)
			}
			slog.DebugContext(ctx, "all goroutines are closed")
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
		case Ping:
			req = RPCRequest{method: Pong}
			buf = req.Encode()
			if _, err = syscall.Write(socket, buf); err != nil {
				return fmt.Errorf("failed to write pong response: %w", err)
			}
		case Connect:
			slog.DebugContext(ctx, "init incoming connection", "conn_id", req.connID)
			group.Go(func() error { return InitIncomingConnection(ctx, req.connID) })
		default:
			slog.DebugContext(ctx, "rpc request method from proxy is not connect, skip it", "method", req.method)
		}
	}
}

func InitIncomingConnection(ctx context.Context, connID ConnID) error {
	ctx = CtxWithAttr(ctx, slog.String("conn_id", connID.String()))
	slog.DebugContext(ctx, "proxy requested new connection")

	incomingSocket, err := Dial(ctx, "172.17.0.3:32345", false)
	if err != nil {
		return fmt.Errorf("failed to dial incoming: %w", err)
	}
	defer func() {
		if err = syscall.Close(incomingSocket); err != nil {
			slog.ErrorContext(ctx, "failed to close incoming socket", "error", err)
		}
	}()
	slog.DebugContext(ctx, "incoming socket is established", "fd", incomingSocket)

	req := RPCRequest{
		method: Ack,
		connID: connID,
	}
	buf := req.Encode()
	if _, err = syscall.Write(incomingSocket, buf); err != nil {
		return fmt.Errorf("failed to write to incoming rpc request: %w", err)
	}

	targetSocket, err := Dial(ctx, "172.17.0.2:44000", false)
	if err != nil {
		return fmt.Errorf("failed to dial target: %w", err)
	}
	defer func() {
		if err = syscall.Close(targetSocket); err != nil {
			slog.ErrorContext(ctx, "failed to close target socket", "error", err)
		}
	}()
	slog.DebugContext(ctx, "target socket is established", "fd", targetSocket)

	slog.DebugContext(ctx, "start copy streams")
	if err = CopyStreams(ctx, incomingSocket, targetSocket); err != nil {
		return fmt.Errorf("failed to copy streams: %w", err)
	}
	slog.DebugContext(ctx, "copy streams is finished")

	return nil
}

type Handler interface {
	ID() string
	Handle(ctx context.Context, fd int) error
	Close()
}

type Listener[T Handler] struct {
	port     int
	handler  T
	maxConns int
}

func (l *Listener[T]) Listen(ctx context.Context, nonblock bool) error {
	ctx = CtxWithAttr(ctx, slog.String("id", l.handler.ID()))

	socket, err := CreateSocket(true)
	if err != nil {
		return fmt.Errorf("failed to init socket: %w", err)
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
			l.handler.Close()
			return nil
		default:
		}

		var fd int
		var addr syscall.Sockaddr
		fd, addr, err = syscall.Accept(socket)
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
			slog.WarnContext(ctx, "cannot init new connection because of goroutines limit")
			continue
		}
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
			slog.DebugContext(ctx, "fd closed")
		}()
		return handler.Handle(ctx, fd)
	}
}

type ClientHandler struct {
	handshakeReqC chan ConnID
}

func (h *ClientHandler) ID() string { return "client-handler" }

func (h *ClientHandler) Handle(ctx context.Context, fd int) error {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			req := RPCRequest{method: Ping}
			buf := req.Encode()
			if _, err := syscall.Write(fd, buf); err != nil {
				return fmt.Errorf("faield to ping client: %w", err)
			}
			buf = make([]byte, NetworkBufSize)
			for {
				n, err := syscall.Read(fd, buf)
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
				req = RPCRequest{}
				if err = req.Decode(buf[:n]); err != nil {
					return fmt.Errorf("failed to decode rpc request: %w", err)
				}
				if req.method != Pong {
					slog.ErrorContext(ctx, "response method should be pong after ping request", "method", req.method)
					return nil
				}
				break
			}
			t.Reset(time.Second)
		case connID, ok := <-h.handshakeReqC:
			if !ok {
				slog.DebugContext(ctx, "handshake request channel was closed")
				return nil
			}
			ctx = CtxWithAttr(ctx, slog.String("conn_id", connID.String()))
			slog.DebugContext(ctx, "received new handshake")
			req := RPCRequest{
				method: Connect,
				connID: connID,
			}
			buf := req.Encode()
			if _, err := syscall.Write(fd, buf); err != nil {
				return fmt.Errorf("failed to write ping message to fd: %w", err)
			}
			slog.DebugContext(ctx, "rpc request to client was sent")
		}
	}
}

func (h *ClientHandler) Close() {}

type ProxyHandler struct {
	handshakeResC map[ConnID]chan int
	closeProxyFd  map[ConnID]chan struct{}
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
	if req.method != Ack {
		slog.DebugContext(ctx, "rpc request message is not ack", "method", req.method)
		return nil
	}

	handshakeResC := h.handshakeResC[req.connID]
	defer func() {
		close(handshakeResC)
		delete(h.handshakeResC, req.connID)
	}()

	closeProxyFd := make(chan struct{})
	h.closeProxyFd[req.connID] = closeProxyFd

	handshakeResC <- fd

	<-closeProxyFd

	return nil
}

func (h *ProxyHandler) Close() {
	// Only this handler writes to this channel so it should close it.
	for _, ch := range h.handshakeResC {
		close(ch)
	}
}

type IncomingHandler struct {
	handshakeReqC chan ConnID
	handshakeResC map[ConnID]chan int
	closeProxyFd  map[ConnID]chan struct{}
}

func (h *IncomingHandler) ID() string { return "incoming-handler" }

func (h *IncomingHandler) Handle(ctx context.Context, fd int) error {
	connID := RandomConnID()
	ctx = CtxWithAttr(ctx, slog.String("conn_id", connID.String()))

	// We need to set up and save this channel,
	// before send handshake request to client handler,
	// otherwise there will be panic in client handler,
	// because it will not find a proper channel with exact connection id.
	handshakeResC := make(chan int)
	h.handshakeResC[connID] = handshakeResC

	// Send request to establish new proxy connection.
	h.handshakeReqC <- connID
	slog.DebugContext(ctx, "handshake request was sent")

	proxyFd := <-handshakeResC
	slog.DebugContext(ctx, "received proxy fd", "proxy_fd", proxyFd)

	closeProxyFd := h.closeProxyFd[connID]
	defer func() {
		closeProxyFd <- struct{}{}
		close(closeProxyFd)
		delete(h.closeProxyFd, connID)
	}()

	if err := CopyStreams(ctx, fd, proxyFd); err != nil {
		return fmt.Errorf("failed to copy streams: %w", err)
	}

	return nil
}

func (h *IncomingHandler) Close() {
	// This function is called when all incoming connections are closed.
	// So we need to close this channel because no one will write to it.
	close(h.handshakeReqC)
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
		// Received stop os signal.
		select {
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
			errC <- SpawnCopyDataErr("failed to read from source socket", err, srcFd, dstFd)
			return
		}
		if n == 0 {
			slog.DebugContext(ctx, "connection with source socket is closed")
			errC <- nil
			return
		}

		for {
			select {
			case <-ctx.Done():
				slog.DebugContext(ctx, "stop signal received on writing, stop copying data")
				errC <- nil
				return
			default:
			}

			_, err = syscall.Write(int(dstFd), buf[:n])
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(time.Millisecond * 5)
				continue
			}
			if err != nil {
				errC <- SpawnCopyDataErr("failed to write to destination socket", err, srcFd, dstFd)
				return
			}
			break
		}
	}
}

func SpawnCopyDataErr[T int | uintptr](message string, err error, srcFd, dstFd T) error {
	return fmt.Errorf("%s: src_fd=%d;dst_fd=%d: %w", message, srcFd, dstFd, err)
}

func CopyStreams[T int | uintptr](ctx context.Context, srcFd, dstFd T) error {
	errC := make(chan error)
	defer close(errC)
	go CopyData(ctx, srcFd, dstFd, errC)
	go CopyData(ctx, dstFd, srcFd, errC)
	slog.DebugContext(ctx, "copy goroutines are started")
	var lastErr error
	for i := 0; i < 2; i++ {
		if err := <-errC; err != nil {
			slog.ErrorContext(ctx, "received error from copy data goroutine", "error", err)
			lastErr = err
		}
		slog.DebugContext(ctx, "one goroutine is stopped")
	}
	slog.DebugContext(ctx, "copy goroutines are stopped")
	return lastErr
}

func CreateSocket(nonblock bool) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return 0, fmt.Errorf("failed init socket: %w", err)
	}
	if nonblock {
		if err = syscall.SetNonblock(fd, true); err != nil {
			return 0, fmt.Errorf("failed to set nonblock: %w", err)
		}
	}
	return fd, nil
}

func ListenOnSocket(fd, port int) error {
	addr := syscall.SockaddrInet4{Port: port}
	copy(addr.Addr[:], []byte{0, 0, 0, 0})
	if err := syscall.Bind(fd, &addr); err != nil {
		return fmt.Errorf("failed to bind to address: %w", err)
	}
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	return nil
}

func ShutdownSocket[T int | uintptr](ctx context.Context, fd T) {
	ctx = CtxWithAttr(ctx, slog.Int("fd", int(fd)))
	slog.DebugContext(ctx, "shut down socket")
	if err := syscall.Shutdown(int(fd), syscall.SHUT_WR); err != nil {
		slog.ErrorContext(ctx, "failed to shut down socket", "error", err)
	}
}

func Dial(ctx context.Context, address string, nonblocking bool) (int, error) {
	ctx = CtxWithAttr(ctx, slog.String("address", address))
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return 0, fmt.Errorf("failed to resolve tcp address: %w", err)
	}
	socket, err := CreateSocket(nonblocking)
	if err != nil {
		return 0, fmt.Errorf("failed to init socket: %w", err)
	}
	sockaddr := syscall.SockaddrInet4{Port: addr.Port}
	copy(sockaddr.Addr[:], addr.IP.To4())
	slog.InfoContext(ctx, "starting to connect to socket")
	for {
		err = syscall.Connect(socket, &sockaddr)
		if errors.Is(err, syscall.EINPROGRESS) {
			slog.DebugContext(ctx, "connecting to socket is in progress", "error", err)
			time.Sleep(time.Millisecond * 100)
			continue
		}
		if errors.Is(err, syscall.EALREADY) {
			slog.DebugContext(ctx, "connection is already established", "error", err)
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

func (c ConnID) String() string {
	return string(c[:])
}

func RandomConnID() ConnID {
	letterRunes := []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	var buf ConnID
	for i := 0; i < ConnIDSize; i++ {
		buf[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return buf
}

const RPCMethodSize = 1

type RPCMethod uint8

const (
	Ping RPCMethod = iota + 1
	Pong
	Connect
	Ack
)

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
	method := ""
	switch r.method {
	case Ping:
		method = "ping"
	case Pong:
		method = "pong"
	case Connect:
		method = "connect"
	case Ack:
		method = "ack"
	default:
		method = "unknown"
	}
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
		// To make all levels aligned (text).
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
