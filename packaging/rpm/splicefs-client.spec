# rpmbuild only wraps files here. scripts/package-rpm.sh passes in where the compiled
# program (stage), the service unit (unit) and the source tree (src) are.
Name:           splicefs-client
Version:        %{pkg_version}
Release:        1
Summary:        Client of the SpliceFS read-only multi-source filesystem
License:        MIT
URL:            https://github.com/SpliceFS/client
Requires:       fuse3

# The binary is static and already stripped, and it may be for another CPU type than the
# one building the package, so the usual post-processing has nothing it could do right.
%global debug_package %{nil}
%global __os_install_post %{nil}
%global _build_id_links none
# gzip is the payload format every rpm version in use can read.
%global _binary_payload w9.gzdio

%description
splicefs mounts a read-only folder whose files are streamed from several
SpliceFS servers at once. Every server that holds a copy of a file serves
blocks of it, and the client puts them back together and caches them on local
disk.

%install
install -D -m 755 %{stage}/splicefs %{buildroot}/usr/bin/splicefs
install -D -m 644 %{unit} %{buildroot}/usr/lib/systemd/system/splicefs.service
# mount(8) runs this for "mount -t splicefs" and for fstab lines of that type.
install -d %{buildroot}/usr/sbin
ln -s /usr/bin/splicefs %{buildroot}/usr/sbin/mount.splicefs
install -d %{buildroot}/etc/splicefs/client/conf.d %{buildroot}/etc/splicefs/client/clusters.d \
    %{buildroot}/etc/splicefs/client/mounts.d
install -D -m 644 %{src}/LICENSE %{buildroot}/usr/share/doc/splicefs-client/LICENSE
install -m 644 %{src}/README.md %{src}/CHANGELOG.md %{src}/docs/configuration.md \
    %{buildroot}/usr/share/doc/splicefs-client/

%files
/usr/bin/splicefs
/usr/sbin/mount.splicefs
/usr/lib/systemd/system/splicefs.service
%dir /etc/splicefs
%dir /etc/splicefs/client
# Empty on purpose: the administrator's files go here and no package file ever does.
%dir /etc/splicefs/client/conf.d
%dir /etc/splicefs/client/clusters.d
%dir /etc/splicefs/client/mounts.d
/usr/share/doc/splicefs-client

%post
# mount(8) looks for its helpers in /sbin. Where /sbin is a folder of its own and not a link
# to /usr/sbin, the helper needs a link there as well.
if [ ! -L /sbin ] && [ ! -e /sbin/mount.splicefs ]; then
    ln -s /usr/bin/splicefs /sbin/mount.splicefs
fi
# $1 is 1 on a first install and 2 on an upgrade. A container or a chroot has no running
# systemd; the files are installed and there is nothing to start.
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    if [ "$1" -eq 1 ]; then
        systemctl enable splicefs.service
        systemctl start splicefs.service ||
            echo "splicefs did not start; see: journalctl -u splicefs" >&2
    else
        # A broken configuration must not turn an upgrade into a failed transaction.
        systemctl try-restart splicefs.service ||
            echo "splicefs did not restart; see: journalctl -u splicefs" >&2
    fi
fi

%preun
# $1 is 0 when the package is removed for good, not replaced by another version.
if [ "$1" -eq 0 ] && [ -d /run/systemd/system ]; then
    systemctl disable --now splicefs.service || true
fi
if [ "$1" -eq 0 ]; then
    # The link made at install time for systems where /sbin is its own folder.
    if [ ! -L /sbin ] && [ "$(readlink /sbin/mount.splicefs 2>/dev/null)" = /usr/bin/splicefs ]; then
        rm -f /sbin/mount.splicefs
    fi
fi

%postun
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
