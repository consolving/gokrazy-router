package pxe

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
	"golang.org/x/sys/unix"
)

const (
	tftpRRQ  = 1
	tftpDATA = 3
	tftpACK  = 4
	tftpERR  = 5

	tftpBlockSize = 512
	tftpRetries   = 12
	tftpTimeout   = 3 * time.Second
)

type transferState struct {
	mu     sync.Mutex
	client *net.UDPAddr
}

func (st *transferState) updateClient(client *net.UDPAddr) {
	st.mu.Lock()
	st.client = client
	st.mu.Unlock()
}

func (st *transferState) getClient() *net.UDPAddr {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.client
}

type Server struct {
	cfg     config.PXEConfig
	handler map[string]string
	acks    sync.Map
	active  sync.Map
}

func New(cfg config.PXEConfig) *Server {
	return &Server{
		cfg:     cfg,
		handler: make(map[string]string),
	}
}

func (s *Server) SetDefaultImage(img string) {
	s.cfg.DefaultImage = img
}

func (s *Server) SetMacImages(m map[string]string) {
	s.handler = make(map[string]string)
	for mac, img := range m {
		s.handler[normalizeMAC(mac)] = img
	}
}

func (s *Server) Start() error {
	if !s.cfg.Enabled {
		return nil
	}
	root := s.cfg.TFTPRoot
	if root == "" {
		root = "/tmp/tftpboot"
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return fmt.Errorf("pxe: mkdir %s: %w", root, err)
	}

	for mac, img := range s.cfg.MacImages {
		s.handler[normalizeMAC(mac)] = img
	}

	listen := s.cfg.Listen
	if listen == "" {
		listen = "0.0.0.0:69"
	}

	addr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return fmt.Errorf("pxe: resolve %s: %w", listen, err)
	}
	conn, err := s.listenUDP(addr, s.cfg.BindInterface)
	if err != nil {
		return fmt.Errorf("pxe: listen %s: %w", listen, err)
	}
	if s.cfg.BindInterface != "" {
		log.Printf("pxe: tftp server listening on %s (bound to %s), root %s", listen, s.cfg.BindInterface, root)
	} else {
		log.Printf("pxe: tftp server listening on %s, root %s", listen, root)
	}

	go s.reader(conn, root)
	return nil
}

func (s *Server) listenUDP(addr *net.UDPAddr, iface string) (*net.UDPConn, error) {
	if iface == "" {
		return net.ListenUDP("udp", addr)
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	f := os.NewFile(uintptr(fd), "pxe-"+iface)
	defer f.Close()

	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return nil, fmt.Errorf("SO_REUSEADDR: %w", err)
	}
	if err := unix.BindToDevice(fd, iface); err != nil {
		return nil, fmt.Errorf("SO_BINDTODEVICE(%s): %w", iface, err)
	}

	sa := &unix.SockaddrInet4{Port: addr.Port}
	copy(sa.Addr[:], addr.IP.To4())
	if err := unix.Bind(fd, sa); err != nil {
		return nil, fmt.Errorf("bind %s:%d: %w", addr.IP, addr.Port, err)
	}

	conn, err := net.FilePacketConn(f)
	if err != nil {
		return nil, err
	}
	return conn.(*net.UDPConn), nil
}

func (s *Server) reader(conn *net.UDPConn, root string) {
	buf := make([]byte, 1500)
	for {
		n, client, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("pxe: read: %v", err)
			continue
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		if len(pkt) < 2 {
			continue
		}

		switch binary.BigEndian.Uint16(pkt[:2]) {
		case tftpRRQ:
			log.Printf("pxe: received RRQ from %s (op=%d)", client, binary.BigEndian.Uint16(pkt[:2]))
			go s.serveFile(conn, root, client, pkt)
		case tftpACK:
			log.Printf("pxe: received ACK from %s len=%d", client, len(pkt))
			if len(pkt) >= 4 {
				if ch, ok := s.acks.Load(client.IP.String()); ok {
					select {
					case ch.(chan []byte) <- pkt:
					default:
					}
				} else {
					log.Printf("pxe: no ack channel for %s", client.IP)
				}
			}
		default:
			log.Printf("pxe: received op %d from %s len=%d", binary.BigEndian.Uint16(pkt[:2]), client, len(pkt))
		}
	}
}

