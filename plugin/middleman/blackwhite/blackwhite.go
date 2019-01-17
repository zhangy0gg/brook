package blackwhite

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/txthinking/socks5"
	"github.com/txthinking/x"
)

var Dial x.Dialer = x.DefaultDial

// BlackWhite is a middleman
type BlackWhite struct {
	Mode         string // mode is white or black
	Domains      map[string]byte
	Nets         []*net.IPNet
	Timeout      int
	Deadline     int
	Socks5Handle *socks5.DefaultHandle
	BlackDNS     string
	WhiteDNS     string
}

// NewBlackWhite returns a BlackWhite
func NewBlackWhite(mode, domainURL, cidrURL, blackDNS, whiteDNS string, timeout, deadline int) (*BlackWhite, error) {
	ds := make(map[string]byte)
	ns := make([]*net.IPNet, 0)
	if domainURL != "" {
		data, err := readData(domainURL)
		if err != nil {
			return nil, err
		}
		data = bytes.TrimSpace(data)
		data = bytes.Replace(data, []byte{0x20}, []byte{}, -1)
		data = bytes.Replace(data, []byte{0x0d, 0x0a}, []byte{0x0a}, -1)
		ss := strings.Split(string(data), "\n")
		for _, v := range ss {
			ds[v] = 0
		}
	}
	if cidrURL != "" {
		data, err := readData(cidrURL)
		if err != nil {
			return nil, err
		}
		data = bytes.TrimSpace(data)
		data = bytes.Replace(data, []byte{0x20}, []byte{}, -1)
		data = bytes.Replace(data, []byte{0x0d, 0x0a}, []byte{0x0a}, -1)
		ss := strings.Split(string(data), "\n")
		ns = make([]*net.IPNet, 0, len(ss))
		for _, v := range ss {
			_, in, err := net.ParseCIDR(v)
			if err != nil {
				return nil, err
			}
			ns = append(ns, in)
		}
	}
	return &BlackWhite{
		Mode:         mode,
		Domains:      ds,
		Nets:         ns,
		Timeout:      timeout,
		Deadline:     deadline,
		Socks5Handle: &socks5.DefaultHandle{},
		WhiteDNS:     whiteDNS,
		BlackDNS:     blackDNS,
	}, nil
}

// Has domain or IP
func (b *BlackWhite) Has(host string) bool {
	ip := net.ParseIP(host)
	if ip != nil {
		for _, v := range b.Nets {
			if v.Contains(ip) {
				return true
			}
		}
		return false
	}
	ss := strings.Split(host, ".")
	var s string
	for i := len(ss) - 1; i >= 0; i-- {
		if s == "" {
			s = ss[i]
		} else {
			s = ss[i] + "." + s
		}
		if _, ok := b.Domains[s]; ok {
			return true
		}
	}
	return false
}

// TCPHandle handles tcp request
func (b *BlackWhite) TCPHandle(s *socks5.Server, c *net.TCPConn, r *socks5.Request) (bool, error) {
	if r.Cmd == socks5.CmdConnect {
		h, _, err := net.SplitHostPort(r.Address())
		if err != nil {
			return false, err
		}
		if b.Mode == "white" && !b.Has(h) {
			return false, nil
		}
		if b.Mode == "black" && b.Has(h) {
			return false, nil
		}
		if err := b.Socks5Handle.TCPHandle(s, c, r); err != nil {
			return true, err
		}
		return true, nil
	}
	if r.Cmd == socks5.CmdUDP {
		return false, nil
	}
	return false, socks5.ErrUnsupportCmd
}

// UDPHandle handles udp packet
func (b *BlackWhite) UDPHandle(s *socks5.Server, ca *net.UDPAddr, d *socks5.Datagram) (bool, error) {
	if d.Address() == b.BlackDNS {
		done, err := b.DNSHandle(s, ca, d)
		if err != nil || done {
			return done, err
		}
	}
	h, _, err := net.SplitHostPort(d.Address())
	if err != nil {
		return false, err
	}
	if b.Mode == "white" && !b.Has(h) {
		return false, nil
	}
	if b.Mode == "black" && b.Has(h) {
		return false, nil
	}
	if err := b.Socks5Handle.UDPHandle(s, ca, d); err != nil {
		return true, err
	}
	return true, nil
}

