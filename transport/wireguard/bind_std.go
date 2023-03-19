/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2023 WireGuard LLC. All Rights Reserved.
 */

package wireguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	_ "unsafe"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	wg "golang.zx2c4.com/wireguard/conn"
)

//go:linkname getSrcFromControl golang.zx2c4.com/wireguard/conn.getSrcFromControl
func getSrcFromControl(control []byte, ep *wg.StdNetEndpoint)

//go:linkname setSrcControl golang.zx2c4.com/wireguard/conn.setSrcControl
func setSrcControl(control *[]byte, ep *wg.StdNetEndpoint)

var _ wg.Bind = (*StdNetBind)(nil)

type StdNetBind struct {
	mu         sync.Mutex // protects following fields
	ipv4       *net.UDPConn
	ipv6       *net.UDPConn
	blackhole4 bool
	blackhole6 bool
	ipv4PC     *ipv4.PacketConn // will be nil on non-Linux
	ipv6PC     *ipv6.PacketConn // will be nil on non-Linux

	udpAddrPool  sync.Pool // following fields are not guarded by mu
	ipv4MsgsPool sync.Pool
	ipv6MsgsPool sync.Pool

	controlFns    []func(network, address string, c syscall.RawConn) error
	interfaceName string
	reserved      []byte
}

func (s *StdNetBind) setReserved(b []byte) {
	if len(b) < 4 || s.reserved == nil {
		return
	}
	b[1] = s.reserved[0]
	b[2] = s.reserved[1]
	b[3] = s.reserved[2]
}

func (s *StdNetBind) resetReserved(b []byte) {
	if len(b) < 4 {
		return
	}
	b[1] = 0x00
	b[2] = 0x00
	b[3] = 0x00
}

