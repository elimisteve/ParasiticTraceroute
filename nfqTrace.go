/*
 * nfqTrace.go - Forward/Reverse TCP traceroute using Linux NFQueue
 * Copyright (c) 2014 David Anthony Stainton
 *
 * Abstract:
 * Parasitic forward/reverse Linux-Netfilter Queue traceroute
 * for penetrating network address translation devices...
 * tracing the client's internal network.
 *
 * The MIT License (MIT)
 * Copyright (c) 2014 David Anthony Stainton
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in
 * all copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
 * THE SOFTWARE.
 *
 */

package main

import (
	"code.google.com/p/gopacket"
	"code.google.com/p/gopacket/layers"
	"code.google.com/p/gopacket/pcap"
	"encoding/binary"
	"github.com/david415/go-netfilter-queue"
	"log"
	"net"
	"sync"
	"time"
)

const (
	MAX_TTL uint8 = 255
)

// this is a composite struct type called "flowKey"
// used to track tcp/ip flows... as a hashmap key.
type flowKey [2]gopacket.Flow

// concurrent-safe hashmap of tcp/ip-flowKeys to NFQueueTraceroute`s
type FlowTracker struct {
	lock    *sync.RWMutex
	flowMap map[flowKey]*NFQueueTraceroute
}

func NewFlowTracker() *FlowTracker {
	return &FlowTracker{
		lock:    new(sync.RWMutex),
		flowMap: make(map[flowKey]*NFQueueTraceroute),
	}
}

func (f *FlowTracker) HasFlow(flow flowKey) bool {
	f.lock.RLock()
	_, ok := f.flowMap[flow]
	f.lock.RUnlock()
	return ok
}

func (f *FlowTracker) AddFlow(flow flowKey, nfqTrace *NFQueueTraceroute) {
	f.lock.Lock()
	f.flowMap[flow] = nfqTrace
	f.lock.Unlock()
}

func (f *FlowTracker) Delete(flow flowKey) {
	f.lock.Lock()
	delete(f.flowMap, flow)
	f.lock.Unlock()
}

func (f *FlowTracker) GetFlowTrace(flow flowKey) *NFQueueTraceroute {
	f.lock.RLock()
	ret := f.flowMap[flow]
	f.lock.RUnlock()
	return ret
}

type NFQueueTraceObserverOptions struct {
	// network interface to listen for ICMP responses
	iface        string
	ttlMax       uint8
	ttlRepeatMax int
	mangleFreq   int
}

type NFQueueTraceObserver struct {
	// passed in from the user in our constructor...
	options NFQueueTraceObserverOptions

	flowTracker *FlowTracker
	nfq         *netfilter.NFQueue

	// packet channel for interacting with NFQueue
	packets <-chan netfilter.NFPacket

	// this is used to stop all the traceroutes
	done chan bool

	// signal our calling party that we are finished
	// XXX get rid of this?
	finished chan bool
}

func NewNFQueueTraceObserver(options NFQueueTraceObserverOptions) *NFQueueTraceObserver {
	var err error
	o := NFQueueTraceObserver{
		options:  options,
		done:     make(chan bool),
		finished: make(chan bool),
	}

	flowTracker := NewFlowTracker()
	o.flowTracker = flowTracker
	// XXX adjust these parameters
	o.nfq, err = netfilter.NewNFQueue(0, 100, netfilter.NF_DEFAULT_PACKET_SIZE)
	if err != nil {
		panic(err)
	}
	o.packets = o.nfq.GetPackets()
	return &o
}

func (o *NFQueueTraceObserver) Start() {
	o.startReceivingReplies()
	go func() {
		for true {
			select {
			case p := <-o.packets:
				o.processPacket(p)
			case <-o.done:
				o.nfq.Close()
				close(o.done) // XXX necessary?
				// XXX todo: stop other goroutines
				break
			}
		}
	}()
}

func (o *NFQueueTraceObserver) Stop() {
	o.done <- true
}

// XXX make the locking more efficient?
func (o *NFQueueTraceObserver) processPacket(p netfilter.NFPacket) {
	ipLayer := p.Packet.Layer(layers.LayerTypeIPv4)
	tcpLayer := p.Packet.Layer(layers.LayerTypeTCP)
	if ipLayer == nil || tcpLayer == nil {
		// ignore non-tcp/ip packets
		return
	}
	ip, _ := ipLayer.(*layers.IPv4)
	tcp, _ := tcpLayer.(*layers.TCP)

	flow := flowKey{ip.NetworkFlow(), tcp.TransportFlow()}
	if o.flowTracker.HasFlow(flow) == false {
		nfqTrace := NewNFQueueTraceroute(o.options.ttlMax, o.options.ttlRepeatMax, o.options.mangleFreq)
		o.flowTracker.AddFlow(flow, nfqTrace)
	}
	nfqTrace := o.flowTracker.GetFlowTrace(flow)
	nfqTrace.processPacket(p)
}

