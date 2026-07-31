package main

import (
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
	"github.com/consolving/gokrazy-router/pkg/dhcp"
	"github.com/consolving/gokrazy-router/pkg/mount"
	"github.com/consolving/gokrazy-router/pkg/nat"
	"github.com/consolving/gokrazy-router/pkg/netsetup"
	"github.com/consolving/gokrazy-router/pkg/pxe"
	"github.com/consolving/gokrazy-router/pkg/smb"
	"github.com/consolving/gokrazy-router/pkg/status"
	"github.com/consolving/gokrazy-router/pkg/vlan"
	"github.com/consolving/gokrazy-router/pkg/wifi"
	"github.com/vishvananda/netlink"
)

// dhcpEntry ties one running DHCP server to the reservations from router.json
// for its scope. baseScope points into the immutable base config snapshot;
// reloads merge it with the extras file so base-JSON reservations survive.
type dhcpEntry struct {
	srv       *dhcp.Server
	tftpAddr  string
	baseScope *config.DHCPConfig
}

func main() {
	configPath := flag.String("config", "/etc/gokrazy-router.json", "path to configuration file")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("config not found (%v), using defaults", err)
		cfg = config.Default()
	}

	// Expand ${ENV} references in service configuration.
	cfg.ExpandEnv()

	// Apply global DNS default to all DHCP scopes that don't have one.
	if len(cfg.DNS) > 0 {
		if len(cfg.LAN.DHCP.DNS) == 0 {
			cfg.LAN.DHCP.DNS = append([]string(nil), cfg.DNS...)
		}
		for i := range cfg.VLANs {
			if len(cfg.VLANs[i].DHCP.DNS) == 0 {
				cfg.VLANs[i].DHCP.DNS = append([]string(nil), cfg.DNS...)
			}
		}
		if len(cfg.WiFi.DHCP.DNS) == 0 {
			cfg.WiFi.DHCP.DNS = append([]string(nil), cfg.DNS...)
		}
	}

	// Immutable snapshot of the base JSON config, taken before any extras are
	// merged. Reloads derive effective per-scope state (e.g. reservations)
	// from this snapshot plus the current extras file, instead of mutating
	// cfg cumulatively.
	base := cloneConfig(cfg)

	// 0. Optional: mount a disk used by SMB/PXE/extras config.
	if cfg.Services.Mount.Enabled {
		if err := mount.Mount(cfg.Services.Mount); err != nil {
			log.Fatalf("mount: %v", err)
		}
		defer mount.Unmount(cfg.Services.Mount)
	}

	// Load optional extras file from the mounted volume. It overrides/extends
	// reservations, PXE images and SMB users. Reload happens on every restart.
	// If no extras file exists yet, a minimal one is created from the JSON
	// config so reservations/PXE settings can be edited at runtime.
	var extras *config.ExtrasConfig
	if cfg.Services.ExtrasFile != "" {
		var err error
		extras, err = loadOrCreateExtras(cfg.Services.ExtrasFile, cfg)
		if err != nil {
			log.Printf("extras: %v (continuing without)", err)
		} else {
			cfg.ApplyExtras(extras)
			log.Printf("extras: loaded %s", cfg.Services.ExtrasFile)
		}
	}

	// Derive default service paths from the mount target.
	if cfg.Services.SMB.Enabled && cfg.Services.SMB.SharePath == "" {
		cfg.Services.SMB.SharePath = cfg.Services.Mount.Target
	}
	if cfg.Services.PXE.Enabled && cfg.Services.PXE.TFTPRoot == "" {
		cfg.Services.PXE.TFTPRoot = filepath.Join(cfg.Services.Mount.Target, "tftpboot")
	}

	// 1. Network setup: either VLAN mode or flat bridge mode.
	vlanMode := len(cfg.VLANs) > 0
	var vlanBridges []vlan.VLANBridge

	if vlanMode {
		log.Printf("vlan: configuring %d VLANs", len(cfg.VLANs))
		vlanBridges, err = vlan.Setup(cfg.VLANs)
		if err != nil {
			log.Fatalf("vlan: %v", err)
		}
		_ = vlanBridges
	} else {
		_, err = netsetup.Setup(cfg.LAN.Bridge, cfg.LAN.Interfaces, cfg.LAN.Address)
		if err != nil {
			log.Fatalf("netsetup: %v", err)
		}
	}

	// 2. Enable IP forwarding.
	if err := netsetup.EnableForwarding(); err != nil {
		log.Fatalf("forwarding: %v", err)
	}

	// 3. Install NAT masquerade rules.
	var natMgr *nat.Manager
	if cfg.NAT.Enabled {
		if vlanMode {
			var initialized bool
			for _, vc := range cfg.VLANs {
				if !vc.NAT {
					continue
				}
				_, vNet, _ := net.ParseCIDR(vc.Address)
				if vNet == nil {
					continue
				}
				if !initialized {
					natMgr, err = nat.Setup(cfg.NAT.OutInterface, vNet)
					if err != nil {
						log.Fatalf("nat: %v", err)
					}
					initialized = true
				} else {
					if err := natMgr.AddSource(vNet); err != nil {
						log.Fatalf("nat: add VLAN %d source: %v", vc.ID, err)
					}
				}
			}
		} else {
			_, srcNet, err := net.ParseCIDR(cfg.LAN.Address)
			if err != nil {
				log.Fatalf("parse LAN CIDR: %v", err)
			}
			natMgr, err = nat.Setup(cfg.NAT.OutInterface, srcNet)
			if err != nil {
				log.Fatalf("nat: %v", err)
			}
		}
	}

	// 3b. Install inter-VLAN isolation rules for isolated VLANs.
	if vlanMode && natMgr != nil {
		var allBridges []string
		for _, vc := range cfg.VLANs {
			allBridges = append(allBridges, vlan.BridgeName(vc.ID))
		}
		for _, vc := range cfg.VLANs {
			if !vc.Isolated {
				continue
			}
			isolated := vlan.BridgeName(vc.ID)
			var others []string
			for _, b := range allBridges {
				if b != isolated {
					others = append(others, b)
				}
			}
			if len(others) > 0 {
				if err := natMgr.AddIsolation(isolated, others); err != nil {
					log.Fatalf("nat: isolation for VLAN %d: %v", vc.ID, err)
				}
				log.Printf("vlan: VLAN %d (%s) isolated from %d other VLANs", vc.ID, vc.Name, len(others))
			}
		}
	}

	// 4. Start status monitor (nftables per-client counters).
	wifiIface := cfg.WiFi.Interface
	if wifiIface == "" {
		wifiIface = "wlan0"
	}
	mon, err := status.New(cfg.NAT.OutInterface, cfg.LAN.Bridge, cfg.LAN.Interfaces, wifiIface)
	if err != nil {
		log.Printf("status monitor: %v (continuing without)", err)
	}

	// Collect running services for config reload.
	var dhcpEntries []dhcpEntry

	var activeServices []string

	// 5. Start WiFi AP (hostapd).
	var wifiAP *wifi.AP
	if cfg.WiFi.Enabled {
		log.Printf("wifi: waiting for %s to appear...", wifiIface)
		if err := waitForInterface(wifiIface, 120*time.Second); err != nil {
			log.Printf("wifi: %v — continuing without WiFi", err)
		} else {
			log.Printf("wifi: %s is available", wifiIface)

			wifiBridge := ""
			if vlanMode && cfg.WiFi.MacMapFile != "" {
				defaultVLAN := cfg.WiFi.DefaultVLAN
				if defaultVLAN == 0 {
					defaultVLAN = 1
				}
				wifiBridge = vlan.BridgeName(defaultVLAN)
				log.Printf("wifi: VLAN mode, bridging wlan0 into %s (default VLAN %d)", wifiBridge, defaultVLAN)
			} else if cfg.WiFi.Bridge != "" && cfg.WiFi.Address == "" {
				wifiBridge = cfg.WiFi.Bridge
			}

			ap, err := wifi.New(cfg.WiFi, wifiBridge)
			if err != nil {
				log.Fatalf("wifi: %v", err)
			}
			ap.OnClient(func(ev wifi.ClientEvent) {
				if ev.Associated {
					log.Printf("wifi: client %s connected via WLAN", ev.MAC)
				} else {
					log.Printf("wifi: client %s disconnected from WLAN", ev.MAC)
					if mon != nil {
						if err := mon.RemoveClientByMAC(ev.MAC); err != nil {
							log.Printf("status: failed to remove WiFi client %s: %v", ev.MAC, err)
						}
					}
				}
			})
			if err := ap.Start(); err != nil {
				log.Fatalf("wifi: start: %v", err)
			}
			wifiAP = ap
			if mon != nil {
				mon.SetWiFiSource(&wifiStationAdapter{ap: ap})
			}
			if cfg.WiFi.Address != "" && wifiAP.MacMap() == nil {
				if err := assignIP(wifiIface, cfg.WiFi.Address); err != nil {
					log.Fatalf("wifi: assign IP to %s: %v", wifiIface, err)
				}
				log.Printf("wifi: %s configured with %s (routed mode)", wifiIface, cfg.WiFi.Address)
				if natMgr != nil {
					_, wifiNet, err := net.ParseCIDR(cfg.WiFi.Address)
					if err != nil {
						log.Fatalf("parse WiFi CIDR: %v", err)
					}
					if err := natMgr.AddSource(wifiNet); err != nil {
						log.Fatalf("nat: add WiFi source: %v", err)
					}
				}
			}
		}
	}

	// 6. Start DHCP servers.
	if vlanMode {
		for i := range cfg.VLANs {
			vc := cfg.VLANs[i]
			if !vc.DHCP.Enabled {
				continue
			}
			bridgeName := vlan.BridgeName(vc.ID)
			srv, err := dhcp.New(
				bridgeName,
				vc.Address,
				vc.DHCP.RangeStart,
				vc.DHCP.RangeEnd,
				vc.DHCP.DNS,
				vc.DHCP.ParseLeaseDuration(),
			)
			if err != nil {
				log.Fatalf("dhcp vlan %d: %v", vc.ID, err)
			}
			dhcpEntries = append(dhcpEntries, dhcpEntry{srv: srv, tftpAddr: ifaceIPFromCIDR(vc.Address), baseScope: &base.VLANs[i].DHCP})
			applyDHCPOptions(srv, vc.DHCP, cfg.Services.PXE.Enabled, ifaceIPFromCIDR(vc.Address), cfg.Services.PXE.MacImages, cfg.Services.PXE)
			if mon != nil {
				vlanName := vc.Name
				if vlanName == "" {
					vlanName = fmt.Sprintf("vlan%d", vc.ID)
				}
				via := fmt.Sprintf("V%d", vc.ID)
				srv.OnLease(func(ip net.IP, mac string) {
					if err := mon.AddClient(ip, mac, via); err != nil {
						log.Printf("status: failed to add %s client %s: %v", vlanName, ip, err)
					}
				})
				srv.OnLeaseExpired(func(ip net.IP, mac string) {
					if err := mon.RemoveClient(ip); err != nil {
						log.Printf("status: failed to remove %s client %s: %v", vlanName, ip, err)
					}
				})
			}
			go func(id int) {
				if err := srv.Run(); err != nil {
					log.Fatalf("dhcp server (VLAN %d): %v", id, err)
				}
			}(vc.ID)
			log.Printf("dhcp: started on %s for VLAN %d (%s)", bridgeName, vc.ID, vc.Name)
			activeServices = append(activeServices, fmt.Sprintf("DHCP(VLAN%d)", vc.ID))
		}
	} else if cfg.LAN.DHCP.Enabled {
		srv, err := dhcp.New(
			cfg.LAN.Bridge,
			cfg.LAN.Address,
			cfg.LAN.DHCP.RangeStart,
			cfg.LAN.DHCP.RangeEnd,
			cfg.LAN.DHCP.DNS,
			cfg.LAN.DHCP.ParseLeaseDuration(),
		)
		if err != nil {
			log.Fatalf("dhcp: %v", err)
		}
		dhcpEntries = append(dhcpEntries, dhcpEntry{srv: srv, tftpAddr: ifaceIPFromCIDR(cfg.LAN.Address), baseScope: &base.LAN.DHCP})
		applyDHCPOptions(srv, cfg.LAN.DHCP, cfg.Services.PXE.Enabled, ifaceIPFromCIDR(cfg.LAN.Address), cfg.Services.PXE.MacImages, cfg.Services.PXE)
		if mon != nil {
			srv.OnLease(func(ip net.IP, mac string) {
				if err := mon.AddClient(ip, mac, "L"); err != nil {
					log.Printf("status: failed to add client %s: %v", ip, err)
				}
			})
			srv.OnLeaseExpired(func(ip net.IP, mac string) {
				if err := mon.RemoveClient(ip); err != nil {
					log.Printf("status: failed to remove client %s: %v", ip, err)
				}
			})
		}
		go func() {
			if err := srv.Run(); err != nil {
				log.Fatalf("dhcp server (LAN): %v", err)
			}
		}()
		activeServices = append(activeServices, "DHCP(LAN)")
	}

	// 7. Start DHCP server on WiFi interface (routed mode only, not in VLAN mode).
	if wifiAP != nil && cfg.WiFi.Address != "" && cfg.WiFi.DHCP.Enabled && wifiAP.MacMap() == nil {
		srv, err := dhcp.New(
			wifiIface,
			cfg.WiFi.Address,
			cfg.WiFi.DHCP.RangeStart,
			cfg.WiFi.DHCP.RangeEnd,
			cfg.WiFi.DHCP.DNS,
			cfg.WiFi.DHCP.ParseLeaseDuration(),
		)
		if err != nil {
			log.Fatalf("dhcp wifi: %v", err)
		}
		dhcpEntries = append(dhcpEntries, dhcpEntry{srv: srv, tftpAddr: ifaceIPFromCIDR(cfg.WiFi.Address), baseScope: &base.WiFi.DHCP})
		applyDHCPOptions(srv, cfg.WiFi.DHCP, cfg.Services.PXE.Enabled, ifaceIPFromCIDR(cfg.WiFi.Address), cfg.Services.PXE.MacImages, cfg.Services.PXE)
		if mon != nil {
			srv.OnLease(func(ip net.IP, mac string) {
				if err := mon.AddClient(ip, mac, "W"); err != nil {
					log.Printf("status: failed to add WiFi client %s: %v", ip, err)
				}
			})
			srv.OnLeaseExpired(func(ip net.IP, mac string) {
				if err := mon.RemoveClient(ip); err != nil {
					log.Printf("status: failed to remove WiFi client %s: %v", ip, err)
				}
			})
		}
		go func() {
			if err := srv.Run(); err != nil {
				log.Fatalf("dhcp server (WiFi): %v", err)
			}
		}()
		activeServices = append(activeServices, "DHCP(WiFi)")
	}

	// 8. Start optional SMB server.
	var smbServer *smb.Server
	if cfg.Services.SMB.Enabled {
		smbServer = smb.New(cfg.Services.SMB)
		if extras != nil {
			smbServer.SetExtraUsers(extras.SMBUsers)
		}
		if err := smbServer.Start(); err != nil {
			log.Printf("smb: %v (continuing without SMB)", err)
		} else {
			activeServices = append(activeServices, fmt.Sprintf("SMB(%s@%s)", cfg.Services.SMB.ShareName, cfg.Services.SMB.Listen))
		}
	}

	// 9. Start optional PXE/TFTP server.
	// The listener is left unbound to the local interfaces by default so it can
	// serve every DHCP scope that advertises option 66/67 (all VLAN bridges and
	// the Wi-Fi subnet). Kernel routing sends replies out the correct bridge,
	// since each scope is its own subnet. Set services.pxe.bindInterface to pin
	// the listener to a single interface if a deployment requires it.
	var pxeSrv *pxe.Server
	if cfg.Services.PXE.Enabled {
		pxeSrv = pxe.New(cfg.Services.PXE)
		go func() {
			if err := pxeSrv.Start(); err != nil {
				log.Printf("pxe: %v", err)
			}
		}()
		activeServices = append(activeServices, fmt.Sprintf("PXE/TFTP(%s)", cfg.Services.PXE.Listen))
	}

	// 10. Start administration HTTP API.
	rel := &reloader{
		cfg:             cfg,
		base:            base,
		extrasPath:      cfg.Services.ExtrasFile,
		dhcpEntries:     dhcpEntries,
		pxeSrv:          pxeSrv,
		appliedSMBUsers: extrasSMBUsers(extras),
	}

	if mon != nil {
		http.Handle("/status", mon)
	}
	http.HandleFunc("/api/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		if !cfg.Services.AdminAPI.AllowUnauthenticatedReload {
			tok := cfg.Services.AdminAPI.Token
			if tok == "" {
				http.Error(w, "reload disabled: set services.adminAPI.token or services.adminAPI.allowUnauthenticatedReload", http.StatusUnauthorized)
				return
			}
			expected := []byte("Bearer " + tok)
			got := []byte(r.Header.Get("Authorization"))
			if len(got) != len(expected) || subtle.ConstantTimeCompare(got, expected) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if err := rel.Reload(); err != nil {
			log.Printf("reload: %v", err)
			var restartErr *restartRequiredError
			if errors.As(err, &restartErr) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	go func() {
		addr := cfg.Services.AdminAPI.Listen
		if addr == "" {
			addr = "127.0.0.1:8080"
		}
		log.Printf("HTTP API listening on %s", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Printf("HTTP API: %v", err)
		}
	}()
	adminAddr := cfg.Services.AdminAPI.Listen
	if adminAddr == "" {
		adminAddr = "127.0.0.1:8080"
	}
	activeServices = append(activeServices, "HTTP:"+adminAddr)

	if len(activeServices) > 0 {
		log.Printf("gokrazy-router running with active services: %s", strings.Join(activeServices, ", "))
	} else {
		log.Printf("gokrazy-router running")
	}

	// Wait for shutdown signal.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	sig := <-ch
	log.Printf("received %v, shutting down", sig)

	// Cleanup.
	if smbServer != nil {
		_ = smbServer.Stop()
	}
	if wifiAP != nil {
		wifiAP.Stop()
	}
	if mon != nil {
		mon.Stop()
	}
	if natMgr != nil {
		natMgr.Cleanup()
	}
}

// restartRequiredError reports an extras change that cannot be applied to the
// running daemon and requires a restart (e.g. VLAN address overrides).
type restartRequiredError struct {
	msg string
}

func (e *restartRequiredError) Error() string { return e.msg }

// reloader applies the extras file to the running services. Reloads are
// serialized: Config.ApplyExtras mutates the shared cfg, and concurrent
// requests would otherwise race on it.
type reloader struct {
	mu sync.Mutex

	cfg             *config.Config // live config, mutated by ApplyExtras
	base            *config.Config // immutable snapshot from router.json
	extrasPath      string
	dhcpEntries     []dhcpEntry
	pxeSrv          *pxe.Server
	appliedSMBUsers []config.SMBUser // user list smbd was started with
}

// Reload re-reads the extras file and applies it to DHCP, PXE and (where
// supported) SMB. If the extras file is invalid or unreadable, the reload
// silently falls back to the base JSON config so the router keeps running.
func (r *reloader) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.extrasPath == "" {
		return fmt.Errorf("no services.extrasFile configured")
	}
	extras, err := loadOrCreateExtras(r.extrasPath, r.cfg)
	if err != nil {
		log.Printf("reload: extras load failed (%v), falling back to base config", err)
		extras = &config.ExtrasConfig{}
	}

	if err := r.checkRestartRequired(extras); err != nil {
		return err
	}

	r.cfg.ApplyExtras(extras)

	for _, e := range r.dhcpEntries {
		// Effective reservations = base-JSON reservations for this scope
		// merged with the extras map. SetReservations filters by the server's
		// own subnet, so per-scope base reservations are preserved across
		// reloads instead of being replaced by the raw extras map.
		res := mergedReservations(e.baseScope.Reservations, extras.Reservations)
		e.srv.SetReservations(res)
		bs := ""
		if r.cfg.Services.PXE.Enabled {
			bs = e.tftpAddr
		}
		applyPXEBootOptions(e.srv, r.cfg.Services.PXE.Enabled, bs, r.cfg.Services.PXE.MacImages, r.cfg.Services.PXE)
	}

	if r.pxeSrv != nil {
		r.pxeSrv.SetMacImages(extras.MacImages)
		r.pxeSrv.SetDefaultImage(extras.DefaultImage)
	}

	log.Printf("reload: extras reloaded from %s", r.extrasPath)
	return nil
}

