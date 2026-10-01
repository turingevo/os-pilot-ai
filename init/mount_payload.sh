#!/bin/busybox sh
# 在块设备中查找 Ventoy 数据分区并挂载到 /iso
# 判定方法: 逐个尝试挂载，能挂上且包含 /ventoy 目录的分区即视为数据分区。
# 注意: AI 环境里的 busybox blkid 是最简版本，不支持 -o device/-s LABEL，
#       因此这里通过 /sys/class/block 枚举设备。

mkdir -p /iso

is_payload() {
    [ -d /iso/ventoy ]
}

# 挂载选项候选表：按顺序逐个尝试，全部失败则试裸 rw/ro 兜底。
# 为什么必须显式给掩码：exfat/vfat/ntfs3/hfsplus 这类没有 POSIX 权限位的文件系统，
# 默认挂载出来的文件可能没有执行位，而 root 也只能执行至少带一个 x 位的文件 ——
# 用户放在数据分区上的 llama-server 就要能直接执行，所以不能依赖内核默认值。
# 选项集取自各模块支持的挂载参数（ntfs3 有 fmask/dmask/umask，hfsplus 只有 umask/force，
# f2fs 是原生 POSIX 权限、不接受这类选项，故只裸挂）。
# 注意: 这里与 agent 侧 disk/ops.go 的 fsSpec.mountOpts 是同一份知识的两份实现（sh / Go），改动要同步。
fs_opts() {
    case "$1" in
        exfat|vfat)
            echo "rw,uid=0,gid=0,fmask=0022,dmask=0022 ro,uid=0,gid=0,fmask=0022,dmask=0022 rw ro"
            ;;
        ntfs3)
            echo "rw,uid=0,gid=0,fmask=0022,dmask=0022 ro,uid=0,gid=0,fmask=0022,dmask=0022 rw ro"
            ;;
        hfsplus)
            echo "rw,uid=0,gid=0,umask=0022 ro,uid=0,gid=0,umask=0022 rw ro"
            ;;
        *)
            echo "rw ro"
            ;;
    esac
}

# ntfs 这一项留给极老的载荷盘：本内核只有 ntfs3，ntfs 会直接失败，靠前面的 ntfs3 命中。
try_mount() {
    dev="$1"
    for fs in exfat vfat ntfs3 ntfs ext4 xfs f2fs hfsplus udf iso9660; do
        for o in $(fs_opts "$fs"); do
            case "$o" in ro*) mode=ro ;; *) mode=rw ;; esac
            if mount -t "$fs" -o "$o" "$dev" /iso 2>/dev/null; then
                if is_payload; then
                    echo "[mount_payload] $dev ($fs, $mode)"
                    return 0
                fi
                umount /iso 2>/dev/null
            fi
        done
    done
    return 1
}

# 1) 分区优先
for base in /sys/class/block/*; do
    [ -e "$base/partition" ] || continue
    try_mount "/dev/$(basename "$base")" && exit 0
done

# 2) 整盘（无分区表的裸盘），排除 loop/ram/zram/光驱
for base in /sys/class/block/*; do
    [ -e "$base/partition" ] && continue
    name=$(basename "$base")
    case "$name" in
        loop*|ram*|zram*|sr*|dm-*) continue ;;
    esac
    try_mount "/dev/$name" && exit 0
done

exit 1
