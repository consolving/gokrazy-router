package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/consolving/gokrazy-router/pkg/dhcp"
	"golang.org/x/sys/unix"
)

// bootLog is a logger that writes to /boot for post-mortem debugging.
var bootLog *log.Logger
var bootLogMu sync.Mutex

func initBootLog() {
	f, err := os.OpenFile("/boot/milkv-usbnet.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		// boot partition may not be mounted yet; try again later
		return
	}
	bootLog = log.New(f, "", log.Ldate|log.Ltime|log.Lmsgprefix)
	bootLog.SetPrefix("milkv-boot: ")
}

func bootPrintf(format string, args ...interface{}) {
	bootLogMu.Lock()
	defer bootLogMu.Unlock()
	if bootLog != nil {
		bootLog.Printf(format, args...)
	}
}

const (
	gadgetPath   = "/sys/kernel/config/usb_gadget/gokrazy"
	vid          = "0x3346"
	pid          = "0x100C"
	manufacturer = "gokrazy"
	product      = "Milk-V Duo USB Ethernet"
	serial       = "0000000001"
)

// dw-apb-gpio register offsets
const (
	gpioSWPortData = 0x00 // data register (read: pin state, write: output value)
	gpioSWPortDDR  = 0x04 // direction register (0=input, 1=output)
)

func mmapReg(base uintptr, size int) ([]byte, error) {
	mem, err := os.OpenFile("/dev/mem", os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/mem: %w", err)
	}
	defer mem.Close()

	data, err := unix.Mmap(int(mem.Fd()), int64(base), size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap 0x%x: %w", base, err)
	}
	return data, nil
}

func gpioSetOutput(regs []byte, offset uint) {
	// Set direction bit: 1 = output
	dir := binary.LittleEndian.Uint32(regs[gpioSWPortDDR:])
	dir |= 1 << offset
	binary.LittleEndian.PutUint32(regs[gpioSWPortDDR:], dir)
}

func gpioSetValue(regs []byte, offset uint, val bool) {
	data := binary.LittleEndian.Uint32(regs[gpioSWPortData:])
	if val {
		data |= 1 << offset
	} else {
		data &^= 1 << offset
	}
	binary.LittleEndian.PutUint32(regs[gpioSWPortData:], data)
}

var gpioRegions []func()

func gpioCleanup() {
	for _, f := range gpioRegions {
		f()
	}
}

// blue LED: XGPIOC[24] at GPIO port C, base 0x3022000
const (
	portcBase = 0x3022000
	ledBit    = 24
)

var ledRegs []byte // mapped register region for LED

func initLED() bool {
	regs, err := mmapReg(portcBase, 4096)
	if err != nil {
		log.Printf("led: mmap portc: %v", err)
		bootPrintf("FAIL mmap portc: %v", err)
		return false
	}
	gpioRegions = append(gpioRegions, func() { unix.Munmap(regs) })
	ledRegs = regs
	gpioSetOutput(regs, ledBit)
	log.Printf("led: portc mapped and direction set")
	bootPrintf("OK portc mapped")
	return true
}

func ledOn() {
	if ledRegs == nil {
		return
	}
	gpioSetValue(ledRegs, ledBit, true)
}

func ledOff() {
	if ledRegs == nil {
		return
	}
	gpioSetValue(ledRegs, ledBit, false)
}

func ledBlink(n int) {
	for i := 0; i < n; i++ {
		ledOn()
		time.Sleep(150 * time.Millisecond)
		ledOff()
		time.Sleep(150 * time.Millisecond)
	}
}

func ledLongBlink(n int) {
	for i := 0; i < n; i++ {
		ledOn()
		time.Sleep(600 * time.Millisecond)
		ledOff()
		time.Sleep(300 * time.Millisecond)
	}
}

