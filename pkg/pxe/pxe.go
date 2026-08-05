package pxe

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	tftpOACK = 6

	tftpBlockSize = 512
	tftpRetries   = 12
	tftpTimeout   = 3 * time.Second
)

// transfer tracks one TFTP transfer. Every RRQ gets its own transfer socket
// (the server-side transfer ID, per RFC 1350) and its own transfer state, so
// concurrent or duplicate requests from the same client do not interfere.
type transfer struct {
	once      sync.Once
	mu        sync.Mutex
	client    *net.UDPAddr
	conn      *net.UDPConn
	block1    []byte
	blockSize int
	initial   chan struct{} // closed once conn+block1 are ready (or the transfer aborted)
}

func (t *transfer) setReady(conn *net.UDPConn, block1 []byte, blockSize int) {
	t.once.Do(func() {
		t.mu.Lock()
		t.conn = conn
		t.block1 = block1
		t.blockSize = blockSize
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
		return net.ListenUDP("udp4", addr)
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
			filename, mode, opts := parseRRQ(pkt)
			filename = sanitizeTFTPName(filename)
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
			if len(opts) > 0 {
				log.Printf("pxe: received RRQ for %s from %s mode=%s opts=%v", filename, client, mode, opts)
			} else {
				log.Printf("pxe: received RRQ for %s from %s", filename, client)
			}
			go s.serveFile(conn, root, key, st, filename, opts)
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
func (s *Server) serveFile(control *net.UDPConn, root, key string, st *transfer, filename string, opts map[string]string) {
	defer s.active.Delete(key)
	defer st.abort()

	rootReal, rel, err := s.resolvePath(root, filename)
	if err != nil {
		log.Printf("pxe: %s: %v", filename, err)
		sendERR(control, st.client, 1, "File not found")
		return
	}

	f, err := openTFTPFile(rootReal, rel)
	if err != nil {
		log.Printf("pxe: cannot open %s: %v", filename, err)
		sendERR(control, st.client, 1, "File not found")
		return
	}
	defer f.Close()

	// Determine file size for tsize option negotiation.
	fi, err := f.Stat()
	if err != nil {
		log.Printf("pxe: cannot stat %s: %v", filename, err)
		sendERR(control, st.client, 1, "Server error")
		return
	}
	fileSize := fi.Size()

	// Negotiate RFC 2347 options. The most common request from UEFI PXE
	// firmware is blksize=1468; we honor it up to the Ethernet MTU payload.
	blockSize := tftpBlockSize
	if bs, ok := opts["blksize"]; ok {
		if req, err := strconv.Atoi(bs); err == nil && req > 0 {
			const maxBlockSize = 1468
			if req > maxBlockSize {
				req = maxBlockSize
			}
			blockSize = req
		}
	}

	oack := buildOACK(opts, blockSize, fileSize)

	// Each transfer gets its own UDP socket. DATA is sent from this socket and
	// ACKs are only accepted from the full client IP:port, which is how TFTP
	// transfer IDs work and what isolates concurrent transfers from each other.
	conn, err := s.listenTransfer(st.client)
	if err != nil {
		log.Printf("pxe: transfer socket for %s: %v", filename, err)
		sendERR(control, st.client, 1, "Server error")
		return
	}
	defer conn.Close()

	// If the client requested options, send OACK and wait for the ACK of
	// block 0 before starting the data transfer.
	if len(oack) > 0 {
		if err := sendOACK(conn, st, oack); err != nil {
			log.Printf("pxe: OACK error for %s: %v", filename, err)
			return
		}
	}

	log.Printf("pxe: sending %s (requested %s) via %s blocksize=%d", rel, filename, conn.LocalAddr(), blockSize)

	block := uint16(1)
	data := make([]byte, 4+blockSize)
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
			st.setReady(conn, block1, blockSize)
		}

		if err := sendBlock(conn, st, payload, block); err != nil {
			log.Printf("pxe: transfer error for %s (block %d): %v", filename, block, err)
			return
		}

		if n < blockSize {
			log.Printf("pxe: completed %s (%d blocks)", rel, int(block))
			return
		}
		block++
	}
}

// buildOACK constructs an RFC 2347 option-acknowledgment packet. It echoes
// back negotiated values for blksize and tsize if the client requested them.
// Returns nil when no options need to be acknowledged.
func buildOACK(opts map[string]string, blockSize int, fileSize int64) []byte {
	if len(opts) == 0 {
		return nil
	}

	var pairs []string
	if _, ok := opts["blksize"]; ok {
		pairs = append(pairs, "blksize", strconv.Itoa(blockSize))
	}
	if _, ok := opts["tsize"]; ok {
		pairs = append(pairs, "tsize", strconv.FormatInt(fileSize, 10))
	}
	if len(pairs) == 0 {
		return nil
	}

	size := 2
	for _, s := range pairs {
		size += len(s) + 1
	}
	pkt := make([]byte, 0, size)
	pkt = append(pkt, 0, tftpOACK)
	for _, s := range pairs {
		pkt = append(pkt, s...)
		pkt = append(pkt, 0)
	}
	return pkt
}

// sendOACK transmits the OACK packet and waits for the client to ACK block 0,
// confirming acceptance of the negotiated options.
func sendOACK(conn *net.UDPConn, st *transfer, oack []byte) error {
	for i := 0; i < tftpRetries; i++ {
		if _, err := conn.WriteToUDP(oack, st.client); err != nil {
			return err
		}

		if err := conn.SetReadDeadline(time.Now().Add(tftpTimeout)); err != nil {
			return err
		}
		ack, err := waitForAck(conn, st.client, 0)
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

func (s *Server) listenTransfer(client *net.UDPAddr) (*net.UDPConn, error) {
	s.mu.RLock()
	iface := s.cfg.BindInterface
	s.mu.RUnlock()

	addr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	if iface != "" {
		// Bind to the interface's primary IPv4 address so the TFTP DATA
		// packets leave with a source IP that matches the DHCP option 66
		// TFTP server address. Some PXE clients ignore replies that do not
		// originate from the expected server IP.
		if ip, err := ifaceIPv4(iface); err != nil {
			log.Printf("pxe: bind interface %s has no IPv4 address, falling back to 0.0.0.0: %v", iface, err)
		} else {
			addr.IP = ip
		}
	}
	return listenUDPOn(addr, iface)
}

// ifaceIPv4 returns the first IPv4 address assigned to the named interface.
func ifaceIPv4(name string) (net.IP, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return ip4, nil
			}
		}
	}
	return nil, fmt.Errorf("no IPv4 address on %s", name)
}

