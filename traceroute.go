// Package traceroute provides functions for executing a tracroute to a remote
// host.
package traceroute

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"golang.org/x/net/ipv4"
	"net"
	"syscall"
	"time"
)

const DEFAULT_PORT = 33434
const DEFAULT_MAX_HOPS = 64
const DEFAULT_TIMEOUT_MS = 500
const DEFAULT_RETRIES = 3
const DEFAULT_PACKET_SIZE = 52
const UDP_HEADER_LEN = 8

type UdpHeader struct {
	SourcePort      uint16
	DestinationPort uint16
	Length          uint16
	Checksum        uint16
}

// Return the first non-loopback address as a 4 byte IP address. This address
// is used for sending packets out.
func socketAddr() ([][4]byte, error) {
	addrs, err := net.InterfaceAddrs()
	var addrList [][4]byte
	var addr [4]byte
	if err != nil {
		return addrList, err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if len(ipnet.IP.To4()) == net.IPv4len {
				copy(addr[:], ipnet.IP.To4())
				addrList = append(addrList, addr)
			}
		}
	}
	if len(addrList) == 0 {
		err = errors.New("You do not appear to be connected to the Internet")
	}
	return addrList, err
}

// Given a host name convert it to a 4 byte IP address.
func destAddr(dest string) (destAddr [4]byte, err error) {
	addrs, err := net.LookupHost(dest)
	if err != nil {
		return
	}
	addr := addrs[0]

	ipAddr, err := net.ResolveIPAddr("ip", addr)
	if err != nil {
		return
	}
	copy(destAddr[:], ipAddr.IP.To4())
	return
}

// TracrouteOptions type
type TracerouteOptions struct {
	ip         [4]byte
	port       int
	maxHops    int
	timeoutMs  int
	retries    int
	packetSize int
}

func (options *TracerouteOptions) Port() int {
	if options.port == 0 {
		options.port = DEFAULT_PORT
	}
	return options.port
}

func (options *TracerouteOptions) SetPort(port int) {
	options.port = port
}

func (options *TracerouteOptions) MaxHops() int {
	if options.maxHops == 0 {
		options.maxHops = DEFAULT_MAX_HOPS
	}
	return options.maxHops
}

func (options *TracerouteOptions) SetMaxHops(maxHops int) {
	options.maxHops = maxHops
}

func (options *TracerouteOptions) SetOutGo(ip string) {
	if ipadd := net.ParseIP(ip); ipadd != nil {
		if len(ipadd.To4()) == net.IPv4len {
			var addr [4]byte
			copy(addr[:], ipadd.To4())
			options.ip = addr
		}
	}
}

func (options *TracerouteOptions) TimeoutMs() int {
	if options.timeoutMs == 0 {
		options.timeoutMs = DEFAULT_TIMEOUT_MS
	}
	return options.timeoutMs
}

func (options *TracerouteOptions) SetTimeoutMs(timeoutMs int) {
	options.timeoutMs = timeoutMs
}

func (options *TracerouteOptions) Retries() int {
	if options.retries == 0 {
		options.retries = DEFAULT_RETRIES
	}
	return options.retries
}

func (options *TracerouteOptions) SetRetries(retries int) {
	options.retries = retries
}

func (options *TracerouteOptions) PacketSize() int {
	if options.packetSize == 0 {
		options.packetSize = DEFAULT_PACKET_SIZE
	}
	return options.packetSize
}

func (options *TracerouteOptions) SetPacketSize(packetSize int) {
	options.packetSize = packetSize
}

// TracerouteHop type
type TracerouteHop struct {
	Success     bool
	Address     [4]byte
	Host        string
	N           int
	ElapsedTime time.Duration
	TTL         int
}

func (hop *TracerouteHop) AddressString() string {
	return fmt.Sprintf("%v.%v.%v.%v", hop.Address[0], hop.Address[1], hop.Address[2], hop.Address[3])
}

func (hop *TracerouteHop) HostOrAddressString() string {
	hostOrAddr := hop.AddressString()
	if hop.Host != "" {
		hostOrAddr = hop.Host
	}
	return hostOrAddr
}

// TracerouteResult type
type TracerouteResult struct {
	DestinationAddress [4]byte
	Hops               []TracerouteHop
}

func notify(hop TracerouteHop, channels []chan TracerouteHop) {
	for _, c := range channels {
		c <- hop
	}
}

