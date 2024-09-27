package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"ttun/internal/logutils"
)

const NetworkBufferSize = 512

// It is okay to use global variable as it is atomic
// and we use it globally to stop all loops
// and avoid using bypassing to every function.
//
//nolint:gochecknoglobals // see comment aboce
var StopSignal = &atomic.Bool{}

type RPCMethod string

const (
	Ping RPCMethod = "ping"
	Pong RPCMethod = "pong"
)

type RPCRequest struct {
	Method RPCMethod
	ConnID string
}

type Handler interface {
	ID() string
	Handle(fd int, logger *slog.Logger) error
}

func main() {
	handler := logutils.NewSlogHandler(os.Stdout, &slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: true,
	})
	slog.SetDefault(slog.New(handler))

	if len(os.Args) != 2 {
		slog.Error("incorrect number of os arguments", "length", len(os.Args)-1)
		os.Exit(1)
	}

	group := new(errgroup.Group)
	switch os.Args[1] {
	case "local":
		group.Go(ConnectToProxy)
	case "public":
		handshakeReqC := make(chan string)
		handshakeResC := make(map[string]chan int)
		closeProxyFd := make(map[string]chan struct{})

		cl := &Listener[*ClientHandler]{
			Port: 22000,
			Handler: &ClientHandler{
				handshakeReqC: handshakeReqC,
			}}
		group.Go(cl.Listen)

		pl := &Listener[*ProxyHandler]{
			Port: 32345,
			Handler: &ProxyHandler{
				handshakeResC: handshakeResC,
				closeProxyFd:  closeProxyFd,
			},
		}
		group.Go(pl.Listen)

		il := &Listener[*IncomingHandler]{
			Port: 14600,
			Handler: &IncomingHandler{
				handshakeReqC: handshakeReqC,
				handshakeResC: handshakeResC,
				closeProxyFd:  closeProxyFd,
			},
		}
		group.Go(il.Listen)
	default:
		slog.Error("unknown argument to start ttun", "arg", os.Args[1])
		os.Exit(1)
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	signalName := (<-interrupt).String()
	slog.Debug("received os signal", "signal", signalName)
	StopSignal.Store(true)

	if err := group.Wait(); err != nil {
		slog.Error("failed to wait for threads", "error", err)
		os.Exit(1)
	}

	// todo: make log
}

func ConnectToProxy() error {
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

		req := RPCRequest{}
		if err = json.Unmarshal(buf, &req); err != nil {
			return fmt.Errorf("failed to unmarshal new rpc request from proxy server: %w", err)
		}
		// todo: check that method is ping
		slog.Debug("new rpc request from proxy", "request", req)

		conn := &ProxyConnection{ConnID: req.ConnID}
		group.Go(conn.Init)
	}
	// todo: uncomment
	// if err = group.Wait(); err != nil {
	// 	return fmt.Errorf("failed to wait for all proxy connections: %w", err)
	// }

	// return nil
}

type ProxyConnection struct {
	ConnID string
}

func (c *ProxyConnection) Init() error {
	logger := slog.With("conn_id", c.ConnID)
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

	req := RPCRequest{
		Method: Pong,
		ConnID: c.ConnID,
	}
	buf, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to encode rpc request: %w", err)
	}
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
	Port    int
	Handler T
}

func (l *Listener[T]) Listen() error {
	logger := slog.With("id", l.Handler.ID())

	socket, err := InitSocket()
	if err != nil {
		return fmt.Errorf("failed to init socket: %w", err)
	}
	if err = Listen(socket, l.Port); err != nil {
		return fmt.Errorf("failed to listen socket: %w", err)
	}
	logger.Debug("listen for connections to socket", "port", l.Port)

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
		group.Go(InitHandler(fd, addr, l.Handler, logger))
	}
	if err = group.Wait(); err != nil {
		return fmt.Errorf("failed to wait for connections: %w", err)
	}

	return nil
}

type ClientHandler struct {
	handshakeReqC chan string
}

func (h *ClientHandler) ID() string { return "client-handler" }

// todo: make sure we have only one active connection there
func (h *ClientHandler) Handle(fd int, logger *slog.Logger) error {
	for {
		// todo: where to close handshake channel?
		connID, ok := <-h.handshakeReqC
		if !ok {
			// todo: how to close all proxy connections?
			// todo: do we need to close all proxy connectiond?
			logger.Debug("handshake request channel was closed")
			break
		}
		logger = logger.With("conn_id", connID)
		logger.Debug("received new handshake")
		req := RPCRequest{
			Method: Ping,
			ConnID: connID,
		}
		buf, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("failed to encode ping rpc request: %w", err)
		}
		if _, err = syscall.Write(fd, buf); err != nil {
			return fmt.Errorf("failed to write ping message to fd: %w", err)
		}
		logger.Debug("rpc request to client was sent")
	}
	return nil
}

type ProxyHandler struct {
	handshakeResC map[string]chan int
	closeProxyFd  map[string]chan struct{}
}

func (h *ProxyHandler) ID() string { return "proxy-handler" }

func (h *ProxyHandler) Handle(fd int, _ *slog.Logger) error {
	buf := make([]byte, NetworkBufferSize)
	n, err := syscall.Read(fd, buf)
	if err != nil {
		return fmt.Errorf("failed to read from fd: %w", err)
	}
	req := RPCRequest{}
	if err = json.Unmarshal(buf[:n], &req); err != nil {
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
	handshakeReqC chan string
	handshakeResC map[string]chan int
	closeProxyFd  map[string]chan struct{}
}

func (h *IncomingHandler) ID() string { return "incoming-handler" }

func (h *IncomingHandler) Handle(fd int, logger *slog.Logger) error {
	connID := RandomString(12)
	logger = logger.With("conn_id", connID)

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
		connLogger := logger.With("fd", fd, "addr", addr)
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

func RandomString(n int) string {
	letterRunes := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}
