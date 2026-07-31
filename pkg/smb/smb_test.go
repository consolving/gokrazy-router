package smb

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/consolving/gokrazy-router/pkg/config"
)

func TestNew(t *testing.T) {
	s := New(config.SMBConfig{Enabled: true, User: "u", Password: "p", SharePath: "/s"})
	if s.cfg.Enabled != true {
		t.Error("Expected enabled")
	}
	if s.isSamba {
		t.Error("New server should not be in Samba mode until startSamba is called")
	}
}

func TestAddUserPortableMode(t *testing.T) {
	s := &Server{isSamba: false}
	err := s.AddUser("test", "password")
	if err == nil {
		t.Fatal("expected error for portable mode, got nil")
	}
	if !strings.Contains(err.Error(), "only supported in Samba mode") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestConfTemplateRendered(t *testing.T) {
	tmpl := template.Must(template.New("smb").Parse(confTemplate))

	tests := []struct {
		name     string
		data     map[string]string
		checks   []string // substrings that must be present
		noChecks []string // substrings that must NOT be present
	}{
		{
			name: "with iface param",
			data: map[string]string{
				"IfaceParam": "192.168.1.1",
				"ShareName":  "data",
				"SharePath":  "/mnt/data",
				"ValidUsers": "files",
			},
			checks:   []string{"interfaces = 192.168.1.1", "bind interfaces only = yes", "path = /mnt/data", "valid users = files"},
			noChecks: nil,
		},
		{
			name: "no iface param",
			data: map[string]string{
				"IfaceParam": "",
				"ShareName":  "backup",
				"SharePath":  "/mnt/backup",
				"ValidUsers": "backup",
			},
			checks:   []string{"[backup]", "path = /mnt/backup", "valid users = backup"},
			noChecks: []string{"interfaces =", "bind interfaces only = yes"},
		},
		{
			name: "custom share name",
			data: map[string]string{
				"IfaceParam": "0.0.0.0",
				"ShareName":  "custom",
				"SharePath":  "/mnt/data",
				"ValidUsers": "admin",
			},
			checks:   []string{"[custom]", "valid users = admin"},
			noChecks: nil,
		},
		{
			name: "primary plus extras users",
			data: map[string]string{
				"IfaceParam": "",
				"ShareName":  "data",
				"SharePath":  "/mnt/data",
				"ValidUsers": "files alice bob",
			},
			checks:   []string{"valid users = files alice bob"},
			noChecks: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, tt.data); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			for _, want := range tt.checks {
				if !strings.Contains(out, want) {
					t.Errorf("expected %q in output:\n%s", want, out)
				}
			}
			for _, no := range tt.noChecks {
				if strings.Contains(out, no) {
					t.Errorf("unexpected %q in output:\n%s", no, out)
				}
			}
		})
	}
}

// TestExtrasUsersInValidUsers verifies that extras users configured before
// Start are rendered into the share's "valid users" rule, so created users
// can actually access the share.
func TestExtrasUsersInValidUsers(t *testing.T) {
	s := New(config.SMBConfig{Enabled: true, User: "files", Password: "pw", SharePath: "/mnt/data"})
	s.SetExtraUsers([]config.SMBUser{
		{Name: "alice", Password: "alicepw"},
		{Name: "bob", Password: "bobpw"},
	})

	tmpl := template.Must(template.New("smb").Parse(confTemplate))
	var buf bytes.Buffer
	data := map[string]string{
		"IfaceParam": "",
		"ShareName":  "data",
		"SharePath":  "/mnt/data",
		"ValidUsers": validUsersFor(s),
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"files", "alice", "bob"} {
		if !strings.Contains(out, want) {
			t.Errorf("valid users line missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "valid users = files\n") {
		t.Errorf("valid users must list extras users, got:\n%s", out)
	}
}

// validUsersFor mirrors the startSamba computation for the test above.
func validUsersFor(s *Server) string {
	users := []string{s.cfg.User}
	for _, u := range s.extraUsers {
		users = append(users, u.Name)
	}
	return strings.Join(users, " ")
}

func TestValidateUser(t *testing.T) {
	good := []struct{ name, password string }{
		{"alice", "s3cret"},
		{"a", "p"},
		{"user.name_01", "with space and ümlaut"},
		{"A-1", "\ttab is fine"},
	}
	for _, c := range good {
		if err := validateUser(c.name, c.password); err != nil {
			t.Errorf("validateUser(%q, ...) = %v, want nil", c.name, err)
		}
	}

	bad := []struct{ name, password string }{
		{"", "pw"},
		{"-bad", "pw"},
		{"bad name", "pw"},
		{"bad;name", "pw"},
		{"bad\nname", "pw"},
		{"alice", ""},
		{"alice", "pw\npw"},
		{"alice", "pw\x00pw"},
	}
	for _, c := range bad {
		if err := validateUser(c.name, c.password); err == nil {
			t.Errorf("validateUser(%q, %q) = nil, want error", c.name, c.password)
		}
	}
}

func TestValidateShareAndPath(t *testing.T) {
	for _, name := range []string{"data", "my-share_2", "a"} {
		if err := validateSimpleName(name, "share name"); err != nil {
			t.Errorf("validateSimpleName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "-x", "a b", "a;b", "a\nb", "verylongnameverylongnameverylongnameverylong"} {
		if err := validateSimpleName(name, "share name"); err == nil {
			t.Errorf("validateSimpleName(%q) = nil, want error", name)
		}
	}
	for _, p := range []string{"/mnt/data", "/mnt/data with space"} {
		if err := validatePath(p); err != nil {
			t.Errorf("validatePath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"", "/mnt/data\n# injection", "/mnt/data\x01"} {
		if err := validatePath(p); err == nil {
			t.Errorf("validatePath(%q) = nil, want error", p)
		}
	}
}

func TestPortableModeRejectsExtraUsers(t *testing.T) {
	s := &Server{cfg: config.SMBConfig{Enabled: true, User: "u", Password: "p", SharePath: "/s", UsePortableServer: true}}
	s.SetExtraUsers([]config.SMBUser{{Name: "alice", Password: "pw"}})
	err := s.startPortable("/usr/bin/false", "0.0.0.0:445")
	if err == nil {
		t.Fatal("expected error for extra users in portable mode, got nil")
	}
	if !strings.Contains(err.Error(), "not supported by the portable server") {
		t.Errorf("wrong error: %v", err)
	}
}
