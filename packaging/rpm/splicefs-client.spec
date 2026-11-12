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
install -d %{buildroot}/etc/splicefs/client
install -D -m 644 %{src}/LICENSE %{buildroot}/usr/share/doc/splicefs-client/LICENSE
install -m 644 %{src}/README.md %{src}/CHANGELOG.md %{src}/docs/configuration.md \
    %{buildroot}/usr/share/doc/splicefs-client/

%files
/usr/bin/splicefs
/usr/lib/systemd/system/splicefs.service
%dir /etc/splicefs
%dir /etc/splicefs/client
/usr/share/doc/splicefs-client

%post
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

%postun
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
