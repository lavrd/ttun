package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"golang.org/x/sync/errgroup"
)

const NetworkBufSize = 128

// It is okay to use global variable as it is atomic,
// and we use it globally to stop all loops
// and avoid using bypassing to every function.
//
//nolint:gochecknoglobals // see comment above
var StopSignal = &atomic.Bool{}

func main() {
	slogHandler := NewSlogHandler(
		os.Stdout,
		&slog.HandlerOptions{
			Level:     slog.LevelDebug,
			AddSource: true,
		})
	slog.SetDefault(slog.New(slogHandler))

	kongCtx := kong.Parse(&CLI{})
	if err := kongCtx.Run(); err != nil {
		slog.Error("failed to run command", "error", err)
		os.Exit(1)
	}
}

type CLI struct {
	Client ClientCmd `cmd:"" help:"Start client side."`
	Server ServerCmd `cmd:"" help:"Start server side."`
}

type ClientCmd struct{}

func (cmd *ClientCmd) Run() error {
	conn := &ProxyConnection{}
	group := new(errgroup.Group)
	group.Go(conn.Connect)
	WaitSignal()
	if err := group.Wait(); err != nil {
		return fmt.Errorf("failed to wait for goroutines: %w", err)
	}
	return nil
}

type ServerCmd struct{}

func (cmd *ServerCmd) Run() error {
	handshakeReqC := make(chan ConnID)
	handshakeResC := make(map[ConnID]chan int)
	closeProxyFd := make(map[ConnID]chan struct{})

	cl := &Listener[*ClientHandler]{
		port: 22000,
		handler: &ClientHandler{
			handshakeReqC: handshakeReqC,
		},
	}
	pl := &Listener[*ProxyHandler]{
		port: 32345,
		handler: &ProxyHandler{
			handshakeResC: handshakeResC,
			closeProxyFd:  closeProxyFd,
		},
	}
	il := &Listener[*IncomingHandler]{
		port: 14600,
		handler: &IncomingHandler{
			handshakeReqC: handshakeReqC,
			handshakeResC: handshakeResC,
			closeProxyFd:  closeProxyFd,
		},
	}

	group := new(errgroup.Group)
	group.Go(cl.Listen)
	group.Go(pl.Listen)
	group.Go(il.Listen)
	WaitSignal()
	if err := group.Wait(); err != nil {
		return fmt.Errorf("failed to wait for goroutines: %w", err)
	}

	return nil
}

type ProxyConnection struct{}

func (c *ProxyConnection) Connect() error {
	socket, err := Dial("172.17.0.3:22000")
	if err != nil {
		return fmt.Errorf("failed to dial to proxy: %w", err)
	}
	defer func() {
		if err = syscall.Close(socket); err != nil {
			slog.Error("failed to close proxy connection socket", "error", err)
		}
	}()
	slog.Info("successfully connected to proxy")

	buf := make([]byte, NetworkBufSize)
	group := new(errgroup.Group)
	for !StopSignal.Load() {
		var n int
		n, err = syscall.Read(socket, buf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(time.Millisecond * 5)
				continue
			}
			return fmt.Errorf("failed to read from socket: %w", err)
		}
		if n == 0 {
			slog.Info("connection with proxy is closed")
			return nil
		}
		buf = buf[:n]

		req := RPCRequest{}
		if err := req.Decode(buf); err != nil {
			return fmt.Errorf("failed to decode new rpc request from proxy: %w", err)
		}
		// todo: check that method is ping
		slog.Debug("new rpc request from proxy", "request", req.String())

		conn := &IncomingConnection{connID: req.connID}
		group.Go(conn.Init)
	}
	slog.Debug("received stop signal, wait for proxy goroutines")
	if err = group.Wait(); err != nil {
		return fmt.Errorf("failed to wait for all proxy connections: %w", err)
	}
	slog.Debug("all goroutines are closed")

	return nil
}

type IncomingConnection struct {
	connID ConnID
}

func (c *IncomingConnection) Init() error {
	logger := slog.With("conn_id", c.connID.String())
	logger.Debug("proxy requested new connection")

	incomingSocket, err := Dial("172.17.0.3:32345")
	if err != nil {
		return fmt.Errorf("failed to dial incoming: %w", err)
	}
	defer func() {
		if err = syscall.Close(incomingSocket); err != nil {
			logger.Error("failed to close incoming socket", "error", err)
		}
	}()

	req := RPCRequest{
		method: Pong,
		connID: c.connID,
	}
	buf := req.Encode()
	if _, err = syscall.Write(incomingSocket, buf); err != nil {
		return fmt.Errorf("failed to write to incoming rpc request: %w", err)
	}

	targetSocket, err := Dial("172.17.0.2:44000")
	if err != nil {
		return fmt.Errorf("failed to dial target: %w", err)
	}
	defer func() {
		if err = syscall.Close(targetSocket); err != nil {
			logger.Error("failed to close target socket", "error", err)
		}
	}()

	logger.Debug("start copy streams")
	if err = CopyStreams(incomingSocket, targetSocket, logger); err != nil {
		return fmt.Errorf("failed to copy streams: %w", err)
	}
	logger.Debug("copy streams is finished")

	return nil
}

