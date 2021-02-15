 package main

import (
	"fmt"
	"os"
	"time"
	"net"

	kcp "github.com/xtaci/kcp-go"
	"github.com/xtaci/smux"
)

// wrapper for StartServerTCP(), StartServerKCP(), and StartServerUDP()
func StartServer(conn net.Conn, conf Config) {
	switch conf.getMode() {
	case "tcp":
		StartServerTCP(conn.(*net.TCPConn), conf.(*TCPConfig))
	case "udp":
		if conf.(*UDPConfig).Proto == "kcp" {
			StartServerKCP(conn.(*net.UDPConn), conf.(*UDPConfig))
		} else {
			StartServerUDP(conn.(*net.UDPConn), conf.(*UDPConfig))
		}
	}
}

func StartServerTCP(conn *net.TCPConn, conf *TCPConfig) {

	// Setup server side of smux
	var interval int = g_timeout/3
	interval = bound(interval, 1, 10)
	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 1
	smuxConfig.MaxReceiveBuffer = 4194304
	smuxConfig.MaxStreamBuffer = 2097152
	smuxConfig.KeepAliveInterval = time.Duration(interval) * time.Second
	smuxConfig.KeepAliveTimeout = time.Duration(g_timeout) * time.Second
	if err := smux.VerifyConfig(smuxConfig); err != nil {
		perror("smux.VerifyConfig() failed.", err)
		os.Exit(1)
	}

	session, err := smux.Server(conn, smuxConfig)
	if err != nil {
		perror("smux.Server() failed.", err)
		os.Exit(1)
	}
	fmt.Printf("tunnel created: [local]%v <--> [remote]%v\n", session.LocalAddr(), session.RemoteAddr())

	// Accept and forward
	fmt.Println("Waiting for new stream from tunnel ...")
	for {
		// accept a stream
		stream, err := session.AcceptStream()
		if err != nil {
			perror("smux.AcceptStream() failed.", err)
			break
		}

		fwd_conn, err := net.DialTCP("tcp", nil, conf.FwdAddr.(*net.TCPAddr))
		if err != nil {
			perror("net.Dial() failed.", err)
			stream.Close()
			continue
		}
		fmt.Printf("stream open(%d): tunnel --> %v\n", stream.ID(), fwd_conn.RemoteAddr())

		go stream2conn(stream, fwd_conn)
	}

	// clean up
	fmt.Printf("...\n")
	fmt.Printf("tunnel collapsed: [local]%v <--> [remote]%v\n", session.LocalAddr(), session.RemoteAddr())
	session.Close()
	conn.Close()
	time.Sleep(time.Second)
	fmt.Printf("Done\n")
}

