// Copyright 2018 the u-root Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.12 && (darwin || freebsd || linux || netbsd || openbsd)
// +build go1.12
// +build darwin freebsd linux netbsd openbsd

package nclient4

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"github.com/mdlayher/raw"
	"github.com/u-root/uio/uio"
	"github.com/vishvananda/netlink"
)

// UDPConnType indicates the type of the udp conn.
type UDPConnType int

const (
	// UDPBroadcast specifies the type of udp conn as broadcast.
	//
	// All the packets will be broadcasted.
	UDPBroadcast UDPConnType = 0

	// UDPUnicast specifies the type of udp conn as unicast.
	// All the packets will be sent to a unicast MAC address.
	UDPUnicast UDPConnType = 1
)

var (
	// BroadcastMac is the broadcast MAC address.
	//
	// Any UDP packet sent to this address is broadcast on the subnet.
	BroadcastMac = net.HardwareAddr([]byte{255, 255, 255, 255, 255, 255})
)

var (
	// ErrUDPAddrIsRequired is an error used when a passed argument is not of type "*net.UDPAddr".
	ErrUDPAddrIsRequired = errors.New("must supply UDPAddr")

	// ErrHWAddrNotFound is an error used when getting MAC address failed.
	ErrHWAddrNotFound = errors.New("hardware address not found")
)

// NewRawUDPConn returns a UDP connection bound to the interface and udp address
// given based on a raw packet socket.
//
// The interface can be completely unconfigured.
func NewRawUDPConn(iface string, addr *net.UDPAddr, typ UDPConnType) (net.PacketConn, error) {
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, err
	}
	rawConn, err := raw.ListenPacket(ifc, uint16(ethernet.EtherTypeIPv4), &raw.Config{LinuxSockDGRAM: true})
	if err != nil {
		return nil, err
	}

	if typ == UDPUnicast {
		return NewUnicastRawUDPConn(rawConn, addr), nil
	}

	return NewBroadcastUDPConn(rawConn, addr), nil
}

// BroadcastRawUDPConn uses a raw socket to send UDP packets to the broadcast
// MAC address.
type BroadcastRawUDPConn struct {
	// PacketConn is a raw DGRAM socket.
	net.PacketConn

	// boundAddr is the address this RawUDPConn is "bound" to.
	//
	// Calls to ReadFrom will only return packets destined to this address.
	boundAddr *net.UDPAddr
}

// NewBroadcastUDPConn returns a PacketConn that marshals and unmarshals UDP
// packets, sending them to the broadcast MAC at on rawPacketConn.
//
// Calls to ReadFrom will only return packets destined to boundAddr.
func NewBroadcastUDPConn(rawPacketConn net.PacketConn, boundAddr *net.UDPAddr) net.PacketConn {
	return &BroadcastRawUDPConn{
		PacketConn: rawPacketConn,
		boundAddr:  boundAddr,
	}
}

func udpMatch(addr *net.UDPAddr, bound *net.UDPAddr) bool {
	if bound == nil {
		return true
	}
	if bound.IP != nil && !bound.IP.Equal(addr.IP) {
		return false
	}
	return bound.Port == addr.Port
}