// return a net.IP channel to report all the ICMP reponse SrcIP addresses
// that have the ICMP time exceeded flag set
func (o *NFQueueTraceObserver) startReceivingReplies() {
	snaplen := 65536
	filter := "icmp" // the idea here is to capture only ICMP packets

	var eth layers.Ethernet
	var ip layers.IPv4
	var icmp layers.ICMPv4
	var payload gopacket.Payload
	var flow flowKey

	decoded := make([]gopacket.LayerType, 0, 4)

	handle, err := pcap.OpenLive(o.options.iface, int32(snaplen), true, pcap.BlockForever)
	if err != nil {
		log.Fatal("error opening pcap handle: ", err)
	}
	if err := handle.SetBPFFilter(filter); err != nil {
		log.Fatal("error setting BPF filter: ", err)
	}

	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &ip, &icmp, &payload)

	go func() {
		for true {
			data, _, err := handle.ReadPacketData()
			if err != nil {
				continue
			}
			err = parser.DecodeLayers(data, &decoded)
			if err != nil {
				continue
			}
			typ := uint8(icmp.TypeCode >> 8)
			if typ != layers.ICMPv4TypeTimeExceeded {
				continue
			}

			// XXX todo: check that the IP header protocol value is set to TCP
			flow = getPacketFlow(payload)

			// XXX it feels dirty to have the mutex around the hashmap
			// i'm thinking about using channels instead...
			if o.flowTracker.HasFlow(flow) == false {
				// ignore ICMP ttl expire packets that are for flows other than the ones we are currently tracking
				continue
			}

			nfqTrace := o.flowTracker.GetFlowTrace(flow)
			nfqTrace.replyReceived(ip.SrcIP)
		}
	}()
}

type NFQueueTraceroute struct {
	ttl          uint8
	ttlMax       uint8
	ttlRepeat    int
	ttlRepeatMax int
	mangleFreq   int
	count        int

	// ip.TTL -> list of ip addrs
	traceResult map[uint8][]net.IP

	stopped          bool
	responseTimedOut bool

	// XXX should it be a pointer instead?
	receivePacketChannel chan netfilter.NFPacket

	resumeTimerChannel  chan bool
	stopTimerChannel    chan bool
	restartTimerChannel chan bool
}

// conduct an nfqueue tcp traceroute;
// - send each TTL out ttlRepeatMax number of times.
// - only mangle a packet's TTL after mangleFreq number
// of packets have traversed the flow
func NewNFQueueTraceroute(ttlMax uint8, ttlRepeatMax, mangleFreq int) *NFQueueTraceroute {
	log.Print("NewNFQueueTraceroute\n")
	nfqTrace := NFQueueTraceroute{
		ttl:                 1,
		ttlMax:              ttlMax,
		ttlRepeat:           1,
		ttlRepeatMax:        ttlRepeatMax,
		mangleFreq:          mangleFreq,
		count:               1,
		traceResult:         make(map[uint8][]net.IP, 1),
		stopped:             false,
		responseTimedOut:    false,
		stopTimerChannel:    make(chan bool),
		restartTimerChannel: make(chan bool),
	}
	nfqTrace.StartResponseTimer()
	return &nfqTrace
}

func (n *NFQueueTraceroute) StartResponseTimer() {
	log.Print("StartResponseTimer\n")

	go func() {
		for {
			select {
			case <-time.After(time.Duration(200) * time.Second):
				log.Print("TimerExpired\n")

				if n.ttl >= n.ttlMax && n.ttlRepeat >= n.ttlRepeatMax {
					n.Stop()
					return
				}

				n.responseTimedOut = true
			case <-n.restartTimerChannel:
				log.Print("restartTimerChannel fired\n")
				continue
			case <-n.stopTimerChannel:
				log.Print("stopTimerChannel fired\n")
				return
			}
		}
	}()
}

func (n *NFQueueTraceroute) Stop() {
	log.Print("NFQueueTraceroute.Stop()\n")
	n.stopped = true
	n.stopTimerChannel <- true
	close(n.stopTimerChannel)
	close(n.restartTimerChannel)

}

