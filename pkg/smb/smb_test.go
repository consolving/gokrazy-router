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
				"User":       "files",
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
				"User":       "backup",
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
				"User":       "admin",
			},
			checks:   []string{"[custom]", "valid users = admin"},
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
