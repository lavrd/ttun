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

const NetworkBufferSize = 1024

type RPCMethod string

const (
	Ping RPCMethod = "ping"
	Pong RPCMethod = "pong"
)

type RPCRequest struct {
	Method RPCMethod
	ConnID string
}

type Handler func(fd int) func() error

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

	stop := &atomic.Bool{}

	service := &PublicService{
		stop:          stop,
		handshakeReqC: make(chan string),
		handshakeResC: make(map[string]chan int),
		closeProxyFd:  make(map[string]chan struct{}),
	}

	group := new(errgroup.Group)
	switch os.Args[1] {
	case "local":
		group.Go(HandleLocalConnection)
	case "public":
		group.Go(service.Listen(14600, service.HandleIncomingConnection, stop))
		group.Go(service.Listen(22000, service.HandleLocalConnection, stop))
	default:
		slog.Error("unknown argument to start ttun", "arg", os.Args[1])
		os.Exit(1)
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	signalName := (<-interrupt).String()
	slog.Debug("received os signal", "signal", signalName)

	stop.Store(true)

	if err := group.Wait(); err != nil {
		slog.Error("failed to wait for threads", "error", err)
		os.Exit(1)
	}
}

func HandleLocalConnection() error {
	// todo: close fd
	// todo: rewrite using syscalls
	addr, err := net.ResolveTCPAddr("tcp", "172.17.0.3:22000")
	// todo: check error
	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		return fmt.Errorf("failed to connect to the proxy server: %w", err)
	}

	group := new(errgroup.Group)
	// todo: how to exit loop below
	for {
		buf := make([]byte, 4096) // todo: buffer size
		n, err := conn.Read(buf)
		if err != nil {
			return fmt.Errorf("failed to read from proxy server: %w", err)
		}
		buf = buf[:n]
		req := RPCRequest{}
		if err = json.Unmarshal(buf, &req); err != nil {
			return fmt.Errorf("failed to unmarshal new rpc request from proxy server: %w", err)
		}
		// todo: check that method is ping
		group.Go(NewProxyConnection(req.ConnID))
	}
	// todo: uncomment
	// if err = group.Wait(); err != nil {
	// 	return fmt.Errorf("failed to wait for all proxy connections: %w", err)
	// }
	// return nil
}

func NewProxyConnection(connID string) func() error {
	// todo: remove nested
	return func() error {
		// todo: close connection to target and proxy

		proxyAdd, err := net.ResolveTCPAddr("tcp", "172.17.0.3:32345")
		// todo: check error
		proxyConn, err := net.DialTCP("tcp", nil, proxyAdd)
		if err != nil {
			return fmt.Errorf("failed to connect to the proxy server: %w", err)
		}

		req := RPCRequest{
			Method: Pong,
			ConnID: connID,
		}
		buf, err := json.Marshal(req)
		// todo: check error
		_, err = proxyConn.Write(buf)
		if err != nil {
			return fmt.Errorf("failed to write to proxy server: %w", err) // todo: adjust
		}

		targetAddr, err := net.ResolveTCPAddr("tcp", "172.17.0.2:44000")
		// todo: check error
		targetConn, err := net.DialTCP("tcp", nil, targetAddr)
		if err != nil {
			return fmt.Errorf("failed to connect to the target server: %w", err)
		}

		proxyFile, _ := proxyConn.File()
		proxyFd := proxyFile.Fd()

		targetFile, _ := targetConn.File()
		targetFd := targetFile.Fd()

		CopyStreams(proxyFd, targetFd)

		return nil
	}
}

// todo: comment why name is public service?
type PublicService struct {
	stop          *atomic.Bool
	handshakeReqC chan string
	handshakeResC map[string]chan int
	closeProxyFd  map[string]chan struct{}
}

