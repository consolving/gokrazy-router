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

// transfer tracks one TFTP transfer. Every RRQ gets its own transfer socket
// (the server-side transfer ID, per RFC 1350) and its own transfer state, so
// concurrent or duplicate requests from the same client do not interfere.
type transfer struct {
	once    sync.Once
	mu      sync.Mutex
	client  *net.UDPAddr
	conn    *net.UDPConn
	block1  []byte
	initial chan struct{} // closed once conn+block1 are ready (or the transfer aborted)
}

func (t *transfer) setReady(conn *net.UDPConn, block1 []byte) {
	t.once.Do(func() {
		t.mu.Lock()
		t.conn = conn
		t.block1 = block1
		t.mu.Unlock()
		close(t.initial)
	})
}

// abort releases waiters on initial without establishing a transfer socket.
// Only relevant when the transfer cannot be started at all.
func (t *transfer) abort() {
	t.once.Do(func() {
		close(t.initial)
	})
}

// resendBlock1 re-sends the first DATA block for a duplicate RRQ. TFTP clients
// retransmit the RRQ when their first DATA reply was lost, so the server must
// answer with the first block again instead of starting a second transfer.
func (t *transfer) resendBlock1() {
	select {
	case <-t.initial:
	case <-time.After(tftpTimeout):
		return
	}
	t.mu.Lock()
	conn := t.conn
	block1 := t.block1
	client := t.client
	t.mu.Unlock()
	if conn != nil && block1 != nil {
		if _, err := conn.WriteToUDP(block1, client); err != nil {
			log.Printf("pxe: resend block 1: %v", err)
		}
	}
}

type Server struct {
	mu      sync.RWMutex
	cfg     config.PXEConfig
	handler map[string]string
	active  sync.Map // key: "clientIP:port|filename" -> *transfer
	control *net.UDPConn
}

func New(cfg config.PXEConfig) *Server {
	return &Server{
		cfg:     cfg,
		handler: make(map[string]string),
	}
}

func (s *Server) SetDefaultImage(img string) {
	s.mu.Lock()
	s.cfg.DefaultImage = img
	s.mu.Unlock()
}

func (s *Server) SetMacImages(m map[string]string) {
	replacement := make(map[string]string, len(m))
	for mac, img := range m {
		replacement[normalizeMAC(mac)] = img
	}
	s.mu.Lock()
	s.handler = replacement
	s.mu.Unlock()
}

// Addr returns the control socket address (useful for tests).
func (s *Server) Addr() net.Addr {
	if s.control == nil {
		return nil
	}
	return s.control.LocalAddr()
}

func (s *Server) Start() error {
	s.mu.Lock()
	cfg := s.cfg
	s.handler = make(map[string]string)
	for mac, img := range cfg.MacImages {
		s.handler[normalizeMAC(mac)] = img
	}
	s.mu.Unlock()

	if !cfg.Enabled {
		return nil
	}
	root := cfg.TFTPRoot
	if root == "" {
		root = "/tmp/tftpboot"
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return fmt.Errorf("pxe: mkdir %s: %w", root, err)
	}

	listen := cfg.Listen
	if listen == "" {
		listen = "0.0.0.0:69"
	}

	addr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return fmt.Errorf("pxe: resolve %s: %w", listen, err)
	}
	conn, err := listenUDPOn(addr, cfg.BindInterface)
	if err != nil {
		return fmt.Errorf("pxe: listen %s: %w", listen, err)
	}
	s.control = conn
	if cfg.BindInterface != "" {
		log.Printf("pxe: tftp server listening on %s (bound to %s), root %s", listen, cfg.BindInterface, root)
	} else {
		log.Printf("pxe: tftp server listening on %s, root %s", listen, root)
	}

	go s.reader(conn, root)
	return nil
}

// listenUDPOn creates a UDP socket on addr, optionally bound to iface with
// SO_BINDTODEVICE. A nil/zero IP binds all local addresses.
func listenUDPOn(addr *net.UDPAddr, iface string) (*net.UDPConn, error) {
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
	if ip4 := addr.IP.To4(); ip4 != nil {
		copy(sa.Addr[:], ip4)
	}
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
			filename, _ := cstring(pkt[2:])
			if filename == "" {
				continue
			}
			// Deduplicate in the single-threaded reader so concurrent RRQs for
			// the same client/file cannot start two transfer sockets.
			key := client.String() + "|" + filename
			st := &transfer{client: dupAddr(client), initial: make(chan struct{})}
			if existing, loaded := s.active.LoadOrStore(key, st); loaded {
				log.Printf("pxe: duplicate RRQ for %s from %s; resending first block", filename, client)
				existing.(*transfer).resendBlock1()
				continue
			}
			log.Printf("pxe: received RRQ for %s from %s", filename, client)
			go s.serveFile(conn, root, key, st, filename)
		case tftpACK:
			// ACKs are sent to the per-transfer socket, never to the control
			// socket. A stray ACK here belongs to no transfer; drop it.
		default:
			log.Printf("pxe: received op %d from %s len=%d", binary.BigEndian.Uint16(pkt[:2]), client, len(pkt))
		}
	}
}

