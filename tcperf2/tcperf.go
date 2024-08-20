package main

import (
	"crypto/md5"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"math/rand"

	"github.com/xtaci/gaio"
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
	parallel  = flag.Int("parallel", 10, "num of parallel epoll loops to run")
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
			ret[ip.String()] = ip.String()
		}
	}
	return ret
}

var (
	// per conn buf use too much memory, use global buf, the data in buf is not used
	maxlen = 8 + 4000 + 1000*1000 + 1000
	gbuf   = make([]byte, maxlen)
)

const (
	readHead = iota
	readData
	writeHead
	writeData
)

type serverConn struct {
	stat int
	head [16]byte
	ddl  time.Time
	rlen int
	err  error
	conn net.Conn
	w    *gaio.Watcher
}

func serverhandler(sc *serverConn) {
	if sc.err != nil {
		if sc.err != io.EOF {
			logger.Println("server err", sc)
			serverConnErr.Add(1)
		}
		serverConnCnt.Add(-1)
		sc.conn.Close()
		sc.w.Free(sc.conn)
		return
	}
	switch sc.stat {
	case readHead:
		rlen := binary.LittleEndian.Uint64(sc.head[:])
		if int(rlen) > maxlen || rlen < 8 {
			logger.Println("server Read msg err", rlen)
			serverConnErr.Add(1)
			sc.w.Free(sc.conn)
		} else {
			sc.rlen = int(rlen)
			sc.stat = readData
			sc.w.ReadFull(sc, sc.conn, gbuf[:rlen], sc.ddl)
		}
	case readData:
		sc.stat = writeData
		serverRecvReq.Add(1)
		h := md5.New()
		hash := h.Sum(nil)
		for i := uint(0); i < *md5loop; i++ {
			h.Reset()
			h.Write(hash)
			h.Write(gbuf[:sc.rlen])
			hash = h.Sum(nil)
		}
		binary.LittleEndian.PutUint64(sc.head[:], 8)
		copy(sc.head[8:], hash[:8])
		sc.w.WriteTimeout(sc, sc.conn, sc.head[:], sc.ddl)
	case writeData:
		serverSendResp.Add(1)
		sc.ddl = time.Now().Add(time.Second * time.Duration(*timeout))
		sc.stat = readHead
		sc.w.ReadFull(sc, sc.conn, sc.head[:8], sc.ddl)
	}
}
func serverListen(port uint) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatal(err)
	}
	listenCnt.Add(1)
	defer listenCnt.Add(-1)
	// this loop keeps on accepting connections and send to main loop
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			return
		}
		serverConnCnt.Add(1)
		sc := &serverConn{}
		sc.conn = conn
		sc.w = getiow()
		sc.stat = readHead
		sc.ddl = time.Now().Add(time.Second * time.Duration(*timeout))
		sc.w.ReadFull(sc, conn, sc.head[:8], sc.ddl)
	}
}
func server() {
	for i := uint(*port); i < uint(*port)+uint(*portCnt); i++ {
		go serverListen(i)
	}
}

type clientConn struct {
	stat int
	t    string
	head [8]byte
	ddl  time.Time
	wlen int
	req  int
	conn net.Conn
	err  error
	w    *gaio.Watcher
}

func ccDial(cc *clientConn) {
	if cc.conn != nil {
		cc.w.Free(cc.conn)
		cc.conn.Close()
		cc.conn = nil
	}
	for cc.req < *reqCnt {
		conn, err := net.DialTimeout("tcp", cc.t, time.Second*time.Duration(*ctimeout))
		if err != nil {
			clientDialErr.Add(1)
			cc.req++
		} else {
			if cc.w == nil {
				cc.w = getiow()
			}
			cc.conn = conn
			cc.stat = writeHead
			cc.ddl = time.Now().Add(time.Second * time.Duration(*timeout))
			cc.wlen = 1000*(*kbMin) + rand.Intn(*kb)*1000
			binary.LittleEndian.PutUint64(cc.head[:], uint64(cc.wlen))
			cc.w.WriteTimeout(cc, cc.conn, cc.head[:], cc.ddl)
			break
		}
	}
	if cc.req >= *reqCnt {
		clientConnCnt.Add(-1)
	}
}
func clienthandler(cc *clientConn) {
	if cc.err != nil {
		clientConnErr.Add(1)
		go ccDial(cc)
		return
	}
	switch cc.stat {
	case writeHead:
		cc.stat = writeData
		cc.w.WriteTimeout(cc, cc.conn, gbuf[:cc.wlen], cc.ddl)
	case writeData:
		clientSendReq.Add(1)
		cc.stat = readHead
		cc.w.ReadFull(cc, cc.conn, cc.head[:], cc.ddl)
	case readHead:
		rlen := binary.LittleEndian.Uint64(cc.head[:])
		if rlen != 8 {
			logger.Println("client Read msg err rlen ", rlen)
			clientConnErr.Add(1)
			go ccDial(cc)
			return
		}
		cc.stat = readData
		cc.w.ReadFull(cc, cc.conn, cc.head[:], cc.ddl)
	case readData:
		clientRecvResp.Add(1)
		cc.req++
		if cc.req < *reqCnt {
			cc.stat = writeHead
			cc.ddl = time.Now().Add(time.Second * time.Duration(*timeout))
			cc.wlen = 1000*(*kbMin) + rand.Intn(*kb)*1000
			binary.LittleEndian.PutUint64(cc.head[:], uint64(cc.wlen))
			cc.w.WriteTimeout(cc, cc.conn, cc.head[:], cc.ddl)
		} else {
			cc.w.Free(cc.conn)
			cc.conn.Close()
			cc.conn = nil
			clientConnCnt.Add(-1)
		}
	}
}
func clientDial(peer string) {
	for i := uint(*port); i < uint(*port)+uint(*portCnt); i++ {
		for j := 0; j < *clientCnt; j++ {
			cc := &clientConn{}
			clientConnCnt.Add(1)
			cc.t = peer + ":" + strconv.FormatUint(uint64(i), 10)
			logger.Println("client dial to ", cc.t)
			go ccDial(cc)
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
func startIOLoop() {
	w, err := gaio.NewWatcher()
	if err != nil {
		log.Fatal("io loop NewWatcher failed, ", err)
	}
	iowsLock.Lock()
	iows = append(iows, w)
	iowsLock.Unlock()
	for {
		results, err := w.WaitIO()
		if err != nil {
			log.Println(err)
			return
		}
		for _, res := range results {
			switch c := res.Context.(type) {
			case *serverConn:
				c.err = res.Error
				serverhandler(c)
			case *clientConn:
				c.err = res.Error
				clienthandler(c)
			default:
				logger.Println("wrong conn type", c)
			}
		}
	}
}

var (
	iowsLock sync.Mutex
	iowsidx  int
	iows     = []*gaio.Watcher{}
)

func getiow() *gaio.Watcher {
	var ret *gaio.Watcher
	iowsLock.Lock()
	if len(iows) > 0 {
		ret = iows[iowsidx]
		iowsidx++
		if iowsidx >= len(iows) {
			iowsidx = 0
		}
	}
	iowsLock.Unlock()
	return ret
}
func startIOLoops() {
	for i := 0; i < *parallel; i++ {
		go startIOLoop()
	}
}
func main() {
	flag.Parse()
	if !*debug {
		logger = log.New(io.Discard, "", 0)
	}
	go http.ListenAndServe(fmt.Sprintf(":%d", *pport), nil)
	startIOLoops()
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