func (s *PublicService) Listen(port int, handler Handler, stop *atomic.Bool) func() error {
	// todo: remove nested
	return func() error {
		incomingSocket, err := InitSocket() // todo: rename
		if err != nil {
			return fmt.Errorf("failed to init incoming socket: %w", err) // todo: rename
		}
		if err = Listen(incomingSocket, port); err != nil {
			return fmt.Errorf("failed to listen incoming socket: %w", err) // todo: rename
		}
		// logger.Info().Int("port", incomingSocketPort).Msg("listen for incoming socket") // todo: set // todo: rename

		// todo: how to wait and break array?
		group := new(errgroup.Group)
		for {
			if stop.Load() {
				// todo: send to error group that everything should stopped
				// todo: make log
				break
			}

			var nfd int // todo: rename
			nfd, _, err = syscall.Accept(incomingSocket)
			if err != nil {
				if errors.Is(err, syscall.EAGAIN) {
					time.Sleep(time.Millisecond * 5)
					continue
				}
				return fmt.Errorf("failed to accept new connection: %w", err)
			}

			group.Go(handler(nfd))
		}
		if err = group.Wait(); err != nil {
			return fmt.Errorf("failed to wait for connections: %w", err)
		}
		return nil
	}
}

func (s *PublicService) HandleIncomingConnection(fd int) func() error {
	fmt.Println("new connection to incoming server") // todo: delete

	// todo: remove nested
	return func() error {
		defer func() {
			if err := syscall.Close(fd); err != nil {
				slog.Error("failed to close connection", "error", err)
			}
		}()

		connID := RandomString(12)
		resC := make(chan int)         // todo: where to close this channel?
		s.handshakeResC[connID] = resC // todo: how to clear map
		s.handshakeReqC <- connID
		fmt.Println("waiting for file descriptor to start copying streams") // todo: delete
		nfd := <-resC                                                       // todo: rename
		// todo: do we need to close nfd here?

		fmt.Println("starting to copy streams") // todo: delete
		CopyStreams(fd, nfd)

		s.closeProxyFd[connID] <- struct{}{}

		return nil
	}
}

// todo: this function can be executed only once
func (s *PublicService) HandleLocalConnection(fd int) func() error {
	fmt.Println("new connection to from local side") // todo: delete

	return func() error {
		// todo: close fd on defer
		group := new(errgroup.Group)
		group.Go(s.Listen(32345, s.HandleProxyConnection, s.stop))

		for {
			handshakeConnID, ok := <-s.handshakeReqC // todo: where to close handshake channel?
			if !ok {
				// todo: how to close all proxy connections
				break
			}

			req := RPCRequest{
				Method: Ping,
				ConnID: handshakeConnID,
			}
			buf, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("failed to marshal ping rpc request: %w", err)
			}
			if _, err = syscall.Write(fd, buf); err != nil {
				return fmt.Errorf("failed to write ping message to fd: %w", err)
			}
		}

		if err := group.Wait(); err != nil {
			return fmt.Errorf("failed to wait for server: %w", err)
		}
		return nil
	}
}

func (s *PublicService) HandleProxyConnection(fd int) func() error {
	fmt.Println("new connection to proxy server") // todo: delete

	return func() error {
		// todo: close fd on defer

		buf := make([]byte, 4096) // todo: size
		n, err := syscall.Read(fd, buf)
		if err != nil {
			return fmt.Errorf("failed to read from fd: %w", err)
		}
		req := RPCRequest{}
		if err = json.Unmarshal(buf[:n], &req); err != nil {
			return fmt.Errorf("failed to decode request: %w", err)
		}
		// todo: check that it is pong message

		handshakeResC, ok := s.handshakeResC[req.ConnID]
		if !ok {
			// todo: very bad
		}

		closeC := make(chan struct{})
		s.closeProxyFd[req.ConnID] = closeC
		handshakeResC <- fd
		fmt.Println("wait for closing proxy fd") // todo: delete
		<-closeC
		// todo: make log

		return nil
	}
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

func RandomString(n int) string {
	letterRunes := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func CopyData[T int | uintptr](srcFd, dstFd T, errC chan error) {
	buf := make([]byte, 4096) // todo: buffer size
	for {
		n, err := syscall.Read(int(srcFd), buf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(time.Millisecond * 5)
				continue
			}
			errC <- fmt.Errorf("failed to read from source socket: %w", err)
			return
		}
		if n == 0 {
			// todo: make log
			errC <- nil
			return
		}
		if _, err = syscall.Write(int(dstFd), buf[:n]); err != nil {
			errC <- fmt.Errorf("failed to write to destination socket: %w", err)
			return
		}
	}
}

func CopyStreams[T int | uintptr](srcFd, dstFd T) {
	errC := make(chan error)
	go CopyData(srcFd, dstFd, errC)
	go CopyData(dstFd, srcFd, errC)
	for i := 0; i < 2; i++ {
		if err := <-errC; err != nil {
			// todo: make some log
		}
	}
}