type Handler interface {
	ID() string
	Handle(fd int, logger *slog.Logger) error
}

type Listener[T Handler] struct {
	port    int
	handler T
}

func (l *Listener[T]) Listen() error {
	logger := slog.With("id", l.handler.ID())

	socket, err := InitSocket()
	if err != nil {
		return fmt.Errorf("failed to init socket: %w", err)
	}
	if err = Listen(socket, l.port); err != nil {
		return fmt.Errorf("failed to listen socket: %w", err)
	}
	logger.Debug("listen for connections to socket", "port", l.port)

	group := new(errgroup.Group)
	for {
		if StopSignal.Load() {
			logger.Debug("break listen array because of stop signal")
			break
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
		group.Go(InitListenerHandler(fd, addr, l.handler, logger))
	}
	if err = group.Wait(); err != nil {
		return fmt.Errorf("failed to wait for connections: %w", err)
	}

	return nil
}

type ClientHandler struct {
	handshakeReqC chan ConnID
}

func (h *ClientHandler) ID() string { return "client-handler" }

// todo: make sure we have only one active connection there
func (h *ClientHandler) Handle(fd int, logger *slog.Logger) error {
	for {
		// todo: where to close handshake channel?
		connID, ok := <-h.handshakeReqC
		if !ok {
			// todo: how to close all proxy connections?
			// todo: do we need to close all proxy connections?
			logger.Debug("handshake request channel was closed")
			break
		}
		logger = logger.With("conn_id", connID.String())
		logger.Debug("received new handshake")
		req := RPCRequest{
			method: Ping,
			connID: connID,
		}
		buf := req.Encode()
		if _, err := syscall.Write(fd, buf); err != nil {
			return fmt.Errorf("failed to write ping message to fd: %w", err)
		}
		logger.Debug("rpc request to client was sent")
	}
	return nil
}

type ProxyHandler struct {
	handshakeResC map[ConnID]chan int
	closeProxyFd  map[ConnID]chan struct{}
}

func (h *ProxyHandler) ID() string { return "proxy-handler" }

func (h *ProxyHandler) Handle(fd int, _ *slog.Logger) error {
	buf := make([]byte, NetworkBufSize)
	n, err := syscall.Read(fd, buf)
	if err != nil {
		return fmt.Errorf("failed to read from fd: %w", err)
	}
	req := RPCRequest{}
	if err = req.Decode(buf[:n]); err != nil {
		return fmt.Errorf("failed to decode request: %w", err)
	}
	// todo: check that it is pong message

	handshakeResC, ok := h.handshakeResC[req.connID]
	if !ok {
		// todo: very bad
	}

	closeC := make(chan struct{})
	h.closeProxyFd[req.connID] = closeC
	// todo: who should close and delete this channel from map?
	handshakeResC <- fd
	// todo: who should close and delete this channel from map?
	// todo: check that it is not closed
	<-closeC

	// todo: make log

	return nil
}

type IncomingHandler struct {
	handshakeReqC chan ConnID
	handshakeResC map[ConnID]chan int
	closeProxyFd  map[ConnID]chan struct{}
}

func (h *IncomingHandler) ID() string { return "incoming-handler" }

func (h *IncomingHandler) Handle(fd int, logger *slog.Logger) error {
	connID := RandomConnID()
	logger = logger.With("conn_id", connID.String())

	resC := make(chan int)         // todo: where to close this channel?
	h.handshakeResC[connID] = resC // todo: how to clear map

	h.handshakeReqC <- connID
	logger.Debug("handshake request was sent")
	nfd := <-resC // todo: rename
	// todo: do we need to close nfd here?
	logger.Debug("received client fd", "nfd", nfd)

	if err := CopyStreams(fd, nfd, logger); err != nil {
		h.closeProxyFd[connID] <- struct{}{}
		return fmt.Errorf("failed to copy streams: %w", err)
	}
	h.closeProxyFd[connID] <- struct{}{}

	return nil
}

func InitSocket() (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return 0, fmt.Errorf("failed init socket: %w", err)
	}
	if err = syscall.SetNonblock(fd, true); err != nil {
		return 0, fmt.Errorf("failed to set nonblock: %w", err)
	}
	return fd, nil
}

