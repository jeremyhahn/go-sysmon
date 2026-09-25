#!/bin/sh
#
# The devcontainer's entrypoint: delegates cgroup controllers inside the
# container's own cgroup namespace, then runs the command it was given.
#
# The collector suite creates real container and VM cgroups (docker-<id>.scope,
# machine-qemu-...scope) and moves processes into them, so the collector reads
# real accounting files. In cgroup v2 a cgroup's children get the memory, io,
# cpu and pids controllers only once its subtree_control enables them, and that
# is refused while the cgroup itself holds processes. Docker starts the command
# in the namespace's root cgroup, so this moves it into a leaf, /init, first --
# the same step systemd and docker-in-docker take.
#
# It acts only in a private cgroup namespace, where /proc/self/cgroup reads
# "0::/" and /sys/fs/cgroup is the container's own subtree, and only when that
# tree is writable, as it is under --privileged. Under a host cgroup namespace
# the same writes would reach the host's cgroups, so there it changes nothing
# and the collector suite fails with the reason.
set -eu

root=/sys/fs/cgroup
if [ "$(cat /proc/self/cgroup)" = "0::/" ] && [ -f "$root/cgroup.controllers" ] \
    && [ -w "$root/cgroup.subtree_control" ] && [ -z "$(cat "$root/cgroup.subtree_control")" ]; then
    mkdir -p "$root/init"
    # This shell is the namespace's only process; commands it runs from here
    # on start in /init with it.
    echo $$ >"$root/init/cgroup.procs"
    for controller in $(cat "$root/cgroup.controllers"); do
        case $controller in
        cpu | io | memory | pids) echo "+$controller" >"$root/cgroup.subtree_control" ;;
        esac
    done
fi

exec "$@"
