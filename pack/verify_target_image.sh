#!/bin/sh
# 只读取证一块「已安装」的目标盘镜像 —— 「装完能不能启动」的静态判据。
#
# 判据（任一条不满足，开机就进不去）：
#   1. 有 ESP 且里面有 UEFI 引导器（EFI/BOOT/BOOTX64.EFI 或 EFI/ubuntu/shimx64.efi 等）——否则 UEFI 无物可启；
#   2. root 分区 ≥25 GiB —— 太小 update-initramfs 写不下，装完没有 initrd；
#   3. /boot 里有 initrd 实体文件（不是悬空软链）；
#   4. root/home 的 ext4 **不含** orphan_file / metadata_csum_seed —— 否则 Ubuntu 22.04 的
#      e2fsck 1.46.5 拒检（开机进紧急模式）、GRUB 2.06 读不了（grub-install 报 unknown filesystem）；
#   5. /etc/fstab 里的 root/efi/home UUID 与实际分区一致。
#
# 全程只读：udisksctl 只读挂载，结束即卸载并 detach。不需要 root。
#
# 用法: verify_target_image.sh [镜像]        # 默认 $BUILD/test/reinstall-target.img
# 退出码: 0 = 全部判据通过；1 = 装了但不满足启动条件；2 = 空盘/还没装机
set -e

. "$(dirname "$0")/defaults.sh"

BUILD="$VTOY_AI_BUILD_DIR"
IMG="${1:-${VTOY_AI_TARGET_IMG:-$BUILD/test/reinstall-target.img}}"
MIN_ROOT_MIB="${VTOY_AI_TARGET_MIN_ROOT_MIB:-25600}"   # 默认 25 GiB（桌面版下限）

FAILED=0
ok() { printf '  PASS  %s\n' "$1"; }
no() { printf '  FAIL  %s\n' "$1"; FAILED=$((FAILED + 1)); }

