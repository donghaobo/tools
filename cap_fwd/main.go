package main

import (
	"flag"
	"fmt"
	"net"

	"github.com/google/gopacket/pcap"
)

var (
	ifname = flag.String("if", "eth01", "interface name")
	filter = flag.String("filter", "tcp and port 80", "BPF filter")
	dest   = flag.String("dest", "127.0.0.1:4789", "send to dest by vxlan")
)

func main() {
	flag.Parse()
	conn, err := net.Dial("udp", *dest)
	if err != nil {
		fmt.Println("Error dialing:", err)
		return
	}
	defer conn.Close()
	if handle, err := pcap.OpenLive(*ifname, 1600, false, pcap.BlockForever); err != nil {
		fmt.Println("read errr:", err)
		return
	} else if err := handle.SetBPFFilter(*filter); err != nil { // optional
		fmt.Println("read errr:", err)
		return
	} else {
		for {
			data, info, err := handle.ReadPacketData()
			if err != nil {
				fmt.Println("read errr:", err)
				return
			}
			if len(data) < 12 {
				fmt.Println("got errr pkt", info, data)
				continue
			}
			pkt := make([]byte, len(data)+8)
			pkt[0] = 8
			pkt[6] = 1
			copy(pkt[8:], data)
			copy(pkt[8:], []byte{0, 0, 0, 0, 0, 1}) // fixed mac 00:00:00:00:00:01
			n, err := conn.Write(pkt)
			fmt.Println("read packet:", info, "write:", n, err)
		}
	}
}
