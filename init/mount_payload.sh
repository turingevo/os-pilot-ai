#!/bin/busybox sh
# 在块设备中查找 Ventoy 数据分区并挂载到 /iso
# 判定方法: 逐个尝试挂载，能挂上且包含 /ventoy 目录的分区即视为数据分区。
# 注意: AI 环境里的 busybox blkid 是最简版本，不支持 -o device/-s LABEL，
#       因此这里通过 /sys/class/block 枚举设备。

mkdir -p /iso

is_payload() {
    [ -d /iso/ventoy ]
}

try_mount() {
    dev="$1"
    for fs in exfat vfat ntfs3 ntfs ext4 xfs udf iso9660; do
        if mount -t "$fs" -o rw "$dev" /iso 2>/dev/null; then
            if is_payload; then
                echo "[mount_payload] $dev ($fs, rw)"
                return 0
            fi
            umount /iso 2>/dev/null
        fi
        if mount -t "$fs" -o ro "$dev" /iso 2>/dev/null; then
            if is_payload; then
                echo "[mount_payload] $dev ($fs, ro)"
                return 0
            fi
            umount /iso 2>/dev/null
        fi
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
