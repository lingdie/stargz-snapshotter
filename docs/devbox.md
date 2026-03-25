# Stargz Snapshotter Devbox Writable Layer Guide

本文说明如何在 `stargz-snapshotter` 中使用新加入的 devbox writable layer 能力。

这项能力保留了 stargz 的远端 lazy-pull lower layer，同时把容器的 writable upper/work 目录切换到 LVM 逻辑卷上，实现以下效果：

- 镜像层继续由 stargz snapshotter 按需懒加载
- 可写层落到独立 LVM 卷，而不是普通目录
- 支持按 `content-id` 复用已有 writable 卷
- 支持按 `storage-limit` 创建或扩容 writable 卷
- 支持显式卸载、延迟清理和孤儿卷回收

## 1. 前置条件

- 节点必须是 Linux
- 需要 root 权限启动 snapshotter
- 已安装 LVM 工具，至少包含 `lvs`、`lvcreate`、`lvresize`、`lvremove`
- 已安装 `mkfs.ext4`
- 已存在可用的 volume group
- 如果使用 thin provisioning，还需要已有 thin pool
- containerd 侧已启用 `stargz` snapshotter，并设置：

```toml
[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "stargz"
  disable_snapshot_annotations = false
```

`disable_snapshot_annotations = false` 很重要；否则上层写入的 snapshot labels 不会透传。

## 2. 构建 linux/amd64 二进制

在仓库根目录执行：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
GOCACHE=/tmp/stargz-go-build \
make containerd-stargz-grpc stargz-fuse-manager \
  PREFIX="$(pwd)/out/linux_amd64/"
```

产物为：

- `out/linux_amd64/containerd-stargz-grpc`
- `out/linux_amd64/stargz-fuse-manager`

如果你不启用 fuse manager，`stargz-fuse-manager` 可以不部署，但建议一起保留。

## 3. 安装

示例安装命令：

```bash
install -Dm755 out/linux_amd64/containerd-stargz-grpc /usr/local/bin/containerd-stargz-grpc
install -Dm755 out/linux_amd64/stargz-fuse-manager /usr/local/bin/stargz-fuse-manager

mkdir -p /etc/containerd-stargz-grpc
mkdir -p /var/lib/containerd-stargz-grpc

cp script/config/etc/containerd-stargz-grpc/config.toml /etc/containerd-stargz-grpc/config.toml
cp script/config/etc/systemd/system/stargz-snapshotter.service /etc/systemd/system/stargz-snapshotter.service
```

## 4. 配置 snapshotter

在 `/etc/containerd-stargz-grpc/config.toml` 中增加：

```toml
[snapshotter]
allow_invalid_mounts_on_restart = true

[snapshotter.devbox]
enable = true
lvm_vg_name = "devbox-lvm-vg"
thin_pool_name = "devbox-thinpool"
```

说明：

- `enable`: 开启 devbox writable layer
- `lvm_vg_name`: writable 卷所在的 volume group
- `thin_pool_name`: 可选；为空时走普通 LV，非空时走 thin pool

如果你要启用 fuse manager，可以继续加：

```toml
[fuse_manager]
enable = true
address = "/run/containerd-stargz-grpc/fuse-manager.sock"
path = "/usr/local/bin/stargz-fuse-manager"
```

## 5. 配置 containerd

确保 `/etc/containerd/config.toml` 包含：

```toml
version = 2

