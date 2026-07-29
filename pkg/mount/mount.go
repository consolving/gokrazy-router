// Package mount helpers attaches a block device for use by optional services.
package mount

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
)

// Mount waits for the configured device and mounts it at Target.
func Mount(cfg config.MountConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Device == "" {
		return fmt.Errorf("mount: device not configured")
	}
	if cfg.Target == "" {
		return fmt.Errorf("mount: target not configured")
	}
	if cfg.FsType == "" {
		cfg.FsType = "auto"
	}

	if err := waitForDevice(cfg.Device, 30*time.Second); err != nil {
		return fmt.Errorf("mount: device %s not ready: %w", cfg.Device, err)
	}

	if err := os.MkdirAll(cfg.Target, 0755); err != nil {
		return fmt.Errorf("mount: mkdir %s: %w", cfg.Target, err)
	}

	if mounted, err := isMounted(cfg.Target); err != nil {
		return fmt.Errorf("mount: check %s: %w", cfg.Target, err)
	} else if mounted {
		log.Printf("mount: %s already mounted", cfg.Target)
		return nil
	}

	args := []string{"-t", cfg.FsType}
	if cfg.Options != "" {
		args = append(args, "-o", cfg.Options)
	}
	args = append(args, cfg.Device, cfg.Target)

	cmd := exec.Command("mount", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mount: mount %s to %s failed: %w", cfg.Device, cfg.Target, err)
	}

	log.Printf("mount: mounted %s (%s) at %s", cfg.Device, cfg.FsType, cfg.Target)
	return nil
}

// Unmount tries to unmount the configured target.
func Unmount(cfg config.MountConfig) error {
	if !cfg.Enabled || cfg.Target == "" {
		return nil
	}
	if mounted, err := isMounted(cfg.Target); err != nil || !mounted {
		return nil
	}
	cmd := exec.Command("umount", cfg.Target)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mount: umount %s: %w", cfg.Target, err)
	}
	log.Printf("mount: unmounted %s", cfg.Target)
	return nil
}

func waitForDevice(device string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(device); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", device)
}

func isMounted(target string) (bool, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 2 && fields[1] == target {
			return true, nil
		}
	}
	return false, nil
}
