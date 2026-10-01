#!/bin/busybox sh
# 在块设备中查找 AI 环境的载荷分区（Ventoy 数据分区）并挂载到 /iso
# 判定方法: 逐个尝试挂载，能挂上且带 AI 环境标记的分区才是载荷分区。
# 注意: AI 环境里的 busybox blkid 是最简版本，不支持 -o device/-s LABEL，
#       因此这里通过 /sys/class/block 枚举设备；blkid 的整表输出只用来把已探测到的
#       文件系统类型排到尝试顺序的最前面（少几次无效挂载）。
# 所有判定过程写到 stdout，由 init 决定何时展示（真机重试期间不刷屏）。

mkdir -p /iso

# 载荷标记 = AI 环境自己的资产或配置，只有这种分区才该写日志、才该去里面找模型。
# 为什么不能退而认「有 /ventoy 目录」：官方 Ventoy 安装器在**每一支** Ventoy 盘上都建
# ventoy/ 目录，装了 Ventoy 的内置盘也一样 ⇒ 拿它当判据会把别的盘误认成载荷盘，
# 日志、ai.json、模型全都找错地方，还会往用户的内置盘里建目录写文件。
# 宁可不挂（init 会重试到超时），也不挂错。
is_payload() {
    [ -d /iso/ventoy/ai ] || [ -f /iso/ventoy/ai.json ]
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
FS_ORDER="exfat vfat ntfs3 ntfs ext4 xfs f2fs hfsplus udf iso9660"

# blkid 认出来的类型排最前：真机上分区一多，每块盘把 10 种文件系统全试一遍太慢。
detected_fs() {
    printf '%s\n' "$BLKID_ALL" | sed -n "s#^$1: .*TYPE=\"\\([^\"]*\\)\".*#\\1#p"
}

# $1=设备；命中载荷标记则保持挂载并返回 0
try_mount() {
    dev="$1"
    det=$(detected_fs "$dev")
    for fs in $det $FS_ORDER; do
        [ -n "$fs" ] || continue
        for o in $(fs_opts "$fs"); do
            case "$o" in ro*) mode=ro ;; *) mode=rw ;; esac
            mount -t "$fs" -o "$o" "$dev" /iso 2>/dev/null || continue
            # 挂上了就看一次目录定性别再换文件系统重试：类型已经对了，换法只会重复判定。
            if is_payload; then
                echo "[mount_payload] 命中 $dev ($fs, $mode)"
                return 0
            fi
            echo "[mount_payload] 跳过 $dev ($fs, $mode)：无 ventoy/ai 或 ventoy/ai.json，不是载荷盘"
            umount /iso 2>/dev/null
            return 1
        done
    done
    return 1
}

devices() {
    # 1) 分区优先；2) 无分区表的裸盘（排除 loop/ram/zram/光驱/device-mapper）
    for base in /sys/class/block/*; do
        [ -e "$base/partition" ] || continue
        echo "/dev/$(basename "$base")"
    done
    for base in /sys/class/block/*; do
        [ -e "$base/partition" ] && continue
        name=$(basename "$base")
        case "$name" in
            loop*|ram*|zram*|sr*|dm-*) continue ;;
        esac
        echo "/dev/$name"
    done
}

BLKID_ALL=$(blkid 2>/dev/null)

for dev in $(devices); do
    try_mount "$dev" && exit 0
done

echo "[mount_payload] 没找到载荷分区（当时可见: $(ls /sys/class/block 2>/dev/null | tr '\n' ' ')）"
exit 1