func setupUSBHub() bool {
	// USB hub GPIOs on XGPIOB (port B, base 0x3021000)
	// offset 1 = HUBPORT_EN, offset 2 = ROLESEL, offset 3 = HUBRST
	const portbBase = 0x3021000

	regs, err := mmapReg(portbBase, 4096)
	if err != nil {
		log.Printf("usb: mmap portb: %v", err)
		bootPrintf("FAIL mmap portb: %v", err)
		return false
	}
	gpioRegions = append(gpioRegions, func() { unix.Munmap(regs) })
	log.Printf("usb: portb mapped")
	bootPrintf("OK portb mapped")

	gpioSetOutput(regs, 1)
	gpioSetOutput(regs, 2)
	gpioSetOutput(regs, 3)
	log.Printf("usb: hub GPIOs set to output")
	bootPrintf("OK portb outputs set")

	gpioSetValue(regs, 1, true)  // HUBPORT_EN = 1 (port power on)
	gpioSetValue(regs, 2, false) // ROLESEL = 0 (device mode)
	gpioSetValue(regs, 3, false) // HUBRST = 0 (release reset)
	log.Printf("usb: hub GPIOs initialized for device mode")
	bootPrintf("OK hub device mode")

	// Also try vendor-specific OTG role switch
	if err := os.WriteFile("/proc/cviusb/otg_role", []byte("device\n"), 0644); err != nil {
		log.Printf("usb: /proc/cviusb/otg_role: %v (expected on newer kernels)", err)
		bootPrintf("WARN /proc/cviusb/otg_role: %v", err)
	} else {
		bootPrintf("OK otg_role=device")
	}
	return true
}

func findUDC() (string, error) {
	entries, err := os.ReadDir("/sys/class/udc")
	if err != nil {
		return "", fmt.Errorf("read /sys/class/udc: %w", err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no UDC available")
	}
	return entries[0].Name(), nil
}

func setDWC2Peripheral() {
	matches, err := filepath.Glob("/sys/devices/platform/*/dwc2*/mode")
	if err != nil || len(matches) == 0 {
		matches, err = filepath.Glob("/sys/devices/platform/soc/*/dwc2*/mode")
	}
	if err != nil || len(matches) == 0 {
		matches, err = filepath.Glob("/sys/devices/platform/*/usb/mode")
	}
	if len(matches) == 0 {
		log.Printf("usb: no DWC2 mode sysfs found, assuming correct mode")
		return
	}
	if err := os.WriteFile(matches[0], []byte("peripheral"), 0644); err != nil {
		log.Printf("usb: failed to set DWC2 peripheral mode: %v", err)
		return
	}
	log.Printf("usb: set DWC2 to peripheral mode via %s", matches[0])
}

func writeFile(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value), 0644)
}

func symlink(oldname, newname string) error {
	if _, err := os.Lstat(newname); err == nil {
		os.Remove(newname)
	}
	return os.Symlink(oldname, newname)
}

