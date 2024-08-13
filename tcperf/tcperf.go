package main

import (
	"crypto/md5"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"log"
    _ "net/http/pprof"
    "net/http"
)

var (
	port      = flag.Uint("port", 1000, "start port")
	portCnt   = flag.Uint("port-cnt", 10, "start count")
	peer      = flag.String("peer", "", "peer list")
	clientCnt = flag.Int("client-cnt", 1, "client count")
	reqCnt    = flag.Int("req", 100000, "req cnt")
	md5loop   = flag.Uint("md5-loop", 1, "md5 loop cnt")
	debug     = flag.Bool("debug", false, "debug")
	ctimeout  = flag.Int("ctimeout", 5, "connect timeout")
	timeout   = flag.Int("timeout", 5, "read write timeout")
	pport     = flag.Int("pprof-port", 23333, "pprof port")
	kb        = flag.Int("kb", 10, "kb send")
	kbMin     = flag.Int("kb-min", 4, "kb send")
)

var (
	listenCnt      atomic.Int32
	clientConnCnt  atomic.Int32
	serverConnCnt  atomic.Int32
	serverConnErr  atomic.Int32
	clientDialErr  atomic.Int32
	clientConnErr  atomic.Int32
	serverRecvReq  atomic.Int32
	serverSendResp atomic.Int32
	clientSendReq  atomic.Int32
	clientRecvResp atomic.Int32
)

var (
	logger = log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile|log.Lmicroseconds)
)

func getLocalIp() map[string]string {
    ret := map[string]string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}
			ret[ip.String()]=ip.String()
		}
	}
	return ret
}

const (
	msgTypeReq = 1
	msgTypeMd5 = 2
)

