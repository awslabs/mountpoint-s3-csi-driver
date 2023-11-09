#!/bin/sh

set -euox pipefail

NSENTER_HOST="nsenter --target 1 --mount --net"
CSI_DIR="/csi/"
HOST_CSI_DIR="/var/lib/kubelet/plugins/s3.csi.aws.com/"
RPM_FILE=mount-s3.rpm
DEB_FILE=mount-s3.deb

get_os_info() {
    local key=$1
    local value=$($NSENTER_HOST cat /etc/os-release | grep "^$key=" | cut -d= -f2-)
    # Remove potential quotes around the value
    echo ${value//\"/}
}

# Determine the package manager based on the ID_LIKE or ID from os-release
determine_package_manager() {
    local id_like=$(get_os_info ID_LIKE)
    local id=$(get_os_info ID)

    if [[ "$id_like" == *"debian"* || "$id" == "debian" || "$id" == "ubuntu" ]]; then
        echo "apt"
    elif [[ "$id_like" == *"fedora"* || "$id_like" == *"rhel"* || "$id" == "fedora" || "$id" == "centos" ]]; then
        echo "yum"
    else
        echo "unknown"
    fi
}

cleanup_rpm() {
    rm -f "${CSI_DIR}${RPM_FILE}"
}

cleanup_deb() {
    rm -f "${CSI_DIR}${DEB_FILE}"
}

install_mountpoint_rpm() {
    echo "Using yum to install S3 Mountpoint..."
    local rpm_package_name=$(rpm -qp --queryformat '%{NAME}\n' "/${RPM_FILE}")
    local installed_mp_version=$($NSENTER_HOST rpm -q --queryformat '%{VERSION}-%{RELEASE}\n' "${rpm_package_name}" || true)
    local package_mp_version=$(rpm -qp --queryformat '%{VERSION}-%{RELEASE}\n' "/${RPM_FILE}")
    echo "Installed S3 Mountpoint version: ${installed_mp_version}"
    echo "Package S3 Mountpoint version: ${package_mp_version}"

    if [[ "${installed_mp_version}" != "${package_mp_version}" ]]; then
        cp "/${RPM_FILE}" "${CSI_DIR}${RPM_FILE}"
        trap cleanup_rpm EXIT SIGINT SIGTERM
        # If install fails try downgrade
        $NSENTER_HOST yum install -y "${HOST_CSI_DIR}${RPM_FILE}" || \
            $NSENTER_HOST yum downgrade -y "${HOST_CSI_DIR}${RPM_FILE}"
    else
        echo "S3 Mountpoint already up to date"
    fi
}

install_mountpoint_deb() {
    echo "Using apt to install S3 Mountpoint..."
    $NSENTER_HOST apt-get update
    cp "/${DEB_FILE}" "${CSI_DIR}${DEB_FILE}"
    trap cleanup_deb EXIT SIGINT SIGTERM
    $NSENTER_HOST apt-get install -y --allow-downgrades "${HOST_CSI_DIR}${DEB_FILE}"
}

package_manager=$(determine_package_manager)

if [ "$package_manager" == "apt" ]; then
    install_mountpoint_deb
elif [ "$package_manager" == "yum" ]; then
    install_mountpoint_rpm
else
    echo "Package manager not supported or not detected."
    exit 1
fi
