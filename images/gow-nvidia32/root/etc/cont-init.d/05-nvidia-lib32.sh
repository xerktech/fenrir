# Sourced as root by the GOW entrypoint (no `exit`). The image's 32-bit NVIDIA
# libraries only work against the same kernel module version; on any other the
# loader may pick the NVIDIA ICD and fail rather than fall back to the CPU.
# So remove them when the node's driver differs (XERK-1930), and the NGX Wine
# DLLs too (XERK-2073): they must match the node's libnvidia-ngx. The entrypoint
# runs under `set -e`: a failing command here would abort it and end the
# Session, hence `|| true` where a file may be missing.
want=$(cat /etc/direwolf/nvidia-lib32-version 2>/dev/null || true)
have=$(awk '/^NVRM version/ { for (i = 1; i <= NF; i++) if ($i ~ /^[0-9]+\.[0-9.]+$/) { print $i; exit } }' \
    /proc/driver/nvidia/version 2>/dev/null || true)
if [ -n "$want" ] && [ -n "$have" ] && [ "$want" != "$have" ]; then
    gow_log "NVIDIA driver $have != 32-bit libraries $want: removing them, 32-bit games render on the CPU and DLSS is off"
    rm -f /usr/lib/i386-linux-gnu/lib*nvidia*
    rm -rf /usr/lib/nvidia/wine
    ldconfig
fi