var (
    // per conn buf use too much memory, use global buf, the data in buf is not used
    maxlen = 8 + 4000 + 1000*1000 + 1000
    gbuf = make([]byte, maxlen)
)
func clienthandler(t string) {
    logger.Println("client dial to ", t)
	defer clientConnCnt.Add(-1)
	for i := 0; i < *reqCnt; i++ {
		conn, err := net.DialTimeout("tcp", t, time.Second*time.Duration(*ctimeout))
		if err != nil {
			clientDialErr.Add(1)
			continue
		}
		func() {
			defer conn.Close()
			for ; i < *reqCnt; i++ {
				conn.SetDeadline(time.Now().Add(time.Second * time.Duration(*timeout)))
				wlen := 1000 * (*kbMin) + rand.Intn(*kb)*1000
                var buf [8]byte
				binary.LittleEndian.PutUint64(buf[:], uint64(wlen))
				n, err := conn.Write(buf[:])
				if err != nil || n != 8 {
					logger.Println("client write buf failed", n, err)
					clientConnErr.Add(1)
					return
				}
				n, err = conn.Write(gbuf[:wlen])
				if err != nil || n != wlen {
					logger.Println("client write buf failed", wlen, n, err)
					clientConnErr.Add(1)
					return
				}
				clientSendReq.Add(1)
				n, err = conn.Read(buf[:8])
				if err != nil || n != 8 {
					logger.Println("client Read msg failed", n, err)
					clientConnErr.Add(1)
					return
				}
				rlen := binary.LittleEndian.Uint64(buf[:])
				if rlen != 8 || rlen > uint64(wlen) {
					logger.Println("client Read msg err", rlen, wlen, err)
					clientConnErr.Add(1)
					return
				}
				n, err = io.ReadFull(conn, gbuf[:rlen])
				if err != nil || uint64(n) != rlen {
					logger.Println("client Read msg buf err", n, err)
					clientConnErr.Add(1)
					return
				}
				clientRecvResp.Add(1)
			}
		}()
	}
}
func serverhandler(conn net.Conn) {
	defer conn.Close()
	defer serverConnCnt.Add(-1)
	for {
		conn.SetDeadline(time.Now().Add(time.Second * time.Duration(*timeout)))
		var hbuf [16]byte
		n, err := conn.Read(hbuf[:8])
		if err == io.EOF {
			return
		}
		if err != nil || n != 8 {
			logger.Println("server Read msg failed", err, n)
			serverConnErr.Add(1)
			return
		}
		rlen := binary.LittleEndian.Uint64(hbuf[:])
		if int(rlen) > maxlen || rlen < 8 {
			logger.Println("server Read msg err", rlen, err)
			serverConnErr.Add(1)
			return
		}
		n, err = io.ReadFull(conn, gbuf[:rlen])
		if err != nil || uint64(n) != rlen {
			logger.Println("server Read msg err", n, rlen, err)
			serverConnErr.Add(1)
			return
		}
		serverRecvReq.Add(1)
		// 为了占用CPU，模拟业务计算
		h := md5.New()
		hash := h.Sum(nil)
		for i := uint(0); i < *md5loop; i++ {
			h.Reset()
			h.Write(hash)
			h.Write(gbuf[:rlen])
			hash = h.Sum(nil)
		}
		binary.LittleEndian.PutUint64(hbuf[:], 8)
		copy(hbuf[8:], hash[:8])
		n, err = conn.Write(hbuf[:])
		if err != nil {
			logger.Println("server Write msg err", n, err)
			serverConnErr.Add(1)
			return
		}
		serverSendResp.Add(1)
	}
}
func serverListen(port uint) {
	ln, err := net.Listen("tcp", ":"+strconv.FormatUint(uint64(port), 10))
	if err != nil {
		fmt.Println(err)
		return
	}
	listenCnt.Add(1)
	defer ln.Close()
	defer listenCnt.Add(-1)
	logger.Println("listen port:", port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Println(err)
			return
		}
		serverConnCnt.Add(1)
		//fmt.Println("peerCnt:", peerCnt.Load(), "listenCnt:", listenCnt.Load())
		go serverhandler(conn)
	}
}
func server() {
	for i := uint(*port); i < uint(*port)+uint(*portCnt); i++ {
		go serverListen(i)
	}
}
func clientDial(peer string) {
	for i := uint(*port); i < uint(*port)+uint(*portCnt); i++ {
		for j := 0; j < *clientCnt; j++ {
			go func(port uint) {
				t := peer + ":" + strconv.FormatUint(uint64(port), 10)

				clientConnCnt.Add(1)
				clienthandler(t)
			}(i)
		}
	}
}
func client() {
	lips := getLocalIp()
	ln, _ := os.Hostname()
	peers := strings.Split(*peer, ",")
	for _, p := range peers {
		if _, ok := lips[p]; ok {
            continue
        }
        if p == ln {
			continue
		}
		go clientDial(p)
	}
}
func main() {
	flag.Parse()
	if !*debug {
		logger = log.New(io.Discard, "", 0)
	}
    go http.ListenAndServe(fmt.Sprintf(":%d", *pport), nil)
	go server()
	fmt.Println("wait 5 seconds for server up")
	time.Sleep(time.Second * 5)
	go client()
	waitn := 10
	for {
		fmt.Print(time.Now().Format("2006-01-02 15:04:05.999999999"))
		fmt.Printf("listenCnt:%d clientConnCnt:%d serverConnCnt:%d", listenCnt.Load(), clientConnCnt.Load(), serverConnCnt.Load())
		fmt.Printf(" serverRecvReq:%d serverSendResp:%d", serverRecvReq.Load(), serverSendResp.Load())
		fmt.Printf(" clientSendReq:%d clientRecvResp:%d", clientSendReq.Load(), clientRecvResp.Load())
		fmt.Printf(" clientDialErr:%d clientConnErr:%d serverConnErr:%d\n", clientDialErr.Load(), clientConnErr.Load(), serverConnErr.Load())
		time.Sleep(time.Second)
		if clientConnCnt.Load() == 0 && serverConnCnt.Load() == 0 {
			waitn--
		} else {
			waitn = 10
		}
		if waitn == 0 {
			break
		}
	}
}

