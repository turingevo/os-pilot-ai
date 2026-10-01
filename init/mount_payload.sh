#!/bin/busybox sh
# 在块设备中查找 Ventoy 数据分区并挂载到 /iso
# 判定方法: 逐个尝试挂载，能挂上且包含 /ventoy 目录的分区即视为数据分区。
# 注意: AI 环境里的 busybox blkid 是最简版本，不支持 -o device/-s LABEL，
#       因此这里通过 /sys/class/block 枚举设备。

mkdir -p /iso

is_payload() {
    [ -d /iso/ventoy ]
}

# 挂载选项：exfat/vfat 显式给 fmask/dmask=0022，否则文件可能没有执行位
# （用户自带的 llama-server 放在数据分区上要能直接执行）；root 只能执行至少有一个
# x 位的文件，所以不能依赖内核默认值。其它文件系统不认识 fmask，保持裸 rw/ro。
try_mount() {
    dev="$1"
    for fs in exfat vfat ntfs3 ntfs ext4 xfs udf iso9660; do
        case "$fs" in
            exfat|vfat)
                o_rw="rw,uid=0,gid=0,fmask=0022,dmask=0022"
                o_ro="ro,uid=0,gid=0,fmask=0022,dmask=0022"
                ;;
            *)
                o_rw="rw"
                o_ro="ro"
                ;;
        esac
        if mount -t "$fs" -o "$o_rw" "$dev" /iso 2>/dev/null; then
            if is_payload; then
                echo "[mount_payload] $dev ($fs, rw)"
                return 0
            fi
            umount /iso 2>/dev/null
        fi
        if mount -t "$fs" -o "$o_ro" "$dev" /iso 2>/dev/null; then
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
