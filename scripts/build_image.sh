#!/bin/bash
set -euo pipefail

IMAGE_NAME="artifacts/ubuntu-22.04-goose.ext4"
IMAGE_SIZE="2G"
MNT_DIR="/tmp/goose-rootfs"

# Ensure the destination directory exists before creating the image
mkdir -p $(dirname $IMAGE_NAME)

echo "==> 1. Creating and formatting empty image <=="
fallocate -l $IMAGE_SIZE $IMAGE_NAME
mkfs.ext4 -F $IMAGE_NAME

echo "==> 2. Mounting and running debootstrap <=="
mkdir -p $MNT_DIR
sudo mount $IMAGE_NAME $MNT_DIR
sudo debootstrap jammy $MNT_DIR http://archive.ubuntu.com/ubuntu/

echo "==> 3. Configuring Guest OS (Non-interactive chroot) <=="
# Passing the script to chroot via Here-Doc for full automation
sudo chroot $MNT_DIR /bin/bash << 'EOF'
  export DEBIAN_FRONTEND=noninteractive
  apt update
  apt install -y systemd systemd-sysv openssh-server curl wget libgomp1 bzip2
  
  # Downloading Goose binary from the new official AAIF repository
  curl -L -o /tmp/goose.tar.bz2 "https://github.com/aaif-goose/goose/releases/download/stable/goose-x86_64-unknown-linux-gnu.tar.bz2"
  
  # Extracting and setting up the binary
  tar -xjf /tmp/goose.tar.bz2 -C /usr/local/bin/
  chmod +x /usr/local/bin/goose
  rm /tmp/goose.tar.bz2

  # Network configuration (DHCP via systemd-networkd)
  cat <<NET > /etc/systemd/network/10-microvm.network
[Match]
Name=eth0
[Network]
DHCP=ipv4
NET
  systemctl enable systemd-networkd

  # Allowing root login and enforcing key-based authentication
  sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin without-password/' /etc/ssh/sshd_config
  mkdir -p /root/.ssh
  chmod 700 /root/.ssh
  
  apt clean
  rm -rf /var/lib/apt/lists/*
EOF

echo "==> 4. Cleaning up and unmounting <=="
sudo umount $MNT_DIR
echo "==> Golden image successfully created: $IMAGE_NAME <=="