// DNSHandle handles DNS query
func (b *BlackWhite) DNSHandle(s *socks5.Server, addr *net.UDPAddr, d *socks5.Datagram) (bool, error) {
	bye := func() {
		v, ok := s.TCPUDPAssociate.Get(addr.String())
		if ok {
			ch := v.(chan byte)
			ch <- 0x00
			s.TCPUDPAssociate.Delete(addr.String())
		}
	}
	m := &dns.Msg{}
	if err := m.Unpack(d.Data); err != nil {
		bye()
		return true, err
	}
	white := false
	for _, v := range m.Question {
		if len(v.Name) > 0 && b.Mode == "white" && b.Has(v.Name[0:len(v.Name)-1]) {
			white = true
			break
		}
		if len(v.Name) > 0 && b.Mode == "black" && !b.Has(v.Name[0:len(v.Name)-1]) {
			white = true
			break
		}
	}
	if !white {
		return false, nil
	}

	conn, err := Dial.Dial("udp", b.WhiteDNS)
	if err != nil {
		bye()
		return true, err
	}
	defer conn.Close()
	co := &dns.Conn{Conn: conn}
	if err := co.WriteMsg(m); err != nil {
		bye()
		return true, err
	}
	m1, err := co.ReadMsg()
	if err != nil {
		bye()
		return true, err
	}
	if m1.MsgHdr.Truncated {
		conn, err := Dial.Dial("tcp", b.WhiteDNS)
		if err != nil {
			bye()
			return true, err
		}
		defer conn.Close()
		co := &dns.Conn{Conn: conn}
		if err := co.WriteMsg(m); err != nil {
			bye()
			return true, err
		}
		m1, err = co.ReadMsg()
		if err != nil {
			bye()
			return true, err
		}
	}
	m1b, err := m1.Pack()
	if err != nil {
		bye()
		return true, err
	}

	a, ad, port, err := socks5.ParseAddress(addr.String())
	if err != nil {
		bye()
		return true, err
	}
	d = socks5.NewDatagram(a, ad, port, m1b)
	if _, err := s.UDPConn.WriteToUDP(d.Bytes(), addr); err != nil {
		bye()
		return true, err
	}
	bye()
	return true, nil
}

// Handle handles http proxy request, if the domain is in the white list
func (b *BlackWhite) Handle(method, addr string, request []byte, conn *net.TCPConn) (handled bool, err error) {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	if b.Mode == "white" && !b.Has(h) {
		return false, nil
	}
	if b.Mode == "black" && b.Has(h) {
		return false, nil
	}

	tmp, err := Dial.Dial("tcp", addr)
	if err != nil {
		return true, err
	}
	rc := tmp.(*net.TCPConn)
	defer rc.Close()
	if b.Timeout != 0 {
		if err := rc.SetKeepAlivePeriod(time.Duration(b.Timeout) * time.Second); err != nil {
			return true, err
		}
	}
	if b.Deadline != 0 {
		if err := rc.SetDeadline(time.Now().Add(time.Duration(b.Deadline) * time.Second)); err != nil {
			return true, err
		}
	}
	if method == "CONNECT" {
		_, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		if err != nil {
			return true, err
		}
	}
	if method != "CONNECT" {
		if _, err := rc.Write(request); err != nil {
			return true, err
		}
	}
	// TODO
	go func() {
		_, _ = io.Copy(rc, conn)
	}()
	_, _ = io.Copy(conn, rc)
	return true, nil
}

func readData(url string) ([]byte, error) {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		c := &http.Client{
			Timeout: 9 * time.Second,
		}
		r, err := c.Get(url)
		if err != nil {
			return nil, err
		}
		defer r.Body.Close()
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	if strings.HasPrefix(url, "file://") {
		data, err := ioutil.ReadFile(url)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, errors.New("Unsupport URL")
}