[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
  [proxy_plugins.stargz.exports]
    root = "/var/lib/containerd-stargz-grpc/"

[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "stargz"
  disable_snapshot_annotations = false
```

## 6. 启动

```bash
systemctl daemon-reload
systemctl enable --now stargz-snapshotter
systemctl restart containerd
```

如果不用 systemd，也可以直接前台启动：

```bash
/usr/local/bin/containerd-stargz-grpc \
  --config=/etc/containerd-stargz-grpc/config.toml \
  --log-level=info
```

## 7. 触发 devbox 行为的 labels

只有在 active snapshot 的 `Prepare` 阶段同时带上以下两个 label 时，才会启用 devbox 路径：

- `containerd.io/snapshot/devbox-content-id`
- `containerd.io/snapshot/devbox-storage-limit`

支持的完整 label 集合如下：

### `containerd.io/snapshot/devbox-content-id`

逻辑 writable 卷的业务 ID。相同 `content-id` 会尝试复用同一个 LVM 卷。

### `containerd.io/snapshot/devbox-storage-limit`

目标容量，支持：

- `Ki`
- `Mi`
- `Gi`
- `B`

例如：

- `512Mi`
- `20Gi`

### `containerd.io/snapshot/devbox-init`

如果值存在，创建新 writable 卷时会把父 snapshot 的 upperdir 内容复制到新卷里。

### `containerd.io/snapshot/devbox-unmount-lvm`

通过 `Snapshotter.Update` 传入该 label 且值为 `true` 时，会卸载当前 writable 卷，并清空 `content-id -> snapshot` 的占用关系。

### `containerd.io/snapshot/devbox-remove-content-id`

通过 `Snapshotter.Update` 传入该 label 时，会把对应 `content-id` 标记为待删除。等相关 snapshot `Remove/Cleanup` 后，孤儿 LV 会被真正回收。

## 8. 使用方式

这项能力现在直接挂到标准 snapshotter 名字 `stargz` 下，由 devbox labels 决定是否启用 writable layer 扩展路径。

也就是说：

- lower layer 仍然由 stargz 负责 lazy pull
- upper/work 目录在带 devbox labels 时切换到 LVM
- 不带 devbox labels 时，行为和原来的 stargz snapshotter 一样

上层调用方需要在创建 active snapshot 时把这些 labels 传给 stargz snapshotter。

如果你已经有自己的 containerd client、CRI 扩展或者调度层，只需要在 `Prepare` 前写入这些 labels。

## 9. 一个典型生命周期

1. 上层请求创建 active snapshot，并传入：
   - `devbox-content-id=workspace-123`
   - `devbox-storage-limit=20Gi`
2. stargz snapshotter 创建或复用 `workspace-123` 对应的 LVM 卷
3. snapshot 的 upper/work 落到这个卷上
4. lower layer 仍然从远端 registry lazy pull
5. 提交 snapshot 时，仅重命名 snapshot 元数据；devbox 元数据会同步更新
6. 如果业务希望暂时释放挂载但保留卷，调用 `Update` 并设置 `devbox-unmount-lvm=true`
7. 如果业务希望最终删除该 content，调用 `Update` 并设置 `devbox-remove-content-id=<content-id>`
8. 当相关 snapshot 被 `Remove/Cleanup` 后，孤儿 LV 会被删除

## 10. 验证

### 检查 snapshotter 进程

```bash
systemctl status stargz-snapshotter
```

### 检查 LVM 卷是否创建

```bash
lvs
```

devbox 创建的卷名会以 `devbox-` 开头。

### 检查挂载

```bash
mount | grep /var/lib/containerd-stargz-grpc/snapshotter/snapshots
```

### 检查 stargz socket

```bash
ls -l /run/containerd-stargz-grpc/containerd-stargz-grpc.sock
```

## 11. 故障排查

### `lvcreate` / `lvresize` 失败

- 检查 `lvm_vg_name` 是否存在
- 检查 thin pool 是否存在
- 检查剩余容量

### writable 卷没有生效

- 确认 active snapshot 的 `Prepare` 真的传入了 `devbox-content-id` 和 `devbox-storage-limit`
- 确认 containerd CRI 没有禁用 snapshot annotations

### 重启后挂载丢失

- remote lower layer 会按 stargz 原逻辑恢复
- devbox 的 LVM 卷不会被当作 remote snapshot 处理
- 如果你开启了 fuse manager，确认 `stargz-fuse-manager` 可执行文件存在且路径配置正确

## 12. 当前实现边界

- devbox 逻辑只作用于 active snapshot 的 writable layer
- view/committed snapshot 仍走原有 stargz/overlay 路径
- LVM 通过本机 CLI 管理，不依赖 openebs CRD
- devbox 元数据单独存放在 `snapshotter/devbox.db`，不修改 containerd 内置的 `metadata.db`

## 13. 在 `sealos-staging` 新环境部署（仅脚本生成与执行说明）

如果目标机器通过 `ssh sealos-staging` 登录，并希望把部署所需内容统一放到 `/root/yy/stargz`，可以使用仓库里的脚本：

- `script/devbox/staging/push-stargz-bundle.sh`
- `script/devbox/staging/remote-install.sh`

这两个脚本职责如下：

- `push-stargz-bundle.sh`（本地执行）：
  - 检查二进制是否已构建
  - 组装 bundle（包含二进制、配置模板、安装脚本）
  - 上传到 `sealos-staging:/root/yy/stargz`
- `remote-install.sh`（远端执行）：
  - 仅安装文件到标准路径（`/usr/local/bin`、`/etc/...`）
  - 不自动改配置内容，只提示后续手工步骤

### 13.1 本地构建二进制

在仓库根目录执行：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
GOCACHE=/tmp/stargz-go-build \
make containerd-stargz-grpc stargz-fuse-manager \
  PREFIX="$(pwd)/out/linux_amd64/"
```

### 13.2 上传部署 bundle 到 `sealos-staging`

```bash
chmod +x script/devbox/staging/push-stargz-bundle.sh
chmod +x script/devbox/staging/remote-install.sh
bash script/devbox/staging/push-stargz-bundle.sh
```

如果你需要改 host 或目标目录，可以用环境变量覆盖：

```bash
REMOTE_HOST=sealos-staging \
REMOTE_DIR=/root/yy/stargz \
bash script/devbox/staging/push-stargz-bundle.sh
```

上传后，远端目录大致如下：

```text
/root/yy/stargz/
  bin/
    containerd-stargz-grpc
    stargz-fuse-manager
  config/etc/containerd-stargz-grpc/config.toml
  config/etc/systemd/system/stargz-snapshotter.service
  config/etc/containerd/config.toml.example
  scripts/remote-install.sh
```

### 13.3 在远端安装文件（手工触发）

```bash
ssh sealos-staging
cd /root/yy/stargz
bash scripts/remote-install.sh
```

该脚本执行后只会：

- 安装二进制到 `/usr/local/bin`
- 安装配置模板到 `/etc/containerd-stargz-grpc/config.toml`
- 安装 systemd 文件到 `/etc/systemd/system/stargz-snapshotter.service`

然后你再按自身环境手工调整配置，并手工执行：

```bash
systemctl daemon-reload
systemctl enable --now stargz-snapshotter
systemctl restart containerd
```
