package clickhouse

import (
	"bufio"
	"crypto/tls"
	"database/sql/driver"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var tick int32

type openStrategy int8

func (s openStrategy) String() string {
	switch s {
	case connOpenInOrder:
		return "in_order"
	case connOpenTimeRandom:
		return "time_random"
	}
	return "random"
}

const (
	connOpenRandom openStrategy = iota + 1
	connOpenInOrder
	connOpenTimeRandom
)

type connOptions struct {
	secure, skipVerify                     bool
	tlsConfig                              *tls.Config
	hosts                                  []string
	connTimeout, readTimeout, writeTimeout time.Duration
	noDelay                                bool
	openStrategy                           openStrategy
	logf                                   func(string, ...interface{})
}

// DialFunc is a function which can be used to establish the network connection.
// Custom dial functions must be registered with RegisterDial
type DialFunc func(network, address string, timeout time.Duration, config *tls.Config) (net.Conn, error)

var (
	customDialLock sync.RWMutex
	customDial     DialFunc
)

// RegisterDial registers a custom dial function.
func RegisterDial(dial DialFunc) {
	customDialLock.Lock()
	customDial = dial
	customDialLock.Unlock()
}

// DeregisterDial deregisters the custom dial function.
func DeregisterDial() {
	customDialLock.Lock()
	customDial = nil
	customDialLock.Unlock()
}

func dial(options connOptions) (*connect, error) {
	var (
		err error
		abs = func(v int) int {
			if v < 0 {
				return -1 * v
			}
			return v
		}
		conn  net.Conn
		ident = abs(int(atomic.AddInt32(&tick, 1)))
	)
	tlsConfig := options.tlsConfig
	if options.secure {
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		}
		tlsConfig.InsecureSkipVerify = options.skipVerify
	}
	checkedHosts := make(map[int]struct{}, len(options.hosts))
	for i := range options.hosts {
		var num int
		switch options.openStrategy {
		case connOpenInOrder:
			num = i
		case connOpenRandom:
			num = (ident + i) % len(options.hosts)
		case connOpenTimeRandom:
			// select host based on milliseconds
			num = int((time.Now().UnixNano()/1000)%1000) % len(options.hosts)
			for _, ok := checkedHosts[num]; ok; _, ok = checkedHosts[num] {
				num = int(time.Now().UnixNano()) % len(options.hosts)
			}
			checkedHosts[num] = struct{}{}
		}
		customDialLock.RLock()
		cd := customDial
		customDialLock.RUnlock()
		switch {
		case options.secure:
			if cd != nil {
				conn, err = cd("tcp", options.hosts[num], options.connTimeout, tlsConfig)
			} else {
				conn, err = tls.DialWithDialer(
					&net.Dialer{
						Timeout: options.connTimeout,
					},
					"tcp",
					options.hosts[num],
					tlsConfig,
				)
			}
		default:
			if cd != nil {
				conn, err = cd("tcp", options.hosts[num], options.connTimeout, nil)
			} else {
				conn, err = net.DialTimeout("tcp", options.hosts[num], options.connTimeout)
			}
		}
		if err == nil {
			options.logf(
				"[dial] secure=%t, skip_verify=%t, strategy=%s, ident=%d, server=%d -> %s",
				options.secure,
				options.skipVerify,
				options.openStrategy,
				ident,
				num,
				conn.RemoteAddr(),
			)
			if tcp, ok := conn.(*net.TCPConn); ok {
				err = tcp.SetNoDelay(options.noDelay) // Disable or enable the Nagle Algorithm for this tcp socket
				if err != nil {
					return nil, err
				}
			}
			return &connect{
				Conn:         conn,
				logf:         options.logf,
				ident:        ident,
				buffer:       bufio.NewReader(conn),
				readTimeout:  options.readTimeout,
				writeTimeout: options.writeTimeout,
			}, nil
		} else {
			options.logf(
				"[dial err] secure=%t, skip_verify=%t, strategy=%s, ident=%d, addr=%s\n%#v",
				options.secure,
				options.skipVerify,
				options.openStrategy,
				ident,
				options.hosts[num],
				err,
			)
		}
	}
	return nil, err
}

type connect struct {
	net.Conn
	logf                  func(string, ...interface{})
	ident                 int
	buffer                *bufio.Reader
	closed                bool
	readTimeout           time.Duration
	writeTimeout          time.Duration
	lastReadDeadlineTime  time.Time
	lastWriteDeadlineTime time.Time
}

func (conn *connect) Read(b []byte) (int, error) {
	var (
		n      int
		err    error
		total  int
		dstLen = len(b)
	)
	if currentTime := now(); conn.readTimeout != 0 && currentTime.Sub(conn.lastReadDeadlineTime) > (conn.readTimeout>>2) {
		conn.SetReadDeadline(time.Now().Add(conn.readTimeout))
		conn.lastReadDeadlineTime = currentTime
	}
	for total < dstLen {
		if n, err = conn.buffer.Read(b[total:]); err != nil {
			conn.logf("[connect] read error: %v", err)
			conn.Close()
			return n, driver.ErrBadConn
		}
		total += n
	}
	return total, nil
}

func (conn *connect) Write(b []byte) (int, error) {
	var (
		n      int
		err    error
		total  int
		srcLen = len(b)
	)
	if currentTime := now(); conn.writeTimeout != 0 && currentTime.Sub(conn.lastWriteDeadlineTime) > (conn.writeTimeout>>2) {
		conn.SetWriteDeadline(time.Now().Add(conn.writeTimeout))
		conn.lastWriteDeadlineTime = currentTime
	}
	for total < srcLen {
		if n, err = conn.Conn.Write(b[total:]); err != nil {
			conn.logf("[connect] write error: %v", err)
			conn.Close()
			return n, driver.ErrBadConn
		}
		total += n
	}
	return n, nil
}

func (conn *connect) Close() error {
	if !conn.closed {
		conn.closed = true
		return conn.Conn.Close()
	}
	return nil
}
