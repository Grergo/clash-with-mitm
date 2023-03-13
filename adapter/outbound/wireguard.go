package outbound

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phuslu/log"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/Dreamacro/clash/component/dialer"
	"github.com/Dreamacro/clash/component/resolver"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/transport/wireguard"
)

const dialTimeout = 10 * time.Second

type WireGuard struct {
	*Base
	wgDevice  *device.Device
	tunDevice tun.Device
	netStack  *wireguard.Net
	bind      *wireguard.WgBind

	dialer     *wgDialer
	localIP    netip.Addr
	localIPv6  netip.Addr
	dnsServers []netip.Addr
	reserved   []byte
	uapiConf   []string
	threadId   string
	mtu        int

	upOnce   sync.Once
	downOnce sync.Once
	upErr    error

	remoteDnsResolve bool
}

type WireGuardOption struct {
	BasicOption
	Name             string   `proxy:"name"`
	Server           string   `proxy:"server"`
	Port             int      `proxy:"port"`
	IP               string   `proxy:"ip,omitempty"`
	IPv6             string   `proxy:"ipv6,omitempty"`
	PrivateKey       string   `proxy:"private-key"`
	PublicKey        string   `proxy:"public-key"`
	PresharedKey     string   `proxy:"preshared-key,omitempty"`
	DNS              []string `proxy:"dns,omitempty"`
	MTU              int      `proxy:"mtu,omitempty"`
	UDP              bool     `proxy:"udp,omitempty"`
	RemoteDnsResolve bool     `proxy:"remote-dns-resolve,omitempty"`
	Reserved         string   `proxy:"reserved,omitempty"`
}

// DialContext implements C.ProxyAdapter
func (w *WireGuard) DialContext(ctx context.Context, metadata *C.Metadata, _ ...dialer.Option) (C.Conn, error) {
	w.up()
	if w.upErr != nil {
		return nil, fmt.Errorf("apply wireguard proxy %s config error: %w", w.threadId, w.upErr)
	}

	dialCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithDeadline(ctx, time.Now().Add(dialTimeout))
		defer cancel()
	}

	var remoteAddress string
	if !w.remoteDnsResolve && !metadata.Resolved() {
		rAddrs, err := resolver.LookupIP(ctx, metadata.Host)
		if err != nil {
			return nil, err
		}
		metadata.DstIP = rAddrs[rand.Intn(len(rAddrs))]
		remoteAddress = net.JoinHostPort(metadata.DstIP.String(), metadata.DstPort)
	} else {
		remoteAddress = metadata.RemoteAddress()
	}

	c, err := w.netStack.DialContext(dialCtx, "tcp", remoteAddress)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("conn is nil")
	}
	return NewConn(&wgConn{c}, w), nil
}

// ListenPacketContext implements C.ProxyAdapter
func (w *WireGuard) ListenPacketContext(ctx context.Context, metadata *C.Metadata, _ ...dialer.Option) (C.PacketConn, error) {
	w.up()
	if w.upErr != nil {
		return nil, fmt.Errorf("apply wireguard proxy %s config failure, cause: %w", w.threadId, w.upErr)
	}

	if !metadata.Resolved() {
		if w.remoteDnsResolve {
			rAddrs, err := w.netStack.LookupContextHost(ctx, metadata.Host)
			if err != nil {
				return nil, err
			}
			metadata.DstIP = rAddrs[0]
		} else {
			rAddrs, err := resolver.LookupIP(ctx, metadata.Host)
			if err != nil {
				return nil, err
			}
			metadata.DstIP = rAddrs[0]
		}
	}

	var lAddr netip.Addr
	if metadata.DstIP.Is6() {
		lAddr = w.localIPv6
	} else {
		lAddr = w.localIP
	}

	pc, err := w.netStack.ListenUDPAddrPort(netip.AddrPortFrom(lAddr, 0))
	if err != nil {
		return nil, err
	}
	if pc == nil {
		return nil, errors.New("packetConn is nil")
	}
	return NewPacketConn(&wgPConn{pc}, w), nil
}

// Cleanup implements C.Cleanup
func (w *WireGuard) Cleanup() {
	w.downOnce.Do(func() {
		if w.wgDevice != nil {
			w.wgDevice.Close()
		}
	})
}

func (w *WireGuard) RemoteDnsResolve() bool {
	return w.remoteDnsResolve
}

func (w *WireGuard) up() {
	w.upOnce.Do(func() {
		w.upErr = w.init()
	})
}

