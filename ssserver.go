package brook

import (
	"crypto/aes"
	"crypto/rand"
	"errors"
	"io"
	"log"
	"net"
	"time"

	cache "github.com/patrickmn/go-cache"
	"github.com/txthinking/socks5"
	"github.com/txthinking/x"
)

// SSServer
type SSServer struct {
	Password     []byte
	TCPAddr      *net.TCPAddr
	UDPAddr      *net.UDPAddr
	TCPListen    *net.TCPListener
	UDPConn      *net.UDPConn
	UDPExchanges *cache.Cache
	TCPDeadline  int
	TCPTimeout   int
	UDPDeadline  int
}

// NewSSServer
func NewSSServer(addr, password string, tcpTimeout, tcpDeadline, udpDeadline int) (*SSServer, error) {
	taddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	cs := cache.New(cache.NoExpiration, cache.NoExpiration)
	s := &SSServer{
		Password:     MakeSSKey(password),
		TCPAddr:      taddr,
		UDPAddr:      uaddr,
		UDPExchanges: cs,
		TCPTimeout:   tcpTimeout,
		TCPDeadline:  tcpDeadline,
		UDPDeadline:  udpDeadline,
	}
	return s, nil
}

// ListenAndServe server
func (s *SSServer) ListenAndServe() error {
	errch := make(chan error)
	go func() {
		errch <- s.RunTCPServer()
	}()
	go func() {
		errch <- s.RunUDPServer()
	}()
	return <-errch
}

// RunTCPServer starts tcp server
func (s *SSServer) RunTCPServer() error {
	var err error
	s.TCPListen, err = net.ListenTCP("tcp", s.TCPAddr)
	if err != nil {
		return err
	}
	defer s.TCPListen.Close()
	for {
		c, err := s.TCPListen.AcceptTCP()
		if err != nil {
			return err
		}
		go func(c *net.TCPConn) {
			defer c.Close()
			if s.TCPTimeout != 0 {
				if err := c.SetKeepAlivePeriod(time.Duration(s.TCPTimeout) * time.Second); err != nil {
					log.Println(err)
					return
				}
			}
			if s.TCPDeadline != 0 {
				if err := c.SetDeadline(time.Now().Add(time.Duration(s.TCPDeadline) * time.Second)); err != nil {
					log.Println(err)
					return
				}
			}
			if err := s.TCPHandle(c); err != nil {
				log.Println(err)
			}
		}(c)
	}
	return nil
}

// RunUDPServer starts udp server
func (s *SSServer) RunUDPServer() error {
	var err error
	s.UDPConn, err = net.ListenUDP("udp", s.UDPAddr)
	if err != nil {
		return err
	}
	defer s.UDPConn.Close()
	for {
		b := make([]byte, 65536)
		n, addr, err := s.UDPConn.ReadFromUDP(b)
		if err != nil {
			return err
		}
		go func(addr *net.UDPAddr, b []byte) {
			if err := s.UDPHandle(addr, b); err != nil {
				log.Println(err)
				return
			}
		}(addr, b[0:n])
	}
	return nil
}

// TCPHandle handle request
func (s *SSServer) TCPHandle(c *net.TCPConn) error {
	cc, err := s.WrapCipherConn(c)
	if err != nil {
		return err
	}
	bb := make([]byte, 1)
	if _, err := io.ReadFull(cc, bb); err != nil {
		return err
	}
	var addr []byte
	if bb[0] == socks5.ATYPIPv4 {
		addr = make([]byte, 4)
		if _, err := io.ReadFull(cc, addr); err != nil {
			return err
		}
	} else if bb[0] == socks5.ATYPIPv6 {
		addr = make([]byte, 16)
		if _, err := io.ReadFull(cc, addr); err != nil {
			return err
		}
	} else if bb[0] == socks5.ATYPDomain {
		dal := make([]byte, 1)
		if _, err := io.ReadFull(cc, dal); err != nil {
			return err
		}
		if dal[0] == 0 {
			return err
		}
		addr = make([]byte, int(dal[0]))
		if _, err := io.ReadFull(cc, addr); err != nil {
			return err
		}
		addr = append(dal, addr...)
	} else {
		return errors.New("Unknown address type")
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(cc, port); err != nil {
		return err
	}
	address := socks5.ToAddress(bb[0], addr, port)

	tmp, err := Dial.Dial("tcp", address)
	if err != nil {
		return err
	}
	rc := tmp.(*net.TCPConn)
	defer rc.Close()
	if s.TCPTimeout != 0 {
		if err := rc.SetKeepAlivePeriod(time.Duration(s.TCPTimeout) * time.Second); err != nil {
			return err
		}
	}
	if s.TCPDeadline != 0 {
		if err := rc.SetDeadline(time.Now().Add(time.Duration(s.TCPDeadline) * time.Second)); err != nil {
			return err
		}
	}

	// TODO
	go func() {
		iv := make([]byte, aes.BlockSize)
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			log.Println(err)
			return
		}
		if _, err := cc.Write(iv); err != nil {
			log.Println(err)
			return
		}
		_, _ = io.Copy(cc, rc)
	}()
	_, _ = io.Copy(rc, cc)
	return nil
}