[ -f "$IMG" ] || { echo "错误: 找不到镜像 $IMG" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少 $1（$2）" >&2; exit 1; }; }
need sgdisk "apt install gdisk"
need lsblk "apt install util-linux"

echo "[verify] 镜像: $IMG（$(du -h --apparent-size "$IMG" | cut -f1) 表观 / $(du -h "$IMG" | cut -f1) 实占）"

# ---------- 分区表 ----------
PART_LINE=$(sgdisk -p "$IMG" 2>/dev/null | awk '$1 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/')
if [ -z "$PART_LINE" ]; then
    # 区分「空盘/还没装机」和「有分区表但没装成」——前者是正常中间态，别当成故障
    gpt=$(dd if="$IMG" bs=1 skip=512 count=8 status=none)
    mbr=$(dd if="$IMG" bs=1 skip=510 count=2 status=none | od -An -tx1 | tr -d ' \n')
    if [ "$gpt" != "EFI PART" ] && [ "$mbr" != "55aa" ]; then
        echo "[verify] 这是**空盘**（无 GPT/MBR 分区表）——还没装机。" >&2
        echo "[verify] 先按 docs/测试.md 的「8) 重装 + 修后闭环」走流程（造好盘 → run_qemu_usb.sh → 安装），装完再跑本脚本。" >&2
        exit 2
    fi
    echo "[verify] 有分区表但读不到分区（分区表可能被写坏）——装机未完成" >&2
    exit 1
fi
echo "[verify] 分区表:"; echo "$PART_LINE" | sed 's/^/         /'

part_start() { sgdisk -p "$IMG" 2>/dev/null | awk -v n="$1" '$1==n{print $2}'; }
part_end()   { sgdisk -p "$IMG" 2>/dev/null | awk -v n="$1" '$1==n{print $3}'; }
part_mib()   { st=$(part_start "$1"); en=$(part_end "$1"); echo $(( (en - st + 1) * 512 / 1048576 )); }

# ext4 超级块特性（直接读镜像字节，免 root）：magic@0x38 compat@0x5C incompat@0x60 ro@0x64
sb_field() { # $1=分区号 $2=超级块内偏移 $3=字节数 $4=od 格式
    od -j $(( $(part_start "$1") * 512 + 1024 + $2 )) -N "$3" -An "$4" -v "$IMG" | tr -d ' \n'
}
sb_u32() { # $1=分区号 $2=偏移 -> 十进制
    v=$(sb_field "$1" "$2" 4 -tx4)
    [ -n "$v" ] && echo $(( 0x$v )) || echo 0
}
# superblock magic 0x53EF 在盘上是 53 ef 两字节，od 按小端读成 16 位字即 ef53
sb_has_magic() { [ "$(sb_field "$1" 0x38 2 -tx2)" = "ef53" ]; }

# ---------- 只读挂载 ----------
OUT=$(udisksctl loop-setup -r -f "$IMG")
LOOP=$(printf '%s' "$OUT" | grep -o '/dev/loop[0-9]*')
[ -n "$LOOP" ] || { echo "错误: loop-setup 失败: $OUT" >&2; exit 1; }
unmount_all() {
    for d in "$LOOP"p*; do
        [ -b "$d" ] || continue
        if [ -n "$(mp_of "$d")" ]; then
            # 桌面会话的索引/自动挂载会让普通卸载偶发 "target is busy"；只读挂载，退而强制卸载
            udisksctl unmount -b "$d" >/dev/null 2>&1 \
                || udisksctl unmount -f -b "$d" >/dev/null 2>&1 || true
        fi
    done
}
cleanup() { unmount_all; udisksctl loop-delete -b "$LOOP" >/dev/null 2>&1 || true; }
trap cleanup EXIT

mp_of() { lsblk -nlo MOUNTPOINT "$1" 2>/dev/null | head -1; }
# udisks2 在 loop-setup 时就会把分区自动挂成只读（挂载点顺延 ROOT2/HOME2…），
# 所以这里先看是不是已经挂好了，没挂再显式挂；返回挂载点（失败返回空）。
ensure_mounted() { # $1=分区设备
    M=$(mp_of "$1")
    if [ -z "$M" ]; then
        udisksctl mount -b "$1" -o ro >/dev/null 2>&1 || true
        M=$(mp_of "$1")
    fi
    [ -n "$M" ] && printf '%s' "$M"
}

# 找 root：含 /etc/os-release 的分区就是
ROOT=""; ROOT_DEV=""; ROOT_MNT=""
for d in "$LOOP"p*; do
    [ -b "$d" ] || continue
    M=$(ensure_mounted "$d"); [ -n "$M" ] || continue
    if [ -f "$M/etc/os-release" ]; then ROOT_DEV="$d"; ROOT_MNT="$M"; ROOT=$(basename "$d"); break; fi
done
[ -n "$ROOT_MNT" ] || { no "挂载不到 root 分区（无 /etc/os-release）"; echo "[verify] 失败 $FAILED 项" >&2; exit 1; }
ok "找到 root: $(basename "$ROOT_DEV") 挂载于 $ROOT_MNT（$(head -1 "$ROOT_MNT/etc/os-release" 2>/dev/null)）"

# ---------- 判据 1: ESP 有引导器 ----------
ESP_MNT=""
for d in "$LOOP"p*; do
    [ -b "$d" ] || continue
    [ "$d" = "$ROOT_DEV" ] && continue
    case "$(lsblk -no FSTYPE "$d")" in vfat|msdos) ;; *) continue ;; esac
    M=$(ensure_mounted "$d"); [ -n "$M" ] || continue
    ESP_MNT="$M"; ESP_DEV="$d"; break
done
if [ -z "$ESP_MNT" ]; then
    no "找不到可挂载的 ESP（vfat）"
else
    LOADER=$(find "$ESP_MNT" -maxdepth 4 \( -iname 'BOOTX64.EFI' -o -iname 'shimx64.efi' -o -iname 'grubx64.efi' \) 2>/dev/null | head -1)
    if [ -n "$LOADER" ]; then ok "ESP 有 UEFI 引导器: ${LOADER#$ESP_MNT}"
    else no "ESP 没有 UEFI 引导器（EFI/BOOT/BOOTX64.EFI 等）—— UEFI 无物可启：$(find "$ESP_MNT" -maxdepth 3 2>/dev/null | sed "s#$ESP_MNT#<esp>#" | head -6 | tr '\n' ' ')"; fi
fi