func (w *WireGuard) init() error {
	host, port, _ := net.SplitHostPort(w.Base.Addr())
	tryTimes := 0

lookup:
	endpointIP, err := resolver.ResolveProxyServerHost(host)
	if err != nil {
		if tryTimes < 5 {
			tryTimes++
			time.Sleep(2 * time.Second)
			goto lookup
		}
		return fmt.Errorf("parse server endpoint [%s] failure, cause: %w", w.Base.Addr(), err)
	}

	p, _ := strconv.ParseUint(port, 10, 16)
	endpoint := netip.AddrPortFrom(endpointIP, uint16(p))
	w.uapiConf = append(w.uapiConf, fmt.Sprintf("endpoint=%s", endpoint))

	wgBind := wireguard.NewWgBind(context.Background(), w.dialer, endpoint, w.reserved)

	localIPs := make([]netip.Addr, 0, 2)
	if w.localIP.IsValid() {
		localIPs = append(localIPs, w.localIP)
	}
	if w.localIPv6.IsValid() {
		localIPs = append(localIPs, w.localIPv6)
	}

	tunDevice, netStack, err := wireguard.CreateNetTUN(localIPs, w.dnsServers, w.mtu)
	if err != nil {
		return err
	}

	logger := &device.Logger{
		Verbosef: func(format string, args ...any) {
			log.Debug().Msgf("[WireGuard] [%s] "+strings.ToLower(format), append([]any{w.threadId}, args...)...)
		},
		Errorf: func(format string, args ...any) {
			log.Error().Msgf("[WireGuard] [%s] "+strings.ToLower(format), append([]any{w.threadId}, args...)...)
		},
	}

	wgDevice := device.NewDevice(tunDevice, wgBind, logger)

	log.Debug().Strs("config", w.uapiConf).Msgf("[WireGuard] initial wireguard proxy %s", w.threadId)

	err = wgDevice.IpcSet(strings.Join(w.uapiConf, "\n"))
	if err != nil {
		return err
	}

	w.bind = wgBind
	w.tunDevice = tunDevice
	w.netStack = netStack
	w.wgDevice = wgDevice
	w.uapiConf = nil
	w.dnsServers = nil
	w.reserved = nil
	return nil
}

func NewWireGuard(option WireGuardOption) (*WireGuard, error) {
	uapiConf := make([]string, 0, 6)
	privateKeyBytes, err := base64.StdEncoding.DecodeString(option.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("decode wireguard private key failure, cause: %w", err)
	}
	uapiConf = append(uapiConf, fmt.Sprintf("private_key=%s", hex.EncodeToString(privateKeyBytes)))

	publicKeyBytes, err := base64.StdEncoding.DecodeString(option.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("decode wireguard peer public key failure, cause: %w", err)
	}
	uapiConf = append(uapiConf, fmt.Sprintf("public_key=%s", hex.EncodeToString(publicKeyBytes)))

	if option.PresharedKey != "" {
		bytes, err := base64.StdEncoding.DecodeString(option.PresharedKey)
		if err != nil {
			return nil, fmt.Errorf("decode wireguard preshared key failure, cause: %w", err)
		}
		uapiConf = append(uapiConf, fmt.Sprintf("preshared_key=%s", hex.EncodeToString(bytes)))
	}

	var reservedBytes []byte
	if option.Reserved != "" {
		reserved := strings.TrimPrefix(strings.ToLower(option.Reserved), "0x")
		if reservedBytes, err = hex.DecodeString(reserved); err != nil || len(reservedBytes) != 3 {
			return nil, fmt.Errorf("decode wireguard reserved 3 bytes failure %w", err)
		}
	}

	var (
		localIP   netip.Addr
		localIPv6 netip.Addr
	)
	if option.IP != "" {
		option.IP, _, _ = strings.Cut(option.IP, "/")
		if localIP, err = netip.ParseAddr(option.IP); err != nil {
			return nil, fmt.Errorf("parse wireguard ip address failure, cause: %w", err)
		}
	}

	if option.IPv6 != "" {
		option.IPv6, _, _ = strings.Cut(option.IPv6, "/")
		if localIPv6, err = netip.ParseAddr(option.IPv6); err != nil {
			return nil, fmt.Errorf("parse wireguard ipv6 address failure, cause: %w", err)
		}
	}

	if !localIP.IsValid() && !localIPv6.IsValid() {
		return nil, errors.New("wireguard missing local ip")
	}

	dns := option.DNS
	if len(dns) == 0 {
		dns = append(dns, "1.1.1.1", "8.8.8.8")
	}
	dnsServers := make([]netip.Addr, len(dns))
	for _, d := range dns {
		if ip, err1 := netip.ParseAddr(d); err1 != nil {
			return nil, fmt.Errorf("parse wireguard dns address failure, cause: %w", err1)
		} else {
			dnsServers = append(dnsServers, ip)
		}
	}

	if localIP.IsValid() {
		uapiConf = append(uapiConf, "allowed_ip=0.0.0.0/0")
	}
	if localIPv6.IsValid() {
		uapiConf = append(uapiConf, "allowed_ip=::/0")
	}

	mtu := option.MTU
	if mtu == 0 {
		mtu = 1408
	}

	threadId := fmt.Sprintf("%s-%d", option.Name, rand.Intn(100))

	base := &Base{
		name:  option.Name,
		addr:  net.JoinHostPort(option.Server, strconv.Itoa(option.Port)),
		tp:    C.WireGuard,
		udp:   option.UDP,
		iface: option.Interface,
		rmark: option.RoutingMark,
	}
	wireGuard := &WireGuard{
		Base:       base,
		dialer:     &wgDialer{options: base.DialOptions()},
		localIP:    localIP,
		localIPv6:  localIPv6,
		dnsServers: dnsServers,
		reserved:   reservedBytes,
		uapiConf:   uapiConf,
		threadId:   threadId,
		mtu:        mtu,

		remoteDnsResolve: option.RemoteDnsResolve,
	}
	return wireGuard, nil
}

type wgDialer struct {
	options []dialer.Option
}

func (d *wgDialer) DialContext(ctx context.Context, network string, address netip.AddrPort) (net.Conn, error) {
	return dialer.DialContext(ctx, network, address.String(), d.options...)
}

type wgConn struct {
	net.Conn
}

type wgPConn struct {
	net.PacketConn
}