// checkRestartRequired rejects extras changes that cannot be applied to the
// running daemon safely.
func (r *reloader) checkRestartRequired(extras *config.ExtrasConfig) error {
	// VLAN address overrides change the bridge, DHCP server and NAT state.
	// Those are set up once at boot; applying an override at runtime would
	// leave the live network and DHCP server on the old addresses. Require a
	// restart instead.
	for id, addr := range extras.VLANAddresses {
		for i := range r.cfg.VLANs {
			if r.cfg.VLANs[i].ID != id {
				continue
			}
			if addr != r.cfg.VLANs[i].Address {
				return &restartRequiredError{msg: fmt.Sprintf(
					"vlanAddresses[%d] changed from %s to %s: VLAN address changes require a router restart", r.cfg.VLANs[i].ID, r.cfg.VLANs[i].Address, addr)}
			}
			break
		}
	}
	// SMB user-list changes cannot be applied at runtime: valid users and the
	// password database are baked into smb.conf when smbd starts.
	if !equalSMBUsers(r.appliedSMBUsers, extras.SMBUsers) {
		return &restartRequiredError{msg: "smbUsers changed: SMB user changes require a router restart"}
	}
	return nil
}

// mergedReservations returns base merged with extras; extras win on conflict.
func mergedReservations(base, extras map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extras))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extras {
		out[k] = v
	}
	return out
}

