// Package smb starts an external smbd process and exposes the mounted volume as a share.
package smb

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
)

type Server struct {
	cfg config.SMBConfig
	cmd *exec.Cmd
}

func New(cfg config.SMBConfig) *Server {
	return &Server{cfg: cfg}
}

const confTemplate = `[global]
workgroup = WORKGROUP
server role = standalone server
security = user
map to guest = never
min protocol = SMB2
passdb backend = tdbsam:/tmp/smbpassdb.tdb
pid directory = /tmp
lock directory = /tmp
state directory = /tmp
cache directory = /tmp
private dir = /tmp
interfaces = {{.Interfaces}}
bind interfaces only = yes
server min protocol = SMB2_10
server max protocol = SMB3_11

[{{.ShareName}}]
path = {{.SharePath}}
valid users = {{.User}}
read only = no
browsable = yes
guest ok = no
`

func (s *Server) Start() error {
	if !s.cfg.Enabled {
		return nil
	}
	if s.cfg.User == "" || s.cfg.Password == "" {
		return fmt.Errorf("smb: user and password must be configured")
	}
	if s.cfg.SharePath == "" {
		return fmt.Errorf("smb: share path not configured")
	}
	if err := os.MkdirAll(s.cfg.SharePath, 0755); err != nil {
		return fmt.Errorf("smb: mkdir %s: %w", s.cfg.SharePath, err)
	}

	bin := s.cfg.BinPath
	if bin == "" {
		bin = "/usr/local/bin/smbd"
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("smb: smbd binary not found at %s (bundle it via ExtraFilePaths): %w", bin, err)
	}

	listen := s.cfg.Listen
	if listen == "" {
		listen = "0.0.0.0:445"
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}

	confPath := "/tmp/smb.conf"
	t := template.Must(template.New("smb").Parse(confTemplate))
	var buf bytes.Buffer
	if err := t.Execute(&buf, map[string]string{
		"Interfaces": host,
		"ShareName":  s.cfg.ShareName,
		"SharePath":  s.cfg.SharePath,
		"User":       s.cfg.User,
	}); err != nil {
		return fmt.Errorf("smb: generate config: %w", err)
	}
	if err := os.WriteFile(confPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("smb: write config: %w", err)
	}

	if err := createUser(bin, s.cfg.User, s.cfg.Password); err != nil {
		return fmt.Errorf("smb: create user: %w", err)
	}

	log.Printf("smb: starting smbd on %s, share %s at %s for user %s", listen, s.cfg.ShareName, s.cfg.SharePath, s.cfg.User)
	s.cmd = exec.Command(bin, "--foreground", "--no-process-group", "-s", confPath)
	s.cmd.Stdout = os.Stdout
	s.cmd.Stderr = os.Stderr
	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("smb: start smbd: %w", err)
	}

	// Give smbd a moment and verify it is listening.
	go func() {
		time.Sleep(500 * time.Millisecond)
		if s.cmd.ProcessState != nil && s.cmd.ProcessState.Exited() {
			log.Printf("smb: smbd exited early: %v", s.cmd.ProcessState.String())
		}
	}()

	return nil
}

func (s *Server) Stop() error {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(os.Interrupt)
		_ = s.cmd.Wait()
	}
	return nil
}

// createUser tries to add the user using smbpasswd that ships with the binary.
func createUser(smbdBin, user, password string) error {
	dir := filepath.Dir(smbdBin)
	smbpasswd := filepath.Join(dir, "smbpasswd")
	if _, err := os.Stat(smbpasswd); err != nil {
		// Some static bundles ship smbpasswd next to smbd; otherwise fall back to PATH.
		smbpasswd = "smbpasswd"
	}

	cmd := exec.Command(smbpasswd, "-s", "-a", user)
	cmd.Env = append(os.Environ(), "PASSWD="+password)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	// smbpasswd -s reads password twice (new + retype) from stdin.
	_, _ = io.WriteString(stdin, password+"\n")
	_, _ = io.WriteString(stdin, password+"\n")
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderrBuf.String())
		if strings.Contains(msg, "already exists") {
			log.Printf("smb: user %s already exists, updating password", user)
			// Try update instead.
			cmd2 := exec.Command(smbpasswd, "-s", user)
			cmd2.Env = append(os.Environ(), "PASSWD="+password)
			stdin2, _ := cmd2.StdinPipe()
			cmd2.Stderr = os.Stderr
			cmd2.Stdout = os.Stdout
			_ = cmd2.Start()
			_, _ = io.WriteString(stdin2, password+"\n")
			_, _ = io.WriteString(stdin2, password+"\n")
			_ = stdin2.Close()
			return cmd2.Wait()
		}
		return fmt.Errorf("%s: %w", msg, err)
	}
	log.Printf("smb: user %s created", user)
	return nil
}