// serveFile resolves the request, opens the file inside the TFTP root and
// streams it to the client from a dedicated transfer socket.
func (s *Server) serveFile(control *net.UDPConn, root, key string, st *transfer, filename string) {
	defer s.active.Delete(key)
	defer st.abort()

	img, err := s.resolvePath(root, filename)
	if err != nil {
		log.Printf("pxe: %s: %v", filename, err)
		sendERR(control, st.client, 1, "File not found")
		return
	}

	f, err := os.Open(img)
	if err != nil {
		log.Printf("pxe: cannot open %s: %v", img, err)
		sendERR(control, st.client, 1, "File not found")
		return
	}
	defer f.Close()

	// Each transfer gets its own UDP socket. DATA is sent from this socket and
	// ACKs are only accepted from the full client IP:port, which is how TFTP
	// transfer IDs work and what isolates concurrent transfers from each other.
	conn, err := s.listenTransfer(st.client)
	if err != nil {
		log.Printf("pxe: transfer socket for %s: %v", img, err)
		sendERR(control, st.client, 1, "Server error")
		return
	}
	defer conn.Close()

	log.Printf("pxe: sending %s (requested %s) via %s", img, filename, conn.LocalAddr())

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
		if block == 1 {
			block1 := make([]byte, len(payload))
			copy(block1, payload)
			st.setReady(conn, block1)
		}

		if err := sendBlock(conn, st, payload, block); err != nil {
			log.Printf("pxe: transfer error for %s (block %d): %v", filename, block, err)
			return
		}

		if n < tftpBlockSize {
			log.Printf("pxe: completed %s (%d blocks)", img, int(block))
			return
		}
		block++
	}
}

func (s *Server) listenTransfer(client *net.UDPAddr) (*net.UDPConn, error) {
	s.mu.RLock()
	iface := s.cfg.BindInterface
	s.mu.RUnlock()
	return listenUDPOn(&net.UDPAddr{IP: net.IPv4zero, Port: 0}, iface)
}

// sendBlock transmits one DATA block and waits for the matching ACK,
// retransmitting on timeout. The final (short) block is sent without waiting
// for an ACK, as permitted by RFC 1350.
func sendBlock(conn *net.UDPConn, st *transfer, data []byte, block uint16) error {
	last := len(data) < 4+tftpBlockSize
	for i := 0; i < tftpRetries; i++ {
		if _, err := conn.WriteToUDP(data, st.client); err != nil {
			return err
		}
		if last {
			return nil
		}

		if err := conn.SetReadDeadline(time.Now().Add(tftpTimeout)); err != nil {
			return err
		}
		ack, err := waitForAck(conn, st.client, block)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		if ack {
			return nil
		}
	}
	return fmt.Errorf("timeout after %d retries", tftpRetries)
}

// waitForAck reads packets until an ACK for the expected block arrives from
// the expected client. Packets from other sources, non-ACK packets and stale
// ACKs for earlier blocks are ignored.
func waitForAck(conn *net.UDPConn, client *net.UDPAddr, block uint16) (bool, error) {
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return false, err
		}
		if !sameAddr(from, client) {
			continue
		}
		if n < 4 || binary.BigEndian.Uint16(buf[:2]) != tftpACK {
			continue
		}
		if binary.BigEndian.Uint16(buf[2:4]) == block {
			return true, nil
		}
	}
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

// resolveImage maps a requested TFTP filename to a serving image. Requests
// with a MAC-address filename are matched against macImages and fall back to
// DefaultImage; everything else is served under its requested name.
func (s *Server) resolveImage(filename string) string {
	mac := filenameToMAC(filename)
	if mac == "" {
		return filename
	}
	s.mu.RLock()
	img, ok := s.handler[normalizeMAC(mac)]
	if !ok && s.cfg.DefaultImage != "" {
		img = s.cfg.DefaultImage
	}
	s.mu.RUnlock()
	if img != "" {
		return img
	}
	return filename
}

// resolvePath turns a TFTP request filename into a path inside root. The TFTP
// root is a strict jail: absolute paths, parent-directory traversal and
// symlinks escaping the root are rejected, for the requested filename as well
// as for names resolved through macImages/DefaultImage.
func (s *Server) resolvePath(root, filename string) (string, error) {
	img := s.resolveImage(filename)
	if filepath.IsAbs(img) {
		return "", fmt.Errorf("absolute path rejected: %q", img)
	}

	full := filepath.Join(root, img)
	if !pathWithinRoot(root, full) {
		return "", fmt.Errorf("path escapes TFTP root: %q", img)
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", img, err)
	}
	if !pathWithinRoot(realRoot, realFull) {
		return "", fmt.Errorf("symlink escapes TFTP root: %q", img)
	}
	return realFull, nil
}

func pathWithinRoot(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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

func dupAddr(a *net.UDPAddr) *net.UDPAddr {
	if a == nil {
		return nil
	}
	dup := &net.UDPAddr{Port: a.Port, Zone: a.Zone}
	if a.IP != nil {
		dup.IP = append(net.IP(nil), a.IP...)
	}
	return dup
}

func sameAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Port == b.Port && a.IP != nil && b.IP != nil && a.IP.Equal(b.IP)
}
