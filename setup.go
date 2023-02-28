package multidial

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/zeebo/errs"
	"golang.org/x/sync/errgroup"
)

var (
	Error         = errs.Class("multidial")
	ErrConnClosed = errs.Class("conn closed")
)

const (
	maxIncompleteRequestQueueSize = 5
)

type requestType int

const (
	typeRead             requestType = 1
	typeWrite            requestType = 2
	typeSetDeadline      requestType = 3
	typeSetReadDeadline  requestType = 4
	typeSetWriteDeadline requestType = 5
	typeChoose           requestType = 6
)

type connResponse struct {
	N      int
	Err    error
	Source *connDetails
	Conn   net.Conn
}

type connRequest struct {
	Response chan connResponse
	Type     requestType
	Buf      []byte
	T        time.Time
}

type connDetails struct {
	cancel func()
	reqs   chan connRequest
	conn   atomic.Value
}

func (c *connDetails) init(ctx context.Context) context.Context {
	ctx, c.cancel = context.WithCancel(ctx)
	c.reqs = make(chan connRequest, maxIncompleteRequestQueueSize)
	return ctx
}

type setup struct {
	network      string
	address      string
	conn1, conn2 connDetails
}

func newSetup(m *Multidialer, network string, address string) *setup {
	s := &setup{
		network: network,
		address: address,
	}
	ctx := context.Background()
	go s.conn1.manage(s.conn1.init(ctx), m.dialer1, network, address)
	go s.conn2.manage(s.conn2.init(ctx), m.dialer2, network, address)
	return s
}

func (cd *connDetails) requestOne(req connRequest) {
	select {
	case cd.reqs <- req:
	default:
		req.Response <- connResponse{
			Err:    ErrConnClosed.New("write"),
			Source: cd,
		}
	}
}

func (s *setup) request(req1, req2 connRequest) {
	s.conn1.requestOne(req1)
	s.conn2.requestOne(req2)
}

func (s *setup) setDeadline(t time.Time, typ requestType) (err error) {
	req := connRequest{
		Response: make(chan connResponse, 2),
		Type:     typ,
		T:        t,
	}
	s.request(req, req)

	for answer := range req.Response {
		err = answer.Err
		if err == nil {
			return err
		}
	}
	return err
}

func (s *setup) SetDeadline(t time.Time) (err error) {
	return s.setDeadline(t, typeSetDeadline)
}

func (s *setup) SetReadDeadline(t time.Time) (err error) {
	return s.setDeadline(t, typeSetReadDeadline)
}

func (s *setup) SetWriteDeadline(t time.Time) (err error) {
	return s.setDeadline(t, typeSetWriteDeadline)
}

func (s *setup) Write(p []byte) (n int, err error) {
	pCopy := append([]byte(nil), p...)
	resp := make(chan connResponse, 2)

	s.request(connRequest{
		Response: resp,
		Type:     typeWrite,
		Buf:      p,
	}, connRequest{
		Response: resp,
		Type:     typeWrite,
		Buf:      pCopy,
	})

	for answer := range resp {
		n, err = answer.N, answer.Err
		if err == nil {
			return n, err
		}
	}
	return n, err
}

func (s *setup) Read(p []byte) (n int, conn net.Conn, err error) {
	pCopy := make([]byte, len(p))
	resp := make(chan connResponse, 2)

	s.request(connRequest{
		Response: resp,
		Type:     typeRead,
		Buf:      p,
	}, connRequest{
		Response: resp,
		Type:     typeRead,
		Buf:      pCopy,
	})

	for answer := range resp {
		n, err = answer.N, answer.Err
		if err != nil && !errors.Is(err, io.EOF) {
			continue
		}

		if answer.Source == &s.conn2 {
			copy(p, pCopy[:n])
		}

		selection := make(chan connResponse, 1)
		answer.Source.requestOne(connRequest{
			Response: selection,
			Type:     typeChoose,
		})
		selectionAnswer := <-selection
		if selectionAnswer.Err != nil {
			// well, the read succeeded, so let's at least return that.
			return n, nil, err
		}

		return n, selectionAnswer.Conn, err
	}
	return n, nil, err
}

func (s *setup) Close() error {
	var eg errgroup.Group
	eg.Go(func() error {
		s.conn1.cancel()
		if conn, ok := s.conn1.conn.Load().(net.Conn); ok {
			return conn.Close()
		}
		return nil
	})
	eg.Go(func() error {
		s.conn2.cancel()
		if conn, ok := s.conn2.conn.Load().(net.Conn); ok {
			return conn.Close()
		}
		return nil
	})
	return eg.Wait()
}

func (cd *connDetails) manage(ctx context.Context, dialer DialFunc, network, address string) {
	var connErr error
	defer func() {
		// ha ha! the receiver closes the chan! take that rob pike!
		close(cd.reqs)
		if connErr == nil {
			connErr = ErrConnClosed.New("teardown")
		}
		// drain the incoming requests.
		for req := range cd.reqs {
			req.Response <- connResponse{
				Err:    connErr,
				Source: cd,
			}
		}
	}()
	conn, err := dialer(ctx, network, address)
	if err != nil {
		connErr = err
		return
	}
	defer func() {
		if conn != nil {
			connErr = errs.Combine(connErr, conn.Close())
		}
	}()
	cd.conn.Store(conn)

	for {
		// invariant: if some sort of permanent error happens,
		// set connErr and return.
		select {
		case <-ctx.Done():
			connErr = errs.Combine(connErr, ctx.Err())
			return
		case req := <-cd.reqs:
			switch req.Type {
			case typeRead:
				n, err := conn.Read(req.Buf)
				req.Response <- connResponse{
					N:      n,
					Err:    err,
					Source: cd,
				}
				if err != nil {
					if !errors.Is(err, io.EOF) {
						connErr = err
					}
					return
				}
			case typeWrite:
				n, err := conn.Write(req.Buf)
				req.Response <- connResponse{
					N:      n,
					Err:    err,
					Source: cd,
				}
				if err != nil {
					connErr = err
					return
				}
			case typeSetDeadline:
				connErr = conn.SetDeadline(req.T)
				req.Response <- connResponse{
					Err:    connErr,
					Source: cd,
				}
				if connErr != nil {
					return
				}
			case typeSetReadDeadline:
				connErr = conn.SetReadDeadline(req.T)
				req.Response <- connResponse{
					Err:    connErr,
					Source: cd,
				}
				if connErr != nil {
					return
				}
			case typeSetWriteDeadline:
				connErr = conn.SetWriteDeadline(req.T)
				req.Response <- connResponse{
					Err:    connErr,
					Source: cd,
				}
				if connErr != nil {
					return
				}
			case typeChoose:
				req.Response <- connResponse{
					Source: cd,
					Conn:   conn,
				}
				conn = nil
			default:
				connErr = Error.New("unknown request type")
				return
			}
		}
	}
}
