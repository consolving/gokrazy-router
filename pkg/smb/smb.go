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
	"strconv"
	"text/template"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
)

type Server struct {
	cfg     config.SMBConfig
	cmd     *exec.Cmd
	isSamba bool
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
{{- if .IfaceParam }}
interfaces = {{.IfaceParam}}
bind interfaces only = yes
{{- end }}
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

	if s.cfg.UsePortableServer || filepath.Base(bin) == "portable-smb-server" {
		return s.startPortable(bin, listen)
	}
	return s.startSamba(bin, listen)
}

func (s *Server) startPortable(bin, listen string) error {
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("smb: invalid listen address %q: %w", listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("smb: invalid listen port %q: %w", portStr, err)
	}
	shareName := s.cfg.ShareName
	if shareName == "" {
		shareName = "data"
	}

	log.Printf("smb: starting portable-smb-server on %s, share %s at %s for user %s", listen, shareName, s.cfg.SharePath, s.cfg.User)
	s.cmd = exec.Command(bin,
		"-ip", host,
		"-port", strconv.Itoa(port),
		"-user", s.cfg.User,
		"-pass", s.cfg.Password,
		"-folder", s.cfg.SharePath,
		"-share", shareName,
	)
	s.cmd.Stdout = os.Stdout
	s.cmd.Stderr = os.Stderr
	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("smb: start portable-smb-server: %w", err)
	}
	return nil
}

func (s *Server) startSamba(bin, listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}

	shareName := s.cfg.ShareName
	if shareName == "" {
		shareName = "data"
	}

	confPath := "/tmp/smb.conf"

	ifaceParam := host
	if host == "" || host == "0.0.0.0" || host == "::" {
		ifaceParam = ""
	}
	tmplData := map[string]string{
		"IfaceParam": ifaceParam,
		"ShareName":  shareName,
		"SharePath":  s.cfg.SharePath,
		"User":       s.cfg.User,
	}

	t := template.Must(template.New("smb").Parse(confTemplate))
	var buf bytes.Buffer
	if err := t.Execute(&buf, tmplData); err != nil {
		return fmt.Errorf("smb: generate config: %w", err)
	}
	if err := os.WriteFile(confPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("smb: write config: %w", err)
	}

	if err := createUser(confPath, s.cfg.User, s.cfg.Password); err != nil {
		return fmt.Errorf("smb: create user: %w", err)
	}

	log.Printf("smb: starting smbd on %s, share %s at %s for user %s", listen, shareName, s.cfg.SharePath, s.cfg.User)
	s.cmd = exec.Command(bin, "--foreground", "--no-process-group", "-s", confPath)
	s.cmd.Stdout = os.Stdout
	s.cmd.Stderr = os.Stderr
	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("smb: start smbd: %w", err)
	}

	s.isSamba = true

	// Give smbd a moment and verify it is listening.
	go func() {
		time.Sleep(500 * time.Millisecond)
		if s.cmd.ProcessState != nil && s.cmd.ProcessState.Exited() {
			log.Printf("smb: smbd exited early: %v", s.cmd.ProcessState.String())
		}
	}()

	return nil
}

// AddUser creates an additional SMB user in the password database.
// Only supported for Samba (smbd) mode; returns an error for portable mode.
func (s *Server) AddUser(user, password string) error {
	if !s.isSamba {
		return fmt.Errorf("smb: AddUser only supported in Samba mode")
	}
	log.Printf("smb: adding additional user %s", user)
	return createUser("/tmp/smb.conf", user, password)
}

func (s *Server) Stop() error {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(os.Interrupt)
		_ = s.cmd.Wait()
	}
	return nil
}

// createUser tries to add the user using smbpasswd that ships with the binary.
func createUser(confPath, user, password string) error {
	smbpasswd := "smbpasswd"

	addCmd := exec.Command(smbpasswd, "-s", "-a", user)
	addCmd.Env = append(os.Environ(), "SMB_CONF_PATH="+confPath)
	addCmd.Stderr = os.Stderr
	addCmd.Stdout = os.Stdout
	stdin, err := addCmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := addCmd.Start(); err != nil {
		return err
	}
	_, _ = io.WriteString(stdin, password+"\n")
	_, _ = io.WriteString(stdin, password+"\n")
	_ = stdin.Close()
	if err := addCmd.Wait(); err != nil {
		// User may already exist — try update.
		log.Printf("smb: user %s already exists, updating password", user)
		updCmd := exec.Command(smbpasswd, "-s", user)
		updCmd.Env = append(os.Environ(), "SMB_CONF_PATH="+confPath)
		updCmd.Stderr = os.Stderr
		updCmd.Stdout = os.Stdout
		stdin2, _ := updCmd.StdinPipe()
		_ = updCmd.Start()
		_, _ = io.WriteString(stdin2, password+"\n")
		_, _ = io.WriteString(stdin2, password+"\n")
		_ = stdin2.Close()
		return updCmd.Wait()
	}
	log.Printf("smb: user %s created", user)
	return nil
}