// UDPHandle handle packet
func (s *SSServer) UDPHandle(addr *net.UDPAddr, b []byte) error {
	a, h, p, data, err := s.Decrypt(b)
	if err != nil {
		return err
	}
	send := func(ue *socks5.UDPExchange, data []byte) error {
		_, err := ue.RemoteConn.Write(data)
		if err != nil {
			return err
		}
		return nil
	}

	var ue *socks5.UDPExchange
	iue, ok := s.UDPExchanges.Get(addr.String())
	if ok {
		ue = iue.(*socks5.UDPExchange)
		return send(ue, data)
	}
	address := socks5.ToAddress(a, h, p)

	c, err := Dial.Dial("udp", address)
	if err != nil {
		return err
	}
	rc := c.(*net.UDPConn)
	ue = &socks5.UDPExchange{
		ClientAddr: addr,
		RemoteConn: rc,
	}
	if err := send(ue, data); err != nil {
		ue.RemoteConn.Close()
		return err
	}
	s.UDPExchanges.Set(ue.ClientAddr.String(), ue, cache.DefaultExpiration)
	go func(ue *socks5.UDPExchange) {
		defer func() {
			s.UDPExchanges.Delete(ue.ClientAddr.String())
			ue.RemoteConn.Close()
		}()
		var b [65536]byte
		for {
			if s.UDPDeadline != 0 {
				if err := ue.RemoteConn.SetDeadline(time.Now().Add(time.Duration(s.UDPDeadline) * time.Second)); err != nil {
					log.Println(err)
					break
				}
			}
			n, err := ue.RemoteConn.Read(b[:])
			if err != nil {
				log.Println(err)
				break
			}
			a, addr, port, err := socks5.ParseAddress(ue.ClientAddr.String())
			if err != nil {
				log.Println(err)
				break
			}
			cd, err := s.Encrypt(a, addr, port, b[0:n])
			if err != nil {
				log.Println(err)
				break
			}
			if _, err := s.UDPConn.WriteToUDP(cd, ue.ClientAddr); err != nil {
				log.Println(err)
				break
			}
		}
	}(ue)
	return nil
}

// WrapChiperConn make a chiper conn
func (s *SSServer) WrapCipherConn(conn net.Conn) (*CipherConn, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(conn, iv); err != nil {
		return nil, err
	}
	return NewCipherConn(conn, s.Password, iv)
}

// Encrypt data
func (s *SSServer) Encrypt(a byte, h, p, d []byte) ([]byte, error) {
	b := make([]byte, 0, 7)
	b = append(b, a)
	b = append(b, h...)
	b = append(b, p...)
	b = append(b, d...)
	return x.AESCFBEncrypt(b, s.Password)
}

// Decrypt data
func (s *SSServer) Decrypt(cd []byte) (a byte, addr, port, data []byte, err error) {
	var bb []byte
	bb, err = x.AESCFBDecrypt(cd, s.Password)
	if err != nil {
		return
	}
	err = errors.New("Data length error")
	n := len(bb)
	minl := 1
	if n < minl {
		return
	}
	if bb[0] == socks5.ATYPIPv4 {
		minl += 4
		if n < minl {
			return
		}
		addr = bb[minl-4 : minl]
	} else if bb[0] == socks5.ATYPIPv6 {
		minl += 16
		if n < minl {
			return
		}
		addr = bb[minl-16 : minl]
	} else if bb[0] == socks5.ATYPDomain {
		minl += 1
		if n < minl {
			return
		}
		l := bb[1]
		if l == 0 {
			return
		}
		minl += int(l)
		if n < minl {
			return
		}
		addr = bb[minl-int(l) : minl]
		addr = append([]byte{l}, addr...)
	} else {
		return
	}
	minl += 2
	if n <= minl {
		return
	}
	a = bb[0]
	port = bb[minl-2 : minl]
	data = bb[minl:]
	err = nil
	return
}

// Shutdown server
func (s *SSServer) Shutdown() error {
	var err, err1 error
	if s.TCPListen != nil {
		err = s.TCPListen.Close()
	}
	if s.UDPConn != nil {
		err1 = s.UDPConn.Close()
	}
	if err != nil {
		return err
	}
	return err1
}
