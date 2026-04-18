// Package wifi manages the hostapd subprocess for WiFi AP mode.
//
// It generates a hostapd.conf from the configuration, starts hostapd
// as a supervised child process, and restarts it on unexpected exit.
// The WiFi interface is expected to be bridged into br-lan (or a VLAN
// bridge) by the netsetup package.
package wifi
