#!/usr/bin/env bash
set -euo pipefail

echo "== devbox runtimeclass/snapshotter diagnose =="
echo

section() {
  printf "\n## %s\n" "$1"
}

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

section "1) systemd services"
if has_cmd systemctl; then
  systemctl is-active containerd || true
  systemctl is-active stargz-snapshotter || true
  systemctl --no-pager --full status stargz-snapshotter 2>/dev/null | sed -n '1,20p' || true
else
  echo "systemctl not found, skip"
fi

section "2) runtimeclass resources"
if has_cmd kubectl; then
  kubectl get runtimeclass 2>/dev/null || true
  echo
  kubectl get runtimeclass devbox-runc -o yaml 2>/dev/null | sed -n '1,40p' || true
  echo
  kubectl get runtimeclass devbox-stargz-runc -o yaml 2>/dev/null | sed -n '1,40p' || true
else
  echo "kubectl not found, skip"
fi

section "3) containerd runtime handler mapping"
CFG="/etc/containerd/config.toml"
if [[ -f "$CFG" ]]; then
  grep -nE 'runtimes\.devbox-runc|runtimes\.devbox-stargz-runc|snapshotter\s*=' "$CFG" || true
else
  echo "$CFG not found"
fi

section "4) sockets"
ls -l /run/containerd/containerd.sock 2>/dev/null || true
ls -l /run/containerd-stargz-grpc/containerd-stargz-grpc.sock 2>/dev/null || true
ls -l /run/containerd-stargz-grpc/cri.sock 2>/dev/null || true

section "4.1) kubelet image service endpoint"
if [[ -f /etc/default/kubelet ]]; then
  grep -n -- '--image-service-endpoint' /etc/default/kubelet || true
else
  echo "/etc/default/kubelet not found"
fi

section "5) mount points and lvm"
findmnt /var/lib/containerd 2>/dev/null || true
findmnt /var/lib/containerd-stargz-grpc 2>/dev/null || true
if has_cmd lvs; then
  lvs || true
else
  echo "lvs not found, skip"
fi

section "6) pod runtimeClassName overview"
if has_cmd kubectl; then
  kubectl get pod -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,RUNTIMECLASS:.spec.runtimeClassName' 2>/dev/null || true
fi

section "7) containerd runtime handlers"
if has_cmd ctr; then
  ctr plugins ls 2>/dev/null | grep -E 'snapshot|runtime' || true
fi

echo
echo "diagnose done"
