package pxe

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
)

func TestResolveImage(t *testing.T) {
	s := New(cfgWithDefaults())
	s.cfg.DefaultImage = "undionly.kpxe"
	s.SetMacImages(map[string]string{
		"82:78:97:41:e1:db": "netboot.xyz.img",
	})

	cases := []struct {
		req  string
		want string
	}{
		{"boot.ipxe", "boot.ipxe"},
		{"undionly.kpxe", "undionly.kpxe"},
		{"netboot.xyz.efi", "netboot.xyz.efi"},
		{"01-82-78-97-41-e1-db", "netboot.xyz.img"},
		{"01-00-11-22-33-44-55", "undionly.kpxe"},
	}
	for _, c := range cases {
		if got := s.resolveImage(c.req); got != c.want {
			t.Errorf("resolveImage(%q) = %q, want %q", c.req, got, c.want)
		}
	}
}

func TestResolveImageNoDefault(t *testing.T) {
	s := New(cfgWithDefaults())
	s.SetMacImages(map[string]string{
		"82:78:97:41:e1:db": "netboot.xyz.img",
	})

	cases := []struct {
		req  string
		want string
	}{
		{"boot.ipxe", "boot.ipxe"},
		{"01-00-11-22-33-44-55", "01-00-11-22-33-44-55"},
	}
	for _, c := range cases {
		if got := s.resolveImage(c.req); got != c.want {
			t.Errorf("resolveImage(%q) = %q, want %q", c.req, got, c.want)
		}
	}
}

func TestResolvePathJail(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"boot.img", "sub/boot.img"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	s := New(cfgWithDefaults())
	s.cfg.TFTPRoot = root
	s.SetMacImages(map[string]string{
		"82:78:97:41:e1:db": "../../../etc/passwd",
	})

	cases := []struct {
		req string
		ok  bool
	}{
		{"boot.img", true},
		{"sub/boot.img", true},
		{"boot.img/../sub/boot.img", true}, // stays inside root
		{"..", false},
		{"../boot.img", false},
		{"../../etc/passwd", false},
		{"boot.img/../../etc/passwd", false},
		{"/etc/passwd", false},          // absolute path
		{"//etc/passwd", false},         // absolute path
		{"01-82-78-97-41-e1-db", false}, // macImages escapes root
	}
	for _, c := range cases {
		_, _, err := s.resolvePath(root, c.req)
		if c.ok && err != nil {
			t.Errorf("resolvePath(%q) error = %v, want success", c.req, err)
		}
		if !c.ok && err == nil {
			t.Errorf("resolvePath(%q) succeeded, want rejection", c.req)
		}
	}
}

func TestResolvePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.bin")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "leak.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.bin"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	s := New(cfgWithDefaults())
	s.cfg.TFTPRoot = root

	if _, _, err := s.resolvePath(root, "leak.bin"); err == nil {
		t.Error("resolvePath(leak.bin) = nil error, want symlink escape rejection")
	}
	if _, _, err := s.resolvePath(root, "ok.bin"); err != nil {
		t.Errorf("resolvePath(ok.bin) error = %v, want success", err)
	}
}

// startTestServer starts a PXE server on 127.0.0.1:0 and returns it plus a
// fresh client socket and the server control address.
func startTestServer(t *testing.T, root string, macImages map[string]string) (*Server, *net.UDPConn, *net.UDPAddr) {
	t.Helper()
	s := New(config.PXEConfig{Enabled: true, Listen: "127.0.0.1:0", TFTPRoot: root})
	s.SetMacImages(macImages)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.control.Close() })

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("client socket: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return s, client, s.Addr().(*net.UDPAddr)
}

func buildRRQ(filename string) []byte {
	pkt := []byte{0, tftpRRQ}
	pkt = append(pkt, filename...)
	pkt = append(pkt, 0)
	pkt = append(pkt, "octet"...)
	pkt = append(pkt, 0)
	return pkt
}

func buildACK(block uint16) []byte {
	pkt := make([]byte, 4)
	binary.BigEndian.PutUint16(pkt[:2], tftpACK)
	binary.BigEndian.PutUint16(pkt[2:4], block)
	return pkt
}

