package nfs

import (
	"strings"
	"testing"

	"github.com/consolving/gokrazy-router/pkg/config"
)

func TestExportsFile(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.NFSConfig
		want string
	}{
		{
			name: "default ro no_root_squash all clients",
			cfg:  config.NFSConfig{ExportPath: "/mnt/sata/netboot"},
			want: "/mnt/sata/netboot *(ro,no_root_squash)\n",
		},
		{
			name: "clients restricted, rw, root squash",
			cfg: config.NFSConfig{
				ExportPath: "/mnt/sata/netboot",
				Clients:    "10.0.31.0/25",
				ReadWrite:  true,
				RootSquash: true,
			},
			want: "/mnt/sata/netboot 10.0.31.0/25(rw,root_squash)\n",
		},
		{
			name: "extra exports appended",
			cfg: config.NFSConfig{
				ExportPath:   "/mnt/sata/netboot",
				ExtraExports: []string{"/mnt/sata/data 10.0.1.0/24(rw)"},
			},
			want: "/mnt/sata/netboot *(ro,no_root_squash)\n/mnt/sata/data 10.0.1.0/24(rw)\n",
		},
		{
			name: "only extra exports",
			cfg: config.NFSConfig{
				ExtraExports: []string{"/mnt/sata/data 10.0.1.0/24(rw)"},
			},
			want: "/mnt/sata/data 10.0.1.0/24(rw)\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New(c.cfg)
			got, err := s.exportsFile()
			if err != nil {
				t.Fatalf("exportsFile: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("exportsFile = %q, want %q", got, c.want)
			}
		})
	}
}

func TestUnfsdArgs(t *testing.T) {
	s := New(config.NFSConfig{})
	got := s.unfsdArgs("/tmp/nfs-exports")
	want := []string{"-d", "-e", "/tmp/nfs-exports", "-n", "2049", "-m", "20048"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("unfsdArgs = %v, want %v", got, want)
	}

	s = New(config.NFSConfig{NFSPort: 2049, MountdPort: 30000})
	got = s.unfsdArgs("/tmp/e")
	want = []string{"-d", "-e", "/tmp/e", "-n", "2049", "-m", "30000"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("unfsdArgs = %v, want %v", got, want)
	}
}

func TestValidatePath(t *testing.T) {
	if err := validatePath(""); err != nil {
		t.Errorf("empty path should be allowed: %v", err)
	}
	if err := validatePath("/mnt/sata/netboot"); err != nil {
		t.Errorf("valid path rejected: %v", err)
	}
	if err := validatePath("relative/path"); err == nil {
		t.Error("relative path accepted")
	}
	if err := validatePath("/mnt/sata/foo bar"); err == nil {
		t.Error("path with space accepted")
	}
	if err := validatePath("/mnt/sata/foo\n/bar"); err == nil {
		t.Error("path with newline accepted")
	}
}

func TestValidateClients(t *testing.T) {
	if err := validateClients(""); err != nil {
		t.Errorf("empty clients should be allowed: %v", err)
	}
	for _, ok := range []string{"*", "10.0.31.0/25", "host.example.org", "192.168.1.5"} {
		if err := validateClients(ok); err != nil {
			t.Errorf("valid clients %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"10.0.0.0/8(rw)", "a b", "*\n/path(rw)"} {
		if err := validateClients(bad); err == nil {
			t.Errorf("invalid clients %q accepted", bad)
		}
	}
}

func TestValidateExtraExport(t *testing.T) {
	if err := validateExtraExport("/mnt/sata/data 10.0.1.0/24(rw)"); err != nil {
		t.Errorf("valid extra export rejected: %v", err)
	}
	if err := validateExtraExport("foo\n/mnt/evil(rw)"); err == nil {
		t.Error("extra export with newline accepted")
	}
	if err := validateExtraExport("   "); err != nil {
		t.Errorf("blank line should be allowed: %v", err)
	}
}

func TestStartDisabled(t *testing.T) {
	s := New(config.NFSConfig{Enabled: false})
	if err := s.Start(); err != nil {
		t.Errorf("disabled Start returned error: %v", err)
	}
}