// given a packet we decided weather or not to mangle the TTL
// for our tracerouting purposes.
func (n *NFQueueTraceroute) processPacket(p netfilter.NFPacket) {

	if n.stopped {
		p.SetVerdict(netfilter.NF_ACCEPT)
		return
	}

	if n.count%n.mangleFreq == 0 {
		log.Printf("processPacket mangle case n.ttl %d, n.ttlRepeat %d, n.ttlRepeatMax %d\n", n.ttl, n.ttlRepeat, n.ttlRepeatMax)

		n.ttlRepeat += 1

		if n.responseTimedOut {
			n.ttl += 1
			n.ttlRepeat = 0
			n.responseTimedOut = false
			n.restartTimerChannel <- true
		} else if n.ttlRepeat == n.ttlRepeatMax {
			log.Print("ttlRepeatMax reached case\n")
			n.ttl += 1
			n.ttlRepeat = 0
			n.responseTimedOut = false
			n.restartTimerChannel <- true
		}

		// terminate trace upon max ttl and ttlRepeatMax conditions
		if n.ttl > n.ttlMax && n.ttlRepeat == (n.ttlRepeatMax-1) {
			n.Stop()
			p.SetVerdict(netfilter.NF_ACCEPT)
			return
		}

		p.SetModifiedVerdict(netfilter.NF_REPEAT, serializeWithTTL(p.Packet, n.ttl))
	} else {
		p.SetVerdict(netfilter.NF_ACCEPT)
	}
	n.count = n.count + 1
}

// XXX
// store the "reply" source ip address (icmp ttl expired packet with payload matching this flow)
func (n *NFQueueTraceroute) replyReceived(ip net.IP) {
	log.Printf("replyReceived: ttl %d ip %s\n", n.ttl, ip.String())

	n.traceResult[n.ttl] = append(n.traceResult[n.ttl], ip)
	if n.ttl == n.ttlMax && len(n.traceResult[n.ttl]) >= n.ttlRepeatMax {
		n.Stop() // finished!
	}
}

// This function takes a gopacket.Packet and a TTL
// and returns a byte array of the serialized packet with the specified TTL
func serializeWithTTL(p gopacket.Packet, ttl uint8) []byte {
	ipLayer := p.Layer(layers.LayerTypeIPv4)
	if ipLayer == nil {
		return nil
	}
	tcpLayer := p.Layer(layers.LayerTypeTCP)
	if tcpLayer == nil {
		return nil
	}
	ip, _ := ipLayer.(*layers.IPv4)
	ip.TTL = ttl
	tcp, _ := tcpLayer.(*layers.TCP)
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	tcp.SetNetworkLayerForChecksum(ip)
	rawPacketBuf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(rawPacketBuf, opts, ip, tcp); err != nil {
		return nil
	}
	return rawPacketBuf.Bytes()
}

// We use this to deal with rfc792 implementations where
// the original packet is NOT sent back via ICMP payload but
// instead 64 bits of the original packet are sent.
// https://tools.ietf.org/html/rfc792
// Returns a TCP Flow.
// XXX obviously the 64 bits could be from a UDP packet or something else
// however this is *good-enough* for NFQueue TCP traceroute!
// XXX should we look at the protocol specified in the IP header
// and set it's type here? no we should probably not even get this
// far if the IP header has something other than TCP specified...
func getTCPFlowFromTCPHead(data []byte) gopacket.Flow {
	var srcPort, dstPort layers.TCPPort
	srcPort = layers.TCPPort(binary.BigEndian.Uint16(data[0:2]))
	dstPort = layers.TCPPort(binary.BigEndian.Uint16(data[2:4]))
	// XXX convert to tcp/ip flow
	tcpSrc := layers.NewTCPPortEndpoint(srcPort)
	tcpDst := layers.NewTCPPortEndpoint(dstPort)
	tcpFlow, _ := gopacket.FlowFromEndpoints(tcpSrc, tcpDst)
	// error (^ _) is only non-nil if the two endpoint types don't match
	return tcpFlow
}

// given a byte array packet return a tcp/ip flow
func getPacketFlow(packet []byte) flowKey {
	var ip layers.IPv4
	var tcp layers.TCP
	decoded := []gopacket.LayerType{}
	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeIPv4, &ip, &tcp)
	err := parser.DecodeLayers(packet, &decoded)
	if err != nil {
		// XXX last 64 bits... we only use the last 32 bits
		tcpHead := packet[len(packet)-8 : len(packet)]
		tcpFlow := getTCPFlowFromTCPHead(tcpHead)
		return flowKey{ip.NetworkFlow(), tcpFlow}
	}
	return flowKey{ip.NetworkFlow(), tcp.TransportFlow()}
}

/***
use this rough POC with an iptables nfqueue rule that will select
a tcp flow direction like this:
iptables -A OUTPUT -j NFQUEUE --queue-num 0 -p tcp --dport 2666

***/
func main() {
	options := NFQueueTraceObserverOptions{
		iface:        "wlan0",
		ttlMax:       40,
		ttlRepeatMax: 3,
		mangleFreq:   6,
	}
	o := NewNFQueueTraceObserver(options)
	o.Start()
	// XXX
	<-o.finished
}