func (s *StdNetBind) listenConfig() *net.ListenConfig {
	return &net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			for _, fn := range s.controlFns {
				if err := fn(network, address, c); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func (s *StdNetBind) listenNet(network string, port int) (*net.UDPConn, int, error) {
	listenIP, err := getListenIP(network, s.interfaceName)
	if err != nil {
		return nil, 0, err
	}

	conn, err := s.listenConfig().ListenPacket(context.Background(), network, listenIP+":"+strconv.Itoa(port))
	if err != nil {
		return nil, 0, err
	}

	// Retrieve port.
	laddr := conn.LocalAddr()
	uaddr, err := net.ResolveUDPAddr(
		laddr.Network(),
		laddr.String(),
	)
	if err != nil {
		return nil, 0, err
	}
	return conn.(*net.UDPConn), uaddr.Port, nil
}

func (s *StdNetBind) SetMark(mark uint32) error {
	return nil
}

func (s *StdNetBind) UpdateControlFns(controlFns []func(network, address string, c syscall.RawConn) error) {
	s.controlFns = controlFns
}

func NewStdNetBind(
	controlFns []func(network, address string, c syscall.RawConn) error,
	interfaceName string,
	reserved []byte,
) wg.Bind {
	return &StdNetBind{
		udpAddrPool: sync.Pool{
			New: func() any {
				return &net.UDPAddr{
					IP: make([]byte, 16),
				}
			},
		},

		ipv4MsgsPool: sync.Pool{
			New: func() any {
				msgs := make([]ipv4.Message, wg.IdealBatchSize)
				for i := range msgs {
					msgs[i].Buffers = make(net.Buffers, 1)
					msgs[i].OOB = make([]byte, srcControlSize)
				}
				return &msgs
			},
		},

		ipv6MsgsPool: sync.Pool{
			New: func() any {
				msgs := make([]ipv6.Message, wg.IdealBatchSize)
				for i := range msgs {
					msgs[i].Buffers = make(net.Buffers, 1)
					msgs[i].OOB = make([]byte, srcControlSize)
				}
				return &msgs
			},
		},

		controlFns:    controlFns,
		interfaceName: interfaceName,
		reserved:      reserved,
	}
}

func (*StdNetBind) ParseEndpoint(s string) (wg.Endpoint, error) {
	e, err := netip.ParseAddrPort(s)
	return asEndpoint(e), err
}

func (s *StdNetBind) Open(uport uint16) ([]wg.ReceiveFunc, uint16, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	var tries int

	if s.ipv4 != nil || s.ipv6 != nil {
		return nil, 0, wg.ErrBindAlreadyOpen
	}

	// Attempt to open ipv4 and ipv6 listeners on the same port.
	// If uport is 0, we can retry on failure.
again:
	port := int(uport)
	var v4conn, v6conn *net.UDPConn
	var v4pc *ipv4.PacketConn
	var v6pc *ipv6.PacketConn

	v4conn, port, err = s.listenNet("udp4", port)
	if err != nil && !errors.Is(err, syscall.EAFNOSUPPORT) {
		return nil, 0, err
	}

	// Listen on the same port as we're using for ipv4.
	v6conn, port, err = s.listenNet("udp6", port)
	if uport == 0 && errors.Is(err, syscall.EADDRINUSE) && tries < 100 {
		v4conn.Close()
		tries++
		goto again
	}
	if err != nil && !errors.Is(err, syscall.EAFNOSUPPORT) {
		v4conn.Close()
		return nil, 0, err
	}
	var fns []wg.ReceiveFunc
	if v4conn != nil {
		if runtime.GOOS == "linux" {
			v4pc = ipv4.NewPacketConn(v4conn)
			s.ipv4PC = v4pc
		}
		fns = append(fns, s.makeReceiveIPv4(v4pc, v4conn))
		s.ipv4 = v4conn
	}
	if v6conn != nil {
		if runtime.GOOS == "linux" {
			v6pc = ipv6.NewPacketConn(v6conn)
			s.ipv6PC = v6pc
		}
		fns = append(fns, s.makeReceiveIPv6(v6pc, v6conn))
		s.ipv6 = v6conn
	}
	if len(fns) == 0 {
		return nil, 0, syscall.EAFNOSUPPORT
	}

	return fns, uint16(port), nil
}

func (s *StdNetBind) makeReceiveIPv4(pc *ipv4.PacketConn, conn *net.UDPConn) wg.ReceiveFunc {
	return func(bufs [][]byte, sizes []int, eps []wg.Endpoint) (n int, err error) {
		msgs := s.ipv4MsgsPool.Get().(*[]ipv4.Message)
		defer s.ipv4MsgsPool.Put(msgs)
		for i := range bufs {
			(*msgs)[i].Buffers[0] = bufs[i]
		}
		var numMsgs int
		if runtime.GOOS == "linux" {
			numMsgs, err = pc.ReadBatch(*msgs, 0)
			if err != nil {
				return 0, err
			}
		} else {
			msg := &(*msgs)[0]
			msg.N, msg.NN, _, msg.Addr, err = conn.ReadMsgUDP(msg.Buffers[0], msg.OOB)
			if err != nil {
				return 0, err
			}
			numMsgs = 1
		}
		for i := 0; i < numMsgs; i++ {
			msg := &(*msgs)[i]
			sizes[i] = msg.N
			addrPort := msg.Addr.(*net.UDPAddr).AddrPort()
			ep := asEndpoint(addrPort)
			getSrcFromControl(msg.OOB[:msg.NN], ep)
			eps[i] = ep
			s.resetReserved(msg.Buffers[0])
		}
		return numMsgs, nil
	}
}

func (s *StdNetBind) makeReceiveIPv6(pc *ipv6.PacketConn, conn *net.UDPConn) wg.ReceiveFunc {
	return func(bufs [][]byte, sizes []int, eps []wg.Endpoint) (n int, err error) {
		msgs := s.ipv4MsgsPool.Get().(*[]ipv6.Message)
		defer s.ipv4MsgsPool.Put(msgs)
		for i := range bufs {
			(*msgs)[i].Buffers[0] = bufs[i]
		}
		var numMsgs int
		if runtime.GOOS == "linux" {
			numMsgs, err = pc.ReadBatch(*msgs, 0)
			if err != nil {
				return 0, err
			}
		} else {
			msg := &(*msgs)[0]
			msg.N, msg.NN, _, msg.Addr, err = conn.ReadMsgUDP(msg.Buffers[0], msg.OOB)
			if err != nil {
				return 0, err
			}
			numMsgs = 1
		}
		for i := 0; i < numMsgs; i++ {
			msg := &(*msgs)[i]
			sizes[i] = msg.N
			addrPort := msg.Addr.(*net.UDPAddr).AddrPort()
			ep := asEndpoint(addrPort)
			getSrcFromControl(msg.OOB[:msg.NN], ep)
			eps[i] = ep
			s.resetReserved(msg.Buffers[0])
		}
		return numMsgs, nil
	}
}

// TODO: When all Binds handle IdealBatchSize, remove this dynamic function and
// rename the IdealBatchSize constant to BatchSize.
func (s *StdNetBind) BatchSize() int {
	if runtime.GOOS == "linux" {
		return wg.IdealBatchSize
	}
	return 1
}

func (s *StdNetBind) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var err1, err2 error
	if s.ipv4 != nil {
		err1 = s.ipv4.Close()
		s.ipv4 = nil
		s.ipv4PC = nil
	}
	if s.ipv6 != nil {
		err2 = s.ipv6.Close()
		s.ipv6 = nil
		s.ipv6PC = nil
	}
	s.blackhole4 = false
	s.blackhole6 = false
	if err1 != nil {
		return err1
	}
	return err2
}

func (s *StdNetBind) Send(bufs [][]byte, endpoint wg.Endpoint) error {
	s.mu.Lock()
	blackhole := s.blackhole4
	conn := s.ipv4
	var (
		pc4 *ipv4.PacketConn
		pc6 *ipv6.PacketConn
	)
	is6 := false
	if endpoint.DstIP().Is6() {
		blackhole = s.blackhole6
		conn = s.ipv6
		pc6 = s.ipv6PC
		is6 = true
	} else {
		pc4 = s.ipv4PC
	}
	s.mu.Unlock()

	if blackhole {
		return nil
	}
	if conn == nil {
		return syscall.EAFNOSUPPORT
	}

	for i := range bufs {
		s.setReserved(bufs[i])
	}

	if is6 {
		return s.send6(conn, pc6, endpoint, bufs)
	} else {
		return s.send4(conn, pc4, endpoint, bufs)
	}
}

func (s *StdNetBind) send4(conn *net.UDPConn, pc *ipv4.PacketConn, ep wg.Endpoint, bufs [][]byte) error {
	ua := s.udpAddrPool.Get().(*net.UDPAddr)
	as4 := ep.DstIP().As4()
	copy(ua.IP, as4[:])
	ua.IP = ua.IP[:4]
	ua.Port = int(ep.(*wg.StdNetEndpoint).Port())
	msgs := s.ipv4MsgsPool.Get().(*[]ipv4.Message)
	for i, buf := range bufs {
		(*msgs)[i].Buffers[0] = buf
		(*msgs)[i].Addr = ua
		setSrcControl(&(*msgs)[i].OOB, ep.(*wg.StdNetEndpoint))
	}
	var (
		n     int
		err   error
		start int
	)
	if runtime.GOOS == "linux" {
		for {
			n, err = pc.WriteBatch((*msgs)[start:len(bufs)], 0)
			if err != nil || n == len((*msgs)[start:len(bufs)]) {
				break
			}
			start += n
		}
	} else {
		for i, buf := range bufs {
			_, _, err = conn.WriteMsgUDP(buf, (*msgs)[i].OOB, ua)
			if err != nil {
				break
			}
		}
	}
	s.udpAddrPool.Put(ua)
	s.ipv4MsgsPool.Put(msgs)
	return err
}

func (s *StdNetBind) send6(conn *net.UDPConn, pc *ipv6.PacketConn, ep wg.Endpoint, bufs [][]byte) error {
	ua := s.udpAddrPool.Get().(*net.UDPAddr)
	as16 := ep.DstIP().As16()
	copy(ua.IP, as16[:])
	ua.IP = ua.IP[:16]
	ua.Port = int(ep.(*wg.StdNetEndpoint).Port())
	msgs := s.ipv6MsgsPool.Get().(*[]ipv6.Message)
	for i, buf := range bufs {
		(*msgs)[i].Buffers[0] = buf
		(*msgs)[i].Addr = ua
		setSrcControl(&(*msgs)[i].OOB, ep.(*wg.StdNetEndpoint))
	}
	var (
		n     int
		err   error
		start int
	)
	if runtime.GOOS == "linux" {
		for {
			n, err = pc.WriteBatch((*msgs)[start:len(bufs)], 0)
			if err != nil || n == len((*msgs)[start:len(bufs)]) {
				break
			}
			start += n
		}
	} else {
		for i, buf := range bufs {
			_, _, err = conn.WriteMsgUDP(buf, (*msgs)[i].OOB, ua)
			if err != nil {
				break
			}
		}
	}
	s.udpAddrPool.Put(ua)
	s.ipv6MsgsPool.Put(msgs)
	return err
}

// endpointPool contains a re-usable set of mapping from netip.AddrPort to Endpoint.
// This exists to reduce allocations: Putting a netip.AddrPort in an Endpoint allocates,
// but Endpoints are immutable, so we can re-use them.
var endpointPool = sync.Pool{
	New: func() any {
		return make(map[netip.AddrPort]*wg.StdNetEndpoint)
	},
}

// asEndpoint returns an Endpoint containing ap.
func asEndpoint(ap netip.AddrPort) *wg.StdNetEndpoint {
	m := endpointPool.Get().(map[netip.AddrPort]*wg.StdNetEndpoint)
	defer endpointPool.Put(m)
	e, ok := m[ap]
	if !ok {
		e = &wg.StdNetEndpoint{AddrPort: ap}
		m[ap] = e
	}
	return e
}