# ---------- 判据 2: root 容量 ----------
ROOT_PN=$(echo "$ROOT" | sed 's/.*p//')
ROOT_MIB=$(part_mib "$ROOT_PN")
if [ "$ROOT_MIB" -ge "$MIN_ROOT_MIB" ]; then ok "root 分区 ${ROOT_MIB} MiB ≥ ${MIN_ROOT_MIB} MiB"
else no "root 分区仅 ${ROOT_MIB} MiB < ${MIN_ROOT_MIB} MiB —— initramfs 可能写不下"; fi

# ---------- 判据 3: /boot 有 initrd 实体 ----------
if [ -d "$ROOT_MNT/boot" ]; then
    INITRD=$(find "$ROOT_MNT/boot" -maxdepth 1 -name 'initrd.img-*' -type f 2>/dev/null | head -1)
    VMLINUZ=$(find "$ROOT_MNT/boot" -maxdepth 1 -name 'vmlinuz-*' -type f 2>/dev/null | head -1)
    [ -n "$INITRD" ] && ok "/boot 有 initrd 实体: $(basename "$INITRD")（$(du -h "$INITRD" | cut -f1)）" \
        || no "/boot 没有 initrd 实体文件（装完起不来）"
    [ -n "$VMLINUZ" ] && ok "/boot 有内核: $(basename "$VMLINUZ")" || no "/boot 没有 vmlinuz-*"
else
    no "/boot 目录不存在"
fi

# ---------- 判据 4: ext4 特性（修后不得带 orphan_file / metadata_csum_seed） ----------
echo "[verify] ext4 特性（直接读超级块）:"
for pn in $(echo "$PART_LINE" | awk '{print $1}'); do
    [ "$pn" = "$(echo "$ROOT" | sed 's/.*p//')" ] || true
    dev="$LOOP""p$pn"
    [ -b "$dev" ] || continue
    case "$(lsblk -no FSTYPE "$dev")" in ext4|ext3|ext2) ;; *) continue ;; esac
    if ! sb_has_magic "$pn"; then no "分区 $pn 超级块 magic 不是 ext4"; continue; fi
    COMPAT=$(sb_u32 "$pn" 0x5C); INCOMPAT=$(sb_u32 "$pn" 0x60)
    BAD=""
    [ $(( COMPAT & 0x1000 ))   -ne 0 ] && BAD="$BAD orphan_file"
    [ $(( INCOMPAT & 0x2000 )) -ne 0 ] && BAD="$BAD metadata_csum_seed"
    if [ -n "$BAD" ]; then no "分区 $pn (compat=$COMPAT incompat=$INCOMPAT) 带$BAD —— 22.04 的 e2fsck/GRUB 读不了"
    else ok "分区 $pn 特性干净（无 orphan_file/metadata_csum_seed）"; fi
done

# ---------- 判据 5: fstab UUID 与实际分区一致 ----------
echo "[verify] fstab 与实际分区对齐:"
FSTAB="$ROOT_MNT/etc/fstab"
if [ -f "$FSTAB" ]; then
    check_uuid() { # $1=mountpoint $2=期望的 UUID/PARTUUID
        case "$2" in
            UUID=*) id="${2#UUID=}"; field=UUID ;;
            PARTUUID=*) id="${2#PARTUUID=}"; field=PARTUUID ;;
            *) return 0 ;;
        esac
        found=$(lsblk -rno "$field" "$LOOP"p* 2>/dev/null | grep -ix "$id" | head -1)
        [ -n "$found" ] && ok "fstab $1 $2 命中实际分区" || no "fstab $1 $2 与实际分区不符"
    }
    while read -r dev mnt _; do
        case "$dev" in ''|'#'*) continue ;; esac
        case "$mnt" in /|/boot/efi|/home) check_uuid "$mnt" "$dev" ;; esac
    done < "$FSTAB"
else
    no "root 里没有 /etc/fstab"
fi

echo "[verify] 完成"
if [ "$FAILED" -gt 0 ]; then
    echo "[verify] 失败 $FAILED 项" >&2
    exit 1
fi
echo "[verify] 全部通过 —— 该盘具备可启动条件，可用 run_qemu_target.sh 实地引导确认"
