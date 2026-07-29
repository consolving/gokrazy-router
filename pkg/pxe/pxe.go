// Package pxe implements a minimal TFTP server for PXE boot.
package pxe

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
	"github.com/pin/tftp/v3"
)

// Server serves PXE boot images over TFTP.
type Server struct {
	cfg     config.PXEConfig
	handler map[string]string // normalized MAC -> abs path
}

// New creates a TFTP server for the given PXE config.
func New(cfg config.PXEConfig) *Server {
	return &Server{
		cfg:     cfg,
		handler: make(map[string]string),
	}
}

// SetMacImages updates the MAC-to-image mapping at runtime (e.g. after reload).
func (s *Server) SetMacImages(m map[string]string) {
	s.handler = make(map[string]string)
	for mac, img := range m {
		s.handler[normalizeMAC(mac)] = img
	}
}

// Start launches the TFTP listener. It blocks until the server stops.
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
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("pxe: listen %s: %w", listen, err)
	}

	srv := tftp.NewServer(s.readHandler(root), nil)
	srv.SetTimeout(5 * time.Second)

	log.Printf("pxe: tftp server listening on %s, root %s", listen, root)
	return srv.Serve(conn)
}

func (s *Server) readHandler(root string) func(string, io.ReaderFrom) error {
	return func(filename string, rf io.ReaderFrom) error {
		mac := filenameToMAC(filename)
		img := ""
		if mac != "" {
			img = s.handler[normalizeMAC(mac)]
		}
		if img == "" {
			img = s.cfg.DefaultImage
		}
		if img == "" {
			return fmt.Errorf("pxe: no image for %s", filename)
		}

		if !filepath.IsAbs(img) {
			img = filepath.Join(root, img)
		}

		f, err := os.Open(img)
		if err != nil {
			log.Printf("pxe: cannot open %s: %v", img, err)
			return fmt.Errorf("pxe: open %s: %w", img, err)
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if ot, ok := rf.(tftp.OutgoingTransfer); ok {
			ot.SetSize(st.Size())
		}
		log.Printf("pxe: sending %s (requested %s)", img, filename)
		_, err = rf.ReadFrom(f)
		return err
	}
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
	mac = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(mac, "-", ":")))
	return mac
}
