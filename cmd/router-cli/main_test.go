package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "router-cli")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func TestCLIExtrasListEmpty(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "extras.toml")

	out, err := exec.Command(bin, "extras", "--file", file, "list").CombinedOutput()
	if err != nil {
		t.Fatalf("extras list: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "(empty)" {
		t.Errorf("expected (empty), got %q", strings.TrimSpace(string(out)))
	}
}

func TestCLIExtrasSetReservation(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "extras.toml")

	out, err := exec.Command(bin, "extras", "--file", file, "set-reservation", "aa:bb:cc:dd:ee:ff", "10.0.1.10").CombinedOutput()
	if err != nil {
		t.Fatalf("set-reservation: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "reservation set") {
		t.Errorf("unexpected output: %s", out)
	}

	// Verify file was created.
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !strings.Contains(string(data), "aa:bb:cc:dd:ee:ff") {
		t.Errorf("file missing reservation:\n%s", data)
	}

	// Idempotent — second call should say unchanged.
	out2, _ := exec.Command(bin, "extras", "--file", file, "set-reservation", "aa:bb:cc:dd:ee:ff", "10.0.1.10").CombinedOutput()
	if !strings.Contains(string(out2), "unchanged") {
		t.Errorf("expected unchanged, got: %s", out2)
	}
}

func TestCLIExtrasRemoveReservation(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "extras.toml")

	// Remove non-existent.
	out, _ := exec.Command(bin, "extras", "--file", file, "remove-reservation", "aa:bb:cc:dd:ee:ff").CombinedOutput()
	if !strings.Contains(string(out), "no reservation found") {
		t.Errorf("expected not found, got: %s", out)
	}

	// Add then remove.
	exec.Command(bin, "extras", "--file", file, "set-reservation", "aa:bb:cc:dd:ee:ff", "10.0.1.10").Run()
	out2, err := exec.Command(bin, "extras", "--file", file, "remove-reservation", "aa:bb:cc:dd:ee:ff").CombinedOutput()
	if err != nil {
		t.Fatalf("remove-reservation: %v\n%s", err, out2)
	}
	if !strings.Contains(string(out2), "reservation removed") {
		t.Errorf("unexpected output: %s", out2)
	}
}

func TestCLIExtrasSetMacImage(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "extras.toml")

	out, err := exec.Command(bin, "extras", "--file", file, "set-mac-image", "aa:bb:cc:dd:ee:ff", "ipxe.efi").CombinedOutput()
	if err != nil {
		t.Fatalf("set-mac-image: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "mac image set") {
		t.Errorf("unexpected output: %s", out)
	}

	// Verify file.
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "ipxe.efi") {
		t.Errorf("file missing mac image:\n%s", data)
	}
}

func TestCLIExtrasRemoveMacImage(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "extras.toml")

	out, _ := exec.Command(bin, "extras", "--file", file, "remove-mac-image", "aa:bb:cc:dd:ee:ff").CombinedOutput()
	if !strings.Contains(string(out), "no mac image found") {
		t.Errorf("expected not found, got: %s", out)
	}

	exec.Command(bin, "extras", "--file", file, "set-mac-image", "aa:bb:cc:dd:ee:ff", "ipxe.efi").Run()
	out2, err := exec.Command(bin, "extras", "--file", file, "remove-mac-image", "aa:bb:cc:dd:ee:ff").CombinedOutput()
	if err != nil {
		t.Fatalf("remove-mac-image: %v\n%s", err, out2)
	}
	if !strings.Contains(string(out2), "mac image removed") {
		t.Errorf("unexpected output: %s", out2)
	}
}

func TestCLIExtrasBadArgs(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "extras.toml")

	// Missing args should exit non-zero.
	err := exec.Command(bin, "extras", "--file", file, "set-reservation", "too-few-args").Run()
	if err == nil {
		t.Error("expected error for missing args")
	}
}
