#!/bin/sh

NSENTER_HOST="nsenter --target 1 --mount --uts --ipc --net"

cp /mount-s3-wrap /host/usr/bin/mount-s3-wrap

echo 'Installing mountpoint'
cp /mount-s3.rpm /host/usr/mount-s3.rpm
$NSENTER_HOST yum install -y /usr/mount-s3.rpm
$NSENTER_HOST rm -f /usr/mount-s3.rpm
$NSENTER_HOST mkdir -p /var/run/mountpoint-s3