func setupUSBGadget() error {
	if _, err := os.Stat(filepath.Join(gadgetPath, "UDC")); err == nil {
		if data, err := os.ReadFile(filepath.Join(gadgetPath, "UDC")); err == nil && strings.TrimSpace(string(data)) != "" {
			log.Printf("usb: gadget already active on %s", strings.TrimSpace(string(data)))
			return nil
		}
	}

	if err := os.MkdirAll(gadgetPath, 0755); err != nil {
		return fmt.Errorf("mkdir gadget: %w", err)
	}

	if err := writeFile(filepath.Join(gadgetPath, "idVendor"), vid+"\n"); err != nil {
		return fmt.Errorf("idVendor: %w", err)
	}
	if err := writeFile(filepath.Join(gadgetPath, "idProduct"), pid+"\n"); err != nil {
		return fmt.Errorf("idProduct: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(gadgetPath, "strings/0x409"), 0755); err != nil {
		return fmt.Errorf("mkdir strings: %w", err)
	}
	if err := writeFile(filepath.Join(gadgetPath, "strings/0x409/manufacturer"), manufacturer+"\n"); err != nil {
		return fmt.Errorf("manufacturer: %w", err)
	}
	if err := writeFile(filepath.Join(gadgetPath, "strings/0x409/product"), product+"\n"); err != nil {
		return fmt.Errorf("product: %w", err)
	}
	if err := writeFile(filepath.Join(gadgetPath, "strings/0x409/serialnumber"), serial+"\n"); err != nil {
		return fmt.Errorf("serial: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(gadgetPath, "functions/ecm.usb0"), 0755); err != nil {
		return fmt.Errorf("mkdir ecm function: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(gadgetPath, "configs/c.1"), 0755); err != nil {
		return fmt.Errorf("mkdir config: %w", err)
	}
	os.MkdirAll(filepath.Join(gadgetPath, "configs/c.1/strings/0x409"), 0755)
	if err := writeFile(filepath.Join(gadgetPath, "configs/c.1/strings/0x409/configuration"), "ECM\n"); err != nil {
		return fmt.Errorf("config string: %w", err)
	}
	if err := writeFile(filepath.Join(gadgetPath, "configs/c.1/MaxPower"), "120\n"); err != nil {
		return fmt.Errorf("maxpower: %w", err)
	}

	src := filepath.Join(gadgetPath, "functions/ecm.usb0")
	dst := filepath.Join(gadgetPath, "configs/c.1")
	if err := symlink(src, filepath.Join(dst, "ecm.usb0")); err != nil {
		return fmt.Errorf("symlink ecm: %w", err)
	}

	udc, err := findUDC()
	if err != nil {
		return fmt.Errorf("find UDC: %w", err)
	}
	log.Printf("usb: binding gadget to UDC %s", udc)
	if err := writeFile(filepath.Join(gadgetPath, "UDC"), udc+"\n"); err != nil {
		return fmt.Errorf("write UDC: %w", err)
	}
	return nil
}

func waitForInterface(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := net.InterfaceByName(name); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("interface %q did not appear within %v", name, timeout)
}

func bringUpInterface(iface, cidr string) error {
	if err := exec.Command("ip", "addr", "add", cidr, "dev", iface).Run(); err != nil {
		return fmt.Errorf("ip addr add: %w", err)
	}
	if err := exec.Command("ip", "link", "set", iface, "up").Run(); err != nil {
		return fmt.Errorf("ip link set up: %w", err)
	}
	return nil
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("milkv-usbnet: ")

	log.Printf("starting Milk-V Duo USB networking")

	initBootLog()
	bootPrintf("=== BOOT START ===")

	time.Sleep(3 * time.Second)

	// Boot progress encoded via blue LED blinks:
	//   3 quick = milkv-usbnet started
	//  +5 quick = LED GPIO mmap OK
	//  +1 long  = USB hub init OK
	//  +2 long  = gadget setup OK
	//  +3 long  = interface up, DHCP running
	//  solid on = all good
	// If LED stops at any stage, that's where boot failed.

	ledBlink(3)
	bootPrintf("BLINK stage1: milkv-usbnet running")

	if !initLED() {
		bootPrintf("FATAL: LED init failed, giving up on LED")
	} else {
		ledBlink(5)
		bootPrintf("BLINK stage2: LED GPIO mmap OK")
	}

	if setupUSBHub() {
		ledLongBlink(1)
		bootPrintf("BLINK stage3: USB hub OK")
	} else {
		bootPrintf("WARN: USB hub init failed, continuing anyway")
	}

	setDWC2Peripheral()
	bootPrintf("OK DWC2 mode set")

	if err := setupUSBGadget(); err != nil {
		log.Printf("USB gadget: %v", err)
		bootPrintf("FATAL gadget: %v", err)
		log.Fatalf("USB gadget: %v", err)
	}
	ledLongBlink(2)
	bootPrintf("BLINK stage4: gadget OK")

	log.Printf("waiting for usb0 interface")
	if err := waitForInterface("usb0", 10*time.Second); err != nil {
		log.Printf("interface: %v", err)
		bootPrintf("FATAL interface: %v", err)
		log.Fatalf("interface: %v", err)
	}
	bootPrintf("OK usb0 detected")

	log.Printf("bringing up usb0 with 192.168.42.1/24")
	if err := bringUpInterface("usb0", "192.168.42.1/24"); err != nil {
		log.Printf("network: %v", err)
		bootPrintf("FATAL network: %v", err)
		log.Fatalf("network: %v", err)
	}
	bootPrintf("OK usb0 configured")

	log.Printf("starting DHCP server on usb0 (range 192.168.42.2-242)")
	srv, err := dhcp.New("usb0", "192.168.42.1/24",
		"192.168.42.2", "192.168.42.242",
		[]string{"1.1.1.1", "8.8.8.8"},
		1*time.Hour)
	if err != nil {
		log.Fatal(err)
	}

	ledLongBlink(3)
	ledOn()
	bootPrintf("BLINK stage5: ready, LED solid on")
	bootPrintf("=== BOOT COMPLETE ===")

	log.Fatal(srv.Run())
}
