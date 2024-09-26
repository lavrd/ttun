package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

const NetworkBufferSize = 1024

func main() {
	logger := zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	}).
		With().Timestamp().Caller().
		Logger().Level(zerolog.TraceLevel)

	if len(os.Args) != 2 {
		logger.Fatal().Msg("incorrect number of os arguments")
	}

	service := &Service{
		stop:   &atomic.Bool{},
		logger: &logger,
	}

	group := new(errgroup.Group)
	switch os.Args[1] {
	case "local":
		group.Go(service.StartLocalServer)
	case "public":
		group.Go(service.StartIncomingServer)
		group.Go(service.StartProxyServer)
	default:
		logger.Fatal().Str("arg", os.Args[1]).Msg("unknown argument to start ttun")
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	signalName := (<-interrupt).String()
	logger.Debug().Str("signal", signalName).Msg("received os signal")

	service.stop.Store(true)

	if err := group.Wait(); err != nil {
		logger.Error().Err(err).Msg("failed to wait for threads")
	}
}

type Service struct {
	stop   *atomic.Bool
	logger *zerolog.Logger
}

func (s *Service) StartLocalServer() error { return nil }

func (s *Service) StartIncomingServer() error {
	incomingSocketPort := 14600
	incomingSocket, err := InitSocket()
	if err != nil {
		return fmt.Errorf("failed to init incoming socket: %w", err)
	}
	if err = Listen(incomingSocket, incomingSocketPort); err != nil {
		return fmt.Errorf("failed to listen incoming socket: %w", err)
	}
	// logger.Info().Int("port", incomingSocketPort).Msg("listen for incoming socket") // todo: set

	// todo: how to wait and break array?
	group := new(errgroup.Group)
	for {
		if s.stop.Load() {
			// todo: make log
			break
		}

		var nfd int // todo: rename
		nfd, _, err = syscall.Accept(incomingSocket)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				fmt.Println("") // todo: delete
				time.Sleep(time.Millisecond * 5)
				continue
			}
			return fmt.Errorf("failed to accept new connection: %w", err)
		}

		conn := &Connection{
			fd:     nfd,
			logger: s.logger,
		}
		group.Go(conn.Handle)
	}
	if err = group.Wait(); err != nil {
		return fmt.Errorf("failed to wait for connections: %w", err)
	}

	return nil
}

func (s *Service) StartProxyServer() error {
	// 22000
	return nil
}

type Connection struct {
	fd int
	// todo: store address of the connection inside logger
	logger *zerolog.Logger
}

func (c *Connection) Handle() error {
	defer func() {
		if err := syscall.Close(c.fd); err != nil {
			c.logger.Error().Err(err).Msg("failed to close connection")
		}
	}()

	buffer := make([]byte, NetworkBufferSize)
	for {
		n, err := syscall.Read(c.fd, buffer)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(time.Millisecond * 5)
				continue
			}
			return fmt.Errorf("failed to read from socket: %w", err)
		}
		if n == 0 {
			// todo: log that connection was closed
			return nil
		}
		if _, err = syscall.Write(c.fd, buffer[:n]); err != nil {
			return fmt.Errorf("failed to write to the socket: %w", err)
		}
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