func TestSanitizeTFTPName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"netboot.xyz.efi", "netboot.xyz.efi"},
		{"netboot.xyz.efi\xff", "netboot.xyz.efi"},
		{"netboot.xyz.efi\xff\xff\x00", "netboot.xyz.efi"},
		{"menu.ipxe", "menu.ipxe"},
		{"\xff\xfe", ""},
	}
	for _, c := range cases {
		if got := sanitizeTFTPName(c.in); got != c.want {
			t.Errorf("sanitizeTFTPName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTFTPTrailingJunkByte verifies the UEFI PXE firmware workaround: some
// clients append junk bytes (typically 0xff) to the requested filename, which
// must be ignored instead of failing with "file not found".
func TestTFTPTrailingJunkByte(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "boot.img"), []byte("pxeboot"), 0644); err != nil {
		t.Fatal(err)
	}

	_, client, srvAddr := startTestServer(t, root, nil)

	rq := []byte{0, tftpRRQ}
	rq = append(rq, "boot.img"...)
	rq = append(rq, 0xff, 0)
	rq = append(rq, "octet"...)
	rq = append(rq, 0)
	if _, err := client.WriteToUDP(rq, srvAddr); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1500)
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, from, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read DATA: %v", err)
	}
	if n < 4 || binary.BigEndian.Uint16(buf[:2]) != tftpDATA {
		t.Fatalf("expected DATA op=3, got op=%d", binary.BigEndian.Uint16(buf[:2]))
	}
	if binary.BigEndian.Uint16(buf[2:4]) != 1 {
		t.Fatalf("first DATA block = %d, want 1", binary.BigEndian.Uint16(buf[2:4]))
	}
	if payload := buf[4:n]; string(payload) != "pxeboot" {
		t.Fatalf("received %q, want %q", payload, "pxeboot")
	}
	if _, err := client.WriteToUDP(buildACK(1), from); err != nil {
		t.Fatal(err)
	}
}

func TestTFTPTransfer(t *testing.T) {
	root := t.TempDir()
	content := make([]byte, 1500)
	for i := range content {
		content[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(root, "boot.img"), content, 0644); err != nil {
		t.Fatal(err)
	}

	_, client, srvAddr := startTestServer(t, root, nil)

	if _, err := client.WriteToUDP(buildRRQ("boot.img"), srvAddr); err != nil {
		t.Fatal(err)
	}

	var got []byte
	var transferAddr *net.UDPAddr
	block := uint16(1)
	for {
		buf := make([]byte, 1500)
		if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, from, err := client.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read DATA: %v", err)
		}
		if transferAddr == nil {
			transferAddr = from
		} else if !sameAddr(transferAddr, from) {
			t.Fatalf("DATA from %s, expected transfer socket %s", from, transferAddr)
		}
		if n < 4 || binary.BigEndian.Uint16(buf[:2]) != tftpDATA {
			t.Fatalf("expected DATA op=3, got op=%d len=%d", binary.BigEndian.Uint16(buf[:2]), n)
		}
		gotBlock := binary.BigEndian.Uint16(buf[2:4])
		if gotBlock != block {
			t.Fatalf("got block %d, want %d", gotBlock, block)
		}
		payload := buf[4:n]
		got = append(got, payload...)
		last := len(payload) < tftpBlockSize
		if _, err := client.WriteToUDP(buildACK(block), transferAddr); err != nil {
			t.Fatal(err)
		}
		if last {
			break
		}
		block++
	}

	if !bytes.Equal(got, content) {
		t.Fatalf("received %d bytes, want %d", len(got), len(content))
	}
}

func TestTFTPDuplicateRRQResendsFirstBlock(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "boot.img"), make([]byte, 1024), 0644); err != nil {
		t.Fatal(err)
	}

	_, client, srvAddr := startTestServer(t, root, nil)

	if _, err := client.WriteToUDP(buildRRQ("boot.img"), srvAddr); err != nil {
		t.Fatal(err)
	}

	readData := func(deadline time.Duration) (uint16, []byte) {
		buf := make([]byte, 1500)
		if err := client.SetReadDeadline(time.Now().Add(deadline)); err != nil {
			t.Fatal(err)
		}
		n, _, err := client.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read DATA: %v", err)
		}
		if n < 4 || binary.BigEndian.Uint16(buf[:2]) != tftpDATA {
			t.Fatalf("expected DATA, got op=%d", binary.BigEndian.Uint16(buf[:2]))
		}
		return binary.BigEndian.Uint16(buf[2:4]), buf[4:n]
	}

	block1, _ := readData(5 * time.Second)
	if block1 != 1 {
		t.Fatalf("first DATA block = %d, want 1", block1)
	}

	// Duplicate RRQ (retransmission) must re-send block 1, not start a new transfer.
	if _, err := client.WriteToUDP(buildRRQ("boot.img"), srvAddr); err != nil {
		t.Fatal(err)
	}
	blockAgain, _ := readData(5 * time.Second)
	if blockAgain != 1 {
		t.Fatalf("duplicate RRQ DATA block = %d, want 1", blockAgain)
	}
}

func TestTFTPTraversalRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "boot.img"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	_, client, srvAddr := startTestServer(t, root, nil)

	for _, name := range []string{
		"../../../../etc/passwd",
		"boot.img/../../../etc/passwd",
		"/etc/passwd",
		"..",
	} {
		if _, err := client.WriteToUDP(buildRRQ(name), srvAddr); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1500)
		if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, _, err := client.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("RRQ %q: expected ERR reply, got read error: %v", name, err)
		}
		if n < 4 || binary.BigEndian.Uint16(buf[:2]) != tftpERR {
			t.Fatalf("RRQ %q: expected ERR op=5, got op=%d", name, binary.BigEndian.Uint16(buf[:2]))
		}
	}
}

func TestTFTPRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.bin")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "leak.bin")); err != nil {
		t.Fatal(err)
	}

	_, client, srvAddr := startTestServer(t, root, nil)

	if _, err := client.WriteToUDP(buildRRQ("leak.bin"), srvAddr); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected ERR reply, got read error: %v", err)
	}
	if n < 4 || binary.BigEndian.Uint16(buf[:2]) != tftpERR {
		t.Fatalf("expected ERR op=5, got op=%d", binary.BigEndian.Uint16(buf[:2]))
	}
}

func TestSetMacImagesConcurrentWithResolve(t *testing.T) {
	s := New(cfgWithDefaults())
	s.cfg.DefaultImage = "undionly.kpxe"

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.resolveImage("01-82-78-97-41-e1-db")
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			s.SetMacImages(map[string]string{
				"82:78:97:41:e1:db": fmt.Sprintf("img-%d.bin", i),
			})
			s.SetDefaultImage(fmt.Sprintf("default-%d.bin", i))
		}
		close(stop)
	}()
	wg.Wait()
}

// TestOpenTFTPFileRejectsSymlinkSwap verifies the TOCTOU defence: a symlink
// swapped in after path validation is rejected at open time, not followed.
func TestOpenTFTPFileRejectsSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "boot.img"), []byte("real"), 0644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.bin")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}

	// Validate while the path is a real file.
	s := New(cfgWithDefaults())
	s.cfg.TFTPRoot = root
	rootReal, rel, err := s.resolvePath(root, "sub/boot.img")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}

	// Swap the file for a symlink to an outside file between validation and open.
	realPath := filepath.Join(root, "sub", "boot.img")
	if err := os.Remove(realPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, realPath); err != nil {
		t.Fatal(err)
	}

	f, err := openTFTPFile(rootReal, rel)
	if err == nil {
		f.Close()
		t.Fatal("openTFTPFile followed a swapped symlink; want rejection")
	}
}

// TestTFTPFinalBlockRetransmit verifies the final (short) DATA block is
// retransmitted when its ACK is lost, instead of silently ending the transfer.
func TestTFTPFinalBlockRetransmit(t *testing.T) {
	root := t.TempDir()
	content := make([]byte, 600) // block 1 full (512), block 2 short (88)
	for i := range content {
		content[i] = byte(i*13 + 1)
	}
	if err := os.WriteFile(filepath.Join(root, "boot.img"), content, 0644); err != nil {
		t.Fatal(err)
	}

	_, client, srvAddr := startTestServer(t, root, nil)

	if _, err := client.WriteToUDP(buildRRQ("boot.img"), srvAddr); err != nil {
		t.Fatal(err)
	}

	var transferAddr *net.UDPAddr
	block := uint16(1)
	var got []byte
	for {
		buf := make([]byte, 1500)
		if err := client.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, from, err := client.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read DATA: %v", err)
		}
		if transferAddr == nil {
			transferAddr = from
		}
		if n < 4 || binary.BigEndian.Uint16(buf[:2]) != tftpDATA {
			t.Fatalf("expected DATA op=3, got op=%d len=%d", binary.BigEndian.Uint16(buf[:2]), n)
		}
		gotBlock := binary.BigEndian.Uint16(buf[2:4])
		if gotBlock != block {
			t.Fatalf("got block %d, want %d", gotBlock, block)
		}
		payload := buf[4:n]
		got = append(got, payload...)

		if len(payload) < tftpBlockSize {
			// Final block received. Simulate a lost ACK: wait for the server
			// to retransmit it before ACKing.
			buf2 := make([]byte, 1500)
			if err := client.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			n2, from2, err := client.ReadFromUDP(buf2)
			if err != nil {
				t.Fatalf("expected final block retransmit, got read error: %v", err)
			}
			if !sameAddr(transferAddr, from2) {
				t.Fatalf("retransmit from %s, expected %s", from2, transferAddr)
			}
			if n2 < 4 || binary.BigEndian.Uint16(buf2[:2]) != tftpDATA || binary.BigEndian.Uint16(buf2[2:4]) != block {
				t.Fatalf("expected DATA block %d retransmit, got op=%d blk=%d", block, binary.BigEndian.Uint16(buf2[:2]), binary.BigEndian.Uint16(buf2[2:4]))
			}
			if _, err := client.WriteToUDP(buildACK(block), transferAddr); err != nil {
				t.Fatal(err)
			}
			break
		}

		if _, err := client.WriteToUDP(buildACK(block), transferAddr); err != nil {
			t.Fatal(err)
		}
		block++
	}

	if !bytes.Equal(got, content) {
		t.Fatalf("received %d bytes, want %d", len(got), len(content))
	}
}

func cfgWithDefaults() config.PXEConfig {
	return config.PXEConfig{Enabled: true, TFTPRoot: "/tmp/tftpboot"}
}