func Listen(fd, port int) error {
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

func CopyData[T int | uintptr](srcFd, dstFd T, errC chan error, logger *slog.Logger) {
	logger = logger.With("src_fd", srcFd, "dst_fd", dstFd)
	buf := make([]byte, NetworkBufSize)
	for {
		// Received stop os signal.
		if StopSignal.Load() {
			logger.Debug("stop signal received, stop copying data")
			errC <- nil
			return
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
			logger.Debug("connection with source socket is closed")
			errC <- nil
			return
		}
		if _, err = syscall.Write(int(dstFd), buf[:n]); err != nil {
			errC <- SpawnCopyDataErr("failed to write to destination socket", err, srcFd, dstFd)
			return
		}
	}
}

func SpawnCopyDataErr[T int | uintptr](message string, err error, srcFd, dstFd T) error {
	return fmt.Errorf("%s: src_fd=%d;dst_fd=%d: %w", message, srcFd, dstFd, err)
}

func CopyStreams[T int | uintptr](srcFd, dstFd T, logger *slog.Logger) error {
	errC := make(chan error)
	go CopyData(srcFd, dstFd, errC, logger)
	go CopyData(dstFd, srcFd, errC, logger)
	var lastErr error
	for i := 0; i < 2; i++ {
		if err := <-errC; err != nil {
			lastErr = err
		}
	}
	logger.Debug("copy goroutines are stopped")
	return lastErr
}

func InitListenerHandler(
	fd int, addr syscall.Sockaddr,
	handler Handler,
	logger *slog.Logger,
) func() error {

	return func() error {
		connLogger := logger.With("fd", fd, "addr", SockaddrToString(addr))
		connLogger.Debug("new connection")
		defer func() {
			if err := syscall.Close(fd); err != nil {
				connLogger.Error("failed to close fd", "error", err)
				return
			}
			connLogger.Debug("fd closed")
		}()
		if err := handler.Handle(fd, connLogger); err != nil {
			return fmt.Errorf("failed to handle: %w", err)
		}
		return nil
	}
}

func Dial(address string) (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return 0, fmt.Errorf("failed to resolve tcp address: %w", err)
	}
	socket, err := InitSocket()
	if err != nil {
		return 0, fmt.Errorf("failed to init socket: %w", err)
	}
	sockaddr := syscall.SockaddrInet4{Port: addr.Port}
	copy(sockaddr.Addr[:], addr.IP.To4())
	slog.Info("starting to connect to socket")
	for {
		err = syscall.Connect(socket, &sockaddr)
		if errors.Is(err, syscall.EINPROGRESS) {
			slog.Debug("connecting to socket is in progress", "error", err)
			time.Sleep(time.Millisecond * 100)
			continue
		}
		if errors.Is(err, syscall.EALREADY) {
			slog.Debug("connection is already established", "error", err)
			break
		}
		if err != nil {
			return 0, fmt.Errorf("failed to connect to socket: %w", err)
		}
		break
	}
	return socket, nil
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

func WaitSignal() {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	signalName := (<-interrupt).String()
	slog.Debug("received os signal", "signal", signalName)
	StopSignal.Store(true)
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
	default:
		method = "unknown"
	}
	return fmt.Sprintf("method:%s,conn_id:%s", method, r.connID.String())
}

type CtxKey string

const CtxKeySlogFields CtxKey = "slog_fields"

func CtxWithAttr(ctx context.Context, attr slog.Attr) context.Context {
	if ctx == nil {
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

type SlogHandler struct {
	slog.Handler
	l     *log.Logger
	attrs []slog.Attr
}

func NewSlogHandler(
	out io.Writer,
	opts *slog.HandlerOptions,
) *SlogHandler {

	return &SlogHandler{
		Handler: slog.NewTextHandler(out, opts),
		l:       log.New(out, "", 0),
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
	fields := make(map[string]interface{}, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})
	attrs := []byte(" ")
	for key, value := range fields {
		attrs = append(attrs, []byte(fmt.Sprintf(`%s="%v" `, key, value))...)
	}
	// If there are no attributes this byte array is empty.
	if len(attrs) != 1 {
		attrs = attrs[:len(attrs)-1] // remove last space
	}
	timeStr := r.Time.Format(time.RFC3339)
	level := r.Level.String() + " "
	h.l.Println(timeStr, level, r.Message, string(attrs))
	return nil
}

func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &SlogHandler{
		Handler: h.Handler,
		l:       h.l,
		attrs:   attrs,
	}
}