func StartServerKCP(conn *net.UDPConn, conf *UDPConfig) {

	// setup kcp
	kconf := getKCPConfig(conf.KConf)
	fmt.Printf("%T: %v\n", kconf, kconf)
	block := getKCPBlockCipher(kconf)
	klis, err := kcp.ServeConn(block, kconf.DataShard, kconf.ParityShard, conn)
	if err != nil {
		perror("kcp.ServeConn() failed.", err)
		os.Exit(1)
	}
	if err := klis.SetDSCP(kconf.DSCP); err != nil {
		perror("klis.SetDSCP() failed.", err)
	}
	if err := klis.SetReadBuffer(kconf.SockBuf); err != nil {
		perror("klis.SetReadBuffer() failed.", err)
	}
	if err := klis.SetWriteBuffer(kconf.SockBuf); err != nil {
		perror("klis.SetWriteBuffer() failed.", err)
	}

	klis.SetDeadline(time.Now().Add(time.Duration(g_timeout) * time.Second))
	kconn, err := klis.AcceptKCP()
	if err != nil {
		perror("klis.AcceptKCP() failed.", err)
		os.Exit(1)
	}
	fmt.Println("kcp remote address:", kconn.RemoteAddr())
	kconn.SetStreamMode(true)
	kconn.SetWriteDelay(false)
	kconn.SetNoDelay(kconf.NoDelay, kconf.Interval, kconf.Resend, kconf.NoCongestion)
	kconn.SetMtu(kconf.MTU)
	kconn.SetWindowSize(kconf.SndWnd, kconf.RcvWnd)
	kconn.SetACKNoDelay(kconf.AckNodelay)

	// Setup server side of smux
	var interval int = g_timeout/3
	interval = bound(interval, 1, 10)
	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 1
	smuxConfig.MaxReceiveBuffer = 4194304
	smuxConfig.MaxStreamBuffer = 2097152
	smuxConfig.KeepAliveInterval = time.Duration(interval) * time.Second
	smuxConfig.KeepAliveTimeout = time.Duration(g_timeout) * time.Second
	if err := smux.VerifyConfig(smuxConfig); err != nil {
		perror("smux.VerifyConfig() failed.", err)
		os.Exit(1)
	}

	sess, err := smux.Server(kconn, smuxConfig)
	if err != nil {
		perror("smux.Server() failed.", err)
		os.Exit(1)
	}
	fmt.Printf("tunnel created: [local]%v <--> [remote]%v\n", sess.LocalAddr(), sess.RemoteAddr())

	// Accept and forward
	fmt.Println("Waiting for new stream from tunnel ...")
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			perror("smux.AcceptStream() failed.", err)
			break
		}

		fwd_conn, err := net.DialTCP("tcp", nil, conf.FwdAddr.(*net.TCPAddr))
		if err != nil {
			perror("net.Dial() failed.", err)
			stream.Close()
			continue
		}
		fmt.Printf("stream open(%d): tunnel --> %v\n", stream.ID(), fwd_conn.RemoteAddr())

		go stream2conn(stream, fwd_conn)
	} // AcceptStream()

	// clean up
	fmt.Printf("...\n")
	fmt.Printf("tunnel collapsed: [local]%v <--> [remote]%v\n", sess.LocalAddr(), sess.RemoteAddr())
	sess.Close()
	kconn.Close()
	conn.Close()
	time.Sleep(time.Second)
	fmt.Printf("Done\n")
}

func StartServerUDP(conn *net.UDPConn, conf *UDPConfig) {
	fmt.Printf("Connect to fwd address: %s\n", conf.FwdAddr)
	fwd_conn, err := net.DialUDP("udp", nil, conf.FwdAddr.(*net.UDPAddr))
	if err != nil {
		perror("net.DialUDP() failed.", err)
		os.Exit(1)
	}

	// recreate socket with sendto() on same endpoints
	conn.Close()
	conn, err = net.DialUDP("udp", conf.LAddr, conf.RAddr)

	// TODO: Needs udp muxing.

	// conn --> fwd_conn
	go func() {
		defer conn.Close()
		defer fwd_conn.Close()
		buf := make([]byte, 4096)
		for {
			conn.SetDeadline(time.Now().Add(time.Duration(g_timeout) * time.Second))
			n, err := conn.Read(buf)
			if err != nil {
				fmt.Println("conn.Read() failed.", err)
				return
			}
			_, err = fwd_conn.Write(buf[:n])
			if err != nil {
				fmt.Println("fwd_conn.Write() failed.", err)
				return
			}
		}
	}()

	// fwd_conn --> conn
	func() {
		defer conn.Close()
		defer fwd_conn.Close()
		buf := make([]byte, 4096)
		for {
			n, err := fwd_conn.Read(buf)
			if err != nil {
				fmt.Println("fwd_conn.Read() failed.", err)
				return
			}

			_, err = conn.Write(buf[:n])
			if err != nil {
				fmt.Println("conn.Write() failed.", err)
				return
			}
			conn.SetDeadline(time.Now().Add(time.Duration(g_timeout) * time.Second))
		}
	}()

	fmt.Println("...")
	fmt.Printf("tunnel collapsed: [local]%v <--> [remote]%v\n", conf.LocalAddr(), conf.RemoteAddr())
	time.Sleep(time.Second)
	fmt.Printf("Done\n")
}
