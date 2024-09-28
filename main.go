package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"golang.org/x/sync/errgroup"

	"ttun/internal/logutils"
	"ttun/internal/rpc"
	"ttun/internal/types"
)

const NetworkBufferSize = 128

// It is okay to use global variable as it is atomic,
// and we use it globally to stop all loops
// and avoid using bypassing to every function.
//
//nolint:gochecknoglobals // see comment above
var StopSignal = &atomic.Bool{}

type Handler interface {
	ID() string
	Handle(fd int, logger *slog.Logger) error
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
	handshakeReqC := make(chan types.ConnID)
	handshakeResC := make(map[types.ConnID]chan int)
	closeProxyFd := make(map[types.ConnID]chan struct{})

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

type CLI struct {
	Client ClientCmd `cmd:"" help:"Start client side."`
	Server ServerCmd `cmd:"" help:"Start server side."`
}

func main() {
	slogHandler := logutils.NewSlogHandler(
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

type ProxyConnection struct{}

func (c *ProxyConnection) Connect() error {
	addr, err := net.ResolveTCPAddr("tcp", "172.17.0.3:22000")
	if err != nil {
		return fmt.Errorf("failed to resolve proxy address: %w", err)
	}
	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		return fmt.Errorf("failed to connect to the proxy: %w", err)
	}
	defer func() {
		if err = conn.Close(); err != nil {
			slog.Error("failed to close socket with proxy", "error", err)
		}
	}()
	slog.Debug("connected to proxy")

	group := new(errgroup.Group)
	// todo: how to exit loop below
	for {
		buf := make([]byte, NetworkBufferSize)
		n, err := conn.Read(buf)
		if err != nil {
			return fmt.Errorf("failed to read from proxy: %w", err)
		}
		buf = buf[:n]

		req := rpc.Request{}
		if err := req.Decode(buf); err != nil {
			return fmt.Errorf("failed to decode new rpc request from proxy: %w", err)
		}
		// todo: check that method is ping
		slog.Debug("new rpc request from proxy", "request", req.String())

		conn := &IncomingConnection{connID: req.ConnID}
		group.Go(conn.Init)
	}
	// todo: uncomment
	// if err = group.Wait(); err != nil {
	// 	return fmt.Errorf("failed to wait for all proxy connections: %w", err)
	// }

	// return nil
}

type IncomingConnection struct {
	connID types.ConnID
}

func (c *IncomingConnection) Init() error {
	logger := slog.With("conn_id", c.connID.String())
	logger.Debug("proxy requested new connection")

	proxyAddr, err := net.ResolveTCPAddr("tcp", "172.17.0.3:32345")
	if err != nil {
		return fmt.Errorf("failed to resolve proxy address: %w", err)
	}
	proxyConn, err := net.DialTCP("tcp", nil, proxyAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to the proxy: %w", err)
	}
	defer func() {
		if err = proxyConn.Close(); err != nil {
			logger.Error("failed to close proxy connection", "error", err)
		}
	}()

	req := rpc.Request{
		Method: rpc.Pong,
		ConnID: c.connID,
	}
	buf := req.Encode()
	if _, err = proxyConn.Write(buf); err != nil {
		return fmt.Errorf("failed to write to proxy rpc request: %w", err)
	}

	targetAddr, err := net.ResolveTCPAddr("tcp", "172.17.0.2:44000")
	if err != nil {
		return fmt.Errorf("failed to resolve target address: %w", err)
	}
	targetConn, err := net.DialTCP("tcp", nil, targetAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to the target: %w", err)
	}
	defer func() {
		if err = targetConn.Close(); err != nil {
			logger.Error("failed to close target connection", "error", err)
		}
	}()

	proxyFile, err := proxyConn.File()
	if err != nil {
		return fmt.Errorf("failed to get proxy fd: %w", err)
	}
	proxyFd := proxyFile.Fd()

	targetFile, err := targetConn.File()
	if err != nil {
		return fmt.Errorf("failed to get target fd: %w", err)
	}
	targetFd := targetFile.Fd()

	logger.Debug("start copy streams")
	if err = CopyStreams(proxyFd, targetFd); err != nil {
		return fmt.Errorf("failed to copy streams: %w", err)
	}

	return nil
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
		group.Go(InitHandler(fd, addr, l.handler, logger))
	}
	if err = group.Wait(); err != nil {
		return fmt.Errorf("failed to wait for connections: %w", err)
	}

	return nil
}

type ClientHandler struct {
	handshakeReqC chan types.ConnID
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
		req := rpc.Request{
			Method: rpc.Ping,
			ConnID: connID,
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
	handshakeResC map[types.ConnID]chan int
	closeProxyFd  map[types.ConnID]chan struct{}
}

func (h *ProxyHandler) ID() string { return "proxy-handler" }

func (h *ProxyHandler) Handle(fd int, _ *slog.Logger) error {
	buf := make([]byte, NetworkBufferSize)
	n, err := syscall.Read(fd, buf)
	if err != nil {
		return fmt.Errorf("failed to read from fd: %w", err)
	}
	req := rpc.Request{}
	if err = req.Decode(buf[:n]); err != nil {
		return fmt.Errorf("failed to decode request: %w", err)
	}
	// todo: check that it is pong message

	handshakeResC, ok := h.handshakeResC[req.ConnID]
	if !ok {
		// todo: very bad
	}

	closeC := make(chan struct{})
	h.closeProxyFd[req.ConnID] = closeC
	// todo: who should close and delete this channel from map?
	handshakeResC <- fd
	// todo: who should close and delete this channel from map?
	// todo: check that it is not closed
	<-closeC

	// todo: make log

	return nil
}

type IncomingHandler struct {
	handshakeReqC chan types.ConnID
	handshakeResC map[types.ConnID]chan int
	closeProxyFd  map[types.ConnID]chan struct{}
}

func (h *IncomingHandler) ID() string { return "incoming-handler" }

func (h *IncomingHandler) Handle(fd int, logger *slog.Logger) error {
	connID, err := types.RandomConnID()
	if err != nil {
		return fmt.Errorf("failed to get random connection id: %w", err)
	}
	logger = logger.With("conn_id", connID.String())

	resC := make(chan int)         // todo: where to close this channel?
	h.handshakeResC[connID] = resC // todo: how to clear map

	h.handshakeReqC <- connID
	logger.Debug("handshake request was sent")
	nfd := <-resC // todo: rename
	// todo: do we need to close nfd here?
	logger.Debug("received client fd", "nfd", nfd)

	if err := CopyStreams(fd, nfd); err != nil {
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

func CopyData[T int | uintptr](srcFd, dstFd T, errC chan error) {
	logger := slog.With("src_fd", srcFd, "dst_fd", dstFd)
	buf := make([]byte, NetworkBufferSize)
	for {
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

func CopyStreams[T int | uintptr](srcFd, dstFd T) error {
	errC := make(chan error)
	go CopyData(srcFd, dstFd, errC)
	go CopyData(dstFd, srcFd, errC)
	var lastErr error
	for i := 0; i < 2; i++ {
		if err := <-errC; err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func InitHandler(
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