// extrasSMBUsers returns a stable copy of the extras SMB user list.
func extrasSMBUsers(extras *config.ExtrasConfig) []config.SMBUser {
	if extras == nil {
		return nil
	}
	return append([]config.SMBUser(nil), extras.SMBUsers...)
}

func equalSMBUsers(a, b []config.SMBUser) bool {
	return smbUsersKey(a) == smbUsersKey(b)
}

func smbUsersKey(users []config.SMBUser) string {
	parts := make([]string, len(users))
	for i, u := range users {
		parts[i] = u.Name + ":" + u.Password
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

// cloneConfig deep-copies the maps that ApplyExtras mutates, so the base
// snapshot stays pristine for later reloads.
func cloneConfig(c *config.Config) *config.Config {
	if c == nil {
		return nil
	}
	out := *c
	out.LAN.DHCP.Reservations = cloneStringMap(c.LAN.DHCP.Reservations)
	out.WiFi.DHCP.Reservations = cloneStringMap(c.WiFi.DHCP.Reservations)
	out.VLANs = make([]config.VLANConfig, len(c.VLANs))
	for i := range c.VLANs {
		out.VLANs[i] = c.VLANs[i]
		out.VLANs[i].DHCP.Reservations = cloneStringMap(c.VLANs[i].DHCP.Reservations)
	}
	out.Services.PXE.MacImages = cloneStringMap(c.Services.PXE.MacImages)
	return &out
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// loadOrCreateExtras loads the runtime extras file, creating a minimal
// initial version from the JSON config when the file does not exist yet.
func loadOrCreateExtras(path string, cfg *config.Config) (*config.ExtrasConfig, error) {
	extras, err := config.LoadExtras(path)
	if err == nil {
		return extras, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	extras = config.ExtrasFromConfig(cfg)
	if err := extras.Save(path); err != nil {
		return nil, fmt.Errorf("create extras: %w", err)
	}
	log.Printf("extras: created %s from config", path)
	return extras, nil
}

func applyDHCPOptions(srv *dhcp.Server, dhcpcfg config.DHCPConfig, pxeEnabled bool, tftpServer string, macImages map[string]string, pxeCfg config.PXEConfig) {
	if len(dhcpcfg.Reservations) > 0 {
		srv.SetReservations(dhcpcfg.Reservations)
	}
	if !pxeEnabled {
		return
	}
	bootServer := tftpServer
	if dhcpcfg.PXEBootServer != "" {
		bootServer = dhcpcfg.PXEBootServer
	}
	scopePXE := pxeCfg
	if dhcpcfg.PXEBootFile != "" {
		scopePXE.BootFile = dhcpcfg.PXEBootFile
	}
	applyPXEBootOptions(srv, true, bootServer, macImages, scopePXE)
}

func applyPXEBootOptions(srv *dhcp.Server, pxeEnabled bool, tftpServer string, macImages map[string]string, pxeCfg config.PXEConfig) {
	if !pxeEnabled {
		return
	}

	bootFile := pxeCfg.BootFile
	if bootFile == "" && pxeCfg.DefaultImage != "" {
		bootFile = pxeCfg.DefaultImage
	}
	if bootFile == "" {
		bootFile = "netboot.xyz.efi"
	}
	legacy := pxeCfg.LegacyBootFile
	if legacy == "" {
		legacy = bootFile
	}
	uefi := pxeCfg.UEFIBootFile
	if uefi == "" {
		uefi = "netboot.xyz.efi"
	}
	ipxe := pxeCfg.IPXEScript
	if ipxe == "" {
		ipxe = "boot.ipxe"
	}

	if ipxe == bootFile || ipxe == legacy || ipxe == uefi {
		log.Printf("pxe: warning: ipxe script %q is identical to another boot file; iPXE clients will boot-loop", ipxe)
	}

	if tftpServer != "" || bootFile != "" {
		srv.SetPXEOptions(tftpServer, bootFile)
	}
	srv.SetLegacyBootFile(legacy)
	srv.SetUEFIBootFile(uefi)
	srv.SetIPXEBootFile(ipxe)
	// Always replace: an empty map must clear per-MAC overrides removed from
	// the extras file at runtime.
	srv.SetMacPXEBootFiles(macImages)
}

func ifaceIPFromCIDR(cidr string) string {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil || ip.To4() == nil {
		return ""
	}
	return ip.To4().String()
}

// assignIP assigns a CIDR address to the named interface and brings it up.
func assignIP(ifaceName, cidr string) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return err
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	return netlink.LinkSetUp(link)
}

// enableProxyARP enables proxy ARP on the named interface via sysctl.
// This allows the router to answer ARP requests on behalf of hosts
// reachable via a different interface on the same subnet.
func enableProxyARP(ifaceName string) {
	path := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/proxy_arp", ifaceName)
	if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
		log.Printf("proxy_arp: failed to enable on %s: %v", ifaceName, err)
	}
}

// waitForInterface polls until the named network interface exists.
func waitForInterface(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := netlink.LinkByName(name); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("interface %s did not appear within %v", name, timeout)
}

// wifiStationAdapter adapts wifi.AP to status.WiFiStationSource.
type wifiStationAdapter struct {
	ap *wifi.AP
}

func (a *wifiStationAdapter) StationInfoAll() ([]status.WiFiStation, error) {
	stations, err := a.ap.StationInfoAll()
	if err != nil {
		return nil, err
	}
	result := make([]status.WiFiStation, len(stations))
	for i, s := range stations {
		result[i] = status.WiFiStation{
			MAC:       s.MAC,
			Signal:    s.Signal,
			TxBitrate: s.TxBitrate,
			RxBitrate: s.RxBitrate,
		}
	}
	return result, nil
}
