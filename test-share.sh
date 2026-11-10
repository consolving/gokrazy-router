#!/usr/bin/env bash
# test-share.sh — quick check that the router's SMB share and NFS export are
# serving files: lists the file names via smbclient or a temporary mount.
#
# Client tools are assumed to be installed. The script picks what is available
# and skips a test with a hint when its client is missing:
#   SMB: smbclient            (Linux/macOS, brew install smbclient)
#        mount_smbfs          (macOS)
#        mount.cifs           (Linux, needs root)
#   NFS: mount_nfs            (macOS)
#        mount -t nfs         (Linux, needs root)
#
# Usage:
#   test-share.sh [smb|nfs|all]           default: all
#
# Configuration via environment variables:
#   SHARE_HOST   router address      (default 192.168.168.134)
#   SMB_USER     SMB user            (default gokrazy)
#   SMB_PASSWORD SMB password        (required for SMB; value from the
#                                     gokrazy instance config SMB_PASSWORD)
#   SMB_SHARE    SMB share name      (default sata)
#   NFS_EXPORT   NFS export path     (default /mnt/sata/netboot)
#
# Example:
#   SMB_PASSWORD=... ./test-share.sh
#   ./test-share.sh nfs
set -u

HOST="${SHARE_HOST:-192.168.168.134}"
SMB_USER="${SMB_USER:-gokrazy}"
SMB_PASSWORD="${SMB_PASSWORD:-}"
SMB_SHARE="${SMB_SHARE:-sata}"
NFS_EXPORT="${NFS_EXPORT:-/mnt/sata/netboot}"

MODE="${1:-all}"
case "$MODE" in
	smb|nfs|all) ;;
	*) echo "usage: $0 [smb|nfs|all]" >&2; exit 2 ;;
esac

TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

mount_dir() { mkdir -p "$TMP/$1"; }

unmount_dir() {
	# macOS first, Linux second.
	umount "$TMP/$1" 2>/dev/null || umount "$TMP/$1" 2>/dev/null || \
		umount -f "$TMP/$1" 2>/dev/null || true
}

test_smb() {
	echo "== SMB: //$HOST/$SMB_SHARE (user $SMB_USER)"
	if [ -z "$SMB_PASSWORD" ]; then
		echo "   SKIPPED: set SMB_PASSWORD to test SMB" >&2
		return 1
	fi

	if command -v smbclient >/dev/null 2>&1; then
		# No mount needed: smbclient lists the share directly.
		smbclient //"$HOST"/"$SMB_SHARE" -U "$SMB_USER"%"$SMB_PASSWORD" \
			-c 'ls' && return 0
		echo "   smbclient failed" >&2
		return 1
	fi

	if command -v mount_smbfs >/dev/null 2>&1; then
		mount_dir smb
		if mount_smbfs "//${SMB_USER}:${SMB_PASSWORD}@${HOST}/${SMB_SHARE}" "$TMP/smb"; then
			ls -la "$TMP/smb"
			unmount_dir smb
			return 0
		fi
		echo "   mount_smbfs failed" >&2
		return 1
	fi

	if command -v mount.cifs >/dev/null 2>&1; then
		mount_dir smb
		if mount.cifs //"$HOST"/"$SMB_SHARE" "$TMP/smb" \
			-o "user=$SMB_USER,pass=$SMB_PASSWORD,vers=3.0"; then
			ls -la "$TMP/smb"
			unmount_dir smb
			return 0
		fi
		echo "   mount.cifs failed (root?)" >&2
		return 1
	fi

	echo "   SKIPPED: no SMB client found (install smbclient)" >&2
	return 1
}

test_nfs() {
	echo "== NFS: $HOST:$NFS_EXPORT"
	# Sanity: the export must be advertised by mountd.
	if command -v showmount >/dev/null 2>&1; then
		if ! showmount -e "$HOST" 2>/dev/null | grep -q "$NFS_EXPORT"; then
			echo "   export $NFS_EXPORT not in mountd export list for $HOST:" >&2
			showmount -e "$HOST" 2>/dev/null | sed 's/^/   /' >&2
			return 1
		fi
	fi

	# unfs3 has no statd/lock manager: mount with local locking.
	if command -v mount_nfs >/dev/null 2>&1; then
		mount_dir nfs
		if mount_nfs -o ro,locallocks,vers=3,mountport=20048,port=2049 \
			"$HOST:$NFS_EXPORT" "$TMP/nfs"; then
			ls -la "$TMP/nfs"
			unmount_dir nfs
			return 0
		fi
		echo "   mount_nfs failed; if the error is 'Permission denied', this" >&2
		echo "   host is outside the export ACL (see the showmount output above)" >&2
		return 1
	fi

	if [ -x /sbin/mount.nfs ] || command -v mount.nfs >/dev/null 2>&1; then
		mount_dir nfs
		if mount.nfs -o "ro,nolock,vers=3,mountport=20048,port=2049" \
			"$HOST:$NFS_EXPORT" "$TMP/nfs"; then
			ls -la "$TMP/nfs"
			unmount_dir nfs
			return 0
		fi
		echo "   mount.nfs failed; if the error is 'Permission denied', this" >&2
		echo "   host is outside the export ACL (see the showmount output above)" >&2
		return 1
	fi

	if command -v mount >/dev/null 2>&1; then
		# Generic fallback (busybox etc.); needs root for the nfs helper.
		mount_dir nfs
		if mount -t nfs -o "ro,nolock,vers=3,mountport=20048,port=2049" \
			"$HOST:$NFS_EXPORT" "$TMP/nfs" 2>/dev/null; then
			ls -la "$TMP/nfs"
			unmount_dir nfs
			return 0
		fi
		echo "   mount -t nfs failed; 'Permission denied' = host outside the" >&2
		echo "   export ACL (see the showmount output above)" >&2
		return 1
	fi

	echo "   SKIPPED: no NFS client found" >&2
	return 1
}

rc=0
case "$MODE" in
	smb) test_smb || rc=1 ;;
	nfs) test_nfs || rc=1 ;;
	all)
		test_smb || rc=1
		test_nfs || rc=1
		;;
esac
exit $rc
