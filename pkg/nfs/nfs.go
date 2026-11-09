// Package nfs starts a userspace NFSv3 server (unfs3) with an optional
// portmapper (rpcbind) to serve the mounted volume, e.g. for Mac NetBoot
// root images referenced by the DHCP root-path option.
package nfs

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
)

const (
	defaultUnfsdBin   = "/usr/local/bin/unfsd"
	defaultRpcbindBin = "/usr/local/bin/rpcbind"
	defaultExports    = "/tmp/nfs-exports"
	defaultNFSPort    = 2049
	defaultMountdPort = 20048
)

// Server supervises the rpcbind and unfsd subprocesses.
type Server struct {
	cfg  config.NFSConfig
	cmds []*exec.Cmd
}

// New creates an NFS server config wrapper. Start must be called to run it.
func New(cfg config.NFSConfig) *Server {
	return &Server{cfg: cfg}
}

// Start launches rpcbind (if configured) and unfsd in the background.
// The process stays alive for the lifetime of the router; there is no
// crash-restart supervision (matching the SMB service).
func (s *Server) Start() error {
	if !s.cfg.Enabled {
		return nil
	}
	if s.cfg.ExportPath == "" && len(s.cfg.ExtraExports) == 0 {
		return fmt.Errorf("nfs: no exportPath or extraExports configured")
	}
	if err := validatePath(s.cfg.ExportPath); err != nil {
		return fmt.Errorf("nfs: %w", err)
	}
	if err := validateClients(s.cfg.Clients); err != nil {
		return fmt.Errorf("nfs: %w", err)
	}
	for _, line := range s.cfg.ExtraExports {
		if err := validateExtraExport(line); err != nil {
			return fmt.Errorf("nfs: %w", err)
		}
	}

	exportsPath := s.cfg.ExportsFile
	if exportsPath == "" {
		exportsPath = defaultExports
	}
	content, err := s.exportsFile()
	if err != nil {
		return fmt.Errorf("nfs: %w", err)
	}
	if err := os.WriteFile(exportsPath, content, 0644); err != nil {
		return fmt.Errorf("nfs: write exports: %w", err)
	}
	log.Printf("nfs: exports: %s", strings.TrimSpace(string(content)))

	// rpcbind must be running before unfsd registers its ports, otherwise
	// clients cannot discover the mountd port and mounting fails.
	rpcbindBin := s.cfg.RpcbindBin
	if rpcbindBin == "" {
		rpcbindBin = defaultRpcbindBin
	}
	if rpcbindBin != "-" {
		if _, err := os.Stat(rpcbindBin); err != nil {
			return fmt.Errorf("nfs: rpcbind binary not found at %s (bundle it via ExtraFilePaths, or set rpcbindBin to \"-\" to skip): %w", rpcbindBin, err)
		}
		// -f: foreground. -d: debug mode — rpcbind logs errors to stderr
		// (the router daemon's log) instead of syslog, which gokrazy lacks.
		cmd := exec.Command(rpcbindBin, "-f", "-d")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("nfs: start rpcbind: %w", err)
		}
		s.cmds = append(s.cmds, cmd)
		log.Printf("nfs: rpcbind started (pid %d)", cmd.Process.Pid)
		go func() {
			if err := cmd.Wait(); err != nil {
				log.Printf("nfs: rpcbind exited: %v", err)
			} else {
				log.Printf("nfs: rpcbind exited")
			}
		}()
		time.Sleep(300 * time.Millisecond)
	}

	unfsdBin := s.cfg.UnfsdBin
	if unfsdBin == "" {
		unfsdBin = defaultUnfsdBin
	}
	if _, err := os.Stat(unfsdBin); err != nil {
		return fmt.Errorf("nfs: unfsd binary not found at %s (bundle it via ExtraFilePaths): %w", unfsdBin, err)
	}

	args := s.unfsdArgs(exportsPath)
	cmd := exec.Command(unfsdBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("nfs: start unfsd: %w", err)
	}
	s.cmds = append(s.cmds, cmd)
	log.Printf("nfs: unfsd started (pid %d, args %v)", cmd.Process.Pid, args)

	// Log unexpected daemon exits: unfsd runs in the foreground and only
	// terminates on error (bad exports file, port in use, missing
	// /etc/netconfig, ...).
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("nfs: unfsd exited: %v", err)
		} else {
			log.Printf("nfs: unfsd exited")
		}
	}()
	return nil
}

// Stop terminates rpcbind and unfsd.
func (s *Server) Stop() error {
	for _, cmd := range s.cmds {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
			_, _ = cmd.Process.Wait()
		}
	}
	return nil
}

// exportsFile renders the unfs3 exports file content.
func (s *Server) exportsFile() ([]byte, error) {
	opts := "no_root_squash"
	if s.cfg.RootSquash {
		opts = "root_squash"
	}
	mode := "ro"
	if s.cfg.ReadWrite {
		mode = "rw"
	}

	var lines []string
	if s.cfg.ExportPath != "" {
		clients := s.cfg.Clients
		if clients == "" {
			clients = "*"
		}
		lines = append(lines, fmt.Sprintf("%s %s(%s,%s)", s.cfg.ExportPath, clients, mode, opts))
	}
	lines = append(lines, s.cfg.ExtraExports...)
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// unfsdArgs builds the unfsd command line: foreground, custom exports file,
// fixed ports so firewall rules and DHCP root-path references stay stable.
// unfsd listens on UDP+TCP and registers with the portmapper by default,
// which is what classic Mac NetBoot booters require.
func (s *Server) unfsdArgs(exportsPath string) []string {
	nfsPort := s.cfg.NFSPort
	if nfsPort == 0 {
		nfsPort = defaultNFSPort
	}
	mountdPort := s.cfg.MountdPort
	if mountdPort == 0 {
		mountdPort = defaultMountdPort
	}
	return []string{
		"-d",
		"-e", exportsPath,
		"-n", strconv.Itoa(nfsPort),
		"-m", strconv.Itoa(mountdPort),
	}
}

// validatePath rejects export paths with whitespace or control characters
// that could inject additional exports-file entries.
func validatePath(p string) error {
	if p == "" {
		return nil
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("export path %q must be absolute", p)
	}
	for _, r := range p {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("export path %q contains whitespace or control characters", p)
		}
	}
	return nil
}

// validateClients rejects client specs with whitespace, control characters or
// characters that would inject exports-file syntax (options parentheses,
// commas). Valid specs are hostnames, IPs, CIDR ranges or "*", matching the
// unfs3 exports-file syntax.
func validateClients(c string) error {
	if c == "" {
		return nil
	}
	for _, r := range c {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("clients %q contains whitespace or control characters", c)
		}
		if r == '(' || r == ')' || r == ',' {
			return fmt.Errorf("clients %q contains exports-file syntax characters ( ) ,", c)
		}
	}
	return nil
}

// validateExtraExport rejects raw exports-file lines that could smuggle in a
// second export entry or options.
func validateExtraExport(line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	for _, r := range line {
		if r == '\n' || r == '\r' || (r < 0x20 && r != '\t') || r == 0x7f {
			return fmt.Errorf("extra export %q contains control characters", line)
		}
	}
	return nil
}
