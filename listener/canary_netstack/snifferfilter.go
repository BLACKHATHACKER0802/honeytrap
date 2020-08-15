package nscanary

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/honeytrap/honeytrap/event"
	"github.com/honeytrap/honeytrap/pkg/protonames"
	"github.com/honeytrap/honeytrap/pushers"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/fragmentation"
	"gvisor.dev/gvisor/pkg/tcpip/network/hash"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type SniffAndFilter struct {
	events    pushers.Channel
	knockChan chan KnockGrouper

	ourMAC tcpip.LinkAddress //our network interface hardware address.

	blockTCPPort       func(uint16) bool
	blockUDPPort       func(uint16) bool
	blockSourceIP      func(tcpip.Address) bool
	blockDestinationIP func(tcpip.Address) bool

	fragmentation *fragmentation.Fragmentation
}

type SniffAndFilterOpts struct {
	EventChan          pushers.Channel
	KnockChan          chan KnockGrouper
	BlockPorts         []string
	BlockSourceIP      []string
	BlockDestinationIP []string

	// blocks events for outbound packets.
	OurMAC tcpip.LinkAddress
}

func NewSniffAndFilter(opts SniffAndFilterOpts) *SniffAndFilter {

	return &SniffAndFilter{
		events:             opts.EventChan,
		knockChan:          opts.KnockChan,
		ourMAC:             opts.OurMAC,
		blockTCPPort:       BlockPortFn(opts.BlockPorts, "tcp"),
		blockUDPPort:       BlockPortFn(opts.BlockPorts, "udp"),
		blockSourceIP:      BlockIPFn(opts.BlockSourceIP),
		blockDestinationIP: BlockIPFn(opts.BlockDestinationIP),
		fragmentation:      fragmentation.NewFragmentation(fragmentation.HighFragThreshold, fragmentation.LowFragThreshold, fragmentation.DefaultReassembleTimeout),
	}
}

//BlockPortFn sets function to block on port number.
// format: "<protocol>/{port-number> ex. tcp/80
// proto: "tcp", "udp"
func BlockPortFn(block []string, proto string) func(uint16) bool {
	ports := []uint16{}

	for _, p := range block {
		pp := strings.Split(p, "/")
		if len(pp) != 2 {
			log.Errorf("bad address format: %s", p)
			continue
		}
		if pp[0] != proto {
			continue
		}
		num, err := strconv.ParseUint(pp[1], 10, 16)
		if err != nil {
			log.Errorf("bad port number: %s", pp[1])
			continue
		}
		ports = append(ports, uint16(num))
	}

	return func(port uint16) bool {
		for _, p := range ports {
			if p == port {
				log.Debugf("blocking port: %d", p)
				return true
			}
		}
		return false
	}
}

//BlockIPFn sets function to block on IP address.
// block format: IP address as string, "10.1.1.100" or "2001:db7::68"
func BlockIPFn(block []string) func(tcpip.Address) bool {
	addrs := make([]tcpip.Address, len(block))

	for i, a := range block {
		// Parse the IP address. Support both ipv4 and ipv6.
		parsedAddr := net.ParseIP(a)
		if parsedAddr == nil {
			log.Errorf("Bad IP address: %v", a)
			continue
		}

		if parsedAddr.To4() != nil {
			addrs[i] = tcpip.Address(parsedAddr.To4())
		} else if parsedAddr.To16() != nil {
			addrs[i] = tcpip.Address(parsedAddr.To16())
		} else {
			log.Errorf("Unknown IP type: %v", a)
		}
	}

	return func(ip tcpip.Address) bool {
		for _, a := range addrs {
			if a == ip {
				return true
			}
		}
		return false
	}
}

