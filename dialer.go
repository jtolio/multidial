package multidial

import (
	"context"
	"net"
)

type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Multidialer is a wrapper type that wraps two other dialers. It is intended to
// support finding the fastest dialing approach (which may be 0-RTT) by simply
// dialing both connections simultaneously. Because the fastest dialing
// approach might be 0-RTT, the first packet may have some written data in it.
// So, Multidialer's Dial method is actually a no-op, and the dialing happens
// on the first Write call to the returned Conn.
// Write calls to returned Conns will duplicate writes to both underlying
// connections until the first Read or when an error is received, whichever
// is first. It is expected that this type would be used with something like
// github.com/jtolio/noiseconn/debounce on the server side.
type Multidialer struct {
	dialer1, dialer2 DialFunc
}

func NewMultidialer(dialer1, dialer2 DialFunc) *Multidialer {
	return &Multidialer{dialer1: dialer1,
		dialer2: dialer2,
	}
}

func (m *Multidialer) Dial(network, address string) (net.Conn, error) {
	return m.DialContext(context.Background(), network, address)
}

func (m *Multidialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return &conn{
		setup: newSetup(m, network, address),
	}, nil
}