// ReadFrom implements net.PacketConn.ReadFrom.
//
// ReadFrom reads raw IP packets and will try to match them against
// upc.boundAddr. Any matching packets are returned via the given buffer.
func (upc *BroadcastRawUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	ipHdrMaxLen := ipv4MaximumHeaderSize
	udpHdrLen := udpMinimumSize

	for {
		pkt := make([]byte, ipHdrMaxLen+udpHdrLen+len(b))
		n, _, err := upc.PacketConn.ReadFrom(pkt)
		if err != nil {
			return 0, nil, err
		}
		if n == 0 {
			return 0, nil, io.EOF
		}
		pkt = pkt[:n]
		buf := uio.NewBigEndianBuffer(pkt)

		// To read the header length, access data directly.
		if !buf.Has(ipv4MinimumSize) {
			continue
		}

		ipHdr := ipv4(buf.Data())

		if !buf.Has(int(ipHdr.headerLength())) {
			continue
		}

		ipHdr = ipv4(buf.Consume(int(ipHdr.headerLength())))

		if ipHdr.transportProtocol() != udpProtocolNumber {
			continue
		}

		if !buf.Has(udpHdrLen) {
			continue
		}

		udpHdr := udp(buf.Consume(udpHdrLen))

		addr := &net.UDPAddr{
			IP:   ipHdr.destinationAddress(),
			Port: int(udpHdr.destinationPort()),
		}
		if !udpMatch(addr, upc.boundAddr) {
			continue
		}
		srcAddr := &net.UDPAddr{
			IP:   ipHdr.sourceAddress(),
			Port: int(udpHdr.sourcePort()),
		}
		// Extra padding after end of IP packet should be ignored,
		// if not dhcp option parsing will fail.
		dhcpLen := int(ipHdr.payloadLength()) - udpHdrLen
		return copy(b, buf.Consume(dhcpLen)), srcAddr, nil
	}
}

// WriteTo implements net.PacketConn.WriteTo and broadcasts all packets at the
// raw socket level.
//
// WriteTo wraps the given packet in the appropriate UDP and IP header before
// sending it on the packet conn.
func (upc *BroadcastRawUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, ErrUDPAddrIsRequired
	}

	// Using the boundAddr is not quite right here, but it works.
	packet := udp4pkt(b, udpAddr, upc.boundAddr)

	// Broadcasting is not always right, but hell, what the ARP do I know.
	return upc.PacketConn.WriteTo(packet, &raw.Addr{HardwareAddr: BroadcastMac})
}

// UnicastRawUDPConn inherits from BroadcastRawUDPConn and override the WriteTo method
type UnicastRawUDPConn struct {
	*BroadcastRawUDPConn
}

// NewUnicastRawUDPConn returns a PacketConn which sending the packets to a unicast MAC address.
func NewUnicastRawUDPConn(rawPacketConn net.PacketConn, boundAddr *net.UDPAddr) net.PacketConn {
	return &UnicastRawUDPConn{
		BroadcastRawUDPConn: NewBroadcastUDPConn(rawPacketConn, boundAddr).(*BroadcastRawUDPConn),
	}
}

// WriteTo implements net.PacketConn.WriteTo.
//
// WriteTo try to get the MAC address of destination IP address before
// unicast all packets at the raw socket level.
func (upc *UnicastRawUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, ErrUDPAddrIsRequired
	}

	// Using the boundAddr is not quite right here, but it works.
	packet := udp4pkt(b, udpAddr, upc.boundAddr)
	dstMac, err := getHwAddr(udpAddr.IP)
	if err != nil {
		return 0, ErrHWAddrNotFound
	}

	return upc.PacketConn.WriteTo(packet, &raw.Addr{HardwareAddr: dstMac})
}

// getHwAddr from local arp cache. If no existing, try to get it by arp protocol.
func getHwAddr(ip net.IP) (net.HardwareAddr, error) {
	neighList, err := netlink.NeighListExecute(netlink.Ndmsg{
		Family: netlink.FAMILY_V4,
		State:  netlink.NUD_REACHABLE,
	})
	if err != nil {
		return nil, err
	}

	for _, neigh := range neighList {
		if ip.Equal(neigh.IP) && neigh.HardwareAddr != nil {
			return neigh.HardwareAddr, nil
		}
	}

	return arpResolve(ip)
}

func arpResolve(dest net.IP) (net.HardwareAddr, error) {
	// auto match the interface based on routes
	routes, err := netlink.RouteGet(dest)
	if err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("no route to %s found", dest.String())
	}
	ifc, err := net.InterfaceByIndex(routes[0].LinkIndex)
	if err != nil {
		return nil, err
	}

	c, err := arp.Dial(ifc)
	if err != nil {
		return nil, err
	}

	return c.Resolve(dest)
}
