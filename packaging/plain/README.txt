SpliceFS client, the plain programs

This archive holds the programs and nothing else: no service file, no mount helper link and no
install script. It is for a system the packages do not cover, or for people who want to place
every file themselves. The packages are the easier way where one exists for your system.

Put the programs where you want them, for example in a folder on your PATH. Then:

  splicefs --version
  splicefs [--config DIR]                run the client, in the foreground, until it is stopped
  splicefs [--config DIR] config check   check the configuration files
  splicefs [--config DIR] config show    print the configuration and the file each value is from
  splicefsctl [--config DIR] status      show the mounts, the nodes and the cache use
  splicefsctl [--config DIR] umount      unmount, and keep the client running

DIR after --config holds what you write: settings in conf.d/, clusters in clusters.d/ and
mounts in mounts.d/. Without the option the program uses the default folder of its system
(/etc/splicefs/client, and ~/.splicefs/client on macOS). With no cluster and no mount it runs
and mounts nothing.

Send the running client SIGHUP (kill -HUP PID) to apply a changed configuration without a
restart. Run it from the init system of your choice, as a user that may mount.

To use mount -t splicefs and fstab lines of that type on Linux or FreeBSD, make a link named
mount.splicefs to splicefsctl in a folder where mount looks for helpers, for example
ln -s /usr/local/bin/splicefsctl /usr/sbin/mount.splicefs. splicefsctl is the mount helper
when it is started under that name.

Files in the release are checked with SHA256SUMS and its signature SHA256SUMS.asc.

The real commands, every option and the format of the files: README.md and
docs/configuration.md at https://github.com/SpliceFS/client

The license is in the file LICENSE.