func closeNotify(channels []chan TracerouteHop) {
	for _, c := range channels {
		close(c)
	}
}

// Traceroute uses the given dest (hostname) and options to execute a traceroute
// from your machine to the remote host.
//
// Outbound packets are UDP packets and inbound packets are ICMP.
//
// Returns a TracerouteResult which contains an array of hops. Each hop includes
// the elapsed time and its IP address.
func Traceroute(dest string, options *TracerouteOptions, c ...chan TracerouteHop) (result TracerouteResult, err error) {
	result.Hops = []TracerouteHop{}
	destAddr, err := destAddr(dest)
	result.DestinationAddress = destAddr
	addrList, err := socketAddr()
	if err != nil {
		return
	}
	socketAddr := addrList[0]
	if options.ip[0] != byte(0) {
		for _, addr := range addrList {
			if addr == options.ip {
				socketAddr = addr
			}
		}
	}
	timeoutMs := (int64)(options.TimeoutMs())
	tv := syscall.NsecToTimeval(1000 * 1000 * timeoutMs)

	ttl := 1
	retry := 0
	for {
		//log.Println("TTL: ", ttl)
		start := time.Now()

		// Set up the socket to receive inbound packets
		recvSocket, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
		if err != nil {
			return result, err
		}

		// Set up the socket to send packets out.
		sendSocket, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
		if err != nil {
			return result, err
		}
		// This sets the current hop TTL
		syscall.SetsockoptInt(sendSocket, syscall.IPPROTO_IP, syscall.IP_HDRINCL, ttl)
		//syscall.SetsockoptInt(sendSocket, 0x0, syscall.IP_TTL, ttl)
		// This sets the timeout to wait for a response from the remote host
		syscall.SetsockoptTimeval(recvSocket, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

		defer syscall.Close(recvSocket)
		defer syscall.Close(sendSocket)

		// Bind to the local socket to listen for ICMP packets
		syscall.Bind(recvSocket, &syscall.SockaddrInet4{Port: options.Port(), Addr: socketAddr})
		data := []byte("00000000000000000000000")
		udpHeader_t := UdpHeader{
			SourcePort:      1,
			DestinationPort: 34567,
			Length:          uint16(UDP_HEADER_LEN + len(data)),
		}
		buf := bytes.NewBuffer([]byte{})
		err = binary.Write(buf, binary.BigEndian, &udpHeader_t)
		if err != nil {
			return result, err
		}
		udpHeader := buf.Bytes()
		dataWithHeader := append(udpHeader, data...)
		h := &ipv4.Header{
			Version:  ipv4.Version,
			Len:      ipv4.HeaderLen,
			TotalLen: ipv4.HeaderLen + UDP_HEADER_LEN + len(data),
			ID:       12345,
			Protocol: 17,
			TTL:      ttl,
			Dst:      net.IPv4(destAddr[0], destAddr[1], destAddr[2], destAddr[3]),
			Src:      net.IPv4(socketAddr[0], socketAddr[1], socketAddr[2], socketAddr[3]),
		}
		out, err := h.Marshal()
		if err != nil {
			fmt.Println(err)
		}
		packet := append(out, dataWithHeader...)
		// Send a single null byte UDP packet
		syscall.Sendto(sendSocket, packet, 0, &syscall.SockaddrInet4{Addr: destAddr})

		var p = make([]byte, options.PacketSize())
		n, from, err := syscall.Recvfrom(recvSocket, p, 0)
		elapsed := time.Since(start)
		if err == nil {
			currAddr := from.(*syscall.SockaddrInet4).Addr

			hop := TracerouteHop{Success: true, Address: currAddr, N: n, ElapsedTime: elapsed, TTL: ttl}

			// TODO: this reverse lookup appears to have some standard timeout that is relatively
			// high. Consider switching to something where there is greater control.
			currHost, err := net.LookupAddr(hop.AddressString())
			if err == nil {
				hop.Host = currHost[0]
			}

			notify(hop, c)

			result.Hops = append(result.Hops, hop)

			ttl += 1
			retry = 0

			if ttl > options.MaxHops() || currAddr == destAddr {
				closeNotify(c)
				return result, nil
			}
		} else {
			retry += 1
			if retry > options.Retries() {
				notify(TracerouteHop{Success: false, TTL: ttl}, c)
				ttl += 1
				retry = 0
			}

			if ttl > options.MaxHops() {
				closeNotify(c)
				return result, nil
			}
		}

	}
}
