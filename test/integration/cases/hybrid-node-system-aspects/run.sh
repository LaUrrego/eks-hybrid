#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

source /helpers.sh

mock::aws
mock::kubelet 1.30.0
wait::dbus-ready

# install, enable and start firewalld to test ports aspect
dnf install -y firewalld
systemctl enable firewalld
systemctl start firewalld

nodeadm init --skip run,install-validation --config-source file://config.yaml

# Check if aws config file has been created as specifed in NodeConfig
assert::files-equal /.aws/config expected-aws-config

# Check if sysctl aspect has been setup
assert::files-equal /etc/sysctl.d/99-nodeadm.conf expected-99-nodeadm.conf

# Check if swap has been disabled and partition removed from /etc/fstab
assert::file-not-contains /etc/fstab "swap"
assert::swap-disabled-validate-path

# Check if port has been allowed by firewalld
assert::allowed-by-firewalld "10250" "tcp"
assert::allowed-by-firewalld "10256" "tcp"
assert::allowed-by-firewalld "30000-32767" "tcp"