func (s *Server) serveFile(control *net.UDPConn, root string, client *net.UDPAddr, req []byte) {
	if len(req) < 2 || binary.BigEndian.Uint16(req[:2]) != tftpRRQ {
		return
	}

	filename, _ := cstring(req[2:])
	if filename == "" {
		return
	}

	dupKey := client.IP.String() + "|" + filename
	st := &transferState{
		client: client,
	}

	if existing, loaded := s.active.LoadOrStore(dupKey, st); loaded {
		existing.(*transferState).updateClient(client)
		return
	}
	defer s.active.Delete(dupKey)

	mac := filenameToMAC(filename)
	img := ""
	if mac != "" {
		img = s.handler[normalizeMAC(mac)]
	}
	if img == "" {
		img = s.cfg.DefaultImage
	}
	if img == "" {
		img = filename
	}

	if !filepath.IsAbs(img) {
		img = filepath.Join(root, img)
	}

	f, err := os.Open(img)
	if err != nil {
		log.Printf("pxe: cannot open %s: %v", img, err)
		sendERR(control, client, 1, "File not found")
		return
	}
	defer f.Close()

	ackCh := make(chan []byte, 3)
	s.acks.Store(client.IP.String(), ackCh)
	defer s.acks.Delete(client.IP.String())

	log.Printf("pxe: sending %s (requested %s)", img, filename)

	block := uint16(1)
	data := make([]byte, 4+tftpBlockSize)
	data[0] = 0
	data[1] = tftpDATA

	for {
		n, err := f.Read(data[4:])
		if err != nil && err != io.EOF {
			return
		}

		binary.BigEndian.PutUint16(data[2:4], block)
		payload := data[:4+n]

		if err := sendBlock(control, st, payload, block, ackCh); err != nil {
			log.Printf("pxe: transfer error for %s (block %d): timeout after %d retries", filename, block, tftpRetries)
			return
		}

		if n < tftpBlockSize {
			log.Printf("pxe: completed %s (%d blocks)", filename, int(block))
			return
		}
		block++
	}
}

func sendBlock(conn *net.UDPConn, st *transferState, data []byte, block uint16, ackCh chan []byte) error {
	for i := 0; i < tftpRetries; i++ {
		addr := st.getClient()
		if _, err := conn.WriteToUDP(data, addr); err != nil {
			return err
		}

		if len(data) < 4+tftpBlockSize {
			return nil
		}

		timer := time.NewTimer(tftpTimeout)
		select {
		case ack := <-ackCh:
			timer.Stop()
			if len(ack) >= 4 && binary.BigEndian.Uint16(ack[2:4]) == block {
				return nil
			}
		case <-timer.C:
		}
	}
	return fmt.Errorf("timeout after %d retries", tftpRetries)
}

func sendERR(conn *net.UDPConn, addr *net.UDPAddr, code uint16, msg string) {
	pkt := make([]byte, 4+len(msg)+1)
	binary.BigEndian.PutUint16(pkt[:2], tftpERR)
	binary.BigEndian.PutUint16(pkt[2:4], code)
	copy(pkt[4:], msg)
	conn.WriteToUDP(pkt, addr)
}

func cstring(b []byte) (string, []byte) {
	for i, v := range b {
		if v == 0 {
			return string(b[:i]), b[i+1:]
		}
	}
	return "", nil
}

func filenameToMAC(name string) string {
	base := strings.ToLower(filepath.Base(name))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.TrimPrefix(base, "01-")
	if len(base) == 17 && strings.Count(base, "-") == 5 {
		return strings.ReplaceAll(base, "-", ":")
	}
	return ""
}

func normalizeMAC(mac string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(mac, "-", ":")))
}