func (s *SniffAndFilter) logPacket(prefix string, protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer, gso *stack.GSO) bool {
	// true: netstack handles request.
	// false: let host handle it.
	handleRequest := true

	// Figure out the network layer info.
	var (
		transProto uint8
		srcMAC     tcpip.LinkAddress
		destMAC    tcpip.LinkAddress
	)

	eoptions := make([]event.Option, 0, 16)
	eoptions = append(eoptions, event.Custom("network-protocol-number", protocol))
	eoptions = append(eoptions, event.Custom("network-protocol", protonames.NetworkName(int(protocol))))

	// set the hardware addresses.
	if len(pkt.LinkHeader) > 0 {
		eth := header.Ethernet(pkt.LinkHeader)
		srcMAC = eth.SourceAddress()
		if srcMAC == s.ourMAC {
			// skip logging if we are the source.
			return true
		}
		destMAC = eth.DestinationAddress()
		eoptions = append(eoptions,
			event.DestinationHardwareAddr(net.HardwareAddr(destMAC)),
			event.SourceHardwareAddr(net.HardwareAddr(srcMAC)),
		)
	}

	src := tcpip.Address("unknown")
	dst := tcpip.Address("unknown")
	id := 0
	size := uint16(0)

	// Create a clone of pkt, including any headers if present. Avoid allocating
	// backing memory for the clone.
	views := [8]buffer.View{}
	vv := buffer.NewVectorisedView(0, views[:0])
	vv.AppendView(pkt.Header.View())
	vv.Append(pkt.Data)

	switch protocol {
	case header.IPv4ProtocolNumber:
		h := header.IPv4(vv.ToView())
		if !h.IsValid(len(h)) {
			log.Debugf("IPv4 malformed packet: %x", h)
			return handleRequest
		}

		if h.More() || h.FragmentOffset() != 0 {
			if pkt.Data.Size()+len(pkt.TransportHeader) == 0 {
				// Drop the packet as it's marked as a fragment but has
				// no payload.
				return handleRequest
			}
			// The packet is a fragment, let's try to reassemble it.
			last := h.FragmentOffset() + uint16(pkt.Data.Size()) - 1
			// Drop the packet if the fragmentOffset is incorrect. i.e the
			// combination of fragmentOffset and pkt.Data.size() causes a
			// wrap around resulting in last being less than the offset.
			if last < h.FragmentOffset() {
				return true
			}
			var ready bool
			var err error
			vv, ready, err = s.fragmentation.Process(hash.IPv4FragmentHash(h), h.FragmentOffset(), last, h.More(), vv)
			if err != nil {
				return handleRequest
			}
			if !ready {
				return handleRequest
			}
		}

		src = h.SourceAddress()
		dst = h.DestinationAddress()
		if s.blockSourceIP(src) || s.blockDestinationIP(dst) {
			handleRequest = false
		}
		transProto = h.Protocol()
		size = h.PayloadLength()
		vv.TrimFront(int(h.HeaderLength()))
		id = int(h.ID())
		eoptions = append(eoptions,
			event.Custom("ip-version", "4"),
			event.Custom("source-ip", src.String()),
			event.Custom("destination-ip", dst.String()),
		)

	case header.IPv6ProtocolNumber:
		hdr := header.IPv6(vv.ToView())
		if !hdr.IsValid(len(hdr)) {
			//TODO (jerry): log invalid header??
			return handleRequest
		}
		src = hdr.SourceAddress()
		dst = hdr.DestinationAddress()
		if s.blockSourceIP(src) || s.blockDestinationIP(dst) {
			handleRequest = false
		}
		transProto = hdr.NextHeader()
		size = hdr.PayloadLength()
		vv.TrimFront(vv.Size() - int(size))

		eoptions = append(eoptions,
			event.Custom("ip-version", "6"),
			event.Custom("source-ip", src.String()),
			event.Custom("destination-ip", dst.String()),
			event.Payload(hdr.Payload()),
		)

	case header.ARPProtocolNumber:
		hdr := header.ARP(vv.ToView())
		if !hdr.IsValid() {
			log.Debug("Invalid ARP header")
			return handleRequest
		}
		line := fmt.Sprintf(
			"%s arp %s (%s) -> %s (%s) valid:%t",
			prefix,
			tcpip.Address(hdr.ProtocolAddressSender()), tcpip.LinkAddress(hdr.HardwareAddressSender()),
			tcpip.Address(hdr.ProtocolAddressTarget()), tcpip.LinkAddress(hdr.HardwareAddressTarget()),
			hdr.IsValid(),
		)

		s.events.Send(event.New(
			event.Category("arp"),
			event.DestinationHardwareAddr(net.HardwareAddr(hdr.HardwareAddressTarget())),
			event.SourceHardwareAddr(net.HardwareAddr(hdr.HardwareAddressSender())),
			event.SourceIP(hdr.ProtocolAddressSender()),
			event.DestinationIP(hdr.ProtocolAddressTarget()),
			event.Custom("arp-opcode", hdr.Op()),
			event.Message(line),
		))
		return handleRequest
	default:
		eoptions = append(eoptions, EventCategoryUnknown)
		eoptions = append(eoptions, event.Payload(vv.ToView()))
		s.events.Send(event.New(
			eoptions...,
		))
		return handleRequest
	}

	// Figure out the transport layer info.
	eoptions = append(eoptions,
		event.Custom("transport-protocol-number", transProto),
		event.Custom("transport-protocol", protonames.TransportName(transProto)),
	)
	transName := "unknown"
	srcPort := uint16(0)
	dstPort := uint16(0)
	details := ""
	switch tcpip.TransportProtocolNumber(transProto) {
	case header.ICMPv4ProtocolNumber:
		transName = "icmp"
		if vv.Size() < header.ICMPv4MinimumSize {
			break
		}
		hdr := header.ICMPv4(vv.ToView())
		icmpType := "unknown"
		switch hdr.Type() {
		case header.ICMPv4EchoReply:
			icmpType = "echo reply"
		case header.ICMPv4DstUnreachable:
			icmpType = "destination unreachable"
		case header.ICMPv4SrcQuench:
			icmpType = "source quench"
		case header.ICMPv4Redirect:
			icmpType = "redirect"
		case header.ICMPv4Echo:
			icmpType = "echo"
		case header.ICMPv4TimeExceeded:
			icmpType = "time exceeded"
		case header.ICMPv4ParamProblem:
			icmpType = "param problem"
		case header.ICMPv4Timestamp:
			icmpType = "timestamp"
		case header.ICMPv4TimestampReply:
			icmpType = "timestamp reply"
		case header.ICMPv4InfoRequest:
			icmpType = "info request"
		case header.ICMPv4InfoReply:
			icmpType = "info reply"
		}
		line := fmt.Sprintf("%s %s %s -> %s %s len:%d id:%04x code:%d", prefix, transName, src, dst, icmpType, size, id, hdr.Code())
		//TODO (jerry): Add communty-id
		eoptions = append(eoptions,
			event.Category("icmp"),
			event.Protocol("icmp4"),
			event.Custom("icmp-type", icmpType),
			event.Custom("icmp-code", hdr.Code()),
			event.Message(line),
			event.Payload(hdr.Payload()),
		)

		s.knockChan <- KnockICMP{
			IPVersion:               4,
			SourceHardwareAddr:      net.HardwareAddr(srcMAC),
			DestinationHardwareAddr: net.HardwareAddr(destMAC),
			SourceIP:                src,
			DestinationIP:           dst,
		}

	case header.ICMPv6ProtocolNumber:
		transName = "icmp"
		if vv.Size() < header.ICMPv6MinimumSize {
			break
		}
		hdr := header.ICMPv6(vv.ToView())
		icmpType := "unknown"
		switch hdr.Type() {
		case header.ICMPv6DstUnreachable:
			icmpType = "destination unreachable"
		case header.ICMPv6PacketTooBig:
			icmpType = "packet too big"
		case header.ICMPv6TimeExceeded:
			icmpType = "time exceeded"
		case header.ICMPv6ParamProblem:
			icmpType = "param problem"
		case header.ICMPv6EchoRequest:
			icmpType = "echo request"
		case header.ICMPv6EchoReply:
			icmpType = "echo reply"
		case header.ICMPv6RouterSolicit:
			icmpType = "router solicit"
		case header.ICMPv6RouterAdvert:
			icmpType = "router advert"
		case header.ICMPv6NeighborSolicit:
			icmpType = "neighbor solicit"
		case header.ICMPv6NeighborAdvert:
			icmpType = "neighbor advert"
		case header.ICMPv6RedirectMsg:
			icmpType = "redirect message"
		}
		line := fmt.Sprintf("%s %s %s -> %s %s len:%d id:%04x code:%d", prefix, transName, src, dst, icmpType, size, id, hdr.Code())
		//TODO (jerry): Add communty-id
		eoptions = append(eoptions,
			event.Category("icmp"),
			event.Protocol("icmp6"),
			event.Custom("icmp-type", icmpType),
			event.Custom("icmp-code", hdr.Code()),
			event.Message(line),
			event.Payload(hdr.Payload()),
		)

		s.knockChan <- KnockICMP{
			IPVersion:               6,
			SourceHardwareAddr:      net.HardwareAddr(srcMAC),
			DestinationHardwareAddr: net.HardwareAddr(destMAC),
			SourceIP:                src,
			DestinationIP:           dst,
		}

	case header.UDPProtocolNumber:
		transName = "udp"
		if vv.Size() < header.UDPMinimumSize {
			break
		}
		hdr := header.UDP(vv.ToView())

		srcPort = hdr.SourcePort()
		dstPort = hdr.DestinationPort()
		details = fmt.Sprintf("xsum: 0x%x", hdr.Checksum())
		size -= header.UDPMinimumSize

		if s.blockUDPPort(srcPort) || s.blockUDPPort(dstPort) {
			handleRequest = false
		}
		//TODO (jerry): Add communty-id
		eoptions = append(eoptions,
			event.Category("udp"),
			event.SourcePort(srcPort),
			event.DestinationPort(dstPort),
			event.Payload(hdr.Payload()),
		)

		s.knockChan <- KnockUDPPort{
			SourceHardwareAddr:      net.HardwareAddr(srcMAC),
			DestinationHardwareAddr: net.HardwareAddr(destMAC),
			SourceIP:                src,
			DestinationIP:           dst,
			DestinationPort:         dstPort,
		}

	case header.TCPProtocolNumber:
		transName = "tcp"
		if vv.Size() < header.TCPMinimumSize {
			break
		}
		hdr := header.TCP(vv.ToView())
		offset := int(hdr.DataOffset())
		if offset < header.TCPMinimumSize {
			details += fmt.Sprintf("invalid packet: tcp data offset too small %d", offset)
			break
		}
		if offset > vv.Size() {
			details += fmt.Sprintf("invalid packet: tcp data offset %d larger than packet buffer length %d", offset, vv.Size())
			break
		}

		srcPort = hdr.SourcePort()
		dstPort = hdr.DestinationPort()
		size -= uint16(offset)

		if s.blockTCPPort(srcPort) || s.blockTCPPort(dstPort) {
			handleRequest = false
		}

		// Initialize the TCP flags.
		flags := hdr.Flags()
		flagsStr := []byte("FSRPAU")
		for i := range flagsStr {
			if flags&(1<<uint(i)) == 0 {
				flagsStr[i] = ' '
			}
		}
		details = fmt.Sprintf("flags:0x%02x (%s) seqnum: %d ack: %d win: %d xsum:0x%x", flags, string(flagsStr), hdr.SequenceNumber(), hdr.AckNumber(), hdr.WindowSize(), hdr.Checksum())
		if flags&header.TCPFlagSyn != 0 {
			details += fmt.Sprintf(" options: %+v", header.ParseSynOptions(hdr.Options(), flags&header.TCPFlagAck != 0))
		} else {
			details += fmt.Sprintf(" options: %+v", hdr.ParsedOptions())
		}

		//TODO (jerry): Add communty-id
		eoptions = append(eoptions,
			event.Category("tcp"),
			event.SourcePort(srcPort),
			event.DestinationPort(dstPort),
			event.Payload(hdr.Payload()),
		)

		s.knockChan <- KnockTCPPort{
			SourceHardwareAddr:      net.HardwareAddr(srcMAC),
			DestinationHardwareAddr: net.HardwareAddr(destMAC),
			SourceIP:                src,
			DestinationIP:           dst,
			DestinationPort:         dstPort,
		}

	default:
		eoptions = append(eoptions, EventCategoryUnknown)
		eoptions = append(eoptions, event.Payload(vv.ToView()))
	}

	line := fmt.Sprintf("%s %s %s:%d -> %s:%d len:%d id:%04x %s", prefix, transName, src, srcPort, dst, dstPort, size, id, details)

	eoptions = append(eoptions,
		event.Message(line),
	)

	s.events.Send(event.New(
		eoptions...,
	))

	return handleRequest
}