// sendBlock transmits one DATA block and waits for the matching ACK,
// retransmitting on timeout. RFC 1350 requires clients to ACK the final
// (short) block too, so the same retry logic is applied to it: losing the
// last block silently terminated the transfer otherwise.
func sendBlock(conn *net.UDPConn, st *transfer, data []byte, block uint16) error {
	for i := 0; i < tftpRetries; i++ {
		if _, err := conn.WriteToUDP(data, st.client); err != nil {
			return err
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

// parseRRQ extracts the filename, mode and any RFC 2347 options from a TFTP
// read-request packet. Options are returned as a map for diagnostics; the
// server currently does not negotiate them.
func parseRRQ(pkt []byte) (filename, mode string, opts map[string]string) {
	filename, rest := cstring(pkt[2:])
	mode, rest = cstring(rest)
	opts = make(map[string]string)
	for len(rest) > 0 {
		key, r := cstring(rest)
		val, r2 := cstring(r)
		if key == "" {
			break
		}
		opts[strings.ToLower(key)] = val
		rest = r2
	}
	return filename, mode, opts
}

func cstring(b []byte) (string, []byte) {
	for i, v := range b {
		if v == 0 {
			return string(b[:i]), b[i+1:]
		}
	}
	return "", nil
}

// sanitizeTFTPName strips trailing non-printable bytes from a requested TFTP
// filename. Some UEFI PXE firmware implementations (Intel UNDI PXE-2.1 era,
// Realtek NICs) misparse DHCP option 67 as a NUL-terminated string and append
// junk bytes -- typically 0xff -- to the TFTP RRQ filename. This mirrors the
// tftpd-hpa remap workaround ("r (.*)[^0-9A-Za-z._-]$ \1").
func sanitizeTFTPName(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] >= 0x20 && name[i] < 0x7f {
			return name[:i+1]
		}
	}
	return ""
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

// resolvePath turns a TFTP request filename into the real root directory and a
// cleaned relative path inside it. The TFTP root is a strict jail: absolute
// paths, parent-directory traversal and symlinks escaping the root are
// rejected, for the requested filename as well as for names resolved through
// macImages/DefaultImage.
func (s *Server) resolvePath(root, filename string) (rootReal, rel string, err error) {
	img := s.resolveImage(filename)
	if filepath.IsAbs(img) {
		return "", "", fmt.Errorf("absolute path rejected: %q", img)
	}

	full := filepath.Join(root, img)
	if !pathWithinRoot(root, full) {
		return "", "", fmt.Errorf("path escapes TFTP root: %q", img)
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", "", fmt.Errorf("open %q: %w", img, err)
	}
	if !pathWithinRoot(realRoot, realFull) {
		return "", "", fmt.Errorf("symlink escapes TFTP root: %q", img)
	}
	rel, err = filepath.Rel(realRoot, realFull)
	if err != nil {
		return "", "", fmt.Errorf("rel %q: %w", img, err)
	}
	return realRoot, rel, nil
}

// openTFTPFile opens rel inside rootDir component-by-component using openat
// with O_NOFOLLOW on every component. Opening relative to the already-resolved
// root descriptor makes the jail immune to a post-validation symlink swap
// (TOCTOU): any path component that becomes a symlink after validation is
// rejected at open time instead of being followed outside the root.
func openTFTPFile(rootDir, rel string) (*os.File, error) {
	dirfd, err := unix.Open(rootDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			unix.Close(dirfd)
			return nil, fmt.Errorf("invalid path component %q", part)
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(dirfd, part, flags, 0)
		if err != nil {
			unix.Close(dirfd)
			return nil, err
		}
		unix.Close(dirfd)
		dirfd = fd
		if i == len(parts)-1 {
			return os.NewFile(uintptr(fd), part), nil
		}
	}
	unix.Close(dirfd)
	return nil, fmt.Errorf("empty path")
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
